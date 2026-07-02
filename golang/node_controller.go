package main

import (
"log"
"sync"
)

type NodeController struct {
	sync.RWMutex
	ActiveCookie  string
	StandbyCookie string
	CookieQueue   []string
	QueueIndex    int
}

var nc *NodeController

func initNodeController() {
	nc = &NodeController{
		CookieQueue: make([]string, 0),
	}
	nc.RefreshQueue()
}

func (c *NodeController) RefreshQueue() {
	c.Lock()
	defer c.Unlock()
	cookies, err := ListCookies()
	if err != nil {
		log.Printf("Failed to list cookies: %v", err)
		return
	}
	c.CookieQueue = cookies
	c.QueueIndex = 0
}

func (c *NodeController) StartInitialNodes() {
	c.Lock()
	defer c.Unlock()
	
	if len(c.CookieQueue) == 0 {
		log.Println("No cookies available to start nodes")
		return
	}
	
	// Start Active
	c.ActiveCookie = c.CookieQueue[c.QueueIndex]
	c.QueueIndex = (c.QueueIndex + 1) % len(c.CookieQueue)
	log.Printf("NodeController: Starting initial active node %s", c.ActiveCookie)
	go pm.StartProcess(c.ActiveCookie)
	
	// Start Standby if available
	if len(c.CookieQueue) > 1 {
		c.StandbyCookie = c.CookieQueue[c.QueueIndex]
		c.QueueIndex = (c.QueueIndex + 1) % len(c.CookieQueue)
		log.Printf("NodeController: Starting initial standby node %s", c.StandbyCookie)
		go pm.StartProcess(c.StandbyCookie)
	}
}

func (c *NodeController) GetActiveCookie() string {
	c.RLock()
	defer c.RUnlock()
	return c.ActiveCookie
}

func (c *NodeController) GetStandbyCookie() string {
	c.RLock()
	defer c.RUnlock()
	return c.StandbyCookie
}

func (c *NodeController) HandleLimit(failedCookie string, statusCode int) {
	c.Lock()
	defer c.Unlock()
	
	if c.ActiveCookie != failedCookie {
		// Already handled or not active
		return
	}
	
	if statusCode == 401 || statusCode == 403 {
		log.Printf("NodeController: Active node %s hit %d (Auth Failed/Forbidden). Marking as invalid.", failedCookie, statusCode)
		pm.MarkInvalidCookie(failedCookie)
		// 从队列中剔除该失效 Cookie，防止轮询时再次被选中
		c.removeCookieFromQueue(failedCookie)
	} else {
		log.Printf("NodeController: Active node %s hit %d. Promoting standby %s to active.", failedCookie, statusCode, c.StandbyCookie)
	}
	
	// Kill old active
	go pm.StopProcess(failedCookie)
	
	c.promoteStandbyOrStartNew()
}

// removeCookieFromQueue 从队列中移除指定的 Cookie
func (c *NodeController) removeCookieFromQueue(cookieToRemove string) {
	for i, cookie := range c.CookieQueue {
		if cookie == cookieToRemove {
			c.CookieQueue = append(c.CookieQueue[:i], c.CookieQueue[i+1:]...)
			if c.QueueIndex > i && c.QueueIndex > 0 {
				c.QueueIndex--
			}
			// 如果删除后刚好 QueueIndex 到达或者超出了末尾，需要绕回
			if len(c.CookieQueue) > 0 && c.QueueIndex >= len(c.CookieQueue) {
				c.QueueIndex = 0
			}
			break
		}
	}
}

// HandleNodeExit 处理节点意外退出的情况
func (c *NodeController) HandleNodeExit(exitedCookie string) {
	c.Lock()
	defer c.Unlock()

	if c.ActiveCookie == exitedCookie {
		log.Printf("NodeController: Active node %s exited unexpectedly. Promoting standby %s to active.", exitedCookie, c.StandbyCookie)
		c.promoteStandbyOrStartNew()
	} else if c.StandbyCookie == exitedCookie {
		log.Printf("NodeController: Standby node %s exited unexpectedly. Starting a new standby.", exitedCookie)
		c.startNewStandby()
	}
}

// getNextValidCookie 从队列中获取下一个未失效的 Cookie
func (c *NodeController) getNextValidCookie() string {
	if len(c.CookieQueue) == 0 {
		return ""
	}
	
	checkedCount := 0
	for checkedCount < len(c.CookieQueue) {
		cookie := c.CookieQueue[c.QueueIndex]
		c.QueueIndex = (c.QueueIndex + 1) % len(c.CookieQueue)
		checkedCount++
		
		if !pm.IsCookieInvalid(cookie) {
			return cookie
		}
	}
	return "" // 所有 Cookie 均已失效
}

// promoteStandbyOrStartNew 内部方法：晋升备用节点，如果没有备用节点则直接启动一个新的主节点
func (c *NodeController) promoteStandbyOrStartNew() {
	if c.StandbyCookie != "" {
		// Promote standby to active
		c.ActiveCookie = c.StandbyCookie
		c.startNewStandby()
	} else {
		// No standby available (e.g. only 1 cookie in total), start a new active directly
		nextCookie := c.getNextValidCookie()
		if nextCookie != "" {
			c.ActiveCookie = nextCookie
			log.Printf("NodeController: No standby available. Starting new active node %s directly.", c.ActiveCookie)
			go pm.StartProcess(c.ActiveCookie)
		} else {
			c.ActiveCookie = ""
			log.Println("NodeController: CRITICAL - No valid cookies available to promote or start.")
		}
	}
}

// startNewStandby 内部方法：从队列中取出一个新的 Cookie 作为备用节点启动
func (c *NodeController) startNewStandby() {
	nextCookie := c.getNextValidCookie()
	if nextCookie != "" {
		c.StandbyCookie = nextCookie
		log.Printf("NodeController: Starting new standby node %s", c.StandbyCookie)
		go pm.StartProcess(c.StandbyCookie)
	} else {
		c.StandbyCookie = ""
		log.Println("NodeController: No valid cookies available for a standby node.")
	}
}
