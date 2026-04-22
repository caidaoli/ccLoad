package app

import (
	"context"
	"crypto/tls"
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
	"ccLoad/internal/protocol"
	protocolbuiltin "ccLoad/internal/protocol/builtin"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// Server 是 ccLoad 的核心HTTP服务器，负责代理请求转发和管理API
type Server struct {
	// ============================================================================
	// 服务层
	// ============================================================================
	authService   *AuthService   // 认证授权服务
	logService    *LogService    // 日志管理服务
	configService *ConfigService // 配置管理服务

	// ============================================================================
	// 核心字段
	// ============================================================================
	store                         storage.Store
	channelCache                  *storage.ChannelCache // 高性能渠道缓存层
	keySelector                   *KeySelector          // Key选择器（多Key支持）
	cooldownManager               *cooldown.Manager     // 统一冷却管理器
	healthCache                   *HealthCache          // 渠道健康度缓存
	costCache                     *CostCache            // 渠道每日成本缓存
	statsCache                    *StatsCache           // 统计结果缓存层
	channelBalancer               *SmoothWeightedRR     // 渠道负载均衡器（平滑加权轮询）
	urlSelector                   *URLSelector          // URL选择器（多URL场景的延迟追踪与冷却）
	protocolRegistry              *protocol.Registry
	client                        *http.Client          // HTTP客户端
	activeRequests                *activeRequestManager // 进行中请求（内存状态，不持久化）
	scheduledChannelChecksRunning atomic.Bool

	// 异步统计（有界队列，避免每请求起goroutine）
	tokenStatsCh        chan tokenStatsUpdate
	tokenStatsDropCount atomic.Int64

	// 运行时配置（启动时从数据库加载，修改后重启生效）
	maxKeyRetries    int           // 单个渠道内最大Key重试次数
	firstByteTimeout time.Duration // 上游首字节超时（流式请求）
	nonStreamTimeout time.Duration // 非流式请求超时
	// 模型匹配配置（启动时从数据库加载，修改后重启生效）
	modelFuzzyMatch bool // 未命中时启用模糊匹配（子串匹配+版本排序）

	// 登录速率限制器（用于传递给AuthService）
	loginRateLimiter *util.LoginRateLimiter

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	// 优雅关闭机制
	baseCtx        context.Context    // server生命周期context，Shutdown时取消
	baseCancel     context.CancelFunc // 取消baseCtx
	shutdownCh     chan struct{}      // 关闭信号channel
	shutdownDone   chan struct{}      // Shutdown完成信号（幂等）
	isShuttingDown atomic.Bool        // shutdown标志，防止向已关闭channel写入
	wg             sync.WaitGroup     // 等待所有后台goroutine结束

	// [OPT] P3: 渠道类型缓存（TTL 30s）
	channelTypesCache     map[int64]string
	channelTypesCacheTime time.Time
	channelTypesCacheMu   sync.RWMutex
}

// NewServer 创建并初始化一个新的 Server 实例
func NewServer(store storage.Store) *Server {
	// 初始化ConfigService（优先从数据库加载配置,环境变量作Fallback）
	configService := NewConfigService(store)
	loadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := configService.LoadDefaults(loadCtx); err != nil {
		log.Fatalf("[FATAL] ConfigService初始化失败: %v", err)
	}
	log.Print("[INFO] ConfigService已加载系统配置（支持Web界面管理）")

	// 管理员密码：仅从环境变量读取（安全考虑：密码不应存储在数据库中）
	password := os.Getenv("CCLOAD_PASS")
	if password == "" {
		log.Print("[FATAL] 未设置 CCLOAD_PASS，出于安全原因程序将退出。请设置强管理员密码后重试。")
		os.Exit(1)
	}

	log.Printf("[INFO] 管理员密码已从环境变量加载（长度: %d 字符）", len(password))
	log.Print("[INFO] API访问令牌将从数据库动态加载（支持Web界面管理）")

	// 从ConfigService读取运行时配置（启动时加载一次，修改后重启生效）
	maxKeyRetries := configService.GetInt("max_key_retries", config.DefaultMaxKeyRetries)
	if maxKeyRetries < 1 {
		log.Printf("[WARN] 无效的 max_key_retries=%d（必须 >= 1），已使用默认值 %d", maxKeyRetries, config.DefaultMaxKeyRetries)
		maxKeyRetries = config.DefaultMaxKeyRetries
	}

	firstByteTimeout := configService.GetDuration("upstream_first_byte_timeout", 0)
	if firstByteTimeout < 0 {
		log.Printf("[WARN] 无效的 upstream_first_byte_timeout=%v（必须 >= 0），已设为 0（禁用首字节超时，仅流式生效）", firstByteTimeout)
		firstByteTimeout = 0
	}

	nonStreamTimeout := configService.GetDuration("non_stream_timeout", 120*time.Second)
	if nonStreamTimeout < 0 {
		log.Printf("[WARN] 无效的 non_stream_timeout=%v（必须 >= 0，0=禁用），已使用默认值 %v", nonStreamTimeout, 120*time.Second)
		nonStreamTimeout = 120 * time.Second
	}

	logRetentionDays := configService.GetInt("log_retention_days", 7)

	modelFuzzyMatch := configService.GetBool("model_fuzzy_match", false)
	if modelFuzzyMatch {
		log.Print("[INFO] 已启用模型模糊匹配：未命中时进行子串匹配并按版本排序选择最新模型")
	}

	// 最大并发数保留环境变量读取（启动参数，不支持Web管理）
	maxConcurrency := config.DefaultMaxConcurrency
	if concEnv := os.Getenv("CCLOAD_MAX_CONCURRENCY"); concEnv != "" {
		if val, err := strconv.Atoi(concEnv); err == nil && val > 0 {
			maxConcurrency = val
		}
	}

	// TLS证书验证配置（仅环境变量）
	// 这是一个危险开关：一旦关闭证书校验，上游 HTTPS 等同明文 + 任意中间人。
	skipTLSVerify := os.Getenv("CCLOAD_ALLOW_INSECURE_TLS") == "1"
	if skipTLSVerify {
		log.Print("[WARN] 已禁用上游 TLS 证书校验（InsecureSkipVerify=true）：仅用于临时排障/受控内网环境")
	}

	// 构建HTTP Transport（使用统一函数，消除DRY违反）
	transport := buildHTTPTransport(skipTLSVerify)
	log.Print("[INFO] HTTP/2已启用（头部压缩+多路复用，HTTPS自动协商）")

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &Server{
		store:            store,
		configService:    configService,
		loginRateLimiter: util.NewLoginRateLimiter(),

		// 运行时配置（启动时加载，修改后重启生效）
		maxKeyRetries:    maxKeyRetries,
		firstByteTimeout: firstByteTimeout,
		nonStreamTimeout: nonStreamTimeout,
		// 模型匹配配置（启动时加载，修改后重启生效）
		modelFuzzyMatch: modelFuzzyMatch,

		// HTTP客户端
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 不设置全局超时，避免中断长时间任务
		},

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// 初始化优雅关闭机制
		baseCtx:      baseCtx,
		baseCancel:   baseCancel,
		shutdownCh:   make(chan struct{}),
		shutdownDone: make(chan struct{}),

		// Token统计队列（避免每请求起goroutine）
		tokenStatsCh: make(chan tokenStatsUpdate, config.DefaultTokenStatsBufferSize),

		activeRequests: newActiveRequestManager(),
	}

	reg := protocol.NewRegistry()
	protocolbuiltin.Register(reg)
	s.protocolRegistry = reg

	// 初始化高性能缓存层（60秒TTL，避免数据库性能杀手查询）
	s.channelCache = storage.NewChannelCache(store, 60*time.Second)

	// 初始化冷却管理器（统一管理渠道级和Key级冷却）
	// 传入Server作为configGetter，利用缓存层查询渠道配置
	s.cooldownManager = cooldown.NewManager(store, s)

	// 初始化Key选择器（移除store依赖，避免重复查询）
	s.keySelector = NewKeySelector()

	// 初始化渠道负载均衡器（平滑加权轮询，确定性分流）
	s.channelBalancer = NewSmoothWeightedRR()

	// 初始化URL选择器（多URL场景：EWMA延迟追踪+URL级冷却）
	s.urlSelector = NewURLSelector()

	// 初始化健康度缓存（启动时读取配置，修改后重启生效）
	defaultHealthCfg := model.DefaultHealthScoreConfig()
	successRatePenaltyWeight := configService.GetInt("success_rate_penalty_weight", defaultHealthCfg.SuccessRatePenaltyWeight)
	if successRatePenaltyWeight < 0 {
		log.Printf("[WARN] 无效的 success_rate_penalty_weight=%d（必须 >= 0），已使用默认值 %d", successRatePenaltyWeight, defaultHealthCfg.SuccessRatePenaltyWeight)
		successRatePenaltyWeight = defaultHealthCfg.SuccessRatePenaltyWeight
	}
	windowMinutes := configService.GetInt("health_score_window_minutes", 30)
	if windowMinutes < 1 {
		log.Printf("[WARN] 无效的 health_score_window_minutes=%d（必须 >= 1），已使用默认值 30", windowMinutes)
		windowMinutes = 30
	}
	updateInterval := configService.GetInt("health_score_update_interval", 30)
	if updateInterval < 1 {
		log.Printf("[WARN] 无效的 health_score_update_interval=%d（必须 >= 1），已使用默认值 30", updateInterval)
		updateInterval = 30
	}
	minConfidentSample := configService.GetInt("health_min_confident_sample", defaultHealthCfg.MinConfidentSample)
	if minConfidentSample < 1 {
		log.Printf("[WARN] 无效的 health_min_confident_sample=%d（必须 >= 1），已使用默认值 %d", minConfidentSample, defaultHealthCfg.MinConfidentSample)
		minConfidentSample = defaultHealthCfg.MinConfidentSample
	}
	healthConfig := model.HealthScoreConfig{
		Enabled:                  configService.GetBool("enable_health_score", defaultHealthCfg.Enabled),
		SuccessRatePenaltyWeight: successRatePenaltyWeight,
		WindowMinutes:            windowMinutes,
		UpdateIntervalSeconds:    updateInterval,
		MinConfidentSample:       minConfidentSample,
	}
	s.healthCache = NewHealthCache(store, healthConfig, s.shutdownCh, &s.isShuttingDown, &s.wg)
	if healthConfig.Enabled {
		s.healthCache.Start()
		log.Print("[INFO] 健康度排序已启用（基于成功率动态调整渠道优先级；冷却仍按原规则过滤）")
	}

	// 初始化成本缓存（启动时从数据库加载当日成本）
	s.costCache = NewCostCache()
	costLoadCtx, costCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer costCancel()
	todayCosts, err := store.GetTodayChannelCosts(costLoadCtx, s.costCache.DayStart())
	if err != nil {
		log.Printf("[WARN] 加载今日渠道成本失败: %v（成本限额功能可能不准确）", err)
	} else {
		s.costCache.Load(todayCosts)
		log.Printf("[INFO] 已加载今日渠道成本缓存（%d个渠道有消耗）", len(todayCosts))
	}

	urlStatsLoadCtx, urlStatsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer urlStatsCancel()
	todayURLStats, err := store.GetTodayChannelURLStats(urlStatsLoadCtx, s.costCache.DayStart())
	if err != nil {
		log.Printf("[WARN] 加载今日 URL 运行状态失败: %v（多URL状态展示可能为空）", err)
	} else {
		s.urlSelector.LoadPersistedStats(todayURLStats)
		if len(todayURLStats) > 0 {
			log.Printf("[INFO] 已从日志恢复今日 URL 运行状态（%d条URL）", len(todayURLStats))
		}
	}

	// 初始化统计缓存层（减少重复聚合查询）
	s.statsCache = NewStatsCache(store)
	log.Print("[INFO] 统计缓存已启用（智能 TTL，减少数据库聚合查询）")

	// ============================================================================
	// 创建服务层（仅保留有价值的服务）
	// ============================================================================

	// 1. LogService（负责日志管理）
	s.logService = NewLogService(
		store,
		config.DefaultLogBufferSize,
		config.DefaultLogWorkers,
		logRetentionDays, // 启动时读取，修改后重启生效
		s.shutdownCh,
		&s.isShuttingDown,
		&s.wg,
	)
	// 启动日志 Workers
	s.logService.StartWorkers()

	// 启动清理协程（调试日志清理始终运行，普通日志按保留天数决定）
	s.logService.StartCleanupLoop()

	// 2. AuthService（负责认证授权）
	// 初始化时自动从数据库加载API访问令牌
	s.authService = NewAuthService(
		password,
		s.loginRateLimiter,
		store, // 传入store用于热更新令牌
	)

	// 启动Token统计Worker（有界队列：性能可控，Shutdown可等待）
	s.wg.Add(1)
	go s.tokenStatsWorker()

	// 启动后台清理协程（Token 认证）
	s.wg.Add(1)
	go s.tokenCleanupLoop() // 定期清理过期Token

	// [FIX] P1: 启动后台状态清理协程（防止内存泄漏）
	s.wg.Add(1)
	go s.stateCleanupLoop()

	channelCheckIntervalHours := normalizeChannelCheckIntervalHours(
		configService.GetInt("channel_check_interval_hours", defaultChannelCheckIntervalHours),
	)
	if channelCheckIntervalHours == 0 {
		log.Print("[INFO] 渠道定时检测未启用（channel_check_interval_hours=0）")
	} else {
		s.startScheduledChannelCheckLoop(time.Duration(channelCheckIntervalHours) * time.Hour)
	}

	return s

}

