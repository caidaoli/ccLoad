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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/service"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
)

type Server struct {
	// ============================================================================
	// 服务层（仅保留有价值的服务）
	// ============================================================================
	authService *service.AuthService // 认证授权服务
	logService  *service.LogService  // 日志管理服务

	// ============================================================================
	// 核心字段
	// ============================================================================
	store            storage.Store
	channelCache     *storage.ChannelCache // 高性能渠道缓存层
	keySelector      *KeySelector          // Key选择器（多Key支持）
	cooldownManager  *cooldown.Manager     // 统一冷却管理器（DRY原则）
	client           *http.Client
	firstByteTimeout time.Duration

	// ============================================================================
	// ⚠️ DEPRECATED: 以下字段已迁移到 AuthService（阶段 3）
	// 请使用 s.authService 访问认证相关功能
	// ============================================================================
	password         string                 // DEPRECATED: 已迁移到 authService.password
	validTokens      map[string]time.Time   // DEPRECATED: 已迁移到 authService.validTokens
	tokensMux        sync.RWMutex           // DEPRECATED: 已迁移到 authService.tokensMux
	authTokens       map[string]bool        // DEPRECATED: 已迁移到 authService.authTokens
	loginRateLimiter *util.LoginRateLimiter // 仍需保留：用于传递给 authService

	// 重试配置
	maxKeyRetries int // 单个渠道内最大Key重试次数（默认3次）

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	// 优雅关闭机制
	shutdownCh     chan struct{}  // 关闭信号channel
	isShuttingDown atomic.Bool    // shutdown标志，防止向已关闭channel写入
	wg             sync.WaitGroup // 等待所有后台goroutine结束
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

	// 生产环境强制检查 CCLOAD_AUTH
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

	// 解析上游首字节超时阈值（可选，单位：秒）
	var firstByteTimeout time.Duration
	if v := os.Getenv("CCLOAD_UPSTREAM_FIRST_BYTE_TIMEOUT"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			firstByteTimeout = time.Duration(sec) * time.Second
			util.SafePrintf("⏱️  上游首字节超时阈值已启用：%v", firstByteTimeout)
		} else {
			util.SafePrintf("⚠️  无法解析 CCLOAD_UPSTREAM_FIRST_BYTE_TIMEOUT=%q，已忽略", v)
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
	// 启用TCP_NODELAY降低SSE首包延迟5~15ms
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
		// 防御性配置，避免打爆上游API
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

	if firstByteTimeout > 0 {
		transport.ResponseHeaderTimeout = firstByteTimeout
	}

	// 启用HTTP/2降低头部开销10~20ms
	// 优势：头部压缩、多路复用、服务器推送
	if err := http2.ConfigureTransport(transport); err != nil {
		util.SafePrint("⚠️  警告：HTTP/2配置失败，将使用HTTP/1.1: " + err.Error())
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
		firstByteTimeout: firstByteTimeout,
		password:         password,
		validTokens:      make(map[string]time.Time),
		authTokens:       authTokens,
		loginRateLimiter: util.NewLoginRateLimiter(), // 登录速率限制

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// 初始化优雅关闭机制
		shutdownCh: make(chan struct{}),
	}

	// 初始化高性能缓存层（60秒TTL，避免数据库性能杀手查询）
	s.channelCache = storage.NewChannelCache(store, 60*time.Second)

	// 初始化冷却管理器（统一管理渠道级和Key级冷却）
	s.cooldownManager = cooldown.NewManager(store)

	// 初始化Key选择器（移除store依赖，避免重复查询）
	s.keySelector = NewKeySelector(nil)

	// ============================================================================
	// 创建服务层（仅保留有价值的服务）
	// ============================================================================

	// 1. LogService（负责日志管理）
	s.logService = service.NewLogService(
		store,
		logBuf,
		logWorkers,
		s.shutdownCh,
		&s.isShuttingDown,
		&s.wg,
	)
	// 启动日志 Workers 和清理协程
	s.logService.StartWorkers()
	s.logService.StartCleanupLoop()

	// 2. AuthService（负责认证授权）
	s.authService = service.NewAuthService(
		password,
		authTokens,
		s.loginRateLimiter,
	)

	// 启动后台清理协程（Token 认证）
	s.wg.Add(1)
	go s.tokenCleanupLoop() // 定期清理过期Token

	return s

}

// ================== 缓存辅助函数 ==================

func (s *Server) getChannelCache() *storage.ChannelCache {
	if s == nil {
		return nil
	}
	return s.channelCache
}

func (s *Server) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		if channels, err := cache.GetEnabledChannelsByModel(ctx, model); err == nil {
			return channels, nil
		}
	}
	return s.store.GetEnabledChannelsByModel(ctx, model)
}

func (s *Server) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		if channels, err := cache.GetEnabledChannelsByType(ctx, channelType); err == nil {
			return channels, nil
		}
	}
	return s.store.GetEnabledChannelsByType(ctx, channelType)
}

