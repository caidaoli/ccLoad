//go:build mysql_integration

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"

	_ "github.com/go-sql-driver/mysql"
)

// ============================================================================
// MySQL 迁移条件化测试
// 运行条件：go test -tags "sonic mysql_integration" ./internal/storage/... -v -run TestMySQL
//
// 依赖环境：
// - Docker 已安装
// - 或设置 CCLOAD_TEST_MYSQL_DSN 环境变量指向现有 MySQL 实例
//
// 示例：
//   # 使用现有 MySQL
//   CCLOAD_TEST_MYSQL_DSN="root:test@tcp(127.0.0.1:3306)/ccload_test?parseTime=true" \
//       go test -tags "sonic mysql_integration" ./internal/storage/... -v -run TestMySQL
//
//   # 自动使用 Docker（无 DSN 环境变量时）
//   go test -tags "sonic mysql_integration" ./internal/storage/... -v -run TestMySQL
// ============================================================================

const (
	testMySQLImage    = "mysql:8.0"
	testMySQLRootPass = "testroot"
	testMySQLDB       = "ccload_test"
)

// mysqlTestEnv 管理测试用 MySQL 环境
type mysqlTestEnv struct {
	dsn         string
	containerID string
	db          *sql.DB
}

// setupMySQLEnv 创建 MySQL 测试环境
// 优先使用 CCLOAD_TEST_MYSQL_DSN 环境变量，否则启动 Docker 容器
func setupMySQLEnv(t *testing.T) *mysqlTestEnv {
	t.Helper()

	if dsn := os.Getenv("CCLOAD_TEST_MYSQL_DSN"); dsn != "" {
		t.Logf("使用环境变量提供的 MySQL DSN")
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("连接 MySQL 失败: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Ping(); err != nil {
			t.Fatalf("MySQL ping 失败: %v", err)
		}
		return &mysqlTestEnv{dsn: dsn, db: db}
	}

	return startDockerMySQL(t)
}

// TestMySQLRepeatableReadSnapshotCompatibility 可安全指向远程实例：只验证恢复所需的
// REPEATABLE READ 事务能力，不创建、修改或删除任何对象。
func TestMySQLRepeatableReadSnapshotCompatibility(t *testing.T) {
	dsn := os.Getenv("CCLOAD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CCLOAD_TEST_MYSQL_DSN 未设置")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatalf("begin repeatable-read transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query in snapshot transaction: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}

// BenchmarkMySQLReadOnlyRoundTrip 测量远程主库最小查询往返；不触碰业务表。
func BenchmarkMySQLReadOnlyRoundTrip(b *testing.B) {
	dsn := os.Getenv("CCLOAD_TEST_MYSQL_DSN")
	if dsn == "" {
		b.Skip("CCLOAD_TEST_MYSQL_DSN 未设置")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		b.Fatalf("open MySQL: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		b.Fatalf("ping MySQL: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			b.Fatalf("SELECT 1: %v", err)
		}
	}
}

// startDockerMySQL 启动 Docker MySQL 容器
func startDockerMySQL(t *testing.T) *mysqlTestEnv {
	t.Helper()

	// 检查 Docker 是否可用
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("Docker 不可用，跳过 MySQL 集成测试")
	}

	containerName := fmt.Sprintf("ccload-mysql-test-%d", time.Now().UnixNano())

	// 启动 MySQL 容器
	args := []string{
		"run", "-d",
		"--name", containerName,
		"-e", "MYSQL_ROOT_PASSWORD=" + testMySQLRootPass,
		"-e", "MYSQL_DATABASE=" + testMySQLDB,
		// 随机挑选空闲端口，避免与并行测试/本机服务冲突
		"-p", "127.0.0.1::3306",
		testMySQLImage,
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("启动 MySQL 容器失败: %v\n%s", err, out)
	}
	containerID := lastNonEmptyLine(string(out))
	if len(containerID) < 12 {
		t.Fatalf("无法解析容器 ID，docker 输出:\n%s", out)
	}
	t.Logf("启动 MySQL 容器: %s", containerID[:12])

	hostPort := dockerMappedHostPort(t, containerID, "3306/tcp")
	t.Logf("MySQL 端口映射: 127.0.0.1:%s -> 3306", hostPort)

	// 注册清理（在顶层测试结束时执行）
	t.Cleanup(func() {
		t.Logf("停止并删除 MySQL 容器: %s", containerID[:12])
		_ = exec.Command("docker", "stop", containerID).Run()
		_ = exec.Command("docker", "rm", containerID).Run()
	})

	// 等待 MySQL 就绪
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:%s)/%s?parseTime=true&multiStatements=true",
		testMySQLRootPass, hostPort, testMySQLDB)

	var db *sql.DB
	for i := range 30 {
		time.Sleep(time.Second)
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			continue
		}
		if err := db.Ping(); err == nil {
			t.Logf("MySQL 就绪（等待 %d 秒）", i+1)
			t.Cleanup(func() { _ = db.Close() })
			return &mysqlTestEnv{dsn: dsn, containerID: containerID, db: db}
		}
		_ = db.Close()
	}

	t.Fatalf("MySQL 容器启动超时（30秒）")
	return nil
}

