package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

func createTestSQLiteStore(t *testing.T) *sqlstore.SQLStore {
	t.Helper()
	store, err := CreateSQLiteStore(t.TempDir() + "/hybrid_test.db")
	if err != nil {
		t.Fatalf("创建测试 SQLite 失败: %v", err)
	}
	return store.(*sqlstore.SQLStore)
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("等待条件超时")
	}
}

func TestHybridStore_SQLiteSuccessDoesNotWaitForPrimary(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	if err := primary.Close(); err != nil {
		t.Fatalf("关闭主库: %v", err)
	}

	ctx := context.Background()
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name:     "sqlite-authority",
		AuthType: model.AuthTypeAPIKey,
		URLs:     model.ChannelURLs{{URL: "https://example.com"}},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("主库故障不应阻塞 SQLite 写入: %v", err)
	}
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil || got.Name != "sqlite-authority" {
		t.Fatalf("SQLite 权威读取 = (%+v, %v)", got, err)
	}

	logEntry := &model.LogEntry{Time: model.JSONTime{Time: time.Now()}, StatusCode: 200, Message: "local"}
	if err := hybrid.AddLog(ctx, logEntry); err != nil {
		t.Fatalf("主库故障不应阻塞日志写入: %v", err)
	}
	logs, err := hybrid.ListLogs(ctx, time.Time{}, 10, 0, nil)
	if err != nil || len(logs) != 1 {
		t.Fatalf("SQLite 日志 = (%d, %v)", len(logs), err)
	}

	waitForCondition(t, time.Second, func() bool {
		metrics := hybrid.RuntimeMetrics()
		return metrics.PrimarySyncPending > 0 && metrics.PrimarySyncFailures > 0
	})
	if err := hybrid.Ping(ctx); err != nil {
		t.Fatalf("主库故障不应让混合模式离线: %v", err)
	}
}

func TestHybridStore_ChannelFinalStateConvergesToPrimary(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()

	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name:     "initial",
		AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{
			{URL: "https://one.example.com"},
			{URL: "https://two.example.com"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := hybrid.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID,
		KeyIndex:  0,
		APIKey:    "secret",
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}
	if err := hybrid.SetAPIKeyDisabled(ctx, created.ID, 0, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	if err := hybrid.SetURLDisabled(ctx, created.ID, "https://two.example.com", true); err != nil {
		t.Fatalf("SetURLDisabled: %v", err)
	}
	if _, err := hybrid.BumpModelCooldown(ctx, created.ID, "upstream-model", time.Now(), 429); err != nil {
		t.Fatalf("BumpModelCooldown: %v", err)
	}
	updated := created.Clone()
	updated.Name = "final"
	if _, err := hybrid.UpdateConfig(ctx, created.ID, updated); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		cfg, cfgErr := primary.GetConfig(ctx, created.ID)
		keys, keysErr := primary.GetAPIKeys(ctx, created.ID)
		disabled, disabledErr := primary.LoadDisabledURLs(ctx)
		return cfgErr == nil && cfg.Name == "final" &&
			keysErr == nil && len(keys) == 1 && keys[0].Disabled &&
			disabledErr == nil && len(disabled[created.ID]) == 1
	})
	if pending := hybrid.RuntimeMetrics().PrimarySyncPending; pending != 0 {
		t.Fatalf("同步后仍有 %d 个待处理任务", pending)
	}
	var localUntil, localDuration, primaryUntil, primaryDuration int64
	if err := sqlite.QueryRowContext(ctx, `
		SELECT cooldown_until, cooldown_duration_ms FROM channel_model_cooldowns
		WHERE channel_id = ? AND model = ?
	`, created.ID, "upstream-model").Scan(&localUntil, &localDuration); err != nil {
		t.Fatal(err)
	}
	if err := primary.QueryRowContext(ctx, `
		SELECT cooldown_until, cooldown_duration_ms FROM channel_model_cooldowns
		WHERE channel_id = ? AND model = ?
	`, created.ID, "upstream-model").Scan(&primaryUntil, &primaryDuration); err != nil {
		t.Fatal(err)
	}
	if localUntil != primaryUntil || localDuration != primaryDuration {
		t.Fatalf("模型冷却终态不一致: local=(%d,%d) primary=(%d,%d)",
			localUntil, localDuration, primaryUntil, primaryDuration)
	}
}

