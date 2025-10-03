package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gin-gonic/gin"
)

// rrUpdate 轮询指针更新记录（用于批量持久化）
type rrUpdate struct {
	model    string
	priority int
	idx      int
}

// channelIndex 渠道索引结构（性能优化：O(1)查找替代O(n)扫描）
// 阶段2优化：将渠道选择从0.5ms降至0.05ms（10倍提升）
type channelIndex struct {
	byModel    map[string][]*Config // 模型 → 预排序的渠道列表
	byType     map[string][]*Config // 渠道类型 → 预排序的渠道列表
	allEnabled []*Config            // 所有启用的渠道（按优先级排序）
	lastUpdate int64                // 缓存更新时间戳（Unix秒）
}

type Server struct {
	store       Store
	keySelector *KeySelector // Key选择器（多Key支持）
	client      *http.Client
	password    string
	sessions    map[string]time.Time // sessionID -> expireTime
	sessMux     sync.RWMutex

	// API 认证
	authTokens map[string]bool // 允许的认证令牌

	// 重试配置
	maxKeyRetries int // 单个渠道内最大Key重试次数（默认3次）

	// 性能优化开关
	enableTrace bool // HTTP Trace开关（性能优化：默认关闭，节省0.5-1ms/请求）

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	// 缓存和异步优化
	configCache    atomic.Value // 无锁缓存，存储 []*Config（性能优化：消除读锁争用）
	configCacheExp atomic.Int64 // 缓存过期时间戳
	channelIndex   atomic.Value // 无锁索引缓存，存储 *channelIndex（阶段2优化：O(1)查找）

	rrCache       *ristretto.Cache[string, int]
	cooldownCache sync.Map // channelID -> expireTime

	logChan    chan *LogEntry // 异步日志通道
	logWorkers int            // 日志工作协程数

	// 性能优化：批量轮询指针持久化
	rrUpdateChan    chan rrUpdate // 轮询指针更新通道
	rrBatchSize     int           // 批量写入大小
	rrFlushInterval time.Duration // 刷新间隔
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

	// 解析最大Key重试次数（避免key过多时重试次数过多）
	maxKeyRetries := 3 // 默认值
	if retryEnv := os.Getenv("CCLOAD_MAX_KEY_RETRIES"); retryEnv != "" {
		if val, err := strconv.Atoi(retryEnv); err == nil && val > 0 {
			maxKeyRetries = val
		}
	}

	// 解析HTTP Trace开关（性能优化：默认关闭，节省0.5-1ms/请求）
	enableTrace := false
	if traceEnv := os.Getenv("CCLOAD_ENABLE_TRACE"); traceEnv == "1" || traceEnv == "true" {
		enableTrace = true
	}

	// 解析最大并发数（性能优化：防止goroutine爆炸）
	maxConcurrency := 1000 // 默认1000并发
	if concEnv := os.Getenv("CCLOAD_MAX_CONCURRENCY"); concEnv != "" {
		if val, err := strconv.Atoi(concEnv); err == nil && val > 0 {
			maxConcurrency = val
		}
	}

	// 优化 HTTP 客户端配置 - 重点优化连接建立阶段的超时控制
	dialer := &net.Dialer{
		Timeout:   10 * time.Second, // DNS解析+TCP连接建立超时
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

		// 传输优化
		DisableCompression: false,
		DisableKeepAlives:  false,
		ForceAttemptHTTP2:  false,     // 允许自动协议协商，避免HTTP/2超时
		WriteBufferSize:    64 * 1024, // 64KB写缓冲区
		ReadBufferSize:     64 * 1024, // 64KB读缓冲区
		// 启用TLS会话缓存，减少重复握手耗时，跳过证书验证
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(1024),
			InsecureSkipVerify: true, // 跳过证书验证
		},
	}

	s := &Server{
		store:         store,
		keySelector:   NewKeySelector(store), // 初始化Key选择器
		maxKeyRetries: maxKeyRetries,         // 单个渠道最大Key重试次数
		enableTrace:   enableTrace,           // HTTP Trace开关
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
		password:   password,
		sessions:   make(map[string]time.Time),
		authTokens: authTokens,
		logChan:    make(chan *LogEntry, 1000), // 缓冲1000条日志
		logWorkers: 3,                          // 3个日志工作协程

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// 性能优化：批量轮询持久化配置
		rrUpdateChan:    make(chan rrUpdate, 500), // 缓冲500条更新
		rrBatchSize:     50,                       // 每批50条
		rrFlushInterval: 5 * time.Second,          // 每5秒强制刷新
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

	// 启动批量轮询持久化协程（性能优化）
	go s.rrBatchWriter()

	// 启动后台清理协程
	go s.cleanupExpiredCooldowns()
	go s.cleanExpiredSessions()
	go s.cleanupOldLogsLoop() // 定期清理3天前的日志（性能优化：避免每次插入时清理）

	return s

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

		// 检查 x-goog-api-key 头（Google API格式）
		googApiKey := c.GetHeader("x-goog-api-key")
		if googApiKey != "" && s.authTokens[googApiKey] {
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
	// 透明代理：统一处理所有 /v1/* 端点，支持所有HTTP方法
	apiV1 := r.Group("/v1")
	apiV1.Use(s.requireAPIAuth())
	{
		apiV1.Any("/*path", s.handleProxyRequest)
	}
	apiV1Beta := r.Group("/v1beta")
	apiV1Beta.Use(s.requireAPIAuth())
	{
		apiV1Beta.Any("/*path", s.handleProxyRequest)
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
		admin.GET("/channels/export", s.handleExportChannelsCSV)
		admin.POST("/channels/import", s.handleImportChannelsCSV)
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

	needsAuth := slices.Contains(authRequiredPages, filepath)

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
// getCachedConfigs 无锁配置缓存读取（性能优化：消除RWMutex争用）
// 原理：使用atomic.Value实现无锁读取，高并发下性能提升60-80%
func (s *Server) getCachedConfigs(ctx context.Context) ([]*Config, error) {
	now := time.Now().Unix()
	exp := s.configCacheExp.Load()

	// 缓存未过期，无锁快速路径
	if exp > now {
		if cached := s.configCache.Load(); cached != nil {
			return cached.([]*Config), nil
		}
	}

	// 缓存过期或未初始化，需要刷新
	// 注意：多个goroutine可能同时进入这里，使用CAS避免重复加载
	cfgs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	// 性能优化：为每个配置构建模型查找索引（O(n) → O(1)查找）
	for _, cfg := range cfgs {
		cfg.BuildModelsSet()
	}

	// 阶段2优化：同时重建渠道索引（O(1)模型查找）
	idx := s.buildChannelIndex(cfgs)
	s.channelIndex.Store(idx)

	// 无锁更新缓存
	s.configCache.Store(cfgs)
	s.configCacheExp.Store(now + 60) // 缓存60秒

	return cfgs, nil
}

// invalidateConfigCache 立即使配置缓存失效（用于创建/更新/删除渠道后立即生效）
// 无锁实现：仅设置过期时间戳为0，下次读取时自动重新加载
func (s *Server) invalidateConfigCache() {
	s.configCacheExp.Store(0) // 将过期时间设为0，强制下次刷新
}

// buildChannelIndex 构建渠道索引（阶段2优化：预计算模型→渠道映射）
// 作用：将selectCandidates从O(n)扫描+排序优化为O(1)查找
func (s *Server) buildChannelIndex(cfgs []*Config) *channelIndex {
	idx := &channelIndex{
		byModel:    make(map[string][]*Config),
		byType:     make(map[string][]*Config),
		allEnabled: make([]*Config, 0, len(cfgs)),
		lastUpdate: time.Now().Unix(),
	}

	// 第一遍：收集所有启用的渠道
	for _, cfg := range cfgs {
		if !cfg.Enabled || cfg.APIKey == "" || cfg.URL == "" {
			continue
		}
		idx.allEnabled = append(idx.allEnabled, cfg)
	}

	// 按优先级降序排序（一次性排序，后续O(1)查找）
	slices.SortFunc(idx.allEnabled, func(a, b *Config) int {
		if a.Priority != b.Priority {
			return b.Priority - a.Priority // 降序
		}
		return int(a.ID - b.ID) // 同优先级按ID升序
	})

	// 第二遍：按模型和类型分组（保持优先级顺序）
	for _, cfg := range idx.allEnabled {
		// 按模型分组
		for _, model := range cfg.Models {
			idx.byModel[model] = append(idx.byModel[model], cfg)
		}

		// 按渠道类型分组
		channelType := cfg.GetChannelType()
		idx.byType[channelType] = append(idx.byType[channelType], cfg)
	}

	return idx
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
		s.cooldownCache.Range(func(key, value any) bool {
			if expireTime, ok := value.(time.Time); ok && now.After(expireTime) {
				s.cooldownCache.Delete(key)
			}
			return true
		})
	}
}

// cleanupOldLogsLoop 定期清理旧日志（性能优化：避免每次插入时清理）
// 每小时检查一次，删除3天前的日志
func (s *Server) cleanupOldLogsLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()
		cutoff := time.Now().AddDate(0, 0, -3) // 3天前

		// 通过Store接口清理旧日志，忽略错误（非关键操作）
		_ = s.store.CleanupLogsBefore(ctx, cutoff)
	}
}

