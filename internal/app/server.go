package app

import (
	"container/list"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	protocolbuiltin "ccLoad/internal/protocol/builtin"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"ccLoad/internal/version"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"

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
	startedAt                     time.Time
	cpuUsage                      cpuUsageTracker // 运行状态查询间的 CPU 占用率采样(零值可用)
	store                         storage.Store
	channelCache                  *storage.ChannelCache      // 高性能渠道缓存层
	keySelector                   *KeySelector               // Key选择器（多Key支持）
	cooldownManager               *cooldown.Manager          // 统一冷却管理器
	healthCache                   *HealthCache               // 渠道健康度缓存
	costCache                     *CostCache                 // 渠道每日成本缓存
	channelRPMLimiter             *channelRPMLimiter         // 渠道RPM限制器（内存滑动窗口）
	channelConcurrencyLimiter     *channelConcurrencyLimiter // 渠道并发限制器（内存计数）
	statsCache                    *StatsCache                // 统计结果缓存层
	updateManager                 *version.UpdateManager     // 版本检查与可选自动应用的唯一状态源
	channelBalancer               *SmoothWeightedRR          // 渠道负载均衡器（平滑加权轮询）
	urlSelector                   *URLSelector               // URL选择器（多URL场景的延迟追踪与冷却）
	disabledURLSyncMu             sync.Mutex
	protocolRegistry              *protocol.Registry
	client                        *http.Client // HTTP客户端（全局默认）
	antigravityClient             *http.Client // Antigravity 专用标准 HTTP/1.1 客户端
	xaiSSOClient                  *http.Client // xAI Web SSO 专用 HTTP/1.1 客户端
	proxyTransports               sync.Map     // upstreamHTTPClientCacheKey → *http.Client（渠道级代理缓存）
	antigravityTransports         *antigravityHTTPClientCache
	antigravityTransportMu        sync.Mutex
	protocolCapabilities          protocolCapabilityCache
	skipTLSVerify                 bool                  // 透传给渠道级 Transport
	activeRequests                *activeRequestManager // 进行中请求（内存状态，不持久化）
	httpRuntime                   httpProxyRuntimeMetrics
	restartMu                     sync.RWMutex
	restartFunc                   func()
	responsesExecutionSessions    *responsesExecutionSessionStore
	responsesWebsocketConnections *responsesWebsocketConnectionLimiter
	codexOAuth                    *codexOAuthManager
	codexService                  *codexauth.Service
	codexCredentials              *codexCredentialManager
	codexQuotaResetInFlight       sync.Map
	antigravityOAuth              *codexOAuthManager
	antigravityCredentials        *antigravityCredentialManager
	antigravityService            *antigravityauth.Service
	xaiService                    *xaiauth.Service
	xaiCredentials                *xaiCredentialManager
	xaiOAuth                      *xaiOAuthManager
	anthropicService              *anthropicauth.Service
	anthropicCredentials          *anthropicCredentialManager
	anthropicOAuth                *codexOAuthManager
	zaiService                    *zaiauth.Service
	zaiCredentials                *zaiCredentialManager
	zaiOAuth                      *zaiOAuthManager
	cursorService                 *cursorauth.Service
	cursorCredentials             *cursorCredentialManager
	cursorOAuth                   *cursorOAuthManager
	cursorRunner                  cursorauth.Runner
	antigravityPromptMatcher      *regexp.Regexp
	scheduledChannelChecksRunning atomic.Bool

	// 异步统计（有界队列，避免每请求起goroutine）
	tokenStatsCh               chan tokenStatsUpdate
	tokenStatsDropCount        atomic.Int64
	codexPassiveUsageCh        chan codexPassiveUsageTask
	codexPassiveUsageDropCount atomic.Int64

	// 运行时配置（启动时从数据库加载，修改后重启生效）
	maxKeyRetries    int // 单个渠道内最大Key重试次数
	bodyLimits       requestBodyLimits
	firstByteTimeout time.Duration // 上游首字节超时（流式请求）
	httpReadTimeout  time.Duration // 下游请求读取超时（HTTP Server ReadTimeout）
	streamTimeout    time.Duration // 流式请求总超时
	nonStreamTimeout time.Duration // 非流式请求超时
	// 上游 HTTP/1.1、HTTP/2 和 WebSocket 物理连接最长复用时间；0 表示不限制。
	upstreamConnectionMaxAge time.Duration
	// 仅供测试注入（缩短下游与上游 WebSocket 的 idle/ping 间隔以覆盖保活路径）；
	// 生产始终为零值，实际取值回退到各自的默认常量。
	responsesWebsocketIdleTimeoutOverride  time.Duration
	responsesWebsocketPingIntervalOverride time.Duration
	protocolTimeouts                       map[string]protocolTimeoutConfig // 按运行时上游协议覆盖超时，0=回退全局
	// 模型匹配配置（启动时从数据库加载，修改后重启生效）
	modelFuzzyMatch           bool // 未命中时启用模糊匹配（子串匹配+版本排序）
	activeRequestTitleEnabled bool
	codexMap429To503          bool
	// 渠道未配置专属规则时使用的进程级默认规则。
	globalCooldownDetectionRules *model.CooldownDetectionRules

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

	oauthCredentialImportRunMu   sync.Mutex
	oauthCredentialImportJobsMu  sync.Mutex
	oauthCredentialImportJobs    *oauthCredentialImportJobManager
	oauthCredentialCleanupJobsMu sync.Mutex
	oauthCredentialCleanupJobs   *oauthCredentialCleanupJobManager
	oauthCredentialRefreshes     *oauthCredentialRefreshTracker
}

