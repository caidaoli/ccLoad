//go:build postgres_integration

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"

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

func newPostgresQueryStore(t *testing.T, env *postgresTestEnv) Store {
	t.Helper()
	cleanupPostgresTables(t, env.db)
	store, err := CreatePostgresStoreForTest(env.dsn)
	if err != nil {
		t.Fatalf("创建 PostgreSQL store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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

	t.Run("StructuredChannelURLs", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("初始迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		created, err := store.CreateConfig(ctx, &model.Config{
			Name:    "postgres-legacy-urls",
			URLs:    model.ChannelURLs{{URL: "https://placeholder.example.com"}},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("创建迁移夹具失败: %v", err)
		}
		if _, err := env.db.ExecContext(ctx, "UPDATE channels SET url = $1 WHERE id = $2", "https://one.example.com\nhttps://two.example.com/v1/messages#", created.ID); err != nil {
			t.Fatalf("写入旧 URL 格式失败: %v", err)
		}
		if _, err := env.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = $1", structuredChannelURLsMigrationVersion); err != nil {
			t.Fatalf("重置迁移标记失败: %v", err)
		}
		if err := migratePostgres(ctx, env.db); err != nil {
			t.Fatalf("迁移结构化 URL 失败: %v", err)
		}

		var raw string
		if err := env.db.QueryRowContext(ctx, "SELECT url FROM channels WHERE id = $1", created.ID).Scan(&raw); err != nil {
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
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("初始迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		verifyClientProtocolBackfill(t, ctx, env.db, DialectPostgres, func(ctx context.Context, db *sql.DB) error {
			return migratePostgres(ctx, db)
		})
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

		for _, col := range []string{"auth_token_id", "client_protocol", "client_ip", "minute_bucket", "cache_read_input_tokens", "actual_model", "log_source", "upstream_websocket"} {
			checkCol("logs", col)
		}
		for _, col := range []string{"allowed_models", "cost_used_microusd", "cost_limit_microusd"} {
			checkCol("auth_tokens", col)
		}
		for _, col := range []string{"daily_cost_limit", "scheduled_check_model", "cost_multiplier"} {
			checkCol("channels", col)
		}
	})

	t.Run("ChannelModelDisabledRoundTrip", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		created, err := store.CreateConfig(ctx, &model.Config{
			Name:    "pg-model-disabled-round-trip",
			URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
			Enabled: true,
			ModelEntries: []model.ModelEntry{
				{Model: "enabled-model"},
				{Model: "disabled-model", Disabled: true},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		assertDisabled := func(entries []model.ModelEntry, want map[string]bool) {
			t.Helper()
			if len(entries) != len(want) {
				t.Fatalf("ModelEntries len=%d want=%d: %+v", len(entries), len(want), entries)
			}
			for _, entry := range entries {
				disabled, ok := want[entry.Model]
				if !ok {
					t.Fatalf("unexpected model %q", entry.Model)
				}
				if entry.Disabled != disabled {
					t.Fatalf("model %q Disabled=%v want=%v", entry.Model, entry.Disabled, disabled)
				}
			}
		}
		assertDisabled(created.ModelEntries, map[string]bool{
			"enabled-model":  false,
			"disabled-model": true,
		})

		created.ModelEntries = []model.ModelEntry{
			{Model: "enabled-model", Disabled: true},
			{Model: "disabled-model"},
		}
		updated, err := store.UpdateConfig(ctx, created.ID, created)
		if err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
		assertDisabled(updated.ModelEntries, map[string]bool{
			"enabled-model":  true,
			"disabled-model": false,
		})
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
			Name:     "pg-test-channel",
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: 10,
			Enabled:  true,
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

	t.Run("DashboardAggregates", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		ch, err := store.CreateConfig(ctx, &model.Config{
			Name:     "pg-dashboard-aggregates",
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: 1,
			Enabled:  true,
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-4o"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		authToken := &model.AuthToken{
			Token:       "pg-dashboard-token",
			Description: "dashboard aggregate queries",
			IsActive:    true,
			CreatedAt:   time.Now(),
		}
		if err := store.CreateAuthToken(ctx, authToken); err != nil {
			t.Fatalf("CreateAuthToken: %v", err)
		}

		// 固定在分钟中点，避免并行执行或容器启动耗时使两条夹具数据跨越分钟桶。
		now := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
		for _, entry := range []*model.LogEntry{
			{
				Time:          model.JSONTime{Time: now.Add(-20 * time.Second)},
				Model:         "gpt-4o",
				LogSource:     model.LogSourceProxy,
				ChannelID:     ch.ID,
				StatusCode:    200,
				Message:       "ok",
				Duration:      0.1234,
				IsStreaming:   true,
				FirstByteTime: 0.01234,
				AuthTokenID:   authToken.ID,
				BaseURL:       "https://api.example.com",
				InputTokens:   10,
				OutputTokens:  20,
				Cost:          0.01,
			},
			{
				Time:          model.JSONTime{Time: now.Add(-10 * time.Second)},
				Model:         "gpt-4o",
				LogSource:     model.LogSourceProxy,
				ChannelID:     ch.ID,
				StatusCode:    200,
				Message:       "ok",
				Duration:      0.2345,
				IsStreaming:   true,
				FirstByteTime: 0.02345,
				AuthTokenID:   authToken.ID,
				BaseURL:       "https://api.example.com",
				InputTokens:   30,
				OutputTokens:  40,
				Cost:          0.02,
			},
		} {
			if err := store.AddLog(ctx, entry); err != nil {
				t.Fatalf("AddLog: %v", err)
			}
		}

		start := now.Add(-time.Minute)
		end := now.Add(time.Minute)
		filter := &model.LogFilter{LogSource: model.LogSourceProxy}

		assertStats := func(t *testing.T, stats []model.StatsEntry) {
			t.Helper()
			if len(stats) != 1 {
				t.Fatalf("stats len=%d want=1", len(stats))
			}
			got := stats[0]
			if got.Success != 2 || got.Error != 0 || got.Total != 2 {
				t.Fatalf("stats counts=%+v want success=2 error=0 total=2", got)
			}
			if got.AvgFirstByteTimeSeconds == nil || math.Abs(*got.AvgFirstByteTimeSeconds-0.017895) > 0.001 {
				t.Fatalf("avg first byte=%v want about 0.017895", got.AvgFirstByteTimeSeconds)
			}
			if got.AvgDurationSeconds == nil || math.Abs(*got.AvgDurationSeconds-0.17895) > 0.001 {
				t.Fatalf("avg duration=%v want about 0.17895", got.AvgDurationSeconds)
			}
		}

		t.Run("GetStatsLite", func(t *testing.T) {
			stats, err := store.GetStatsLite(ctx, start, end, filter)
			if err != nil {
				t.Fatalf("GetStatsLite: %v", err)
			}
			assertStats(t, stats)
		})

		t.Run("GetStats", func(t *testing.T) {
			stats, err := store.GetStats(ctx, start, end, filter, false)
			if err != nil {
				t.Fatalf("GetStats: %v", err)
			}
			assertStats(t, stats)

			modelFilter := *filter
			modelFilter.Model = "gpt-4o"
			stats, err = store.GetStats(ctx, start, end, &modelFilter, false)
			if err != nil {
				t.Fatalf("GetStats model filter: %v", err)
			}
			assertStats(t, stats)
		})

		t.Run("AggregateRangeWithFilter", func(t *testing.T) {
			points, err := store.AggregateRangeWithFilter(ctx, start, end, time.Minute, filter)
			if err != nil {
				t.Fatalf("AggregateRangeWithFilter: %v", err)
			}
			for _, point := range points {
				if point.Success != 2 {
					continue
				}
				if point.Error != 0 || point.AvgFirstByteTimeSeconds == nil || point.AvgDurationSeconds == nil {
					t.Fatalf("aggregate point=%+v", point)
				}
				return
			}
			t.Fatalf("未找到包含两次成功请求的聚合点: %+v", points)
		})

		t.Run("RemainingMetricQueries", func(t *testing.T) {
			models, err := store.GetDistinctModels(ctx, start, end, filter)
			if err != nil {
				t.Fatalf("GetDistinctModels: %v", err)
			}
			if len(models) != 1 || models[0] != "gpt-4o" {
				t.Fatalf("models=%v want=[gpt-4o]", models)
			}

			channels, err := store.GetDistinctChannels(ctx, start, end, filter)
			if err != nil {
				t.Fatalf("GetDistinctChannels: %v", err)
			}
			if len(channels) != 1 || channels[0].ID != ch.ID {
				t.Fatalf("channels=%v want channel_id=%d", channels, ch.ID)
			}

			rpm, err := store.GetRPMStats(ctx, start, end, filter, true)
			if err != nil {
				t.Fatalf("GetRPMStats: %v", err)
			}
			if rpm.PeakRPM <= 0 || rpm.AvgRPM <= 0 {
				t.Fatalf("rpm=%+v want positive peak and average", rpm)
			}

			rates, err := store.GetChannelSuccessRates(ctx, start)
			if err != nil {
				t.Fatalf("GetChannelSuccessRates: %v", err)
			}
			if rates[ch.ID].SampleCount != 2 {
				t.Fatalf("success rates=%+v want channel sample_count=2", rates)
			}

			timeline, err := store.GetHealthTimeline(ctx, model.HealthTimelineParams{
				SinceMs:  start.UnixMilli(),
				UntilMs:  end.UnixMilli(),
				BucketMs: time.Minute.Milliseconds(),
				Filter:   filter,
			})
			if err != nil {
				t.Fatalf("GetHealthTimeline: %v", err)
			}
			if len(timeline) == 0 {
				t.Fatal("GetHealthTimeline returned no rows")
			}

			costs, err := store.GetTodayChannelCosts(ctx, now.Add(-time.Hour))
			if err != nil {
				t.Fatalf("GetTodayChannelCosts: %v", err)
			}
			if _, ok := costs[ch.ID]; !ok {
				t.Fatalf("today channel costs=%v missing channel_id=%d", costs, ch.ID)
			}

			urlStats, err := store.GetTodayChannelURLStats(ctx, now.Add(-time.Hour))
			if err != nil {
				t.Fatalf("GetTodayChannelURLStats: %v", err)
			}
			if len(urlStats) != 1 || urlStats[0].ChannelID != ch.ID {
				t.Fatalf("url stats=%v want channel_id=%d", urlStats, ch.ID)
			}

			tokenStats, err := store.GetAuthTokenStatsInRange(ctx, start, end)
			if err != nil {
				t.Fatalf("GetAuthTokenStatsInRange: %v", err)
			}
			if tokenStats[authToken.ID] == nil || tokenStats[authToken.ID].SuccessCount != 2 {
				t.Fatalf("auth token stats=%+v want token_id=%d success=2", tokenStats, authToken.ID)
			}
			if err := store.FillAuthTokenRPMStats(ctx, tokenStats, start, end, true); err != nil {
				t.Fatalf("FillAuthTokenRPMStats: %v", err)
			}
			if tokenStats[authToken.ID].PeakRPM <= 0 {
				t.Fatalf("auth token rpm=%+v want positive peak", tokenStats[authToken.ID])
			}
		})
	})

	t.Run("RuntimeQueryCompatibility", func(t *testing.T) {
		t.Run("ChannelsAndAPIKeys", func(t *testing.T) {
			store := newPostgresQueryStore(t, env)
			ch, err := store.CreateConfig(ctx, &model.Config{
				Name:     "pg-query-channel",
				URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
				Priority: 10,
				Enabled:  true,
				ModelEntries: []model.ModelEntry{
					{Model: "gpt-4o"},
				},
			})
			if err != nil {
				t.Fatalf("CreateConfig: %v", err)
			}
			if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
				{ChannelID: ch.ID, KeyIndex: 0, APIKey: "sk-pg-0", KeyStrategy: model.KeyStrategySequential},
				{ChannelID: ch.ID, KeyIndex: 1, APIKey: "sk-pg-1", KeyStrategy: model.KeyStrategySequential},
				{ChannelID: ch.ID, KeyIndex: 2, APIKey: "sk-pg-2", KeyStrategy: model.KeyStrategySequential},
			}); err != nil {
				t.Fatalf("CreateAPIKeysBatch: %v", err)
			}

			if configs, err := store.ListConfigs(ctx); err != nil || len(configs) != 1 {
				t.Fatalf("ListConfigs: configs=%v err=%v", configs, err)
			}
			if _, err := store.GetConfig(ctx, ch.ID); err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			if channels, err := store.GetEnabledChannelsByModel(ctx, "gpt-4o"); err != nil || len(channels) != 1 {
				t.Fatalf("GetEnabledChannelsByModel: channels=%v err=%v", channels, err)
			}

			ch.Name = "pg-query-channel-updated"
			ch.Priority = 20
			if _, err := store.UpdateConfig(ctx, ch.ID, ch); err != nil {
				t.Fatalf("UpdateConfig: %v", err)
			}
			if _, err := store.UpdateChannelEnabled(ctx, ch.ID, false); err != nil {
				t.Fatalf("UpdateChannelEnabled false: %v", err)
			}
			if _, err := store.UpdateChannelEnabled(ctx, ch.ID, true); err != nil {
				t.Fatalf("UpdateChannelEnabled true: %v", err)
			}
			priorityUpdates := []struct {
				ID       int64
				Priority int
			}{{ID: ch.ID, Priority: 30}}
			if _, err := store.BatchUpdatePriority(ctx, priorityUpdates); err != nil {
				t.Fatalf("BatchUpdatePriority: %v", err)
			}

			if err := store.SetURLDisabled(ctx, ch.ID, "https://a.example.com", true); err != nil {
				t.Fatalf("SetURLDisabled a: %v", err)
			}
			if err := store.SetURLDisabled(ctx, ch.ID, "https://b.example.com", true); err != nil {
				t.Fatalf("SetURLDisabled b: %v", err)
			}
			if _, err := store.LoadDisabledURLs(ctx); err != nil {
				t.Fatalf("LoadDisabledURLs: %v", err)
			}
			if err := store.CleanupOrphanedURLStates(ctx, ch.ID, []string{"https://a.example.com"}); err != nil {
				t.Fatalf("CleanupOrphanedURLStates keep: %v", err)
			}
			if err := store.CleanupOrphanedURLStates(ctx, ch.ID, nil); err != nil {
				t.Fatalf("CleanupOrphanedURLStates empty: %v", err)
			}

			if _, err := store.GetAPIKey(ctx, ch.ID, 0); err != nil {
				t.Fatalf("GetAPIKey: %v", err)
			}
			if _, err := store.GetAPIKeys(ctx, ch.ID); err != nil {
				t.Fatalf("GetAPIKeys: %v", err)
			}
			if _, err := store.GetAllAPIKeys(ctx); err != nil {
				t.Fatalf("GetAllAPIKeys: %v", err)
			}
			if err := store.UpdateAPIKeysStrategy(ctx, ch.ID, model.KeyStrategyRoundRobin); err != nil {
				t.Fatalf("UpdateAPIKeysStrategy: %v", err)
			}
			if err := store.UpdateAPIKeyNotes(ctx, ch.ID, map[int]string{0: "primary", 1: "backup"}); err != nil {
				t.Fatalf("UpdateAPIKeyNotes: %v", err)
			}
			if err := store.SetAPIKeyDisabled(ctx, ch.ID, 0, true); err != nil {
				t.Fatalf("SetAPIKeyDisabled true: %v", err)
			}
			if err := store.SetAPIKeyDisabled(ctx, ch.ID, 0, false); err != nil {
				t.Fatalf("SetAPIKeyDisabled false: %v", err)
			}
			if err := store.DeleteAPIKey(ctx, ch.ID, 1); err != nil {
				t.Fatalf("DeleteAPIKey: %v", err)
			}
			if err := store.CompactKeyIndices(ctx, ch.ID, 1); err != nil {
				t.Fatalf("CompactKeyIndices: %v", err)
			}
			if err := store.DeleteAllAPIKeys(ctx, ch.ID); err != nil {
				t.Fatalf("DeleteAllAPIKeys: %v", err)
			}
			if err := store.DeleteConfig(ctx, ch.ID); err != nil {
				t.Fatalf("DeleteConfig: %v", err)
			}
		})

		t.Run("Cooldowns", func(t *testing.T) {
			store := newPostgresQueryStore(t, env)
			ch, err := store.CreateConfig(ctx, &model.Config{
				Name: "pg-query-cooldowns", URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
			})
			if err != nil {
				t.Fatalf("CreateConfig: %v", err)
			}
			if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
				ChannelID: ch.ID, KeyIndex: 0, APIKey: "sk-cooldown", KeyStrategy: model.KeyStrategySequential,
			}}); err != nil {
				t.Fatalf("CreateAPIKeysBatch: %v", err)
			}

			now := time.Now()
			if _, err := store.BumpChannelCooldown(ctx, ch.ID, now, 500); err != nil {
				t.Fatalf("BumpChannelCooldown: %v", err)
			}
			if err := store.SetChannelCooldown(ctx, ch.ID, now.Add(time.Minute)); err != nil {
				t.Fatalf("SetChannelCooldown: %v", err)
			}
			if _, err := store.GetAllChannelCooldowns(ctx); err != nil {
				t.Fatalf("GetAllChannelCooldowns: %v", err)
			}
			if err := store.ResetChannelCooldown(ctx, ch.ID); err != nil {
				t.Fatalf("ResetChannelCooldown: %v", err)
			}

			if _, err := store.BumpKeyCooldown(ctx, ch.ID, 0, now, 401); err != nil {
				t.Fatalf("BumpKeyCooldown: %v", err)
			}
			if err := store.SetKeyCooldown(ctx, ch.ID, 0, now.Add(time.Minute)); err != nil {
				t.Fatalf("SetKeyCooldown: %v", err)
			}
			if _, err := store.GetAllKeyCooldowns(ctx); err != nil {
				t.Fatalf("GetAllKeyCooldowns: %v", err)
			}
			sqlStore, ok := store.(*sqlstore.SQLStore)
			if !ok {
				t.Fatalf("store type=%T want *sqlstore.SQLStore", store)
			}
			if _, ok := sqlStore.GetKeyCooldownUntil(ctx, ch.ID, 0); !ok {
				t.Fatal("GetKeyCooldownUntil returned no active cooldown")
			}
			if err := sqlStore.ClearAllKeyCooldowns(ctx, ch.ID); err != nil {
				t.Fatalf("ClearAllKeyCooldowns: %v", err)
			}
			if err := store.ResetKeyCooldown(ctx, ch.ID, 0); err != nil {
				t.Fatalf("ResetKeyCooldown: %v", err)
			}

			if err := store.SetModelCooldown(ctx, ch.ID, "gpt-4o", now.Add(time.Minute)); err != nil {
				t.Fatalf("SetModelCooldown: %v", err)
			}
			if _, err := store.GetAllModelCooldowns(ctx); err != nil {
				t.Fatalf("GetAllModelCooldowns: %v", err)
			}
			if err := store.ResetModelCooldown(ctx, ch.ID, "gpt-4o"); err != nil {
				t.Fatalf("ResetModelCooldown: %v", err)
			}
			if err := store.ResetAllCooldowns(ctx, ch.ID); err != nil {
				t.Fatalf("ResetAllCooldowns: %v", err)
			}
		})

		t.Run("LogsAndDebugLogs", func(t *testing.T) {
			store := newPostgresQueryStore(t, env)
			ch, err := store.CreateConfig(ctx, &model.Config{
				Name: "pg-query-logs", URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
			})
			if err != nil {
				t.Fatalf("CreateConfig: %v", err)
			}
			authToken := &model.AuthToken{
				Token: "pg-query-log-token", Description: "log query token", IsActive: true, CreatedAt: time.Now(),
			}
			if err := store.CreateAuthToken(ctx, authToken); err != nil {
				t.Fatalf("CreateAuthToken: %v", err)
			}

			now := time.Now().UTC().Truncate(time.Second)
			if err := store.AddLog(ctx, &model.LogEntry{
				Time: model.JSONTime{Time: now.Add(-30 * time.Second)}, Model: "gpt-4o", LogSource: model.LogSourceProxy,
				ChannelID: ch.ID, StatusCode: 200, Message: "single", Duration: 0.2, AuthTokenID: authToken.ID,
				BaseURL: "https://api.example.com", Cost: 0.01,
			}); err != nil {
				t.Fatalf("AddLog: %v", err)
			}
			if err := store.BatchAddLogs(ctx, []*model.LogEntry{
				{
					Time: model.JSONTime{Time: now.Add(-20 * time.Second)}, Model: "gpt-4o", LogSource: model.LogSourceProxy,
					ChannelID: ch.ID, StatusCode: 500, Message: "plain-batch", Duration: 0.4, AuthTokenID: authToken.ID,
					BaseURL: "https://api.example.com",
				},
				{
					Time: model.JSONTime{Time: now.Add(-10 * time.Second)}, Model: "gpt-4o", LogSource: model.LogSourceProxy,
					ChannelID: ch.ID, StatusCode: 200, Message: "debug-batch", Duration: 0.3, AuthTokenID: authToken.ID,
					BaseURL: "https://api.example.com",
					DebugData: &model.DebugLogEntry{
						CreatedAt: now.Unix(), ReqMethod: "POST", ReqURL: "https://api.example.com/v1/chat/completions",
						ReqHeaders: "{}", ReqBody: []byte(`{"model":"gpt-4o"}`), RespStatus: 200,
						RespHeaders: "{}", RespBody: []byte(`{"ok":true}`),
					},
				},
			}); err != nil {
				t.Fatalf("BatchAddLogs: %v", err)
			}

			channelID := ch.ID
			filter := &model.LogFilter{ChannelID: &channelID, ModelLike: "gpt"}
			logs, err := store.ListLogs(ctx, now.Add(-time.Hour), 20, 0, filter)
			if err != nil || len(logs) != 3 {
				t.Fatalf("ListLogs: logs=%v err=%v", logs, err)
			}
			if _, err := store.ListLogsRange(ctx, now.Add(-time.Hour), now.Add(time.Hour), 20, 0, filter); err != nil {
				t.Fatalf("ListLogsRange: %v", err)
			}
			if _, count, err := store.ListLogsRangeWithCount(ctx, now.Add(-time.Hour), now.Add(time.Hour), 20, 0, filter); err != nil || count != 3 {
				t.Fatalf("ListLogsRangeWithCount: count=%d err=%v", count, err)
			}
			if count, err := store.CountLogs(ctx, now.Add(-time.Hour), filter); err != nil || count != 3 {
				t.Fatalf("CountLogs: count=%d err=%v", count, err)
			}
			if count, err := store.CountLogsRange(ctx, now.Add(-time.Hour), now.Add(time.Hour), filter); err != nil || count != 3 {
				t.Fatalf("CountLogsRange: count=%d err=%v", count, err)
			}
			if stats, err := store.GetTodayChannelURLStats(ctx, now.Add(-time.Hour)); err != nil || len(stats) != 1 {
				t.Fatalf("GetTodayChannelURLStats: stats=%v err=%v", stats, err)
			}

			var debugLogID int64
			var directDebugLogID int64
			for _, entry := range logs {
				switch entry.Message {
				case "debug-batch":
					debugLogID = entry.ID
				case "single":
					directDebugLogID = entry.ID
				}
			}
			if debugLogID == 0 || directDebugLogID == 0 {
				t.Fatalf("log ids missing: debug=%d direct=%d", debugLogID, directDebugLogID)
			}
			if debugLog, err := store.GetDebugLogByLogID(ctx, debugLogID); err != nil || debugLog == nil {
				t.Fatalf("GetDebugLogByLogID batch: debug=%v err=%v", debugLog, err)
			}
			if err := store.AddDebugLog(ctx, &model.DebugLogEntry{
				LogID: directDebugLogID, CreatedAt: now.Unix(), ReqMethod: "POST", ReqURL: "https://api.example.com",
				ReqHeaders: "{}", ReqBody: []byte(`{}`), RespStatus: 200, RespHeaders: "{}", RespBody: []byte(`{}`),
			}); err != nil {
				t.Fatalf("AddDebugLog: %v", err)
			}
			if deleted, err := store.CleanupDebugLogsBatch(ctx, now.Add(time.Hour), 1); err != nil || deleted != 1 {
				t.Fatalf("CleanupDebugLogsBatch: deleted=%d err=%v", deleted, err)
			}
			if err := store.TruncateDebugLogs(ctx); err != nil {
				t.Fatalf("TruncateDebugLogs: %v", err)
			}
			if err := store.CleanupLogsBefore(ctx, now.Add(-time.Hour)); err != nil {
				t.Fatalf("CleanupLogsBefore: %v", err)
			}
		})

		t.Run("AuthSessionsAndSettings", func(t *testing.T) {
			store := newPostgresQueryStore(t, env)
			const tokenValue = "pg-query-auth-token"
			token := &model.AuthToken{
				Token: tokenValue, Description: "query compatibility", IsActive: true, CreatedAt: time.Now(),
				AllowedModels: []string{"gpt-4o"}, MaxConcurrency: 2,
			}
			if err := store.CreateAuthToken(ctx, token); err != nil {
				t.Fatalf("CreateAuthToken: %v", err)
			}
			ensured := &model.AuthToken{Token: tokenValue, Description: "ignored", IsActive: true, CreatedAt: time.Now()}
			if created, err := store.EnsureAuthToken(ctx, ensured); err != nil || created || ensured.ID != token.ID {
				t.Fatalf("EnsureAuthToken: created=%v token=%+v err=%v", created, ensured, err)
			}
			got, err := store.GetAuthToken(ctx, token.ID)
			if err != nil {
				t.Fatalf("GetAuthToken: %v", err)
			}
			if _, err := store.GetAuthTokenByValue(ctx, tokenValue); err != nil {
				t.Fatalf("GetAuthTokenByValue: %v", err)
			}
			if tokens, err := store.ListAuthTokens(ctx); err != nil || len(tokens) != 1 {
				t.Fatalf("ListAuthTokens: tokens=%v err=%v", tokens, err)
			}
			if tokens, err := store.ListActiveAuthTokens(ctx); err != nil || len(tokens) != 1 {
				t.Fatalf("ListActiveAuthTokens: tokens=%v err=%v", tokens, err)
			}

			got.Description = "query compatibility updated"
			got.AllowedChannelIDs = []int64{11, 22}
			got.ChannelRestrictionMode = model.ChannelRestrictionModeDeny
			if err := store.UpdateAuthToken(ctx, got); err != nil {
				t.Fatalf("UpdateAuthToken: %v", err)
			}
			sqlStore, ok := store.(*sqlstore.SQLStore)
			if !ok {
				t.Fatalf("store type=%T want *sqlstore.SQLStore", store)
			}
			if err := sqlStore.UpsertAuthTokenAllFields(ctx, got); err != nil {
				t.Fatalf("UpsertAuthTokenAllFields: %v", err)
			}
			if err := store.UpdateTokenLastUsed(ctx, tokenValue, time.Now()); err != nil {
				t.Fatalf("UpdateTokenLastUsed: %v", err)
			}
			if err := store.UpdateTokenStats(ctx, tokenValue, true, 0.5, false, 0, 10, 20, 3, 4, 0.01, 0.02); err != nil {
				t.Fatalf("UpdateTokenStats: %v", err)
			}

			if err := store.CreateWebSession(ctx, "pg-query-valid-session", model.WebSession{
				Role: model.WebRoleAPIToken, AuthTokenID: token.ID, ExpiresAt: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("CreateWebSession valid: %v", err)
			}
			if err := store.CreateWebSession(ctx, "pg-query-expired-session", model.WebSession{
				Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatalf("CreateWebSession expired: %v", err)
			}
			if _, exists, err := store.GetWebSession(ctx, "pg-query-valid-session"); err != nil || !exists {
				t.Fatalf("GetWebSession: exists=%v err=%v", exists, err)
			}
			if sessions, err := store.LoadWebSessions(ctx); err != nil || len(sessions) != 1 {
				t.Fatalf("LoadWebSessions: sessions=%v err=%v", sessions, err)
			}
			if err := store.CleanExpiredWebSessions(ctx); err != nil {
				t.Fatalf("CleanExpiredWebSessions: %v", err)
			}
			if err := store.DeleteWebSession(ctx, "pg-query-expired-session"); err != nil {
				t.Fatalf("DeleteWebSession: %v", err)
			}
			if err := store.DeleteWebSessionsByAuthTokenID(ctx, token.ID); err != nil {
				t.Fatalf("DeleteWebSessionsByAuthTokenID: %v", err)
			}

			if _, err := store.GetSetting(ctx, "log_retention_days"); err != nil {
				t.Fatalf("GetSetting: %v", err)
			}
			if _, err := store.ListAllSettings(ctx); err != nil {
				t.Fatalf("ListAllSettings: %v", err)
			}
			if err := store.UpdateSetting(ctx, "log_retention_days", "8"); err != nil {
				t.Fatalf("UpdateSetting: %v", err)
			}
			if err := store.BatchUpdateSettings(ctx, map[string]string{"log_retention_days": "9"}); err != nil {
				t.Fatalf("BatchUpdateSettings: %v", err)
			}
			if err := store.DeleteAuthToken(ctx, token.ID); err != nil {
				t.Fatalf("DeleteAuthToken: %v", err)
			}
			if err := store.Ping(ctx); err != nil {
				t.Fatalf("Ping: %v", err)
			}
		})

		t.Run("Fingerprints", func(t *testing.T) {
			store := newPostgresQueryStore(t, env)
			ch, err := store.CreateConfig(ctx, &model.Config{
				Name: "pg-query-fingerprint", URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
			})
			if err != nil {
				t.Fatalf("CreateConfig: %v", err)
			}
			channelID := ch.ID
			fingerprint, err := store.CreateModelFingerprint(ctx, &model.ModelFingerprint{
				Name: "pg-query-fingerprint", ChannelID: &channelID, ChannelName: ch.Name,
				Model: "gpt-4o", SampleCount: 3,
				Distribution: []float64{0.5, 0.25, 0.25},
				Stats: model.FingerprintStats{
					Mean: 2, Median: 2, Min: 1, Max: 3, Unique: 3, Mode: 1, ModeCount: 1,
				},
				RawData: []int{1, 2, 3}, PromptVersion: "v1",
			})
			if err != nil {
				t.Fatalf("CreateModelFingerprint: %v", err)
			}
			if fingerprints, err := store.ListModelFingerprints(ctx); err != nil || len(fingerprints) != 1 {
				t.Fatalf("ListModelFingerprints: fingerprints=%v err=%v", fingerprints, err)
			}
			if _, err := store.GetModelFingerprint(ctx, fingerprint.ID); err != nil {
				t.Fatalf("GetModelFingerprint: %v", err)
			}
			if err := store.ClearFingerprintChannelID(ctx, ch.ID); err != nil {
				t.Fatalf("ClearFingerprintChannelID: %v", err)
			}

			result := &model.FingerprintTestRecord{
				ChannelID: &channelID, ChannelName: ch.Name, Model: "gpt-4o", SampleCount: 3,
				BestScore: 0.9, Distribution: []float64{0.5, 0.25, 0.25}, MatchesJSON: `[{"score":0.9}]`,
			}
			if err := store.CreateFingerprintTestResult(ctx, result); err != nil {
				t.Fatalf("CreateFingerprintTestResult: %v", err)
			}
			if results, err := store.ListFingerprintTestResults(ctx, 10); err != nil || len(results) != 1 {
				t.Fatalf("ListFingerprintTestResults: results=%v err=%v", results, err)
			}
			if err := store.DeleteFingerprintTestResult(ctx, result.ID); err != nil {
				t.Fatalf("DeleteFingerprintTestResult: %v", err)
			}
			if err := store.DeleteModelFingerprint(ctx, fingerprint.ID); err != nil {
				t.Fatalf("DeleteModelFingerprint: %v", err)
			}
		})
	})

	t.Run("URLState_ONConflict", func(t *testing.T) {
		cleanupPostgresTables(t, env.db)

		store, err := CreatePostgresStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer func() { _ = store.Close() }()

		ch, err := store.CreateConfig(ctx, &model.Config{
			Name:     "pg-url-state",
			URLs:     model.ChannelURLs{{URL: "https://a.example.com,https://b.example.com"}},
			Priority: 1,
			Enabled:  true,
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
			Name:    "pg-import-automatic-id",
			URLs:    model.ChannelURLs{{URL: "https://import-auto.example.com"}},
			Enabled: true,
		}
		created, updated, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{
			{
				Config: &model.Config{
					ID:      1,
					Name:    "pg-import-explicit-id",
					URLs:    model.ChannelURLs{{URL: "https://import.example.com"}},
					Enabled: true,
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
			ID:      3,
			Name:    "pg-create-explicit-id",
			URLs:    model.ChannelURLs{{URL: "https://explicit.example.com"}},
			Enabled: true,
		}); err != nil {
			t.Fatalf("CreateConfig explicit id: %v", err)
		}

		automatic, err := store.CreateConfig(ctx, &model.Config{
			Name:    "pg-create-automatic-id",
			URLs:    model.ChannelURLs{{URL: "https://automatic.example.com"}},
			Enabled: true,
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
		if _, err := env.db.Exec(`
			INSERT INTO channels(name, url, channel_type, models, model_redirects, created_at, updated_at)
			VALUES('legacy-codex-column', 'https://legacy.example.com', 'codex', '[]', '{}', 1, 1)
		`); err != nil {
			t.Fatalf("insert legacy channel: %v", err)
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

		var channelID int64
		var channelType, authType string
		var credential sql.NullString
		if err := env.db.QueryRow(`
			SELECT id, channel_type, auth_type, codex_credential
			FROM channels WHERE name = 'legacy-codex-column'
		`).Scan(&channelID, &channelType, &authType, &credential); err != nil {
			t.Fatalf("read migrated legacy channel: %v", err)
		}
		if channelType != "codex" || authType != model.AuthTypeAPIKey || credential.Valid {
			t.Fatalf("migrated auth fields=(%q, %q, %v)", channelType, authType, credential)
		}

		var credentialNullable string
		var credentialDefault sql.NullString
		if err := env.db.QueryRow(`
			SELECT is_nullable, column_default
			FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='channels' AND column_name='codex_credential'
		`).Scan(&credentialNullable, &credentialDefault); err != nil {
			t.Fatalf("read codex_credential definition: %v", err)
		}
		if credentialNullable != "YES" || credentialDefault.Valid {
			t.Fatalf("codex_credential nullable=%q default=%v, want nullable without default", credentialNullable, credentialDefault)
		}
		loaded, err := store.GetConfig(ctx, channelID)
		if err != nil {
			t.Fatalf("load migrated channel through store: %v", err)
		}
		if loaded.CodexCredential != "" {
			t.Fatalf("store CodexCredential=%q, want empty", loaded.CodexCredential)
		}
		if err := migratePostgres(ctx, env.db); err != nil {
			t.Fatalf("second legacy channels migration: %v", err)
		}

		created, err := store.CreateConfig(ctx, &model.Config{
			Name:    "pg-after-legacy-migration",
			URLs:    model.ChannelURLs{{URL: "https://legacy.example.com"}},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig after legacy migration: %v", err)
		}
		if created.GetProtocolTransformMode() != model.ProtocolTransformModeAuto {
			t.Fatalf("protocol_transform_mode=%q, want auto", created.GetProtocolTransformMode())
		}
	})
}
