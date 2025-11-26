package storage

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

// Store 数据持久化接口
// 设计原则：依赖倒置原则（DIP），业务逻辑依赖接口而非具体实现
type Store interface {
	// Config management - 渠道配置管理
	ListConfigs(ctx context.Context) ([]*model.Config, error)
	GetConfig(ctx context.Context, id int64) (*model.Config, error)
	CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error)
	UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error)
	DeleteConfig(ctx context.Context, id int64) error
	ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error)

	// 简化查询：直接从数据库按条件查询（利用索引）
	GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error)
	GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error)

	// API Keys management - API Key管理
	GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error)
	GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error)
	CreateAPIKey(ctx context.Context, key *model.APIKey) error
	UpdateAPIKey(ctx context.Context, key *model.APIKey) error
	DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error
	CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error
	DeleteAllAPIKeys(ctx context.Context, channelID int64) error // 删除渠道的所有Key

	// Cooldown (channel-level) - 渠道级冷却管理
	// 简化接口：冷却数据直接在channels表中
	GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error)
	BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error)
	ResetChannelCooldown(ctx context.Context, channelID int64) error
	SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error

	// Cooldown (key-level) - Key级冷却管理
	// 简化接口：冷却数据直接在api_keys表中
	GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error)
	BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error)
	ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error
	SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error

	// Logs - 日志管理
	AddLog(ctx context.Context, e *model.LogEntry) error
	ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error)

	CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error)

	// Metrics - 指标管理
	Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]model.MetricPoint, error)

	// Stats - 统计功能
	GetStats(ctx context.Context, since time.Time, filter *model.LogFilter) ([]model.StatsEntry, error)

	// Auth Tokens - API访问令牌管理
	// 令牌用于代理API (/v1/*) 的认证授权
	CreateAuthToken(ctx context.Context, token *model.AuthToken) error
	GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error)
	GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error)
	ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error)
	ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error)
	UpdateAuthToken(ctx context.Context, token *model.AuthToken) error
	DeleteAuthToken(ctx context.Context, id int64) error
	UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error
	UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, costUSD float64) error

	// Maintenance - 维护功能
	CleanupLogsBefore(ctx context.Context, cutoff time.Time) error
}
