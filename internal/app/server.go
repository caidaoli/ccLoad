package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	neturl "net/url"
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
	channelCache                  *storage.ChannelCache      // 高性能渠道缓存层
	keySelector                   *KeySelector               // Key选择器（多Key支持）
	cooldownManager               *cooldown.Manager          // 统一冷却管理器
	healthCache                   *HealthCache               // 渠道健康度缓存
	costCache                     *CostCache                 // 渠道每日成本缓存
	channelRPMLimiter             *channelRPMLimiter         // 渠道RPM限制器（内存滑动窗口）
	channelConcurrencyLimiter     *channelConcurrencyLimiter // 渠道并发限制器（内存计数）
	statsCache                    *StatsCache                // 统计结果缓存层
	channelBalancer               *SmoothWeightedRR          // 渠道负载均衡器（平滑加权轮询）
	urlSelector                   *URLSelector               // URL选择器（多URL场景的延迟追踪与冷却）
	protocolRegistry              *protocol.Registry
	client                        *http.Client          // HTTP客户端（全局默认）
	proxyTransports               sync.Map              // proxyURL → *http.Transport（渠道级代理缓存）
	skipTLSVerify                 bool                  // 透传给渠道级 Transport
	activeRequests                *activeRequestManager // 进行中请求（内存状态，不持久化）
	scheduledChannelChecksRunning atomic.Bool

	// 异步统计（有界队列，避免每请求起goroutine）
	tokenStatsCh        chan tokenStatsUpdate
	tokenStatsDropCount atomic.Int64

	// 运行时配置（启动时从数据库加载，修改后重启生效）
	maxKeyRetries       int                                 // 单个渠道内最大Key重试次数
	firstByteTimeout    time.Duration                       // 上游首字节超时（流式请求）
	nonStreamTimeout    time.Duration                       // 非流式请求超时
	channelTypeTimeouts map[string]channelTypeTimeoutConfig // 按运行时上游协议覆盖超时，0=回退全局
	// 模型匹配配置（启动时从数据库加载，修改后重启生效）
	modelFuzzyMatch bool // 未命中时启用模糊匹配（子串匹配+版本排序）

	// 登录速率限制器（用于传递给AuthService）
	loginRateLimiter *util.LoginRateLimiter

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数（防止goroutine爆炸）
	maxConcurrency int           // 最大并发数（默认1000）

	// 优雅关闭机制
	baseCtx                 context.Context    // server生命周期context，Shutdown时取消
	baseCancel              context.CancelFunc // 取消baseCtx
	shutdownCh              chan struct{}      // 关闭信号channel
	shutdownDone            chan struct{}      // Shutdown完成信号（幂等）
	isShuttingDown          atomic.Bool        // shutdown标志，防止向已关闭channel写入
	modelCatalogSyncMu      sync.Mutex         // 串行化模型目录启动和关闭，保护 WaitGroup
	modelCatalogSyncStarted atomic.Bool
	wg                      sync.WaitGroup // 等待所有后台goroutine结束

	// [OPT] P3: 渠道类型缓存（TTL 30s）
	channelTypesCache     map[int64]string
	channelTypesCacheTime time.Time
	channelTypesCacheMu   sync.RWMutex

	// 指纹任务管理器（内存）
	fingerprintJobs *FingerprintJobManager
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
	provisionCtx, provisionCancel := context.WithTimeout(context.Background(), authTokenProvisionTimeout)
	provisionResult, err := ProvisionAuthTokensFromEnv(provisionCtx, store)
	provisionCancel()
	if err != nil {
		log.Fatalf("[FATAL] API令牌预置失败: %v", err)
	}
	if provisionResult.Configured > 0 {
		log.Printf("[INFO] API令牌预置完成（配置: %d, 新增: %d）", provisionResult.Configured, provisionResult.Created)
	}
	log.Print("[INFO] API访问令牌将从数据库动态加载（支持Web界面管理与环境变量预置）")

	// 从ConfigService读取运行时配置（启动时加载一次，修改后重启生效）
	runtimeCfg := loadServerRuntimeConfig(configService)

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
	logHostOverrides(getHostOverrides())

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &Server{
		store:            store,
		configService:    configService,
		loginRateLimiter: util.NewLoginRateLimiter(),

		// 运行时配置（启动时加载，修改后重启生效）
		maxKeyRetries:       runtimeCfg.MaxKeyRetries,
		firstByteTimeout:    runtimeCfg.FirstByteTimeout,
		nonStreamTimeout:    runtimeCfg.NonStreamTimeout,
		channelTypeTimeouts: runtimeCfg.ChannelTypeTimeouts,
		// 模型匹配配置（启动时加载，修改后重启生效）
		modelFuzzyMatch: runtimeCfg.ModelFuzzyMatch,

		// HTTP客户端
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 不设置全局超时，避免中断长时间任务
		},
		skipTLSVerify: skipTLSVerify,

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

		activeRequests:            newActiveRequestManager(),
		channelRPMLimiter:         newChannelRPMLimiter(time.Now),
		channelConcurrencyLimiter: newChannelConcurrencyLimiter(),
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
	healthConfig := loadHealthScoreConfig(configService)
	s.healthCache = NewHealthCache(store, healthConfig, s.shutdownCh, &s.isShuttingDown, &s.wg)
	if healthConfig.Enabled {
		s.healthCache.Start()
		log.Print("[INFO] 健康度排序已启用（基于成功率动态调整渠道优先级；冷却仍按原规则过滤）")
	}

	// 初始化成本缓存（启动时从数据库加载当日成本）
	s.costCache = NewCostCache()
	bootstrapCostAndURLStats(store, s.costCache, s.urlSelector)

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
		runtimeCfg.LogRetentionDays, // 启动时读取，修改后重启生效
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

	// 启动后台 worker（Token 统计 / Token 清理 / 状态清理）
	s.startBackgroundWorkers()

	channelCheckIntervalHours := normalizeChannelCheckIntervalHours(
		configService.GetFloat("channel_check_interval_hours", defaultChannelCheckIntervalHours),
	)
	if channelCheckIntervalHours == 0 {
		log.Print("[INFO] 渠道定时检测未启用（channel_check_interval_hours=0）")
	} else {
		s.startScheduledChannelCheckLoop(time.Duration(channelCheckIntervalHours * float64(time.Hour)))
	}

	// 指纹 Job 管理器（内存）
	s.fingerprintJobs = NewFingerprintJobManager(2)

	return s

}

