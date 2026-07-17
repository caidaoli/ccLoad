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
	"api_keys":                    true,
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
		log.Printf("[MIGRATE] 已向 %s 添加列", table)
	}
	return nil
}

// ensurePostgresColumns 通用 PostgreSQL 添加列（幂等）
func ensurePostgresColumns(ctx context.Context, db *sql.DB, table string, cols []mysqlColumnDef) error {
	added := false
	for _, col := range cols {
		def := stripMySQLColumnDecorators(col.definition)
		def = mysqlColDefToPostgres(def)
		var count int
		if err := db.QueryRowContext(ctx,
			rebindIfPostgres(DialectPostgres, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? AND column_name=?"),
			table, col.name,
		).Scan(&count); err != nil {
			return fmt.Errorf("check %s field: %w", col.name, err)
		}
		if count == 0 {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col.name, def)); err != nil {
				return fmt.Errorf("add %s column: %w", col.name, err)
			}
			added = true
		}
	}
	if added {
		log.Printf("[MIGRATE] 已向 %s 添加列 (postgres)", table)
	}
	return nil
}

func stripMySQLColumnDecorators(def string) string {
	// 去掉 COMMENT '...'
	for {
		i := strings.Index(strings.ToUpper(def), " COMMENT ")
		if i < 0 {
			break
		}
		def = strings.TrimSpace(def[:i])
	}
	return def
}

func mysqlColDefToPostgres(def string) string {
	def = strings.ReplaceAll(def, "TINYINT", "SMALLINT")
	def = strings.ReplaceAll(def, "DOUBLE", "DOUBLE PRECISION")
	return def
}

// ensureColumn 跨方言单列幂等添加。
// MySQL 走 INFORMATION_SCHEMA 探测 + ALTER ADD；SQLite 走 PRAGMA table_info + ALTER ADD。
// 调用方各自传入 MySQL/SQLite 列定义子句（不含 ADD COLUMN 关键字）。
func ensureColumn(ctx context.Context, db *sql.DB, dialect Dialect, table, col, mysqlDef, sqliteDef string) error {
	switch dialect {
	case DialectMySQL:
		return ensureMySQLColumns(ctx, db, table, []mysqlColumnDef{{name: col, definition: mysqlDef}})
	case DialectPostgres:
		return ensurePostgresColumns(ctx, db, table, []mysqlColumnDef{{name: col, definition: mysqlDef}})
	default:
		return ensureSQLiteColumns(ctx, db, table, []sqliteColumnDef{{name: col, definition: sqliteDef}})
	}
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
	switch dialect {
	case DialectMySQL:
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
		if err := ensureLogsThinkingEffortMySQL(ctx, db); err != nil {
			return err
		}
		return ensureLogsLogSourceMySQL(ctx, db)
	case DialectPostgres:
		return ensureLogsColumnsPostgres(ctx, db)
	default:
		return ensureLogsColumnsSQLite(ctx, db)
	}
}