// cleanupMySQLTables 清理所有表（用于测试前重置）
func cleanupMySQLTables(t *testing.T, db *sql.DB) {
	t.Helper()

	// 禁用外键检查
	_, _ = db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	defer func() { _, _ = db.Exec("SET FOREIGN_KEY_CHECKS = 1") }()

	tables := []string{"debug_logs", "logs", "web_sessions", "admin_sessions", "system_settings", "auth_tokens", "channel_model_cooldowns", "channel_url_states", "channel_models", "channel_protocol_transforms", "api_keys", "channels", "schema_migrations"}
	for _, table := range tables {
		_, _ = db.Exec("DROP TABLE IF EXISTS " + table)
	}
}

// ============================================================================
// MySQL 迁移测试套件
// 使用顶层测试函数包裹子测试，确保容器生命周期正确管理
// ============================================================================

func TestMySQL(t *testing.T) {
	env := setupMySQLEnv(t)

	// 子测试共享同一个容器
	t.Run("FullMigration", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("CreateMySQLStore 失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		// 验证关键表存在
		tables := []string{"channels", "api_keys", "channel_models", "auth_tokens", "logs", "system_settings", "web_sessions"}
		for _, table := range tables {
			var count int
			err := env.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
			if err != nil {
				t.Fatalf("表 %s 查询失败: %v", table, err)
			}
			t.Logf("表 %s 存在（行数: %d）", table, count)
		}
	})

	t.Run("SyncManagerLargeRestore", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)
		primaryStore, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("CreateMySQLStore: %v", err)
		}
		defer func() { _ = primaryStore.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		channel, err := primaryStore.CreateConfig(ctx, &model.Config{
			Name: "large-restore", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			if _, cleanupErr := env.db.ExecContext(cleanupCtx, "DELETE FROM channels WHERE id = ?", channel.ID); cleanupErr != nil {
				t.Errorf("cleanup large restore fixture: %v", cleanupErr)
			}
		})
		const rowCount = 10_001
		const batchSize = 500
		for start := 0; start < rowCount; start += batchSize {
			end := min(start+batchSize, rowCount)
			placeholders := make([]string, 0, end-start)
			args := make([]any, 0, (end-start)*5)
			for i := start; i < end; i++ {
				placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
				args = append(args, channel.ID, fmt.Sprintf("model-%05d", i), "", 0, time.Now().UnixMilli())
			}
			query := "INSERT INTO channel_models (channel_id, model, redirect_model, disabled, created_at) VALUES " + strings.Join(placeholders, ",")
			if _, err := env.db.ExecContext(ctx, query, args...); err != nil {
				t.Fatalf("seed channel_models [%d,%d): %v", start, end, err)
			}
		}

		sqliteStore, err := CreateSQLiteStore(t.TempDir() + "/restore.db")
		if err != nil {
			t.Fatalf("CreateSQLiteStore: %v", err)
		}
		defer func() { _ = sqliteStore.Close() }()
		primarySQL, ok := primaryStore.(*sqlstore.SQLStore)
		if !ok {
			t.Fatalf("primary store type %T", primaryStore)
		}
		sqliteSQL, ok := sqliteStore.(*sqlstore.SQLStore)
		if !ok {
			t.Fatalf("sqlite store type %T", sqliteStore)
		}
		if err := NewSyncManager(primarySQL, sqliteSQL).RestoreOnStartup(ctx, 0); err != nil {
			t.Fatalf("RestoreOnStartup: %v", err)
		}
		restored, err := sqliteStore.GetConfig(ctx, channel.ID)
		if err != nil {
			t.Fatalf("GetConfig restored channel: %v", err)
		}
		if len(restored.ModelEntries) != rowCount {
			t.Fatalf("restored model rows=%d, want %d", len(restored.ModelEntries), rowCount)
		}
	})

	t.Run("DebugLogCleanup", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("CreateMySQLStore: %v", err)
		}
		defer func() { _ = store.Close() }()

		ctx := t.Context()
		cutoff := time.Now()
		for i := 1; i <= 3; i++ {
			if err := store.AddDebugLog(ctx, &model.DebugLogEntry{
				LogID:       int64(i),
				CreatedAt:   cutoff.Add(-time.Duration(i) * time.Minute).Unix(),
				ReqURL:      "https://upstream.example.com",
				ReqHeaders:  "{}",
				ReqBody:     []byte("request"),
				RespHeaders: "{}",
			}); err != nil {
				t.Fatalf("AddDebugLog(%d): %v", i, err)
			}
		}
		if deleted, err := store.CleanupDebugLogsBatch(ctx, cutoff, 1); err != nil || deleted != 1 {
			t.Fatalf("CleanupDebugLogsBatch: deleted=%d err=%v", deleted, err)
		}
		if err := store.TruncateDebugLogs(ctx); err != nil {
			t.Fatalf("TruncateDebugLogs: %v", err)
		}
		for i := 1; i <= 3; i++ {
			if entry, err := store.GetDebugLogByLogID(ctx, int64(i)); err != nil || entry != nil {
				t.Fatalf("debug log %d after truncate: entry=%v err=%v", i, entry, err)
			}
		}
	})

	t.Run("StructuredChannelURLs", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("初始迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		ctx := context.Background()
		created, err := store.CreateConfig(ctx, &model.Config{
			Name:    "mysql-legacy-urls",
			URLs:    model.ChannelURLs{{URL: "https://placeholder.example.com"}},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("创建迁移夹具失败: %v", err)
		}
		if _, err := env.db.ExecContext(ctx, "UPDATE channels SET url = ? WHERE id = ?", "https://one.example.com\nhttps://two.example.com/v1/messages#", created.ID); err != nil {
			t.Fatalf("写入旧 URL 格式失败: %v", err)
		}
		if _, err := env.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", structuredChannelURLsMigrationVersion); err != nil {
			t.Fatalf("重置迁移标记失败: %v", err)
		}
		if err := migrateMySQL(ctx, env.db); err != nil {
			t.Fatalf("迁移结构化 URL 失败: %v", err)
		}

		var raw string
		if err := env.db.QueryRowContext(ctx, "SELECT url FROM channels WHERE id = ?", created.ID).Scan(&raw); err != nil {
			t.Fatalf("读取迁移结果失败: %v", err)
		}
		var urls model.ChannelURLs
		if err := json.Unmarshal([]byte(raw), &urls); err != nil {
			t.Fatalf("迁移结果不是 JSON: %v (%q)", err, raw)
		}
		if len(urls) != 2 || urls[0].URL != "https://one.example.com" || urls[1].URL != "https://two.example.com/v1/messages" || !urls[1].Exact {
			t.Fatalf("迁移 URL=%+v", urls)
		}
	})

	t.Run("ClientProtocolBackfill", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("初始迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		verifyClientProtocolBackfill(t, context.Background(), env.db, DialectMySQL, func(ctx context.Context, db *sql.DB) error {
			return migrateMySQL(ctx, db)
		})
	})

	t.Run("Idempotent", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		// 第一次迁移
		store1, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第一次迁移失败: %v", err)
		}
		_ = store1.Close()

		// 第二次迁移（应该幂等）
		store2, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第二次迁移失败（应幂等）: %v", err)
		}
		_ = store2.Close()

		t.Log("幂等性验证通过：二次迁移成功")
	})

	t.Run("EnsureColumns_AddNew", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		// 验证 logs 表的新列存在
		expectedColumns := []string{"auth_token_id", "client_protocol", "client_ip", "minute_bucket", "cache_read_input_tokens", "actual_model", "response_model", "log_source"}
		for _, col := range expectedColumns {
			var columnName string
			err := env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'logs' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 logs.%s 不存在: %v", col, err)
			}
			t.Logf("列 logs.%s 存在", col)
		}

		// 验证 auth_tokens 表的新列
		authTokenCols := []string{
			"allowed_models", "cost_used_microusd", "cost_limit_microusd",
			"cost_daily_used_microusd", "cost_daily_limit_microusd", "cost_daily_period_start",
			"cost_monthly_used_microusd", "cost_monthly_limit_microusd", "cost_monthly_period_start",
		}
		for _, col := range authTokenCols {
			var columnName string
			err := env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'auth_tokens' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 auth_tokens.%s 不存在: %v", col, err)
			}
			t.Logf("列 auth_tokens.%s 存在", col)
		}
		for _, col := range []string{"allowed_models", "model_scope_empty"} {
			var columnName string
			err := env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'api_keys' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 api_keys.%s 不存在: %v", col, err)
			}
		}

		// 验证 channels 表的新增列
		var columnName string
		for _, col := range []string{"daily_cost_limit", "scheduled_check_model"} {
			err = env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'channels' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 channels.%s 不存在: %v", col, err)
			}
			t.Logf("列 channels.%s 存在", col)
		}
	})

	t.Run("LegacyChannelsOAuthCredential", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		if _, err := env.db.Exec(`
			CREATE TABLE channels (
				id INT PRIMARY KEY AUTO_INCREMENT,
				name VARCHAR(191) NOT NULL UNIQUE,
				url VARCHAR(191) NOT NULL,
				priority INT NOT NULL DEFAULT 0,
				channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic',
				codex_credential TEXT NULL,
				enabled TINYINT NOT NULL DEFAULT 1,
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL
			)
		`); err != nil {
			t.Fatalf("创建旧版 channels: %v", err)
		}
		if _, err := env.db.Exec(`
			INSERT INTO channels(name, url, channel_type, created_at, updated_at)
			VALUES('legacy-codex-column', 'https://legacy.example.com', 'codex', 1, 1)
		`); err != nil {
			t.Fatalf("写入旧版渠道: %v", err)
		}

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移旧版 channels: %v", err)
		}

		var channelID int64
		var channelType, authType string
		var credential sql.NullString
		if err := env.db.QueryRow(`
			SELECT id, channel_type, auth_type, oauth_credential
			FROM channels WHERE name = 'legacy-codex-column'
		`).Scan(&channelID, &channelType, &authType, &credential); err != nil {
			t.Fatalf("读取迁移后渠道: %v", err)
		}
		if channelType != "codex" || authType != model.AuthTypeAPIKey || credential.Valid {
			t.Fatalf("迁移后认证字段=(%q, %q, %v)", channelType, authType, credential)
		}

		var isNullable string
		var defaultValue sql.NullString
		if err := env.db.QueryRow(`
			SELECT IS_NULLABLE, COLUMN_DEFAULT
			FROM INFORMATION_SCHEMA.COLUMNS
			WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='oauth_credential'
		`).Scan(&isNullable, &defaultValue); err != nil {
			t.Fatalf("读取 oauth_credential 列定义: %v", err)
		}
		if !strings.EqualFold(isNullable, "YES") || defaultValue.Valid {
			t.Fatalf("oauth_credential nullable=%q default=%v, want nullable without default", isNullable, defaultValue)
		}
		var legacyColumnCount int
		if err := env.db.QueryRow(`
			SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
			WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='codex_credential'
		`).Scan(&legacyColumnCount); err != nil {
			t.Fatalf("检查旧凭证列: %v", err)
		}
		if legacyColumnCount != 0 {
			t.Fatalf("codex_credential column still exists")
		}
		loaded, err := store.GetConfig(context.Background(), channelID)
		if err != nil {
			t.Fatalf("通过存储读取迁移渠道: %v", err)
		}
		if loaded.OAuthCredential != "" {
			t.Fatalf("存储读取 OAuthCredential=%q, want empty", loaded.OAuthCredential)
		}
		_ = store.Close()
		if err := migrateMySQL(context.Background(), env.db); err != nil {
			t.Fatalf("重复迁移旧版 channels: %v", err)
		}
	})

	t.Run("EnsureColumns_APIKeyLengthDrift", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		mustExec := func(stmt string) {
			t.Helper()
			if _, err := env.db.Exec(stmt); err != nil {
				t.Fatalf("预置旧表失败: %v\nSQL=%s", err, stmt)
			}
		}

		// 预置旧版 schema：api_keys.api_key 仍是 VARCHAR(64)
		mustExec(`
			CREATE TABLE channels (
				id INT PRIMARY KEY AUTO_INCREMENT,
				name VARCHAR(191) NOT NULL UNIQUE,
				url VARCHAR(191) NOT NULL,
				priority INT NOT NULL DEFAULT 0,
				enabled TINYINT NOT NULL DEFAULT 1,
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				daily_cost_limit DOUBLE NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				INDEX idx_channels_enabled (enabled),
				INDEX idx_channels_priority (priority DESC),
				INDEX idx_channels_cooldown (cooldown_until)
			)
		`)

		mustExec(`
			CREATE TABLE api_keys (
				id INT PRIMARY KEY AUTO_INCREMENT,
				channel_id INT NOT NULL,
				key_index INT NOT NULL,
				api_key VARCHAR(64) NOT NULL,
				key_strategy VARCHAR(32) NOT NULL DEFAULT 'sequential',
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				UNIQUE KEY uk_channel_key (channel_id, key_index),
				FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE,
				INDEX idx_api_keys_cooldown (cooldown_until),
				INDEX idx_api_keys_channel_cooldown (channel_id, cooldown_until)
			)
		`)

		mustExec(`
			CREATE TABLE channel_models (
				channel_id INT NOT NULL,
				model VARCHAR(191) NOT NULL,
				redirect_model VARCHAR(191) NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL DEFAULT 0,
				PRIMARY KEY (channel_id, model),
				FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE,
				INDEX idx_channel_models_model (model)
			)
		`)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移旧 schema 失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		var (
			dataType   string
			charLen    sql.NullInt64
			isNullable string
		)
		err = env.db.QueryRow(`
			SELECT DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, IS_NULLABLE
			FROM INFORMATION_SCHEMA.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'api_keys' AND COLUMN_NAME = 'api_key'
		`).Scan(&dataType, &charLen, &isNullable)
		if err != nil {
			t.Fatalf("查询 api_keys.api_key 列定义失败: %v", err)
		}

		if !strings.EqualFold(dataType, "varchar") {
			t.Fatalf("api_keys.api_key 类型错误: got=%s want=varchar", dataType)
		}
		if !charLen.Valid || charLen.Int64 != 255 {
			t.Fatalf("api_keys.api_key 长度错误: got=%v want=255", charLen)
		}
		if !strings.EqualFold(isNullable, "NO") {
			t.Fatalf("api_keys.api_key 可空性错误: got=%s want=NO", isNullable)
		}

		var protocolTransformDefault sql.NullString
		if err := env.db.QueryRow(`
			SELECT COLUMN_DEFAULT
			FROM INFORMATION_SCHEMA.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'channels' AND COLUMN_NAME = 'protocol_transform_mode'
		`).Scan(&protocolTransformDefault); err != nil {
			t.Fatalf("查询 channels.protocol_transform_mode 失败: %v", err)
		}
		if !protocolTransformDefault.Valid || protocolTransformDefault.String != "auto" {
			t.Fatalf("protocol_transform_mode 默认值=%v, want auto", protocolTransformDefault)
		}

		longKey := "sk-" + strings.Repeat("x", 197) // 长度 200，验证迁移后的 VARCHAR(255) 契约
		created, updated, err := store.ImportChannelBatch(context.Background(), []*model.ChannelWithKeys{
			{
				Config: &model.Config{
					Name:     "legacy-key-len",
					URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
					Priority: 1,
					Enabled:  true,
					ModelEntries: []model.ModelEntry{
						{Model: "gpt-4"},
					},
				},
				APIKeys: []model.APIKey{
					{KeyIndex: 0, APIKey: longKey, KeyStrategy: model.KeyStrategySequential},
				},
			},
		})
		if err != nil {
			t.Fatalf("导入长 key 失败: %v", err)
		}
		if created != 1 || updated != 0 {
			t.Fatalf("导入计数异常: created=%d updated=%d", created, updated)
		}

		var keyLen int
		err = env.db.QueryRow("SELECT CHAR_LENGTH(api_key) FROM api_keys WHERE key_index = 0 LIMIT 1").Scan(&keyLen)
		if err != nil {
			t.Fatalf("查询导入 key 长度失败: %v", err)
		}
		if keyLen != len(longKey) {
			t.Fatalf("导入 key 长度不匹配: got=%d want=%d", keyLen, len(longKey))
		}

		_ = store.Close()
		store2, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("二次迁移失败（应幂等）: %v", err)
		}
		defer func() { _ = store2.Close() }()
	})
}