// NewServer 创建并初始化一个新的 Server 实例
func NewServer(store storage.Store) *Server {
	startedAt := time.Now()
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
	warnMigratedEnvSettings()

	// 运行时配置必须归属具体 Server/存储实例，构造函数不修改进程级状态。
	bodyLimits := newRequestBodyLimits(runtimeCfg.MaxBodyBytes, runtimeCfg.MaxImageBodyBytes)
	store.ConfigureCooldown(runtimeCfg.Cooldown)

	maxConcurrency := runtimeCfg.MaxConcurrency

	// TLS证书验证配置（仅环境变量）
	// 这是一个危险开关：一旦关闭证书校验，上游 HTTPS 等同明文 + 任意中间人。
	skipTLSVerify := os.Getenv("CCLOAD_ALLOW_INSECURE_TLS") == "1"
	if skipTLSVerify {
		log.Print("[WARN] 已禁用上游 TLS 证书校验（InsecureSkipVerify=true）：仅用于临时排障/受控内网环境")
	}

	// 构建HTTP Transport（使用统一函数，消除DRY违反）
	transport := buildHTTPTransport(skipTLSVerify)
	log.Print("[INFO] HTTP/2已启用（HTTPS自动协商）；Antigravity 固定使用 HTTP/1.1")
	logHostOverrides(getHostOverrides())

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &Server{
		startedAt:        startedAt,
		store:            store,
		configService:    configService,
		loginRateLimiter: util.NewLoginRateLimiter(),

		// 运行时配置（启动时加载，修改后重启生效）
		maxKeyRetries:            runtimeCfg.MaxKeyRetries,
		bodyLimits:               bodyLimits,
		firstByteTimeout:         runtimeCfg.FirstByteTimeout,
		httpReadTimeout:          runtimeCfg.HTTPReadTimeout,
		streamTimeout:            runtimeCfg.StreamTimeout,
		nonStreamTimeout:         runtimeCfg.NonStreamTimeout,
		upstreamConnectionMaxAge: runtimeCfg.UpstreamConnectionMaxAge,
		protocolTimeouts:         runtimeCfg.ProtocolTimeouts,
		// 模型匹配配置（启动时加载，修改后重启生效）
		modelFuzzyMatch:              runtimeCfg.ModelFuzzyMatch,
		activeRequestTitleEnabled:    runtimeCfg.ActiveRequestTitleEnabled,
		codexMap429To503:             runtimeCfg.CodexMap429To503,
		globalCooldownDetectionRules: runtimeCfg.GlobalCooldownDetectionRules,

		// HTTP客户端：不设置请求总超时，连接复用时限只轮换连接池，不中断在途请求。
		client:                newUpstreamHTTPClient(transport, runtimeCfg.UpstreamConnectionMaxAge),
		antigravityClient:     newAntigravityHTTPClient(transport, runtimeCfg.UpstreamConnectionMaxAge),
		antigravityTransports: newAntigravityHTTPClientCache(antigravityHTTPClientCacheCapacity),
		xaiSSOClient:          newXAISSOHTTPClient(transport),
		skipTLSVerify:         skipTLSVerify,

		// 并发控制：使用信号量限制最大并发请求数
		concurrencySem: make(chan struct{}, maxConcurrency),
		maxConcurrency: maxConcurrency,

		// 初始化优雅关闭机制
		baseCtx:                  baseCtx,
		baseCancel:               baseCancel,
		shutdownCh:               make(chan struct{}),
		shutdownDone:             make(chan struct{}),
		oauthCredentialRefreshes: newOAuthCredentialRefreshTracker(),

		// Token统计队列（避免每请求起goroutine）
		tokenStatsCh:        make(chan tokenStatsUpdate, config.DefaultTokenStatsBufferSize),
		codexPassiveUsageCh: make(chan codexPassiveUsageTask, codexPassiveUsageQueueSize),

		activeRequests: newActiveRequestManager(),
		responsesExecutionSessions: newResponsesExecutionSessionStore(
			configService,
			bodyLimits.maxForPath("/v1/responses"),
			runtimeCfg.UpstreamConnectionMaxAge,
		),
		responsesWebsocketConnections: newResponsesWebsocketConnectionLimiter(
			configService.GetInt(responsesWebsocketMaxConnectionsSetting, defaultResponsesWebsocketConnectionLimit),
			configService.GetInt(
				responsesWebsocketMaxConnectionsPerTokenSetting,
				defaultResponsesWebsocketConnectionPerSubjectLimit,
			),
		),
		channelRPMLimiter:         newChannelRPMLimiter(time.Now),
		channelConcurrencyLimiter: newChannelConcurrencyLimiter(),
	}

	reg := protocol.NewRegistry()
	protocolbuiltin.Register(reg)
	s.protocolRegistry = reg

	// 初始化高性能缓存层（60秒TTL，避免数据库性能杀手查询）
	s.channelCache = storage.NewChannelCache(store, 60*time.Second)
	channelLoadCtx, channelLoadCancel := context.WithTimeout(context.Background(), 5*time.Second)
	channels, err := store.ListConfigs(channelLoadCtx)
	channelLoadCancel()
	if err != nil {
		log.Fatalf("[FATAL] 加载渠道配置失败: %v", err)
	}
	log.Printf("[INFO] 已加载渠道配置（%d项）", len(channels))

	codexOAuthService := codexauth.NewService(s.client)
	s.codexService = codexOAuthService
	s.codexCredentials = newCodexCredentialManager(codexOAuthService, store, s.getClientForChannel, func(int64) {
		s.InvalidateChannelListCache()
	})
	s.codexCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.codexOAuth = newCodexOAuthManager(codexOAuthService, store, func(channelID int64) {
		s.codexCredentials.invalidate(channelID)
		s.InvalidateChannelListCache()
	})
	s.antigravityService = antigravityauth.NewService(s.antigravityClient)
	s.antigravityPromptMatcher = loadAntigravityPromptMatcher(configService)
	s.antigravityCredentials = newAntigravityCredentialManager(s.antigravityService, store, s.getClientForChannel, func(int64) {
		s.InvalidateChannelListCache()
	})
	s.antigravityCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.antigravityOAuth = newAntigravityOAuthManager(s.antigravityService, store, func(channelID int64) {
		s.antigravityCredentials.invalidate(channelID)
		s.InvalidateChannelListCache()
	})
	s.xaiService = xaiauth.NewService(s.client)
	s.xaiCredentials = newXAICredentialManager(store, s.getClientForChannel, func(int64) {
		s.InvalidateChannelListCache()
	})
	s.xaiCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.xaiOAuth = newXAIOAuthManager(
		s.baseCtx,
		s.xaiService,
		func(ctx context.Context, credential *xaiauth.Credential) (*xaiauth.Credential, error) {
			return completeXAICredential(ctx, s.xaiService, s.client, credential, xaiauth.CLIBaseURL)
		},
		func(ctx context.Context, credential *xaiauth.Credential) (int64, error) {
			cfg, _, err := createOrUpdateXAIChannel(ctx, store, credential)
			if err != nil {
				return 0, err
			}
			s.xaiCredentials.invalidate(cfg.ID)
			s.InvalidateChannelListCache()
			return cfg.ID, nil
		},
	)
	s.anthropicService = anthropicauth.NewService(s.client)
	s.anthropicCredentials = newAnthropicCredentialManager(
		s.anthropicService, store, s.getClientForChannel, func(int64) { s.InvalidateChannelListCache() },
	)
	s.anthropicCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.anthropicOAuth = newAnthropicOAuthManager(s.anthropicService, store, func(channelID int64) {
		s.anthropicCredentials.invalidate(channelID)
		s.InvalidateChannelListCache()
	})
	s.zaiService = zaiauth.NewService(s.client)
	s.zaiCredentials = newZAICredentialManager(store, s.getClientForChannel, func(int64) {
		s.InvalidateChannelListCache()
	})
	s.zaiCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.zaiOAuth = newZAIOAuthManager(
		s.baseCtx,
		s.zaiService,
		func(ctx context.Context, accessToken string) (*zaiauth.Credential, error) {
			return s.buildZAICredential(ctx, zaiCredentialImportRequest{AccessToken: accessToken})
		},
		func(ctx context.Context, credential *zaiauth.Credential) (int64, string, error) {
			cfg, _, err := s.commitZAICredential(ctx, credential)
			if err != nil {
				return 0, "", err
			}
			return cfg.ID, cfg.Name, nil
		},
	)
	s.cursorService = cursorauth.NewService(s.client)
	s.cursorCredentials = newCursorCredentialManager(store, s.getClientForChannel, func(int64) {
		s.InvalidateChannelListCache()
	})
	s.cursorCredentials.refreshTracker = s.oauthCredentialRefreshes
	s.cursorRunner = cursorauth.NewCLIRunner()
	s.cursorOAuth = newCursorOAuthManager(
		s.baseCtx,
		s.cursorService,
		func(ctx context.Context, credential *cursorauth.Credential) (int64, string, error) {
			cfg, _, err := s.commitCursorCredential(ctx, credential)
			if err != nil {
				return 0, "", err
			}
			return cfg.ID, cfg.Name, nil
		},
	)

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
	// 2. AuthService（负责认证授权）
	// 初始化时自动从数据库加载API访问令牌
	s.authService = NewAuthService(
		password,
		s.loginRateLimiter,
		store, // 传入store用于热更新令牌
	)
	s.authService.RegisterWebSessionRevokeHook(s.xaiOAuth.cancelByAdmin)

	// 启动后台 worker（Token 统计 / Token 清理 / 状态清理）
	s.startBackgroundWorkers()

	channelCheckIntervalHours := normalizeChannelCheckIntervalHours(
		configService.GetFloat("channel_check_interval_hours", defaultChannelCheckIntervalHours),
	)
	if channelCheckIntervalHours == 0 {
		log.Print("[INFO] 渠道定时检测未启用（channel_check_interval_hours=0）")
	} else {
		interval, _ := settingDurationFromFloat64(channelCheckIntervalHours, time.Hour)
		s.startScheduledChannelCheckLoop(interval)
	}

	s.oauthCredentialImportJobs = newOAuthCredentialImportJobManager(s.baseCtx, oauthCredentialImportMaxRunningJobs)
	s.oauthCredentialCleanupJobs = newOAuthCredentialCleanupJobManager(s.baseCtx)

	// 所有启动关键状态完成加载后，再启动非关键后台任务。
	// SQLite 纯模式只有一个连接，维护任务不得与认证/渠道初始化竞争。
	s.healthCache.Start()
	s.logService.StartWorkers()
	s.logService.StartCleanupLoop()

	return s

}

