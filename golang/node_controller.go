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

func (c *NodeController) Handle429(failedCookie string) {
	c.Lock()
	defer c.Unlock()
	
	if c.ActiveCookie != failedCookie {
		// Already handled or not active
		return
	}
	
	log.Printf("NodeController: Active node %s hit 429. Promoting standby %s to active.", failedCookie, c.StandbyCookie)
	
	// Kill old active
	go pm.StopProcess(failedCookie)
	
	c.promoteStandbyOrStartNew()
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

// promoteStandbyOrStartNew 内部方法：晋升备用节点，如果没有备用节点则直接启动一个新的主节点
func (c *NodeController) promoteStandbyOrStartNew() {
	if c.StandbyCookie != "" {
		// Promote standby to active
		c.ActiveCookie = c.StandbyCookie
		c.startNewStandby()
	} else {
		// No standby available (e.g. only 1 cookie in total), start a new active directly
		if len(c.CookieQueue) > 0 {
			c.ActiveCookie = c.CookieQueue[c.QueueIndex]
			c.QueueIndex = (c.QueueIndex + 1) % len(c.CookieQueue)
			log.Printf("NodeController: No standby available. Starting new active node %s directly.", c.ActiveCookie)
			go pm.StartProcess(c.ActiveCookie)
		} else {
			c.ActiveCookie = ""
			log.Println("NodeController: CRITICAL - No cookies available to promote or start.")
		}
	}
}

// startNewStandby 内部方法：从队列中取出一个新的 Cookie 作为备用节点启动
func (c *NodeController) startNewStandby() {
	if len(c.CookieQueue) > 0 {
		c.StandbyCookie = c.CookieQueue[c.QueueIndex]
		c.QueueIndex = (c.QueueIndex + 1) % len(c.CookieQueue)
		log.Printf("NodeController: Starting new standby node %s", c.StandbyCookie)
		go pm.StartProcess(c.StandbyCookie)
	} else {
		c.StandbyCookie = ""
	}
}
