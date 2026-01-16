package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"ccLoad/internal/storage/schema"
)

// Dialect 数据库方言
type Dialect int

// Dialect 数据库方言常量
const (
	// DialectSQLite SQLite数据库方言
	DialectSQLite Dialect = iota
	// DialectMySQL MySQL数据库方言
	DialectMySQL
)

// sqliteMigratableTables 允许增量迁移的SQLite表名白名单
// 安全设计：防止SQL注入，新增表时需在此处注册
var sqliteMigratableTables = map[string]bool{
	"logs":              true,
	"auth_tokens":       true,
	"channel_models":    true,
	"channels":          true,
	"schema_migrations": true,
}

// migrateSQLite 执行SQLite数据库迁移
func migrateSQLite(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, DialectSQLite)
}

// migrateMySQL 执行MySQL数据库迁移
func migrateMySQL(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, DialectMySQL)
}

// migrate 统一迁移逻辑
func migrate(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// 表定义（顺序重要：外键依赖）
	tables := []func() *schema.TableBuilder{
		schema.DefineSchemaMigrationsTable, // 迁移版本表必须最先创建
		schema.DefineChannelsTable,
		schema.DefineAPIKeysTable,
		schema.DefineChannelModelsTable,
		schema.DefineAuthTokensTable,
		schema.DefineSystemSettingsTable,
		schema.DefineAdminSessionsTable,
		schema.DefineLogsTable,
	}

	// 创建表和索引
	for _, defineTable := range tables {
		tb := defineTable()

		// 创建表
		if _, err := db.ExecContext(ctx, buildDDL(tb, dialect)); err != nil {
			return fmt.Errorf("create %s table: %w", tb.Name(), err)
		}

		// 增量迁移：确保logs表新字段存在（2025-12新增）
		if tb.Name() == "logs" {
			if err := ensureLogsNewColumns(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs new columns: %w", err)
			}
		}

		// 增量迁移：确保channels表有daily_cost_limit字段（2026-01新增）
		if tb.Name() == "channels" {
			if err := ensureChannelsDailyCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels daily_cost_limit: %w", err)
			}
		}

		// 增量迁移：确保auth_tokens表有缓存token字段（2025-12新增）
		if tb.Name() == "auth_tokens" {
			if err := ensureAuthTokensCacheFields(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cache fields: %w", err)
			}
			// 增量迁移：确保auth_tokens表有allowed_models字段（2026-01新增）
			if err := ensureAuthTokensAllowedModels(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens allowed_models: %w", err)
			}
			// 增量迁移：确保auth_tokens表有费用限额字段（2026-01新增）
			if err := ensureAuthTokensCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cost_limit: %w", err)
			}
		}

		// 增量迁移：channel_models表添加redirect_model字段，迁移数据后删除channels冗余字段
		if tb.Name() == "channel_models" {
			if err := migrateChannelModelsSchema(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channel_models schema: %w", err)
			}
		}

		// 创建索引
		for _, idx := range buildIndexes(tb, dialect) {
			if err := createIndex(ctx, db, idx, dialect); err != nil {
				return err
			}
		}
	}

	// 初始化默认配置
	if err := initDefaultSettings(ctx, db, dialect); err != nil {
		return err
	}

	// 清理已移除的配置项（Fail-fast：确保Web管理界面不再暴露危险开关）
	if err := cleanupRemovedSettings(ctx, db, dialect); err != nil {
		return err
	}

	return nil
}

func cleanupRemovedSettings(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// skip_tls_verify 已移除：仅允许通过环境变量 CCLOAD_ALLOW_INSECURE_TLS 控制
	if err := deleteSystemSetting(ctx, db, dialect, "skip_tls_verify"); err != nil {
		return err
	}
	return nil
}

func deleteSystemSetting(ctx context.Context, db *sql.DB, dialect Dialect, key string) error {
	query := "DELETE FROM system_settings WHERE key = ?"
	if dialect == DialectMySQL {
		query = "DELETE FROM system_settings WHERE `key` = ?"
	}
	if _, err := db.ExecContext(ctx, query, key); err != nil {
		return fmt.Errorf("delete system setting %s: %w", key, err)
	}
	return nil
}