// RefreshAntigravityUserAgent resolves the Antigravity Hub version before the
// HTTP server starts accepting requests. The returned value is always usable;
// on failure it is the fixed production fallback.
func (s *Server) RefreshAntigravityUserAgent(ctx context.Context) (string, error) {
	if s == nil || s.antigravityService == nil {
		return antigravityauth.DefaultUserAgent, errors.New("antigravity service is unavailable")
	}
	err := s.antigravityService.RefreshUserAgent(ctx)
	return s.antigravityUserAgent(), err
}

func (s *Server) antigravityUserAgent() string {
	if s == nil {
		return antigravityauth.DefaultUserAgent
	}
	return s.antigravityService.RequestUserAgent()
}

func loadAntigravityPromptMatcher(configService *ConfigService) *regexp.Regexp {
	if configService == nil {
		return nil
	}
	var words []string
	if err := json.Unmarshal([]byte(configService.GetString("antigravity_sensitive_words", config.DefaultAntigravitySensitiveWordsJSON)), &words); err != nil {
		log.Printf("[WARN] 无效的 antigravity_sensitive_words，已禁用提示词替换: %v", err)
		return nil
	}
	return buildAntigravitySensitiveWordMatcher(words)
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

	interval, valid := settingDurationFromFloat64(intervalHours, time.Hour)
	if !valid || interval <= 0 {
		log.Printf("[WARN] 无效的模型目录同步间隔: %v 小时", intervalHours)
		return
	}
	s.wg.Add(1)
	go s.runModelCatalogSyncLoop(syncer, interval)
}

type protocolTimeoutConfig struct {
	FirstByteTimeout time.Duration
	StreamTimeout    time.Duration
	NonStreamTimeout time.Duration
}

// serverRuntimeConfig 启动期从数据库读取的运行时配置（修改后重启生效）
type serverRuntimeConfig struct {
	MaxKeyRetries                int
	MaxConcurrency               int
	MaxBodyBytes                 int
	MaxImageBodyBytes            int
	HTTPReadTimeout              time.Duration
	FirstByteTimeout             time.Duration
	StreamTimeout                time.Duration
	NonStreamTimeout             time.Duration
	UpstreamConnectionMaxAge     time.Duration
	ProtocolTimeouts             map[string]protocolTimeoutConfig
	LogRetentionDays             int
	ModelFuzzyMatch              bool
	ActiveRequestTitleEnabled    bool
	CodexMap429To503             bool
	GlobalCooldownDetectionRules *model.CooldownDetectionRules
	Cooldown                     util.CooldownSettings
}

func loadGlobalCooldownDetectionRules(cs *ConfigService) *model.CooldownDetectionRules {
	rules, err := parseGlobalCooldownDetectionRules(cs.GetString(globalCooldownDetectionRulesSettingKey, "{}"))
	if err != nil {
		log.Printf("[WARN] 无效的 %s，已回退为空规则: %v", globalCooldownDetectionRulesSettingKey, err)
		return nil
	}
	return rules
}

// loadPositiveInt 读取必须为正数的配置项，非法值回退默认并告警。
// loadHTTPReadTimeout 读取下游请求读取超时。0 表示使用内建默认值，负数非法。
func loadHTTPReadTimeout(cs *ConfigService) time.Duration {
	value := cs.GetDuration(config.HTTPReadTimeoutSettingKey, 0)
	if value < 0 {
		log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已使用默认值 %v",
			config.HTTPReadTimeoutSettingKey, value, config.DefaultHTTPReadTimeout)
		return config.DefaultHTTPReadTimeout
	}
	if value == 0 {
		return config.DefaultHTTPReadTimeout
	}
	return value
}

