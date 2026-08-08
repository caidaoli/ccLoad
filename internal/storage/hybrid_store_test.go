package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

// createTestSQLiteStore 创建测试用的 SQLite store
func createTestSQLiteStore(t *testing.T) *sqlstore.SQLStore {
	t.Helper()
	tmpDB := t.TempDir() + "/hybrid_test.db"
	store, err := CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试 SQLite 失败: %v", err)
	}
	return store.(*sqlstore.SQLStore)
}

func TestHybridStore_BasicOperations(t *testing.T) {
	// 创建两个独立的 SQLite：一个模拟 MySQL（主存储），一个作为 SQLite 缓存
	mysql := createTestSQLiteStore(t)  // 用 SQLite 模拟 MySQL（主存储）
	sqlite := createTestSQLiteStore(t) // SQLite 缓存
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// 测试 CreateConfig - 应该先写 MySQL，再同步到 SQLite
	cfg := &model.Config{
		Name:     "test-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.openai.com"}},
		Priority: 100,
		Enabled:  true,
	}

	created, err := hybrid.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig 失败: %v", err)
	}
	if created.ID == 0 {
		t.Error("创建的配置 ID 不应为 0")
	}

	// 验证 MySQL（主存储）有数据
	mysqlCfg, err := mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL 主存储应该有数据: %v", err)
	}
	if mysqlCfg.Name != cfg.Name {
		t.Errorf("MySQL 数据不匹配: got %s, want %s", mysqlCfg.Name, cfg.Name)
	}

	// 测试 GetConfig（从 SQLite 缓存读取）
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig 失败: %v", err)
	}
	if got.Name != cfg.Name {
		t.Errorf("GetConfig 返回名称不匹配: got %s, want %s", got.Name, cfg.Name)
	}

	// 测试 ListConfigs
	list, err := hybrid.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListConfigs 返回数量不匹配: got %d, want 1", len(list))
	}

	// 测试 UpdateConfig
	cfg.Name = "updated-channel"
	updated, err := hybrid.UpdateConfig(ctx, created.ID, cfg)
	if err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if updated.Name != "updated-channel" {
		t.Errorf("UpdateConfig 返回名称不匹配: got %s, want updated-channel", updated.Name)
	}

	// 验证 MySQL 主存储已更新
	mysqlCfg, err = mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "updated-channel" {
		t.Errorf("MySQL 数据未更新: got %s, want updated-channel", mysqlCfg.Name)
	}

	// 测试 DeleteConfig
	err = hybrid.DeleteConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteConfig 失败: %v", err)
	}

	// 验证 MySQL 主存储已删除
	_, err = mysql.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("删除后 MySQL 应该返回错误")
	}

	// 验证 SQLite 缓存也已清理
	_, err = hybrid.GetConfig(ctx, created.ID)
	if err == nil {
		t.Error("删除后 SQLite 缓存应该返回错误")
	}
}