// ================== 缓存辅助函数 ==================

func (s *Server) getChannelCache() *storage.ChannelCache {
	if s == nil {
		return nil
	}
	return s.channelCache
}

// buildHTTPTransport 构建HTTP Transport（DRY：统一配置逻辑）
// 参数:
//   - skipTLSVerify: 是否跳过TLS证书验证
func buildHTTPTransport(skipTLSVerify bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   config.HTTPDialTimeout,
		KeepAlive: config.HTTPKeepAliveInterval,
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = setTCPNoDelay(fd)
			})
		},
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // 支持 HTTPS_PROXY/HTTP_PROXY/NO_PROXY
		MaxIdleConns:        config.HTTPMaxIdleConns,
		MaxIdleConnsPerHost: config.HTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second, // 空闲连接90秒后关闭，避免僵尸连接
		MaxConnsPerHost:     config.HTTPMaxConnsPerHost,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: config.HTTPTLSHandshakeTimeout,
		DisableCompression:  false,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   true, // 启用标准库 HTTP/2（HTTPS 自动协商）
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(config.TLSSessionCacheSize),
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: skipTLSVerify, //nolint:gosec // G402: 由环境变量CCLOAD_SKIP_TLS_VERIFY控制，用于开发测试
		},
	}

	return transport // HTTP/2 已通过 ForceAttemptHTTP2 启用
}