func loadPositiveInt(cs *ConfigService, key string, defaultValue int) int {
	value := cs.GetInt(key, defaultValue)
	if value <= 0 {
		log.Printf("[WARN] 无效的 %s=%d（必须 > 0），已使用默认值 %d", key, value, defaultValue)
		return defaultValue
	}
	return value
}

// loadPositiveDurationSeconds 读取必须为正数且可安全转换为 time.Duration 的秒数。
func loadPositiveDurationSeconds(cs *ConfigService, key string, defaultValue int) int {
	value := loadPositiveInt(cs, key, defaultValue)
	if _, valid := settingDurationFromInt64(int64(value), time.Second); !valid {
		log.Printf("[WARN] 无效的 %s=%d（超出可表示的时长），已使用默认值 %d", key, value, defaultValue)
		return defaultValue
	}
	return value
}

// loadCooldownSettings 从系统设置读取冷却时长（秒），非法值回退默认。
func loadCooldownSettings(cs *ConfigService) util.CooldownSettings {
	settings := util.CooldownSettings{
		AuthSec:      loadPositiveDurationSeconds(cs, "cooldown_auth_seconds", config.DefaultCooldownAuthSeconds),
		ServerSec:    loadPositiveDurationSeconds(cs, "cooldown_server_seconds", config.DefaultCooldownServerSeconds),
		TimeoutSec:   loadPositiveDurationSeconds(cs, "cooldown_timeout_seconds", config.DefaultCooldownTimeoutSeconds),
		RateLimitSec: loadPositiveDurationSeconds(cs, "cooldown_rate_limit_seconds", config.DefaultCooldownRateLimitSeconds),
		MaxSec:       loadPositiveDurationSeconds(cs, "cooldown_max_seconds", config.DefaultCooldownMaxSeconds),
		MinSec:       loadPositiveDurationSeconds(cs, "cooldown_min_seconds", config.DefaultCooldownMinSeconds),
	}
	// 上下限倒挂会让指数退避直接被 max 钳死在下限之下，语义不可用，回退默认对。
	if settings.MinSec > settings.MaxSec {
		log.Printf("[WARN] cooldown_min_seconds=%d 大于 cooldown_max_seconds=%d，已回退默认值 %d/%d",
			settings.MinSec, settings.MaxSec, config.DefaultCooldownMinSeconds, config.DefaultCooldownMaxSeconds)
		settings.MinSec = config.DefaultCooldownMinSeconds
		settings.MaxSec = config.DefaultCooldownMaxSeconds
	}
	return settings
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

	streamTimeout := cs.GetDuration("stream_timeout", 0)
	if streamTimeout < 0 {
		log.Printf("[WARN] 无效的 stream_timeout=%v（必须 >= 0，0=禁用），已设为 0", streamTimeout)
		streamTimeout = 0
	}

	nonStreamTimeout := cs.GetDuration("non_stream_timeout", 120*time.Second)
	if nonStreamTimeout < 0 {
		log.Printf("[WARN] 无效的 non_stream_timeout=%v（必须 >= 0，0=禁用），已使用默认值 %v", nonStreamTimeout, 120*time.Second)
		nonStreamTimeout = 120 * time.Second
	}

	upstreamConnectionMaxAge := cs.GetDuration("upstream_connection_reuse_limit_seconds", 0)
	if upstreamConnectionMaxAge < 0 {
		log.Printf("[WARN] 无效的 upstream_connection_reuse_limit_seconds=%v（必须 >= 0，0=不限制），已设为 0", upstreamConnectionMaxAge)
		upstreamConnectionMaxAge = 0
	}

	protocolTimeouts := loadProtocolTimeouts(cs)

	logRetentionDays := cs.GetInt("log_retention_days", 7)

	modelFuzzyMatch := cs.GetBool("model_fuzzy_match", false)
	if modelFuzzyMatch {
		log.Print("[INFO] 已启用模型模糊匹配：未命中时进行子串匹配并按版本排序选择最新模型")
	}

	return serverRuntimeConfig{
		MaxKeyRetries:                maxKeyRetries,
		MaxConcurrency:               loadPositiveInt(cs, "max_concurrency", config.DefaultMaxConcurrency),
		MaxBodyBytes:                 loadPositiveInt(cs, "max_body_bytes", config.DefaultMaxBodyBytes),
		MaxImageBodyBytes:            loadPositiveInt(cs, "max_image_body_bytes", config.DefaultMaxImageBodyBytes),
		HTTPReadTimeout:              loadHTTPReadTimeout(cs),
		FirstByteTimeout:             firstByteTimeout,
		StreamTimeout:                streamTimeout,
		NonStreamTimeout:             nonStreamTimeout,
		UpstreamConnectionMaxAge:     upstreamConnectionMaxAge,
		ProtocolTimeouts:             protocolTimeouts,
		LogRetentionDays:             logRetentionDays,
		ModelFuzzyMatch:              modelFuzzyMatch,
		ActiveRequestTitleEnabled:    cs.GetBool(config.ActiveRequestTitleEnabledSettingKey, false),
		CodexMap429To503:             cs.GetBool(config.CodexMap429To503SettingKey, false),
		GlobalCooldownDetectionRules: loadGlobalCooldownDetectionRules(cs),
		Cooldown:                     loadCooldownSettings(cs),
	}
}

func loadProtocolTimeouts(cs *ConfigService) map[string]protocolTimeoutConfig {
	supported := protocol.AllProtocols()
	timeouts := make(map[string]protocolTimeoutConfig, len(supported))
	for _, supportedProtocol := range supported {
		name := string(supportedProtocol)
		firstByteTimeout := cs.GetDuration(protocolFirstByteTimeoutSettingKey(name), 0)
		if firstByteTimeout < 0 {
			log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已设为 0（回退全局首字超时）",
				protocolFirstByteTimeoutSettingKey(name), firstByteTimeout)
			firstByteTimeout = 0
		}

		nonStreamTimeout := cs.GetDuration(protocolNonStreamTimeoutSettingKey(name), 0)
		if nonStreamTimeout < 0 {
			log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已设为 0（回退全局非流超时）",
				protocolNonStreamTimeoutSettingKey(name), nonStreamTimeout)
			nonStreamTimeout = 0
		}

		timeouts[name] = protocolTimeoutConfig{
			FirstByteTimeout: firstByteTimeout,
			NonStreamTimeout: nonStreamTimeout,
		}
	}
	return timeouts
}

