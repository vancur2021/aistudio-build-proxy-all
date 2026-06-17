package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//go:embed frontend/index.html
var frontendFs embed.FS

// --- Constants ---
const (
	wsPath              = "/v1/ws"
	proxyListenAddr     = ":5345"
	wsReadTimeout       = 60 * time.Second
	proxyRequestTimeout = 600 * time.Second
)

// --- 1. 连接管理与负载均衡 ---

// UserConnection 存储单个WebSocket连接及其元数据
type UserConnection struct {
	Conn       *websocket.Conn
	UserID     string
	ClientID   string     // 对应前端生成的 Guest-XXXX ID
	LastActive time.Time
	writeMutex sync.Mutex // 保护对此单个连接的并发写入
}

// safeWriteJSON 线程安全地向单个WebSocket连接写入JSON
func (uc *UserConnection) safeWriteJSON(v interface{}) error {
	uc.writeMutex.Lock()
	defer uc.writeMutex.Unlock()
	return uc.Conn.WriteJSON(v)
}

// UserConnections 维护单个用户的所有连接和负载均衡状态
type UserConnections struct {
	sync.Mutex
	Connections []*UserConnection
	NextIndex   int // 用于轮询 (round-robin)
}

// ConnectionPool 全局连接池，并发安全
type ConnectionPool struct {
	sync.RWMutex
	Users map[string]*UserConnections
}

var globalPool = &ConnectionPool{
	Users: make(map[string]*UserConnections),
}

// AddConnection 将新连接添加到池中
func (p *ConnectionPool) AddConnection(userID string, clientID string, conn *websocket.Conn) *UserConnection {
	userConn := &UserConnection{
		Conn:       conn,
		UserID:     userID,
		ClientID:   clientID,
		LastActive: time.Now(),
	}

	p.Lock()
	defer p.Unlock()

	userConns, exists := p.Users[userID]
	if !exists {
		userConns = &UserConnections{
			Connections: make([]*UserConnection, 0),
			NextIndex:   0,
		}
		p.Users[userID] = userConns
	}

	userConns.Lock()
	userConns.Connections = append(userConns.Connections, userConn)
	userConns.Unlock()

	log.Printf("WebSocket connected: UserID=%s, Total connections for user: %d", userID, len(userConns.Connections))
	return userConn
}

// RemoveConnection 从池中移除连接
func (p *ConnectionPool) RemoveConnection(userID string, conn *websocket.Conn) {
	p.Lock()
	defer p.Unlock()

	userConns, exists := p.Users[userID]
	if !exists {
		return
	}

	userConns.Lock()
	defer userConns.Unlock()

	// 查找并移除连接
	for i, uc := range userConns.Connections {
		if uc.Conn == conn {
			// 高效删除：将最后一个元素移到当前位置，然后截断切片
			userConns.Connections[i] = userConns.Connections[len(userConns.Connections)-1]
			userConns.Connections = userConns.Connections[:len(userConns.Connections)-1]
			log.Printf("WebSocket disconnected: UserID=%s, Remaining connections for user: %d", userID, len(userConns.Connections))
			break
		}
	}

	// 如果该用户没有连接了，可以从主map中删除用户条目（可选）
	if len(userConns.Connections) == 0 {
		delete(p.Users, userID)
	}
}

// GetActiveConnection 获取当前主节点(ActiveCookie)对应的WebSocket连接
func (p *ConnectionPool) GetActiveConnection(userID string) (*UserConnection, error) {
	activeCookie := nc.GetActiveCookie()
	if activeCookie == "" {
		return nil, errors.New("no active node configured")
	}

	p.RLock()
	userConns, exists := p.Users[userID]
	p.RUnlock()

	if !exists {
		return nil, errors.New("no available client for this user")
	}

	userConns.Lock()
	defer userConns.Unlock()

	// 查找与 activeCookie 绑定的连接
	for _, conn := range userConns.Connections {
		if cookieFileObj, ok := GuestToCookie.Load(conn.ClientID); ok {
			if cookieFileObj.(string) == activeCookie {
				return conn, nil
			}
		}
	}

	return nil, errors.New("active node websocket not connected yet")
}