// NOTE: 这些缓存fallback函数存在重复逻辑，可使用泛型重构（Go 1.18+）
// 当前设计选择：保持简单直接，避免过度抽象（YAGNI）

// GetConfig 获取渠道配置（实现cooldown.ConfigGetter接口）
func (s *Server) GetConfig(ctx context.Context, channelID int64) (*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		return cache.GetConfig(ctx, channelID)
	}
	return s.store.GetConfig(ctx, channelID)
}

// GetEnabledChannelsByModel 根据模型名称获取所有启用的渠道配置
func (s *Server) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		if channels, err := cache.GetEnabledChannelsByModel(ctx, model); err == nil {
			return channels, nil
		}
	}
	return s.store.GetEnabledChannelsByModel(ctx, model)
}

// GetEnabledChannelsByType 根据渠道类型获取所有启用的渠道配置
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
	// 渠道配置变更时重置轮询状态，确保新配置下的分布正确
	if s.channelBalancer != nil {
		s.channelBalancer.ResetAll()
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

// invalidateChannelRelatedCache 失效渠道相关的冷却/Key缓存
// 注意：此函数仅失效冷却和Key缓存，不重置轮询状态
// 在冷却状态变更后调用（成功请求清除冷却、错误重试等场景）
func (s *Server) invalidateChannelRelatedCache(channelID int64) {
	// 仅失效冷却缓存，不调用 InvalidateChannelListCache
	// 因为渠道列表本身未变更，只是冷却状态变更
	s.InvalidateAPIKeysCache(channelID)
	s.invalidateCooldownCache()
}

// GetWriteTimeout 返回建议的 HTTP WriteTimeout
// 基于 nonStreamTimeout 动态计算，确保传输层超时 >= 业务层超时
func (s *Server) GetWriteTimeout() time.Duration {
	const minWriteTimeout = 120 * time.Second
	if s.nonStreamTimeout > minWriteTimeout {
		return s.nonStreamTimeout
	}
	return minWriteTimeout
}

// SetupRoutes - 新的路由设置函数，适配Gin
func (s *Server) SetupRoutes(r *gin.Engine) {
	// 安全响应头（管理界面防护）
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	})

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

	// 健康检查（公开访问，无需认证，K8s liveness/readiness probe）
	r.GET("/health", s.HandleHealth)

	// 公开访问的API（首页仪表盘数据）
	// [SECURITY NOTE] /public/* 端点故意不做认证，用于首页展示。
	// 如需隐藏运营数据，可添加 s.authService.RequireTokenAuth() 中间件。
	public := r.Group("/public", ZstdMiddleware())
	{
		public.GET("/summary", s.HandlePublicSummary)
		public.GET("/channel-types", s.HandleGetChannelTypes)
		public.GET("/version", s.HandlePublicVersion)
	}

	// 事件日志（公开访问，兼容性占位接口）
	r.POST("/api/event_logging/batch", s.HandleEventLoggingBatch)

	// 登录相关（公开访问）
	r.POST("/login", s.authService.HandleLogin)
	r.POST("/logout", s.authService.HandleLogout)

	// 需要身份验证的admin APIs（使用Token认证）
	admin := r.Group("/admin", ZstdMiddleware())
	admin.Use(s.authService.RequireTokenAuth())
	{
		// 渠道管理
		admin.GET("/channels", s.HandleChannels)
		admin.POST("/channels", s.HandleChannels)
		admin.GET("/channels/export", s.HandleExportChannelsCSV)
		admin.POST("/channels/import", s.HandleImportChannelsCSV)
		admin.POST("/channels/batch-priority", s.HandleBatchUpdatePriority) // 批量更新渠道优先级
		admin.POST("/channels/batch-enabled", s.HandleBatchSetEnabled)      // 批量启用/禁用渠道
		admin.POST("/channels/batch-delete", s.HandleBatchDeleteChannels)   // 批量删除渠道
		admin.GET("/channels/:id", s.HandleChannelByID)
		admin.PUT("/channels/:id", s.HandleChannelByID)
		admin.DELETE("/channels/:id", s.HandleChannelByID)
		admin.GET("/channels/:id/keys", s.HandleChannelKeys)
		admin.GET("/channels/:id/url-stats", s.HandleChannelURLStats)
		admin.POST("/channels/:id/url-disable", s.HandleURLDisable)
		admin.POST("/channels/:id/url-enable", s.HandleURLEnable)
		admin.POST("/channels/models/fetch", s.HandleFetchModelsPreview) // 临时渠道配置获取模型列表
		admin.POST("/channels/models/refresh-batch", s.HandleBatchRefreshModels)
		admin.GET("/channels/:id/models/fetch", s.HandleFetchModels) // 获取渠道可用模型列表(新增)
		admin.POST("/channels/:id/models", s.HandleAddModels)        // 添加渠道模型
		admin.DELETE("/channels/:id/models", s.HandleDeleteModels)   // 删除渠道模型
		admin.POST("/channels/:id/test", s.HandleChannelTest)
		admin.POST("/channels/:id/test-url", s.HandleChannelURLTest)
		admin.POST("/channels/:id/cooldown", s.HandleSetChannelCooldown)
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.HandleSetKeyCooldown)
		admin.DELETE("/channels/:id/keys/:keyIndex", s.HandleDeleteAPIKey)

		// 统计分析
		admin.GET("/logs", s.HandleErrors)
		admin.GET("/debug-logs/:log_id", s.HandleGetDebugLog)
		admin.GET("/active-requests", s.HandleActiveRequests) // 进行中请求（内存状态）
		admin.GET("/metrics", s.HandleMetrics)
		admin.GET("/stats", s.HandleStats)
		admin.GET("/cooldown/stats", s.HandleCooldownStats)
		admin.GET("/models", s.HandleGetModels)

		// API访问令牌管理
		admin.GET("/auth-tokens", s.HandleListAuthTokens)
		admin.POST("/auth-tokens", s.HandleCreateAuthToken)
		admin.PUT("/auth-tokens/:id", s.HandleUpdateAuthToken)
		admin.DELETE("/auth-tokens/:id", s.HandleDeleteAuthToken)

		// 系统配置管理
		admin.GET("/settings", s.AdminListSettings)
		admin.GET("/settings/:key", s.AdminGetSetting)
		admin.PUT("/settings/:key", s.AdminUpdateSetting)
		admin.POST("/settings/:key/reset", s.AdminResetSetting)
		admin.POST("/settings/batch", s.AdminBatchUpdateSettings)
	}

	// 静态文件服务（带版本号和缓存控制）
	// - HTML：不缓存，动态替换 __VERSION__ 占位符
	// - CSS/JS：长缓存（1年），通过版本号查询参数刷新
	setupStaticFiles(r)

	// 默认首页重定向
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/index.html")
	})
}