// migratedEnvSettings 已迁移到系统设置的旧环境变量 → 新配置项。
// 保留告警而非静默忽略：老部署仍在 .env 里设着这些值，不提示会让人以为限额还生效。
var migratedEnvSettings = map[string]string{
	"CCLOAD_MAX_CONCURRENCY":         "max_concurrency",
	"CCLOAD_MAX_BODY_BYTES":          "max_body_bytes / max_image_body_bytes",
	"CCLOAD_COOLDOWN_AUTH_SEC":       "cooldown_auth_seconds",
	"CCLOAD_COOLDOWN_SERVER_SEC":     "cooldown_server_seconds",
	"CCLOAD_COOLDOWN_TIMEOUT_SEC":    "cooldown_timeout_seconds",
	"CCLOAD_COOLDOWN_RATE_LIMIT_SEC": "cooldown_rate_limit_seconds",
	"CCLOAD_COOLDOWN_MAX_SEC":        "cooldown_max_seconds",
	"CCLOAD_COOLDOWN_MIN_SEC":        "cooldown_min_seconds",
}

func warnMigratedEnvSettings() {
	keys := make([]string, 0, len(migratedEnvSettings))
	for key := range migratedEnvSettings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if os.Getenv(key) != "" {
			log.Printf("[WARN] 环境变量 %s 已废弃且不再生效，请改用系统设置项 %s", key, migratedEnvSettings[key])
		}
	}
}

func protocolFirstByteTimeoutSettingKey(upstreamProtocol string) string {
	return util.NormalizeProtocol(upstreamProtocol) + "_first_byte_timeout"
}

func protocolNonStreamTimeoutSettingKey(upstreamProtocol string) string {
	return util.NormalizeProtocol(upstreamProtocol) + "_non_stream_timeout"
}

// loadHealthScoreConfig 从 ConfigService 加载健康度配置，无效值兜底为默认值
func loadHealthScoreConfig(cs *ConfigService) model.HealthScoreConfig {
	defaultHealthCfg := model.DefaultHealthScoreConfig()
	successRatePenaltyWeight := cs.GetInt("success_rate_penalty_weight", defaultHealthCfg.SuccessRatePenaltyWeight)
	if successRatePenaltyWeight < 0 {
		log.Printf("[WARN] 无效的 success_rate_penalty_weight=%d（必须 >= 0），已使用默认值 %d", successRatePenaltyWeight, defaultHealthCfg.SuccessRatePenaltyWeight)
		successRatePenaltyWeight = defaultHealthCfg.SuccessRatePenaltyWeight
	}
	windowMinutes := cs.GetInt("health_score_window_minutes", defaultHealthCfg.WindowMinutes)
	if _, valid := settingDurationFromInt64(int64(windowMinutes), time.Minute); windowMinutes < 1 || !valid {
		log.Printf("[WARN] 无效的 health_score_window_minutes=%d（必须 >= 1），已使用默认值 %d", windowMinutes, defaultHealthCfg.WindowMinutes)
		windowMinutes = defaultHealthCfg.WindowMinutes
	}
	updateInterval := cs.GetInt("health_score_update_interval", defaultHealthCfg.UpdateIntervalSeconds)
	if _, valid := settingDurationFromInt64(int64(updateInterval), time.Second); updateInterval < 1 || !valid {
		log.Printf("[WARN] 无效的 health_score_update_interval=%d（必须 >= 1），已使用默认值 %d", updateInterval, defaultHealthCfg.UpdateIntervalSeconds)
		updateInterval = defaultHealthCfg.UpdateIntervalSeconds
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
	} else {
		urlSelector.LoadDisabled(disabledURLs)
		count := 0
		for _, urls := range disabledURLs {
			count += len(urls)
		}
		if count > 0 {
			log.Printf("[INFO] 已恢复手动禁用URL状态（%d条）", count)
		}
	}
}

// startBackgroundWorkers 启动所有常驻后台协程。
// 全部纳入 s.wg，Shutdown 时通过 shutdownCh 协调退出。
func (s *Server) startBackgroundWorkers() {
	// 启动Token统计Worker（有界队列：性能可控，Shutdown可等待）
	s.wg.Add(1)
	go s.tokenStatsWorker()

	// Codex SSE 额度是旁路元数据，异步落库不得阻塞流式响应。
	s.wg.Add(1)
	go s.codexPassiveUsageWorker()

	// 启动后台清理协程（Token 认证）
	s.wg.Add(1)
	go s.tokenCleanupLoop() // 定期清理过期Token

	// [FIX] P1: 启动后台状态清理协程（防止内存泄漏）
	s.wg.Add(1)
	go s.stateCleanupLoop()

	s.wg.Add(1)
	go s.disabledURLReloadLoop()

	s.wg.Add(1)
	go s.responsesExecutionSessionCleanupLoop()
}

func (s *Server) disabledURLReloadLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.disabledURLSyncMu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			disabled, err := s.store.LoadDisabledURLs(ctx)
			cancel()
			if err != nil {
				s.disabledURLSyncMu.Unlock()
				log.Printf("[WARN] 刷新手动禁用URL状态失败: %v", err)
				continue
			}
			s.urlSelector.LoadDisabled(disabled)
			s.disabledURLSyncMu.Unlock()
		}
	}
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

const antigravityHTTPClientCacheCapacity = 8192

var errAntigravityHTTPClientCacheClosed = errors.New("antigravity HTTP client cache is closed")

// antigravityHTTPClientCache is a bounded LRU of Antigravity clients. A client
// owns a transport and therefore a physical connection pool; evicting it must
// close idle connections or credential/proxy churn would retain sockets forever.
type antigravityHTTPClientCache struct {
	mu       sync.Mutex
	capacity int
	entries  map[upstreamHTTPClientCacheKey]*list.Element
	order    *list.List
	closed   bool
}

type antigravityHTTPClientCacheEntry struct {
	key    upstreamHTTPClientCacheKey
	client *http.Client
}

func newAntigravityHTTPClientCache(capacity int) *antigravityHTTPClientCache {
	if capacity < 1 {
		capacity = 1
	}
	return &antigravityHTTPClientCache{
		capacity: capacity,
		entries:  make(map[upstreamHTTPClientCacheKey]*list.Element, capacity),
		order:    list.New(),
	}
}