func TestHybridStore_CompareAndSwapOAuthCredentialKeepsReplicaOnWinner(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	stale := `{"type":"codex","access_token":"at-stale","refresh_token":"rt-stale","expired":"2032-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "codex-cas", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "*"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig() error = %v", err)
	}

	updated, err := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
	if err != nil || !updated {
		t.Fatalf("winner CompareAndSwapOAuthCredential() = (%v, %v), want (true, nil)", updated, err)
	}
	updated, err = hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, stale)
	if err != nil || updated {
		t.Fatalf("stale CompareAndSwapOAuthCredential() = (%v, %v), want (false, nil)", updated, err)
	}
	for name, store := range map[string]*sqlstore.SQLStore{"primary": mysql, "replica": sqlite} {
		persisted, getErr := store.GetConfig(ctx, created.ID)
		if getErr != nil {
			t.Fatalf("%s GetConfig() error = %v", name, getErr)
		}
		if persisted.OAuthCredential != winner {
			t.Fatalf("%s credential = %q, want winner", name, persisted.OAuthCredential)
		}
	}
}

func TestHybridStore_CompareAndSwapOAuthCredentialMissRestoresReadMode(t *testing.T) {
	t.Run("remote winner repairs stale replica", func(t *testing.T) {
		mysql := createTestSQLiteStore(t)
		sqlite := createTestSQLiteStore(t)
		defer func() {
			_ = sqlite.Close()
			_ = mysql.Close()
		}()
		hybrid := NewHybridStore(sqlite, mysql)
		defer func() { _ = hybrid.Close() }()
		ctx := context.Background()
		initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
		winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
		created, err := hybrid.CreateConfig(ctx, &model.Config{
			Name: "cas-miss-read-mode", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
			URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true,
			ModelEntries: []model.ModelEntry{{Model: "gpt-old"}}, ScheduledCheckModel: "gpt-old",
		})
		if err != nil {
			t.Fatal(err)
		}
		updated, err := mysql.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
		if err != nil || !updated {
			t.Fatalf("persist remote winner = (%v, %v)", updated, err)
		}
		modelsUpdated, err := mysql.UpdateOAuthModelStateIfCredentialMatches(
			ctx, created.ID, model.AuthTypeCodexOAuth, winner,
			[]model.ModelEntry{{Model: "gpt-winner"}}, "gpt-winner",
		)
		if err != nil || !modelsUpdated {
			t.Fatalf("persist remote winner models = (%v, %v)", modelsUpdated, err)
		}
		updated, err = hybrid.CompareAndSwapOAuthCredential(
			ctx, created.ID, model.AuthTypeCodexOAuth, initial, initial+"-loser",
		)
		if err != nil || updated {
			t.Fatalf("CompareAndSwapOAuthCredential() = (%v, %v), want miss", updated, err)
		}
		got, err := hybrid.GetConfig(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.OAuthCredential != winner || !got.SupportsModel("gpt-winner") {
			t.Fatalf("GetConfig() = %#v, want primary winner", got)
		}
		configs, err := hybrid.ListConfigs(ctx)
		if err != nil || len(configs) != 1 || configs[0].OAuthCredential != winner || !configs[0].SupportsModel("gpt-winner") {
			t.Fatalf("ListConfigs() = (%#v, %v), want repaired winner", configs, err)
		}
		enabled, err := hybrid.GetEnabledChannelsByModel(ctx, "gpt-winner")
		if err != nil || len(enabled) != 1 || enabled[0].OAuthCredential != winner {
			t.Fatalf("GetEnabledChannelsByModel() = (%#v, %v), want repaired winner", enabled, err)
		}
		if err := mysql.Close(); err != nil {
			t.Fatal(err)
		}
		got, err = hybrid.GetConfig(ctx, created.ID)
		if err != nil || got.OAuthCredential != winner || !got.SupportsModel("gpt-winner") {
			t.Fatalf("replica after primary close = (%#v, %v), want repaired winner", got, err)
		}
	})

	t.Run("missing primary deletes replica and clears fallback", func(t *testing.T) {
		mysql := createTestSQLiteStore(t)
		sqlite := createTestSQLiteStore(t)
		defer func() {
			_ = sqlite.Close()
			_ = mysql.Close()
		}()
		hybrid := NewHybridStore(sqlite, mysql)
		defer func() { _ = hybrid.Close() }()
		ctx := context.Background()
		initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
		created, err := hybrid.CreateConfig(ctx, &model.Config{
			Name: "cas-miss-deleted-primary", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
			URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := mysql.DeleteConfig(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		updated, err := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, initial+"-loser")
		if err != nil || updated {
			t.Fatalf("CompareAndSwapOAuthCredential() = (%v, %v), want missing-primary miss", updated, err)
		}
		if got, err := hybrid.GetConfig(ctx, created.ID); err == nil || got != nil {
			t.Fatalf("GetConfig() = (%#v, %v), want authoritative deletion", got, err)
		}
		if err := mysql.Close(); err != nil {
			t.Fatal(err)
		}
		configs, err := hybrid.ListConfigs(ctx)
		if err != nil || len(configs) != 0 {
			t.Fatalf("ListConfigs() after primary close = (%#v, %v), want cleared replica fallback", configs, err)
		}
	})
}

func TestHybridStore_UpdateOAuthModelStateMissRepairsRemoteWinner(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "model-state-cas-miss", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-old"}}, ScheduledCheckModel: "gpt-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := mysql.UpdateOAuthModelStateIfCredentialMatches(
		ctx, created.ID, model.AuthTypeCodexOAuth, credential,
		[]model.ModelEntry{{Model: "gpt-winner"}}, "gpt-winner",
	)
	if err != nil || !updated {
		t.Fatalf("persist remote model winner = (%v, %v)", updated, err)
	}
	updated, err = hybrid.UpdateOAuthModelStateIfCredentialMatches(
		ctx, created.ID, model.AuthTypeCodexOAuth, credential+"-stale",
		[]model.ModelEntry{{Model: "gpt-loser"}}, "gpt-loser",
	)
	if err != nil || updated {
		t.Fatalf("UpdateOAuthModelStateIfCredentialMatches() = (%v, %v), want miss", updated, err)
	}
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil || !got.SupportsModel("gpt-winner") || got.SupportsModel("gpt-old") || got.ScheduledCheckModel != "gpt-winner" {
		t.Fatalf("GetConfig() = (%#v, %v), want remote winner models", got, err)
	}
	if err := mysql.Close(); err != nil {
		t.Fatal(err)
	}
	enabled, err := hybrid.GetEnabledChannelsByModel(ctx, "gpt-winner")
	if err != nil || len(enabled) != 1 || enabled[0].ScheduledCheckModel != "gpt-winner" {
		t.Fatalf("replica winner models = (%#v, %v)", enabled, err)
	}
}

func TestHybridStore_DeleteOAuthConfigRecoversReplicaDeletionFailure(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "delete-replica-failure", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.ExecContext(ctx, `
		CREATE TRIGGER reject_oauth_delete
		BEFORE DELETE ON channels
		BEGIN
			SELECT RAISE(FAIL, 'replica delete rejected');
		END
	`); err != nil {
		t.Fatal(err)
	}
	if err := hybrid.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("DeleteConfig() error = %v", err)
	}
	configs, err := hybrid.ListConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Fatalf("ListConfigs() after primary deletion = (%#v, %v), want empty", configs, err)
	}
	if _, err := sqlite.ExecContext(ctx, `DROP TRIGGER reject_oauth_delete`); err != nil {
		t.Fatal(err)
	}
	configs, err = hybrid.ListConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Fatalf("ListConfigs() during repair = (%#v, %v), want empty", configs, err)
	}
	if err := mysql.Close(); err != nil {
		t.Fatal(err)
	}
	configs, err = hybrid.ListConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Fatalf("ListConfigs() after repair and primary close = (%#v, %v), want replica read", configs, err)
	}
}

func TestHybridStore_DeleteOAuthConfigSerializesWithCredentialCAS(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "delete-during-cas", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
		URLs: model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := mysql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `UPDATE channels SET updated_at = updated_at WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	casDone := make(chan error, 1)
	go func() {
		updated, casErr := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
		if casErr == nil && !updated {
			casErr = errors.New("OAuth CAS missed")
		}
		casDone <- casErr
	}()
	time.Sleep(10 * time.Millisecond)
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- hybrid.DeleteConfig(ctx, created.ID) }()
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteConfig crossed in-flight CAS: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-casDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := mysql.Close(); err != nil {
		t.Fatal(err)
	}
	configs, err := hybrid.ListConfigs(ctx)
	if err != nil || len(configs) != 0 {
		t.Fatalf("ListConfigs() after serialized delete = (%#v, %v), want empty replica read", configs, err)
	}
}