// ensureLogsColumnsPostgres PostgreSQL 增量迁移 logs 新字段（新库 DDL 已含全列；此路径服务旧库）
func ensureLogsColumnsPostgres(ctx context.Context, db *sql.DB) error {
	if err := ensurePostgresColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "minute_bucket", definition: "BIGINT NOT NULL DEFAULT 0"},
		{name: "auth_token_id", definition: "BIGINT NOT NULL DEFAULT 0"},
		{name: "client_ip", definition: "VARCHAR(64) NOT NULL DEFAULT ''"},
		{name: "cache_5m_input_tokens", definition: "BIGINT NOT NULL DEFAULT 0"},
		{name: "cache_1h_input_tokens", definition: "BIGINT NOT NULL DEFAULT 0"},
		{name: "actual_model", definition: "VARCHAR(191) NOT NULL DEFAULT ''"},
		{name: "log_source", definition: "VARCHAR(32) NOT NULL DEFAULT 'proxy'"},
		{name: "api_key_hash", definition: "VARCHAR(64) NOT NULL DEFAULT ''"},
		{name: "base_url", definition: "VARCHAR(500) NOT NULL DEFAULT ''"},
		{name: "service_tier", definition: "VARCHAR(32) NOT NULL DEFAULT ''"},
		{name: "thinking_effort", definition: "VARCHAR(32) NOT NULL DEFAULT ''"},
		{name: "reasoning_tokens", definition: "INT NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}
	// 历史回填与 MySQL 同语义，标记记到 schema_migrations
	const cache5mBackfillMarker = "cache_5m_backfill_done"
	if !hasMigration(ctx, db, cache5mBackfillMarker, DialectPostgres) {
		if _, err := db.ExecContext(ctx,
			"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens WHERE cache_5m_input_tokens = 0 AND cache_1h_input_tokens = 0 AND cache_creation_input_tokens > 0",
		); err != nil {
			return fmt.Errorf("migrate cache_5m data: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			"UPDATE logs SET cache_5m_input_tokens = cache_creation_input_tokens - cache_1h_input_tokens WHERE cache_1h_input_tokens > 0 AND cache_5m_input_tokens = cache_creation_input_tokens",
		); err != nil {
			return fmt.Errorf("repair cache_5m data: %w", err)
		}
		if err := recordMigration(ctx, db, cache5mBackfillMarker, DialectPostgres); err != nil {
			return fmt.Errorf("record cache_5m migration marker: %w", err)
		}
	}
	const backfillMarker = "minute_bucket_backfill_done"
	if !hasMigration(ctx, db, backfillMarker, DialectPostgres) {
		if _, err := db.ExecContext(ctx, "UPDATE logs SET minute_bucket = (time / 60000) WHERE minute_bucket = 0 AND time > 0"); err != nil {
			return fmt.Errorf("backfill minute_bucket: %w", err)
		}
		if err := recordMigration(ctx, db, backfillMarker, DialectPostgres); err != nil {
			return fmt.Errorf("record migration marker: %w", err)
		}
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
		{name: "log_source", definition: "TEXT NOT NULL DEFAULT 'proxy'"},
		{name: "api_key_hash", definition: "TEXT NOT NULL DEFAULT ''"}, // API Key SHA256（用于精确定位 key_index）
		{name: "base_url", definition: "TEXT NOT NULL DEFAULT ''"},     // 请求使用的上游URL（多URL场景）
		{name: "service_tier", definition: "TEXT NOT NULL DEFAULT ''"}, // OpenAI service_tier: priority/flex
		{name: "thinking_effort", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "reasoning_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}

	// 第二步：迁移历史数据，将cache_creation_input_tokens复制到cache_5m_input_tokens（一次性）
	const cache5mBackfillMarker = "cache_5m_backfill_done"
	if !hasMigration(ctx, db, cache5mBackfillMarker, DialectSQLite) {
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
	if !hasMigration(ctx, db, backfillMarker, DialectSQLite) {
		log.Println("[migrate] 正在为 SQLite 回填 minute_bucket...")
		if err := backfillLogsMinuteBucketSQLite(ctx, db, 5_000); err != nil {
			return fmt.Errorf("backfill minute_bucket: %w", err)
		}
		if err := recordMigration(ctx, db, backfillMarker, DialectSQLite); err != nil {
			return fmt.Errorf("record migration marker: %w", err)
		}
		log.Println("[migrate] minute_bucket 回填完成")
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
	if !hasMigration(ctx, db, backfillMarker, DialectMySQL) {
		log.Println("[migrate] 正在为 MySQL 回填 minute_bucket...")
		if err := backfillLogsMinuteBucketMySQL(ctx, db, 10_000); err != nil {
			return err
		}
		if err := recordMigration(ctx, db, backfillMarker, DialectMySQL); err != nil {
			return fmt.Errorf("record migration marker: %w", err)
		}
		log.Println("[migrate] minute_bucket 回填完成")
	}
	return nil
}

// ensureLogsActualModelMySQL 确保logs表有actual_model字段(MySQL增量迁移)
func ensureLogsActualModelMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "actual_model", definition: "VARCHAR(191) NOT NULL DEFAULT '' COMMENT '实际转发的模型(空表示未重定向)'"},
	})
}

func ensureLogsThinkingEffortMySQL(ctx context.Context, db *sql.DB) error {
	return ensureMySQLColumns(ctx, db, "logs", []mysqlColumnDef{
		{name: "thinking_effort", definition: "VARCHAR(32) NOT NULL DEFAULT '' COMMENT '请求或上游返回的思考等级'"},
		{name: "reasoning_tokens", definition: "INT NOT NULL DEFAULT 0 COMMENT '思考/推理Token数'"},
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
	switch dialect {
	case DialectMySQL:
		return ensureAuthTokensCacheFieldsMySQL(ctx, db)
	case DialectPostgres:
		return ensurePostgresColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "cache_read_tokens_total", definition: "BIGINT NOT NULL DEFAULT 0"},
			{name: "cache_creation_tokens_total", definition: "BIGINT NOT NULL DEFAULT 0"},
		})
	default:
		return ensureAuthTokensCacheFieldsSQLite(ctx, db)
	}
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
	switch dialect {
	case DialectMySQL:
		return ensureMySQLColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "cost_used_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
			{name: "cost_limit_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
		})
	case DialectPostgres:
		return ensurePostgresColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "cost_used_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
			{name: "cost_limit_microusd", definition: "BIGINT NOT NULL DEFAULT 0"},
		})
	default:
		return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
			{name: "cost_used_microusd", definition: "INTEGER NOT NULL DEFAULT 0"},
			{name: "cost_limit_microusd", definition: "INTEGER NOT NULL DEFAULT 0"},
		})
	}
}

