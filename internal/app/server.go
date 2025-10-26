package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/storage/sqlite"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
)

type Server struct {
	store           storage.Store
	keySelector     *KeySelector      // Key选择器（多Key支持）
	cooldownManager *cooldown.Manager // ✅ P2重构：统一冷却管理器（DRY原则）
	client          *http.Client
	password        string

	// Token认证系统
	validTokens map[string]time.Time // 动态Token -> 过期时间
	tokensMux   sync.RWMutex

	// API 认证
	authTokens map[string]bool // 静态认证令牌（CCLOAD_AUTH配置）

	// ✅ P2安全加固：登录速率限制
	loginRateLimiter *util.LoginRateLimiter // 防暴力破解

	// 重试配置
	maxKeyRetries int // 单个渠道内最大Key重试次数（默认3次）

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	logChan      chan *model.LogEntry // 异步日志通道
	logWorkers   int                  // 日志工作协程数
	logDropCount atomic.Int64         // 日志丢弃计数器（P1修复 2025-10-05）

	// ✅ P0修复（2025-10-13）：优雅关闭机制
	shutdownCh chan struct{}  // 关闭信号channel
	wg         sync.WaitGroup // 等待所有后台goroutine结束
}

func NewServer(store storage.Store) *Server {
	password := os.Getenv("CCLOAD_PASS")
	if password == "" {
		util.SafePrint("❌ 未设置 CCLOAD_PASS，出于安全原因程序将退出。请设置强管理员密码后重试。")
		os.Exit(1)
	}

	util.SafePrint("✅ 管理员密码已从环境变量加载（长度: ", len(password), " 字符）")

	// 解析 API 认证令牌
	authTokens := make(map[string]bool)
	if authEnv := os.Getenv("CCLOAD_AUTH"); authEnv != "" {
		tokens := strings.SplitSeq(authEnv, ",")
		for token := range tokens {
			token = strings.TrimSpace(token)
			if token != "" {
				authTokens[token] = true
			}
		}
	}

	// ✅ P0安全修复：生产环境强制检查 CCLOAD_AUTH
	// 设计原则：Fail-Fast，避免生产环境配置错误导致安全风险
	ginMode := os.Getenv("GIN_MODE")
	if ginMode != "debug" && ginMode != "test" && len(authTokens) == 0 {
		util.SafePrint("❌ 严重错误：生产环境必须设置 CCLOAD_AUTH 环境变量以保护 API 端点")
		util.SafePrint("   当前模式: " + ginMode)
		util.SafePrint("   请设置格式：CCLOAD_AUTH=token1,token2,token3")
		util.SafePrint("   建议生成方法：openssl rand -hex 32")
		os.Exit(1)
	}

	if len(authTokens) == 0 {
		util.SafePrint("⚠️  警告：未设置 CCLOAD_AUTH，所有 /v1/* API 请求将被拒绝（401）")
	} else {
		util.SafePrint("✅ API 认证已启用（" + strconv.Itoa(len(authTokens)) + " 个令牌配置）")
	}

	// 解析最大Key重试次数（避免key过多时重试次数过多）
	maxKeyRetries := config.DefaultMaxKeyRetries
	if retryEnv := os.Getenv("CCLOAD_MAX_KEY_RETRIES"); retryEnv != "" {
		if val, err := strconv.Atoi(retryEnv); err == nil && val > 0 {
			maxKeyRetries = val
		}
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
	// ✅ P2优化（2025-10-17）：启用TCP_NODELAY降低SSE首包延迟5~15ms
	dialer := &net.Dialer{
		Timeout:   config.SecondsToDuration(config.HTTPDialTimeout),
		KeepAlive: config.SecondsToDuration(config.HTTPKeepAliveInterval),
		// 禁用Nagle算法，立即发送小包数据（SSE事件通常<2KB）
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// 设置TCP_NODELAY=1，禁用Nagle算法
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			})
		},
	}

	transport := &http.Transport{
		// ✅ P2连接池优化（2025-10-06）：防御性配置，避免打爆上游API
		MaxIdleConns:        config.HTTPMaxIdleConns,
		MaxIdleConnsPerHost: config.HTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     config.SecondsToDuration(config.HTTPIdleConnTimeout),
		MaxConnsPerHost:     config.HTTPMaxConnsPerHost,

		// 连接建立超时（保留必要的底层网络超时）
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: config.SecondsToDuration(config.HTTPTLSHandshakeTimeout),

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

	// ✅ P1优化（2025-10-17）：启用HTTP/2降低头部开销10~20ms
	// 优势：头部压缩、多路复用、服务器推送
	if err := http2.ConfigureTransport(transport); err != nil {
		util.SafePrint("⚠️  警告：HTTP/2配置失败，将使用HTTP/1.1: ", err.Error())
	} else {
		util.SafePrint("✅ HTTP/2已启用（头部压缩+多路复用）")
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
		store:         store,
		maxKeyRetries: maxKeyRetries, // 单个渠道最大Key重试次数
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 不设置全局超时，避免中断长时间任务
		},
		password:         password,
		validTokens:      make(map[string]time.Time),
		authTokens:       authTokens,
		loginRateLimiter: util.NewLoginRateLimiter(),         // ✅ P2安全加固：登录速率限制
		logChan:          make(chan *model.LogEntry, logBuf), // 可配置日志缓冲
		logWorkers:       logWorkers,                         // 可配置日志worker数量

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// ✅ P0修复（2025-10-13）：初始化优雅关闭机制
		shutdownCh: make(chan struct{}),
	}

	// ✅ P2重构：初始化冷却管理器（统一管理渠道级和Key级冷却）
	s.cooldownManager = cooldown.NewManager(store)

	// 初始化Key选择器
	s.keySelector = NewKeySelector(store, nil)

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

