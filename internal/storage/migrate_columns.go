package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// sqliteMigratableTables 允许增量迁移的SQLite表名白名单
// 安全设计：防止SQL注入，新增表时需在此处注册
var sqliteMigratableTables = map[string]bool{
	"logs":                        true,
	"auth_tokens":                 true,
	"channel_models":              true,
	"channel_protocol_transforms": true,
	"channels":                    true,
	"debug_logs":                  true,
	"schema_migrations":           true,
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

// ensureColumn 跨方言单列幂等添加。
// MySQL 走 INFORMATION_SCHEMA 探测 + ALTER ADD；SQLite 走 PRAGMA table_info + ALTER ADD。
// 调用方各自传入 MySQL/SQLite 列定义子句（不含 ADD COLUMN 关键字）。
func ensureColumn(ctx context.Context, db *sql.DB, dialect Dialect, table, col, mysqlDef, sqliteDef string) error {
	if dialect == DialectMySQL {
		return ensureMySQLColumns(ctx, db, table, []mysqlColumnDef{{name: col, definition: mysqlDef}})
	}
	return ensureSQLiteColumns(ctx, db, table, []sqliteColumnDef{{name: col, definition: sqliteDef}})
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
		if err := ensureLogsAPIKeyHashMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsActualModelMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsBaseURLMySQL(ctx, db); err != nil {
			return err
		}
		if err := ensureLogsServiceTierMySQL(ctx, db); err != nil {
			return err
		}
		return ensureLogsLogSourceMySQL(ctx, db)
	}
	// SQLite: 使用PRAGMA table_info检查列
	return ensureLogsColumnsSQLite(ctx, db)
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
		{name: "log_source", definition: "TEXT NOT NULL DEFAULT 'proxy'"},
		{name: "api_key_hash", definition: "TEXT NOT NULL DEFAULT ''"}, // API Key SHA256（用于精确定位 key_index）
		{name: "base_url", definition: "TEXT NOT NULL DEFAULT ''"},     // 请求使用的上游URL（多URL场景）
		{name: "service_tier", definition: "TEXT NOT NULL DEFAULT ''"}, // OpenAI service_tier: priority/flex
	}); err != nil {
		return err
	}

	// 第二步：迁移历史数据，将cache_creation_input_tokens复制到cache_5m_input_tokens（一次性）
	const cache5mBackfillMarker = "cache_5m_backfill_done"
	if !hasMigration(ctx, db, cache5mBackfillMarker) {
		_, err := db.ExecContext(ctx,
			"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens WHERE cache_5m_input_tokens = 0 AND cache_1h_input_tokens = 0 AND cache_creation_input_tokens > 0",
		)
		if err != nil {
			return fmt.Errorf("migrate cache_5m data: %w", err)
		}
		// 修复已损坏的数据：之前的迁移对1h缓存行错误地设置了cache_5m
		_, err = db.ExecContext(ctx,
			"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens - cache_1h_input_tokens WHERE cache_1h_input_tokens > 0 AND cache_5m_input_tokens = cache_creation_input_tokens",
		)
		if err != nil {
			return fmt.Errorf("repair cache_5m data: %w", err)
		}
		if err := recordMigration(ctx, db, cache5mBackfillMarker, DialectSQLite); err != nil {
			return fmt.Errorf("record cache_5m migration marker: %w", err)
		}
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

// ensureLogsAuthTokenIDMySQL 确保logs表有auth_token_id字段(MySQL增量迁移,2025-12新增)
func ensureLogsAuthTokenIDMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "auth_token_id", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '客户端使用的API令牌ID(新增2025-12)'"},
	})
}

// ensureLogsClientIPMySQL 确保logs表有client_ip字段(MySQL增量迁移,2025-12新增)
func ensureLogsClientIPMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "client_ip", definition: "VARCHAR(45) NOT NULL DEFAULT '' COMMENT '客户端IP地址(新增2025-12)'"},
	})
}

func ensureLogsAPIKeyHashMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "api_key_hash", definition: "VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'API Key SHA256(新增2026-02)'"},
	})
}

func ensureLogsBaseURLMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "base_url", definition: "VARCHAR(500) NOT NULL DEFAULT '' COMMENT '请求使用的上游URL(新增2026-03)'"},
	})
}

func ensureLogsServiceTierMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "service_tier", definition: "VARCHAR(20) NOT NULL DEFAULT '' COMMENT 'OpenAI service_tier: priority/flex(新增2026-03)'"},
	})
}

func ensureLogsLogSourceMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{{name: "log_source", definition: "VARCHAR(32) NOT NULL DEFAULT 'proxy'"}})
}

// ensureLogsCacheFieldsMySQL 确保logs表有缓存细分字段(MySQL增量迁移,2025-12新增)
func ensureLogsCacheFieldsMySQL(ctx context.Context, db *sql.DB) error {
	// 历史数据回填判断：5m 字段是否已存在决定是否需要回填
	var hasCache5m int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs' AND COLUMN_NAME='cache_5m_input_tokens'",
	).Scan(&hasCache5m); err != nil {
		return fmt.Errorf("check cache_5m_input_tokens existence: %w", err)
	}
	if hasCache5m > 0 {
		return nil
	}

	if err := ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "cache_5m_input_tokens", definition: "INT NOT NULL DEFAULT 0 COMMENT '5分钟缓存写入Token数(新增2025-12)'"},
		{name: "cache_1h_input_tokens", definition: "INT NOT NULL DEFAULT 0 COMMENT '1小时缓存写入Token数(新增2025-12)'"},
	}); err != nil {
		return err
	}

	// 迁移历史数据，将cache_creation_input_tokens复制到cache_5m_input_tokens
	if _, err := db.ExecContext(ctx,
		"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens WHERE cache_5m_input_tokens = 0 AND cache_creation_input_tokens > 0",
	); err != nil {
		return fmt.Errorf("migrate cache_5m data: %w", err)
	}

	return nil
}

func ensureLogsMinuteBucketMySQL(ctx context.Context, db *sql.DB) error {
	// 第一步：添加列（幂等操作）
	if err := ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "minute_bucket", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT 'time/60000，用于RPM类聚合(新增2026-01)'"},
	}); err != nil {
		return err
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

// ensureLogsActualModelMySQL 确保logs表有actual_model字段(MySQL增量迁移)
func ensureLogsActualModelMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "actual_model", definition: "VARCHAR(191) NOT NULL DEFAULT '' COMMENT '实际转发的模型(空表示未重定向)'"},
	})
}

// ensureLogsCostMultiplier 确保logs表有cost_multiplier字段（2026-04新增，写日志时快照渠道倍率）
func ensureLogsCostMultiplier(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "logs", "cost_multiplier",
		"DOUBLE NOT NULL DEFAULT 1",
		"REAL NOT NULL DEFAULT 1")
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
	return ensureMySQLColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
		{name: "cache_read_tokens_total", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '累计缓存读Token数'"},
		{name: "cache_creation_tokens_total", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '累计缓存写Token数'"},
	})
}

// ensureAuthTokensAllowedModels 确保auth_tokens表有allowed_models字段
func ensureAuthTokensAllowedModels(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "auth_tokens", "allowed_models",
		"VARCHAR(2000) NOT NULL DEFAULT ''",
		"TEXT NOT NULL DEFAULT ''")
}

// ensureAuthTokensAllowedChannelIDs 确保auth_tokens表有allowed_channel_ids字段
func ensureAuthTokensAllowedChannelIDs(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "auth_tokens", "allowed_channel_ids",
		"VARCHAR(2000) NOT NULL DEFAULT ''",
		"TEXT NOT NULL DEFAULT ''")
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

// ensureAuthTokensMaxConcurrency 确保auth_tokens表有令牌并发限制字段（2026-04新增）
func ensureAuthTokensMaxConcurrency(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		return ensureMySQLColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "max_concurrency", definition: "INT NOT NULL DEFAULT 0"},
		})
	}

	return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
		{name: "max_concurrency", definition: "INTEGER NOT NULL DEFAULT 0"},
	})
}

func ensureChannelsProtocolTransformMode(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "protocol_transform_mode",
		"VARCHAR(32) NOT NULL DEFAULT 'local'",
		"TEXT NOT NULL DEFAULT 'local'")
}

