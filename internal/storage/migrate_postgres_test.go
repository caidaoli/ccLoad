//go:build postgres_integration

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"ccLoad/internal/model"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ============================================================================
// PostgreSQL 迁移/业务条件化测试
// 运行：
//   go test -tags "sonic postgres_integration" ./internal/storage -v -count=1 -run TestPostgres
//
// 环境：
// - Docker 已安装；或
// - CCLOAD_TEST_POSTGRES_DSN 指向已有实例
//
// 示例：
//   CCLOAD_TEST_POSTGRES_DSN="postgres://ccload:test@127.0.0.1:5432/ccload_test?sslmode=disable" \
//       go test -tags "sonic postgres_integration" ./internal/storage -v -count=1 -run TestPostgres
// ============================================================================

const (
	testPostgresImage = "postgres:16-alpine"
	testPostgresUser  = "ccload"
	testPostgresPass  = "testpass"
	testPostgresDB    = "ccload_test"
)

type postgresTestEnv struct {
	dsn         string
	containerID string
	db          *sql.DB
}

func setupPostgresEnv(t *testing.T) *postgresTestEnv {
	t.Helper()

	if dsn := os.Getenv("CCLOAD_TEST_POSTGRES_DSN"); dsn != "" {
		t.Logf("使用环境变量提供的 PostgreSQL DSN")
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("连接 PostgreSQL 失败: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Ping(); err != nil {
			t.Fatalf("PostgreSQL ping 失败: %v", err)
		}
		return &postgresTestEnv{dsn: dsn, db: db}
	}

	return startDockerPostgres(t)
}

func startDockerPostgres(t *testing.T) *postgresTestEnv {
	t.Helper()

	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("Docker 不可用，跳过 PostgreSQL 集成测试")
	}

	containerName := fmt.Sprintf("ccload-pg-test-%d", time.Now().UnixNano())
	args := []string{
		"run", "-d",
		"--name", containerName,
		"-e", "POSTGRES_USER=" + testPostgresUser,
		"-e", "POSTGRES_PASSWORD=" + testPostgresPass,
		"-e", "POSTGRES_DB=" + testPostgresDB,
		"-p", "127.0.0.1::5432",
		testPostgresImage,
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("启动 PostgreSQL 容器失败: %v\n%s", err, out)
	}
	// docker pull 时 stdout/stderr 混有进度行，真正的容器 ID 在最后一行
	containerID := lastNonEmptyLine(string(out))
	if len(containerID) < 12 {
		t.Fatalf("无法解析容器 ID，docker 输出:\n%s", out)
	}
	t.Logf("启动 PostgreSQL 容器: %s", containerID[:12])

	hostPort := dockerMappedHostPort(t, containerID, "5432/tcp")
	t.Logf("PostgreSQL 端口映射: 127.0.0.1:%s -> 5432", hostPort)

	t.Cleanup(func() {
		t.Logf("停止并删除 PostgreSQL 容器: %s", containerID[:12])
		_ = exec.Command("docker", "stop", containerID).Run()
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})

	dsn := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		testPostgresUser, testPostgresPass, hostPort, testPostgresDB)

	var db *sql.DB
	for i := range 45 {
		time.Sleep(time.Second)
		db, err = sql.Open("pgx", dsn)
		if err != nil {
			continue
		}
		if err := db.Ping(); err == nil {
			t.Logf("PostgreSQL 就绪（等待 %d 秒）", i+1)
			t.Cleanup(func() { _ = db.Close() })
			return &postgresTestEnv{dsn: dsn, containerID: containerID, db: db}
		}
		_ = db.Close()
	}

	t.Fatalf("PostgreSQL 容器启动超时（45秒）")
	return nil
}

func cleanupPostgresTables(t *testing.T, db *sql.DB) {
	t.Helper()

	tables := []string{
		"fingerprint_test_results", "model_fingerprints", "debug_logs", "logs", "web_sessions", "admin_sessions", "system_settings",
		"auth_tokens", "channel_models", "channel_model_cooldowns", "channel_protocol_transforms", "api_keys", "channel_url_states",
		"channels", "schema_migrations", "key_rr",
	}
	for _, table := range tables {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table + " CASCADE"); err != nil {
			t.Logf("DROP TABLE %s: %v", table, err)
		}
	}
}

