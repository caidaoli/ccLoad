package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
)

func newTestContext(t testing.TB, req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	return testutil.NewTestContext(t, req)
}

func newRecorder() *httptest.ResponseRecorder {
	return testutil.NewRecorder()
}

func waitForGoroutineDeltaLE(t testing.TB, baseline int, maxDelta int, timeout time.Duration) int {
	return testutil.WaitForGoroutineDeltaLE(t, baseline, maxDelta, timeout)
}

func serveHTTP(t testing.TB, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
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
		_ = srv.Shutdown(ctx)
	})

	return srv
}

func newRequest(method, target string, body io.Reader) *http.Request {
	return testutil.NewRequestReader(method, target, body)
}

func newJSONRequest(t testing.TB, method, target string, v any) *http.Request {
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

// newMinimalTestServer 创建最小化测试 Server（不依赖环境变量）
func newMinimalTestServer(t testing.TB, store storage.Store, authSvc *AuthService) *Server {
	t.Helper()

	statsCache := NewStatsCache(store)
	channelCache := storage.NewChannelCache(store, 60*time.Second)
	costCache := NewCostCache()

	srv := &Server{
		store:            store,
		authService:      authSvc,
		channelCache:     channelCache,
		statsCache:       statsCache,
		costCache:        costCache,
		cooldownManager:  cooldown.NewManager(store, nil),
		keySelector:      NewKeySelector(),
		channelBalancer:  NewSmoothWeightedRR(),
		healthCache:      NewHealthCache(store, model.HealthScoreConfig{}, make(chan struct{}), nil, nil),
		client:           http.DefaultClient,
		maxKeyRetries:    3,
		concurrencySem:   make(chan struct{}, 100),
		maxConcurrency:   100,
		shutdownCh:       make(chan struct{}),
		shutdownDone:     make(chan struct{}),
		tokenStatsCh:     make(chan tokenStatsUpdate, 256),
		activeRequests:   newActiveRequestManager(),
		nonStreamTimeout: 30 * time.Second,
	}

	logSvc := NewLogService(store, 100, 1, 7, srv.shutdownCh, &srv.isShuttingDown, &srv.wg)
	srv.logService = logSvc
	logSvc.StartWorkers()
	srv.wg.Add(1)
	go srv.tokenStatsWorker()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	return srv
}