// hasSystemSetting 检查系统设置是否存在（用于配置迁移和旧版标记兼容）
func hasSystemSetting(ctx context.Context, db *sql.DB, dialect Dialect, key string) bool {
	query := "SELECT 1 FROM system_settings WHERE key = ? LIMIT 1"
	if dialect == DialectMySQL {
		query = "SELECT 1 FROM system_settings WHERE `key` = ? LIMIT 1"
	}
	var exists int
	err := db.QueryRowContext(ctx, query, key).Scan(&exists)
	return err == nil
}

// ensureLogsNewColumns 确保logs表有新增字段(2025-12新增,支持MySQL和SQLite)
func ensureLogsNewColumns(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		if err := ensureLogsMinuteBucketMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsAuthTokenIDMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsClientIPMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsCacheFieldsMySQL(ctx, db); err != nil {
			return err
		}
		return ensureLogsActualModelMySQL(ctx, db)
	}
	// SQLite: 使用PRAGMA table_info检查列
	return ensureLogsColumnsSQLite(ctx, db)
}

type sqliteColumnDef struct {
	name       string
	definition string
}

func ensureSQLiteColumns(ctx context.Context, db *sql.DB, table string, cols []sqliteColumnDef) error {
	existingCols, err := sqliteExistingColumns(ctx, db, table)
	if err != nil {
		return err
	}

	for _, col := range cols {
		if existingCols[col.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col.name, col.definition)); err != nil {
			return fmt.Errorf("add %s: %w", col.name, err)
		}
	}

	return nil
}

// mysqlColumnDef MySQL列定义
type mysqlColumnDef struct {
	name       string
	definition string
}

// ensureMySQLColumns 通用MySQL添加列函数（幂等操作）
func ensureMySQLColumns(ctx context.Context, db *sql.DB, table string, cols []mysqlColumnDef) error {
	added := false
	for _, col := range cols {
		var count int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?",
			table, col.name,
		).Scan(&count); err != nil {
			return fmt.Errorf("check %s field: %w", col.name, err)
		}
		if count == 0 {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col.name, col.definition)); err != nil {
				return fmt.Errorf("add %s column: %w", col.name, err)
			}
			added = true
		}
	}
	if added {
		log.Printf("[MIGRATE] Added columns to %s", table)
	}
	return nil
}

// ensureLogsColumnsSQLite SQLite增量迁移logs表新字段
func ensureLogsColumnsSQLite(ctx context.Context, db *sql.DB) error {
	// 第一步：添加基础字段（幂等操作）
	if err := ensureSQLiteColumns(ctx, db, "logs", []sqliteColumnDef{
		{name: "minute_bucket", definition: "INTEGER NOT NULL DEFAULT 0"}, // time/60000，用于RPM类聚合
		{name: "auth_token_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "client_ip", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "cache_5m_input_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "cache_1h_input_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "actual_model", definition: "TEXT NOT NULL DEFAULT ''"}, // 实际转发的模型
	}); err != nil {
		return err
	}

	// 第二步：迁移历史数据，将cache_creation_input_tokens复制到cache_5m_input_tokens
	_, err := db.ExecContext(ctx,
		"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens WHERE cache_5m_input_tokens = 0 AND cache_creation_input_tokens > 0",
	)
	if err != nil {
		return fmt.Errorf("migrate cache_5m data: %w", err)
	}

	// 第三步：回填 minute_bucket（基于标记机制，支持崩溃恢复）
	const backfillMarker = "minute_bucket_backfill_done"
	if !hasMigration(ctx, db, backfillMarker) {
		log.Println("[migrate] backfilling minute_bucket for SQLite...")
		if err := backfillLogsMinuteBucketSQLite(ctx, db, 5_000); err != nil {
			return fmt.Errorf("backfill minute_bucket: %w", err)
		}
		if err := recordMigration(ctx, db, backfillMarker, DialectSQLite); err != nil {
			return fmt.Errorf("record migration marker: %w", err)
		}
		log.Println("[migrate] minute_bucket backfill completed")
	}

	return nil
}

func backfillLogsMinuteBucketSQLite(ctx context.Context, db *sql.DB, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 5_000
	}

	for {
		res, err := db.ExecContext(ctx,
			"UPDATE logs SET minute_bucket = (time / 60000) WHERE id IN (SELECT id FROM logs WHERE minute_bucket = 0 AND time > 0 LIMIT ?)",
			batchSize,
		)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
	}
}