func TestHybridStore_CreateUpdateDeleteKeepsTombstone(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()

	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "short-lived", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	token := &model.AuthToken{
		Token:                  "hybrid-delete-channel-token",
		Description:            "hybrid channel restriction",
		IsActive:               true,
		AllowedChannelIDs:      []int64{created.ID},
		ChannelRestrictionMode: model.ChannelRestrictionModeAllow,
	}
	if err := hybrid.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken: %v", err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		primaryToken, tokenErr := primary.GetAuthToken(ctx, token.ID)
		return tokenErr == nil && len(primaryToken.AllowedChannelIDs) == 1 &&
			hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
	if err := hybrid.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		_, getErr := primary.GetConfig(ctx, created.ID)
		primaryToken, tokenErr := primary.GetAuthToken(ctx, token.ID)
		return getErr != nil && tokenErr == nil && !primaryToken.IsActive && len(primaryToken.AllowedChannelIDs) == 0 &&
			hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
	if _, err := hybrid.GetConfig(ctx, created.ID); err == nil {
		t.Fatal("SQLite 中被删除的渠道不应复活")
	}
	localToken, err := hybrid.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("GetAuthToken: %v", err)
	}
	if len(localToken.AllowedChannelIDs) != 0 {
		t.Fatalf("SQLite allowed_channel_ids=%v, want empty", localToken.AllowedChannelIDs)
	}
	if localToken.IsActive {
		t.Fatal("SQLite token must be disabled after its last allowed channel is deleted")
	}
}

func TestHybridStore_ChannelReplicaUpdateIsAtomic(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	hybrid.primarySync.retry = 20 * time.Millisecond
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()

	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "before", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hybrid.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "key",
	}}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 3*time.Second, func() bool { return hybrid.RuntimeMetrics().PrimarySyncPending == 0 })

	if _, err := primary.ExecContext(ctx, `
		CREATE TRIGGER reject_replica_key_insert
		BEFORE INSERT ON api_keys
		BEGIN
			SELECT RAISE(FAIL, 'reject key replica');
		END
	`); err != nil {
		t.Fatal(err)
	}
	updated := created.Clone()
	updated.Name = "after"
	if _, err := hybrid.UpdateConfig(ctx, created.ID, updated); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, func() bool { return hybrid.RuntimeMetrics().PrimarySyncFailures > 0 })
	primaryConfig, err := primary.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if primaryConfig.Name != "before" {
		t.Fatalf("聚合事务部分提交: primary name=%q", primaryConfig.Name)
	}
	if _, err := primary.ExecContext(ctx, `DROP TRIGGER reject_replica_key_insert`); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		cfg, getErr := primary.GetConfig(ctx, created.ID)
		return getErr == nil && cfg.Name == "after" && hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
}