func (s *Server) getAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	if cache := s.getChannelCache(); cache != nil {
		if keys, err := cache.GetAPIKeys(ctx, channelID); err == nil {
			return keys, nil
		}
	}
	return s.store.GetAPIKeys(ctx, channelID)
}

func (s *Server) getAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	if cache := s.getChannelCache(); cache != nil {
		if cooldowns, err := cache.GetAllChannelCooldowns(ctx); err == nil {
			return cooldowns, nil
		}
	}
	return s.store.GetAllChannelCooldowns(ctx)
}

func (s *Server) getAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	if cache := s.getChannelCache(); cache != nil {
		if cooldowns, err := cache.GetAllKeyCooldowns(ctx); err == nil {
			return cooldowns, nil
		}
	}
	return s.store.GetAllKeyCooldowns(ctx)
}

// InvalidateChannelListCache 使渠道列表缓存失效
//
// DEPRECATED: 此方法已迁移到 ProxyService.InvalidateCache()（阶段 2）
// 请使用 s.proxyService.InvalidateCache() 替代
func (s *Server) InvalidateChannelListCache() {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateCache()
	}
}

// InvalidateAPIKeysCache 使指定渠道的 API Keys 缓存失效
//
// DEPRECATED: 此方法已迁移到 ProxyService.InvalidateAPIKeysCache()（阶段 2）
// 请使用 s.proxyService.InvalidateAPIKeysCache(channelID) 替代
func (s *Server) InvalidateAPIKeysCache(channelID int64) {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateAPIKeysCache(channelID)
	}
}

// InvalidateAllAPIKeysCache 使所有 API Keys 缓存失效
//
// DEPRECATED: 此方法已迁移到 ProxyService.InvalidateAllAPIKeysCache()（阶段 2）
// 请使用 s.proxyService.InvalidateAllAPIKeysCache() 替代
func (s *Server) InvalidateAllAPIKeysCache() {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateAllAPIKeysCache()
	}
}

func (s *Server) invalidateCooldownCache() {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateCooldownCache()
	}
}

// ================== Token认证系统 ==================