func TestHybridStore_CompareAndSwapOAuthCredentialHealsDriftedReplica(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
	drifted := `{"type":"codex","access_token":"at-drifted","refresh_token":"rt-drifted","expired":"2030-01-01T00:00:00Z"}`
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "codex-drift", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	driftedReplica, err := sqlite.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, drifted)
	if err != nil || !driftedReplica {
		t.Fatalf("drift replica = (%v, %v)", driftedReplica, err)
	}

	updated, err := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
	if err != nil || !updated {
		t.Fatalf("CompareAndSwapOAuthCredential() = (%v, %v), want healed success", updated, err)
	}
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCredential != winner {
		t.Fatalf("GetConfig() credential = %q, want winner", got.OAuthCredential)
	}
	replica, err := sqlite.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replica.OAuthCredential != winner {
		t.Fatalf("SQLite credential = %q, want repaired winner", replica.OAuthCredential)
	}
}

func TestHybridStore_OAuthReplicaFailureFallsBackForAllConfigReadsAndRecovers(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "codex-replica-failure", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-old"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.ExecContext(ctx, `
		CREATE TRIGGER reject_oauth_credential_update
		BEFORE UPDATE OF oauth_credential ON channels
		BEGIN
			SELECT RAISE(FAIL, 'oauth credential replica is read only');
		END
	`); err != nil {
		t.Fatal(err)
	}

	updated, err := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
	if !updated || err != nil {
		t.Fatalf("CompareAndSwapOAuthCredential() = (%v, %v), want committed primary success", updated, err)
	}
	modelsUpdated, err := hybrid.UpdateOAuthModelStateIfCredentialMatches(
		ctx, created.ID, model.AuthTypeCodexOAuth, winner,
		[]model.ModelEntry{{Model: "gpt-winner"}}, "gpt-winner",
	)
	if !modelsUpdated || err != nil {
		t.Fatalf("UpdateOAuthModelStateIfCredentialMatches() = (%v, %v), want committed primary success", modelsUpdated, err)
	}
	got, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() after replica failure error = %v", err)
	}
	if got.OAuthCredential != winner || !got.SupportsModel("gpt-winner") {
		t.Fatalf("GetConfig() = %#v, want primary winner", got)
	}
	configs, err := hybrid.ListConfigs(ctx)
	if err != nil || len(configs) != 1 || configs[0].OAuthCredential != winner || !configs[0].SupportsModel("gpt-winner") {
		t.Fatalf("ListConfigs() = (%#v, %v), want primary winner", configs, err)
	}
	enabled, err := hybrid.GetEnabledChannelsByModel(ctx, "gpt-winner")
	if err != nil || len(enabled) != 1 || enabled[0].OAuthCredential != winner {
		t.Fatalf("GetEnabledChannelsByModel() = (%#v, %v), want primary winner", enabled, err)
	}
	if _, err := sqlite.ExecContext(ctx, `DROP TRIGGER reject_oauth_credential_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := hybrid.GetConfig(ctx, created.ID); err != nil {
		t.Fatalf("GetConfig() recovery error = %v", err)
	}
	if err := mysql.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := hybrid.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() after primary close error = %v", err)
	}
	if recovered.OAuthCredential != winner || !recovered.SupportsModel("gpt-winner") || recovered.ScheduledCheckModel != "gpt-winner" {
		t.Fatalf("recovered SQLite config = %#v, want repaired winner", recovered)
	}
}