// GetTotalConnections 获取指定租户(userID)当前的有效WebSocket连接总数
func (p *ConnectionPool) GetTotalConnections(userID string) int {
	p.RLock()
	defer p.RUnlock()
	if userConns, exists := p.Users[userID]; exists {
		// 为了更加精确，由于RemoveConnection是同步清理，直接拿切片长度即可
		userConns.Lock()
		count := len(userConns.Connections)
		userConns.Unlock()
		return count
	}
	return 0
}

// --- 2. WebSocket 消息结构 & 待处理请求 ---

// WSMessage 是前后端之间通信的基本结构
type WSMessage struct {
	ID      string                 `json:"id"`      // 请求/响应的唯一ID
	Type    string                 `json:"type"`    // ping, pong, http_request, http_response, stream_start, stream_chunk, stream_end, error
	Payload map[string]interface{} `json:"payload"` // 具体数据
}

// pendingRequests 存储待处理的HTTP请求，等待WS响应
// key: reqID (string), value: chan *WSMessage
var pendingRequests sync.Map

// --- 3. WebSocket 处理器和心跳 ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// 生产环境中应设置严格的CheckOrigin
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 认证
	authToken := r.URL.Query().Get("auth_token")
	clientID := r.URL.Query().Get("client_id")
	userID, err := validateJWT(authToken)
	if err != nil {
		log.Printf("WebSocket authentication failed: %v", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 升级连接
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}

	// 添加到连接池
	userConn := globalPool.AddConnection(userID, clientID, conn)

	// 启动读取循环
	go readPump(userConn)
}

// readPump 处理来自单个WebSocket连接的所有传入消息
func readPump(uc *UserConnection) {
	defer func() {
		globalPool.RemoveConnection(uc.UserID, uc.Conn)
		uc.Conn.Close()
		log.Printf("readPump closed for user %s", uc.UserID)
	}()

	// 设置读取超时 (心跳机制)
	uc.Conn.SetReadDeadline(time.Now().Add(wsReadTimeout))

	for {
		_, message, err := uc.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error for user %s: %v", uc.UserID, err)
			} else {
				log.Printf("WebSocket closed for user %s: %v", uc.UserID, err)
			}
			// 如果读取失败（包括超时），退出循环并清理连接
			break
		}

		// 收到任何消息，重置读取超时
		uc.Conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		uc.LastActive = time.Now()

		// 解析消息
		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Error unmarshalling WebSocket message: %v", err)
			continue
		}

		switch msg.Type {
		case "ping":
			// 心跳响应
			err := uc.safeWriteJSON(map[string]string{"type": "pong", "id": msg.ID})
			if err != nil {
				log.Printf("Error sending pong: %v", err)
				return // 发送失败，认为连接已断
			}
		case "http_response", "stream_start", "stream_chunk", "stream_end", "error":
			// 路由响应到等待的HTTP Handler
			if ch, ok := pendingRequests.Load(msg.ID); ok {
				respChan := ch.(chan *WSMessage)
				// 尝试发送，如果通道已满（不太可能，但为了安全），则记录日志
				select {
				case respChan <- &msg:
				default:
					log.Printf("Warning: Response channel full for request ID %s, dropping message type %s", msg.ID, msg.Type)
				}
			} else {
				log.Printf("Received response for unknown or timed-out request ID: %s", msg.ID)
			}
		default:
			log.Printf("Received unknown message type from client: %s", msg.Type)
		}
	}
}

// --- 4. HTTP 反向代理与 WS 隧道 ---

var ErrLimitExceeded = errors.New("limit exceeded (429/403)")

