package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type testHTTPServer struct {
	URL    string
	host   string
	closed atomic.Bool
}

type testHTTPResponseWriter struct {
	header         http.Header
	headerSnapshot http.Header
	statusCode     int
	body           *io.PipeWriter
	pending        bytes.Buffer
	closedErr      error
	bodyClosed     bool
	ready          chan struct{}
	readyOnce      sync.Once
	mu             sync.Mutex
}

var (
	testHTTPServerSeq      atomic.Uint64
	testHTTPServerRegistry sync.Map // host -> http.Handler
	sharedTestHTTPClient   = &http.Client{
		Transport: roundTripperFunc(dispatchTestHTTPRequest),
		Timeout:   0,
	}
)

func init() {
	util.SetModelsFetcherHTTPClientForTesting(sharedTestHTTPClient)
}

func newTestHTTPClient() *http.Client {
	return sharedTestHTTPClient
}

func newTestHTTPServer(t testing.TB, handler http.Handler) *testHTTPServer {
	t.Helper()

	host := fmt.Sprintf("test-upstream-%d.invalid", testHTTPServerSeq.Add(1))
	testHTTPServerRegistry.Store(host, handler)

	srv := &testHTTPServer{
		URL:  "http://" + host,
		host: host,
	}
	t.Cleanup(srv.Close)
	return srv
}

func (s *testHTTPServer) Client() *http.Client {
	return sharedTestHTTPClient
}

func (s *testHTTPServer) Close() {
	if s == nil {
		return
	}
	if s.closed.CompareAndSwap(false, true) {
		testHTTPServerRegistry.Delete(s.host)
	}
}

func dispatchTestHTTPRequest(req *http.Request) (*http.Response, error) {
	handlerValue, ok := testHTTPServerRegistry.Load(req.URL.Host)
	if !ok {
		return nil, fmt.Errorf("no test upstream registered for host %q", req.URL.Host)
	}

	pr, pw := io.Pipe()
	rw := &testHTTPResponseWriter{
		header: make(http.Header),
		body:   pw,
		ready:  make(chan struct{}),
	}

	go func() {
		<-req.Context().Done()
		rw.abort(req.Context().Err())
	}()

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				rw.finish(fmt.Errorf("test upstream panic: %v", recovered))
				return
			}
			rw.finish(nil)
		}()

		clone := req.Clone(req.Context())
		if clone.Body == nil {
			clone.Body = http.NoBody
		}
		clone.RequestURI = clone.URL.RequestURI()
		if clone.Host == "" {
			clone.Host = clone.URL.Host
		}

		handlerValue.(http.Handler).ServeHTTP(rw, clone)
	}()

	select {
	case <-rw.ready:
		return rw.response(req, pr), nil
	case <-req.Context().Done():
		_ = pw.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
}

func (w *testHTTPResponseWriter) Header() http.Header {
	return w.header
}

func (w *testHTTPResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.headerSnapshot != nil {
		return
	}
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.statusCode = statusCode
	w.headerSnapshot = w.header.Clone()
	w.readyOnce.Do(func() { close(w.ready) })
}

func (w *testHTTPResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closedErr != nil {
		return 0, w.closedErr
	}
	return w.pending.Write(p)
}

func (w *testHTTPResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
	w.mu.Lock()
	if w.closedErr != nil {
		w.mu.Unlock()
		return
	}
	if w.pending.Len() == 0 {
		w.mu.Unlock()
		return
	}
	data := append([]byte(nil), w.pending.Bytes()...)
	w.pending.Reset()
	w.mu.Unlock()
	if _, err := w.body.Write(data); err != nil {
		w.mu.Lock()
		if w.closedErr == nil {
			w.closedErr = err
		}
		w.mu.Unlock()
	}
}

func (w *testHTTPResponseWriter) response(req *http.Request, body *io.PipeReader) *http.Response {
	w.mu.Lock()
	defer w.mu.Unlock()

	statusCode := w.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	header := w.headerSnapshot
	if header == nil {
		header = w.header.Clone()
	}

	return &http.Response{
		StatusCode:    statusCode,
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:        header,
		Body:          body,
		ContentLength: -1,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Request:       req,
	}
}

func (w *testHTTPResponseWriter) finish(err error) {
	w.WriteHeader(http.StatusOK)
	w.Flush()
	w.mu.Lock()
	if w.bodyClosed {
		w.mu.Unlock()
		return
	}
	w.bodyClosed = true
	if err != nil && w.closedErr == nil {
		w.closedErr = err
	}
	w.mu.Unlock()
	if err != nil {
		_ = w.body.CloseWithError(err)
		return
	}
	_ = w.body.Close()
}

func (w *testHTTPResponseWriter) abort(err error) {
	if err == nil {
		err = context.Canceled
	}
	w.mu.Lock()
	if w.bodyClosed {
		w.mu.Unlock()
		return
	}
	w.bodyClosed = true
	w.closedErr = err
	w.pending.Reset()
	w.mu.Unlock()
	_ = w.body.CloseWithError(err)
}

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
	srv.client = newTestHTTPClient()
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
		authTokenChannels:   make(map[string][]int64),
		authTokenCostLimits: make(map[string]tokenCostLimit),
		authTokenMaxConns:   make(map[string]int),
		authTokenActiveReqs: make(map[string]int),
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