func TestHybridStore_ConfigCollectionReadsWaitForOAuthCAS(t *testing.T) {
	readers := map[string]func(context.Context, *HybridStore) ([]*model.Config, error){
		"list": func(ctx context.Context, store *HybridStore) ([]*model.Config, error) {
			return store.ListConfigs(ctx)
		},
		"enabled": func(ctx context.Context, store *HybridStore) ([]*model.Config, error) {
			return store.GetEnabledChannelsByModel(ctx, "gpt-test")
		},
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			mysql := createTestSQLiteStore(t)
			sqlite := createTestSQLiteStore(t)
			defer func() {
				_ = sqlite.Close()
				_ = mysql.Close()
			}()
			hybrid := NewHybridStore(sqlite, mysql)
			defer func() { _ = hybrid.Close() }()
			ctx := context.Background()
			initial := `{"type":"codex","access_token":"at-old","refresh_token":"rt-old","expired":"2030-01-01T00:00:00Z"}`
			winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
			created, err := hybrid.CreateConfig(ctx, &model.Config{
				Name: "codex-read-lock-" + name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: initial,
				URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
				Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := mysql.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.ExecContext(ctx, `UPDATE channels SET updated_at = updated_at WHERE id = ?`, created.ID); err != nil {
				t.Fatal(err)
			}
			casDone := make(chan error, 1)
			casStarted := make(chan struct{})
			go func() {
				close(casStarted)
				updated, casErr := hybrid.CompareAndSwapOAuthCredential(ctx, created.ID, model.AuthTypeCodexOAuth, initial, winner)
				if casErr == nil && !updated {
					casErr = errors.New("OAuth CAS missed")
				}
				casDone <- casErr
			}()
			<-casStarted
			// The primary transaction above keeps the CAS inside its public call.
			// Give that goroutine a scheduling turn before starting the competing read.
			time.Sleep(10 * time.Millisecond)
			readDone := make(chan []*model.Config, 1)
			readErr := make(chan error, 1)
			go func() {
				configs, readError := read(ctx, hybrid)
				readDone <- configs
				readErr <- readError
			}()
			select {
			case configs := <-readDone:
				t.Fatalf("collection read crossed in-flight CAS: %#v", configs)
			case <-time.After(50 * time.Millisecond):
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-casDone; err != nil {
				t.Fatal(err)
			}
			configs := <-readDone
			if err := <-readErr; err != nil {
				t.Fatal(err)
			}
			if len(configs) != 1 || configs[0].OAuthCredential != winner {
				t.Fatalf("collection read = %#v, want committed winner", configs)
			}
		})
	}
}

func TestHybridStore_InitialOAuthWritesFallBackAndRecoverMissingReplica(t *testing.T) {
	writers := map[string]func(context.Context, *HybridStore, *model.Config) (*model.Config, error){
		"create": func(ctx context.Context, store *HybridStore, cfg *model.Config) (*model.Config, error) {
			return store.CreateConfig(ctx, cfg)
		},
		"import": func(ctx context.Context, store *HybridStore, cfg *model.Config) (*model.Config, error) {
			created, updated, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{Config: cfg}})
			if err != nil {
				return nil, err
			}
			if created != 1 || updated != 0 {
				return nil, fmt.Errorf("import counts = (%d, %d)", created, updated)
			}
			return cfg, nil
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			mysql := createTestSQLiteStore(t)
			sqlite := createTestSQLiteStore(t)
			defer func() {
				_ = sqlite.Close()
				_ = mysql.Close()
			}()
			hybrid := NewHybridStore(sqlite, mysql)
			defer func() { _ = hybrid.Close() }()
			ctx := context.Background()
			if _, err := sqlite.ExecContext(ctx, `
				CREATE TRIGGER reject_oauth_channel_insert
				BEFORE INSERT ON channels WHEN NEW.auth_type <> 'api_key'
				BEGIN
					SELECT RAISE(FAIL, 'oauth channel replica is read only');
				END
			`); err != nil {
				t.Fatal(err)
			}
			credential := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
			created, err := write(ctx, hybrid, &model.Config{
				Name: "codex-initial-" + name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
				URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
				Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-winner"}},
			})
			if err != nil {
				t.Fatalf("initial OAuth %s error = %v", name, err)
			}
			if _, err := sqlite.GetConfig(ctx, created.ID); err == nil {
				t.Fatal("OAuth channel unexpectedly exists in blocked replica")
			}
			configs, err := hybrid.ListConfigs(ctx)
			if err != nil || len(configs) != 1 || configs[0].OAuthCredential != credential {
				t.Fatalf("ListConfigs() = (%#v, %v), want primary OAuth channel", configs, err)
			}
			enabled, err := hybrid.GetEnabledChannelsByModel(ctx, "gpt-winner")
			if err != nil || len(enabled) != 1 {
				t.Fatalf("GetEnabledChannelsByModel() = (%#v, %v), want primary OAuth channel", enabled, err)
			}
			if _, err := sqlite.ExecContext(ctx, `DROP TRIGGER reject_oauth_channel_insert`); err != nil {
				t.Fatal(err)
			}
			if _, err := hybrid.GetConfig(ctx, created.ID); err != nil {
				t.Fatalf("GetConfig() recovery error = %v", err)
			}
			if err := mysql.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := hybrid.GetConfig(ctx, created.ID)
			if err != nil || recovered.OAuthCredential != credential || !recovered.SupportsModel("gpt-winner") {
				t.Fatalf("recovered config = (%#v, %v)", recovered, err)
			}
		})
	}
}

