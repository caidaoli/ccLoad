package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/storage/sqlite"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type Server struct {
	store       storage.Store
	keySelector *KeySelector // Key选择器（多Key支持）
	client      *http.Client
	password    string

	// Token认证系统
	validTokens map[string]time.Time // 动态Token -> 过期时间
	tokensMux   sync.RWMutex

	// API 认证
	authTokens map[string]bool // 静态认证令牌（CCLOAD_AUTH配置）

	// 重试配置
	maxKeyRetries int // 单个渠道内最大Key重试次数（默认3次）

	// 超时配置
	firstByteTimeout time.Duration // 流式请求首字节超时时间（默认2分钟）

	// 性能优化开关
	enableTrace bool // HTTP Trace开关（性能优化：默认关闭，节省0.5-1ms/请求）

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	logChan    chan *model.LogEntry // 异步日志通道
	logWorkers int                  // 日志工作协程数

	// 监控指标（P2优化：实时统计冷却状态）
	channelCooldownGauge atomic.Int64 // 当前活跃的渠道级冷却数量
	keyCooldownGauge     atomic.Int64 // 当前活跃的Key级冷却数量
	logDropCount         atomic.Int64 // 日志丢弃计数器（P1修复 2025-10-05）

	// ✅ P0修复（2025-10-13）：优雅关闭机制
	shutdownCh chan struct{}  // 关闭信号channel
	wg         sync.WaitGroup // 等待所有后台goroutine结束
}

func NewServer(store storage.Store) *Server {
	password := os.Getenv("CCLOAD_PASS")
	if password == "" {
		password = "admin" // 默认密码，生产环境应该设置环境变量
		util.SafePrint("⚠️  安全警告：使用默认密码 'admin'，生产环境必须设置环境变量 CCLOAD_PASS")
	} else if password == "admin" {
		util.SafePrint("⚠️  安全警告：密码设置为 'admin'，建议使用更强的密码")
	} else {
		util.SafePrint("✅ 管理员密码已从环境变量加载（长度: ", len(password), " 字符）")
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
	maxKeyRetries := config.DefaultMaxKeyRetries
	if retryEnv := os.Getenv("CCLOAD_MAX_KEY_RETRIES"); retryEnv != "" {
		if val, err := strconv.Atoi(retryEnv); err == nil && val > 0 {
			maxKeyRetries = val
		}
	}

	// 解析首字节超时时间（流式请求首字节响应超时）
	firstByteTimeout := config.SecondsToDuration(config.DefaultFirstByteTimeout)
	if timeoutEnv := os.Getenv("CCLOAD_FIRST_BYTE_TIMEOUT"); timeoutEnv != "" {
		if val, err := strconv.Atoi(timeoutEnv); err == nil && val > 0 {
			firstByteTimeout = time.Duration(val) * time.Second
		}
	}

	// 解析HTTP Trace开关（性能优化：默认关闭，节省0.5-1ms/请求）
	enableTrace := false
	if traceEnv := os.Getenv("CCLOAD_ENABLE_TRACE"); traceEnv == "1" || traceEnv == "true" {
		enableTrace = true
	}

	// 解析最大并发数（性能优化：防止goroutine爆炸）
	maxConcurrency := config.DefaultMaxConcurrency
	if concEnv := os.Getenv("CCLOAD_MAX_CONCURRENCY"); concEnv != "" {
		if val, err := strconv.Atoi(concEnv); err == nil && val > 0 {
			maxConcurrency = val
		}
	}

	// TLS证书验证配置（安全优化：默认启用证书验证）
	skipTLSVerify := false
	if os.Getenv("CCLOAD_SKIP_TLS_VERIFY") == "true" {
		skipTLSVerify = true
		util.SafePrint("⚠️  警告：TLS证书验证已禁用（CCLOAD_SKIP_TLS_VERIFY=true）")
		util.SafePrint("   仅用于开发/测试环境，生产环境严禁使用！")
		util.SafePrint("   当前配置存在中间人攻击风险，API Key可能泄漏")
	}

	// 优化 HTTP 客户端配置 - 重点优化连接建立阶段的超时控制
	dialer := &net.Dialer{
		Timeout:   config.SecondsToDuration(config.HTTPDialTimeout),
		KeepAlive: config.SecondsToDuration(config.HTTPKeepAliveInterval),
	}

	transport := &http.Transport{
		// ✅ P2连接池优化（2025-10-06）：防御性配置，避免打爆上游API
		MaxIdleConns:        config.HTTPMaxIdleConns,
		MaxIdleConnsPerHost: config.HTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     config.SecondsToDuration(config.HTTPIdleConnTimeout),
		MaxConnsPerHost:     config.HTTPMaxConnsPerHost,

		// ✅ P2握手超时优化（2025-10-06）：仅限制握手阶段，不影响长任务
		// ✅ P2修复（2025-10-12）：修正注释错误，明确各超时配置的作用范围
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   config.SecondsToDuration(config.HTTPTLSHandshakeTimeout),
		ResponseHeaderTimeout: config.SecondsToDuration(config.HTTPResponseHeaderTimeout),
		// 注意：流式请求使用应用层 firstByteTimeout 控制，更精确且支持运行时配置
		ExpectContinueTimeout: config.SecondsToDuration(config.HTTPExpectContinueTimeout),

		// 传输优化
		DisableCompression: false,
		DisableKeepAlives:  false,
		ForceAttemptHTTP2:  false, // 允许自动协议协商，避免HTTP/2超时
		WriteBufferSize:    config.HTTPWriteBufferSize,
		ReadBufferSize:     config.HTTPReadBufferSize,
		// 启用TLS会话缓存，减少重复握手耗时
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(config.TLSSessionCacheSize),
			MinVersion:         tls.VersionTLS12, // 强制 TLS 1.2+
			InsecureSkipVerify: skipTLSVerify,    // 默认false（启用证书验证）
		},
	}

	// 可配置的日志缓冲与工作协程（修复：支持环境变量）
	logBuf := config.DefaultLogBufferSize
	if v := os.Getenv("CCLOAD_LOG_BUFFER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			logBuf = n
		}
	}
	logWorkers := config.DefaultLogWorkers
	if v := os.Getenv("CCLOAD_LOG_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			logWorkers = n
		}
	}

	s := &Server{
		store:            store,
		maxKeyRetries:    maxKeyRetries,    // 单个渠道最大Key重试次数
		firstByteTimeout: firstByteTimeout, // 流式请求首字节超时
		enableTrace:      enableTrace,      // HTTP Trace开关
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 不设置全局超时，避免中断长时间任务
		},
		password:    password,
		validTokens: make(map[string]time.Time),
		authTokens:  authTokens,
		logChan:     make(chan *model.LogEntry, logBuf), // 可配置日志缓冲
		logWorkers:  logWorkers,                         // 可配置日志worker数量

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// ✅ P0修复（2025-10-13）：初始化优雅关闭机制
		shutdownCh: make(chan struct{}),
	}

	// 初始化Key选择器（传递Key冷却监控指标）
	s.keySelector = NewKeySelector(store, &s.keyCooldownGauge)

	// ✅ P0修复（2025-10-13）：启动日志工作协程（支持优雅关闭）
	for i := 0; i < s.logWorkers; i++ {
		s.wg.Add(1)
		go s.logWorker()
	}

	// ✅ P0修复（2025-10-13）：启动后台清理协程（支持优雅关闭）
	s.wg.Add(1)
	go s.tokenCleanupLoop() // Token认证：定期清理过期Token

	s.wg.Add(1)
	go s.cleanupOldLogsLoop() // 定期清理3天前的日志

	return s

}

