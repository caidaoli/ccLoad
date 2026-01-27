package sql

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// SQLStore 通用SQL存储实现
// 支持 SQLite 和 MySQL（时间/布尔值存储格式完全一致，SQL语法按驱动分支）
type SQLStore struct {
	db         *sql.DB
	driverName string // "sqlite" 或 "mysql"

	// [FIX] 2025-12：保证 Close 幂等性，防止重复关闭导致 panic
	closeOnce sync.Once
}

// GetHealthTimeline 执行健康时间线查询（用于 stats API）
func (s *SQLStore) GetHealthTimeline(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// NewSQLStore 创建通用SQL存储实例
// db: 数据库连接（由调用方初始化）
// driverName: "sqlite" 或 "mysql"
func NewSQLStore(db *sql.DB, driverName string) *SQLStore {
	return &SQLStore{
		db:         db,
		driverName: driverName,
	}
}

// IsSQLite 检查是否为SQLite驱动
func (s *SQLStore) IsSQLite() bool {
	return s.driverName == "sqlite"
}

// Ping 检查数据库连接是否活跃（用于健康检查）
func (s *SQLStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close 关闭存储（优雅关闭）
func (s *SQLStore) Close() error {
	var err error
	s.closeOnce.Do(func() {
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

// ============================================================================
// 底层数据库访问方法（供 SyncManager 等组件使用）
// ============================================================================

// QueryContext 执行查询语句
func (s *SQLStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// QueryRowContext 执行查询单行
func (s *SQLStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// ExecContext 执行非查询语句
func (s *SQLStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

// BeginTx 开启事务
func (s *SQLStore) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, opts)
}