func TestHybridStore_ImportOAuthUsesPrimarySnapshotAfterConcurrentWinner(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	input := `{"type":"codex","access_token":"at-input","refresh_token":"rt-input","expired":"2030-01-01T00:00:00Z"}`
	winner := `{"type":"codex","access_token":"at-winner","refresh_token":"rt-winner","expired":"2031-01-01T00:00:00Z"}`
	const channelID int64 = 77
	cfg := &model.Config{
		ID: channelID, Name: "codex-import-winner", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: input,
		URLs:    model.ChannelURLs{{URL: "https://example.com", Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "gpt-input"}}, ScheduledCheckModel: "gpt-input",
	}
	blocker, err := sqlite.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `UPDATE channels SET updated_at = updated_at WHERE id = -1`); err != nil {
		t.Fatal(err)
	}
	importDone := make(chan error, 1)
	go func() {
		_, _, importErr := hybrid.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{Config: cfg}})
		importDone <- importErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		primary, getErr := mysql.GetConfig(ctx, channelID)
		if getErr == nil && primary.OAuthCredential == input {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("primary import did not commit: %v", getErr)
		}
		time.Sleep(time.Millisecond)
	}
	updated, err := mysql.CompareAndSwapOAuthCredential(ctx, channelID, model.AuthTypeCodexOAuth, input, winner)
	if err != nil || !updated {
		t.Fatalf("primary winner CAS = (%v, %v)", updated, err)
	}
	modelsUpdated, err := mysql.UpdateOAuthModelStateIfCredentialMatches(
		ctx, channelID, model.AuthTypeCodexOAuth, winner,
		[]model.ModelEntry{{Model: "gpt-winner"}}, "gpt-winner",
	)
	if err != nil || !modelsUpdated {
		t.Fatalf("primary winner models = (%v, %v)", modelsUpdated, err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-importDone; err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() ([]*model.Config, error){
		"get": func() ([]*model.Config, error) {
			got, getErr := hybrid.GetConfig(ctx, channelID)
			return []*model.Config{got}, getErr
		},
		"list":    func() ([]*model.Config, error) { return hybrid.ListConfigs(ctx) },
		"enabled": func() ([]*model.Config, error) { return hybrid.GetEnabledChannelsByModel(ctx, "gpt-winner") },
	} {
		configs, readErr := read()
		if readErr != nil || len(configs) != 1 || configs[0] == nil || configs[0].OAuthCredential != winner ||
			!configs[0].SupportsModel("gpt-winner") || configs[0].ScheduledCheckModel != "gpt-winner" {
			t.Fatalf("%s = (%#v, %v), want primary winner snapshot", name, configs, readErr)
		}
	}
}

func TestHybridStore_OAuthReplicaRecoveryCopiesCompletePrimaryConfig(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	const channelID int64 = 91
	staleCredential := `{"type":"codex","access_token":"stale","refresh_token":"stale-rt"}`
	winnerCredential := `{"type":"codex","access_token":"winner","refresh_token":"winner-rt"}`
	if _, err := sqlite.CreateConfig(ctx, &model.Config{
		ID: channelID, Name: "stale-name", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: staleCredential,
		URLs:     model.ChannelURLs{{URL: "https://stale.example.com", Protocols: []string{"openai"}}},
		Priority: -1, RPMLimit: 1, MaxConcurrency: 1, Websockets: false,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream, Enabled: false,
		ScheduledCheckEnabled: false, ScheduledCheckModel: "stale-model",
		DailyCostLimit: 1, CostMultiplier: 2, ProxyURL: "http://stale-proxy.example.com",
		CustomRequestRules: &model.CustomRequestRules{Headers: []model.CustomHeaderRule{{
			Action: model.RuleActionOverride, Name: "X-Stale", Value: "true",
		}}},
		RetryOtherKeysOnFailure: false, ModelEntries: []model.ModelEntry{{Model: "stale-model"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.ExecContext(ctx, `UPDATE channels SET cooldown_until = 123, cooldown_duration_ms = 456 WHERE id = ?`, channelID); err != nil {
		t.Fatal(err)
	}
	winner := &model.Config{
		ID: channelID, Name: "winner-name", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: winnerCredential,
		URLs: model.ChannelURLs{
			{URL: "https://winner.example.com/v1", Exact: true, Protocols: []string{"codex"}},
			{URL: "https://winner-backup.example.com", Protocols: []string{"openai"}},
		},
		Priority: 42, RPMLimit: 120, MaxConcurrency: 7, Websockets: true,
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true,
		ScheduledCheckEnabled: true, ScheduledCheckModel: "winner-model",
		DailyCostLimit: 33.5, CostMultiplier: 0.75, ProxyURL: "socks5://winner-proxy.example.com:1080",
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "winner-rule", Priority: 1, StatusCodes: []int{429},
			Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
		RetryOtherKeysOnFailure: true,
		ModelEntries:            []model.ModelEntry{{Model: "winner-model", RedirectModel: "winner-upstream"}},
	}
	created, updated, err := hybrid.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{Config: winner}})
	if err != nil || created != 1 || updated != 0 {
		t.Fatalf("ImportChannelBatch() = (%d, %d, %v)", created, updated, err)
	}
	primary, err := mysql.GetConfig(ctx, channelID)
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.Close(); err != nil {
		t.Fatal(err)
	}
	assertWinner := func(source string, got *model.Config) {
		t.Helper()
		if got == nil || got.ID != primary.ID || got.Name != primary.Name || got.GetAuthType() != primary.GetAuthType() ||
			got.OAuthCredential != primary.OAuthCredential || !reflect.DeepEqual(got.URLs, primary.URLs) ||
			got.Priority != primary.Priority || got.RPMLimit != primary.RPMLimit || got.MaxConcurrency != primary.MaxConcurrency ||
			got.Websockets != primary.Websockets || got.ProtocolTransformMode != primary.ProtocolTransformMode ||
			got.Enabled != primary.Enabled || got.ScheduledCheckEnabled != primary.ScheduledCheckEnabled ||
			got.ScheduledCheckModel != primary.ScheduledCheckModel || got.DailyCostLimit != primary.DailyCostLimit ||
			got.CostMultiplier != primary.CostMultiplier || got.ProxyURL != primary.ProxyURL ||
			got.CooldownUntil != primary.CooldownUntil || got.CooldownDurationMs != primary.CooldownDurationMs ||
			!reflect.DeepEqual(got.CustomRequestRules, primary.CustomRequestRules) ||
			!reflect.DeepEqual(got.CooldownDetectionRules, primary.CooldownDetectionRules) ||
			got.RetryOtherKeysOnFailure != primary.RetryOtherKeysOnFailure || !reflect.DeepEqual(got.ModelEntries, primary.ModelEntries) {
			t.Fatalf("%s stale replica = %+v, want %+v", source, got, primary)
		}
	}
	got, err := hybrid.GetConfig(ctx, channelID)
	if err != nil {
		t.Fatal(err)
	}
	assertWinner("GetConfig", got)
	listed, err := hybrid.ListConfigs(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListConfigs() = (%+v, %v)", listed, err)
	}
	assertWinner("ListConfigs", listed[0])
	enabled, err := hybrid.GetEnabledChannelsByModel(ctx, "winner-model")
	if err != nil || len(enabled) != 1 {
		t.Fatalf("GetEnabledChannelsByModel() = (%+v, %v)", enabled, err)
	}
	assertWinner("GetEnabledChannelsByModel", enabled[0])
}

func TestHybridStore_BatchPatchOAuthModelsSerializesAfterConditionalCommit(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()
	ctx := context.Background()
	credential := `{"type":"codex","access_token":"winner","refresh_token":"winner-rt"}`
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name: "codex-batch-patch-order", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "old-model"}}, ScheduledCheckModel: "old-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := sqlite.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `UPDATE channels SET updated_at = updated_at WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	commitDone := make(chan error, 1)
	go func() {
		updated, updateErr := hybrid.UpdateOAuthModelStateIfCredentialMatches(
			ctx, created.ID, model.AuthTypeCodexOAuth, credential,
			[]model.ModelEntry{{Model: "winner-model"}}, "winner-model",
		)
		if updateErr == nil && !updated {
			updateErr = errors.New("conditional OAuth model update missed")
		}
		commitDone <- updateErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		primary, getErr := mysql.GetConfig(ctx, created.ID)
		if getErr == nil && primary.SupportsModel("winner-model") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("primary conditional update did not commit: %v", getErr)
		}
		time.Sleep(time.Millisecond)
	}
	patchDone := make(chan error, 1)
	go func() {
		_, patchErr := hybrid.BatchPatchConfigs(ctx, []int64{created.ID}, model.BatchConfigPatch{
			ModelImportMode: model.ModelImportModeReplace,
			ModelEntries:    []model.ModelEntry{{Model: "admin-model"}},
		})
		patchDone <- patchErr
	}()
	select {
	case patchErr := <-patchDone:
		t.Fatalf("BatchPatchConfigs crossed in-flight OAuth commit: %v", patchErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	if err := <-patchDone; err != nil {
		t.Fatal(err)
	}
	for source, store := range map[string]*sqlstore.SQLStore{"primary": mysql, "replica": sqlite} {
		got, getErr := store.GetConfig(ctx, created.ID)
		if getErr != nil || !got.SupportsModel("admin-model") || got.ScheduledCheckModel != "" ||
			!got.UsesCodexOAuth() || got.OAuthCredential != credential {
			t.Fatalf("%s state = (%+v, %v)", source, got, getErr)
		}
	}
}

func TestHybridStore_AuthToken_IDFromMySQL(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	token := &model.AuthToken{
		Token:       fmt.Sprintf("token-%d", time.Now().UnixNano()),
		Description: "test",
		IsActive:    true,
	}
	if err := hybrid.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken 失败: %v", err)
	}
	if token.ID == 0 {
		t.Fatalf("token.ID 不应为 0")
	}

	// ID 来自 MySQL 主存储
	mysqlToken, err := mysql.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("MySQL GetAuthToken 失败: %v", err)
	}
	if mysqlToken.Token != token.Token {
		t.Fatalf("MySQL token 不匹配")
	}

	// SQLite 缓存也应该有相同数据
	sqliteToken, err := sqlite.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("SQLite GetAuthToken 失败: %v", err)
	}
	if sqliteToken.ID != token.ID {
		t.Fatalf("SQLite token ID 不匹配: got %d, want %d", sqliteToken.ID, token.ID)
	}
}