// StartModelCatalogSync 加载本地快照，并在启用时同步官方模型目录。
func (s *Server) StartModelCatalogSync() {
	if s == nil || s.configService == nil {
		return
	}
	s.modelCatalogSyncMu.Lock()
	defer s.modelCatalogSyncMu.Unlock()
	if s.isShuttingDown.Load() || !s.modelCatalogSyncStarted.CompareAndSwap(false, true) {
		return
	}

	intervalHours := normalizeModelCatalogSyncIntervalHours(
		s.configService.GetFloat("model_catalog_sync_interval_hours", defaultModelCatalogSyncHours),
	)
	syncer := NewModelCatalogSyncer(
		&http.Client{Timeout: modelCatalogRequestTimeout},
		modelsDevCatalogURL,
		modelCatalogCachePath(),
	)
	if err := syncer.LoadCache(); err != nil {
		log.Printf("[WARN] 模型目录缓存加载失败: %v", err)
	}
	if intervalHours == 0 {
		return
	}

	interval := time.Duration(intervalHours * float64(time.Hour))
	if interval <= 0 {
		log.Printf("[WARN] 无效的模型目录同步间隔: %v 小时", intervalHours)
		return
	}
	s.wg.Add(1)
	go s.runModelCatalogSyncLoop(syncer, interval)
}

type channelTypeTimeoutConfig struct {
	FirstByteTimeout time.Duration
	NonStreamTimeout time.Duration
}

// serverRuntimeConfig 启动期从数据库读取的运行时配置（修改后重启生效）
type serverRuntimeConfig struct {
	MaxKeyRetries       int
	FirstByteTimeout    time.Duration
	NonStreamTimeout    time.Duration
	ChannelTypeTimeouts map[string]channelTypeTimeoutConfig
	LogRetentionDays    int
	ModelFuzzyMatch     bool
}