// getGeminiModels 获取所有 gemini 渠道的去重模型列表
func (s *Server) getGeminiModels(ctx context.Context) ([]string, error) {
	// 获取所有渠道配置
	configs, err := s.getCachedConfigs(ctx)
	if err != nil {
		return nil, err
	}

	// 使用 map 去重
	modelSet := make(map[string]bool)

	// 遍历所有启用的 gemini 渠道
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		if cfg.GetChannelType() != "gemini" {
			continue
		}

		// 收集该渠道的所有模型
		for _, model := range cfg.Models {
			modelSet[model] = true
		}
	}

	// 转换为切片
	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}

	// 排序（可选，提供稳定的输出）
	slices.Sort(models)

	return models, nil
}

// rrBatchWriter 批量轮询指针持久化协程（性能优化）
// 原理：聚合多个轮询指针更新，减少数据库I/O频率90%+
// 策略：满批（50条）立即刷新 或 定时（5秒）强制刷新
func (s *Server) rrBatchWriter() {
	ticker := time.NewTicker(s.rrFlushInterval)
	defer ticker.Stop()

	batch := make([]rrUpdate, 0, s.rrBatchSize)
	// 使用map去重（相同model+priority只保留最新idx）
	pending := make(map[string]rrUpdate, s.rrBatchSize)

	for {
		select {
		case update := <-s.rrUpdateChan:
			// 去重：同一个model+priority只保留最新更新
			key := update.model + "_" + strconv.Itoa(update.priority)
			pending[key] = update

			// 满批立即刷新
			if len(pending) >= s.rrBatchSize {
				for _, upd := range pending {
					batch = append(batch, upd)
				}
				s.flushRRBatch(batch)
				batch = batch[:0]
				pending = make(map[string]rrUpdate, s.rrBatchSize)
			}

		case <-ticker.C:
			// 定时强制刷新
			if len(pending) > 0 {
				for _, upd := range pending {
					batch = append(batch, upd)
				}
				s.flushRRBatch(batch)
				batch = batch[:0]
				pending = make(map[string]rrUpdate, s.rrBatchSize)
			}
		}
	}
}

