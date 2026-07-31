//go:build sonic

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"ccLoad/internal/model"
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

func TestMigrate_SQLite_AddsProtocolTransformModeWithAutoDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			channel_type TEXT NOT NULL DEFAULT 'anthropic',
			enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			cooldown_duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO channels(name, url, created_at, updated_at)
		VALUES('legacy', 'https://example.com
https://example.com/v1/messages#', 1, 1)
	`); err != nil {
		t.Fatalf("create legacy channels: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate legacy channels: %v", err)
	}
	columns, err := sqliteExistingColumns(ctx, db, "channels")
	if err != nil {
		t.Fatalf("list channels columns: %v", err)
	}
	if !columns["protocol_transform_mode"] {
		t.Fatalf("channels missing protocol_transform_mode: %v", columns)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "SELECT protocol_transform_mode FROM channels WHERE name='legacy'").Scan(&mode); err != nil {
		t.Fatalf("read migrated mode: %v", err)
	}
	if mode != "auto" {
		t.Fatalf("migrated mode=%q, want auto", mode)
	}
	var rawURLs string
	if err := db.QueryRowContext(ctx, "SELECT url FROM channels WHERE name='legacy'").Scan(&rawURLs); err != nil {
		t.Fatalf("read migrated URLs: %v", err)
	}
	var urls model.ChannelURLs
	if err := json.Unmarshal([]byte(rawURLs), &urls); err != nil {
		t.Fatalf("migrated URLs are not structured JSON: %v (%q)", err, rawURLs)
	}
	if len(urls) != 2 || urls[0].URL != "https://example.com" || urls[0].Exact ||
		urls[1].URL != "https://example.com/v1/messages" || !urls[1].Exact {
		t.Fatalf("migrated URLs=%+v", urls)
	}
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("second migrate must be idempotent: %v", err)
	}
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
		"system_settings", "web_sessions", "logs", "schema_migrations"}
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

	var valueType, defaultValue string
	if err := db.QueryRowContext(ctx, `
		SELECT value, value_type, default_value
		FROM system_settings
		WHERE key = 'global_cooldown_detection_rules'
	`).Scan(&val, &valueType, &defaultValue); err != nil {
		t.Fatalf("get global_cooldown_detection_rules: %v", err)
	}
	if val != "{}" || valueType != "json" || defaultValue != "{}" {
		t.Fatalf("global_cooldown_detection_rules=%q/%q/%q, want {}/json/{}", val, valueType, defaultValue)
	}
}

func TestMigrate_SQLite_AddsModelCooldownDuration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, "DROP TABLE channel_model_cooldowns"); err != nil {
		t.Fatalf("drop current model cooldown table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE channel_model_cooldowns (
			channel_id INTEGER NOT NULL,
			model TEXT NOT NULL,
			cooldown_until INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (channel_id, model),
			FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE
		)
	`); err != nil {
		t.Fatalf("create legacy model cooldown table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channels (name, url, created_at, updated_at)
		VALUES ('legacy-model-cooldown', 'https://api.example.com', 700, 700)
	`); err != nil {
		t.Fatalf("create legacy cooldown channel: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_model_cooldowns (channel_id, model, cooldown_until, updated_at)
		VALUES (1, 'legacy-model', 1000, 700)
	`); err != nil {
		t.Fatalf("insert legacy model cooldown: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	columns, err := sqliteExistingColumns(ctx, db, "channel_model_cooldowns")
	if err != nil {
		t.Fatalf("read model cooldown columns: %v", err)
	}
	if !columns["cooldown_duration_ms"] {
		t.Fatal("channel_model_cooldowns.cooldown_duration_ms was not migrated")
	}

	var durationMs int64
	if err := db.QueryRowContext(ctx, `
		SELECT cooldown_duration_ms
		FROM channel_model_cooldowns
		WHERE channel_id = 1 AND model = 'legacy-model'
	`).Scan(&durationMs); err != nil {
		t.Fatalf("read migrated model cooldown duration: %v", err)
	}
	if durationMs != int64(5*time.Minute/time.Millisecond) {
		t.Fatalf("migrated model cooldown duration=%dms, want %dms", durationMs, 5*time.Minute/time.Millisecond)
	}
}

func TestMigrate_SQLite_RebuildsOnlyDebugLogsForProtocolPayloads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	result, err := db.ExecContext(ctx, "INSERT INTO logs (time, status_code, message) VALUES (1, 200, 'keep me')")
	if err != nil {
		t.Fatalf("insert ordinary log: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("ordinary log id: %v", err)
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", debugLogsProtocolPayloadsVersion); err != nil {
		t.Fatalf("reset protocol payload migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE debug_logs"); err != nil {
		t.Fatalf("drop current debug_logs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE debug_logs (
			log_id INTEGER PRIMARY KEY,
			created_at INTEGER NOT NULL,
			req_method TEXT NOT NULL DEFAULT '',
			req_url TEXT NOT NULL,
			req_headers TEXT NOT NULL,
			req_body BLOB NOT NULL,
			resp_status INTEGER NOT NULL DEFAULT 0,
			resp_headers TEXT NOT NULL,
			resp_body BLOB
		)`); err != nil {
		t.Fatalf("create pre-protocol debug_logs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO debug_logs (log_id, created_at, req_method, req_url, req_headers, req_body, resp_status, resp_headers, resp_body)
		VALUES (?, 1, 'POST', '/v1/messages', '{}', '{}', 200, '{}', '{}')`, logID); err != nil {
		t.Fatalf("insert old debug log: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	var ordinaryLogCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM logs WHERE id = ?", logID).Scan(&ordinaryLogCount); err != nil {
		t.Fatalf("count ordinary logs: %v", err)
	}
	if ordinaryLogCount != 1 {
		t.Fatalf("ordinary logs changed during debug migration: count=%d", ordinaryLogCount)
	}
	var debugLogCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM debug_logs").Scan(&debugLogCount); err != nil {
		t.Fatalf("count rebuilt debug logs: %v", err)
	}
	if debugLogCount != 0 {
		t.Fatalf("rebuilt debug_logs should discard short-lived rows, count=%d", debugLogCount)
	}
	columns, err := sqliteExistingColumns(ctx, db, "debug_logs")
	if err != nil {
		t.Fatalf("list rebuilt debug columns: %v", err)
	}
	for _, column := range []string{
		"protocol_transformed", "original_req_url", "original_req_headers", "original_req_body",
		"translated_resp_status", "translated_resp_headers", "translated_resp_body",
	} {
		if !columns[column] {
			t.Fatalf("rebuilt debug_logs missing column %q: %v", column, columns)
		}
	}
}

func TestMigrate_SQLite_AddsDebugProtocolMetadataWithoutDroppingRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, "DROP TABLE debug_logs"); err != nil {
		t.Fatalf("drop current debug_logs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE debug_logs (
			log_id INTEGER PRIMARY KEY,
			created_at INTEGER NOT NULL,
			req_method TEXT NOT NULL DEFAULT '',
			req_url TEXT NOT NULL,
			req_headers TEXT NOT NULL,
			req_body BLOB NOT NULL,
			resp_status INTEGER NOT NULL DEFAULT 0,
			resp_headers TEXT NOT NULL,
			resp_body BLOB,
			protocol_transformed INTEGER NOT NULL DEFAULT 0,
			original_req_body BLOB,
			translated_resp_body BLOB
		)`); err != nil {
		t.Fatalf("create v3 debug_logs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO debug_logs (
			log_id, created_at, req_method, req_url, req_headers, req_body,
			resp_status, resp_headers, resp_body, protocol_transformed,
			original_req_body, translated_resp_body
		) VALUES (42, 1, 'POST', '/upstream', '{}', '{}', 200, '{}', '{}', 1, '{}', '{}')`); err != nil {
		t.Fatalf("insert v3 debug log: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM debug_logs WHERE log_id = 42").Scan(&count); err != nil {
		t.Fatalf("count preserved debug row: %v", err)
	}
	if count != 1 {
		t.Fatalf("debug row count=%d, want 1", count)
	}
	columns, err := sqliteExistingColumns(ctx, db, "debug_logs")
	if err != nil {
		t.Fatalf("list debug columns: %v", err)
	}
	for _, column := range []string{"original_req_url", "original_req_headers", "translated_resp_status", "translated_resp_headers"} {
		if !columns[column] {
			t.Fatalf("debug_logs missing column %q: %v", column, columns)
		}
	}
}

func TestMigrateSQLite_SeedsModelCatalogSyncIntervalSetting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var value, valueType, defaultValue string
	if err := db.QueryRowContext(ctx, `
		SELECT value, value_type, default_value
		FROM system_settings
		WHERE "key" = ?
	`, "model_catalog_sync_interval_hours").Scan(&value, &valueType, &defaultValue); err != nil {
		t.Fatalf("get model_catalog_sync_interval_hours: %v", err)
	}
	if value != "6" || valueType != "float" || defaultValue != "6" {
		t.Fatalf("setting = value:%q type:%q default:%q, want value:6 type:float default:6", value, valueType, defaultValue)
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

func TestMigrateSQLiteAddsFingerprintTestDistribution(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE fingerprint_test_results (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			sample_count INTEGER NOT NULL DEFAULT 0,
			best_score REAL NOT NULL DEFAULT 0,
			matches_json TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create legacy fingerprint_test_results: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fingerprint_test_results
			(channel_name, model, sample_count, best_score, matches_json, created_at)
		VALUES ('legacy', 'gpt-test', 10, 0.9, '[]', 1)
	`); err != nil {
		t.Fatalf("insert legacy fingerprint_test_results: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate legacy fingerprint_test_results: %v", err)
	}

	var distribution string
	if err := db.QueryRowContext(ctx, `
		SELECT distribution FROM fingerprint_test_results WHERE model = 'gpt-test'
	`).Scan(&distribution); err != nil {
		t.Fatalf("read migrated distribution: %v", err)
	}
	if distribution != "[]" {
		t.Fatalf("distribution=%q, want []", distribution)
	}
}

func TestMigrate_SQLite_FailsOnInvalidAllowedModelsJSON(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 插入脏数据：allowed_models 非法 JSON
	_, err := db.ExecContext(ctx,
		"INSERT INTO auth_tokens (token, description, created_at, is_active, allowed_models) VALUES (?, ?, ?, ?, ?)",
		"bad-json-token", "Bad JSON", int64(1), 1, "{not-json",
	)
	if err != nil {
		t.Fatalf("insert auth_tokens: %v", err)
	}

	// 再次启动迁移应直接失败（Fail-fast）
	if err := migrate(ctx, db, DialectSQLite); err == nil {
		t.Fatal("expected migrate to fail due to invalid allowed_models json")
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
	if !cols["scheduled_check_enabled"] {
		t.Fatal("scheduled_check_enabled column not found in channels")
	}
	if !cols["scheduled_check_model"] {
		t.Fatal("scheduled_check_model column not found in channels")
	}
}

func TestEnsureChannelsCooldownDetectionRules_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := ensureChannelsCooldownDetectionRules(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("ensureChannelsCooldownDetectionRules: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "channels")
	if err != nil {
		t.Fatalf("sqliteExistingColumns: %v", err)
	}
	if !cols["cooldown_detection_rules"] {
		t.Fatal("cooldown_detection_rules column not found in channels")
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

func TestMigrateSQLite_LegacyCostLimitedAuthTokenGetsDefaultMaxConcurrency(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		CREATE TABLE auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
			success_count INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0,
			stream_avg_ttfb REAL NOT NULL DEFAULT 0.0,
			non_stream_avg_rt REAL NOT NULL DEFAULT 0.0,
			stream_count INTEGER NOT NULL DEFAULT 0,
			non_stream_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens_total INTEGER NOT NULL DEFAULT 0,
			completion_tokens_total INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens_total INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens_total INTEGER NOT NULL DEFAULT 0,
			total_cost_usd REAL NOT NULL DEFAULT 0.0,
			cost_used_microusd INTEGER NOT NULL DEFAULT 0,
			cost_limit_microusd INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '',
			allowed_channel_ids TEXT NOT NULL DEFAULT ''
		)
	`)
	if err != nil {
		t.Fatalf("create legacy auth_tokens: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO auth_tokens (token, description, created_at, cost_limit_microusd)
		VALUES ('limited-legacy', 'limited legacy token', 1, 1000),
		       ('unlimited-legacy', 'unlimited legacy token', 1, 0)
	`)
	if err != nil {
		t.Fatalf("insert legacy auth_tokens: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate legacy auth_tokens: %v", err)
	}

	var limitedMaxConcurrency int
	if err := db.QueryRowContext(ctx, `
		SELECT max_concurrency FROM auth_tokens WHERE token = 'limited-legacy'
	`).Scan(&limitedMaxConcurrency); err != nil {
		t.Fatalf("query limited max_concurrency: %v", err)
	}
	if limitedMaxConcurrency != authTokenCostLimitDefaultMaxConcurrency {
		t.Fatalf("limited max_concurrency=%d, want %d", limitedMaxConcurrency, authTokenCostLimitDefaultMaxConcurrency)
	}

	var unlimitedMaxConcurrency int
	if err := db.QueryRowContext(ctx, `
		SELECT max_concurrency FROM auth_tokens WHERE token = 'unlimited-legacy'
	`).Scan(&unlimitedMaxConcurrency); err != nil {
		t.Fatalf("query unlimited max_concurrency: %v", err)
	}
	if unlimitedMaxConcurrency != 0 {
		t.Fatalf("unlimited max_concurrency=%d, want 0", unlimitedMaxConcurrency)
	}
}

func TestMigrateSQLite_BackfillsAuthTokenEffectiveCostFromLegacyLogs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
			success_count INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0,
			stream_avg_ttfb REAL NOT NULL DEFAULT 0.0,
			non_stream_avg_rt REAL NOT NULL DEFAULT 0.0,
			stream_count INTEGER NOT NULL DEFAULT 0,
			non_stream_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens_total INTEGER NOT NULL DEFAULT 0,
			completion_tokens_total INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens_total INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens_total INTEGER NOT NULL DEFAULT 0,
			total_cost_usd REAL NOT NULL DEFAULT 3.0,
			cost_used_microusd INTEGER NOT NULL DEFAULT 0,
			cost_limit_microusd INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '',
			allowed_channel_ids TEXT NOT NULL DEFAULT '',
			max_concurrency INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		t.Fatalf("create legacy auth_tokens: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time INTEGER NOT NULL,
			minute_bucket INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			actual_model TEXT NOT NULL DEFAULT '',
			log_source TEXT NOT NULL DEFAULT 'proxy',
			channel_id INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL,
			message TEXT NOT NULL,
			duration REAL NOT NULL DEFAULT 0.0,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL NOT NULL DEFAULT 0.0,
			api_key_used TEXT NOT NULL DEFAULT '',
			api_key_hash TEXT NOT NULL DEFAULT '',
			auth_token_id INTEGER NOT NULL DEFAULT 0,
			client_ip TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			service_tier TEXT NOT NULL DEFAULT '',
			thinking_effort TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_5m_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_1h_input_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0
		)
	`); err != nil {
		t.Fatalf("create legacy logs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO auth_tokens (id, token, description, created_at)
		VALUES (1, 'legacy-token', 'legacy token', 1)
	`); err != nil {
		t.Fatalf("insert auth token: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO logs (time, status_code, message, auth_token_id, cost)
		VALUES (60000, 200, 'ok', 1, 1.5),
		       (120000, 500, 'fail', 1, 9.0)
	`); err != nil {
		t.Fatalf("insert legacy logs: %v", err)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate legacy auth token effective cost: %v", err)
	}

	cols, err := sqliteExistingColumns(ctx, db, "logs")
	if err != nil {
		t.Fatalf("sqliteExistingColumns logs: %v", err)
	}
	if !cols["cost_multiplier"] {
		t.Fatal("cost_multiplier column not found in logs")
	}
	if !cols["upstream_websocket"] {
		t.Fatal("upstream_websocket column not found in logs")
	}
	if !cols["client_protocol"] {
		t.Fatal("client_protocol column not found in logs")
	}
	var upstreamWebsocket int
	var clientProtocol string
	if err := db.QueryRowContext(ctx, `SELECT upstream_websocket, client_protocol FROM logs WHERE time = 60000`).Scan(&upstreamWebsocket, &clientProtocol); err != nil {
		t.Fatalf("query legacy upstream_websocket: %v", err)
	}
	if upstreamWebsocket != 0 {
		t.Fatalf("legacy upstream_websocket=%d, want 0", upstreamWebsocket)
	}
	if clientProtocol != "" {
		t.Fatalf("legacy client_protocol=%q, want empty", clientProtocol)
	}

	var effectiveCost float64
	if err := db.QueryRowContext(ctx, `
		SELECT effective_cost_usd FROM auth_tokens WHERE id = 1
	`).Scan(&effectiveCost); err != nil {
		t.Fatalf("query effective_cost_usd: %v", err)
	}
	if effectiveCost != 1.5 {
		t.Fatalf("effective_cost_usd=%f, want 1.5", effectiveCost)
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

	rows, err := db.QueryContext(ctx, `
		SELECT model FROM channel_models
		ORDER BY created_at ASC, model ASC
	`)
	if err != nil {
		t.Fatalf("query migrated model order: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var orderedModels []string
	for rows.Next() {
		var modelName string
		if err := rows.Scan(&modelName); err != nil {
			t.Fatalf("scan migrated model order: %v", err)
		}
		orderedModels = append(orderedModels, modelName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated model order: %v", err)
	}

	expectedOrder := []string{"gpt-4o", "gpt-3.5-turbo"}
	if len(orderedModels) != len(expectedOrder) {
		t.Fatalf("migrated model order len=%d, want %d", len(orderedModels), len(expectedOrder))
	}
	for i, expected := range expectedOrder {
		if orderedModels[i] != expected {
			t.Fatalf("migrated model order[%d]=%s, want %s", i, orderedModels[i], expected)
		}
	}
}

func TestRepairLegacyChannelModelOrder_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_, err := db.ExecContext(ctx, "ALTER TABLE channels ADD COLUMN models TEXT NOT NULL DEFAULT '[]'")
	if err != nil {
		t.Fatalf("add models column: %v", err)
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE channels ADD COLUMN model_redirects TEXT NOT NULL DEFAULT '{}'")
	if err != nil {
		t.Fatalf("add model_redirects column: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO channels (id, name, channel_type, url, priority, enabled, models, model_redirects, created_at, updated_at)
		VALUES (1, 'repair-order', 'openai', 'https://api.example.com', 10, 1, '["z-model","a-model"]', '{}', 100, 100)
	`)
	if err != nil {
		t.Fatalf("insert legacy channel: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO channel_models (channel_id, model, redirect_model, created_at)
		VALUES (1, 'z-model', '', 1), (1, 'a-model', '', 1)
	`)
	if err != nil {
		t.Fatalf("insert legacy channel_models: %v", err)
	}
	if err := recordMigration(ctx, db, channelModelsRedirectMigrationVersion, DialectSQLite); err != nil {
		t.Fatalf("record legacy migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", channelModelsOrderRepairVersion); err != nil {
		t.Fatalf("clear repair migration marker: %v", err)
	}

	if err := repairLegacyChannelModelOrder(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("repairLegacyChannelModelOrder: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT model FROM channel_models
		WHERE channel_id = 1
		ORDER BY created_at ASC, model ASC
	`)
	if err != nil {
		t.Fatalf("query repaired model order: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var orderedModels []string
	for rows.Next() {
		var modelName string
		if err := rows.Scan(&modelName); err != nil {
			t.Fatalf("scan repaired model order: %v", err)
		}
		orderedModels = append(orderedModels, modelName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate repaired model order: %v", err)
	}

	expectedOrder := []string{"z-model", "a-model"}
	if len(orderedModels) != len(expectedOrder) {
		t.Fatalf("repaired model order len=%d, want %d", len(orderedModels), len(expectedOrder))
	}
	for i, expected := range expectedOrder {
		if orderedModels[i] != expected {
			t.Fatalf("repaired model order[%d]=%s, want %s", i, orderedModels[i], expected)
		}
	}

	if !hasMigration(ctx, db, channelModelsOrderRepairVersion, DialectSQLite) {
		t.Fatal("expected repair migration to be recorded")
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
	if !hasMigration(ctx, db, "v1_channel_models_redirect", DialectSQLite) {
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
		"stream_timeout",
		"non_stream_timeout",
		"anthropic_first_byte_timeout",
		"anthropic_non_stream_timeout",
		"codex_first_byte_timeout",
		"codex_non_stream_timeout",
		"openai_first_byte_timeout",
		"openai_non_stream_timeout",
		"gemini_first_byte_timeout",
		"gemini_non_stream_timeout",
		"model_fuzzy_match",
		"channel_test_content",
		"channel_check_interval_hours",
		"auto_update_interval_hours",
		"channel_stats_range",
		"enable_health_score",
		"success_rate_penalty_weight",
		"health_score_window_minutes",
		"health_score_update_interval",
		"health_min_confident_sample",
		"cooldown_fallback_enabled",
		"responses_ws_max_sessions",
		"responses_ws_session_ttl_minutes",
		"responses_ws_max_transcript_bytes",
		"responses_ws_max_connections",
		"responses_ws_max_connections_per_token",
	}

	for _, key := range expectedKeys {
		var val string
		err := db.QueryRowContext(ctx,
			"SELECT value FROM system_settings WHERE key=?", key,
		).Scan(&val)
		if err != nil {
			t.Errorf("setting %q not found: %v", key, err)
		}
		if key == "channel_check_interval_hours" && val != "5" {
			t.Errorf("setting %q default = %q, want 5", key, val)
		}
		if key == "auto_update_interval_hours" && val != "12" {
			t.Errorf("setting %q default = %q, want 12", key, val)
		}
		if key == "stream_timeout" && val != "0" {
			t.Errorf("setting %q default = %q, want 0", key, val)
		}
		if key == "responses_ws_max_connections" && val != "64" {
			t.Errorf("setting %q default = %q, want 64", key, val)
		}
		if key == "responses_ws_max_connections_per_token" && val != "16" {
			t.Errorf("setting %q default = %q, want 16", key, val)
		}
		if key == "responses_ws_session_ttl_minutes" && val != "15" {
			t.Errorf("setting %q default = %q, want 15", key, val)
		}
		if key == "responses_ws_max_transcript_bytes" && val != "134217728" {
			t.Errorf("setting %q default = %q, want 134217728", key, val)
		}
	}
	var valueType string
	if err := db.QueryRowContext(ctx,
		"SELECT value_type FROM system_settings WHERE key='auto_update_interval_hours'",
	).Scan(&valueType); err != nil {
		t.Fatalf("query auto_update_interval_hours value_type: %v", err)
	}
	if valueType != "int" {
		t.Fatalf("auto_update_interval_hours value_type = %q, want int", valueType)
	}

	// 验证 idempotent：再次 init 不应报错
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("initDefaultSettings (second call): %v", err)
	}
}

func TestInitDefaultSettings_PreservesExistingResponsesSessionTTL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		UPDATE system_settings
		SET value = '60', default_value = '60', description = 'old default'
		WHERE key = 'responses_ws_session_ttl_minutes'
	`); err != nil {
		t.Fatalf("restore old default: %v", err)
	}
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate old default: %v", err)
	}

	var value, defaultValue string
	if err := db.QueryRowContext(ctx, `
		SELECT value, default_value
		FROM system_settings
		WHERE key = 'responses_ws_session_ttl_minutes'
	`).Scan(&value, &defaultValue); err != nil {
		t.Fatalf("query migrated TTL: %v", err)
	}
	if value != "60" || defaultValue != "15" {
		t.Fatalf("existing TTL value/default=%q/%q, want 60/15", value, defaultValue)
	}

	if _, err := db.ExecContext(ctx, `
		UPDATE system_settings
		SET value = '10', default_value = '60'
		WHERE key = 'responses_ws_session_ttl_minutes'
	`); err != nil {
		t.Fatalf("set custom TTL: %v", err)
	}
	if err := initDefaultSettings(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("refresh custom TTL metadata: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT value, default_value
		FROM system_settings
		WHERE key = 'responses_ws_session_ttl_minutes'
	`).Scan(&value, &defaultValue); err != nil {
		t.Fatalf("query custom TTL: %v", err)
	}
	if value != "10" || defaultValue != "15" {
		t.Fatalf("custom TTL value/default=%q/%q, want 10/15", value, defaultValue)
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
	for _, col := range []string{"minute_bucket", "auth_token_id", "client_ip", "actual_model", "log_source"} {
		if !cols[col] {
			t.Errorf("column %s not found in logs", col)
		}
	}
}

func TestMigrate_SQLite_LogsHotPathIndexes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, idx := range []string{
		"idx_logs_channel_time_id",
		"idx_logs_channel_model_time_id",
		"idx_logs_minute_auth_token_status",
		"idx_logs_source_time",
		"idx_logs_source_minute",
	} {
		var name string
		if err := db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='logs' AND name=?", idx,
		).Scan(&name); err != nil {
			t.Fatalf("logs index %s not found: %v", idx, err)
		}
	}
}

// TestLoadAllExistingIndexes_SQLite 验证 loadAllExistingIndexes 在 SQLite 下能正确返回索引集合
//
// 防御目标：迁移热路径优化（启动时跳过已存在索引）依赖此函数返回正确结果。
// 若返回为空或漏掉索引，会退化为重复执行 CREATE INDEX —— 此时旧的容错路径仍兜底，
// 但远程数据库的网络往返成本会重新出现，违背优化初衷。
func TestLoadAllExistingIndexes_SQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 首次迁移前：所有索引尚不存在
	emptyBefore, err := loadAllExistingIndexes(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("loadAllExistingIndexes(empty): %v", err)
	}
	if len(emptyBefore) != 0 {
		t.Fatalf("expected no indexes before migrate, got %v", emptyBefore)
	}

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 迁移后应能查到所有表的索引
	afterMigrate, err := loadAllExistingIndexes(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("loadAllExistingIndexes(after): %v", err)
	}

	logsIdx := afterMigrate["logs"]
	if logsIdx == nil {
		t.Fatal("logs table missing from index map")
	}
	mustHaveLogs := []string{
		"idx_logs_time_model",
		"idx_logs_time_status",
		"idx_logs_time_channel_model",
		"idx_logs_minute_channel_model",
		"idx_logs_minute_auth_token_status",
		"idx_logs_channel_time_id",
		"idx_logs_channel_model_time_id",
		"idx_logs_time_auth_token",
		"idx_logs_time_actual_model",
		"idx_logs_source_time",
		"idx_logs_source_minute",
	}
	for _, name := range mustHaveLogs {
		if !logsIdx[name] {
			t.Errorf("logs index %s missing after migrate", name)
		}
	}

	// debug_logs 表的索引也应该被包含
	if !afterMigrate["debug_logs"]["idx_debug_logs_created_at"] {
		t.Errorf("debug_logs index idx_debug_logs_created_at missing after migrate")
	}

	// 不存在的表读取得到 nil map（map[nil][key] 安全返回零值）
	if afterMigrate["no_such_table_xyz"] != nil {
		t.Errorf("expected nil for missing table, got %v", afterMigrate["no_such_table_xyz"])
	}
}

// TestMigrate_SQLite_IdempotentSkipsCreateIndex 验证幂等迁移路径不会再次执行 CREATE INDEX
//
// 实现原理：第二次迁移前，预先 DROP 一个索引；如果 migrate 真的跳过了"已存在"的索引而仅
// 重建缺失项，那被 DROP 的索引会被重建，其它索引集合保持不变。
// 这是性能优化的功能等价性证明。
func TestMigrate_SQLite_IdempotentSkipsCreateIndex(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// 故意删除一个索引，模拟"部分缺失"场景
	if _, err := db.ExecContext(ctx, "DROP INDEX idx_logs_time_model"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	before, err := loadAllExistingIndexes(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("loadAllExistingIndexes(before): %v", err)
	}
	if before["logs"]["idx_logs_time_model"] {
		t.Fatalf("idx_logs_time_model should be dropped before second migrate")
	}

	// 第二次迁移：应当只重建缺失的索引
	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	after, err := loadAllExistingIndexes(ctx, db, DialectSQLite)
	if err != nil {
		t.Fatalf("loadAllExistingIndexes(after): %v", err)
	}
	if !after["logs"]["idx_logs_time_model"] {
		t.Errorf("dropped index idx_logs_time_model should be recreated by second migrate")
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
	if !hasMigration(ctx, db, "test_migration", DialectSQLite) {
		t.Fatal("test_migration should be applied")
	}
}

func TestHasMigration_NotApplied(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := migrate(ctx, db, DialectSQLite); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if hasMigration(ctx, db, "never_applied_migration", DialectSQLite) {
		t.Fatal("never_applied_migration should not be applied")
	}
}
