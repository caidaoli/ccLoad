package service

import (
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ProxyConfig 代理服务配置
// 遵循 KISS 原则：仅包含必要的配置项
type ProxyConfig struct {
	MaxKeyRetries    int           // 单渠道最大 Key 重试次数（默认 3）
	FirstByteTimeout time.Duration // 上游首字节超时（可选）
	MaxConcurrency   int           // 最大并发数（默认 1000）
}

// DefaultProxyConfig 返回默认代理配置
func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{
		MaxKeyRetries:    3,
		FirstByteTimeout: 0,    // 不设置超时
		MaxConcurrency:   1000, // 默认最大并发
	}
}

// ProxyService 代理服务
// 阶段 2：直接管理依赖，不再委托给 Server
//
// 职责：处理所有代理相关的业务逻辑
// - 路由选择和候选渠道筛选
// - 多 Key 管理和故障切换
// - 上游请求转发和响应处理
//
// 遵循 SRP 原则：仅负责代理逻辑，不涉及认证、日志、管理 API
type ProxyService struct {
	store           storage.Store
	channelCache    *storage.ChannelCache
	cooldownManager *cooldown.Manager
	client          *http.Client
	config          ProxyConfig

	// 并发控制
	concurrencySem chan struct{} // 信号量：限制最大并发请求数
	maxConcurrency int

	// 日志服务（用于记录代理日志）
	logService *LogService

	// 阶段 7：Key选择器（依赖接口而非具体实现，遵循DIP原则）
	keySelector KeySelector

	// 阶段 2：暂时保留对 Server 的引用（用于复杂方法的委托）
	// TODO: 阶段 8 移除这个字段，完全独立
	serverDelegate serverProxyDelegate
}

// serverProxyDelegate 暂时保留的委托接口（仅用于 HandleProxyRequest）
// TODO: 阶段 2.4 移除此接口
type serverProxyDelegate interface {
	HandleProxyRequest(c *gin.Context)
}

// NewProxyService 创建代理服务实例
// 阶段 7：添加 KeySelector 接口依赖（遵循 DIP 原则）
func NewProxyService(
	store storage.Store,
	channelCache *storage.ChannelCache,
	cooldownManager *cooldown.Manager,
	client *http.Client,
	logService *LogService,
	keySelector KeySelector, // 阶段 7：新增参数（接口类型）
	config ProxyConfig,
	serverDelegate serverProxyDelegate, // TODO: 阶段 8 移除此参数
) *ProxyService {
	return &ProxyService{
		store:           store,
		channelCache:    channelCache,
		cooldownManager: cooldownManager,
		client:          client,
		config:          config,
		concurrencySem:  make(chan struct{}, config.MaxConcurrency),
		maxConcurrency:  config.MaxConcurrency,
		logService:      logService,
		keySelector:     keySelector,
		serverDelegate:  serverDelegate,
	}
}

// ============================================================================
// 核心代理方法（阶段 2.4：暂时仍然委托）
// ============================================================================

// HandleProxyRequest 处理代理请求（主入口）
// 阶段 2：暂时仍然委托给 Server.HandleProxyRequest
// TODO: 阶段 2.4 迁移完整实现到这里
func (s *ProxyService) HandleProxyRequest(c *gin.Context) {
	s.serverDelegate.HandleProxyRequest(c)
}

// ============================================================================
// 渠道选择方法（阶段 2：已迁移 ✅）
// ============================================================================

// SelectCandidates 选择支持指定模型的候选渠道
// 阶段 2：✅ 已迁移 - 直接使用缓存查询
func (s *ProxyService) SelectCandidates(ctx context.Context, modelName string) ([]*model.Config, error) {
	// 缓存优先查询（自动 60 秒 TTL 刷新）
	return s.channelCache.GetEnabledChannelsByModel(ctx, modelName)
}

// SelectCandidatesByType 根据渠道类型选择候选渠道
// 阶段 2：✅ 已迁移 - 直接使用缓存查询
func (s *ProxyService) SelectCandidatesByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	return s.channelCache.GetEnabledChannelsByType(ctx, channelType)
}

// GetAPIKeys 获取渠道的 API Keys
// 阶段 2：✅ 已迁移 - 直接使用缓存查询
func (s *ProxyService) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	return s.channelCache.GetAPIKeys(ctx, channelID)
}

// ============================================================================
// 缓存管理方法（阶段 2：已迁移 ✅）
// ============================================================================

// InvalidateCache 使缓存失效（在渠道配置更新时调用）
// 阶段 2：✅ 已迁移 - 直接操作缓存
func (s *ProxyService) InvalidateCache() {
	s.channelCache.InvalidateCache()
}

// InvalidateAPIKeysCache 使指定渠道的 API Keys 缓存失效
// 阶段 2：✅ 已迁移 - 直接操作缓存
func (s *ProxyService) InvalidateAPIKeysCache(channelID int64) {
	s.channelCache.InvalidateAPIKeysCache(channelID)
}

// InvalidateAllAPIKeysCache 使所有 API Keys 缓存失效
// 阶段 2：✅ 已迁移 - 直接操作缓存
func (s *ProxyService) InvalidateAllAPIKeysCache() {
	s.channelCache.InvalidateAllAPIKeysCache()
}