// ================== Token认证系统 ==================

// 生成安全Token（64字符十六进制）
func (s *Server) generateToken() string {
	b := make([]byte, config.TokenRandomBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// 验证Token有效性（检查过期时间）
func (s *Server) isValidToken(token string) bool {
	s.tokensMux.RLock()
	expiry, exists := s.validTokens[token]
	s.tokensMux.RUnlock()

	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(expiry) {
		// 异步清理过期Token（避免阻塞）
		go func() {
			s.tokensMux.Lock()
			delete(s.validTokens, token)
			s.tokensMux.Unlock()
		}()
		return false
	}

	return true
}

// 清理过期Token（定期任务）
func (s *Server) cleanExpiredTokens() {
	now := time.Now()

	// 使用快照模式避免长时间持锁
	s.tokensMux.RLock()
	toDelete := make([]string, 0, len(s.validTokens)/10)
	for token, expiry := range s.validTokens {
		if now.After(expiry) {
			toDelete = append(toDelete, token)
		}
	}
	s.tokensMux.RUnlock()

	// 批量删除过期Token
	if len(toDelete) > 0 {
		s.tokensMux.Lock()
		for _, token := range toDelete {
			if expiry, exists := s.validTokens[token]; exists && now.After(expiry) {
				delete(s.validTokens, token)
			}
		}
		s.tokensMux.Unlock()
	}
}

// ================== End Token认证 ==================

// 统一Token认证中间件（管理界面 + API统一使用）
func (s *Server) requireTokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 优先从 Authorization 头获取Token
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token := strings.TrimPrefix(authHeader, prefix)

				// 检查动态Token（登录生成的24小时Token）
				if s.isValidToken(token) {
					c.Next()
					return
				}

				// 检查静态Token（CCLOAD_AUTH配置的永久Token）
				if len(s.authTokens) > 0 && s.authTokens[token] {
					c.Next()
					return
				}
			}
		}

		// 未授权
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未授权访问，请先登录"})
		c.Abort()
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

