package app

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleActiveRequests_ExposesDebugAvailability(t *testing.T) {
	t.Parallel()

	srv := newInMemoryServer(t)
	m := newActiveRequestManager()
	id := m.Register(time.Now(), "claude-3-opus", "1.2.3.4", true)
	m.SetDebugCapture(id, &debugCapture{})

	srv.activeRequests = m

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active-requests", nil))

	srv.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
		Count   int              `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}

	value, ok := resp.Data[0]["debug_log_available"]
	if !ok {
		t.Fatalf("debug_log_available missing in response: %+v", resp.Data[0])
	}
	if got, ok := value.(bool); !ok || !got {
		t.Fatalf("debug_log_available=%v, want true", value)
	}
}

func TestHandleGetActiveRequestDebugLog_ReturnsLiveSnapshot(t *testing.T) {
	t.Parallel()

	srv := newInMemoryServer(t)
	srv.configService.mu.Lock()
	srv.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	srv.configService.mu.Unlock()

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", strings.NewReader(`{"model":"claude-3-opus"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Trace-Id", "trace-123")

	dc := srv.captureDebugRequest(req, []byte(`{"model":"claude-3-opus"}`))
	if dc == nil {
		t.Fatal("captureDebugRequest returned nil")
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Upstream":   []string{"debug"},
		},
		Body: io.NopCloser(strings.NewReader(`partial-debug-body`)),
	}
	dc.wrapResponseBody(resp)

	buf := make([]byte, len("partial"))
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read wrapped response body: %v", err)
	}
	if n != len("partial") {
		t.Fatalf("read=%d, want %d", n, len("partial"))
	}

	m := newActiveRequestManager()
	id := m.Register(time.Now(), "claude-3-opus", "1.2.3.4", true)
	m.SetDebugCapture(id, dc)
	srv.activeRequests = m

	requestPath := "/admin/active-requests/" + strconv.FormatInt(id, 10) + "/debug-log"
	c, w := newTestContext(t, newRequest(http.MethodGet, requestPath, nil))
	c.Params = gin.Params{{Key: "request_id", Value: strconv.FormatInt(id, 10)}}

	srv.HandleGetActiveRequestDebugLog(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	respPayload := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !respPayload.Success {
		t.Fatalf("success=%v, want true", respPayload.Success)
	}

	if got, ok := respPayload.Data["req_method"].(string); !ok || got != http.MethodPost {
		t.Fatalf("req_method=%v, want POST", respPayload.Data["req_method"])
	}
	if got, ok := respPayload.Data["req_url"].(string); !ok || got != "https://api.example.com/v1/messages" {
		t.Fatalf("req_url=%v, want upstream URL", respPayload.Data["req_url"])
	}
	if got, ok := respPayload.Data["req_body"].(string); !ok || got != `{"model":"claude-3-opus"}` {
		t.Fatalf("req_body=%v, want captured request body", respPayload.Data["req_body"])
	}
	if got, ok := respPayload.Data["resp_status"].(float64); !ok || got != http.StatusOK {
		t.Fatalf("resp_status=%v, want %d", respPayload.Data["resp_status"], http.StatusOK)
	}
	if got, ok := respPayload.Data["resp_body"].(string); !ok || got != "partial" {
		t.Fatalf("resp_body=%v, want partial snapshot", respPayload.Data["resp_body"])
	}
}