// ================== Token认证 ==================
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
		// ✅ P0修复（2025-10-16）：同步删除过期Token（避免goroutine泄漏）
		// 原因：map删除操作非常快（O(1)），无需异步，异步反而导致goroutine泄漏
		s.tokensMux.Lock()
		delete(s.validTokens, token)
		s.tokensMux.Unlock()
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
		// 未配置认证令牌时，默认全部返回 401（不允许公开访问）
		if len(s.authTokens) == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
			c.Abort()
			return
		}

		// 检查 Authorization 头（Bearer token）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
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
// ✅ P2安全加固：集成登录速率限制，防暴力破解
func (s *Server) handleLogin(c *gin.Context) {
	clientIP := c.ClientIP()

	// ✅ P2安全加固：检查速率限制
	if !s.loginRateLimiter.AllowAttempt(clientIP) {
		lockoutTime := s.loginRateLimiter.GetLockoutTime(clientIP)
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":           "Too many failed login attempts",
			"message":         fmt.Sprintf("Account locked for %d seconds. Please try again later.", lockoutTime),
			"lockout_seconds": lockoutTime,
		})
		return
	}

	var req struct {
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	// 验证密码
	if req.Password != s.password {
		// ✅ P2安全加固：记录失败尝试（速率限制器已在AllowAttempt中增加计数）
		attemptCount := s.loginRateLimiter.GetAttemptCount(clientIP)
		util.SafePrintf("⚠️  登录失败: IP=%s, 尝试次数=%d/5", clientIP, attemptCount)

		c.JSON(http.StatusUnauthorized, gin.H{
			"error":              "Invalid password",
			"remaining_attempts": 5 - attemptCount,
		})
		return
	}

	// ✅ P2安全加固：密码正确，重置速率限制
	s.loginRateLimiter.RecordSuccess(clientIP)

	// 生成Token
	token := s.generateToken()

	// 存储Token到内存
	s.tokensMux.Lock()
	s.validTokens[token] = time.Now().Add(config.HoursToDuration(config.TokenExpiryHours))
	s.tokensMux.Unlock()

	util.SafePrintf("✅ 登录成功: IP=%s", clientIP)

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
	if after, ok := strings.CutPrefix(authHeader, prefix); ok {
		token := after

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

	// 静态文件服务（安全）：使用框架自带的静态文件路由，自动做路径清理，防止目录遍历
	// 等价于 http.FileServer，避免手工拼接路径导致的 /web/../ 泄露
	r.Static("/web", "./web")

	// 默认首页重定向
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/index.html")
	})
}