// ensureLogsAuthTokenIDMySQL 确保logs表有auth_token_id字段(MySQL增量迁移,2025-12新增)
func ensureLogsAuthTokenIDMySQL(ctx context.Context, db *sql.DB) error {
	// 检查字段是否存在
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='auth_token_id'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check column existence: %w", err)
	}

	// 字段已存在,跳过
	if count > 0 {
		return nil
	}

	// 添加auth_token_id字段
	_, err = db.ExecContext(ctx,
		"ALTER TABLE logs ADD COLUMN auth_token_id BIGINT NOT NULL DEFAULT 0 COMMENT '客户端使用的API令牌ID(新增2025-12)'",
	)
	if err != nil {
		return fmt.Errorf("add auth_token_id column: %w", err)
	}

	return nil
}

// ensureLogsClientIPMySQL 确保logs表有client_ip字段(MySQL增量迁移,2025-12新增)
func ensureLogsClientIPMySQL(ctx context.Context, db *sql.DB) error {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='client_ip'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check column existence: %w", err)
	}

	if count > 0 {
		return nil
	}

	_, err = db.ExecContext(ctx,
		"ALTER TABLE logs ADD COLUMN client_ip VARCHAR(45) NOT NULL DEFAULT '' COMMENT '客户端IP地址(新增2025-12)'",
	)
	if err != nil {
		return fmt.Errorf("add client_ip column: %w", err)
	}

	return nil
}

// ensureLogsCacheFieldsMySQL 确保logs表有缓存细分字段(MySQL增量迁移,2025-12新增)
func ensureLogsCacheFieldsMySQL(ctx context.Context, db *sql.DB) error {
	// 检查cache_5m_input_tokens字段是否存在
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='cache_5m_input_tokens'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check cache_5m_input_tokens existence: %w", err)
	}

	// 字段已存在,跳过
	if count > 0 {
		return nil
	}

	// 添加cache_5m_input_tokens字段
	_, err = db.ExecContext(ctx,
		"ALTER TABLE logs ADD COLUMN cache_5m_input_tokens INT NOT NULL DEFAULT 0 COMMENT '5分钟缓存写入Token数(新增2025-12)'",
	)
	if err != nil {
		return fmt.Errorf("add cache_5m_input_tokens column: %w", err)
	}

	// 添加cache_1h_input_tokens字段
	_, err = db.ExecContext(ctx,
		"ALTER TABLE logs ADD COLUMN cache_1h_input_tokens INT NOT NULL DEFAULT 0 COMMENT '1小时缓存写入Token数(新增2025-12)'",
	)
	if err != nil {
		return fmt.Errorf("add cache_1h_input_tokens column: %w", err)
	}

	// 迁移历史数据，将cache_creation_input_tokens复制到cache_5m_input_tokens
	_, err = db.ExecContext(ctx,
		"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens WHERE cache_5m_input_tokens = 0 AND cache_creation_input_tokens > 0",
	)
	if err != nil {
		return fmt.Errorf("migrate cache_5m data: %w", err)
	}

	return nil
}

func ensureLogsMinuteBucketMySQL(ctx context.Context, db *sql.DB) error {
	// 第一步：添加列（幂等操作）
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='minute_bucket'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check minute_bucket existence: %w", err)
	}
	if count == 0 {
		_, err = db.ExecContext(ctx,
			"ALTER TABLE logs ADD COLUMN minute_bucket BIGINT NOT NULL DEFAULT 0 COMMENT 'time/60000，用于RPM类聚合(新增2026-01)'",
		)
		if err != nil {
			return fmt.Errorf("add minute_bucket column: %w", err)
		}
	}

	// 第二步：回填历史数据（基于标记机制，支持崩溃恢复）
	const backfillMarker = "minute_bucket_backfill_done"
	if !hasMigration(ctx, db, backfillMarker) {
		log.Println("[migrate] backfilling minute_bucket for MySQL...")
		if err := backfillLogsMinuteBucketMySQL(ctx, db, 10_000); err != nil {
			return err
		}
		if err := recordMigration(ctx, db, backfillMarker, DialectMySQL); err != nil {
			return fmt.Errorf("record migration marker: %w", err)
		}
		log.Println("[migrate] minute_bucket backfill completed")
	}
	return nil
}