func TestHybridStore_StaleNaturalKeysConvergeToSQLiteIDs(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	ctx := context.Background()
	staleChannel, err := primary.CreateConfig(ctx, &model.Config{
		Name: "reused-name", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://old.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	filler, err := sqlite.CreateConfig(ctx, &model.Config{
		Name: "filler", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://filler.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = filler

	tokenHash := model.HashToken("reused-token")
	staleToken := &model.AuthToken{Token: tokenHash, Description: "old", IsActive: true, CreatedAt: time.Now()}
	if err := primary.CreateAuthToken(ctx, staleToken); err != nil {
		t.Fatal(err)
	}
	fillerToken := &model.AuthToken{Token: model.HashToken("filler"), Description: "filler", IsActive: true, CreatedAt: time.Now()}
	if err := sqlite.CreateAuthToken(ctx, fillerToken); err != nil {
		t.Fatal(err)
	}

	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	currentChannel, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "reused-name", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://new.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	currentToken := &model.AuthToken{Token: tokenHash, Description: "new", IsActive: true, CreatedAt: time.Now()}
	if err := hybrid.CreateAuthToken(ctx, currentToken); err != nil {
		t.Fatal(err)
	}
	if currentChannel.ID == staleChannel.ID || currentToken.ID == staleToken.ID {
		t.Fatal("测试前提失败：SQLite 应分配不同 ID")
	}

	waitForCondition(t, 3*time.Second, func() bool {
		cfg, cfgErr := primary.GetConfig(ctx, currentChannel.ID)
		token, tokenErr := primary.GetAuthTokenByValue(ctx, tokenHash)
		return cfgErr == nil && cfg.Name == "reused-name" &&
			tokenErr == nil && token.ID == currentToken.ID &&
			hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
	if _, err := primary.GetConfig(ctx, staleChannel.ID); err == nil {
		t.Fatal("主库旧渠道 ID 未清理")
	}
}

func TestHybridStore_OAuthCASHasSingleSQLiteWinner(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()

	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "oauth", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: "old", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	var winners atomic.Int64
	var wg sync.WaitGroup
	for _, next := range []string{"winner-a", "winner-b"} {
		wg.Add(1)
		go func(credential string) {
			defer wg.Done()
			updated, casErr := hybrid.CompareAndSwapOAuthCredential(
				ctx, created.ID, model.AuthTypeCodexOAuth, "old", credential,
			)
			if casErr != nil {
				t.Errorf("CAS: %v", casErr)
				return
			}
			if updated {
				winners.Add(1)
			}
		}(next)
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("CAS winners=%d, want 1", got)
	}
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil || (got.OAuthCredential != "winner-a" && got.OAuthCredential != "winner-b") {
		t.Fatalf("winner credential = (%q, %v)", got.OAuthCredential, err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		primaryConfig, primaryErr := primary.GetConfig(ctx, created.ID)
		return primaryErr == nil && primaryConfig.OAuthCredential == got.OAuthCredential &&
			hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
}

func TestHybridStore_ChannelAuthTypeChangeConvergesByReplacingPrimaryAggregate(t *testing.T) {
	ctx := context.Background()
	sqlite := createTestSQLiteStore(t)
	primary := createTestSQLiteStore(t)
	if _, err := primary.CreateConfig(ctx, &model.Config{
		Name: "old-api-key", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://old.example"}}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "new-oauth", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: `{"access_token":"new"}`,
		URLs: model.ChannelURLs{{URL: "https://new.example"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, time.Second, func() bool {
		cfg, getErr := primary.GetConfig(ctx, created.ID)
		return getErr == nil && cfg.Name == "new-oauth" && cfg.AuthType == model.AuthTypeCodexOAuth
	})
}

func TestHybridStore_HighCardinalityDirtyStateCollapsesToFullReconcile(t *testing.T) {
	ctx := context.Background()
	sqlite := createTestSQLiteStore(t)
	primary := createTestSQLiteStore(t)
	release := make(chan struct{})
	hybrid := newHybridStore(sqlite, primary, func(initCtx context.Context) error {
		select {
		case <-release:
			return nil
		case <-initCtx.Done():
			return initCtx.Err()
		}
	})
	hybrid.primarySync.configureReconcile(2, hybrid.reconcilePrimary, hybrid.markPrimaryReconcileDirty)
	t.Cleanup(func() { _ = hybrid.Close() })

	const configCount = primaryReconcilePageSize + 5
	for i := 0; i < configCount; i++ {
		if _, err := hybrid.CreateConfig(ctx, &model.Config{
			Name: fmt.Sprintf("bounded-%d", i), AuthType: model.AuthTypeAPIKey,
			URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if pending := hybrid.RuntimeMetrics().PrimarySyncPending; pending != 2 {
		t.Fatalf("pending=%d, want initializer + one full reconciliation", pending)
	}
	close(release)
	waitForCondition(t, 2*time.Second, func() bool {
		configs, listErr := primary.ListConfigs(ctx)
		return listErr == nil && len(configs) == configCount && hybrid.RuntimeMetrics().PrimarySyncPending == 0
	})
}

func TestHybridStore_AnalyticsReadFailureCanUsePrimaryHistory(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()
	if err := primary.AddLog(ctx, &model.LogEntry{
		Time: model.JSONTime{Time: time.Now()}, StatusCode: 200, Message: "history",
	}); err != nil {
		t.Fatalf("seed primary log: %v", err)
	}
	if err := sqlite.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	logs, err := hybrid.ListLogs(ctx, time.Time{}, 10, 0, nil)
	if err != nil || len(logs) != 1 || logs[0].Message != "history" {
		t.Fatalf("history fallback = (%+v, %v)", logs, err)
	}
	if !hybrid.RuntimeMetrics().AnalyticsReadsPrimary {
		t.Fatal("分析读取失败后应记录主库降级")
	}
}

func TestHybridStore_AuthoritativeSQLiteFailureIsReturned(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	if err := sqlite.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	_, err := hybrid.CreateConfig(context.Background(), &model.Config{
		Name: "must-fail", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
	})
	if err == nil {
		t.Fatal("SQLite 权威写失败不能伪装成功")
	}
	if err := hybrid.Ping(context.Background()); err == nil {
		t.Fatal("SQLite 故障必须让健康检查失败")
	}
}

func TestPrimaryWriteBehindDoesNotClearNewGeneration(t *testing.T) {
	w := newPrimaryWriteBehind(10*time.Millisecond, time.Second)
	t.Cleanup(w.close)
	started := make(chan struct{})
	release := make(chan struct{})
	var final atomic.Int64
	w.enqueue("same", "old", func(context.Context) error {
		close(started)
		<-release
		final.Store(1)
		return nil
	})
	<-started
	w.enqueue("same", "new", func(context.Context) error {
		final.Store(2)
		return nil
	})
	close(release)
	waitForCondition(t, time.Second, func() bool { return final.Load() == 2 && w.pending() == 0 })
}

func TestPrimaryWriteBehindRetriesFailedFinalState(t *testing.T) {
	w := newPrimaryWriteBehind(10*time.Millisecond, time.Second)
	t.Cleanup(w.close)
	var attempts atomic.Int64
	w.enqueue("state", "retry", func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("temporary")
		}
		return nil
	})
	waitForCondition(t, time.Second, func() bool { return attempts.Load() >= 2 && w.pending() == 0 })
	if failures := w.failures.Load(); failures != 1 {
		t.Fatalf("failures=%d, want 1", failures)
	}
}

func TestPrimaryWriteBehindDoesNotRetryBestEffortLogs(t *testing.T) {
	w := newPrimaryWriteBehind(10*time.Millisecond, time.Second)
	t.Cleanup(w.close)
	var attempts atomic.Int64
	w.enqueueBestEffort("logs/latest", "logs", func(context.Context) error {
		attempts.Add(1)
		return errors.New("unavailable")
	})
	waitForCondition(t, time.Second, func() bool { return w.pending() == 0 })
	time.Sleep(30 * time.Millisecond)
	if attempts.Load() != 1 || w.dropped.Load() != 1 {
		t.Fatalf("best effort attempts=%d dropped=%d", attempts.Load(), w.dropped.Load())
	}
}

func TestPrimaryWriteBehindPublishesReconcileOnlyAfterDirtyMarker(t *testing.T) {
	w := newPrimaryWriteBehind(10*time.Millisecond, time.Second)
	t.Cleanup(w.close)
	dirtyStarted := make(chan struct{})
	releaseDirty := make(chan struct{})
	var dirtyOnce sync.Once
	var reconcileRuns atomic.Int64
	w.configureReconcile(1, func(context.Context) error {
		reconcileRuns.Add(1)
		return nil
	}, func() {
		dirtyOnce.Do(func() { close(dirtyStarted) })
		<-releaseDirty
	})

	w.enqueue("first", "first", func(context.Context) error {
		return errors.New("keep pending")
	})
	enqueued := make(chan struct{})
	go func() {
		w.enqueue("second", "second", func(context.Context) error { return nil })
		close(enqueued)
	}()
	<-dirtyStarted
	time.Sleep(20 * time.Millisecond)
	if reconcileRuns.Load() != 0 {
		close(releaseDirty)
		<-enqueued
		t.Fatal("reconciliation ran before its dirty marker was committed")
	}
	close(releaseDirty)
	<-enqueued
	waitForCondition(t, time.Second, func() bool { return reconcileRuns.Load() > 0 })
}

func TestHybridStore_CloseIsIdempotent(t *testing.T) {
	hybrid := NewHybridStore(createTestSQLiteStore(t), createTestSQLiteStore(t))
	if err := hybrid.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := hybrid.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestHybridStore_MySQLWriteBehindIntegration(t *testing.T) {
	dsn := os.Getenv("CCLOAD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CCLOAD_TEST_MYSQL_DSN 未设置")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 测试库: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		t.Fatalf("连接 MySQL 测试库: %v", err)
	}
	primary := sqlstore.NewSQLStore(db, "mysql")
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	localWriteStarted := time.Now()

	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name:     "writebehind-integration-" + time.Unix(0, suffix).Format("150405.000000000"),
		AuthType: model.AuthTypeAPIKey,
		URLs:     model.ChannelURLs{{URL: "https://example.com"}},
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("SQLite CreateConfig: %v", err)
	}
	if err := hybrid.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "integration-secret",
	}}); err != nil {
		t.Fatalf("SQLite CreateAPIKeysBatch: %v", err)
	}
	localWriteElapsed := time.Since(localWriteStarted)
	t.Logf("SQLite 请求侧两次写入耗时: %v", localWriteElapsed)
	if localWriteElapsed > 2*time.Second {
		t.Fatalf("主库延迟泄漏到请求侧: 本地写入耗时 %v", localWriteElapsed)
	}

	waitForCondition(t, 20*time.Second, func() bool {
		cfg, cfgErr := primary.GetConfig(ctx, created.ID)
		keys, keyErr := primary.GetAPIKeys(ctx, created.ID)
		return cfgErr == nil && cfg.Name == created.Name && keyErr == nil && len(keys) == 1
	})
	if failures := hybrid.RuntimeMetrics().PrimarySyncFailures; failures != 0 {
		t.Fatalf("MySQL 同步失败次数=%d", failures)
	}
	if err := primary.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("清理 MySQL 测试渠道: %v", err)
	}
}