// loadServerRuntimeConfig 从 ConfigService 加载运行时配置并校验，无效值兜底为默认值
func loadServerRuntimeConfig(cs *ConfigService) serverRuntimeConfig {
	maxKeyRetries := cs.GetInt("max_key_retries", config.DefaultMaxKeyRetries)
	if maxKeyRetries < 1 {
		log.Printf("[WARN] 无效的 max_key_retries=%d（必须 >= 1），已使用默认值 %d", maxKeyRetries, config.DefaultMaxKeyRetries)
		maxKeyRetries = config.DefaultMaxKeyRetries
	}

	firstByteTimeout := cs.GetDuration("upstream_first_byte_timeout", 0)
	if firstByteTimeout < 0 {
		log.Printf("[WARN] 无效的 upstream_first_byte_timeout=%v（必须 >= 0），已设为 0（禁用首字节超时，仅流式生效）", firstByteTimeout)
		firstByteTimeout = 0
	}

	nonStreamTimeout := cs.GetDuration("non_stream_timeout", 120*time.Second)
	if nonStreamTimeout < 0 {
		log.Printf("[WARN] 无效的 non_stream_timeout=%v（必须 >= 0，0=禁用），已使用默认值 %v", nonStreamTimeout, 120*time.Second)
		nonStreamTimeout = 120 * time.Second
	}

	channelTypeTimeouts := loadChannelTypeTimeouts(cs)

	logRetentionDays := cs.GetInt("log_retention_days", 7)

	modelFuzzyMatch := cs.GetBool("model_fuzzy_match", false)
	if modelFuzzyMatch {
		log.Print("[INFO] 已启用模型模糊匹配：未命中时进行子串匹配并按版本排序选择最新模型")
	}

	return serverRuntimeConfig{
		MaxKeyRetries:       maxKeyRetries,
		FirstByteTimeout:    firstByteTimeout,
		NonStreamTimeout:    nonStreamTimeout,
		ChannelTypeTimeouts: channelTypeTimeouts,
		LogRetentionDays:    logRetentionDays,
		ModelFuzzyMatch:     modelFuzzyMatch,
	}
}

func loadChannelTypeTimeouts(cs *ConfigService) map[string]channelTypeTimeoutConfig {
	timeouts := make(map[string]channelTypeTimeoutConfig, len(util.ChannelTypes))
	for _, channelType := range util.ChannelTypes {
		firstByteTimeout := cs.GetDuration(channelTypeFirstByteTimeoutSettingKey(channelType.Value), 0)
		if firstByteTimeout < 0 {
			log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已设为 0（回退全局首字超时）",
				channelTypeFirstByteTimeoutSettingKey(channelType.Value), firstByteTimeout)
			firstByteTimeout = 0
		}

		nonStreamTimeout := cs.GetDuration(channelTypeNonStreamTimeoutSettingKey(channelType.Value), 0)
		if nonStreamTimeout < 0 {
			log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已设为 0（回退全局非流超时）",
				channelTypeNonStreamTimeoutSettingKey(channelType.Value), nonStreamTimeout)
			nonStreamTimeout = 0
		}

		timeouts[channelType.Value] = channelTypeTimeoutConfig{
			FirstByteTimeout: firstByteTimeout,
			NonStreamTimeout: nonStreamTimeout,
		}
	}
	return timeouts
}

func channelTypeFirstByteTimeoutSettingKey(channelType string) string {
	return util.NormalizeChannelType(channelType) + "_first_byte_timeout"
}

func channelTypeNonStreamTimeoutSettingKey(channelType string) string {
	return util.NormalizeChannelType(channelType) + "_non_stream_timeout"
}

