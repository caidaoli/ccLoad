//go:build go_json

package storage

import (
	"context"
	"database/sql"
	"testing"

	"ccLoad/internal/storage/schema"

	_ "modernc.org/sqlite"
)

// openTestDB 创建一个干净的 SQLite 内存数据库用于迁移测试
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrate_SQLite_FullFlow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 首次迁移
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	// 验证核心表存在
	tables := []string{"channels", "api_keys", "channel_models", "auth_tokens",
		"system_settings", "admin_sessions", "logs", "schema_migrations"}
	for _, tbl := range tables {
		var name string
		err := db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", tbl, err)
		}
	}

	// 验证 system_settings 已初始化默认值
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM system_settings").Scan(&count); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if count == 0 {
		t.Fatal("expected default settings to be initialized")
	}

	// 验证特定默认设置
	var val string
	if err := db.QueryRowContext(ctx,
		"SELECT value FROM system_settings WHERE key='log_retention_days'",
	).Scan(&val); err != nil {
		t.Fatalf("get log_retention_days: %v", err)
	}
	if val != "7" {
		t.Errorf("log_retention_days=%q, want %q", val, "7")
	}
}

func TestMigrate_SQLite_Idempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 迁移两次应该不报错
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestEnsureChannelsDailyCostLimit_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 列应该已经存在，再次调用应该是 no-op
	if err := ensureChannelsDailyCostLimit(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureChannelsDailyCostLimit: %v", err)
	}

	// 验证列存在
	cols, err := sqliteExistingColumns(ctx, db, "channels")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	if !cols["daily_cost_limit"] {
		t.Fatal("daily_cost_limit column not found in channels")
	}
}

func TestEnsureAuthTokensAllowedModels_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := ensureAuthTokensAllowedModels(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureAuthTokensAllowedModels: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "auth_tokens")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	if !cols["allowed_models"] {
		t.Fatal("allowed_models column not found in auth_tokens")
	}
}

func TestEnsureAuthTokensCostLimit_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := ensureAuthTokensCostLimit(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureAuthTokensCostLimit: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "auth_tokens")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	for _, col := range []string{"cost_used_microusd", "cost_limit_microusd"} {
		if !cols[col] {
			t.Errorf("column %s not found in auth_tokens", col)
		}
	}
}

func TestEnsureChannelModelsRedirectField_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 已存在时应该是 no-op
	if err := ensureChannelModelsRedirectField(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureChannelModelsRedirectField: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "channel_models")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	if !cols["redirect_model"] {
		t.Fatal("redirect_model column not found in channel_models")
	}
}

func TestRelaxDeprecatedChannelFields_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// SQLite 不需要实际操作，应该直接返回 nil
	if err := relaxDeprecatedChannelFields(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("relaxDeprecatedChannelFields: %v", err)
	}
}

func TestNeedChannelModelsMigration_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 迁移前：表不存在，应返回 false
	need, err := needChannelModelsMigration(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("needChannelModelsMigration (pre-migrate): %v", err)
	}
	if need {
		t.Fatal("expected no migration needed before tables exist")
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 新建库：channels 表没有旧的 models 字段，不需要迁移
	need, err = needChannelModelsMigration(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("needChannelModelsMigration (post-migrate): %v", err)
	}
	// 新建数据库的 channels 表不包含废弃的 models 列
	if need {
		t.Fatal("expected no migration needed for fresh database")
	}
}

func TestMigrateModelRedirectsData_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 对于新数据库（没有旧 models 列），迁移应直接返回
	if err := migrateModelRedirectsData(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrateModelRedirectsData: %v", err)
	}
}

