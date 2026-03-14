package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type RateLimitInfo struct {
	ModelName string `json:"model_name"`
	LimitTime int64  `json:"limit_time"` // 发生 429 的时间戳(Unix)
}

type ProcessState struct {
	Pid               int             `json:"pid"`
	Running           bool            `json:"running"`
	StartTime         int64           `json:"start_time"`
	RateLimitedModels []RateLimitInfo `json:"rate_limited_models,omitempty"`
}

var (
	pm        ProcessManager
	PythonBin = "python" // 或者 python3，根据环境而定
	ScriptDir = "../camoufox-py" // 默认开发环境路径
)

type ProcessInfo struct {
	Cmd               *exec.Cmd
	StartTime         time.Time
	RateLimitedModels map[string]time.Time // 用map记录触发了429的模型名称以及时间
}

// GuestToCookie 记录 WebSocket client_id 与 cookie_file 的映射关系
var GuestToCookie sync.Map

func initProcess() {
	pm = ProcessManager{
		processes: make(map[string]*ProcessInfo),
	}
	if pb := os.Getenv("PYTHON_BIN"); pb != "" {
		PythonBin = pb
	}
	
	if sd := os.Getenv("SCRIPT_DIR"); sd != "" {
		ScriptDir = sd
	} else if _, err := os.Stat("camoufox-py/run_single.py"); err == nil {
		ScriptDir = "camoufox-py" // 根目录下运行
	} else if _, err := os.Stat("../camoufox-py/run_single.py"); err == nil {
		ScriptDir = "../camoufox-py" // golang 目录下运行
	}
	fmt.Printf("Initialised ScriptDir: %s\n", ScriptDir)
}

type ProcessManager struct {
	sync.RWMutex
	processes map[string]*ProcessInfo // key=cookieFileName
}

// StartProcess 启动某个配置的浏览器实例
func (m *ProcessManager) StartProcess(cookieFileName string) error {
	m.Lock()
	defer m.Unlock()

	if info, ok := m.processes[cookieFileName]; ok && info.Cmd.Process != nil {
		// 检查是否真在运行
		if err := info.Cmd.Process.Signal(syscall.Signal(0)); err == nil {
			return fmt.Errorf("实例 %s 已经在运行中, PID=%d", cookieFileName, info.Cmd.Process.Pid)
		}
	}

	cfg := GetConfig()
	
	expectedUrl := ""
	if cfg.GlobalSettings.URL != nil {
		expectedUrl = *cfg.GlobalSettings.URL
	}
	if expectedUrl == "" {
		return fmt.Errorf("启动失败: 全局配置中缺少必须的 url 字段")
	}

	// 从全局继承
	finalCfg := map[string]interface{}{
		"cookie_file": cookieFileName,
		"url":         expectedUrl,
		"headless":    cfg.GlobalSettings.Headless,
	}

	if cfg.GlobalSettings.Proxy != nil && *cfg.GlobalSettings.Proxy != "" {
		finalCfg["proxy"] = *cfg.GlobalSettings.Proxy
	}

	jsonBytes, _ := json.Marshal(finalCfg)
	b64Config := base64.StdEncoding.EncodeToString(jsonBytes)

	// 因为我们将工作目录改变为 ScriptDir，所以可以直接运行当前目录下的 run_single.py
	cmd := exec.Command(PythonBin, "run_single.py", "--config-b64", b64Config)
	cmd.Dir = ScriptDir

	// 将子进程的输出重定向到主服务的标准输出，便于由 supervisor 一并收集
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}

	info := &ProcessInfo{
		Cmd:               cmd,
		StartTime:         time.Now(),
		RateLimitedModels: make(map[string]time.Time),
	}
	m.processes[cookieFileName] = info

	// 开启一个goroutine等待它退出，以便回收资源并不变成僵尸进程
	go func(filename string, i *ProcessInfo) {
		i.Cmd.Wait()
		m.Lock()
		if m.processes[filename] == i {
			delete(m.processes, filename)
		}
		m.Unlock()
		log.Printf("实例 %s (PID: %d) 已退出", filename, i.Cmd.Process.Pid)
	}(cookieFileName, info)

	log.Printf("成功启动实例 %s (PID: %d), 目标URL: %s", cookieFileName, cmd.Process.Pid, expectedUrl)
	return nil
}