// loadHealthScoreConfig 从 ConfigService 加载健康度配置，无效值兜底为默认值
func loadHealthScoreConfig(cs *ConfigService) model.HealthScoreConfig {
	defaultHealthCfg := model.DefaultHealthScoreConfig()
	successRatePenaltyWeight := cs.GetInt("success_rate_penalty_weight", defaultHealthCfg.SuccessRatePenaltyWeight)
	if successRatePenaltyWeight < 0 {
		log.Printf("[WARN] 无效的 success_rate_penalty_weight=%d（必须 >= 0），已使用默认值 %d", successRatePenaltyWeight, defaultHealthCfg.SuccessRatePenaltyWeight)
		successRatePenaltyWeight = defaultHealthCfg.SuccessRatePenaltyWeight
	}
	windowMinutes := cs.GetInt("health_score_window_minutes", 30)
	if windowMinutes < 1 {
		log.Printf("[WARN] 无效的 health_score_window_minutes=%d（必须 >= 1），已使用默认值 30", windowMinutes)
		windowMinutes = 30
	}
	updateInterval := cs.GetInt("health_score_update_interval", 30)
	if updateInterval < 1 {
		log.Printf("[WARN] 无效的 health_score_update_interval=%d（必须 >= 1），已使用默认值 30", updateInterval)
		updateInterval = 30
	}
	minConfidentSample := cs.GetInt("health_min_confident_sample", defaultHealthCfg.MinConfidentSample)
	if minConfidentSample < 1 {
		log.Printf("[WARN] 无效的 health_min_confident_sample=%d（必须 >= 1），已使用默认值 %d", minConfidentSample, defaultHealthCfg.MinConfidentSample)
		minConfidentSample = defaultHealthCfg.MinConfidentSample
	}
	ttfbPenaltyWeight := cs.GetFloat("ttfb_penalty_weight", defaultHealthCfg.TTFBPenaltyWeight)
	if ttfbPenaltyWeight < 0 {
		log.Printf("[WARN] 无效的 ttfb_penalty_weight=%v（必须 >= 0），已使用默认值 %v", ttfbPenaltyWeight, defaultHealthCfg.TTFBPenaltyWeight)
		ttfbPenaltyWeight = defaultHealthCfg.TTFBPenaltyWeight
	}
	ttfbMaxSlowRatio := cs.GetFloat("ttfb_max_slow_ratio", defaultHealthCfg.TTFBMaxSlowRatio)
	if ttfbMaxSlowRatio < 0 {
		log.Printf("[WARN] 无效的 ttfb_max_slow_ratio=%v（必须 >= 0），已使用默认值 %v", ttfbMaxSlowRatio, defaultHealthCfg.TTFBMaxSlowRatio)
		ttfbMaxSlowRatio = defaultHealthCfg.TTFBMaxSlowRatio
	}
	ttfbMinConfidentSample := cs.GetInt("ttfb_min_confident_sample", defaultHealthCfg.TTFBMinConfidentSample)
	if ttfbMinConfidentSample < 1 {
		log.Printf("[WARN] 无效的 ttfb_min_confident_sample=%d（必须 >= 1），已使用默认值 %d", ttfbMinConfidentSample, defaultHealthCfg.TTFBMinConfidentSample)
		ttfbMinConfidentSample = defaultHealthCfg.TTFBMinConfidentSample
	}
	return model.HealthScoreConfig{
		Enabled:                  cs.GetBool("enable_health_score", defaultHealthCfg.Enabled),
		SuccessRatePenaltyWeight: successRatePenaltyWeight,
		WindowMinutes:            windowMinutes,
		UpdateIntervalSeconds:    updateInterval,
		MinConfidentSample:       minConfidentSample,
		EnableTTFBScore:          cs.GetBool("enable_ttfb_score", defaultHealthCfg.EnableTTFBScore),
		TTFBPenaltyWeight:        ttfbPenaltyWeight,
		TTFBMaxSlowRatio:         ttfbMaxSlowRatio,
		TTFBMinConfidentSample:   ttfbMinConfidentSample,
	}
}

// bootstrapCostAndURLStats 启动时从数据库恢复当日渠道成本与多URL运行状态。
// 失败仅记录 WARN（不影响启动），保留两段独立 10s 超时 context（defer cancel 无条件调用）。
func bootstrapCostAndURLStats(store storage.Store, costCache *CostCache, urlSelector *URLSelector) {
	costLoadCtx, costCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer costCancel()
	todayCosts, err := store.GetTodayChannelCosts(costLoadCtx, costCache.DayStart())
	if err != nil {
		log.Printf("[WARN] 加载今日渠道成本失败: %v（成本限额功能可能不准确）", err)
	} else {
		costCache.Load(todayCosts)
		log.Printf("[INFO] 已加载今日渠道成本缓存（%d个渠道有消耗）", len(todayCosts))
	}

	urlStatsLoadCtx, urlStatsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer urlStatsCancel()
	todayURLStats, err := store.GetTodayChannelURLStats(urlStatsLoadCtx, costCache.DayStart())
	if err != nil {
		log.Printf("[WARN] 加载今日 URL 运行状态失败: %v（多URL状态展示可能为空）", err)
	} else {
		urlSelector.LoadPersistedStats(todayURLStats)
		if len(todayURLStats) > 0 {
			log.Printf("[INFO] 已从日志恢复今日 URL 运行状态（%d条URL）", len(todayURLStats))
		}
	}

	disabledLoadCtx, disabledCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer disabledCancel()
	disabledURLs, err := store.LoadDisabledURLs(disabledLoadCtx)
	if err != nil {
		log.Printf("[WARN] 加载手动禁用URL状态失败: %v（重启后禁用状态可能丢失）", err)
	} else if len(disabledURLs) > 0 {
		urlSelector.LoadDisabled(disabledURLs)
		count := 0
		for _, urls := range disabledURLs {
			count += len(urls)
		}
		log.Printf("[INFO] 已恢复手动禁用URL状态（%d条）", count)
	}
}