// generateToken 生成安全Token（64字符十六进制）
//
// DEPRECATED: 此方法已迁移到 AuthService（阶段 3）
// 内部方法，不对外暴露。新代码请勿使用。
func (s *Server) generateToken() string {
	b := make([]byte, config.TokenRandomBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ================== Token认证 ==================

// isValidToken 验证Token有效性（检查过期时间）
//
// DEPRECATED: 此方法已迁移到 AuthService（阶段 3）
// 内部方法，不对外暴露。新代码请勿使用。
func (s *Server) isValidToken(token string) bool {
	s.tokensMux.RLock()
	expiry, exists := s.validTokens[token]
	s.tokensMux.RUnlock()

	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(expiry) {
		// 同步删除过期Token（避免goroutine泄漏）
		// 原因：map删除操作非常快（O(1)），无需异步，异步反而导致goroutine泄漏
		s.tokensMux.Lock()
		delete(s.validTokens, token)
		s.tokensMux.Unlock()
		return false
	}

	return true
}

// ================== End Token认证 ==================

// RequireTokenAuth 统一Token认证中间件（管理界面 + API统一使用）
//
// DEPRECATED: 此方法已迁移到 AuthService.RequireTokenAuth()（阶段 3）
// 请使用 s.authService.RequireTokenAuth() 替代
func (s *Server) RequireTokenAuth() gin.HandlerFunc {
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

// RequireAPIAuth API 认证中间件 - Gin版本
//
// DEPRECATED: 此方法已迁移到 AuthService.RequireAPIAuth()（阶段 3）
// 请使用 s.authService.RequireAPIAuth() 替代
func (s *Server) RequireAPIAuth() gin.HandlerFunc {
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

// HandleLogin 登录处理程序 - Token认证版本（替代Cookie Session）
// 集成登录速率限制，防暴力破解
//
// DEPRECATED: 此方法已迁移到 AuthService.HandleLogin()（阶段 3）
// 请使用 s.authService.HandleLogin(c) 替代
func (s *Server) HandleLogin(c *gin.Context) {
	clientIP := c.ClientIP()

	// 检查速率限制
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
		// 记录失败尝试（速率限制器已在AllowAttempt中增加计数）
		attemptCount := s.loginRateLimiter.GetAttemptCount(clientIP)
		util.SafePrintf("⚠️  登录失败: IP=%s, 尝试次数=%d/5", clientIP, attemptCount)

		c.JSON(http.StatusUnauthorized, gin.H{
			"error":              "Invalid password",
			"remaining_attempts": 5 - attemptCount,
		})
		return
	}

	// 密码正确，重置速率限制
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

// HandleLogout 登出处理程序 - Token认证版本
//
// DEPRECATED: 此方法已迁移到 AuthService.HandleLogout()（阶段 3）
// 请使用 s.authService.HandleLogout(c) 替代
func (s *Server) HandleLogout(c *gin.Context) {
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
	apiV1.Use(s.authService.RequireAPIAuth())
	{
		apiV1.Any("/*path", s.HandleProxyRequest)
	}
	apiV1Beta := r.Group("/v1beta")
	apiV1Beta.Use(s.authService.RequireAPIAuth())
	{
		apiV1Beta.Any("/*path", s.HandleProxyRequest)
	}

	// 公开访问的API（基础统计）
	public := r.Group("/public")
	{
		public.GET("/summary", s.HandlePublicSummary)
		public.GET("/channel-types", s.HandleGetChannelTypes)
	}

	// 登录相关（公开访问）
	r.POST("/login", s.authService.HandleLogin)
	r.POST("/logout", s.authService.HandleLogout)

	// 需要身份验证的admin APIs（使用Token认证）
	admin := r.Group("/admin")
	admin.Use(s.authService.RequireTokenAuth())
	{
		// 渠道管理
		admin.GET("/channels", s.HandleChannels)
		admin.POST("/channels", s.HandleChannels)
		admin.GET("/channels/export", s.HandleExportChannelsCSV)
		admin.POST("/channels/import", s.HandleImportChannelsCSV)
		admin.GET("/channels/:id", s.HandleChannelByID)
		admin.PUT("/channels/:id", s.HandleChannelByID)
		admin.DELETE("/channels/:id", s.HandleChannelByID)
		admin.GET("/channels/:id/keys", s.HandleChannelKeys)
		admin.POST("/channels/:id/test", s.HandleChannelTest)
		admin.POST("/channels/:id/cooldown", s.HandleSetChannelCooldown)
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.HandleSetKeyCooldown)

		// 统计分析
		admin.GET("/errors", s.HandleErrors)
		admin.GET("/metrics", s.HandleMetrics)
		admin.GET("/stats", s.HandleStats)
		admin.GET("/cooldown/stats", s.HandleCooldownStats)
		admin.GET("/cache/stats", s.HandleCacheStats)
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
// 支持优雅关闭
func (s *Server) tokenCleanupLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(config.HoursToDuration(config.TokenCleanupIntervalHours))
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			// 优先检查shutdown信号,快速响应关闭
			// 移除shutdown时的额外清理,避免潜在的死锁或延迟
			// Token清理不是关键路径,可以在下次启动时清理过期Token
			return
		case <-ticker.C:
			s.authService.CleanExpiredTokens()
		}
	}
}

// AddLogAsync 异步添加日志
// 添加丢弃计数和告警机制
//
// DEPRECATED: 此方法已迁移到 LogService.AddLogAsync()（阶段 4）
// 请使用 s.logService.AddLogAsync(entry) 替代
func (s *Server) AddLogAsync(entry *model.LogEntry) {
	// 委托给 LogService 处理日志写入
	s.logService.AddLogAsync(entry)
}

// getGeminiModels 获取所有 gemini 渠道的去重模型列表
func (s *Server) getGeminiModels(ctx context.Context) ([]string, error) {
	if cache := s.getChannelCache(); cache != nil {
		if models, err := cache.GetGeminiModels(ctx); err == nil {
			return models, nil
		}
	}

	// 缓存不可用时退化：按渠道类型查询并去重模型
	channels, err := s.store.GetEnabledChannelsByType(ctx, util.ChannelTypeGemini)
	if err != nil {
		return nil, err
	}
	modelSet := make(map[string]struct{})
	for _, cfg := range channels {
		for _, modelName := range cfg.Models {
			modelSet[modelName] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}
	return models, nil
}

// WarmHTTPConnections HTTP连接预热（性能优化：为高优先级渠道预建立连接）
// 作用：消除首次请求的TLS握手延迟10-50ms，提升用户体验
// 等待所有预热goroutine完成，避免goroutine泄漏
func (s *Server) WarmHTTPConnections(ctx context.Context) {
	// 使用缓存层查询所有启用的渠道（已按优先级排序，避免数据库性能杀手）
	configs, err := s.GetEnabledChannelsByModel(ctx, "*")
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
func (s *Server) HandleChannelKeys(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	s.handleGetChannelKeys(c, id)
}

// 优雅关闭Server
// Shutdown 优雅关闭Server，等待所有后台goroutine完成
// 参数ctx用于控制最大等待时间，超时后强制退出
// 返回值：nil表示成功，context.DeadlineExceeded表示超时
func (s *Server) Shutdown(ctx context.Context) error {
	util.SafePrint("🛑 正在关闭Server，等待后台任务完成...")

	// 设置shutdown标志，防止新的日志写入
	s.isShuttingDown.Store(true)

	// 关闭shutdownCh，通知所有goroutine退出
	close(s.shutdownCh)

	// ✅ 修复: 关闭 LogService 的 logChan，让 logWorker 更快退出
	// 由于 isShuttingDown 已设置，AddLogAsync 不会再写入日志，可以安全关闭
	s.logService.Shutdown(ctx)

	// 停止LoginRateLimiter的cleanupLoop
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
		// 关闭数据库连接，防止 goroutine 泄漏
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