// ensureAuthTokensMaxConcurrency 确保auth_tokens表有令牌并发限制字段（2026-04新增）
func ensureAuthTokensMaxConcurrency(ctx context.Context, db *sql.DB, dialect Dialect) error {
	switch dialect {
	case DialectMySQL:
		return ensureMySQLColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "max_concurrency", definition: "INT NOT NULL DEFAULT 0"},
		})
	case DialectPostgres:
		return ensurePostgresColumns(ctx, db, "auth_tokens", []mysqlColumnDef{
			{name: "max_concurrency", definition: "INT NOT NULL DEFAULT 0"},
		})
	default:
		return ensureSQLiteColumns(ctx, db, "auth_tokens", []sqliteColumnDef{
			{name: "max_concurrency", definition: "INTEGER NOT NULL DEFAULT 0"},
		})
	}
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

// ensureChannelsRPMLimit 确保channels表有rpm_limit字段（0=无限制）。
func ensureChannelsRPMLimit(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "rpm_limit",
		"INT NOT NULL DEFAULT 0",
		"INTEGER NOT NULL DEFAULT 0")
}

// ensureChannelsMaxConcurrency 确保channels表有max_concurrency字段（0=无限制）。
func ensureChannelsMaxConcurrency(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "max_concurrency",
		"INT NOT NULL DEFAULT 0",
		"INTEGER NOT NULL DEFAULT 0")
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

func ensureChannelsProxyURL(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "channels", "proxy_url",
		"VARCHAR(255) NOT NULL DEFAULT ''",
		"TEXT NOT NULL DEFAULT ''")
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
	log.Printf("[MIGRATE] 已修改 channels.url: VARCHAR → TEXT")
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

func ensureAPIKeysDisabled(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "api_keys", "disabled",
		"TINYINT NOT NULL DEFAULT 0",
		"INTEGER NOT NULL DEFAULT 0")
}

func ensureAPIKeysNote(ctx context.Context, db *sql.DB, dialect Dialect) error {
	return ensureColumn(ctx, db, dialect, "api_keys", "note",
		"VARCHAR(512) NOT NULL DEFAULT ''",
		"TEXT NOT NULL DEFAULT ''")
}

// ensureAuthTokensEffectiveCost 确保auth_tokens表有effective_cost_usd字段（2026-07新增）
func ensureAuthTokensEffectiveCost(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if err := ensureColumn(ctx, db, dialect, "auth_tokens", "effective_cost_usd",
		"DOUBLE NOT NULL DEFAULT 0.0",
		"REAL NOT NULL DEFAULT 0.0"); err != nil {
		return err
	}

	// 从 logs 回填历史数据（一次性）
	const marker = "auth_tokens_effective_cost_backfill"
	if hasMigration(ctx, db, marker, dialect) {
		return nil
	}

	// 用 logs 表的 SUM(cost * cost_multiplier) 回填，按 token 聚合
	// 防御性检查：logs 表可能尚未创建（迁移顺序依赖）
	hasLogs := false
	switch dialect {
	case DialectMySQL:
		var count int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='logs'",
		).Scan(&count); err == nil && count > 0 {
			hasLogs = true
		}
	case DialectPostgres:
		var count int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='logs'",
		).Scan(&count); err == nil && count > 0 {
			hasLogs = true
		}
	default:
		var name string
		if err := db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name='logs'",
		).Scan(&name); err == nil {
			hasLogs = true
		}
	}

	if hasLogs {
		_, err := db.ExecContext(ctx, `
			UPDATE auth_tokens SET effective_cost_usd = COALESCE((
				SELECT SUM(COALESCE(l.cost, 0.0) * COALESCE(l.cost_multiplier, 1))
				FROM logs l
				WHERE l.auth_token_id = auth_tokens.id
				  AND l.status_code >= 200 AND l.status_code < 300
			), 0.0)
		`)
		if err != nil {
			return fmt.Errorf("backfill effective_cost_usd: %w", err)
		}
	}

	return recordMigration(ctx, db, marker, dialect)
}