// startBackgroundWorkers 启动 Token 统计 / Token 清理 / 状态清理三个后台协程。
// 全部纳入 s.wg，Shutdown 时通过 shutdownCh 协调退出。
func (s *Server) startBackgroundWorkers() {
	// 启动Token统计Worker（有界队列：性能可控，Shutdown可等待）
	s.wg.Add(1)
	go s.tokenStatsWorker()

	// 启动后台清理协程（Token 认证）
	s.wg.Add(1)
	go s.tokenCleanupLoop() // 定期清理过期Token

	// [FIX] P1: 启动后台状态清理协程（防止内存泄漏）
	s.wg.Add(1)
	go s.stateCleanupLoop()
}

// ================== 缓存辅助函数 ==================

func (s *Server) getChannelCache() *storage.ChannelCache {
	if s == nil {
		return nil
	}
	return s.channelCache
}

func readThroughChannelCache[T any](
	s *Server,
	readCache func(*storage.ChannelCache) (T, error),
	readStore func() (T, error),
) (T, error) {
	if cache := s.getChannelCache(); cache != nil {
		if value, err := readCache(cache); err == nil {
			return value, nil
		}
	}
	return readStore()
}

// getHostOverrides 延迟解析域名→IP覆盖表（CCLOAD_HOST_OVERRIDES）。
// 必须延迟到首次调用时求值：包级变量初始化早于 main() 中的 godotenv.Load()，
// 此时 .env 尚未加载，os.Getenv 读到空值。sync.OnceValue 保证只解析一次且并发安全。
var getHostOverrides = sync.OnceValue(func() map[string]string {
	overrides, err := parseHostOverrides(os.Getenv("CCLOAD_HOST_OVERRIDES"))
	if err != nil {
		log.Fatalf("[FATAL] CCLOAD_HOST_OVERRIDES 配置错误: %v", err)
	}
	return overrides
})

// buildHTTPTransport 构建HTTP Transport（DRY：统一配置逻辑）
// 参数:
//   - skipTLSVerify: 是否跳过TLS证书验证
func buildHTTPTransport(skipTLSVerify bool) *http.Transport {
	overrides := getHostOverrides()
	dialer := &net.Dialer{
		Timeout:   config.HTTPDialTimeout,
		KeepAlive: config.HTTPKeepAliveInterval,
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = setTCPNoDelay(fd)
			})
		},
	}

	// DNS覆盖：在拨号层替换域名为指定IP，保留TLS SNI/证书/Host头不受影响。
	// 走代理（HTTP/SOCKS5）时 DNS 在代理端解析，此包装不影响。
	dialCtx := wrapDialerWithHostOverrides(dialer.DialContext, overrides)

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // 支持 HTTPS_PROXY/HTTP_PROXY/NO_PROXY
		MaxIdleConns:        config.HTTPMaxIdleConns,
		MaxIdleConnsPerHost: config.HTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second, // 空闲连接90秒后关闭，避免僵尸连接
		MaxConnsPerHost:     config.HTTPMaxConnsPerHost,
		DialContext:         dialCtx,
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

// getClientForChannel 返回渠道对应的 HTTP 客户端。
// 无代理或空串 → 全局 client；相同 proxyURL 共享 Transport 和连接池。
//
// 缓存按 proxyURL 永久保留：渠道改 proxyURL 后旧 client 不再被引用，
// 其空闲连接随 IdleConnTimeout 自然回收，进程退出时由 Shutdown 统一关闭。
// 这是有界泄漏（proxyURL 种类有限），故意不引入 LRU/引用计数（YAGNI）。
func (s *Server) getClientForChannel(cfg *model.Config) *http.Client {
	if cfg.ProxyURL == "" {
		return s.client
	}
	if v, ok := s.proxyTransports.Load(cfg.ProxyURL); ok {
		return v.(*http.Client)
	}

	t, err := buildChannelProxyTransport(cfg.ProxyURL, s.skipTLSVerify)
	if err != nil {
		log.Printf("[WARN] 渠道 %d 代理 %q 无效，回退全局: %v", cfg.ID, cfg.ProxyURL, err)
		return s.client
	}
	c := &http.Client{Transport: t, Timeout: 0}
	if actual, loaded := s.proxyTransports.LoadOrStore(cfg.ProxyURL, c); loaded {
		t.CloseIdleConnections()
		return actual.(*http.Client)
	}
	log.Printf("[INFO] 渠道 %d 使用独立代理: %s", cfg.ID, cfg.ProxyURL)
	return c
}

