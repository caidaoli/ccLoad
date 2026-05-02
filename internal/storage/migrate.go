package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ccLoad/internal/storage/schema"
)

const (
	channelModelsRedirectMigrationVersion = "v1_channel_models_redirect"
	channelModelsOrderRepairVersion       = "v2_channel_models_created_at_order"
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
		schema.DefineChannelProtocolTransformsTable,
		schema.DefineChannelURLStatesTable,
		schema.DefineAuthTokensTable,
		schema.DefineSystemSettingsTable,
		schema.DefineAdminSessionsTable,
		schema.DefineLogsTable,
		schema.DefineDebugLogsTable,
	}

	// 一次性预查全库索引，避免每张表单独 SELECT 网络往返
	allIndexes, err := loadAllExistingIndexes(ctx, db, dialect)
	if err != nil {
		return fmt.Errorf("load all existing indexes: %w", err)
	}

	// 创建表和索引
	for _, defineTable := range tables {
		tb := defineTable()

		// Pre-create hook: debug_logs 表改用 log_id 作为主键（2026-04 重构）
		if tb.Name() == "debug_logs" {
			if err := rebuildDebugLogsPrimaryKey(ctx, db, dialect); err != nil {
				return fmt.Errorf("rebuild debug_logs primary key: %w", err)
			}
			if err := relaxDebugLogsRespBodyNullable(ctx, db, dialect); err != nil {
				return fmt.Errorf("relax debug_logs.resp_body nullability: %w", err)
			}
			delete(allIndexes, "debug_logs")
		}

		// Pre-create hook: channel_url_states 主键从 (channel_id, url) 重建为 (channel_id, url_hash)
		// （MySQL utf8mb4 下 VARCHAR(500) 超过 InnoDB 索引列 767 字节上限）
		if tb.Name() == "channel_url_states" {
			if err := rebuildChannelURLStatesPrimaryKey(ctx, db, dialect); err != nil {
				return fmt.Errorf("rebuild channel_url_states primary key: %w", err)
			}
			delete(allIndexes, "channel_url_states")
		}

		// 创建表
		if _, err := db.ExecContext(ctx, buildDDL(tb, dialect)); err != nil {
			return fmt.Errorf("create %s table: %w", tb.Name(), err)
		}

		// 增量迁移：确保logs表新字段存在（2025-12新增）
		if tb.Name() == "logs" {
			if err := ensureLogsNewColumns(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs new columns: %w", err)
			}
			if err := ensureLogsCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs cost_multiplier: %w", err)
			}
		}

		// 增量迁移：确保channels表有daily_cost_limit字段（2026-01新增）
		if tb.Name() == "channels" {
			if err := ensureChannelsDailyCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels daily_cost_limit: %w", err)
			}
			if err := ensureChannelsProtocolTransformMode(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels protocol_transform_mode: %w", err)
			}
			if err := ensureChannelsScheduledCheckEnabled(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels scheduled_check_enabled: %w", err)
			}
			if err := ensureChannelsScheduledCheckModel(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels scheduled_check_model: %w", err)
			}
			if err := ensureChannelsCustomRequestRules(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels custom_request_rules: %w", err)
			}
			if err := ensureChannelsCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels cost_multiplier: %w", err)
			}
			// 增量迁移：将url字段从VARCHAR(191)扩展为TEXT（支持多URL存储）
			if err := migrateChannelsURLToText(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels url to text: %w", err)
			}
		}

		// 增量迁移：修复 api_keys.api_key 历史长度漂移（旧版可能为 VARCHAR(64)）
		if tb.Name() == "api_keys" {
			if err := ensureAPIKeysAPIKeyLength(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys api_key column: %w", err)
			}
		}

		// 增量迁移：确保auth_tokens表有缓存token字段（2025-12新增）
		if tb.Name() == "auth_tokens" {
			if err := ensureAuthTokensCacheFields(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cache fields: %w", err)
			}
			if err := ensureAuthTokensAllowedModels(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens allowed_models: %w", err)
			}
			if err := validateAuthTokensAllowedModelsJSON(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens allowed_models: %w", err)
			}
			if err := ensureAuthTokensAllowedChannelIDs(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens allowed_channel_ids: %w", err)
			}
			if err := validateAuthTokensAllowedChannelIDsJSON(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens allowed_channel_ids: %w", err)
			}
			if err := ensureAuthTokensCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cost_limit: %w", err)
			}
			if err := ensureAuthTokensMaxConcurrency(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens max_concurrency: %w", err)
			}
			if err := validateAuthTokensMaxConcurrency(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens max_concurrency: %w", err)
			}
		}

		// 增量迁移：channel_models表添加redirect_model字段，迁移数据后删除channels冗余字段
		if tb.Name() == "channel_models" {
			if err := migrateChannelModelsSchema(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channel_models schema: %w", err)
			}
			if err := repairLegacyChannelModelOrder(ctx, db, dialect); err != nil {
				return fmt.Errorf("repair legacy channel_models order: %w", err)
			}
		}

		// 创建索引
		existingIdx := allIndexes[tb.Name()]
		for _, idx := range buildIndexes(tb, dialect) {
			if existingIdx[idx.Name] {
				continue
			}
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
	// model_lookup_strip_date_suffix 已移除：不再提供日期后缀回退匹配开关（避免行为分叉）
	if err := deleteSystemSetting(ctx, db, dialect, "model_lookup_strip_date_suffix"); err != nil {
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

// loadAllExistingIndexes 一次性查询整个数据库下所有表的现有索引集合
func loadAllExistingIndexes(ctx context.Context, db *sql.DB, dialect Dialect) (map[string]map[string]bool, error) {
	var query string
	if dialect == DialectMySQL {
		query = "SELECT DISTINCT TABLE_NAME, INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()"
	} else {
		query = "SELECT tbl_name, name FROM sqlite_master WHERE type='index' AND tbl_name IS NOT NULL"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query all indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]map[string]bool)
	for rows.Next() {
		var tbl, idx string
		if err := rows.Scan(&tbl, &idx); err != nil {
			return nil, fmt.Errorf("scan index row: %w", err)
		}
		if result[tbl] == nil {
			result[tbl] = make(map[string]bool)
		}
		result[tbl][idx] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexes: %w", err)
	}
	return result, nil
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

	// MySQL 5.6不支持IF NOT EXISTS，忽略重复索引错误(1061)
	if dialect == DialectMySQL {
		errMsg := err.Error()
		if strings.Contains(errMsg, "1061") ||
			strings.Contains(errMsg, "Duplicate key name") ||
			strings.Contains(errMsg, "already exist") {
			return nil
		}
	}

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
		{"model_fuzzy_match", "false", "bool", "模型匹配失败时，使用子串模糊匹配(多匹配时选最新版本)", "false"},
		{"channel_test_content", "sonnet 4.0的发布日期是什么", "string", "渠道测试默认内容", "sonnet 4.0的发布日期是什么"},
		{"channel_check_interval_hours", "5", "int", "渠道定时检测间隔(小时,0=关闭,修改后重启生效)", "5"},
		{"log_channel_click_action", "edit", "string", "日志页点击渠道名行为(edit=打开编辑器,navigate=跳转到渠道管理定位)", "edit"},
		{"channel_stats_range", "today", "string", "渠道管理费用统计范围", "today"},
		// 健康度排序配置
		{"enable_health_score", "false", "bool", "启用基于健康度的渠道动态排序", "false"},
		{"success_rate_penalty_weight", "100", "int", "成功率惩罚权重(乘以失败率)", "100"},
		{"health_score_window_minutes", "30", "int", "成功率统计时间窗口(分钟)", "30"},
		{"health_score_update_interval", "30", "int", "成功率缓存更新间隔(秒)", "30"},
		{"health_min_confident_sample", "20", "int", "置信样本量阈值(样本量达到此值时惩罚全额生效)", "20"},
		// 冷却兜底配置
		{"cooldown_fallback_enabled", "true", "bool", "所有渠道冷却时选最优渠道兜底(关闭则直接拒绝请求)", "true"},
		// Debug日志配置
		{"debug_log_enabled", "false", "bool", "启用Debug日志(记录上游请求/响应原始数据)", "false"},
		{"debug_log_retention_minutes", "2", "int", "Debug日志保留时长(分钟,1-1440)", "2"},
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

	// 刷新部分配置项的元信息（description/default/value_type），避免"代码语义已变但DB描述仍旧"。
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
		if _, err := db.ExecContext(ctx, metaSQL,
			"Debug日志保留时长(分钟,1-1440)",
			"2",
			"int",
			"debug_log_retention_minutes",
		); err != nil {
			return fmt.Errorf("refresh setting metadata debug_log_retention_minutes: %w", err)
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

	// 清理已废弃的配置项
	obsoleteKeys := []string{
		"88code_free_only", // 2026-01移除：88code免费订阅限制功能已删除
	}
	for _, key := range obsoleteKeys {
		_ = deleteSystemSetting(ctx, db, dialect, key)
	}

	// 迁移旧 migration marker 从 system_settings 到 schema_migrations
	legacyMigrationMarkers := []string{
		"minute_bucket_backfill_done", // 2026-01迁移：迁移标记改存 schema_migrations 表
	}
	for _, marker := range legacyMigrationMarkers {
		if hasSystemSetting(ctx, db, dialect, marker) {
			_ = recordMigration(ctx, db, marker, dialect)
			_ = deleteSystemSetting(ctx, db, dialect, marker)
		}
	}

	// 迁移旧键名 cooldown_fallback_threshold → cooldown_fallback_enabled
	if hasSystemSetting(ctx, db, dialect, "cooldown_fallback_threshold") {
		const oldKey = "cooldown_fallback_threshold"
		const newKey = "cooldown_fallback_enabled"

		keyCol := "key"
		if dialect == DialectMySQL {
			keyCol = "`key`"
		}

		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		valueMigrateSQL := fmt.Sprintf(`UPDATE system_settings SET value = CASE WHEN value = '0' THEN 'false' ELSE 'true' END WHERE %s = ? AND value_type = 'int'`, keyCol)
		if _, err := db.ExecContext(ctx, valueMigrateSQL, oldKey); err != nil {
			return fmt.Errorf("migrate setting value %s: %w", oldKey, err)
		}

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

func migrationAppliedAt(ctx context.Context, db *sql.DB, version string) (int64, bool, error) {
	var appliedAt int64
	err := db.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = ?`, version).Scan(&appliedAt)
	if err == nil {
		return appliedAt, true, nil
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("query migration %s applied_at: %w", version, err)
}

func recordMigrationTx(ctx context.Context, tx *sql.Tx, version string, dialect Dialect) error {
	var insertSQL string
	if dialect == DialectMySQL {
		insertSQL = `INSERT IGNORE INTO schema_migrations (version, applied_at) VALUES (?, UNIX_TIMESTAMP())`
	} else {
		insertSQL = `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, unixepoch())`
	}
	_, err := tx.ExecContext(ctx, insertSQL, version)
	return err
}