// HandleEventLoggingBatch 返回空JSON响应（兼容性占位接口）
func (s *Server) HandleEventLoggingBatch(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{})
}

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

// stateCleanupLoop 后台状态清理循环（防止内存泄漏）
// [FIX] P1: 清理 SmoothWeightedRR 和 KeySelector 的过期状态
func (s *Server) stateCleanupLoop() {
	defer s.wg.Done()

	// 每小时清理一次过期状态
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	log.Print("[INFO] 后台状态清理循环已启动（每小时清理过期的轮询状态和计数器）")

	for {
		select {
		case <-s.shutdownCh:
			log.Print("[INFO] 后台状态清理循环已停止")
			return
		case <-ticker.C:
			// 清理SmoothWeightedRR的过期轮询状态（24小时未访问视为过期）
			if s.channelBalancer != nil {
				s.channelBalancer.Cleanup(24 * time.Hour)
			}

			// [FIX] P1: 清理KeySelector的过期轮询计数器（24小时未使用视为过期）
			// 避免渠道删除后计数器累积导致内存泄漏
			if s.keySelector != nil {
				s.keySelector.CleanupInactiveCounters(24 * time.Hour)
			}
		}
	}
}

// AddLogAsync 异步添加日志（委托给LogService处理）
// 在代理请求完成后调用，记录请求日志
func (s *Server) AddLogAsync(entry *model.LogEntry) {
	if entry != nil && entry.LogSource == "" {
		entry.LogSource = model.LogSourceProxy
	}

	// 更新成本缓存（用于每日成本限额功能）
	// 语义：缓存累加倍率后成本（effective），与 daily_cost_limit 直接比较
	if s.costCache != nil && entry.ChannelID > 0 && entry.Cost > 0 && entry.LogSource == model.LogSourceProxy {
		multiplier := entry.CostMultiplier
		if multiplier <= 0 {
			multiplier = 1
		}
		s.costCache.Add(entry.ChannelID, entry.Cost*multiplier)
	}

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
		for _, modelName := range cfg.GetModels() {
			modelSet[modelName] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}
	return models, nil
}