// 登录处理程序 - Token认证版本（替代Cookie Session）
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

	// 密码正确，生成Token
	token := s.generateToken()

	// 存储Token到内存
	s.tokensMux.Lock()
	s.validTokens[token] = time.Now().Add(config.HoursToDuration(config.TokenExpiryHours))
	s.tokensMux.Unlock()

	// 返回Token给客户端（前端存储到localStorage）
	c.JSON(http.StatusOK, gin.H{
		"status":    "success",
		"token":     token,
		"expiresIn": config.TokenExpiryHours * 3600, // 秒数
	})
}

// 登出处理程序 - Token认证版本
func (s *Server) handleLogout(c *gin.Context) {
	// 从Authorization头提取Token
	authHeader := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(authHeader, prefix) {
		token := strings.TrimPrefix(authHeader, prefix)

		// 删除服务器端Token
		s.tokensMux.Lock()
		delete(s.validTokens, token)
		s.tokensMux.Unlock()
	}

	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "已登出"})
}

// SetupRoutes - 新的路由设置函数，适配Gin
func (s *Server) SetupRoutes(r *gin.Engine) {
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
		public.GET("/channel-types", s.handleGetChannelTypes)
	}

	// 登录相关（公开访问）
	r.POST("/login", s.handleLogin)
	r.POST("/logout", s.handleLogout) // 改为POST（前端需携带Token）

	// 需要身份验证的admin APIs（使用Token认证）
	admin := r.Group("/admin")
	admin.Use(s.requireTokenAuth())
	{
		admin.GET("/channels", s.handleChannels)
		admin.POST("/channels", s.handleChannels)
		admin.GET("/channels/export", s.handleExportChannelsCSV)
		admin.POST("/channels/import", s.handleImportChannelsCSV)
		admin.GET("/channels/:id", s.handleChannelByID)
		admin.PUT("/channels/:id", s.handleChannelByID)
		admin.DELETE("/channels/:id", s.handleChannelByID)
		admin.GET("/channels/:id/keys", s.handleChannelKeys) // ✅ 修复：获取渠道API Keys
		admin.POST("/channels/:id/test", s.handleChannelTest)
		admin.POST("/channels/:id/cooldown", s.handleSetChannelCooldown)            // 设置渠道级别冷却
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.handleSetKeyCooldown) // 设置Key级别冷却
		admin.GET("/errors", s.handleErrors)
		admin.GET("/metrics", s.handleMetrics)
		admin.GET("/stats", s.handleStats)
		admin.GET("/cooldown/stats", s.handleCooldownStats) // P2优化：冷却状态监控
	}

	// 静态文件服务
	r.GET("/web/*filepath", s.handleWebFiles)

	// 默认首页重定向
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/index.html")
	})
}

// 处理web静态文件 - Gin版本
// 注意：认证已迁移到Token机制，由前端fetchWithAuth()和后端API中间件处理
func (s *Server) handleWebFiles(c *gin.Context) {
	filepath := c.Param("filepath")
	c.File("web" + filepath)
}

// Token清理循环（定期清理过期Token）
// ✅ P0修复（2025-10-13）：支持优雅关闭
func (s *Server) tokenCleanupLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(config.HoursToDuration(config.TokenCleanupIntervalHours))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanExpiredTokens()
		case <-s.shutdownCh:
			// 收到关闭信号，执行最后一次清理后退出
			s.cleanExpiredTokens()
			return
		}
	}
}

// 异步日志工作协程
// ✅ P0修复（2025-10-13）：支持优雅关闭
func (s *Server) logWorker() {
	defer s.wg.Done()

	batch := make([]*model.LogEntry, 0, config.LogBatchSize)
	timer := time.NewTimer(config.SecondsToDuration(config.LogBatchTimeout))
	defer timer.Stop()

	for {
		select {
		case entry := <-s.logChan:
			batch = append(batch, entry)
			if len(batch) >= config.LogBatchSize {
				s.flushLogs(batch)
				batch = batch[:0]
			}
			timer.Reset(config.SecondsToDuration(config.LogBatchTimeout))

		case <-timer.C:
			if len(batch) > 0 {
				s.flushLogs(batch)
				batch = batch[:0]
			}

		case <-s.shutdownCh:
			// 收到关闭信号，刷新剩余日志后退出
			if len(batch) > 0 {
				s.flushLogs(batch)
			}
			return
		}
	}
}