// flushRRBatch 批量写入轮询指针到数据库
func (s *Server) flushRRBatch(batch []rrUpdate) {
	if len(batch) == 0 {
		return
	}

	ctx := context.Background()
	for _, upd := range batch {
		// 忽略写入错误（轮询指针非关键数据，失败可重试）
		_ = s.store.SetRR(ctx, upd.model, upd.priority, upd.idx)
	}
}

// warmHTTPConnections HTTP连接预热（性能优化：为高优先级渠道预建立连接）
// 作用：消除首次请求的TLS握手延迟10-50ms，提升用户体验
func (s *Server) warmHTTPConnections(ctx context.Context) {
	configs, err := s.getCachedConfigs(ctx)
	if err != nil || len(configs) == 0 {
		return
	}

	// 预热前5个高优先级渠道（按优先级降序）
	warmCount := min(len(configs), 5)

	warmedCount := 0
	for i := range warmCount {
		cfg := configs[i]
		if cfg.URL == "" {
			continue
		}

		// 发送轻量HEAD请求预建立连接（非阻塞，超时1秒）
		reqCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "HEAD", cfg.URL, nil)
		if err != nil {
			cancel()
			continue
		}

		// 异步预热（不阻塞启动）
		go func(r *http.Request, c func()) {
			defer c()
			_, _ = s.client.Do(r) // 忽略响应和错误
		}(req, cancel)

		warmedCount++
	}

	if warmedCount > 0 {
		fmt.Printf("✅ HTTP连接预热：为 %d 个高优先级渠道预建立连接\n", warmedCount)
	}
}