// 说明：已改为使用 r.Static("/web", "./web") 提供静态文件服务，
// 该实现会自动进行路径清理和越界防护，避免目录遍历风险。

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
			// 收到关闭信号，尽快刷新剩余日志并有限期地清空队列后退出
			deadline := time.Now().Add(200 * time.Millisecond)
			// 先尽量从队列中取出更多日志，避免遗漏
			for {
				select {
				case e := <-s.logChan:
					batch = append(batch, e)
					if len(batch) >= config.LogBatchSize {
						s.flushLogs(batch)
						batch = batch[:0]
					}
				default:
					// 无更多日志或时间到
					if time.Now().After(deadline) {
						goto FLUSH_AND_EXIT
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		FLUSH_AND_EXIT:
			if len(batch) > 0 {
				s.flushLogs(batch)
				batch = batch[:0]
			}
			return
		}
	}
}

// 批量写入日志
func (s *Server) flushLogs(logs []*model.LogEntry) {
	// 为日志持久化增加超时控制，避免阻塞关闭或积压
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LogFlushTimeoutMs)*time.Millisecond)
	defer cancel()

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

	ticker := time.NewTicker(config.HoursToDuration(config.LogCleanupIntervalHours))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// ✅ P0-3修复：使用带超时的context，避免日志清理阻塞关闭流程
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cutoff := time.Now().AddDate(0, 0, -config.LogRetentionDays)

			// 通过Store接口清理旧日志，忽略错误（非关键操作）
			_ = s.store.CleanupLogsBefore(ctx, cutoff)
			cancel() // 立即释放资源

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
// ✅ P0修复（2025-10-16）：等待所有预热goroutine完成，避免goroutine泄漏
func (s *Server) WarmHTTPConnections(ctx context.Context) {
	// 直接从数据库查询所有启用的渠道（已按优先级排序）
	configs, err := s.store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil || len(configs) == 0 {
		return
	}

	// 预热高优先级渠道（按优先级降序）
	warmCount := min(len(configs), config.CacheWarmupChannelCount)

	// ✅ 使用WaitGroup等待所有预热goroutine完成
	var wg sync.WaitGroup
	warmedCount := 0

	for i := 0; i < warmCount; i++ {
		cfg := configs[i]
		if cfg.URL == "" {
			continue
		}

		// 发送轻量HEAD请求预建立连接（超时1秒）
		reqCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "HEAD", cfg.URL, nil)
		if err != nil {
			cancel()
			continue
		}

		// 异步预热（使用WaitGroup跟踪）
		wg.Add(1)
		go func(r *http.Request, c func()) {
			defer wg.Done()
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

	// 等待所有预热完成
	wg.Wait()

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

	// ✅ P0修复（2025-10-16）：停止LoginRateLimiter的cleanupLoop
	s.loginRateLimiter.Stop()

	// 使用channel等待所有goroutine完成
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// 等待完成或超时
	select {
	case <-done:
		// ✅ P0-2 修复：关闭数据库连接，防止 goroutine 泄漏
		// SQLiteStore 创建了 2 个 database/sql.connectionOpener goroutine
		// 必须显式调用 Close() 才能清理这些 goroutine
		if closer, ok := s.store.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				util.SafePrintf("❌ 关闭数据库连接失败: %v", err)
			}
		}

		util.SafePrint("✅ Server优雅关闭完成")
		return nil
	case <-ctx.Done():
		util.SafePrint("⚠️  Server关闭超时，部分后台任务可能未完成")
		return ctx.Err()
	}
}
