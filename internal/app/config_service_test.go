package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestConfigService_LoadDefaults_Idempotent(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	_ = server
	defer cleanup()

	cs := NewConfigService(store)
	ctx := context.Background()

	if err := cs.LoadDefaults(ctx); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}
	if err := cs.LoadDefaults(ctx); err != nil {
		t.Fatalf("LoadDefaults second call should be no-op: %v", err)
	}
}

func TestConfigService_Getters_FromCache(t *testing.T) {
	cs := NewConfigService(storage.Store(nil))

	cs.mu.Lock()
	cs.cache["i"] = &model.SystemSetting{Key: "i", Value: "42"}
	cs.cache["b1"] = &model.SystemSetting{Key: "b1", Value: "true"}
	cs.cache["b2"] = &model.SystemSetting{Key: "b2", Value: "1"}
	cs.cache["s"] = &model.SystemSetting{Key: "s", Value: "x"}
	cs.cache["f"] = &model.SystemSetting{Key: "f", Value: "1.25"}
	cs.cache["bad_int"] = &model.SystemSetting{Key: "bad_int", Value: "nope"}
	cs.cache["bad_float"] = &model.SystemSetting{Key: "bad_float", Value: "nope"}
	cs.mu.Unlock()

	if got := cs.GetInt("i", 0); got != 42 {
		t.Fatalf("GetInt(i)= %d, want 42", got)
	}
	if got := cs.GetInt("bad_int", 7); got != 7 {
		t.Fatalf("GetInt(bad_int)= %d, want default 7", got)
	}
	if got := cs.GetBool("b1", false); got != true {
		t.Fatalf("GetBool(b1)= %v, want true", got)
	}
	if got := cs.GetBool("b2", false); got != true {
		t.Fatalf("GetBool(b2)= %v, want true", got)
	}
	if got := cs.GetBool("missing", true); got != true {
		t.Fatalf("GetBool(missing)= %v, want default true", got)
	}
	if got := cs.GetString("s", "d"); got != "x" {
		t.Fatalf("GetString(s)= %q, want x", got)
	}
	if got := cs.GetString("missing", "d"); got != "d" {
		t.Fatalf("GetString(missing)= %q, want default d", got)
	}
	if got := cs.GetFloat("f", 0); got != 1.25 {
		t.Fatalf("GetFloat(f)= %v, want 1.25", got)
	}
	if got := cs.GetFloat("bad_float", 9.9); got != 9.9 {
		t.Fatalf("GetFloat(bad_float)= %v, want default 9.9", got)
	}
	if got := cs.GetDuration("i", 2*time.Second); got != 42*time.Second {
		t.Fatalf("GetDuration(i)= %v, want 42s", got)
	}
}

func TestConfigService_GetSetting_LazyLoadAndCache(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	_ = server
	defer cleanup()

	cs := NewConfigService(store)
	ctx := context.Background()
	if err := cs.LoadDefaults(ctx); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	// 选择一个已存在的key，并从cache中删除，触发懒加载路径。
	key := "log_retention_days"

	cs.mu.Lock()
	delete(cs.cache, key)
	cs.mu.Unlock()

	if got := cs.GetSetting(key); got == nil || got.Key != key {
		t.Fatalf("GetSetting(%q) returned %+v", key, got)
	}

	// 再次调用应命中cache（覆盖双检锁分支）。
	if got := cs.GetSetting(key); got == nil || got.Key != key {
		t.Fatalf("GetSetting(%q) second call returned %+v", key, got)
	}
}
