package app

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

type runtimeMetricsStore struct {
	storage.Store
	metrics storage.HybridRuntimeMetrics
}

func (s *runtimeMetricsStore) RuntimeMetrics() storage.HybridRuntimeMetrics {
	return s.metrics
}

func TestHandleActiveRequests_ExposesDebugAvailability(t *testing.T) {
	t.Parallel()

	srv := newInMemoryServer(t)
	m := newActiveRequestManager()
	id := beginTestActiveRequest(m, time.Now(), "claude-3-opus", "1.2.3.4", true)
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

func TestHandleRuntimeMetricsExposesResponsesWebsocketResources(t *testing.T) {
	srv := newInMemoryServer(t)
	srv.logService.logDropCount.Store(4)
	srv.logService.logFailCount.Store(5)
	srv.store = &runtimeMetricsStore{
		Store: srv.store,
		metrics: storage.HybridRuntimeMetrics{
			SQLiteReadFailures:     2,
			AnalyticsReadsPrimary:  true,
			PrimarySyncPending:     7,
			PrimarySyncFailures:    3,
			PrimarySyncDropped:     1,
			PrimarySyncLastSuccess: 123456,
		},
	}
	srv.responsesWebsocketConnections = newResponsesWebsocketConnectionLimiter(1, 1)
	releaseConnection, limit := srv.responsesWebsocketConnections.acquire("token-a")
	if limit != nil {
		t.Fatalf("acquire downstream websocket connection: %+v", limit)
	}
	defer releaseConnection()
	if _, rejected := srv.responsesWebsocketConnections.acquire("token-a"); rejected == nil {
		t.Fatal("expected per-token downstream websocket rejection")
	}

	expired, releaseExpired, err := srv.responsesExecutionSessions.acquire("token-a", "expired-session")
	if err != nil {
		t.Fatalf("acquire expiring execution session: %v", err)
	}
	releaseExpired()
	srv.responsesExecutionSessions.cleanup(time.Now().Add(srv.responsesExecutionSessions.sessionTTL() + time.Second))
	if expired == nil {
		t.Fatal("expected expiring execution session")
	}

	srv.configService.mu.Lock()
	srv.configService.cache["responses_ws_max_sessions"] = &model.SystemSetting{
		Key: "responses_ws_max_sessions", Value: "1",
	}
	srv.configService.mu.Unlock()
	session, release, err := srv.responsesExecutionSessions.acquire("token-a", "session-a")
	if err != nil {
		t.Fatalf("acquire execution session: %v", err)
	}
	defer release()
	if _, _, errCapacity := srv.responsesExecutionSessions.acquire("token-b", "session-b"); !errors.Is(errCapacity, errResponsesExecutionSessionCapacity) {
		t.Fatalf("capacity rejection error=%v", errCapacity)
	}
	srv.responsesExecutionSessions.commit(session, []byte(`{}`), responsesWebsocketTurnResult{completedOutput: []byte(`[]`)})
	srv.configService.mu.Lock()
	srv.configService.cache["responses_ws_max_transcript_bytes"] = &model.SystemSetting{
		Key: "responses_ws_max_transcript_bytes", Value: "1",
	}
	srv.configService.mu.Unlock()
	if _, _, errBudget := srv.responsesExecutionSessions.acquire("token-b", "session-b"); !errors.Is(errBudget, errResponsesExecutionSessionTranscriptBudget) {
		t.Fatalf("budget rejection error=%v", errBudget)
	}
	srv.responsesExecutionSessions.recordPreviousResponseMiss(session, "resp-stale", true)
	session.upstream.recordReconnect("test reconnect")

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/runtime-metrics", nil))
	srv.HandleRuntimeMetrics(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	ws, ok := resp.Data["responses_websocket"].(map[string]any)
	if !ok {
		t.Fatalf("responses_websocket metrics missing: %#v", resp.Data)
	}
	if ws["sessions"] != float64(1) || ws["active_attachments"] != float64(1) || ws["transcript_bytes"] != float64(4) {
		t.Fatalf("unexpected websocket resource metrics: %#v", ws)
	}
	if ws["reconnects"] != float64(1) {
		t.Fatalf("websocket reconnect metric missing: %#v", ws)
	}
	if ws["max_sessions"] != float64(1) {
		t.Fatalf("websocket limits missing: %#v", ws)
	}
	if ws["max_transcript_bytes"] != float64(1) {
		t.Fatalf("websocket transcript budget missing: %#v", ws)
	}
	for key, want := range map[string]float64{
		"ttl_expired":              1,
		"capacity_rejected":        1,
		"budget_rejected":          1,
		"previous_response_misses": 1,
	} {
		if ws[key] != want {
			t.Fatalf("%s=%v, want %v; metrics=%#v", key, ws[key], want, ws)
		}
	}
	if ws["downstream_connections"] != float64(1) ||
		ws["rejected_downstream_connections"] != float64(1) ||
		ws["max_downstream_connections"] != float64(1) ||
		ws["max_downstream_connections_per_token"] != float64(1) {
		t.Fatalf("downstream websocket metrics missing: %#v", ws)
	}
	for _, key := range []string{
		"upstream_handshakes",
		"upstream_reuses",
		"upstream_heartbeat_failures",
		"upstream_queued_read_bytes",
		"oldest_upstream_connection_seconds",
	} {
		if _, exists := ws[key]; !exists {
			t.Fatalf("%s metric missing: %#v", key, ws)
		}
	}
	storageMetrics, ok := resp.Data["storage"].(map[string]any)
	if !ok {
		t.Fatalf("storage metrics missing: %#v", resp.Data)
	}
	if storageMetrics["sqlite_read_failures"] != float64(2) || storageMetrics["analytics_reads_primary"] != true {
		t.Fatalf("unexpected storage metrics: %#v", storageMetrics)
	}
	if storageMetrics["primary_sync_pending"] != float64(7) ||
		storageMetrics["primary_sync_failures"] != float64(3) ||
		storageMetrics["primary_sync_dropped"] != float64(1) ||
		storageMetrics["primary_sync_last_success_unix_ms"] != float64(123456) {
		t.Fatalf("unexpected primary sync metrics: %#v", storageMetrics)
	}
	logMetrics, ok := resp.Data["logs"].(map[string]any)
	if !ok || logMetrics["dropped_entries"] != float64(4) || logMetrics["persistence_failed_entries"] != float64(5) {
		t.Fatalf("unexpected log metrics: %#v", logMetrics)
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
	id := beginTestActiveRequest(m, time.Now(), "claude-3-opus", "1.2.3.4", true)
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
