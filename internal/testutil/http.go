package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// NewTestContext 创建用于测试的 gin.Context 和响应记录器
func NewTestContext(t testing.TB, req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

// NewRecorder 创建 HTTP 响应记录器
func NewRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func normalizeReader(r io.Reader) io.Reader {
	if r == nil {
		return nil
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return nil
		}
	}
	return r
}

// NewRequestReader 创建 HTTP 请求（支持 io.Reader），并安全处理 typed-nil Reader。
func NewRequestReader(method, target string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, target, normalizeReader(body))
}

// NewRequest 创建 HTTP 请求
func NewRequest(method, target string, body []byte) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return NewRequestReader(method, target, reader)
}

func newJSONRequest(method, target string, v any) (*http.Request, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// MustNewJSONRequest 创建 JSON 请求，序列化失败时直接终止测试。
func MustNewJSONRequest(t testing.TB, method, target string, v any) *http.Request {
	t.Helper()

	req, err := newJSONRequest(method, target, v)
	if err != nil {
		t.Fatalf("marshal json failed: %v", err)
	}
	return req
}

// NewJSONRequestBytes 创建 JSON 请求（请求体已是 JSON bytes）。
func NewJSONRequestBytes(method, target string, b []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// ServeHTTP 执行 HTTP 处理器并返回响应
func ServeHTTP(t testing.TB, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// APIResponse 通用 API 响应结构
type APIResponse[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// WaitForGoroutineDeltaLE 等待 goroutine 数量回落到基线+阈值以内
// 用于检测 goroutine 泄漏
func WaitForGoroutineDeltaLE(t testing.TB, baseline int, maxDelta int, timeout time.Duration) int {
	t.Helper()

	if maxDelta < 0 {
		maxDelta = 0
	}
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		cur := runtime.NumGoroutine()
		if cur <= baseline+maxDelta {
			return cur
		}
		if time.Now().After(deadline) {
			return cur
		}
		time.Sleep(10 * time.Millisecond)
	}
}