func TestPostgres(t *testing.T) {
	env := setupPostgresEnv(t)
	ctx := context.Background()

	t.Run("FullMigration", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("CreatePostgresStore 失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		tables := []string{"channels", "api_keys", "channel_models", "auth_tokens", "logs", "system_settings", "web_sessions", "schema_migrations", "model_fingerprints", "fingerprint_test_results"}
		for _, table := range tables {
			var count int
			if err := env.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count); err != nil {
				t.Fatalf("表 %s 查询失败: %v", table, err)
			}
			t.Logf("表 %s 存在（行数: %d）", table, count)
		}
	})

	t.Run("FingerprintExplicitIDAndRestore", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		verifyFingerprintStorageContract(t, store)
	})

	t.Run("Idempotent", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store1, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第一次迁移失败: %v", err)
		}
		_ = store1.Close()

		store2, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第二次迁移失败（应幂等）: %v", err)
		}
		_ = store2.Close()
		t.Log("幂等性验证通过：二次迁移成功")
	})

	t.Run("EnsureColumns", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		checkCol := func(table, col string) {
			t.Helper()
			var name string
			err := env.db.QueryRow(`
				SELECT column_name
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
			`, table, col).Scan(&name)
			if err != nil {
				t.Fatalf("列 %s.%s 不存在: %v", table, col, err)
			}
			t.Logf("列 %s.%s 存在", table, col)
		}

		for _, col := range []string{"auth_token_id", "client_ip", "minute_bucket", "cache_read_input_tokens", "actual_model", "log_source"} {
			checkCol("logs", col)
		}
		for _, col := range []string{"allowed_models", "cost_used_microusd", "cost_limit_microusd"} {
			checkCol("auth_tokens", col)
		}
		for _, col := range []string{"daily_cost_limit", "scheduled_check_model", "cost_multiplier"} {
			checkCol("channels", col)
		}
	})

	t.Run("CRUD_Settings_Channel_Token_Log", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		// system_settings: 引号 + rebind
		settings, err := store.ListAllSettings(ctx)
		if err != nil {
			t.Fatalf("ListAllSettings: %v", err)
		}
		if len(settings) == 0 {
			t.Fatal("期望默认 system_settings 非空")
		}
		if err := store.UpdateSetting(ctx, "log_retention_days", "14"); err != nil {
			t.Fatalf("UpdateSetting: %v", err)
		}
		got, err := store.GetSetting(ctx, "log_retention_days")
		if err != nil {
			t.Fatalf("GetSetting: %v", err)
		}
		if got.Value != "14" {
			t.Fatalf("setting value got=%q want=14", got.Value)
		}

		// channels: RETURNING id
		ch, err := store.CreateConfig(ctx, &model.Config{
			Name:        "pg-test-channel",
			URL:         "https://api.example.com",
			Priority:    10,
			ChannelType: "openai",
			Enabled:     true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-4o"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		if ch.ID <= 0 {
			t.Fatalf("CreateConfig 未返回 id: %+v", ch)
		}
		t.Logf("CreateConfig id=%d", ch.ID)

		// api_keys
		keys := []*model.APIKey{
			{ChannelID: ch.ID, KeyIndex: 0, APIKey: "sk-test-key-1", KeyStrategy: model.KeyStrategySequential},
			{ChannelID: ch.ID, KeyIndex: 1, APIKey: "sk-test-key-2", KeyStrategy: model.KeyStrategySequential},
		}
		if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
			t.Fatalf("CreateAPIKeysBatch: %v", err)
		}
		gotKeys, err := store.GetAPIKeys(ctx, ch.ID)
		if err != nil {
			t.Fatalf("GetAPIKeys: %v", err)
		}
		if len(gotKeys) != 2 {
			t.Fatalf("GetAPIKeys len=%d want=2", len(gotKeys))
		}

		// auth_tokens: RETURNING + ON CONFLICT
		token := &model.AuthToken{
			Token:       "pg-test-token-" + fmt.Sprint(time.Now().UnixNano()),
			Description: "pg integration",
			IsActive:    true,
		}
		if err := store.CreateAuthToken(ctx, token); err != nil {
			t.Fatalf("CreateAuthToken: %v", err)
		}
		if token.ID <= 0 {
			t.Fatalf("CreateAuthToken 未回填 id")
		}
		created, err := store.EnsureAuthToken(ctx, &model.AuthToken{
			Token:       token.Token,
			Description: "should-not-overwrite",
			IsActive:    true,
		})
		if err != nil {
			t.Fatalf("EnsureAuthToken: %v", err)
		}
		if created {
			t.Fatal("EnsureAuthToken 对已存在 token 应返回 created=false")
		}

		// logs insert + list
		now := time.Now()
		if err := store.AddLog(ctx, &model.LogEntry{
			Time:       model.JSONTime{Time: now},
			Model:      "gpt-4o",
			ChannelID:  ch.ID,
			StatusCode: 200,
			Message:    "ok",
			Duration:   0.12,
			BaseURL:    "https://api.example.com",
		}); err != nil {
			t.Fatalf("AddLog: %v", err)
		}
		logs, err := store.ListLogs(ctx, now.Add(-time.Hour), 10, 0, nil)
		if err != nil {
			t.Fatalf("ListLogs: %v", err)
		}
		if len(logs) == 0 {
			t.Fatal("ListLogs 期望至少 1 条")
		}

		// cooldown FOR UPDATE 路径
		if _, err := store.BumpChannelCooldown(ctx, ch.ID, now, 500); err != nil {
			t.Fatalf("BumpChannelCooldown: %v", err)
		}
		if err := store.SetChannelCooldown(ctx, ch.ID, now.Add(2*time.Minute)); err != nil {
			t.Fatalf("SetChannelCooldown: %v", err)
		}

		// cleanup logs 子查询 LIMIT
		if err := store.CleanupLogsBefore(ctx, now.Add(time.Hour)); err != nil {
			t.Fatalf("CleanupLogsBefore: %v", err)
		}
		logsAfter, err := store.ListLogs(ctx, now.Add(-time.Hour), 10, 0, nil)
		if err != nil {
			t.Fatalf("ListLogs after cleanup: %v", err)
		}
		if len(logsAfter) != 0 {
			t.Fatalf("CleanupLogsBefore 后仍有 %d 条日志", len(logsAfter))
		}
	})

	t.Run("URLState_ONConflict", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		ch, err := store.CreateConfig(ctx, &model.Config{
			Name:        "pg-url-state",
			URL:         "https://a.example.com,https://b.example.com",
			Priority:    1,
			ChannelType: "anthropic",
			Enabled:     true,
		})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}

		if err := store.SetURLDisabled(ctx, ch.ID, "https://a.example.com", true); err != nil {
			t.Fatalf("SetURLDisabled true: %v", err)
		}
		if err := store.SetURLDisabled(ctx, ch.ID, "https://a.example.com", false); err != nil {
			t.Fatalf("SetURLDisabled false: %v", err)
		}
		disabled, err := store.LoadDisabledURLs(ctx)
		if err != nil {
			t.Fatalf("LoadDisabledURLs: %v", err)
		}
		if urls := disabled[ch.ID]; len(urls) != 0 {
			t.Fatalf("期望 URL 已启用，got disabled=%v", urls)
		}
	})

	t.Run("WebSession_Upsert", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		const token = "pg-web-session"
		if err := store.CreateWebSession(ctx, token, model.WebSession{
			Role:      model.WebRoleAdmin,
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateWebSession initial: %v", err)
		}
		if err := store.CreateWebSession(ctx, token, model.WebSession{
			Role:        model.WebRoleAPIToken,
			AuthTokenID: 42,
			ExpiresAt:   time.Now().Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateWebSession overwrite: %v", err)
		}

		got, exists, err := store.GetWebSession(ctx, token)
		if err != nil {
			t.Fatalf("GetWebSession: %v", err)
		}
		if !exists {
			t.Fatal("覆盖后的 WebSession 不存在")
		}
		if got.Role != model.WebRoleAPIToken || got.AuthTokenID != 42 {
			t.Fatalf("WebSession identity=(%q,%d), want=(%q,42)", got.Role, got.AuthTokenID, model.WebRoleAPIToken)
		}
	})

	t.Run("ExplicitID_ChannelSequence", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		mixedAutomatic := &model.Config{
			Name:        "pg-import-automatic-id",
			URL:         "https://import-auto.example.com",
			ChannelType: "gemini",
			Enabled:     true,
		}
		created, updated, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{
			{
				Config: &model.Config{
					ID:          1,
					Name:        "pg-import-explicit-id",
					URL:         "https://import.example.com",
					ChannelType: "openai",
					Enabled:     true,
				},
			},
			{Config: mixedAutomatic},
		})
		if err != nil {
			t.Fatalf("ImportChannelBatch mixed explicit/automatic ids: %v", err)
		}
		if created != 2 || updated != 0 {
			t.Fatalf("ImportChannelBatch counts=(%d,%d), want=(2,0)", created, updated)
		}
		if mixedAutomatic.ID <= 1 {
			t.Fatalf("mixed batch automatic channel id=%d, want >1", mixedAutomatic.ID)
		}

		if _, err := store.CreateConfig(ctx, &model.Config{
			ID:          3,
			Name:        "pg-create-explicit-id",
			URL:         "https://explicit.example.com",
			ChannelType: "anthropic",
			Enabled:     true,
		}); err != nil {
			t.Fatalf("CreateConfig explicit id: %v", err)
		}

		automatic, err := store.CreateConfig(ctx, &model.Config{
			Name:        "pg-create-automatic-id",
			URL:         "https://automatic.example.com",
			ChannelType: "gemini",
			Enabled:     true,
		})
		if err != nil {
			t.Fatalf("CreateConfig automatic id after explicit ids: %v", err)
		}
		if automatic.ID <= 3 {
			t.Fatalf("automatic channel id=%d, want >3", automatic.ID)
		}
	})

	t.Run("ExplicitID_AuthTokenSequence", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		explicit := &model.AuthToken{
			ID:          1,
			Token:       "pg-explicit-auth-token",
			Description: "explicit id",
			IsActive:    true,
		}
		if err := store.CreateAuthToken(ctx, explicit); err != nil {
			t.Fatalf("CreateAuthToken explicit id: %v", err)
		}

		automatic := &model.AuthToken{
			Token:       "pg-automatic-auth-token",
			Description: "automatic id",
			IsActive:    true,
		}
		if err := store.CreateAuthToken(ctx, automatic); err != nil {
			t.Fatalf("CreateAuthToken automatic id after explicit id: %v", err)
		}
		if automatic.ID <= explicit.ID {
			t.Fatalf("automatic auth token id=%d, want >%d", automatic.ID, explicit.ID)
		}
	})

	t.Run("LegacyChannelColumnsNullable", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		if _, err := env.db.Exec(`
			CREATE TABLE channels (
				id BIGSERIAL PRIMARY KEY,
				name VARCHAR(191) NOT NULL UNIQUE,
				url TEXT NOT NULL,
				priority INT NOT NULL DEFAULT 0,
				channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic',
				enabled SMALLINT NOT NULL DEFAULT 1,
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				models TEXT NOT NULL DEFAULT '[]',
				model_redirects TEXT NOT NULL DEFAULT '{}',
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL
			)
		`); err != nil {
			t.Fatalf("create legacy channels table: %v", err)
		}

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("legacy schema migration: %v", err)
		}
		defer func() { _ = store.Close() }()

		for _, column := range []string{"models", "model_redirects"} {
			var nullable string
			if err := env.db.QueryRow(`
				SELECT is_nullable
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'channels' AND column_name = $1
			`, column).Scan(&nullable); err != nil {
				t.Fatalf("query channels.%s nullability: %v", column, err)
			}
			if nullable != "YES" {
				t.Fatalf("channels.%s is_nullable=%q, want YES", column, nullable)
			}
		}

		if _, err := store.CreateConfig(ctx, &model.Config{
			Name:        "pg-after-legacy-migration",
			URL:         "https://legacy.example.com",
			ChannelType: "openai",
			Enabled:     true,
		}); err != nil {
			t.Fatalf("CreateConfig after legacy migration: %v", err)
		}
	})
}
