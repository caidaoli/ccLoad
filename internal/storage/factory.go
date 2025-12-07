package storage

import (
	"context"
	"fmt"
	"os"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/mysql"
)

// DBType 数据库类型
type DBType string

const (
	DBTypeSQLite DBType = "sqlite"
	DBTypeMySQL  DBType = "mysql"
)

// RedisSync Redis同步接口（从sqlite包提取，避免循环依赖）
type RedisSync interface {
	IsEnabled() bool
	LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error)
	SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error
	SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error
	LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error)
}

// NewStore 根据环境变量创建存储实例
// CCLOAD_MYSQL 设置时使用MySQL，否则使用SQLite
func NewStore(redisSync RedisSync) (Store, DBType, error) {
	mysqlDSN := os.Getenv("CCLOAD_MYSQL")
	if mysqlDSN != "" {
		mysqlStore, err := createMySQLStore(mysqlDSN, redisSync)
		if err != nil {
			return nil, DBTypeMySQL, fmt.Errorf("MySQL 初始化失败: %w", err)
		}
		return mysqlStore, DBTypeMySQL, nil
	}

	// SQLite 模式：由调用方创建，这里只返回类型标识
	return nil, DBTypeSQLite, nil
}

// GetDBType 获取当前配置的数据库类型
func GetDBType() DBType {
	if os.Getenv("CCLOAD_MYSQL") != "" {
		return DBTypeMySQL
	}
	return DBTypeSQLite
}

// createMySQLStore 创建 MySQL 存储实例（延迟导入）
func createMySQLStore(dsn string, redisSync RedisSync) (Store, error) {
	// 动态导入 mysql 包，避免编译时依赖
	// 这里使用类型断言将 redisSync 转换为 mysql 包需要的接口
	mysqlStore, err := mysql.NewMySQLStore(dsn, redisSyncAdapter{redisSync})
	if err != nil {
		return nil, err
	}
	return mysqlStore, nil
}

// redisSyncAdapter 适配器，将 storage.RedisSync 转换为 mysql.RedisSync
type redisSyncAdapter struct {
	RedisSync
}

func (a redisSyncAdapter) IsEnabled() bool {
	if a.RedisSync == nil {
		return false
	}
	return a.RedisSync.IsEnabled()
}

func (a redisSyncAdapter) LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error) {
	return a.RedisSync.LoadChannelsWithKeysFromRedis(ctx)
}

func (a redisSyncAdapter) SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error {
	return a.RedisSync.SyncAllChannelsWithKeys(ctx, channels)
}

func (a redisSyncAdapter) SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error {
	return a.RedisSync.SyncAllAuthTokens(ctx, tokens)
}

func (a redisSyncAdapter) LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error) {
	return a.RedisSync.LoadAuthTokensFromRedis(ctx)
}