// ensureChannelsDailyCostLimit 确保channels表有daily_cost_limit字段
func ensureChannelsDailyCostLimit(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "daily_cost_limit",
		"DOUBLE NOT NULL DEFAULT 0",
		"REAL NOT NULL DEFAULT 0")
}

// ensureChannelsCostMultiplier 确保channels表有cost_multiplier字段（2026-04新增，渠道成本倍率）
func ensureChannelsCostMultiplier(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "cost_multiplier",
		"DOUBLE NOT NULL DEFAULT 1",
		"REAL NOT NULL DEFAULT 1")
}

// ensureChannelsScheduledCheckEnabled 确保channels表有scheduled_check_enabled字段
func ensureChannelsScheduledCheckEnabled(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "scheduled_check_enabled",
		"TINYINT NOT NULL DEFAULT 0",
		"INTEGER NOT NULL DEFAULT 0")
}

func ensureChannelsScheduledCheckModel(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "scheduled_check_model",
		"VARCHAR(191) NOT NULL DEFAULT ''",
		"TEXT NOT NULL DEFAULT ''")
}

func ensureChannelsCustomRequestRules(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "custom_request_rules", "TEXT", "TEXT")
}

// migrateChannelsURLToText 将channels.url从VARCHAR(191)扩展为TEXT
// 支持多URL存储（换行分隔）
func migrateChannelsURLToText(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect != DialectMySQL {
		// SQLite: VARCHAR(191) 本质上就是 TEXT，无需变更
		return nil
	}

	// MySQL: 检查当前列类型
	var dataType string
	err := db.QueryRowContext(ctx,
		"SELECT DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='url'",
	).Scan(&dataType)
	if err != nil {
		return fmt.Errorf("check url column type: %w", err)
	}

	if strings.EqualFold(dataType, "text") {
		return nil // 已经是 TEXT
	}

	if _, err := db.ExecContext(ctx,
		"ALTER TABLE channels MODIFY COLUMN url TEXT NOT NULL"); err != nil {
		return fmt.Errorf("modify url column to TEXT: %w", err)
	}
	log.Printf("[MIGRATE] Modified channels.url: VARCHAR → TEXT")
	return nil
}

// ensureAPIKeysAPIKeyLength 修复 api_keys.api_key 列定义漂移（MySQL）
func ensureAPIKeysAPIKeyLength(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect != DialectMySQL {
		return nil
	}

	var (
		dataType   string
		charMaxLen sql.NullInt64
		isNullable string
	)
	err := db.QueryRowContext(ctx, `
		SELECT DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, IS_NULLABLE
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='api_keys' AND COLUMN_NAME='api_key'
	`).Scan(&dataType, &charMaxLen, &isNullable)
	if err != nil {
		return fmt.Errorf("query api_keys.api_key column info: %w", err)
	}

	const targetLen = 255

	needModify := !strings.EqualFold(dataType, "varchar") ||
		!charMaxLen.Valid ||
		charMaxLen.Int64 < targetLen ||
		!strings.EqualFold(isNullable, "NO")
	if !needModify {
		return nil
	}

	if _, err := db.ExecContext(ctx, "ALTER TABLE api_keys MODIFY COLUMN api_key VARCHAR(255) NOT NULL"); err != nil {
		return fmt.Errorf("modify api_keys.api_key column: %w", err)
	}

	currentLen := int64(0)
	if charMaxLen.Valid {
		currentLen = charMaxLen.Int64
	}
	log.Printf(
		"[MIGRATE] Modified api_keys.api_key column: type=%s len=%d nullable=%s -> VARCHAR(255) NOT NULL",
		dataType,
		currentLen,
		isNullable,
	)

	return nil
}

// ensureChannelModelsRedirectField 确保channel_models表有redirect_model字段
func ensureChannelModelsRedirectField(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channel_models", "redirect_model",
		"VARCHAR(191) NOT NULL DEFAULT '' COMMENT '重定向目标模型(空表示不重定向)'",
		"TEXT NOT NULL DEFAULT ''")
}
