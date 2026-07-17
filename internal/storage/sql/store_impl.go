package sql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// SQLStore 通用SQL存储实现
// 支持 SQLite / MySQL / PostgreSQL（时间/布尔值存储格式完全一致，SQL语法按驱动分支）
type SQLStore struct {
	db         *sql.DB
	driverName string // "sqlite" | "mysql" | "postgres"

	// [FIX] 2025-12：保证 Close 幂等性，防止重复关闭导致 panic
	closeOnce sync.Once

	// 删除渠道后，异步日志队列里可能还有旧渠道日志等待刷盘。
	// tombstone 让迟到日志在存储层被丢弃，避免删除后又被插回。
	deletedChannels sync.Map // map[int64]struct{}
}

func (s *SQLStore) markChannelDeleted(id int64) {
	if id > 0 {
		s.deletedChannels.Store(id, struct{}{})
	}
}

func (s *SQLStore) unmarkChannelDeleted(id int64) {
	if id > 0 {
		s.deletedChannels.Delete(id)
	}
}

func (s *SQLStore) isChannelDeleted(id int64) bool {
	if id <= 0 {
		return false
	}
	_, ok := s.deletedChannels.Load(id)
	return ok
}

// GetHealthTimeline 查询健康时间线数据
// SQL 构建封装在存储层内部，业务层只传结构化参数
func (s *SQLStore) GetHealthTimeline(ctx context.Context, params model.HealthTimelineParams) ([]model.HealthTimelineRow, error) {
	baseQuery := `
		SELECT
			FLOOR(time / ?) * ? AS bucket_ts,
			channel_id,
			COALESCE(model, '') AS model,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN (status_code < 200 OR status_code >= 300) AND status_code != 499 THEN 1 ELSE 0 END) AS error,
			SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END) AS rate_limited,
			COALESCE(AVG(CASE WHEN first_byte_time > 0 AND status_code >= 200 AND status_code < 300 THEN first_byte_time ELSE NULL END), 0) AS avg_first_byte_time,
			COALESCE(AVG(CASE WHEN duration > 0 AND status_code >= 200 AND status_code < 300 THEN duration ELSE NULL END), 0) AS avg_duration,
			SUM(COALESCE(input_tokens, 0)) AS input_tokens,
			SUM(COALESCE(output_tokens, 0)) AS output_tokens,
			SUM(COALESCE(cache_read_input_tokens, 0)) AS cache_read_tokens,
			SUM(COALESCE(cache_creation_input_tokens, 0)) AS cache_creation_tokens,
			SUM(COALESCE(cost, 0.0)) AS total_cost,
			SUM(COALESCE(cost, 0.0) * COALESCE(cost_multiplier, 1)) AS effective_cost
		FROM logs
	`

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", params.SinceMs).
		Where("time <= ?", params.UntilMs).
		Where("status_code != 499").
		Where("channel_id > 0")

	_, isEmpty, err := s.applyChannelFilter(ctx, qb, params.Filter)
	if err != nil {
		return nil, fmt.Errorf("resolve health timeline channel filter: %w", err)
	}
	if isEmpty {
		return []model.HealthTimelineRow{}, nil
	}
	qb.ApplyFilter(params.Filter)

	query, args := qb.BuildWithSuffix("GROUP BY bucket_ts, channel_id, model ORDER BY bucket_ts ASC")
	args = append([]any{params.BucketMs, params.BucketMs}, args...)

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query health timeline: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []model.HealthTimelineRow
	for rows.Next() {
		var r model.HealthTimelineRow
		if err := rows.Scan(&r.BucketTs, &r.ChannelID, &r.Model, &r.Success, &r.ErrorCount, &r.RateLimitedCount,
			&r.AvgFirstByteTime, &r.AvgDuration, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalCost, &r.EffectiveCost); err != nil {
			continue
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate health timeline rows: %w", err)
	}
	return result, nil
}

// NewSQLStore 创建通用SQL存储实例
// db: 数据库连接（由调用方初始化）
// driverName: "sqlite" | "mysql" | "postgres"
func NewSQLStore(db *sql.DB, driverName string) *SQLStore {
	return &SQLStore{
		db:         db,
		driverName: driverName,
	}
}

// DriverName 返回底层驱动名
func (s *SQLStore) DriverName() string {
	return s.driverName
}

// IsSQLite 检查是否为SQLite驱动
func (s *SQLStore) IsSQLite() bool {
	return s.driverName == "sqlite"
}

// IsMySQL 检查是否为MySQL驱动
func (s *SQLStore) IsMySQL() bool {
	return s.driverName == "mysql"
}

// IsPostgres 检查是否为PostgreSQL驱动
func (s *SQLStore) IsPostgres() bool {
	return s.driverName == "postgres"
}

// supportsONConflict SQLite/Postgres 使用标准 UPSERT 语法
func (s *SQLStore) supportsONConflict() bool {
	return s.IsSQLite() || s.IsPostgres()
}

// supportsRowLock MySQL/Postgres 支持 SELECT ... FOR UPDATE
func (s *SQLStore) supportsRowLock() bool {
	return s.IsMySQL() || s.IsPostgres()
}

// q 按驱动 rebind 占位符（Postgres: ? → $n）
func (s *SQLStore) q(query string) string {
	if !s.IsPostgres() {
		return query
	}
	return RebindPostgres(query)
}

// quoteIdent 引用保留字列名（key/value 等）
// MySQL/SQLite DDL 用反引号；Postgres 用双引号。
func (s *SQLStore) quoteIdent(name string) string {
	if s.IsPostgres() {
		return `"` + name + `"`
	}
	return "`" + name + "`"
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
	var deleted int64

	for {
		var query string
		if s.IsMySQL() {
			// MySQL: 直接使用 LIMIT
			query = `DELETE FROM logs WHERE time < ? LIMIT ?`
		} else {
			// SQLite / Postgres: 子查询 LIMIT（Postgres 无 DELETE ... LIMIT）
			query = `DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE time < ? LIMIT ?)`
		}

		result, err := s.ExecContext(ctx, query, cutoffMs, batchSize)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		deleted += affected
		if affected < batchSize {
			break // 已删完
		}
	}
	s.runSQLiteIncrementalVacuum(ctx, deleted)
	return nil
}

func (s *SQLStore) runSQLiteIncrementalVacuum(ctx context.Context, deletedRows int64) {
	if !s.IsSQLite() || deletedRows <= 0 {
		return
	}

	var mode int
	if err := s.db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		log.Printf("[WARN] SQLite 查询 auto_vacuum 失败: %v", err)
		return
	}
	if mode != 2 {
		return
	}

	var freePages int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		log.Printf("[WARN] SQLite 查询 freelist_count 失败: %v", err)
		return
	}
	if freePages <= 0 {
		return
	}

	const maxIncrementalVacuumPages int64 = 4096
	pages := min(freePages, maxIncrementalVacuumPages)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)); err != nil {
		log.Printf("[WARN] SQLite incremental_vacuum(%d) 失败: %v", pages, err)
	}
}