func TestHybridStore_ImportChannelBatch(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	channels := []*model.ChannelWithKeys{
		{
			Config: &model.Config{
				Name:     "import-chan",
				URLs:     model.ChannelURLs{{URL: "https://example.com"}},
				Priority: 10,
				Enabled:  true,
				ModelEntries: []model.ModelEntry{
					{Model: "gpt-4.1"},
				},
			},
			APIKeys: []model.APIKey{
				{KeyIndex: 0, APIKey: "sk-test-0", KeyStrategy: model.KeyStrategySequential},
				{KeyIndex: 1, APIKey: "sk-test-1", KeyStrategy: model.KeyStrategySequential},
			},
		},
	}

	created, updated, err := hybrid.ImportChannelBatch(ctx, channels)
	if err != nil {
		t.Fatalf("ImportChannelBatch 失败: %v", err)
	}
	if created != 1 || updated != 0 {
		t.Fatalf("ImportChannelBatch 计数不符合预期: created=%d updated=%d", created, updated)
	}
	if channels[0].Config.ID == 0 {
		t.Fatalf("导入后 channels[0].Config.ID 不应为 0")
	}
	id := channels[0].Config.ID

	// MySQL 主存储应该有数据
	mysqlCfg, err := mysql.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "import-chan" {
		t.Fatalf("MySQL 渠道名称不匹配: got %s, want %s", mysqlCfg.Name, "import-chan")
	}

	// SQLite 缓存也应该有数据
	sqliteCfg, err := sqlite.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("SQLite GetConfig 失败: %v", err)
	}
	if sqliteCfg.Name != "import-chan" {
		t.Fatalf("SQLite 渠道名称不匹配: got %s, want %s", sqliteCfg.Name, "import-chan")
	}

	// 验证 API Keys
	keys, err := mysql.GetAPIKeys(ctx, id)
	if err != nil {
		t.Fatalf("MySQL GetAPIKeys 失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("MySQL API Keys 数量不匹配: got %d, want %d", len(keys), 2)
	}
}

