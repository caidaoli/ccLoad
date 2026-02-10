package storage

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

// ErrSettingNotFound 系统设置未找到错误（重导出自 model 包以保持兼容性）
var ErrSettingNotFound = model.ErrSettingNotFound

// Store 数据持久化接口
// [REFACTOR] 2025-12：合并子接口，所有方法平铺
// 理由：8个子接口无任何地方被独立使用，所有消费者都依赖完整 Store
type Store interface {
	// === Channel Management ===
	ListConfigs(ctx context.Context) ([]*model.Config, error)
	GetConfig(ctx context.Context, id int64) (*model.Config, error)
	CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error)
	UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error)
	DeleteConfig(ctx context.Context, id int64) error
	GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error)
	GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error)
	BatchUpdatePriority(ctx context.Context, updates []struct {
		ID       int64
		Priority int
	}) (int64, error)

	// === API Key Management ===
	GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error)
	GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error)
	GetAllAPIKeys(ctx context.Context) (map[int64][]*model.APIKey, error)
	CreateAPIKeysBatch(ctx context.Context, keys []*model.APIKey) error
	UpdateAPIKeysStrategy(ctx context.Context, channelID int64, strategy string) error
	DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error
	CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error
	DeleteAllAPIKeys(ctx context.Context, channelID int64) error

	// === Cooldown Management ===
	// Channel-level cooldown
	GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error)
	BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error)
	ResetChannelCooldown(ctx context.Context, channelID int64) error
	SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error
	// Key-level cooldown
	GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error)
	BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error)
	ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error
	SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error

	// === Log Management ===
	AddLog(ctx context.Context, e *model.LogEntry) error
	BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error)
	ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error)
	CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error)
	CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error)
	CleanupLogsBefore(ctx context.Context, cutoff time.Time) error

	// === Metrics & Statistics ===
	AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, filter *model.LogFilter) ([]model.MetricPoint, error)
	GetDistinctModels(ctx context.Context, since, until time.Time, channelType string) ([]string, error)
	GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) ([]model.StatsEntry, error)
	GetStatsLite(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) // 轻量版：跳过RPM计算和渠道名填充
	GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error)
	GetChannelSuccessRates(ctx context.Context, since time.Time) (map[int64]model.ChannelHealthStats, error)
	GetHealthTimeline(ctx context.Context, params model.HealthTimelineParams) ([]model.HealthTimelineRow, error)
	GetTodayChannelCosts(ctx context.Context, todayStart time.Time) (map[int64]float64, error) // 获取今日各渠道成本（启动时加载）

	// === Auth Token Management ===
	CreateAuthToken(ctx context.Context, token *model.AuthToken) error
	GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error)
	GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error)
	ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error)
	ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error)
	UpdateAuthToken(ctx context.Context, token *model.AuthToken) error
	DeleteAuthToken(ctx context.Context, id int64) error
	UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error
	UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, cacheReadTokens int64, cacheCreationTokens int64, costUSD float64) error
	GetAuthTokenStatsInRange(ctx context.Context, startTime, endTime time.Time) (map[int64]*model.AuthTokenRangeStats, error)
	FillAuthTokenRPMStats(ctx context.Context, stats map[int64]*model.AuthTokenRangeStats, startTime, endTime time.Time, isToday bool) error

	// === System Settings ===
	GetSetting(ctx context.Context, key string) (*model.SystemSetting, error)
	ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error)
	UpdateSetting(ctx context.Context, key, value string) error
	BatchUpdateSettings(ctx context.Context, updates map[string]string) error

	// === Admin Session Management ===
	CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error
	GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error)
	DeleteAdminSession(ctx context.Context, token string) error
	CleanExpiredSessions(ctx context.Context) error
	LoadAllSessions(ctx context.Context) (map[string]time.Time, error)

	// === Batch Operations ===
	ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error)

	// === Infrastructure ===
	Ping(ctx context.Context) error
	Close() error
}
