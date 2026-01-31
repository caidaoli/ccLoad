package testutil_test

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"

	"ccLoad/internal/testutil"
)

func TestSetupTestStore_CreatesValidStore(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	if store == nil {
		t.Fatal("store should not be nil")
	}

	// 验证可以执行基本操作
	ctx := context.Background()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}

	// 初始应该没有配置
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}
}

func TestNewTestContext_CreatesValidContext(t *testing.T) {
	req := testutil.MustNewJSONRequest(t, http.MethodPost, "/test", map[string]string{"key": "value"})
	c, w := testutil.NewTestContext(t, req)

	if c == nil {
		t.Fatal("context should not be nil")
	}
	if w == nil {
		t.Fatal("recorder should not be nil")
	}
	if c.Request != req {
		t.Error("request not set correctly")
	}
}

func TestNewJSONRequest_SetsContentType(t *testing.T) {
	req := testutil.MustNewJSONRequest(t, http.MethodPost, "/test", map[string]string{"key": "value"})

	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type: application/json, got %s", req.Header.Get("Content-Type"))
	}
}

func TestNewRequest_NilBody_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic, got %v", r)
		}
	}()

	req := testutil.NewRequest(http.MethodGet, "/test", nil)
	if req == nil {
		t.Fatal("request should not be nil")
	}
}

func TestNewRequestReader_TypedNil_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic, got %v", r)
		}
	}()

	var r *bytes.Reader
	req := testutil.NewRequestReader(http.MethodGet, "/test", r)
	if req == nil {
		t.Fatal("request should not be nil")
	}
}

func TestNewJSONRequest_MarshalError_ReturnsError(t *testing.T) {
	req, err := testutil.NewJSONRequest(http.MethodPost, "/test", func() {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if req != nil {
		t.Fatal("expected request to be nil on error")
	}
}

func TestMustParseAPIResponse_ParsesCorrectly(t *testing.T) {
	body := []byte(`{"success":true,"data":"hello"}`)
	resp := testutil.MustParseAPIResponse[string](t, body)

	if !resp.Success {
		t.Error("expected success to be true")
	}
	if resp.Data != "hello" {
		t.Errorf("expected data to be 'hello', got %s", resp.Data)
	}
}

func TestCreateTestChannel_CreatesChannel(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	cfg := testutil.CreateTestChannel(t, store, "test-channel")

	if cfg.ID == 0 {
		t.Error("channel ID should not be 0")
	}
	if cfg.Name != "test-channel" {
		t.Errorf("expected name 'test-channel', got %s", cfg.Name)
	}
	if !cfg.Enabled {
		t.Error("channel should be enabled")
	}
}

func TestCreateTestAPIKey_CreatesKey(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	cfg := testutil.CreateTestChannel(t, store, "test-channel")
	testutil.CreateTestAPIKey(t, store, cfg.ID, 0)

	ctx := context.Background()
	allKeys, err := store.GetAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("GetAllAPIKeys failed: %v", err)
	}

	count := testutil.CountAPIKeys(allKeys)
	if count != 1 {
		t.Errorf("expected 1 key, got %d", count)
	}
}

func TestCreateTestAPIKeys_CreatesBatch(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	cfg := testutil.CreateTestChannel(t, store, "test-channel")
	testutil.CreateTestAPIKeys(t, store, cfg.ID, 3)

	ctx := context.Background()
	allKeys, err := store.GetAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("GetAllAPIKeys failed: %v", err)
	}

	count := testutil.CountAPIKeys(allKeys)
	if count != 3 {
		t.Errorf("expected 3 keys, got %d", count)
	}
}

func TestWaitForGoroutineDeltaLE_ReturnsCurrentCount(t *testing.T) {
	baseline := testutil.GetGoroutineBaseline()

	// 没有新增 goroutine，应该立即返回
	result := testutil.WaitForGoroutineDeltaLE(t, baseline, 5, 100*time.Millisecond)

	if result > baseline+5 {
		t.Errorf("expected goroutine count <= %d, got %d", baseline+5, result)
	}
}

func TestGetGoroutineBaseline_ReturnsPositive(t *testing.T) {
	baseline := testutil.GetGoroutineBaseline()

	if baseline <= 0 {
		t.Errorf("expected positive goroutine count, got %d", baseline)
	}

	// baseline 应该大于等于运行时报告的数量
	cur := runtime.NumGoroutine()
	if baseline < cur-5 { // 允许一些误差
		t.Errorf("baseline %d seems too low compared to current %d", baseline, cur)
	}
}
