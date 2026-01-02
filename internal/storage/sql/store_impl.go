package sql

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
)

// syncType 定义同步类型（位标记，支持组合）
// 包内私有：仅在 sql 包内使用，无需导出 (YAGNI)
type syncType uint32

const (
	syncChannels   syncType = 1 << iota // 同步渠道配置和 API Keys
	syncAuthTokens                      // 同步认证令牌

	syncAll = syncChannels | syncAuthTokens // 全量同步
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
// 支持 SQLite 和 MySQL（时间/布尔值存储格式完全一致，SQL语法按驱动分支）
type SQLStore struct {
	db         *sql.DB
	driverName string // "sqlite" 或 "mysql"

	// 异步Redis同步机制（性能优化: 避免同步等待）
	syncCh           chan struct{} // 同步触发信号（缓冲1，去重合并多个请求）
	pendingSyncTypes atomic.Uint32 // 待同步类型（位标记，支持合并）
	done             chan struct{} // 优雅关闭信号

	redisSync RedisSync // Redis同步接口（依赖注入，支持测试和扩展）

	// 优雅关闭：等待后台worker
	wg sync.WaitGroup

	// [FIX] 2025-12：保证 StartRedisSync 幂等性，防止多次调用启动多个 worker
	startOnce sync.Once
	// [FIX] 2025-12：保证 Close 幂等性，防止重复关闭 channel 导致 panic
	closeOnce sync.Once
}

// GetHealthTimeline 执行健康时间线查询（用于 stats API）
func (s *SQLStore) GetHealthTimeline(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// NewSQLStore 创建通用SQL存储实例
// db: 数据库连接（由调用方初始化）
// driverName: "sqlite" 或 "mysql"
// redisSync: Redis同步器（可选，测试时可传nil）
func NewSQLStore(db *sql.DB, driverName string, redisSync RedisSync) *SQLStore {
	return &SQLStore{
		db:         db,
		driverName: driverName,
		syncCh:     make(chan struct{}, 1),
		done:       make(chan struct{}),
		redisSync:  redisSync,
	}
}

// StartRedisSync 显式启动 Redis 同步 worker
// 必须在迁移完成且恢复逻辑执行后调用，避免空数据覆盖 Redis 备份
// [FIX] 2025-12：使用 sync.Once 保证幂等性，防止多次调用启动多个 worker
func (s *SQLStore) StartRedisSync() {
	if s.redisSync == nil || !s.redisSync.IsEnabled() {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.redisSyncWorker()
		// 启动时触发全量同步，确保所有存量数据备份到 Redis
		s.triggerAsyncSync(syncAll)
	})
}

// IsRedisEnabled 检查Redis是否启用
func (s *SQLStore) IsRedisEnabled() bool {
	return s.redisSync != nil && s.redisSync.IsEnabled()
}

// IsSQLite 检查是否为SQLite驱动
func (s *SQLStore) IsSQLite() bool {
	return s.driverName == "sqlite"
}

// Close 关闭存储（优雅关闭）
// Ping 检查数据库连接是否活跃（用于健康检查，<1ms）
func (s *SQLStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLStore) Close() error {
	var err error
	s.closeOnce.Do(func() {
		// 1. 通知后台worker退出
		close(s.done)

		// 2. 等待worker完成
		s.wg.Wait()

		// 3. 关闭数据库连接
		if s.db != nil {
			err = s.db.Close()
		}
	})
	return err
}

// CleanupLogsBefore 清理指定时间之前的日志
func (s *SQLStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	// time 字段是 BIGINT 毫秒时间戳
	// 分批删除避免长时间锁表（P2优化）
	cutoffMs := cutoff.UnixMilli()
	const batchSize = 5000

	for {
		var query string
		if s.IsSQLite() {
			// SQLite: 使用子查询实现分批删除（默认不支持 DELETE LIMIT）
			query = `DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE time < ? LIMIT ?)`
		} else {
			// MySQL: 直接使用 LIMIT
			query = `DELETE FROM logs WHERE time < ? LIMIT ?`
		}

		result, err := s.db.ExecContext(ctx, query, cutoffMs, batchSize)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected < batchSize {
			break // 已删完
		}
	}
	return nil
}