// 批量写入日志
func (s *Server) flushLogs(logs []*model.LogEntry) {
	ctx := context.Background()
	// 优先使用SQLite批量写入，加速刷盘
	if ss, ok := s.store.(*sqlite.SQLiteStore); ok {
		_ = ss.BatchAddLogs(ctx, logs)
		return
	}
	// 回退逐条写入
	for _, e := range logs {
		_ = s.store.AddLog(ctx, e)
	}
}

// 异步添加日志
// P1修复 (2025-10-05): 添加丢弃计数和告警机制
func (s *Server) addLogAsync(entry *model.LogEntry) {
	select {
	case s.logChan <- entry:
		// 成功放入队列
	default:
		// 队列满，丢弃日志并计数
		dropCount := s.logDropCount.Add(1)

		// 告警阈值：定期打印警告
		if dropCount%config.LogDropAlertThreshold == 0 {
			util.SafePrintf("⚠️  严重警告: 日志丢弃计数达到 %d 条！请检查系统负载或增加日志队列容量", dropCount)
			util.SafePrint("   建议: 1) 增加CCLOAD_LOG_BUFFER环境变量 2) 增加日志Worker数量 3) 优化磁盘I/O性能")
		}
	}
}

// cleanupOldLogsLoop 定期清理旧日志（性能优化：避免每次插入时清理）
// 每小时检查一次，删除3天前的日志
// ✅ P0修复（2025-10-13）：支持优雅关闭
func (s *Server) cleanupOldLogsLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx := context.Background()
			cutoff := time.Now().AddDate(0, 0, -3) // 3天前

			// 通过Store接口清理旧日志，忽略错误（非关键操作）
			_ = s.store.CleanupLogsBefore(ctx, cutoff)

		case <-s.shutdownCh:
			// 收到关闭信号，直接退出（不执行最后一次清理）
			return
		}
	}
}

// getGeminiModels 获取所有 gemini 渠道的去重模型列表
func (s *Server) getGeminiModels(ctx context.Context) ([]string, error) {
	// 直接从数据库查询 gemini 渠道
	configs, err := s.store.GetEnabledChannelsByType(ctx, "gemini")
	if err != nil {
		return nil, err
	}

	// 使用 map 去重
	modelSet := make(map[string]bool)

	// 遍历所有 gemini 渠道
	for _, cfg := range configs {

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

// WarmHTTPConnections HTTP连接预热（性能优化：为高优先级渠道预建立连接）
// 作用：消除首次请求的TLS握手延迟10-50ms，提升用户体验
func (s *Server) WarmHTTPConnections(ctx context.Context) {
	// 直接从数据库查询所有启用的渠道（已按优先级排序）
	configs, err := s.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil || len(configs) == 0 {
		return
	}

	// 预热高优先级渠道（按优先级降序）
	warmCount := min(len(configs), config.CacheWarmupChannelCount)

	warmedCount := 0
	for i := 0; i < warmCount; i++ {
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
			resp, err := s.client.Do(r)
			if err == nil && resp != nil && resp.Body != nil {
				// 正确关闭响应体，防止连接泄漏
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(req, cancel)

		warmedCount++
	}

	if warmedCount > 0 {
		util.SafePrintf("✅ HTTP连接预热：为 %d 个高优先级渠道预建立连接", warmedCount)
	}
}

// ✅ 修复：handleChannelKeys 路由处理器(2025-10新架构支持)
// GET /admin/channels/:id/keys - 获取渠道的所有API Keys
func (s *Server) handleChannelKeys(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	s.handleGetChannelKeys(c, id)
}

// ✅ P0修复（2025-10-13）：优雅关闭Server
// Shutdown 优雅关闭Server，等待所有后台goroutine完成
// 参数ctx用于控制最大等待时间，超时后强制退出
// 返回值：nil表示成功，context.DeadlineExceeded表示超时
func (s *Server) Shutdown(ctx context.Context) error {
	util.SafePrint("🛑 正在关闭Server，等待后台任务完成...")

	// 关闭shutdownCh，通知所有goroutine退出
	close(s.shutdownCh)

	// 使用channel等待所有goroutine完成
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// 等待完成或超时
	select {
	case <-done:
		util.SafePrint("✅ Server优雅关闭完成")
		return nil
	case <-ctx.Done():
		util.SafePrint("⚠️  Server关闭超时，部分后台任务可能未完成")
		return ctx.Err()
	}
}