func backfillLogsMinuteBucketMySQL(ctx context.Context, db *sql.DB, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 10_000
	}

	for {
		res, err := db.ExecContext(ctx,
			"UPDATE logs SET minute_bucket = FLOOR(time / 60000) WHERE minute_bucket = 0 AND time > 0 LIMIT ?",
			batchSize,
		)
		if err != nil {
			return fmt.Errorf("backfill minute_bucket: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
	}
}

// ensureAuthTokensCacheFields 确保auth_tokens表有缓存token字段(2025-12新增,支持MySQL和SQLite)
func ensureAuthTokensCacheFields(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		return ensureAuthTokensCacheFieldsMySQL(ctx, db)
	}
	return ensureAuthTokensCacheFieldsSQLite(ctx, db)
}

// ensureAuthTokensCacheFieldsSQLite SQLite增量迁移auth_tokens缓存字段
func ensureAuthTokensCacheFieldsSQLite(ctx context.Context, db *sql.DB) error {
	return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
		{name: "cache_read_tokens_total", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "cache_creation_tokens_total", definition: "INTEGER NOT NULL DEFAULT 0"},
	})
}

// ensureAuthTokensCacheFieldsMySQL MySQL增量迁移auth_tokens缓存字段
func ensureAuthTokensCacheFieldsMySQL(ctx context.Context, db *sql.DB) error {
	// 检查cache_read_tokens_total字段是否存在
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='auth_tokens' AND COLUMN_NAME='cache_read_tokens_total'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check cache_read_tokens_total existence: %w", err)
	}

	// 字段已存在,跳过
	if count > 0 {
		return nil
	}

	// 添加cache_read_tokens_total字段
	_, err = db.ExecContext(ctx,
		"ALTER TABLE auth_tokens ADD COLUMN cache_read_tokens_total BIGINT NOT NULL DEFAULT 0 COMMENT '累计缓存读Token数'",
	)
	if err != nil {
		return fmt.Errorf("add cache_read_tokens_total column: %w", err)
	}

	// 添加cache_creation_tokens_total字段
	_, err = db.ExecContext(ctx,
		"ALTER TABLE auth_tokens ADD COLUMN cache_creation_tokens_total BIGINT NOT NULL DEFAULT 0 COMMENT '累计缓存写Token数'",
	)
	if err != nil {
		return fmt.Errorf("add cache_creation_tokens_total column: %w", err)
	}

	return nil
}

func sqliteExistingColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	if !sqliteMigratableTables[table] {
		return nil, fmt.Errorf("invalid table name: %s", table)
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("get table info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	existingCols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("scan column info: %w", err)
		}
		existingCols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns: %w", err)
	}

	return existingCols, nil
}

func buildDDL(tb *schema.TableBuilder, dialect Dialect) string {
	if dialect == DialectMySQL {
		return tb.BuildMySQL()
	}
	return tb.BuildSQLite()
}

func buildIndexes(tb *schema.TableBuilder, dialect Dialect) []schema.IndexDef {
	if dialect == DialectMySQL {
		return tb.GetIndexesMySQL()
	}
	return tb.GetIndexesSQLite()
}

func createIndex(ctx context.Context, db *sql.DB, idx schema.IndexDef, dialect Dialect) error {
	_, err := db.ExecContext(ctx, idx.SQL)
	if err == nil {
		return nil
	}

	// MySQL 5.6不支持IF NOT EXISTS，忽略重复索引错误
	if dialect == DialectMySQL && strings.Contains(err.Error(), "Duplicate key name") {
		return nil
	}

	// SQLite的IF NOT EXISTS应该不会报错，但如果报错则返回
	return fmt.Errorf("create index: %w", err)
}