func (c *antigravityHTTPClientCache) getOrCreate(
	key upstreamHTTPClientCacheKey,
	create func() (*http.Client, error),
) (*http.Client, error) {
	if c == nil {
		return create()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errAntigravityHTTPClientCacheClosed
	}
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		client := element.Value.(antigravityHTTPClientCacheEntry).client
		c.mu.Unlock()
		return client, nil
	}
	c.mu.Unlock()

	client, err := create()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		closeUpstreamHTTPClient(client)
		return nil, errAntigravityHTTPClientCacheClosed
	}
	// Another goroutine may have populated the same key while the transport
	// was being built. Keep one client and immediately discard the duplicate.
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		actual := element.Value.(antigravityHTTPClientCacheEntry).client
		c.mu.Unlock()
		closeUpstreamHTTPClient(client)
		return actual, nil
	}
	element := c.order.PushFront(antigravityHTTPClientCacheEntry{key: key, client: client})
	c.entries[key] = element
	var evicted *http.Client
	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			entry := oldest.Value.(antigravityHTTPClientCacheEntry)
			delete(c.entries, entry.key)
			c.order.Remove(oldest)
			evicted = entry.client
		}
	}
	c.mu.Unlock()
	if evicted != nil {
		closeUpstreamHTTPClient(evicted)
	}
	return client, nil
}

func (c *antigravityHTTPClientCache) closeAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	clients := make([]*http.Client, 0, len(c.entries))
	for _, element := range c.entries {
		clients = append(clients, element.Value.(antigravityHTTPClientCacheEntry).client)
	}
	c.entries = make(map[upstreamHTTPClientCacheKey]*list.Element, c.capacity)
	c.order.Init()
	c.mu.Unlock()
	for _, client := range clients {
		closeUpstreamHTTPClient(client)
	}
}

type upstreamHTTPClientCacheKey struct {
	proxyURL              string
	antigravityHTTP11Only bool
	credentialScope       string
}

func (s *Server) getAntigravityHTTPClientCache() *antigravityHTTPClientCache {
	s.antigravityTransportMu.Lock()
	defer s.antigravityTransportMu.Unlock()
	if s.antigravityTransports == nil {
		s.antigravityTransports = newAntigravityHTTPClientCache(antigravityHTTPClientCacheCapacity)
	}
	return s.antigravityTransports
}

// antigravityCredentialPoolScope deliberately uses the refresh token rather
// than the access token. Access tokens rotate; the refresh token identifies the
// OAuth identity whose HTTP/1.1 connection pool must remain isolated.
func antigravityCredentialPoolScope(cfg *model.Config) string {
	if cfg == nil || !cfg.UsesAntigravityOAuth() {
		return ""
	}
	if credential, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential)); err == nil && credential.RefreshToken != "" {
		digest := sha256.Sum256([]byte(credential.RefreshToken))
		return "refresh:" + hex.EncodeToString(digest[:])
	}
	// A malformed/incomplete credential must not make unrelated channels share
	// a pool. Channel ID is stable and is only a defensive fallback.
	if cfg.ID != 0 {
		return fmt.Sprintf("channel:%d", cfg.ID)
	}
	return ""
}

