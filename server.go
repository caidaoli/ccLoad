package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gin-gonic/gin"
)

type Server struct {
	store    Store
	client   *http.Client
	password string
	sessions map[string]time.Time // sessionID -> expireTime
	sessMux  sync.RWMutex

	// API 认证
	authTokens map[string]bool // 允许的认证令牌

	// 缓存和异步优化
	configCache    []*Config
	configCacheMux sync.RWMutex
	configCacheExp atomic.Int64 // 缓存过期时间戳

	rrCache       *ristretto.Cache[string, int]
	cooldownCache sync.Map // channelID -> expireTime

	logChan    chan *LogEntry // 异步日志通道
	logWorkers int            // 日志工作协程数
}

func NewServer(store Store) *Server {
	password := os.Getenv("CCLOAD_PASS")
	if password == "" {
		password = "admin" // 默认密码，生产环境应该设置环境变量
	}

	// 解析 API 认证令牌
	authTokens := make(map[string]bool)
	if authEnv := os.Getenv("CCLOAD_AUTH"); authEnv != "" {
		tokens := strings.Split(authEnv, ",")
		for _, token := range tokens {
			token = strings.TrimSpace(token)
			if token != "" {
				authTokens[token] = true
			}
		}
	}

	// 优化 HTTP 客户端配置 - 重点优化连接建立阶段的超时控制
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,  // DNS解析+TCP连接建立超时
		KeepAlive: 30 * time.Second, // TCP keepalive间隔
	}

	transport := &http.Transport{
		// 连接池配置
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		MaxConnsPerHost:     100,

		// 使用优化的Dialer
		DialContext: dialer.DialContext,

		// 关键超时配置 - 直接影响TTFB性能
		TLSHandshakeTimeout:   5 * time.Second,  // TLS握手超时
		ResponseHeaderTimeout: 10 * time.Second, // 响应头读取超时(影响TTFB)
		ExpectContinueTimeout: 1 * time.Second,  // 100-continue超时

		// 传输优化
		DisableCompression: false,
		DisableKeepAlives:  false,
		ForceAttemptHTTP2:  true,      // 优先使用HTTP/2
		WriteBufferSize:    64 * 1024, // 64KB写缓冲区
		ReadBufferSize:     64 * 1024, // 64KB读缓冲区
		// 启用TLS会话缓存，减少重复握手耗时
		TLSClientConfig: &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(1024)},
	}

	s := &Server{
		store: store,
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
		password:   password,
		sessions:   make(map[string]time.Time),
		authTokens: authTokens,
		logChan:    make(chan *LogEntry, 1000), // 缓冲1000条日志
		logWorkers: 3,                          // 3个日志工作协程
	}

	rrCfg := &ristretto.Config[string, int]{
		NumCounters: 10000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	}
	var err error
	s.rrCache, err = ristretto.NewCache(rrCfg)
	if err != nil {
		panic("failed to create rrCache: " + err.Error())
	}
	// 启动日志工作协程
	for i := 0; i < s.logWorkers; i++ {
		go s.logWorker()
	}

	// 启动后台清理协程
	go s.cleanupExpiredCooldowns()
	go s.cleanExpiredSessions()

	return s

}

func parseInt64Param(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// 生成随机session ID
func (s *Server) generateSessionID() string {
	bytes := make([]byte, 32)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// 创建session
func (s *Server) createSession() string {
	s.sessMux.Lock()
	defer s.sessMux.Unlock()

	sessionID := s.generateSessionID()
	// session有效期24小时
	s.sessions[sessionID] = time.Now().Add(24 * time.Hour)
	return sessionID
}

// 验证session
func (s *Server) validateSession(sessionID string) bool {
	s.sessMux.RLock()
	defer s.sessMux.RUnlock()

	expireTime, exists := s.sessions[sessionID]
	if !exists {
		return false
	}

	if time.Now().After(expireTime) {
		// session已过期，删除它
		s.sessMux.RUnlock()
		s.sessMux.Lock()
		delete(s.sessions, sessionID)
		s.sessMux.Unlock()
		s.sessMux.RLock()
		return false
	}

	return true
}

// 清理过期session
func (s *Server) cleanExpiredSessions() {
	s.sessMux.Lock()
	defer s.sessMux.Unlock()

	now := time.Now()
	for sessionID, expireTime := range s.sessions {
		if now.After(expireTime) {
			delete(s.sessions, sessionID)
		}
	}
}

// 身份验证中间件 - Gin版本
func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查cookie中的session
		sessionID, err := c.Cookie("ccload_session")
		if err != nil || !s.validateSession(sessionID) {
			// 未登录，重定向到登录页面
			loginURL := "/web/login.html?redirect=" + c.Request.URL.Path
			if c.Request.URL.RawQuery != "" {
				loginURL += "%3F" + c.Request.URL.RawQuery // 编码查询参数
			}
			c.Redirect(http.StatusFound, loginURL)
			c.Abort()
			return
		}
		c.Next()
	}
}

// API 认证中间件 - Gin版本
func (s *Server) requireAPIAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果没有配置认证令牌，则跳过验证
		if len(s.authTokens) == 0 {
			c.Next()
			return
		}

		// 检查 Authorization 头
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			// 解析 Bearer token
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token := strings.TrimPrefix(authHeader, prefix)
				if s.authTokens[token] {
					c.Next()
					return
				}
			}
		}

		// 检查 X-API-Key 头
		apiKey := c.GetHeader("X-API-Key")
		if apiKey != "" && s.authTokens[apiKey] {
			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
		c.Abort()
	}
}