func initDefaultSettings(ctx context.Context, db *sql.DB, dialect Dialect) error {
	settings := []struct {
		key, value, valueType, desc, defaultVal string
	}{
		{"log_retention_days", "7", "int", "日志保留天数(-1永久保留,1-365天)", "7"},
		{"max_key_retries", "3", "int", "单渠道最大Key重试次数", "3"},
		{"upstream_first_byte_timeout", "0", "duration", "上游首块响应体超时(秒,0=禁用，仅流式)", "0"},
		{"non_stream_timeout", "120", "duration", "非流式请求超时(秒,0=禁用)", "120"},
		{"model_lookup_strip_date_suffix", "true", "bool", "模型匹配失败时，忽略末尾-YYYYMMDD日期后缀进行渠道匹配(优先精确匹配)", "true"},
		{"model_fuzzy_match", "false", "bool", "模型匹配失败时，使用子串模糊匹配(多匹配时选最新版本)", "false"},
		{"channel_test_content", "sonnet 4.0的发布日期是什么", "string", "渠道测试默认内容", "sonnet 4.0的发布日期是什么"},
		{"channel_stats_range", "today", "string", "渠道管理费用统计范围", "today"},
		// 健康度排序配置
		{"enable_health_score", "false", "bool", "启用基于健康度的渠道动态排序", "false"},
		{"success_rate_penalty_weight", "100", "int", "成功率惩罚权重(乘以失败率)", "100"},
		{"health_score_window_minutes", "30", "int", "成功率统计时间窗口(分钟)", "30"},
		{"health_score_update_interval", "30", "int", "成功率缓存更新间隔(秒)", "30"},
		{"health_min_confident_sample", "20", "int", "置信样本量阈值(样本量达到此值时惩罚全额生效)", "20"},
		// 冷却兜底配置
		{"cooldown_fallback_enabled", "true", "bool", "所有渠道冷却时选最优渠道兜底(关闭则直接拒绝请求)", "true"},
	}

	var query string
	if dialect == DialectMySQL {
		query = "INSERT IGNORE INTO system_settings (`key`, value, value_type, description, default_value, updated_at) VALUES (?, ?, ?, ?, ?, UNIX_TIMESTAMP())"
	} else {
		query = "INSERT OR IGNORE INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES (?, ?, ?, ?, ?, unixepoch())"
	}

	for _, s := range settings {
		if _, err := db.ExecContext(ctx, query, s.key, s.value, s.valueType, s.desc, s.defaultVal); err != nil {
			return fmt.Errorf("insert default setting %s: %w", s.key, err)
		}
	}

	// 刷新部分配置项的元信息（description/default/value_type），避免“代码语义已变但DB描述仍旧”。
	// 不更新 updated_at：这不是用户配置变更，只是元数据对齐。
	{
		keyCol := "key"
		if dialect == DialectMySQL {
			keyCol = "`key`"
		}
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		metaSQL := fmt.Sprintf("UPDATE system_settings SET description = ?, default_value = ?, value_type = ? WHERE %s = ?", keyCol)
		if _, err := db.ExecContext(ctx, metaSQL,
			"上游首块响应体超时(秒,0=禁用，仅流式)",
			"0",
			"duration",
			"upstream_first_byte_timeout",
		); err != nil {
			return fmt.Errorf("refresh setting metadata upstream_first_byte_timeout: %w", err)
		}
	}

	// 迁移 success_rate_penalty_weight 类型：float → int（2026-01 类型修正）
	{
		keyCol := "key"
		if dialect == DialectMySQL {
			keyCol = "`key`"
		}
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		typeSQL := fmt.Sprintf("UPDATE system_settings SET value_type = 'int' WHERE %s = 'success_rate_penalty_weight' AND value_type = 'float'", keyCol)
		if _, err := db.ExecContext(ctx, typeSQL); err != nil {
			return fmt.Errorf("migrate success_rate_penalty_weight type: %w", err)
		}
	}

	// 清理已废弃的配置项（先执行，避免被后续逻辑的 return 跳过）
	obsoleteKeys := []string{
		"88code_free_only", // 2026-01移除：88code免费订阅限制功能已删除
	}
	for _, key := range obsoleteKeys {
		_ = deleteSystemSetting(ctx, db, dialect, key) // 忽略错误（可能不存在）
	}

	// 迁移旧 migration marker 从 system_settings 到 schema_migrations
	legacyMigrationMarkers := []string{
		"minute_bucket_backfill_done", // 2026-01迁移：迁移标记改存 schema_migrations 表
	}
	for _, marker := range legacyMigrationMarkers {
		if hasSystemSetting(ctx, db, dialect, marker) {
			// 先迁移到 schema_migrations，再删除旧记录
			_ = recordMigration(ctx, db, marker, dialect)
			_ = deleteSystemSetting(ctx, db, dialect, marker)
		}
	}

	// 迁移旧键名 cooldown_fallback_threshold → cooldown_fallback_enabled
	// 同时处理 int→bool 的类型迁移
	if hasSystemSetting(ctx, db, dialect, "cooldown_fallback_threshold") {
		const oldKey = "cooldown_fallback_threshold"
		const newKey = "cooldown_fallback_enabled"

		keyCol := "key"
		if dialect == DialectMySQL {
			keyCol = "`key`"
		}

		// 1. 先将旧的 int 值(0/非0)迁移为 bool 值(false/true)
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		valueMigrateSQL := fmt.Sprintf(`UPDATE system_settings SET value = CASE WHEN value = '0' THEN 'false' ELSE 'true' END WHERE %s = ? AND value_type = 'int'`, keyCol)
		if _, err := db.ExecContext(ctx, valueMigrateSQL, oldKey); err != nil {
			return fmt.Errorf("migrate setting value %s: %w", oldKey, err)
		}

		// 2. 如果新键已存在，直接删除旧键；否则重命名
		if hasSystemSetting(ctx, db, dialect, newKey) {
			if err := deleteSystemSetting(ctx, db, dialect, oldKey); err != nil {
				return err
			}
		} else {
			//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
			renameSQL := fmt.Sprintf("UPDATE system_settings SET %s = ?, description = ?, default_value = ?, value_type = ? WHERE %s = ?", keyCol, keyCol)
			if _, err := db.ExecContext(ctx, renameSQL, newKey, "所有渠道冷却时选最优渠道兜底(关闭则直接拒绝请求)", "true", "bool", oldKey); err != nil {
				return fmt.Errorf("rename setting %s to %s: %w", oldKey, newKey, err)
			}
		}
	}

	return nil
}