// getModelsByExposedProtocol 获取指定暴露协议的去重模型列表
func (s *Server) getModelsByExposedProtocol(ctx context.Context, protocol string) ([]string, error) {
	channels, err := s.store.GetEnabledChannelsByExposedProtocol(ctx, protocol)
	if err != nil {
		return nil, err
	}
	modelSet := make(map[string]struct{})
	for _, cfg := range channels {
		for _, modelName := range cfg.GetModels() {
			modelSet[modelName] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}
	return models, nil
}

// HandleChannelKeys 获取渠道的所有API Keys
// GET /admin/channels/:id/keys
func (s *Server) HandleChannelKeys(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	s.handleGetChannelKeys(c, id)
}

// Shutdown 优雅关闭Server，等待所有后台goroutine完成
// 参数ctx用于控制最大等待时间，超时后强制退出
// 返回值：nil表示成功，context.DeadlineExceeded表示超时
func (s *Server) Shutdown(ctx context.Context) error {
	if s.isShuttingDown.Swap(true) {
		select {
		case <-s.shutdownDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer close(s.shutdownDone)

	log.Print("🛑 正在关闭Server，等待后台任务完成...")

	// 取消server级context，通知所有派生的后台任务退出
	s.baseCancel()

	// 关闭shutdownCh，通知所有goroutine退出（幂等：由isShuttingDown守护）
	close(s.shutdownCh)

	// 停止LoginRateLimiter的cleanupLoop
	if s.loginRateLimiter != nil {
		s.loginRateLimiter.Stop()
	}

	// 关闭AuthService的后台worker
	if s.authService != nil {
		s.authService.Close()
	}

	// 关闭StatsCache的后台清理worker
	if s.statsCache != nil {
		s.statsCache.Close()
	}

	// 使用channel等待所有goroutine完成
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// 等待完成或超时
	var err error
	select {
	case <-done:
		log.Print("[INFO] Server优雅关闭完成")
	case <-ctx.Done():
		log.Print("[WARN]  Server关闭超时，部分后台任务可能未完成")
		err = ctx.Err()
	}

	// 无论成功还是超时，都要关闭数据库连接
	if closer, ok := s.store.(interface{ Close() error }); ok {
		if closeErr := closer.Close(); closeErr != nil {
			log.Printf("[ERROR] 关闭数据库连接失败: %v", closeErr)
		}
	}

	return err
}