func handleProxyRequest(w http.ResponseWriter, r *http.Request) {
	// 1. 认证并获取UserID (这里模拟)
	userID, err := authenticateHTTPRequest(r)
	if err != nil {
		http.Error(w, "Proxy authentication failed", http.StatusUnauthorized)
		return
	}

	// 2. 缓存请求体，以便重试
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	headers := make(map[string][]string)
	for k, v := range r.Header {
		if k != "Connection" && k != "Keep-Alive" && k != "Proxy-Authenticate" && k != "Proxy-Authorization" && k != "Te" && k != "Trailers" && k != "Transfer-Encoding" && k != "Upgrade" {
			headers[k] = v
		}
	}

	maxRetries := 30 // 增加重试次数，以应对节点启动需要较长时间的情况
	for attempt := 0; attempt < maxRetries; attempt++ {
		reqID := uuid.NewString()
		// 调大通道容量，防止高速流式响应(stream_chunk)导致通道满而丢包
		respChan := make(chan *WSMessage, 1000)
		pendingRequests.Store(reqID, respChan)

		// 获取当前主节点连接
		selectedConn, err := globalPool.GetActiveConnection(userID)
		if err != nil {
			pendingRequests.Delete(reqID)
			log.Printf("Attempt %d: Error getting active connection: %v", attempt, err)
			time.Sleep(2 * time.Second) // 增加等待时间，等待备用节点启动并连上 WebSocket
			continue
		}

		requestPayload := WSMessage{
			ID:   reqID,
			Type: "http_request",
			Payload: map[string]interface{}{
				"method":  r.Method,
				"url":     "https://generativelanguage.googleapis.com" + r.URL.String(),
				"headers": headers,
				"body":    string(bodyBytes),
			},
		}

		if err := selectedConn.safeWriteJSON(requestPayload); err != nil {
			pendingRequests.Delete(reqID)
			log.Printf("Attempt %d: Failed to send request over WebSocket: %v", attempt, err)
			time.Sleep(1 * time.Second)
			continue
		}

		// 异步等待并处理响应
		err = processWebSocketResponse(w, r, respChan, selectedConn)
		pendingRequests.Delete(reqID)

		if err == ErrLimitExceeded {
			log.Printf("Attempt %d: Hit 429/403 Limit. Retrying...", attempt)
			continue // 触发重试
		} else if err != nil {
			// 其他错误，已经由 processWebSocketResponse 处理了 HTTP 响应
			return
		}

		// 成功处理
		return
	}

	http.Error(w, "Service Unavailable: Max retries exceeded or no active node", http.StatusServiceUnavailable)
}

// processWebSocketResponse 处理来自WS通道的响应，构建HTTP响应
func processWebSocketResponse(w http.ResponseWriter, r *http.Request, respChan chan *WSMessage, conn *UserConnection) error {
	ctx, cancel := context.WithTimeout(r.Context(), proxyRequestTimeout)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("Warning: ResponseWriter does not support flushing, streaming will be buffered.")
	}

	headersSet := false

	for {
		select {
		case msg, ok := <-respChan:
			if !ok {
				if !headersSet {
					http.Error(w, "Internal Server Error: Response channel closed unexpectedly", http.StatusInternalServerError)
				}
				return errors.New("channel closed")
			}

			switch msg.Type {
			case "http_response":
				if headersSet {
					log.Println("Received http_response after headers were already set. Ignoring.")
					return nil
				}
				
				// --- 429/403 拦截 ---
				if status, ok := msg.Payload["status"].(float64); ok && (status == 429 || status == 403) {
					handleLimitError(conn, int(status))
					return ErrLimitExceeded
				}
				
				setResponseHeaders(w, msg.Payload)
				writeStatusCode(w, msg.Payload)
				writeBody(w, msg.Payload)
				return nil

			case "stream_start":
				if headersSet {
					log.Println("Received stream_start after headers were already set. Ignoring.")
					continue
				}
				
				// --- 429/403 拦截 ---
				if status, ok := msg.Payload["status"].(float64); ok && (status == 429 || status == 403) {
					handleLimitError(conn, int(status))
					return ErrLimitExceeded
				}
				
				setResponseHeaders(w, msg.Payload)
				writeStatusCode(w, msg.Payload)
				headersSet = true
				if flusher != nil {
					flusher.Flush()
				}

			case "stream_chunk":
				if !headersSet {
					w.WriteHeader(http.StatusOK)
					headersSet = true
				}
				writeBody(w, msg.Payload)
				if flusher != nil {
					flusher.Flush()
				}

			case "stream_end":
				if !headersSet {
					w.WriteHeader(http.StatusOK)
				}
				return nil

			case "error":
				if !headersSet {
					errMsg := "Bad Gateway: Client reported an error"
					if payloadErr, ok := msg.Payload["error"].(string); ok {
						errMsg = payloadErr
					}
					statusCode := http.StatusBadGateway
					if code, ok := msg.Payload["status"].(float64); ok {
						statusCode = int(code)
					}
					http.Error(w, errMsg, statusCode)
				}
				return errors.New("client error")

			default:
				log.Printf("Received unexpected message type %s while waiting for response", msg.Type)
			}

		case <-ctx.Done():
			if !headersSet {
				log.Printf("Gateway Timeout: No response from client for request %s", r.URL.Path)
				http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			}
			return errors.New("timeout")
		}
	}
}