// StopProcess 停止某个实例
func (m *ProcessManager) StopProcess(cookieFileName string) error {
	m.Lock()
	defer m.Unlock()

	info, ok := m.processes[cookieFileName]
	if !ok || info.Cmd.Process == nil {
		return fmt.Errorf("未找到实例 %s 的运行进程", cookieFileName)
	}

	err := info.Cmd.Process.Kill() // 或者使用 cmd.Process.Signal(os.Interrupt) 平滑退出
	if err != nil {
		return fmt.Errorf("杀进程 PID=%d 失败: %w", info.Cmd.Process.Pid, err)
	}

	delete(m.processes, cookieFileName)
	log.Printf("成功停止实例 %s", cookieFileName)
	return nil
}

// GetStatus 获取所有实例的状态
func (m *ProcessManager) GetStatus() map[string]ProcessState {
	m.RLock()
	defer m.RUnlock()

	status := make(map[string]ProcessState)
	for cookieFileName, info := range m.processes {
		if info.Cmd.Process != nil {
			// 简单验证进程是否存活 (Linux/Mac 下可用 signal 0)
			err := info.Cmd.Process.Signal(syscall.Signal(0))
			if err == nil {
				var limited []RateLimitInfo
				now := time.Now()
				
				// 自动清理超过 24 小时 (24 * time.Hour) 的 429 限制
				for mName, limitTime := range info.RateLimitedModels {
					if now.Sub(limitTime) > 24*time.Hour {
						delete(info.RateLimitedModels, mName)
					} else {
						limited = append(limited, RateLimitInfo{
							ModelName: mName,
							LimitTime: limitTime.Unix(),
						})
					}
				}
				
				status[cookieFileName] = ProcessState{
					Pid:               info.Cmd.Process.Pid,
					Running:           true,
					StartTime:         info.StartTime.Unix(),
					RateLimitedModels: limited,
				}
			}
		}
	}
	return status
}

// MarkRateLimited 为给定的 CookieFile 标记一个受到 429 限制的模型
func (m *ProcessManager) MarkRateLimited(cookieFileName string, modelName string) {
	m.Lock()
	defer m.Unlock()
	if info, ok := m.processes[cookieFileName]; ok {
		if info.RateLimitedModels == nil {
			info.RateLimitedModels = make(map[string]time.Time)
		}
		info.RateLimitedModels[modelName] = time.Now()
	}
}

// ClearRateLimit 为给定的 CookieFile 手动清除受限标记
func (m *ProcessManager) ClearRateLimit(cookieFileName string) {
	m.Lock()
	defer m.Unlock()
	if info, ok := m.processes[cookieFileName]; ok {
		info.RateLimitedModels = make(map[string]time.Time)
	}
}

// StopAll 退出主程序前杀死所有子进程
func (m *ProcessManager) StopAll() {
	m.Lock()
	defer m.Unlock()
	for filename, info := range m.processes {
		if info.Cmd.Process != nil {
			info.Cmd.Process.Kill()
		}
		delete(m.processes, filename)
	}
}

// StartAll 从自动拉起所有 cookie 文件 (如果需要此逻辑)
func (m *ProcessManager) StartAll() {
	cookies, err := ListCookies()
	if err != nil {
		log.Printf("StartAll 失败, 无法读取 cookies: %v", err)
		return
	}
	for _, cookie := range cookies {
		err := m.StartProcess(cookie)
		if err != nil {
			log.Printf("StartAll 启动实例 %s 失败: %v", cookie, err)
		}
	}
}
