package app

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"ccLoad/internal/validator"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
)

type Server struct {
	// ============================================================================
	// 服务层（仅保留有价值的服务）
	// ============================================================================
	authService *AuthService // 认证授权服务
	logService  *LogService  // 日志管理服务

	// ============================================================================
	// 核心字段
	// ============================================================================
	store            storage.Store
	channelCache     *storage.ChannelCache // 高性能渠道缓存层
	keySelector      *KeySelector          // Key选择器（多Key支持）
	cooldownManager  *cooldown.Manager     // 统一冷却管理器（DRY原则）
	validatorManager *validator.Manager    // 渠道验证器管理器（SRP+OCP原则）
	client           *http.Client
	firstByteTimeout time.Duration

	// 登录速率限制器（用于传递给AuthService）
	loginRateLimiter *util.LoginRateLimiter

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
		log.Print("❌ 未设置 CCLOAD_PASS，出于安全原因程序将退出。请设置强管理员密码后重试。")
		os.Exit(1)
	}

	log.Print("✅ 管理员密码已从环境变量加载（长度: ", len(password), " 字符）")
	log.Print("ℹ️  API访问令牌将从数据库动态加载（支持Web界面管理）")

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
			log.Printf("⏱️  上游首字节超时阈值已启用：%v", firstByteTimeout)
		} else {
			log.Printf("⚠️  无法解析 CCLOAD_UPSTREAM_FIRST_BYTE_TIMEOUT=%q，已忽略", v)
		}
	}

	// TLS证书验证配置（安全优化：默认启用证书验证）
	skipTLSVerify := false
	if os.Getenv("CCLOAD_SKIP_TLS_VERIFY") == "true" {
		skipTLSVerify = true
		log.Print("⚠️  警告：TLS证书验证已禁用（CCLOAD_SKIP_TLS_VERIFY=true）")
		log.Print("   仅用于开发/测试环境，生产环境严禁使用！")
		log.Print("   当前配置存在中间人攻击风险，API Key可能泄漏")
	}

	// 优化 HTTP 客户端配置 - 重点优化连接建立阶段的超时控制
	// 启用TCP_NODELAY降低SSE首包延迟5~15ms
	dialer := &net.Dialer{
		Timeout:   config.HTTPDialTimeout,
		KeepAlive: config.HTTPKeepAliveInterval,
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
		IdleConnTimeout:     config.HTTPIdleConnTimeout,
		MaxConnsPerHost:     config.HTTPMaxConnsPerHost,

		// 连接建立超时（保留必要的底层网络超时）
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: config.HTTPTLSHandshakeTimeout,

		// 传输优化
		DisableCompression: false,
		DisableKeepAlives:  false,
		ForceAttemptHTTP2:  false, // 允许自动协议协商，避免HTTP/2超时
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
		log.Print("⚠️  警告：HTTP/2配置失败，将使用HTTP/1.1: " + err.Error())
	} else {
		log.Print("✅ HTTP/2已启用（头部压缩+多路复用）")
	}

	s := &Server{
		store:         store,
		maxKeyRetries: maxKeyRetries, // 单个渠道最大Key重试次数
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 不设置全局超时，避免中断长时间任务
		},
		firstByteTimeout: firstByteTimeout,
		loginRateLimiter: util.NewLoginRateLimiter(), // 用于传递给AuthService

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// 初始化优雅关闭机制
		shutdownCh: make(chan struct{}),
	}

	// 初始化高性能缓存层（60秒TTL，避免数据库性能杀手查询）
	s.channelCache = storage.NewChannelCache(store, 60*time.Second)

	// 初始化冷却管理器（统一管理渠道级和Key级冷却）
	// 传入Server作为configGetter，利用缓存层查询渠道配置
	s.cooldownManager = cooldown.NewManager(store, s)

	// 初始化渠道验证器管理器（支持88code套餐验证等扩展规则）
	s.validatorManager = validator.NewManager()

	// 注册88code套餐验证器（如果启用）
	// 支持多种布尔值表示: true/1/yes/on (不区分大小写)
	if enabled, _ := strconv.ParseBool(os.Getenv("CCLOAD_88CODE_FREE_ONLY")); enabled {
		log.Print("✅ 88code免费套餐验证已启用（非FREE套餐将被冷却30分钟）")
		s.validatorManager.AddValidator(validator.NewSubscriptionValidator(true))
	}

	// 初始化Key选择器（移除store依赖，避免重复查询）
	s.keySelector = NewKeySelector(nil)

	// ============================================================================
	// 创建服务层（仅保留有价值的服务）
	// ============================================================================

	// 1. LogService（负责日志管理）
	s.logService = NewLogService(
		store,
		config.DefaultLogBufferSize,
		config.DefaultLogWorkers,
		s.shutdownCh,
		&s.isShuttingDown,
		&s.wg,
	)
	// 启动日志 Workers
	s.logService.StartWorkers()

	// 仅当保留天数>0时启动清理协程(-1表示永久保留,不清理)
	if config.GetLogRetentionDays() > 0 {
		s.logService.StartCleanupLoop()
	}

	// 2. AuthService（负责认证授权）
	// 初始化时自动从数据库加载API访问令牌
	s.authService = NewAuthService(
		password,
		s.loginRateLimiter,
		store, // 传入store用于热更新令牌
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

// GetConfig 获取渠道配置（实现cooldown.ConfigGetter接口）
// 优先使用缓存层（60秒TTL），降级到数据库查询
func (s *Server) GetConfig(ctx context.Context, channelID int64) (*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		return cache.GetConfig(ctx, channelID)
	}
	return s.store.GetConfig(ctx, channelID)
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
// 在渠道CRUD操作后调用，确保缓存一致性
func (s *Server) InvalidateChannelListCache() {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateCache()
	}
}