// buildChannelProxyTransport 构建带代理的 Transport（HTTP/HTTPS 直连，SOCKS5 用自定义 Dialer）。
func buildChannelProxyTransport(rawProxyURL string, skipTLSVerify bool) (*http.Transport, error) {
	u, err := neturl.Parse(rawProxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	base := buildHTTPTransport(skipTLSVerify)

	switch u.Scheme {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		dialer, err := newSOCKS5Dialer(u)
		if err != nil {
			return nil, err
		}
		base.Proxy = nil
		base.DialContext = dialer
		base.ForceAttemptHTTP2 = false // SOCKS5 自定义 Dialer 与 HTTP/2 TLS 协商不兼容
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %q", u.Scheme)
	}

	return base, nil
}

// GetConfig 获取渠道配置（实现cooldown.ConfigGetter接口）
func (s *Server) GetConfig(ctx context.Context, channelID int64) (*model.Config, error) {
	if cache := s.getChannelCache(); cache != nil {
		return cache.GetConfig(ctx, channelID)
	}
	return s.store.GetConfig(ctx, channelID)
}

// GetEnabledChannelsByModel 根据模型名称获取所有启用的渠道配置
func (s *Server) GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*model.Config, error) {
			return cache.GetEnabledChannelsByModel(ctx, modelName)
		},
		func() ([]*model.Config, error) {
			return s.store.GetEnabledChannelsByModel(ctx, modelName)
		},
	)
}

// GetEnabledChannelsByType 根据渠道类型获取所有启用的渠道配置
func (s *Server) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*model.Config, error) {
			return cache.GetEnabledChannelsByType(ctx, channelType)
		},
		func() ([]*model.Config, error) {
			return s.store.GetEnabledChannelsByType(ctx, channelType)
		},
	)
}

func (s *Server) getAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*model.APIKey, error) {
			return cache.GetAPIKeys(ctx, channelID)
		},
		func() ([]*model.APIKey, error) {
			return s.store.GetAPIKeys(ctx, channelID)
		},
	)
}

func (s *Server) getAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) (map[int64]time.Time, error) {
			return cache.GetAllChannelCooldowns(ctx)
		},
		func() (map[int64]time.Time, error) {
			return s.store.GetAllChannelCooldowns(ctx)
		},
	)
}

func (s *Server) getAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) (map[int64]map[int]time.Time, error) {
			return cache.GetAllKeyCooldowns(ctx)
		},
		func() (map[int64]map[int]time.Time, error) {
			return s.store.GetAllKeyCooldowns(ctx)
		},
	)
}

func (s *Server) getAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) (map[int64]map[string]time.Time, error) {
			return cache.GetAllModelCooldowns(ctx)
		},
		func() (map[int64]map[string]time.Time, error) {
			return s.store.GetAllModelCooldowns(ctx)
		},
	)
}

// hasActiveModelCooldown 通过缓存快速判断指定渠道模型是否有活跃冷却。
// 用于成功路径的快速跳过：绝大多数模型从未被冷却，无需每次成功都执行 DELETE。
func (s *Server) hasActiveModelCooldown(ctx context.Context, channelID int64, model string) bool {
	cooldowns, err := s.getAllModelCooldowns(ctx)
	if err != nil {
		return true // 查询失败时保守处理：执行清除
	}
	models := cooldowns[channelID]
	if len(models) == 0 {
		return false
	}
	until, ok := models[model]
	return ok && until.After(time.Now())
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
	// 一并失效渠道类型映射缓存，避免 admin CRUD 后 60s TTL 脏读（read-after-write 一致性）
	s.channelTypesCacheMu.Lock()
	s.channelTypesCache = nil
	s.channelTypesCacheMu.Unlock()
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
	maxTimeout := s.nonStreamTimeout
	for _, timeouts := range s.channelTypeTimeouts {
		if timeouts.NonStreamTimeout > maxTimeout {
			maxTimeout = timeouts.NonStreamTimeout
		}
	}
	if maxTimeout > minWriteTimeout {
		return maxTimeout
	}
	return minWriteTimeout
}

