package sql

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// RedisSync Redis同步接口
// 支持渠道配置和Auth Tokens的双向同步
type RedisSync interface {
	IsEnabled() bool
	LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error)
	SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error
	// Auth Tokens同步
	SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error
	LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error)
}

// SQLStore 通用SQL存储实现
// 支持 SQLite 和 MySQL（时间/布尔值存储格式完全一致，无需方言抽象）
type SQLStore struct {
	db *sql.DB

	// 异步Redis同步机制（性能优化: 避免同步等待）
	syncCh chan struct{} // 同步触发信号（无缓冲，去重合并多个请求）
	done   chan struct{} // 优雅关闭信号

	redisSync RedisSync // Redis同步接口（依赖注入，支持测试和扩展）

	// 优雅关闭：等待后台worker
	wg sync.WaitGroup
}

// NewSQLStore 创建通用SQL存储实例
// db: 数据库连接（由调用方初始化）
// redisSync: Redis同步器（可选，测试时可传nil）
func NewSQLStore(db *sql.DB, redisSync RedisSync) *SQLStore {
	s := &SQLStore{
		db:        db,
		syncCh:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		redisSync: redisSync,
	}

	// 启动Redis同步worker（仅在redisSync启用时）
	if redisSync != nil && redisSync.IsEnabled() {
		s.wg.Add(1)
		go s.redisSyncWorker()
	}

	return s
}

// IsRedisEnabled 检查Redis是否启用
func (s *SQLStore) IsRedisEnabled() bool {
	return s.redisSync != nil && s.redisSync.IsEnabled()
}

// Close 关闭存储（优雅关闭）
func (s *SQLStore) Close() error {
	// 1. 通知后台worker退出
	close(s.done)

	// 2. 等待worker完成
	s.wg.Wait()

	// 3. 关闭数据库连接
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// CleanupLogsBefore 清理指定时间之前的日志
func (s *SQLStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	query := "DELETE FROM logs WHERE timestamp < ?"
	_, err := s.db.ExecContext(ctx, query, timeToUnix(cutoff))
	return err
}