func TestHybridStore_LogsAsync_ClonesInputs(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	// logs 写 SQLite + 异步同步到 MySQL
	// AddLog 返回后修改入参对象，不应与后台同步产生数据竞争
	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  1,
		Model:      "gpt-4",
		StatusCode: 200,
		Duration:   1.5,
	}
	if err := hybrid.AddLog(ctx, entry); err != nil {
		t.Fatalf("AddLog 失败: %v", err)
	}

	// 并发修改入参（测试克隆是否正确）
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				entry.Message = fmt.Sprintf("m-%d", time.Now().UnixNano())
				entry.Duration += 0.001
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)

	// 验证 SQLite 有数据
	logs, err := hybrid.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("ListLogs 返回数量不匹配: got %d, want 1", len(logs))
	}
}

func TestHybridStore_SyncQueueLen(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	// 初始队列应该为空
	if qLen := hybrid.SyncQueueLen(); qLen != 0 {
		t.Errorf("初始队列长度应为 0, got %d", qLen)
	}
}

func TestHybridStore_AddLog(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()

	entry := &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  1,
		Model:      "gpt-4",
		StatusCode: 200,
		Duration:   1.5,
	}

	err := hybrid.AddLog(ctx, entry)
	if err != nil {
		t.Fatalf("AddLog 失败: %v", err)
	}

	// 验证 SQLite 有数据（日志先写 SQLite）
	logs, err := hybrid.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("ListLogs 返回数量不匹配: got %d, want 1", len(logs))
	}

	// 等待异步同步到 MySQL（条件等待，避免固定 sleep 造成漂移/假绿）
	deadline := time.Now().Add(2 * time.Second)
	for {
		mysqlLogs, err := mysql.ListLogs(ctx, time.Now().Add(-1*time.Hour), 10, 0, nil)
		if err != nil {
			t.Fatalf("MySQL ListLogs 失败: %v", err)
		}
		if len(mysqlLogs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 MySQL 异步同步超时：got %d logs, want 1", len(mysqlLogs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHybridStore_BatchAddLogs_DoesNotDuplicateSurvivingLogsAfterDeletedChannelFilter(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	created, err := hybrid.CreateConfig(ctx, &model.Config{
		Name:    "deleted-channel",
		URLs:    model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := hybrid.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	now := time.Now()
	if err := hybrid.BatchAddLogs(ctx, []*model.LogEntry{
		{
			Time:       model.JSONTime{Time: now},
			ChannelID:  created.ID,
			Model:      "stale-channel-log",
			StatusCode: 200,
		},
		{
			Time:       model.JSONTime{Time: now},
			ChannelID:  0,
			Model:      "system-log",
			StatusCode: 503,
		},
	}); err != nil {
		t.Fatalf("batch add logs: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var logs []*model.LogEntry
	for {
		logs, err = mysql.ListLogsRange(ctx, now.Add(-time.Minute), now.Add(time.Minute), 10, 0, nil)
		if err != nil {
			t.Fatalf("mysql.ListLogsRange failed: %v", err)
		}
		if len(logs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected surviving system log to sync to MySQL")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(logs) != 1 {
		t.Fatalf("expected exactly one surviving log synced to MySQL, got %+v", logs)
	}
	if logs[0].ChannelID != 0 || logs[0].Model != "system-log" {
		t.Fatalf("unexpected synced log: %+v", logs[0])
	}
}

func TestHybridStore_GracefulClose(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)

	hybrid := NewHybridStore(sqlite, mysql)

	ctx := context.Background()

	// 添加一些日志触发异步同步任务
	for i := 0; i < 10; i++ {
		entry := &model.LogEntry{
			Time:       model.JSONTime{Time: time.Now()},
			ChannelID:  int64(i),
			Model:      "gpt-4",
			StatusCode: 200,
			Duration:   1.5,
		}
		_ = hybrid.AddLog(ctx, entry)
	}

	// 关闭应该等待同步任务完成
	err := hybrid.Close()
	if err != nil {
		t.Errorf("Close 失败: %v", err)
	}

	// 多次关闭应该是幂等的
	err = hybrid.Close()
	if err != nil {
		t.Errorf("第二次 Close 失败: %v", err)
	}
}

func TestHybridStore_SQLiteCacheFailureDoesNotBlockWrite(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)

	ctx := context.Background()

	// 创建一个配置
	cfg := &model.Config{
		Name:     "test-channel",
		URLs:     model.ChannelURLs{{URL: "https://api.openai.com"}},
		Priority: 100,
		Enabled:  true,
	}

	created, err := hybrid.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig 失败: %v", err)
	}

	// 关闭 SQLite（模拟缓存失败）
	_ = sqlite.Close()

	// 更新操作应该成功（MySQL 写入成功即可）
	cfg.Name = "updated-channel"
	_, err = hybrid.UpdateConfig(ctx, created.ID, cfg)
	if err != nil {
		t.Fatalf("UpdateConfig 应该成功（MySQL 是主存储）: %v", err)
	}

	// 验证 MySQL 有更新
	mysqlCfg, err := mysql.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("MySQL GetConfig 失败: %v", err)
	}
	if mysqlCfg.Name != "updated-channel" {
		t.Errorf("MySQL 数据未更新: got %s, want updated-channel", mysqlCfg.Name)
	}

	_ = hybrid.Close()
}