// ============================================================================
// 底层数据库访问方法（供 SyncManager 等组件使用）
// ============================================================================

// QueryContext 执行查询语句
func (s *SQLStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.q(query), args...)
}

// QueryRowContext 执行查询单行
func (s *SQLStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.q(query), args...)
}

// ExecContext 执行非查询语句
func (s *SQLStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.q(query), args...)
}

// BeginTx 开启事务
func (s *SQLStore) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, opts)
}

// execTx 在事务中执行（自动 rebind）
func (s *SQLStore) execTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.q(query), args...)
}

// queryRowTx 在事务中查单行（自动 rebind）
func (s *SQLStore) queryRowTx(ctx context.Context, tx *sql.Tx, query string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.q(query), args...)
}

// prepareTx 在事务中预处理（自动 rebind）
func (s *SQLStore) prepareTx(ctx context.Context, tx *sql.Tx, query string) (*sql.Stmt, error) {
	return tx.PrepareContext(ctx, s.q(query))
}

// lockPostgresExplicitIDTable serializes explicit-ID writes with normal INSERTs.
// PostgreSQL sequences are independent from table rows, so inserting an id does
// not advance the sequence. The table lock closes the race between the explicit
// write, sequence synchronization, and concurrent auto-ID inserts.
func (s *SQLStore) lockPostgresExplicitIDTable(ctx context.Context, tx *sql.Tx, table string) error {
	if !s.IsPostgres() {
		return nil
	}

	var query string
	switch table {
	case "channels":
		query = "LOCK TABLE channels IN SHARE ROW EXCLUSIVE MODE"
	case "auth_tokens":
		query = "LOCK TABLE auth_tokens IN SHARE ROW EXCLUSIVE MODE"
	default:
		return fmt.Errorf("unsupported explicit-id table: %s", table)
	}

	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("lock %s for explicit id: %w", table, err)
	}
	return nil
}

// syncPostgresIDSequence advances an explicit-ID table's sequence without ever
// moving it backward. The caller must hold lockPostgresExplicitIDTable's lock.
func (s *SQLStore) syncPostgresIDSequence(ctx context.Context, tx *sql.Tx, table string) error {
	if !s.IsPostgres() {
		return nil
	}

	var query string
	switch table {
	case "channels":
		query = `
			SELECT setval(
				pg_get_serial_sequence('channels', 'id'),
				GREATEST(COALESCE(MAX(id), 1), nextval(pg_get_serial_sequence('channels', 'id'))),
				true
			)
			FROM channels`
	case "auth_tokens":
		query = `
			SELECT setval(
				pg_get_serial_sequence('auth_tokens', 'id'),
				GREATEST(COALESCE(MAX(id), 1), nextval(pg_get_serial_sequence('auth_tokens', 'id'))),
				true
			)
			FROM auth_tokens`
	default:
		return fmt.Errorf("unsupported explicit-id table: %s", table)
	}

	var sequenceValue int64
	if err := tx.QueryRowContext(ctx, query).Scan(&sequenceValue); err != nil {
		return fmt.Errorf("sync %s id sequence: %w", table, err)
	}
	return nil
}

func (s *SQLStore) withPostgresExplicitIDTx(ctx context.Context, table string, fn func(*sql.Tx) error) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPostgresExplicitIDTable(ctx, tx, table); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return s.syncPostgresIDSequence(ctx, tx, table)
	})
}