func TestMigrateModelRedirectsData_WithLegacyData(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 模拟旧数据库结构：给 channels 添加 models 和 model_redirects 列
	_, err := db.ExecContext(ctx, "ALTER TABLE channels ADD COLUMN models TEXT NOT NULL DEFAULT '[]'")
	if err != nil {
		t.Fatalf("add models column: %v", err)
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE channels ADD COLUMN model_redirects TEXT NOT NULL DEFAULT '{}'")
	if err != nil {
		t.Fatalf("add model_redirects column: %v", err)
	}

	// 插入带旧格式数据的渠道
	_, err = db.ExecContext(ctx, `
		INSERT INTO channels (name, channel_type, url, priority, enabled, models, model_redirects, created_at, updated_at)
		VALUES ('test-ch', 'openai', 'https://api.example.com', 10, 1, '["gpt-4o","gpt-3.5-turbo"]', '{"gpt-3.5-turbo":"gpt-4o-mini"}', unixepoch(), unixepoch())
	`)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// needChannelModelsMigration 应该返回 true
	need, err := needChannelModelsMigration(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("needChannelModelsMigration: %v", err)
	}
	if !need {
		t.Fatal("expected migration needed with legacy models column")
	}

	// 执行数据迁移
	if err := migrateModelRedirectsData(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrateModelRedirectsData: %v", err)
	}

	// 验证 channel_models 表有正确数据
	var cnt int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM channel_models").Scan(&cnt); err != nil {
		t.Fatalf("count channel_models: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("channel_models count=%d, want 2", cnt)
	}

	// 验证 redirect 数据正确
	var redirect string
	if err := db.QueryRowContext(ctx,
		"SELECT redirect_model FROM channel_models WHERE model='gpt-3.5-turbo'",
	).Scan(&redirect); err != nil {
		t.Fatalf("get redirect: %v", err)
	}
	if redirect != "gpt-4o-mini" {
		t.Errorf("redirect=%q, want %q", redirect, "gpt-4o-mini")
	}

	// gpt-4o 不应该有重定向
	if err := db.QueryRowContext(ctx,
		"SELECT redirect_model FROM channel_models WHERE model='gpt-4o'",
	).Scan(&redirect); err != nil {
		t.Fatalf("get redirect for gpt-4o: %v", err)
	}
	if redirect != "" {
		t.Errorf("gpt-4o redirect=%q, want empty", redirect)
	}
}

func TestMigrateChannelModelsSchema_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 再次调用应该跳过（迁移已记录）
	if err := migrateChannelModelsSchema(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrateChannelModelsSchema: %v", err)
	}

	// 验证迁移记录存在
	applied, err := isMigrationApplied(ctx, db, "v1_channel_models_redirect")
	if err != nil {
		t.Fatalf("isMigrationApplied: %v", err)
	}
	if !applied {
		t.Fatal("expected migration to be recorded")
	}
}

func TestInitDefaultSettings_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 验证所有预期的设置项
	expectedKeys := []string{
		"log_retention_days",
		"max_key_retries",
		"upstream_first_byte_timeout",
		"non_stream_timeout",
		"model_fuzzy_match",
		"channel_test_content",
		"channel_stats_range",
		"enable_health_score",
		"success_rate_penalty_weight",
		"health_score_window_minutes",
		"health_score_update_interval",
		"health_min_confident_sample",
		"cooldown_fallback_enabled",
	}

	for _, key := range expectedKeys {
		var val string
		err := db.QueryRowContext(ctx,
			"SELECT value FROM system_settings WHERE key=?", key,
		).Scan(&val)
		if err != nil {
			t.Errorf("setting %q not found: %v", key, err)
		}
	}

	// 验证 idempotent：再次 init 不应报错
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initDefaultSettings (second call): %v", err)
	}
}

func TestInitDefaultSettings_MigratesOldCooldownThreshold(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 手动创建表，但不调用完整的 migrate 来避免默认值插入
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			value_type TEXT NOT NULL DEFAULT 'string',
			description TEXT,
			default_value TEXT,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create system_settings: %v", err)
	}

	// 插入旧版数据：cooldown_fallback_threshold 值为 '5'（非0，应转为 'true'）
	_, err = db.ExecContext(ctx,
		"INSERT INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES ('cooldown_fallback_threshold', '5', 'int', 'old', '3', unixepoch())")
	if err != nil {
		t.Fatalf("insert old setting: %v", err)
	}

	// 执行 initDefaultSettings
	// 注意：INSERT OR IGNORE 会先插入新键（如果不存在），然后迁移逻辑检查旧键是否存在
	// 因为新键已存在（INSERT OR IGNORE 成功），迁移逻辑会删除旧键
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initDefaultSettings: %v", err)
	}

	// 验证新键存在
	var val string
	err = db.QueryRowContext(ctx,
		"SELECT value FROM system_settings WHERE key='cooldown_fallback_enabled'",
	).Scan(&val)
	if err != nil {
		t.Fatalf("get cooldown_fallback_enabled: %v", err)
	}
	// 新键的值来自 INSERT OR IGNORE（默认值 'true'），不是旧键迁移
	if val != "true" {
		t.Errorf("cooldown_fallback_enabled value=%q, want 'true'", val)
	}

	// 旧键应该被删除
	var cnt int
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM system_settings WHERE key='cooldown_fallback_threshold'",
	).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("expected cooldown_fallback_threshold to be removed")
	}
}

func TestInitDefaultSettings_MigratesOldCooldownThreshold_RenameCase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 创建表
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			value_type TEXT NOT NULL DEFAULT 'string',
			description TEXT,
			default_value TEXT,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create system_settings: %v", err)
	}

	// 先插入新键（模拟代码中 INSERT OR IGNORE 的效果）
	_, err = db.ExecContext(ctx,
		"INSERT INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES ('cooldown_fallback_enabled', 'true', 'bool', 'desc', 'true', unixepoch())")
	if err != nil {
		t.Fatalf("insert new setting: %v", err)
	}

	// 然后插入旧键（模拟升级场景）
	_, err = db.ExecContext(ctx,
		"INSERT INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES ('cooldown_fallback_threshold', '0', 'int', 'old', '3', unixepoch())")
	if err != nil {
		t.Fatalf("insert old setting: %v", err)
	}

	// 当新键和旧键都存在时，应该删除旧键
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initDefaultSettings: %v", err)
	}

	// 旧键应该被删除
	var cnt int
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM system_settings WHERE key='cooldown_fallback_threshold'",
	).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("expected cooldown_fallback_threshold to be removed when new key exists")
	}
}