// handleLimitError 通过 ClientID 反查关联的 CookieFile 并触发主备切换
func handleLimitError(conn *UserConnection, statusCode int) {
	if conn == nil || conn.ClientID == "" {
		return
	}
	cookieFileObj, ok := GuestToCookie.Load(conn.ClientID)
	if ok {
		cookieFile := cookieFileObj.(string)
		log.Printf("Limit hit (%d) mapped! ClientID: %s, CookieFile: %s", statusCode, conn.ClientID, cookieFile)
		nc.HandleLimit(cookieFile, statusCode)
	} else {
		log.Printf("Limit hit (%d) but could not find mapped CookieFile for ClientID: %s", statusCode, conn.ClientID)
	}
}

// --- 辅助函数 ---

// setResponseHeaders 从payload中解析并设置HTTP响应头
func setResponseHeaders(w http.ResponseWriter, payload map[string]interface{}) {
	headers, ok := payload["headers"].(map[string]interface{})
	if !ok {
		return
	}
	for key, value := range headers {
		// 假设值是 []interface{} 或 string
		if values, ok := value.([]interface{}); ok {
			for _, v := range values {
				if strV, ok := v.(string); ok {
					w.Header().Add(key, strV)
				}
			}
		} else if strV, ok := value.(string); ok {
			w.Header().Set(key, strV)
		}
	}
}

// writeStatusCode 从payload中解析并设置HTTP状态码
func writeStatusCode(w http.ResponseWriter, payload map[string]interface{}) {
	status, ok := payload["status"].(float64) // JSON数字默认为float64
	if !ok {
		w.WriteHeader(http.StatusOK) // 默认200
		return
	}
	w.WriteHeader(int(status))
}

// writeBody 从payload中解析并写入HTTP响应体
func writeBody(w http.ResponseWriter, payload map[string]interface{}) {
	var bodyData []byte
	// 对于 http_response，body 键通常包含数据
	if body, ok := payload["body"].(string); ok {
		bodyData = []byte(body)
	}
	// 对于 stream_chunk，data 键通常包含数据
	if data, ok := payload["data"].(string); ok {
		bodyData = []byte(data)
	}
	// 注意：如果前端发送的是二进制数据，这里应该假设它是base64编码的字符串并进行解码

	if len(bodyData) > 0 {
		w.Write(bodyData)
	}
}

// validateJWT 模拟JWT验证并返回userID
func validateJWT(token string) (string, error) {
	if token == "" {
		return "", errors.New("missing auth_token")
	}
	// 实际应用中，这里需要使用JWT库（如golang-jwt/jwt）来验证签名和过期时间
	// 这里我们简单地将token当作userID
	if token == "valid-token-user-1" {
		return "user-1", nil
	}
	//if token == "valid-token-user-2" {
	//	return "user-2", nil
	//}
	return "", errors.New("invalid token")
}