// ensureLogsActualModelMySQL 确保logs表有actual_model字段(MySQL增量迁移)
func ensureLogsActualModelMySQL(ctx context.Context, db *sql.DB) error {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='actual_model'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check actual_model existence: %w", err)
	}

	if count > 0 {
		return nil
	}

	_, err = db.ExecContext(ctx,
		"ALTER TABLE logs ADD COLUMN actual_model VARCHAR(191) NOT NULL DEFAULT '' COMMENT '实际转发的模型(空表示未重定向)'",
	)
	if err != nil {
		return fmt.Errorf("add actual_model column: %w", err)
	}

	return nil
}

// migrateChannelModelsSchema 迁移channel_models表结构
// 版本控制：使用 schema_migrations 表记录已执行的迁移，确保幂等性
// 1. 添加redirect_model字段
// 2. 从channels.models和model_redirects迁移数据到channel_models
// 3. 放宽channels表废弃字段约束(NOT NULL → NULL)，保留兼容性以支持版本回滚
func migrateChannelModelsSchema(ctx context.Context, db *sql.DB, dialect Dialect) error {
	const migrationVersion = "v1_channel_models_redirect"

	// 检查迁移是否已执行（幂等性保证）
	if applied, err := isMigrationApplied(ctx, db, migrationVersion); err != nil {
		return fmt.Errorf("check migration status: %w", err)
	} else if applied {
		return nil // 已执行，跳过
	}

	// 第一步：添加redirect_model字段
	if err := ensureChannelModelsRedirectField(ctx, db, dialect); err != nil {
		return err
	}

	// 第二步：从channels.model_redirects迁移数据到channel_models
	if err := migrateModelRedirectsData(ctx, db, dialect); err != nil {
		return err
	}

	// 第三步：放宽channels表废弃字段约束（NOT NULL → NULL）
	if err := relaxDeprecatedChannelFields(ctx, db, dialect); err != nil {
		return err
	}

	// 记录迁移完成
	if err := recordMigration(ctx, db, migrationVersion, dialect); err != nil {
		log.Printf("[WARN] Failed to record migration %s: %v", migrationVersion, err)
		// 不阻塞，迁移本身已成功
	}

	return nil
}

// isMigrationApplied 检查迁移是否已执行
func isMigrationApplied(ctx context.Context, db *sql.DB, version string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version,
	).Scan(&count)
	if err != nil {
		// 表不存在时视为未执行
		return false, nil
	}
	return count > 0, nil
}

// hasMigration 检查迁移是否已执行（简化版，忽略错误）
func hasMigration(ctx context.Context, db *sql.DB, version string) bool {
	applied, _ := isMigrationApplied(ctx, db, version)
	return applied
}

// recordMigration 记录迁移已执行
func recordMigration(ctx context.Context, db *sql.DB, version string, dialect Dialect) error {
	var insertSQL string
	if dialect == DialectMySQL {
		insertSQL = `INSERT IGNORE INTO schema_migrations (version, applied_at) VALUES (?, UNIX_TIMESTAMP())`
	} else {
		insertSQL = `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, unixepoch())`
	}
	_, err := db.ExecContext(ctx, insertSQL, version)
	return err
}