// 登录处理程序 - Gin版本
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	if req.Password != s.password {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid password"})
		return
	}

	// 密码正确，创建session
	sessionID := s.createSession()

	// 设置cookie
	c.SetCookie("ccload_session", sessionID, 24*60*60, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// 登出处理程序 - Gin版本
func (s *Server) handleLogout(c *gin.Context) {
	// 清除cookie
	c.SetCookie("ccload_session", "", -1, "/", "", false, true)

	// 清除服务器端session
	if sessionID, err := c.Cookie("ccload_session"); err == nil {
		s.sessMux.Lock()
		delete(s.sessions, sessionID)
		s.sessMux.Unlock()
	}

	c.Redirect(http.StatusFound, "/web/login.html")
}

// setupRoutes - 新的路由设置函数，适配Gin
func (s *Server) setupRoutes(r *gin.Engine) {
	// 公开访问的API（代理服务）- 需要 API 认证
	api := r.Group("/v1")
	api.Use(s.requireAPIAuth())
	{
		api.POST("/messages", s.handleMessages)
	}

	// 公开访问的API（基础统计）
	public := r.Group("/public")
	{
		public.GET("/summary", s.handlePublicSummary)
	}

	// 登录相关（公开访问）
	r.POST("/login", s.handleLogin)
	r.GET("/logout", s.handleLogout)

	// 需要身份验证的admin APIs
	admin := r.Group("/admin")
	admin.Use(s.requireAuth())
	{
		admin.GET("/channels", s.handleChannels)
		admin.POST("/channels", s.handleChannels)
		admin.GET("/channels/:id", s.handleChannelByID)
		admin.PUT("/channels/:id", s.handleChannelByID)
		admin.DELETE("/channels/:id", s.handleChannelByID)
		admin.POST("/channels/:id/test", s.handleChannelTest)
		admin.GET("/errors", s.handleErrors)
		admin.GET("/metrics", s.handleMetrics)
		admin.GET("/stats", s.handleStats)
	}

	// 静态文件服务
	r.GET("/web/*filepath", s.handleWebFiles)

	// 默认首页重定向
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/index.html")
	})
}

// 处理web静态文件，对管理页面进行身份验证 - Gin版本
func (s *Server) handleWebFiles(c *gin.Context) {
	filepath := c.Param("filepath")

	// 需要身份验证的页面
	authRequiredPages := []string{
		"/channels.html",
		"/logs.html",
		"/stats.html",
	}

	needsAuth := false
	for _, page := range authRequiredPages {
		if filepath == page {
			needsAuth = true
			break
		}
	}

	if needsAuth {
		// 检查身份验证
		sessionID, err := c.Cookie("ccload_session")
		if err != nil || !s.validateSession(sessionID) {
			loginURL := "/web/login.html?redirect=" + c.Request.URL.Path
			if c.Request.URL.RawQuery != "" {
				loginURL += "%3F" + c.Request.URL.RawQuery
			}
			c.Redirect(http.StatusFound, loginURL)
			return
		}
	}

	// 提供静态文件服务
	c.File("web" + filepath)
}

// session清理循环
func (s *Server) sessionCleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour) // 每小时清理一次
	defer ticker.Stop()

	for range ticker.C {
		s.cleanExpiredSessions()
	}
}

// 获取缓存的配置
func (s *Server) getCachedConfigs(ctx context.Context) ([]*Config, error) {
	now := time.Now().Unix()
	exp := s.configCacheExp.Load()

	// 缓存未过期，直接返回
	if exp > now {
		s.configCacheMux.RLock()
		defer s.configCacheMux.RUnlock()
		if s.configCache != nil {
			return s.configCache, nil
		}
	}

	// 需要刷新缓存
	cfgs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	s.configCacheMux.Lock()
	defer s.configCacheMux.Unlock()
	s.configCache = cfgs
	s.configCacheExp.Store(now + 60) // 缓存60秒

	return cfgs, nil
}

// 异步日志工作协程
func (s *Server) logWorker() {
	batch := make([]*LogEntry, 0, 100)
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()

	for {
		select {
		case entry := <-s.logChan:
			batch = append(batch, entry)
			if len(batch) >= 100 {
				s.flushLogs(batch)
				batch = batch[:0]
			}
			timer.Reset(1 * time.Second)
		case <-timer.C:
			if len(batch) > 0 {
				s.flushLogs(batch)
				batch = batch[:0]
			}
		}
	}
}

// 批量写入日志
func (s *Server) flushLogs(logs []*LogEntry) {
	ctx := context.Background()
	for _, log := range logs {
		_ = s.store.AddLog(ctx, log)
	}
}

// 异步添加日志
func (s *Server) addLogAsync(entry *LogEntry) {
	select {
	case s.logChan <- entry:
		// 成功放入队列
	default:
		// 队列满，丢弃日志（生产环境可以考虑监控）
	}
}

// 清理过期的冷却状态
func (s *Server) cleanupExpiredCooldowns() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		s.cooldownCache.Range(func(key, value interface{}) bool {
			if expireTime, ok := value.(time.Time); ok && now.After(expireTime) {
				s.cooldownCache.Delete(key)
			}
			return true
		})
	}
}