func TestSqliteExistingColumns_InvalidTable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := sqliteExistingColumns(ctx, db, "nonexistent_table")
	if err == nil {
		t.Fatal("expected error for invalid table name")
	}
}

func TestCreateIndex_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 创建索引应该是幂等的（IF NOT EXISTS）
	for _, tb := range []func() *schema.TableBuilder{
		schema.DefineLogsTable,
	} {
		for _, idx := range buildIndexes(tb(), DialectSQLite) {
			if err := createIndex(ctx, db, idx, DialectSQLite); err != nil {
				t.Errorf("createIndex %s: %v", idx.SQL, err)
			}
		}
	}
}

func TestCleanupRemovedSettings_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 插入一个应该被清理的旧设置
	_, err := db.ExecContext(ctx,
		"INSERT OR REPLACE INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES ('model_lookup_strip_date_suffix', 'true', 'bool', 'old', 'true', unixepoch())")
	if err != nil {
		t.Fatalf("insert old setting: %v", err)
	}

	if err := cleanupRemovedSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("cleanupRemovedSettings: %v", err)
	}

	var cnt int
	_ = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM system_settings WHERE key='model_lookup_strip_date_suffix'",
	).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("expected model_lookup_strip_date_suffix to be removed")
	}
}

func TestEnsureLogsNewColumns_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 已有列的情况下再次调用应该是 no-op
	if err := ensureLogsNewColumns(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureLogsNewColumns: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "logs")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	for _, col := range []string{"minute_bucket", "auth_token_id", "client_ip", "actual_model"} {
		if !cols[col] {
			t.Errorf("column %s not found in logs", col)
		}
	}
}

func TestEnsureAuthTokensCacheFields_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 幂等
	if err := ensureAuthTokensCacheFields(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureAuthTokensCacheFields: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "auth_tokens")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	// 这些是由 ensureAuthTokensCacheFields 添加的缓存相关列
	for _, col := range []string{"cache_read_tokens_total", "cache_creation_tokens_total"} {
		if !cols[col] {
			t.Errorf("column %s not found in auth_tokens", col)
		}
	}
}

func TestCreateIndex_MySQL_Syntax(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 创建表
	_, err := db.ExecContext(ctx, `CREATE TABLE idx_test (id INTEGER PRIMARY KEY, val TEXT)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	// MySQL 索引格式（包含 INDEX ... 而不是 CREATE INDEX）
	idx := schema.IndexDef{
		Name: "idx_test_val",
		SQL:  "INDEX idx_test_val (val)",
	}

	// SQLite 不支持这种格式，应该报错或跳过
	// 但 createIndex 会尝试创建，我们主要测试它不会 panic
	_ = createIndex(ctx, db, idx, DialectMySQL)
}

func TestDeleteSystemSetting_NotExists(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 删除不存在的设置应该成功（幂等）
	if err := deleteSystemSetting(ctx, db, DialectSQLite, "nonexistent_key"); err != nil {
		t.Fatalf("deleteSystemSetting: %v", err)
	}
}

func TestHasSystemSetting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 存在的设置
	exists := hasSystemSetting(ctx, db, DialectSQLite, "log_retention_days")
	if !exists {
		t.Fatal("log_retention_days should exist")
	}

	// 不存在的设置
	exists = hasSystemSetting(ctx, db, DialectSQLite, "nonexistent_key")
	if exists {
		t.Fatal("nonexistent_key should not exist")
	}
}

func TestRecordMigration_Idempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 记录同一个迁移两次应该不报错（INSERT OR IGNORE）
	if err := recordMigration(ctx, db, "test_migration", DialectSQLite); err != nil {
		t.Fatalf("first recordMigration: %v", err)
	}
	if err := recordMigration(ctx, db, "test_migration", DialectSQLite); err != nil {
		t.Fatalf("second recordMigration: %v", err)
	}

	// 验证迁移已记录
	applied, err := isMigrationApplied(ctx, db, "test_migration")
	if err != nil {
		t.Fatalf("isMigrationApplied: %v", err)
	}
	if !applied {
		t.Fatal("test_migration should be applied")
	}
}

func TestIsMigrationApplied_NotApplied(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	applied, err := isMigrationApplied(ctx, db, "never_applied_migration")
	if err != nil {
		t.Fatalf("isMigrationApplied: %v", err)
	}
	if applied {
		t.Fatal("never_applied_migration should not be applied")
	}
}