// InvalidateAPIKeysCache 使指定渠道的 API Keys 缓存失效
// 在渠道Key更新后调用，确保缓存一致性
func (s *Server) InvalidateAPIKeysCache(channelID int64) {
	if cache := s.getChannelCache(); cache != nil {
		cache.InvalidateAPIKeysCache(channelID)
	}
}

// InvalidateAllAPIKeysCache 使所有 API Keys 缓存失效
// 在批量导入操作后调用，确保缓存一致性
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
		admin.POST("/channels/models/fetch", s.HandleFetchModelsPreview) // 临时渠道配置获取模型列表
		admin.GET("/channels/:id/models/fetch", s.HandleFetchModels)     // 获取渠道可用模型列表(新增)
		admin.POST("/channels/:id/test", s.HandleChannelTest)
		admin.POST("/channels/:id/cooldown", s.HandleSetChannelCooldown)
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.HandleSetKeyCooldown)

		// 统计分析
		admin.GET("/errors", s.HandleErrors)
		admin.GET("/metrics", s.HandleMetrics)
		admin.GET("/stats", s.HandleStats)
		admin.GET("/cooldown/stats", s.HandleCooldownStats)
		admin.GET("/cache/stats", s.HandleCacheStats)

		// API访问令牌管理
		admin.GET("/auth-tokens", s.HandleListAuthTokens)
		admin.POST("/auth-tokens", s.HandleCreateAuthToken)
		admin.PUT("/auth-tokens/:id", s.HandleUpdateAuthToken)
		admin.DELETE("/auth-tokens/:id", s.HandleDeleteAuthToken)
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

	ticker := time.NewTicker(config.TokenCleanupInterval)
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

// AddLogAsync 异步添加日志（委托给LogService处理）
// 在代理请求完成后调用，记录请求日志
func (s *Server) AddLogAsync(entry *model.LogEntry) {
	// 委托给 LogService 处理日志写入
	s.logService.AddLogAsync(entry)
}

// getModelsByChannelType 获取指定渠道类型的去重模型列表
func (s *Server) getModelsByChannelType(ctx context.Context, channelType string) ([]string, error) {
	// 直接查询数据库（KISS原则，避免过度设计）
	channels, err := s.store.GetEnabledChannelsByType(ctx, channelType)
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

	for i := range warmCount {
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
		log.Printf("✅ HTTP连接预热：为 %d 个高优先级渠道预建立连接", warmedCount)
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
	log.Print("🛑 正在关闭Server，等待后台任务完成...")

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
				log.Printf("❌ 关闭数据库连接失败: %v", err)
			}
		}

		log.Print("✅ Server优雅关闭完成")
		return nil
	case <-ctx.Done():
		log.Print("⚠️  Server关闭超时，部分后台任务可能未完成")
		return ctx.Err()
	}
}