func TestHybridStoreCleanupLogsBeforeIgnoresSQLiteCacheFailure(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, mysql)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := mysql.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: oldTime},
		ChannelID:  1,
		Model:      "gpt-4o",
		StatusCode: 200,
		Duration:   0.1,
	}); err != nil {
		t.Fatalf("AddLog to mysql failed: %v", err)
	}

	if err := sqlite.Close(); err != nil {
		t.Fatalf("close sqlite cache failed: %v", err)
	}

	if err := hybrid.CleanupLogsBefore(ctx, time.Now()); err != nil {
		t.Fatalf("CleanupLogsBefore returned sqlite cache error: %v", err)
	}

	logs, err := mysql.ListLogs(ctx, time.Now().Add(-24*time.Hour), 10, 0, nil)
	if err != nil {
		t.Fatalf("ListLogs from mysql failed: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("mysql still has %d old logs, want 0", len(logs))
	}
}

func TestHybridStore_FingerprintIDsStayAlignedWithPrimary(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, mysql)
	t.Cleanup(func() { _ = hybrid.Close() })

	ctx := context.Background()
	newFingerprint := func(name string) *model.ModelFingerprint {
		return &model.ModelFingerprint{
			Name:          name,
			Model:         "gpt-test",
			SampleCount:   3,
			Distribution:  []float64{0.5, 0.25, 0.25},
			Stats:         model.FingerprintStats{Mean: 2, Median: 2, Min: 1, Max: 3, Unique: 3, Mode: 1, ModeCount: 1},
			RawData:       []int{1, 2, 3},
			PromptVersion: "v1",
		}
	}
	for _, name := range []string{"primary-only-1", "primary-only-2"} {
		if _, err := mysql.CreateModelFingerprint(ctx, newFingerprint(name)); err != nil {
			t.Fatalf("seed primary fingerprint: %v", err)
		}
	}
	if _, err := sqlite.CreateModelFingerprint(ctx, newFingerprint("replica-only")); err != nil {
		t.Fatalf("seed replica fingerprint: %v", err)
	}

	created, err := hybrid.CreateModelFingerprint(ctx, newFingerprint("aligned"))
	if err != nil {
		t.Fatalf("CreateModelFingerprint: %v", err)
	}
	fromPrimary, err := mysql.GetModelFingerprint(ctx, created.ID)
	if err != nil {
		t.Fatalf("primary fingerprint id=%d: %v", created.ID, err)
	}
	fromReplica, err := sqlite.GetModelFingerprint(ctx, created.ID)
	if err != nil {
		t.Fatalf("replica fingerprint id=%d: %v", created.ID, err)
	}
	if fromPrimary.Name != "aligned" || fromReplica.Name != "aligned" {
		t.Fatalf("primary=%q replica=%q", fromPrimary.Name, fromReplica.Name)
	}

	for i := 0; i < 2; i++ {
		if err := mysql.CreateFingerprintTestResult(ctx, &model.FingerprintTestRecord{Model: fmt.Sprintf("primary-%d", i), MatchesJSON: `[]`}); err != nil {
			t.Fatalf("seed primary test result: %v", err)
		}
	}
	if err := sqlite.CreateFingerprintTestResult(ctx, &model.FingerprintTestRecord{Model: "replica-only", MatchesJSON: `[]`}); err != nil {
		t.Fatalf("seed replica test result: %v", err)
	}
	record := &model.FingerprintTestRecord{Model: "aligned", MatchesJSON: `[]`}
	if err := hybrid.CreateFingerprintTestResult(ctx, record); err != nil {
		t.Fatalf("CreateFingerprintTestResult: %v", err)
	}
	primaryResults, err := mysql.ListFingerprintTestResults(ctx, 10)
	if err != nil {
		t.Fatalf("primary ListFingerprintTestResults: %v", err)
	}
	replicaResults, err := sqlite.ListFingerprintTestResults(ctx, 10)
	if err != nil {
		t.Fatalf("replica ListFingerprintTestResults: %v", err)
	}
	if !hasFingerprintTestResult(primaryResults, record.ID, "aligned") || !hasFingerprintTestResult(replicaResults, record.ID, "aligned") {
		t.Fatalf("test result id=%d not aligned; primary=%#v replica=%#v", record.ID, primaryResults, replicaResults)
	}
}

func hasFingerprintTestResult(results []*model.FingerprintTestRecord, id int64, modelName string) bool {
	for _, result := range results {
		if result.ID == id && result.Model == modelName {
			return true
		}
	}
	return false
}