func (s *Server) getClientForChannel(cfg *model.Config) *http.Client {
	if cfg == nil {
		return s.client
	}
	defaultClient := s.client
	clientFactory := newUpstreamHTTPClient
	antigravityHTTP11Only := cfg.UsesAntigravityOAuth()
	if antigravityHTTP11Only {
		defaultClient = s.antigravityClient
		clientFactory = newAntigravityHTTPClient
		// Tests and embedders may inject a semantic RoundTripper. It already is
		// the transport boundary; replacing it with a network transport would
		// silently bypass the injected behavior.
		if defaultClient != nil {
			switch defaultClient.Transport.(type) {
			case *http.Transport, *upstreamConnectionAgeTransport:
			default:
				return defaultClient
			}
		}
		credentialScope := antigravityCredentialPoolScope(cfg)
		// Channels without a parseable credential are retained on the legacy
		// global client. Real OAuth credentials always receive their own scope.
		if credentialScope != "" {
			proxyURL := strings.TrimSpace(cfg.ProxyURL)
			key := upstreamHTTPClientCacheKey{
				proxyURL:              proxyURL,
				antigravityHTTP11Only: true,
				credentialScope:       credentialScope,
			}
			cache := s.getAntigravityHTTPClientCache()
			client, err := cache.getOrCreate(key, func() (*http.Client, error) {
				var transport *http.Transport
				var err error
				if proxyURL == "" {
					transport = buildHTTPTransport(s.skipTLSVerify)
				} else {
					transport, err = buildChannelProxyTransport(proxyURL, s.skipTLSVerify)
				}
				if err != nil {
					return nil, err
				}
				return clientFactory(transport, s.upstreamConnectionMaxAge), nil
			})
			if err == nil {
				return client
			}
			log.Printf("[WARN] 渠道 %d 代理 %q 无效，回退凭证隔离的直连池: %v", cfg.ID, cfg.ProxyURL, err)
			fallbackKey := key
			fallbackKey.proxyURL = ""
			fallback, fallbackErr := cache.getOrCreate(fallbackKey, func() (*http.Client, error) {
				return clientFactory(buildHTTPTransport(s.skipTLSVerify), s.upstreamConnectionMaxAge), nil
			})
			if fallbackErr == nil {
				return fallback
			}
		}
	}
	if cfg.ProxyURL == "" {
		return defaultClient
	}
	cacheKey := upstreamHTTPClientCacheKey{
		proxyURL:              cfg.ProxyURL,
		antigravityHTTP11Only: antigravityHTTP11Only,
	}
	if v, ok := s.proxyTransports.Load(cacheKey); ok {
		return v.(*http.Client)
	}

	t, err := buildChannelProxyTransport(cfg.ProxyURL, s.skipTLSVerify)
	if err != nil {
		log.Printf("[WARN] 渠道 %d 代理 %q 无效，回退全局: %v", cfg.ID, cfg.ProxyURL, err)
		return defaultClient
	}
	c := clientFactory(t, s.upstreamConnectionMaxAge)
	if actual, loaded := s.proxyTransports.LoadOrStore(cacheKey, c); loaded {
		closeUpstreamHTTPClient(c)
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

// getEnabledChannelsSnapshotByModel 返回路由热路径使用的只读渠道快照。
// Config 指针由 ChannelCache 持有，调用方只能过滤或重排外层 slice，不能修改 Config。
func (s *Server) getEnabledChannelsSnapshotByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*model.Config, error) {
			return cache.GetEnabledChannelsSnapshotByModel(ctx, modelName)
		},
		func() ([]*model.Config, error) {
			return s.store.GetEnabledChannelsByModel(ctx, modelName)
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
	// URL 或上游协议配置可能已变化，丢弃运行时学习结果。
	s.protocolCapabilities.clear()
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

// GetReadTimeout 返回 HTTP ReadTimeout：读取请求头与请求体的整段上限。
// 它决定慢速上传何时被传输层切断，与请求体大小上限是两件事。
func (s *Server) GetReadTimeout() time.Duration {
	if s == nil || s.httpReadTimeout <= 0 {
		return config.DefaultHTTPReadTimeout
	}
	return s.httpReadTimeout
}

// GetWriteTimeout 返回建议的 HTTP WriteTimeout
// 基于请求总超时动态计算，确保传输层不会早于业务层截断响应
func (s *Server) GetWriteTimeout() time.Duration {
	const minWriteTimeout = 120 * time.Second
	maxTimeout := s.nonStreamTimeout
	if s.streamTimeout > maxTimeout {
		maxTimeout = s.streamTimeout
	}
	for _, timeouts := range s.protocolTimeouts {
		if timeouts.NonStreamTimeout > maxTimeout {
			maxTimeout = timeouts.NonStreamTimeout
		}
	}
	if maxTimeout > minWriteTimeout {
		return maxTimeout
	}
	return minWriteTimeout
}

func (s *Server) resolveProtocolTimeouts(plan protocol.TransformPlan) protocolTimeoutConfig {
	timeouts := protocolTimeoutConfig{
		FirstByteTimeout: s.firstByteTimeout,
		StreamTimeout:    s.streamTimeout,
		NonStreamTimeout: s.nonStreamTimeout,
	}

	protocolKey := string(plan.UpstreamProtocol)
	if protocolKey == "" {
		return timeouts
	}

	override, ok := s.protocolTimeouts[util.NormalizeProtocol(protocolKey)]
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

	// Codex CLI 直连路由别名（chatgpt_base_url 兼容），对齐 CLIProxyAPI 的
	// codexDirect 路由组。GET 是 Responses WebSocket 升级；POST 是 SSE
	// fallback，两者都由 DetectRequestFamily 识别为 RequestFamilyResponses。
	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(s.authService.RequireAPIAuth())
	codexDirect.Use(captureClientRequestMetadata())
	{
		codexDirect.GET("/responses", s.HandleProxyRequest)
		codexDirect.POST("/responses", s.HandleProxyRequest)
	}

	// 健康检查（公开访问，无需认证，K8s liveness/readiness probe）
	r.GET("/health", s.HandleHealth)

	// 公开访问的API（首页仪表盘数据）
	// [SECURITY NOTE] /public/* 端点故意不做认证，用于首页展示。
	// 认证仪表盘使用 /dashboard/summary；该公开端点保留给未登录首页集成。
	public := r.Group("/public", ZstdMiddleware())
	{
		public.GET("/summary", s.HandlePublicSummary)
		public.GET("/protocols", s.HandleGetProtocols)
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
		admin.POST("/oauth/credentials/import", s.HandleImportOAuthCredentials)
		admin.POST("/oauth/credentials/import/stream", s.HandleImportOAuthCredentialsStream)
		admin.POST("/oauth/credentials/import/jobs", s.HandleStartOAuthCredentialImportJob)
		admin.GET("/oauth/credentials/import/jobs/:id", s.HandleOAuthCredentialImportJob)
		admin.GET("/oauth/credentials/cleanup/options", s.HandleOAuthCredentialCleanupOptions)
		admin.POST("/oauth/credentials/cleanup/jobs", s.HandleStartOAuthCredentialCleanupJob)
		admin.GET("/oauth/credentials/cleanup/jobs/:id/stream", s.HandleOAuthCredentialCleanupStream)
		admin.POST("/oauth/credentials/cleanup/jobs/:id/cancel", s.HandleCancelOAuthCredentialCleanupJob)
		admin.POST("/codex/oauth/start", s.HandleStartCodexOAuth)
		admin.GET("/codex/oauth/status", s.HandleCodexOAuthStatus)
		admin.POST("/codex/oauth/cancel", s.HandleCancelCodexOAuth)
		admin.POST("/codex/oauth/callback", s.HandleSubmitCodexOAuthCallback)
		admin.POST("/codex/personal-access-token", s.HandleCreateCodexPersonalAccessToken)
		admin.POST("/codex/credentials/import", s.HandleImportCodexCredential)
		admin.POST("/channels/:id/codex-credential/refresh", s.HandleRefreshCodexCredential)
		admin.PUT("/channels/:id/codex-quota-overdraft", s.HandleUpdateCodexQuotaOverdraft)
		admin.POST("/channels/:id/oauth-usage", s.HandleOAuthUsage)
		admin.POST("/channels/:id/codex-quota-reset", s.HandleResetCodexQuota)
		admin.POST("/channels/oauth-usage/batch/stream", s.HandleOAuthUsageBatchStream)
		admin.POST("/antigravity/oauth/start", s.HandleStartAntigravityOAuth)
		admin.GET("/antigravity/oauth/status", s.HandleAntigravityOAuthStatus)
		admin.POST("/antigravity/oauth/cancel", s.HandleCancelAntigravityOAuth)
		admin.POST("/antigravity/oauth/callback", s.HandleSubmitAntigravityOAuthCallback)
		admin.POST("/antigravity/credentials/import", s.HandleImportAntigravityCredential)
		admin.POST("/channels/:id/antigravity-credential/refresh", s.HandleRefreshAntigravityCredential)
		admin.POST("/xai/oauth/start", s.HandleStartXAIOAuth)
		admin.GET("/xai/oauth/status", s.HandleXAIOAuthStatus)
		admin.POST("/xai/oauth/cancel", s.HandleCancelXAIOAuth)
		admin.POST("/xai/oauth/callback", s.HandleSubmitXAIOAuthCallback)
		admin.POST("/xai/credentials/import/stream", s.HandleImportXAICredentialsStream)
		admin.POST("/xai/credentials/import/jobs", s.HandleStartXAICredentialImportJob)
		admin.POST("/anthropic/oauth/start", s.HandleStartAnthropicOAuth)
		admin.GET("/anthropic/oauth/status", s.HandleAnthropicOAuthStatus)
		admin.POST("/anthropic/oauth/cancel", s.HandleCancelAnthropicOAuth)
		admin.POST("/anthropic/oauth/callback", s.HandleSubmitAnthropicOAuthCode)
		admin.POST("/anthropic/oauth/cookie", s.HandleAnthropicCookieAuth)
		admin.POST("/channels/:id/anthropic-credential/refresh", s.HandleRefreshAnthropicCredential)
		admin.POST("/zai/oauth/start", s.HandleStartZAIOAuth)
		admin.GET("/zai/oauth/status", s.HandleZAIOAuthStatus)
		admin.POST("/zai/oauth/cancel", s.HandleCancelZAIOAuth)
		admin.POST("/zai/credentials/import", s.HandleImportZAICredential)
		admin.POST("/cursor/oauth/start", s.HandleStartCursorOAuth)
		admin.GET("/cursor/oauth/status", s.HandleCursorOAuthStatus)
		admin.POST("/cursor/oauth/cancel", s.HandleCancelCursorOAuth)
		admin.POST("/cursor/credentials/import", s.HandleImportCursorCredential)
		admin.POST("/channels/check-duplicate", s.HandleCheckDuplicateChannel)
		admin.POST("/channels/batch-priority", s.HandleBatchUpdatePriority) // 批量更新渠道优先级
		admin.POST("/channels/batch-enabled", s.HandleBatchSetEnabled)      // 批量启用/禁用渠道
		admin.POST("/channels/batch-clear-cooldowns", s.HandleBatchClearCooldowns)
		admin.POST("/channels/batch-advanced", s.HandleBatchPatchChannels)
		admin.POST("/channels/batch-delete", s.HandleBatchDeleteChannels) // 批量删除渠道
		admin.POST("/channels/cooldown-detection/test", s.HandleCooldownDetectionTest)
		admin.GET("/channels/:id", s.HandleChannelByID)
		admin.PUT("/channels/:id", s.HandleChannelByID)
		admin.DELETE("/channels/:id", s.HandleChannelByID)
		admin.GET("/channels/:id/editor", s.HandleChannelEditor)
		admin.GET("/channels/:id/keys", s.HandleChannelKeys)
		admin.GET("/channels/:id/model-stats", s.HandleChannelModelStats)
		admin.GET("/channels/:id/url-stats", s.HandleChannelURLStats)
		admin.POST("/channels/:id/url-disable", s.HandleURLDisable)
		admin.POST("/channels/:id/url-enable", s.HandleURLEnable)
		admin.POST("/channels/:id/key-disable", s.HandleAPIKeyDisable)
		admin.POST("/channels/:id/key-enable", s.HandleAPIKeyEnable)
		admin.POST("/channels/models/fetch", s.HandleFetchModelsPreview) // 临时渠道配置获取模型列表
		admin.POST("/channels/billing/fetch", s.HandleFetchSub2APIBilling)
		admin.POST("/channels/websocket-probe", s.HandleChannelWebsocketProbe)
		admin.POST("/channels/models/refresh-batch", s.HandleBatchRefreshModels)
		admin.GET("/channels/:id/models/fetch", s.HandleFetchModels) // 获取渠道可用模型列表(新增)
		admin.POST("/channels/:id/models", s.HandleAddModels)        // 添加渠道模型
		admin.DELETE("/channels/:id/models", s.HandleDeleteModels)   // 删除渠道模型
		admin.POST("/channels/:id/test", s.HandleChannelTest)
		admin.POST("/channels/:id/test-url", s.HandleChannelURLTest)
		admin.POST("/channels/:id/chat", s.HandleChannelChat)
		admin.POST("/channels/:id/images/generations", s.HandleChannelImageGeneration)
		admin.POST("/channels/:id/cooldown", s.HandleSetChannelCooldown)
		admin.POST("/channels/:id/keys/:keyIndex/cooldown", s.HandleSetKeyCooldown)
		admin.DELETE("/channels/:id/keys/:keyIndex", s.HandleDeleteAPIKey)

		// 统计分析
		admin.GET("/logs", s.HandleErrors)
		admin.GET("/logs/bootstrap", s.HandleLogsBootstrap)
		admin.POST("/debug-logs/merged-response", s.HandleMergeDebugResponse)
		admin.GET("/debug-logs/:log_id", s.HandleGetDebugLog)
		admin.GET("/active-requests", s.HandleActiveRequests) // 进行中请求（内存状态）
		admin.GET("/runtime-metrics", s.HandleRuntimeMetrics)
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
	s.recordURLRequestFromLog(entry)
}

func (s *Server) recordURLRequestFromLog(entry *model.LogEntry) {
	if s == nil || s.urlSelector == nil || entry == nil {
		return
	}
	s.urlSelector.RecordRequestResult(entry.ChannelID, entry.BaseURL, entry.StatusCode)
}

// getAllEnabledModels 获取所有启用渠道的去重模型列表。
func (s *Server) getAllEnabledModels(ctx context.Context) ([]string, error) {
	channels, err := s.GetEnabledChannelsByModel(ctx, "*")
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
	if s.codexOAuth != nil {
		s.codexOAuth.close()
	}
	if s.antigravityOAuth != nil {
		s.antigravityOAuth.close()
	}
	if s.xaiOAuth != nil {
		s.xaiOAuth.close()
	}
	if s.anthropicOAuth != nil {
		s.anthropicOAuth.close()
	}
	if s.zaiOAuth != nil {
		s.zaiOAuth.close()
	}
	if s.cursorOAuth != nil {
		s.cursorOAuth.close()
	}
	if s.responsesExecutionSessions != nil {
		s.responsesExecutionSessions.close()
	}

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

	var oauthCredentialImportShutdownErr error
	if manager := s.currentOAuthCredentialImportJobs(); manager != nil {
		oauthCredentialImportShutdownErr = manager.Close(ctx)
	}
	var oauthCredentialCleanupShutdownErr error
	if manager := s.currentOAuthCredentialCleanupJobs(); manager != nil {
		oauthCredentialCleanupShutdownErr = manager.Close(ctx)
	}
	var oauthCredentialRefreshShutdownErr error
	if s.oauthCredentialRefreshes != nil {
		oauthCredentialRefreshShutdownErr = s.oauthCredentialRefreshes.close(ctx)
	}

	// 使用channel等待所有goroutine完成
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// 等待完成或超时
	var err error
	if oauthCredentialImportShutdownErr != nil {
		err = oauthCredentialImportShutdownErr
	}
	if oauthCredentialCleanupShutdownErr != nil && err == nil {
		err = oauthCredentialCleanupShutdownErr
	}
	if oauthCredentialRefreshShutdownErr != nil && err == nil {
		err = oauthCredentialRefreshShutdownErr
	}
	select {
	case <-done:
		log.Print("[INFO] Server优雅关闭完成")
	case <-ctx.Done():
		log.Print("[WARN]  Server关闭超时，部分后台任务可能未完成")
		if err == nil {
			err = ctx.Err()
		}
	}

	// 停止连接池老化计时器并关闭全局及渠道代理 Transport 的空闲连接。
	closeUpstreamHTTPClient(s.client)
	closeUpstreamHTTPClient(s.antigravityClient)
	closeUpstreamHTTPClient(s.xaiSSOClient)
	s.antigravityTransportMu.Lock()
	antigravityTransports := s.antigravityTransports
	s.antigravityTransportMu.Unlock()
	if antigravityTransports != nil {
		antigravityTransports.closeAll()
	}
	s.proxyTransports.Range(func(_, v any) bool {
		closeUpstreamHTTPClient(v.(*http.Client))
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
