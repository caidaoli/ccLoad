package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
