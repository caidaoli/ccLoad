package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ccLoad/internal/storage/schema"
)

// Dialect 数据库方言
type Dialect int

const (
	DialectSQLite Dialect = iota
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

		// 增量迁移：确保auth_tokens表有缓存token字段（2025-12新增）
		if tb.Name() == "auth_tokens" {
			if err := ensureAuthTokensCacheFields(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cache fields: %w", err)
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

	return nil
}

// ensureLogsNewColumns 确保logs表有新增字段(2025-12新增,支持MySQL和SQLite)
func ensureLogsNewColumns(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		if err := ensureLogsAuthTokenIDMySQL(ctx, db); err != nil {
			return err
		}
		return ensureLogsClientIPMySQL(ctx, db)
	}
	// SQLite: 使用PRAGMA table_info检查列
	return ensureLogsColumnsSQLite(ctx, db)
}

// ensureLogsColumnsSQLite SQLite增量迁移logs表新字段
func ensureLogsColumnsSQLite(ctx context.Context, db *sql.DB) error {
	// 获取现有列
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(logs)")
	if err != nil {
		return fmt.Errorf("get table info: %w", err)
	}
	defer rows.Close()

	existingCols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan column info: %w", err)
		}
		existingCols[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate columns: %w", err)
	}

	// 添加缺失的列
	if !existingCols["auth_token_id"] {
		if _, err := db.ExecContext(ctx, "ALTER TABLE logs ADD COLUMN auth_token_id INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add auth_token_id: %w", err)
		}
	}
	if !existingCols["client_ip"] {
		if _, err := db.ExecContext(ctx, "ALTER TABLE logs ADD COLUMN client_ip TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add client_ip: %w", err)
		}
	}
	return nil
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

// ensureAuthTokensCacheFields 确保auth_tokens表有缓存token字段(2025-12新增,支持MySQL和SQLite)
func ensureAuthTokensCacheFields(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		return ensureAuthTokensCacheFieldsMySQL(ctx, db)
	}
	return ensureAuthTokensCacheFieldsSQLite(ctx, db)
}

// ensureAuthTokensCacheFieldsSQLite SQLite增量迁移auth_tokens缓存字段
func ensureAuthTokensCacheFieldsSQLite(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(auth_tokens)")
	if err != nil {
		return fmt.Errorf("get table info: %w", err)
	}
	defer rows.Close()

	existingCols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan column info: %w", err)
		}
		existingCols[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate columns: %w", err)
	}

	if !existingCols["cache_read_tokens_total"] {
		if _, err := db.ExecContext(ctx, "ALTER TABLE auth_tokens ADD COLUMN cache_read_tokens_total INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add cache_read_tokens_total: %w", err)
		}
	}
	if !existingCols["cache_creation_tokens_total"] {
		if _, err := db.ExecContext(ctx, "ALTER TABLE auth_tokens ADD COLUMN cache_creation_tokens_total INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add cache_creation_tokens_total: %w", err)
		}
	}
	return nil
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
		{"upstream_first_byte_timeout", "0", "duration", "上游首字节超时(秒,0=禁用)", "0"},
		{"non_stream_timeout", "120", "duration", "非流式请求超时(秒,0=禁用)", "120"},
		{"88code_free_only", "false", "bool", "仅允许使用88code免费订阅(free订阅可用时生效)", "false"},
		{"skip_tls_verify", "false", "bool", "跳过TLS证书验证", "false"},
		{"channel_test_content", "sonnet 4.0的发布日期是什么", "string", "渠道测试默认内容", "sonnet 4.0的发布日期是什么"},
		{"channel_stats_range", "today", "string", "渠道管理费用统计范围", "today"},
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

	return nil
}