// ensureChannelModelsRedirectField 确保channel_models表有redirect_model字段
func ensureChannelModelsRedirectField(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channel_models' AND COLUMN_NAME='redirect_model'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check redirect_model existence: %w", err)
		}
		if count > 0 {
			return nil
		}
		_, err = db.ExecContext(ctx,
			"ALTER TABLE channel_models ADD COLUMN redirect_model VARCHAR(191) NOT NULL DEFAULT '' COMMENT '重定向目标模型(空表示不重定向)'",
		)
		if err != nil {
			return fmt.Errorf("add redirect_model column: %w", err)
		}
		return nil
	}

	// SQLite
	return ensureSQLiteColumns(ctx, db, "channel_models", []sqliteColumnDef{
		{name: "redirect_model", definition: "TEXT NOT NULL DEFAULT ''"},
	})
}

// migrateModelRedirectsData 从channels.models和model_redirects迁移数据到channel_models
func migrateModelRedirectsData(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// 检查是否需要迁移
	needMigration, err := needChannelModelsMigration(ctx, db, dialect)
	if err != nil {
		return err
	}
	if !needMigration {
		return nil
	}

	// 查询所有需要迁移的渠道（有models数据）
	// 注意：必须同时查询 models 和 model_redirects
	rows, err := db.QueryContext(ctx,
		"SELECT id, models, model_redirects FROM channels WHERE models != '' AND models != '[]'")
	if err != nil {
		return fmt.Errorf("query channels for migration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 收集所有待迁移的数据
	type modelEntry struct {
		channelID     int64
		model         string
		redirectModel string
	}
	var entries []modelEntry
	var channelIDs []int64

	for rows.Next() {
		var channelID int64
		var modelsJSON, redirectsJSON string
		if err := rows.Scan(&channelID, &modelsJSON, &redirectsJSON); err != nil {
			return fmt.Errorf("scan channel data: %w", err)
		}

		// [FIX] P2: 解析 models JSON 数组，失败时中断迁移（Fail-Fast）
		models, err := parseModelsForMigration(modelsJSON)
		if err != nil {
			return fmt.Errorf("channel %d: %w", channelID, err)
		}
		if len(models) == 0 {
			continue
		}

		// 只有解析成功才记录 channelID（避免解析失败的渠道被重命名字段后丢失数据）
		channelIDs = append(channelIDs, channelID)

		// 解析 model_redirects JSON 对象
		redirects, _ := parseModelRedirectsForMigration(redirectsJSON)
		if redirects == nil {
			redirects = make(map[string]string)
		}

		// 构建条目：每个模型一条记录
		for _, model := range models {
			entries = append(entries, modelEntry{
				channelID:     channelID,
				model:         model,
				redirectModel: redirects[model], // 如果没有重定向则为空
			})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 无数据需要迁移
	if len(channelIDs) == 0 {
		return nil
	}

	// 使用事务批量执行
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 插入或更新 channel_models
	for _, e := range entries {
		var upsertSQL string
		if dialect == DialectMySQL {
			upsertSQL = `INSERT INTO channel_models (channel_id, model, redirect_model, created_at)
				VALUES (?, ?, ?, UNIX_TIMESTAMP())
				ON DUPLICATE KEY UPDATE redirect_model = VALUES(redirect_model)`
		} else {
			upsertSQL = `INSERT INTO channel_models (channel_id, model, redirect_model, created_at)
				VALUES (?, ?, ?, unixepoch())
				ON CONFLICT(channel_id, model) DO UPDATE SET redirect_model = excluded.redirect_model`
		}
		if _, err := tx.ExecContext(ctx, upsertSQL, e.channelID, e.model, e.redirectModel); err != nil {
			return fmt.Errorf("upsert channel_model: %w", err)
		}
	}

	// 数据迁移完成，字段约束放宽在 relaxDeprecatedChannelFields 中处理
	return tx.Commit()
}

// needChannelModelsMigration 检查是否需要迁移
// 检查 channels.models 字段是否存在（未被重命名为 _deprecated_models）
func needChannelModelsMigration(ctx context.Context, db *sql.DB, dialect Dialect) (bool, error) {
	if dialect == DialectMySQL {
		// MySQL: 检查 models 字段是否存在
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='models'",
		).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check models column: %w", err)
		}
		return count > 0, nil
	}

	// SQLite: 检查 models 字段是否存在
	existingCols, err := sqliteExistingColumns(ctx, db, "channels")
	if err != nil {
		return false, nil // 表不存在或其他错误，视为无需迁移
	}
	return existingCols["models"], nil
}

// parseModelsForMigration 解析 models JSON 数组用于迁移
// [FIX] P2: 解析失败返回错误而非静默忽略，避免数据丢失
func parseModelsForMigration(jsonStr string) ([]string, error) {
	if jsonStr == "" || jsonStr == "[]" {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal([]byte(jsonStr), &models); err != nil {
		return nil, fmt.Errorf("parse models JSON %q: %w", jsonStr, err)
	}
	return models, nil
}

// parseModelRedirectsForMigration 解析model_redirects JSON用于迁移
func parseModelRedirectsForMigration(jsonStr string) (map[string]string, error) {
	if jsonStr == "" || jsonStr == "{}" {
		return nil, nil
	}
	var redirects map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &redirects); err != nil {
		return nil, fmt.Errorf("parse model_redirects JSON: %w", err)
	}
	return redirects, nil
}

// relaxDeprecatedChannelFields 放宽channels表废弃字段的约束
// 将 models 和 model_redirects 从 NOT NULL 改为允许 NULL
// 这样新版程序 INSERT 时不提供这些字段也不会报错，同时保留字段名以支持版本回滚
func relaxDeprecatedChannelFields(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		// MySQL: 使用 MODIFY COLUMN 去除 NOT NULL
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='models'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check models field: %w", err)
		}
		if count > 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE channels MODIFY COLUMN models TEXT NULL"); err != nil {
				return fmt.Errorf("modify models column: %w", err)
			}
			log.Printf("[MIGRATE] Modified channels.models: NOT NULL → NULL")
		}

		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='model_redirects'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check model_redirects field: %w", err)
		}
		if count > 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE channels MODIFY COLUMN model_redirects TEXT NULL"); err != nil {
				return fmt.Errorf("modify model_redirects column: %w", err)
			}
			log.Printf("[MIGRATE] Modified channels.model_redirects: NOT NULL → NULL")
		}
		return nil
	}

	// SQLite: 不支持直接修改列约束，但 TEXT 类型天然允许 NULL
	// SQLite 的 NOT NULL 约束只在显式 INSERT 该列时检查
	// 新版程序 INSERT 语句不包含这些列，SQLite 会使用默认值（NULL）
	return nil
}

