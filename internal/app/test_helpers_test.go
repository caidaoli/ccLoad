package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

func newTestContext(t testing.TB, req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func waitForGoroutineDeltaLE(t testing.TB, baseline int, maxDelta int, timeout time.Duration) int {
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

func serveHTTP(t testing.TB, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
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
	return httptest.NewRequest(method, target, body)
}

func newJSONRequest(method, target string, v any) *http.Request {
	b, _ := json.Marshal(v)
	req := httptest.NewRequest(method, target, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newJSONRequestBytes(method, target string, b []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
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