// authenticateHTTPRequest 模拟HTTP代理请求的认证
func authenticateHTTPRequest(r *http.Request) (string, error) {
	// 实际应用中，可能检查Authorization头或其他API Key
	apiKey := r.Header.Get("x-goog-api-key")
	if apiKey == "" {
		// r.URL.Query() 会解析URL中的查询参数，返回一个 map[string][]string
		// .Get() 方法可以方便地获取指定参数的第一个值，如果参数不存在则返回空字符串
		apiKey = r.URL.Query().Get("key")
	}

	// 从环境变量中获取预期的API密钥
	expectedAPIKey := os.Getenv("AUTH_API_KEY")
	if expectedAPIKey == "" {
		log.Println("CRITICAL: AUTH_API_KEY environment variable not set.")
		// 在生产环境中，您可能希望完全阻止请求
		return "", errors.New("server configuration error")
	}

	if apiKey == expectedAPIKey {
		// 单租户
		return "user-1", nil
	}

	return "", errors.New("invalid API key")
}

// --- API Handlers ---

func apiHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	// 简单的权限检查 (针对生产环境建议添加Basic Auth或Token)
	// 这里默认开放内网，你可以根据需要自行开启鉴权
	
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/config":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GetConfig())

	case r.Method == http.MethodPost && r.URL.Path == "/api/config":
		var cfg AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := UpdateConfig(cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodGet && r.URL.Path == "/api/cookies":
		cookies, err := ListCookies()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cookies)

	case r.Method == http.MethodPost && r.URL.Path == "/api/cookies/upload":
		err := r.ParseMultipartForm(10 << 20) // 10 MB limit
		if err != nil {
			http.Error(w, "Unable to parse form", http.StatusBadRequest)
			return
		}
		file, handler, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "File is missing", http.StatusBadRequest)
			return
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "Read file error", http.StatusInternalServerError)
			return
		}
		if err := SaveCookieFile(handler.Filename, content); err != nil {
			http.Error(w, "Save file error", http.StatusInternalServerError)
			return
		}
		// 上传/覆盖 Cookie 文件后，清除可能存在的失效标记
		pm.ClearInvalidCookie(handler.Filename)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/cookies/"):
		filename := strings.TrimPrefix(r.URL.Path, "/api/cookies/")
		if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		if err := DeleteCookieFile(filename); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 删除 Cookie 文件后，清除可能存在的失效标记
		pm.ClearInvalidCookie(filename)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/cookies/"):
		filename := strings.TrimPrefix(r.URL.Path, "/api/cookies/")
		if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		content, err := ReadCookieFile(filename)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(content)

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/cookies/"):
		filename := strings.TrimPrefix(r.URL.Path, "/api/cookies/")
		if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
			http.Error(w, "Invalid filename", http.StatusBadRequest)
			return
		}
		content, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Read body error", http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// 简单校验是否为合法 JSON
		var js json.RawMessage
		if err := json.Unmarshal(content, &js); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		if err := SaveCookieFile(filename, content); err != nil {
			http.Error(w, "Save file error", http.StatusInternalServerError)
			return
		}
		// 保存后清除失效标记
		pm.ClearInvalidCookie(filename)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/clear-limit"):
		// POST /api/process/{cookie_file}/clear-limit
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/clear-limit")
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}
		pm.ClearRateLimit(cookieFile)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/mark-invalid"):
		// POST /api/process/{cookie_file}/mark-invalid
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/mark-invalid")
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}
		pm.MarkInvalidCookie(cookieFile)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/refresh-cookie"):
		// POST /api/process/{cookie_file}/refresh-cookie
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/refresh-cookie")
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}

		// 校验进程是否在运行
		statusMap := pm.GetStatus()
		state, exists := statusMap[cookieFile]
		if !exists || !state.Running {
			http.Error(w, "Process is not running", http.StatusBadRequest)
			return
		}

		// 创建 trigger 文件
		logsDir := ScriptDir + "/logs"
		triggerFile := logsDir + "/refresh_cookie_" + cookieFile + ".trigger"
		
		// 确保 logs 目录存在
		os.MkdirAll(logsDir, 0755)
		
		f, err := os.Create(triggerFile)
		if err != nil {
			http.Error(w, "Failed to create trigger file", http.StatusInternalServerError)
			return
		}
		f.Close()

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/reload"):
		// POST /api/process/{cookie_file}/reload
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/reload")
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}

		// 校验进程是否在运行
		statusMap := pm.GetStatus()
		state, exists := statusMap[cookieFile]
		if !exists || !state.Running {
			http.Error(w, "Process is not running", http.StatusBadRequest)
			return
		}

		// 真正的重载：先停止进程，再重新启动
		log.Printf("Reloading process for %s...", cookieFile)
		if err := pm.StopProcess(cookieFile); err != nil {
			log.Printf("Failed to stop process during reload: %v", err)
			// 即使停止失败，也尝试继续启动
		}
		
		// 稍微等待一下确保进程完全退出
		time.Sleep(1 * time.Second)
		
		// 清除可能存在的失效标记
		pm.ClearInvalidCookie(cookieFile)
		
		if err := pm.StartProcess(cookieFile); err != nil {
			http.Error(w, "Failed to restart process: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/bind"):
		// POST /api/process/{cookie_file}/bind
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/bind")
		
		var payload struct {
			ClientID string `json:"client_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.ClientID == "" {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}
		
		// 建立 ClientID 和 Cookie文件的映射关系
		GuestToCookie.Store(payload.ClientID, cookieFile)
		log.Printf("Successfully bound ClientID %s to %s", payload.ClientID, cookieFile)
		
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == http.MethodGet && r.URL.Path == "/api/process/status":
		statusMap := pm.GetStatus()
		
		// 动态计算 connected 状态
		// 在不知道哪个具体进程连上WS的情况下：优先认为早启动的且存活的进程已经连上 WS
		totalWs := globalPool.GetTotalConnections("user-1") // 目前硬编码都是user-1单租户
		
		type ProcessStateWithConnected struct {
			ProcessState
			Connected     bool   `json:"connected"`
			Role          string `json:"role"` // "active", "standby", or "none"
			InvalidCookie bool   `json:"invalid_cookie"`
		}
		
		extStatusMap := make(map[string]ProcessStateWithConnected)
		
		// 将 map 转为 slice 以进行排序
		type sortItem struct {
			File  string
			State ProcessState
		}
		var items []sortItem
		for k, v := range statusMap {
			items = append(items, sortItem{File: k, State: v})
		}
		
		// 按照 StartTime 升序排序 (越早启动的排越前)
		for i := 0; i < len(items); i++ {
			for j := i + 1; j < len(items); j++ {
				if items[i].State.StartTime > items[j].State.StartTime {
					items[i], items[j] = items[j], items[i]
				}
			}
		}
		
		activeCookie := nc.GetActiveCookie()
		standbyCookie := nc.GetStandbyCookie()

		// 将最前面 totalWs 个运行中的节点标记为 connected: true
		activeMatched := 0
		for _, item := range items {
			isConnected := false
			if activeMatched < totalWs {
				isConnected = true
				activeMatched++
			}
			
			role := "none"
			if item.File == activeCookie {
				role = "active"
			} else if item.File == standbyCookie {
				role = "standby"
			}

			extStatusMap[item.File] = ProcessStateWithConnected{
				ProcessState:  item.State,
				Connected:     isConnected,
				Role:          role,
				InvalidCookie: pm.IsCookieInvalid(item.File),
			}
		}

		// 补充那些没有在运行，但是被标记为失效的节点
		cookies, _ := ListCookies()
		for _, cookieFile := range cookies {
			if _, ok := extStatusMap[cookieFile]; !ok {
				if pm.IsCookieInvalid(cookieFile) {
					extStatusMap[cookieFile] = ProcessStateWithConnected{
						ProcessState:  ProcessState{Running: false},
						Connected:     false,
						Role:          "none",
						InvalidCookie: true,
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(extStatusMap)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/process/") && strings.HasSuffix(r.URL.Path, "/screenshot"):
		// GET /api/process/{cookie_file}/screenshot
		pathStr := strings.TrimPrefix(r.URL.Path, "/api/process/")
		cookieFile := strings.TrimSuffix(pathStr, "/screenshot")
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}
		
		// 结合当前的脚本目录找到 trigger 目录, 对应到 camoufox-py/logs
		// 因为执行 ScriptDir 可能带有相对路径，或者就是 "camoufox-py"
		logsDir := ScriptDir + "/logs"
		triggerFile := logsDir + "/take_screenshot_" + cookieFile + ".trigger"
		screenshotFile := logsDir + "/manual_screenshot_" + cookieFile + ".png"

		// 1. 先删掉旧的 screenshot 文件如果存在
		os.Remove(screenshotFile)

		// 2. 创建 trigger 文件
		f, err := os.Create(triggerFile)
		if err != nil {
			http.Error(w, "Failed to create trigger file", http.StatusInternalServerError)
			return
		}
		f.Close()

		// 3. 轮询等待 screenshotFile 生成 或者 triggerFile 被删且 screenshotFile有内容 (设置最长等待20秒)
		timeout := time.After(20 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		var fileContent []byte
		var found = false

	waitLoop:
		for {
			select {
			case <-timeout:
				break waitLoop
			case <-ticker.C:
				if _, err := os.Stat(screenshotFile); err == nil {
					// 等待一点时间确保文件写入完成
					time.Sleep(200 * time.Millisecond)
					fileContent, err = os.ReadFile(screenshotFile)
					if err == nil && len(fileContent) > 0 {
						found = true
						break waitLoop
					}
				}
			}
		}

		if !found {
			// 清理 trigger 文件以免后续干扰
			os.Remove(triggerFile)
			http.Error(w, "Timeout waiting for screenshot", http.StatusGatewayTimeout)
			return
		}

		// 返回二进制图片
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(fileContent)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/process/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/process/"), "/")
		if len(parts) != 2 {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		cookieFile := parts[0]
		action := parts[1]
		if cookieFile == "" {
			http.Error(w, "Invalid cookie file", http.StatusBadRequest)
			return
		}
		
		if action == "start" {
			// 启动时清除失效标记
			pm.ClearInvalidCookie(cookieFile)
			if err := pm.StartProcess(cookieFile); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else if action == "stop" {
			if err := pm.StopProcess(cookieFile); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// 手动停止节点后，通知 NodeController 触发主备切换或替补
			// 注意：因为 pm.StopProcess 现在会标记为预期内停止，不会自动触发 HandleNodeExit
			// 所以这里我们需要手动调用，以满足“手动停止节点后拉起新节点”的需求
			nc.HandleNodeExit(cookieFile)
		} else {
			http.Error(w, "Unknown action", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))

	default:
		http.NotFound(w, r)
	}
}

// 主处理器分发
func mainHandler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		apiHandler(w, r)
		return
	}

	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		htmlData, err := frontendFs.ReadFile("frontend/index.html")
		if err != nil {
			http.Error(w, "Frontend not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(htmlData)
		return
	}

	// 其他请求交由反向代理处理
	handleProxyRequest(w, r)
}

// --- 主函数 ---

func main() {
	// 初始化配置和目录
	initPath()
	if err := LoadConfig(); err != nil {
		log.Printf("LoadConfig warning: %v, using default settings", err)
	}
	
	// 初始化进程管理器
	initProcess()
	
	// 初始化节点控制器并启动初始主备节点
	initNodeController()
	nc.StartInitialNodes()

	// WebSocket 路由
	http.HandleFunc(wsPath, handleWebSocket)

	// 其他 HTTP 请求捕获
	http.HandleFunc("/", mainHandler)

	log.Printf("Starting server on %s", proxyListenAddr)
	log.Printf("Admin panel available at http://%s/admin", proxyListenAddr)
	log.Printf("WebSocket endpoint available at ws://%s%s", proxyListenAddr, wsPath)
	log.Printf("HTTP proxy available at http://%s/", proxyListenAddr)

	srv := &http.Server{
		Addr: proxyListenAddr,
	}

	// 开启一个 Go routine 运行服务
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Could not start server: %s\n", err)
		}
	}()

	// 监听系统退出信号以优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// 停止所有子进程（Camoufox浏览器）
	pm.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
}