// ensureChannelsDailyCostLimit 确保channels表有daily_cost_limit字段
func ensureChannelsDailyCostLimit(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		// MySQL: 检查字段是否存在
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='daily_cost_limit'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check daily_cost_limit field: %w", err)
		}
		if count == 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE channels ADD COLUMN daily_cost_limit DOUBLE NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("add daily_cost_limit column: %w", err)
			}
			log.Printf("[MIGRATE] Added channels.daily_cost_limit column")
		}
		return nil
	}

	// SQLite: 使用通用添加列函数
	return ensureSQLiteColumns(ctx, db, "channels", []sqliteColumnDef{
		{name: "daily_cost_limit", definition: "REAL NOT NULL DEFAULT 0"},
	})
}

// ensureAuthTokensAllowedModels 确保auth_tokens表有allowed_models字段
func ensureAuthTokensAllowedModels(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		// MySQL: 检查字段是否存在
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='auth_tokens' AND COLUMN_NAME='allowed_models'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check allowed_models field: %w", err)
		}
		if count == 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE auth_tokens ADD COLUMN allowed_models VARCHAR(2000) NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("add allowed_models column: %w", err)
			}
			log.Printf("[MIGRATE] Added auth_tokens.allowed_models column")
		}
		return nil
	}

	// SQLite: 使用通用添加列函数
	return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
		{name: "allowed_models", definition: "TEXT NOT NULL DEFAULT ''"},
	})
}

// ensureAuthTokensCostLimit 确保auth_tokens表有费用限额字段（2026-01新增）
func ensureAuthTokensCostLimit(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		return ensureMySQLColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "cost_used_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
			{name: "cost_limit_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
		})
	}

	// SQLite: 使用通用添加列函数
	return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
		{name: "cost_used_microusd", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "cost_limit_microusd", definition: "INTEGER NOT NULL DEFAULT 0"},
	})
}
