package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
)

func newTestContext(t testing.TB, req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	return testutil.NewTestContext(t, req)
}

func newRecorder() *httptest.ResponseRecorder {
	return testutil.NewRecorder()
}

func waitForGoroutineDeltaLE(t testing.TB, baseline int, maxDelta int, timeout time.Duration) int {
	t.Helper()
	return testutil.WaitForGoroutineDeltaLE(t, baseline, maxDelta, timeout)
}

// waitForGoroutineBaselineStable 等待 goroutine 数量“启动完成并稳定”后再取基线。
//
// 逻辑：持续 GC + 采样 goroutine 数量，只要在 stableFor 时间内没有出现“新峰值”，就认为后台 goroutine 已经起齐。
// 返回观测到的最大值（保守基线，避免把惰性启动/调度噪音误判成泄漏）。
func waitForGoroutineBaselineStable(t testing.TB, stableFor, timeout time.Duration) int {
	t.Helper()

	if stableFor <= 0 {
		runtime.GC()
		return runtime.NumGoroutine()
	}

	if timeout <= 0 {
		timeout = stableFor
	}

	deadline := time.Now().Add(timeout)

	runtime.GC()
	maxSeen := runtime.NumGoroutine()
	lastMaxAt := time.Now()

	for {
		cur := runtime.NumGoroutine()
		if cur > maxSeen {
			maxSeen = cur
			lastMaxAt = time.Now()
		}
		if time.Since(lastMaxAt) >= stableFor {
			return maxSeen
		}
		if time.Now().After(deadline) {
			return maxSeen
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func serveHTTP(t testing.TB, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return testutil.ServeHTTP(t, h, req)
}

func newInMemoryServer(t testing.TB) *Server {
	t.Helper()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}

	srv := NewServer(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
		// store.Close() 已在 srv.Shutdown 内部调用，无需重复关闭
	})

	return srv
}

func newRequest(method, target string, body io.Reader) *http.Request {
	return testutil.NewRequestReader(method, target, body)
}

func newJSONRequest(t testing.TB, method, target string, v any) *http.Request {
	t.Helper()
	return testutil.MustNewJSONRequest(t, method, target, v)
}

func newJSONRequestBytes(method, target string, b []byte) *http.Request {
	return testutil.NewJSONRequestBytes(method, target, b)
}

func mustUnmarshalJSON(t testing.TB, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal json failed: %v", err)
	}
}

func mustParseAPIResponse[T any](t testing.TB, body []byte) APIResponse[T] {
	t.Helper()

	var resp APIResponse[T]
	mustUnmarshalJSON(t, body, &resp)
	return resp
}

func mustUnmarshalAPIResponseData(t testing.TB, body []byte, out any) {
	t.Helper()

	wrapper := mustParseAPIResponse[json.RawMessage](t, body)
	if len(wrapper.Data) == 0 {
		t.Fatalf("api response missing data field")
	}
	if err := json.Unmarshal(wrapper.Data, out); err != nil {
		t.Fatalf("unmarshal api response data failed: %v", err)
	}
}

// newTestAuthService 创建测试用 AuthService（不启动 worker，不加载数据库）
func newTestAuthService(t testing.TB) *AuthService {
	t.Helper()
	s := &AuthService{
		authTokens:          make(map[string]int64),
		authTokenIDs:        make(map[string]int64),
		authTokenModels:     make(map[string][]string),
		authTokenCostLimits: make(map[string]tokenCostLimit),
		validTokens:         make(map[string]time.Time),
		lastUsedCh:          make(chan string, 256),
		done:                make(chan struct{}),
	}
	t.Cleanup(s.Close) // 幂等关闭（closeOnce 保护）
	return s
}

// injectAPIToken 注入测试 API token 到 AuthService 的内存映射
func injectAPIToken(svc *AuthService, token string, expiresAt int64, tokenID int64) {
	tokenHash := model.HashToken(token)
	svc.authTokensMux.Lock()
	svc.authTokens[tokenHash] = expiresAt
	svc.authTokenIDs[tokenHash] = tokenID
	svc.authTokensMux.Unlock()
}

// injectAdminToken 注入测试管理 token 到 AuthService 的内存映射
func injectAdminToken(svc *AuthService, token string, expiry time.Time) {
	tokenHash := model.HashToken(token)
	svc.tokensMux.Lock()
	svc.validTokens[tokenHash] = expiry
	svc.tokensMux.Unlock()
}

// runMiddleware 在 gin 路由中运行中间件并返回响应
func runMiddleware(t testing.TB, middleware gin.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)

	// 注册路由：先经过中间件，再到达 handler
	engine.Any("/test", middleware, func(c *gin.Context) {
		data := gin.H{"passed": true}
		if v, ok := c.Get("token_hash"); ok {
			data["token_hash"] = v
		}
		if v, ok := c.Get("token_id"); ok {
			data["token_id"] = v
		}
		c.JSON(http.StatusOK, data)
	})

	engine.ServeHTTP(w, req)
	return w
}