func (s *Server) resolveProtocolTimeouts(cfg *model.Config, plan protocol.TransformPlan) channelTypeTimeoutConfig {
	timeouts := channelTypeTimeoutConfig{
		FirstByteTimeout: s.firstByteTimeout,
		NonStreamTimeout: s.nonStreamTimeout,
	}

	protocolKey := string(plan.UpstreamProtocol)
	if protocolKey == "" && cfg != nil {
		protocolKey = cfg.GetChannelType()
	}
	if protocolKey == "" {
		return timeouts
	}

	override, ok := s.channelTypeTimeouts[util.NormalizeChannelType(protocolKey)]
	if !ok {
		return timeouts
	}
	if override.FirstByteTimeout > 0 {
		timeouts.FirstByteTimeout = override.FirstByteTimeout
	}
	if override.NonStreamTimeout > 0 {
		timeouts.NonStreamTimeout = override.NonStreamTimeout
	}
	return timeouts
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
	apiV1.Use(captureClientRequestMetadata())
	{
		apiV1.Any("/*path", s.HandleProxyRequest)
	}
	apiV1Beta := r.Group("/v1beta")
	apiV1Beta.Use(s.authService.RequireAPIAuth())
	apiV1Beta.Use(captureClientRequestMetadata())
	{
		apiV1Beta.Any("/*path", s.HandleProxyRequest)
	}

	// 健康检查（公开访问，无需认证，K8s liveness/readiness probe）
	r.GET("/health", s.HandleHealth)

	// 公开访问的API（首页仪表盘数据）
	// [SECURITY NOTE] /public/* 端点故意不做认证，用于首页展示。
	// 认证仪表盘使用 /dashboard/summary；该公开端点保留给未登录首页集成。
	public := r.Group("/public", ZstdMiddleware())
	{
		public.GET("/summary", s.HandlePublicSummary)
		public.GET("/channel-types", s.HandleGetChannelTypes)
		public.GET("/version", s.HandlePublicVersion)
	}

	// 登录相关（公开访问）
	r.POST("/login", s.authService.HandleLogin)
	r.POST("/logout", s.authService.HandleLogout)

	// 需要身份验证的admin APIs（使用Token认证）
	admin := r.Group("/admin", ZstdMiddleware())
	admin.Use(s.authService.RequireAdminAuth())
	{
		// 渠道管理
		admin.GET("/channels", s.HandleChannels)
		admin.POST("/channels", s.HandleChannels)
		admin.GET("/channels/filter-options", s.HandleChannelsFilterOptions)
		admin.GET("/channels/export", s.HandleExportChannelsCSV)
		admin.POST("/channels/import", s.HandleImportChannelsCSV)
		admin.POST("/channels/check-duplicate", s.HandleCheckDuplicateChannel)
		admin.POST("/channels/batch-priority", s.HandleBatchUpdatePriority) // 批量更新渠道优先级
		admin.POST("/channels/batch-enabled", s.HandleBatchSetEnabled)      // 批量启用/禁用渠道
		admin.POST("/channels/batch-delete", s.HandleBatchDeleteChannels)   // 批量删除渠道
		admin.GET("/channels/:id", s.HandleChannelByID)
		admin.PUT("/channels/:id", s.HandleChannelByID)
		admin.DELETE("/channels/:id", s.HandleChannelByID)
		admin.GET("/channels/:id/keys", s.HandleChannelKeys)
		admin.GET("/channels/:id/model-stats", s.HandleChannelModelStats)
		admin.GET("/channels/:id/url-stats", s.HandleChannelURLStats)
		admin.POST("/channels/:id/url-disable", s.HandleURLDisable)
		admin.POST("/channels/:id/url-enable", s.HandleURLEnable)
		admin.POST("/channels/:id/key-disable", s.HandleAPIKeyDisable)
		admin.POST("/channels/:id/key-enable", s.HandleAPIKeyEnable)
		admin.POST("/channels/models/fetch", s.HandleFetchModelsPreview) // 临时渠道配置获取模型列表
		admin.POST("/channels/models/refresh-batch", s.HandleBatchRefreshModels)
		admin.GET("/channels/:id/models/fetch", s.HandleFetchModels) // 获取渠道可用模型列表(新增)
		admin.POST("/channels/:id/models", s.HandleAddModels)        // 添加渠道模型
		admin.DELETE("/channels/:id/models", s.HandleDeleteModels)   // 删除渠道模型
		admin.POST("/channels/:id/test", s.HandleChannelTest)
		admin.POST("/channels/:id/test-url", s.HandleChannelURLTest)
		admin.POST("/channels/:id/chat", s.HandleChannelChat)
		admin.POST("/channels/:id/cooldown", s.HandleSetChannelCooldown)
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.HandleSetKeyCooldown)
		admin.DELETE("/channels/:id/keys/:keyIndex", s.HandleDeleteAPIKey)

		// 统计分析
		admin.GET("/logs", s.HandleErrors)
		admin.GET("/logs/bootstrap", s.HandleLogsBootstrap)
		admin.POST("/debug-logs/merged-response", s.HandleMergeDebugResponse)
		admin.GET("/debug-logs/:log_id", s.HandleGetDebugLog)
		admin.GET("/active-requests", s.HandleActiveRequests) // 进行中请求（内存状态）
		admin.GET("/active-requests/:request_id/debug-log", s.HandleGetActiveRequestDebugLog)
		admin.GET("/metrics", s.HandleMetrics)
		admin.GET("/stats", s.HandleStats)
		admin.GET("/stats/filter-options", s.HandleStatsFilterOptions)
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

		// 模型指纹
		admin.GET("/fingerprints", s.HandleListFingerprints)
		admin.GET("/fingerprints/test-results", s.HandleListFingerprintTestResults)
		admin.DELETE("/fingerprints/test-results/:id", s.HandleDeleteFingerprintTestResult)
		admin.GET("/fingerprints/:id", s.HandleGetFingerprint)
		admin.DELETE("/fingerprints/:id", s.HandleDeleteFingerprint)
		admin.POST("/fingerprints/calibrate", s.HandleCalibrateFingerprint)
		admin.POST("/fingerprints/test", s.HandleTestFingerprint)
		admin.GET("/fingerprints/jobs/:id", s.HandleFingerprintJob)
		admin.GET("/fingerprints/jobs/:id/stream", s.HandleFingerprintJobStream)
		admin.POST("/fingerprints/jobs/:id/cancel", s.HandleCancelFingerprintJob)
	}

	// Web 仪表盘只读 API。API Token 会话由服务端强制绑定 auth_token_id。
	dashboard := r.Group("/dashboard", ZstdMiddleware())
	dashboard.Use(s.authService.RequireWebAuth())
	{
		dashboard.GET("/session", s.authService.HandleWebSession)
		dashboard.GET("/summary", s.HandlePublicSummary)
		dashboard.GET("/logs", s.HandleErrors)
		dashboard.GET("/logs/bootstrap", s.HandleLogsBootstrap)
		dashboard.GET("/metrics", s.HandleMetrics)
		dashboard.GET("/stats", s.HandleStats)
		dashboard.GET("/stats/filter-options", s.HandleStatsFilterOptions)
		dashboard.GET("/models", s.HandleGetModels)
		dashboard.GET("/channels", s.HandleDashboardChannels)
		dashboard.GET("/channels/filter-options", s.HandleDashboardChannelFilterOptions)
	}
	dashboardProxy := r.Group("/dashboard")
	dashboardProxy.Use(s.authService.RequireWebAuth(), s.authService.RequireWebAPITokenProxyAuth(), captureDashboardProxyMetadata())
	dashboardProxy.Any("/v1/*path", s.HandleProxyRequest)
	dashboardProxy.Any("/v1beta/*path", s.HandleProxyRequest)

	// 静态文件服务（带版本号和缓存控制）
	// - HTML：不缓存，动态替换 __VERSION__ 占位符
	// - CSS/JS：长缓存（1年），通过版本号查询参数刷新
	setupStaticFiles(r)

	// 默认首页重定向
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/index.html")
	})
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

	log.Print("[INFO] 后台状态清理循环已启动（每小时清理过期的轮询、计数器和 RPM 状态）")

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

			if s.channelRPMLimiter != nil {
				s.channelRPMLimiter.CleanupExpired()
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
		if multiplier < 0 {
			multiplier = 1
		}
		// multiplier == 0 时成本为 0（免费渠道）
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
	return modelNamesFromChannels(channels), nil
}

// getModelsByExposedProtocol 获取指定暴露协议的去重模型列表
func (s *Server) getModelsByExposedProtocol(ctx context.Context, protocol string) ([]string, error) {
	channels, err := s.store.GetEnabledChannelsByExposedProtocol(ctx, protocol)
	if err != nil {
		return nil, err
	}
	return modelNamesFromChannels(channels), nil
}

func modelNamesFromChannels(channels []*model.Config) []string {
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
	return models
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
	s.modelCatalogSyncMu.Lock()
	if s.isShuttingDown.Swap(true) {
		s.modelCatalogSyncMu.Unlock()
		select {
		case <-s.shutdownDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.modelCatalogSyncMu.Unlock()
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

	// 关闭渠道级代理 Transport 的空闲连接
	s.proxyTransports.Range(func(_, v any) bool {
		v.(*http.Client).CloseIdleConnections()
		return true
	})

	// 无论成功还是超时，都要关闭数据库连接
	if closer, ok := s.store.(interface{ Close() error }); ok {
		if closeErr := closer.Close(); closeErr != nil {
			log.Printf("[ERROR] 关闭数据库连接失败: %v", closeErr)
		}
	}

	return err
}
