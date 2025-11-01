package service

import (
	"net/http"

	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

// AdminService 管理 API 服务
// 阶段 5：定义核心依赖，保留委托模式
//
// 职责：处理所有管理 API 相关的业务逻辑
// - 渠道 CRUD（创建、读取、更新、删除）
// - 统计分析（日志、指标、趋势）
// - CSV 导入/导出
// - 冷却管理
// - 测试工具
//
// 遵循 SRP 原则：仅负责管理 API，不涉及代理、认证、日志系统
//
// 设计说明：AdminService 的方法主要是简单的 CRUD 和查询操作，
// 已经在 Server 的各个文件中组织得很好（admin_*.go）。
// 考虑到性价比，暂时保留委托模式，避免过度工程化。
type AdminService struct {
	// 核心依赖
	store        storage.Store
	channelCache *storage.ChannelCache
	client       *http.Client

	// 阶段 5：暂时保留委托（admin 方法已经实现得很好）
	// 这些方法主要是 HTTP 处理逻辑，不涉及复杂的状态管理
	// 未来如果需要独立测试或重用，可以逐步迁移
	delegate adminDelegate
}

// adminDelegate 管理服务委托接口
// 定义 AdminService 需要的方法，避免直接依赖 Server 类型
type adminDelegate interface {
	// 渠道管理（方法名大写，才能跨包实现）
	HandleChannels(c *gin.Context)
	HandleChannelByID(c *gin.Context)
	HandleChannelKeys(c *gin.Context)

	// CSV 导入/导出
	HandleExportChannelsCSV(c *gin.Context)
	HandleImportChannelsCSV(c *gin.Context)

	// 统计分析
	HandleErrors(c *gin.Context)
	HandleMetrics(c *gin.Context)
	HandleStats(c *gin.Context)
	HandlePublicSummary(c *gin.Context)

	// 冷却管理
	HandleSetChannelCooldown(c *gin.Context)
	HandleSetKeyCooldown(c *gin.Context)
	HandleCooldownStats(c *gin.Context)

	// 缓存统计
	HandleCacheStats(c *gin.Context)

	// 渠道类型
	HandleGetChannelTypes(c *gin.Context)

	// 测试工具
	HandleChannelTest(c *gin.Context)
}

// NewAdminService 创建管理服务实例
// 阶段 5：接受核心依赖，保留委托模式
func NewAdminService(
	store storage.Store,
	channelCache *storage.ChannelCache,
	client *http.Client,
	delegate adminDelegate,
) *AdminService {
	return &AdminService{
		store:        store,
		channelCache: channelCache,
		client:       client,
		delegate:     delegate,
	}
}

// ============================================================================
// 渠道管理（阶段 1：委托给 Server）
// ============================================================================

// HandleChannels 处理渠道列表/创建请求
// 阶段 1：委托给 Server.HandleChannels
// TODO: 阶段 2 迁移渠道 CRUD 逻辑到这里
func (s *AdminService) HandleChannels(c *gin.Context) {
	s.delegate.HandleChannels(c)
}

// HandleChannelByID 处理单个渠道的读取/更新/删除请求
// 阶段 1：委托给 Server.HandleChannelByID
// TODO: 阶段 2 迁移渠道 CRUD 逻辑到这里
func (s *AdminService) HandleChannelByID(c *gin.Context) {
	s.delegate.HandleChannelByID(c)
}

// HandleChannelKeys 处理渠道 API Keys 查询请求
// 阶段 1：委托给 Server.HandleChannelKeys
// TODO: 阶段 2 迁移 API Keys 查询逻辑到这里
func (s *AdminService) HandleChannelKeys(c *gin.Context) {
	s.delegate.HandleChannelKeys(c)
}

// ============================================================================
// CSV 导入/导出（阶段 1：委托给 Server）
// ============================================================================

// HandleExportChannelsCSV 处理渠道导出为 CSV 请求
// 阶段 1：委托给 Server.HandleExportChannelsCSV
// TODO: 阶段 2 迁移 CSV 导出逻辑到这里
func (s *AdminService) HandleExportChannelsCSV(c *gin.Context) {
	s.delegate.HandleExportChannelsCSV(c)
}

// HandleImportChannelsCSV 处理从 CSV 导入渠道请求
// 阶段 1：委托给 Server.HandleImportChannelsCSV
// TODO: 阶段 2 迁移 CSV 导入逻辑到这里
func (s *AdminService) HandleImportChannelsCSV(c *gin.Context) {
	s.delegate.HandleImportChannelsCSV(c)
}

// ============================================================================
// 统计分析（阶段 1：委托给 Server）
// ============================================================================

// HandleErrors 处理错误日志查询请求
// 阶段 1：委托给 Server.HandleErrors
// TODO: 阶段 2 迁移日志查询逻辑到这里
func (s *AdminService) HandleErrors(c *gin.Context) {
	s.delegate.HandleErrors(c)
}

// HandleMetrics 处理聚合指标查询请求
// 阶段 1：委托给 Server.HandleMetrics
// TODO: 阶段 2 迁移指标查询逻辑到这里
func (s *AdminService) HandleMetrics(c *gin.Context) {
	s.delegate.HandleMetrics(c)
}

// HandleStats 处理统计分析查询请求
// 阶段 1：委托给 Server.HandleStats
// TODO: 阶段 2 迁移统计查询逻辑到这里
func (s *AdminService) HandleStats(c *gin.Context) {
	s.delegate.HandleStats(c)
}

// HandlePublicSummary 处理公开统计摘要查询请求
// 阶段 1：委托给 Server.HandlePublicSummary
// TODO: 阶段 2 迁移公开统计逻辑到这里
func (s *AdminService) HandlePublicSummary(c *gin.Context) {
	s.delegate.HandlePublicSummary(c)
}

// ============================================================================
// 冷却管理（阶段 1：委托给 Server）
// ============================================================================

// HandleSetChannelCooldown 处理设置渠道冷却请求
// 阶段 1：委托给 Server.HandleSetChannelCooldown
// TODO: 阶段 2 迁移冷却管理逻辑到这里
func (s *AdminService) HandleSetChannelCooldown(c *gin.Context) {
	s.delegate.HandleSetChannelCooldown(c)
}

// HandleSetKeyCooldown 处理设置 Key 冷却请求
// 阶段 1：委托给 Server.HandleSetKeyCooldown
// TODO: 阶段 2 迁移冷却管理逻辑到这里
func (s *AdminService) HandleSetKeyCooldown(c *gin.Context) {
	s.delegate.HandleSetKeyCooldown(c)
}

// HandleCooldownStats 处理冷却统计查询请求
// 阶段 1：委托给 Server.HandleCooldownStats
// TODO: 阶段 2 迁移冷却统计逻辑到这里
func (s *AdminService) HandleCooldownStats(c *gin.Context) {
	s.delegate.HandleCooldownStats(c)
}

// ============================================================================
// 缓存统计（阶段 1：委托给 Server）
// ============================================================================

// HandleCacheStats 处理缓存统计查询请求
// 阶段 1：委托给 Server.HandleCacheStats
// TODO: 阶段 2 迁移缓存统计逻辑到这里
func (s *AdminService) HandleCacheStats(c *gin.Context) {
	s.delegate.HandleCacheStats(c)
}

// ============================================================================
// 渠道类型（阶段 1：委托给 Server）
// ============================================================================

// HandleGetChannelTypes 处理渠道类型查询请求
// 阶段 1：委托给 Server.HandleGetChannelTypes
// TODO: 阶段 2 迁移渠道类型配置到这里
func (s *AdminService) HandleGetChannelTypes(c *gin.Context) {
	s.delegate.HandleGetChannelTypes(c)
}

// ============================================================================
// 测试工具（阶段 1：委托给 Server）
// ============================================================================

// HandleChannelTest 处理渠道测试请求
// 阶段 1：委托给 Server.HandleChannelTest
// TODO: 阶段 2 迁移渠道测试逻辑到这里
func (s *AdminService) HandleChannelTest(c *gin.Context) {
	s.delegate.HandleChannelTest(c)
}
