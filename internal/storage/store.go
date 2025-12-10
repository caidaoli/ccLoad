package storage

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

// ErrSettingNotFound 系统设置未找到错误（重导出自 model 包以保持兼容性）
var ErrSettingNotFound = model.ErrSettingNotFound

// ============================================================================
// 子接口定义（ISP原则：接口隔离）
// ============================================================================

// ChannelStore 渠道配置管理接口
type ChannelStore interface {
	ListConfigs(ctx context.Context) ([]*model.Config, error)
	GetConfig(ctx context.Context, id int64) (*model.Config, error)
	CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error)
	UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error)
	DeleteConfig(ctx context.Context, id int64) error
	ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error)
	GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error)
	GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error)
}

// APIKeyStore API Key管理接口
type APIKeyStore interface {
	GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error)
	GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error)
	GetAllAPIKeys(ctx context.Context) (map[int64][]*model.APIKey, error)
	CreateAPIKey(ctx context.Context, key *model.APIKey) error
	UpdateAPIKey(ctx context.Context, key *model.APIKey) error
	DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error
	CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error
	DeleteAllAPIKeys(ctx context.Context, channelID int64) error
}

// CooldownStore 冷却管理接口
type CooldownStore interface {
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
}

// LogStore 日志管理接口
type LogStore interface {
	AddLog(ctx context.Context, e *model.LogEntry) error
	BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error)
	ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error)
	CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error)
	CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error)
	CleanupLogsBefore(ctx context.Context, cutoff time.Time) error
}

// MetricsStore 指标统计接口
type MetricsStore interface {
	Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]model.MetricPoint, error)
	AggregateRange(ctx context.Context, since, until time.Time, bucket time.Duration) ([]model.MetricPoint, error)
	AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, channelType string, modelFilter string, authTokenID int64) ([]model.MetricPoint, error)
	GetDistinctModels(ctx context.Context, since, until time.Time) ([]string, error)
	GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error)
}

// AuthTokenStore API访问令牌管理接口
type AuthTokenStore interface {
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
}

// SettingsStore 系统配置管理接口
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (*model.SystemSetting, error)
	ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error)
	UpdateSetting(ctx context.Context, key, value string) error
	BatchUpdateSettings(ctx context.Context, updates map[string]string) error
}

// SessionStore 管理员会话管理接口
type SessionStore interface {
	CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error
	GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error)
	DeleteAdminSession(ctx context.Context, token string) error
	CleanExpiredSessions(ctx context.Context) error
	LoadAllSessions(ctx context.Context) (map[string]time.Time, error)
}

// ============================================================================
// 组合接口（向后兼容）
// ============================================================================

// Store 数据持久化接口（组合所有子接口）
// 设计原则：依赖倒置原则（DIP），业务逻辑依赖接口而非具体实现
type Store interface {
	ChannelStore
	APIKeyStore
	CooldownStore
	LogStore
	MetricsStore
	AuthTokenStore
	SettingsStore
	SessionStore

	// Batch Import - 批量导入（CSV导入优化）
	ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error)

	// Redis Restore - Redis数据恢复
	LoadChannelsFromRedis(ctx context.Context) error
	CheckChannelsEmpty(ctx context.Context) (bool, error)

	// Redis Status - Redis状态查询
	IsRedisEnabled() bool

	// Close - 关闭数据库连接并释放资源
	Close() error
}
