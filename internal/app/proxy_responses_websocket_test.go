package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func dialResponsesWebsocket(t testing.TB, handler http.Handler) *websocket.Conn {
	return dialResponsesWebsocketWithToken(t, handler, "test-api-key")
}

func dialResponsesWebsocketWithToken(t testing.TB, handler http.Handler, token string) *websocket.Conn {
	return dialResponsesWebsocketAtPath(t, handler, token, "/v1/responses")
}

func dialResponsesWebsocketWithSessionID(t testing.TB, handler http.Handler, sessionID string) *websocket.Conn {
	return dialResponsesWebsocketWithTokenAndSessionID(t, handler, "test-api-key", sessionID)
}

func dialResponsesWebsocketWithTokenAndSessionID(
	t testing.TB,
	handler http.Handler,
	token string,
	sessionID string,
) *websocket.Conn {
	return dialResponsesWebsocketWithTokenAndHeaders(
		t,
		handler,
		token,
		http.Header{"Session-Id": []string{sessionID}},
	)
}

func dialResponsesWebsocketWithTokenAndHeaders(
	t testing.TB,
	handler http.Handler,
	token string,
	extraHeaders http.Header,
) *websocket.Conn {
	t.Helper()
	appServer := httptest.NewServer(handler)
	t.Cleanup(appServer.Close)
	return dialResponsesWebsocketAtURL(t, appServer.URL, token, "/v1/responses", extraHeaders)
}

// dialResponsesWebsocketAtPath dials a Responses WebSocket at an arbitrary
// upgrade path, so tests can cover route aliases (e.g. the Codex CLI direct
// route /backend-api/codex/responses) alongside the canonical /v1/responses.
func dialResponsesWebsocketAtPath(t testing.TB, handler http.Handler, token, path string) *websocket.Conn {
	t.Helper()
	appServer := httptest.NewServer(handler)
	t.Cleanup(appServer.Close)
	return dialResponsesWebsocketAtURL(t, appServer.URL, token, path, nil)
}

func dialResponsesWebsocketAtURL(
	t testing.TB,
	serverURL string,
	token string,
	path string,
	extraHeaders http.Header,
) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	for name, values := range extraHeaders {
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + path
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		if resp != nil {
			t.Fatalf("websocket upgrade failed: %v (status=%d)", err, resp.StatusCode)
		}
		t.Fatalf("websocket upgrade failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readWebsocketUntilType(t testing.TB, conn *websocket.Conn, wanted string) map[string]any {
	t.Helper()
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
		eventType, _ := event["type"].(string)
		if eventType == "error" {
			t.Fatalf("unexpected websocket error event: %#v", event)
		}
		if eventType == wanted {
			return event
		}
	}
}

func waitForResponsesWebsocketAttachments(
	t testing.TB,
	store *responsesExecutionSessionStore,
	want int,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for store.stats().ActiveAttachments != want {
		if time.Now().After(deadline) {
			t.Fatalf("active websocket attachments did not become %d", want)
		}
		runtime.Gosched()
	}
}

func TestResponsesExecutionSessionPreferredChannelLifecycle(t *testing.T) {
	store := newResponsesExecutionSessionStore(nil, 1024, 0)
	defer store.close()

	parentID := responsesExecutionSessionID(http.Header{
		"Session-Id": {"shared"},
		"Thread-Id":  {"parent"},
	})
	childID := responsesExecutionSessionID(http.Header{
		"Session-Id": {"shared"},
		"Thread-Id":  {"child"},
	})
	if parentID == "" || childID == "" || parentID == childID || responsesExecutionSessionID(http.Header{}) != "" {
		t.Fatalf("unexpected execution identities parent=%q child=%q", parentID, childID)
	}

	parent, releaseParent, err := store.acquire("subject", parentID)
	if err != nil {
		t.Fatalf("acquire parent session: %v", err)
	}
	parent.rememberPreferredChannel(11)
	parent.rememberPreferredChannel(22)
	if channelID, ok := parent.preferredChannelSnapshot(); !ok || channelID != 11 {
		t.Fatalf("preferred channel=%d ok=%v, want first successful channel 11", channelID, ok)
	}

	child, releaseChild, err := store.acquire("subject", childID)
	if err != nil {
		t.Fatalf("acquire child session: %v", err)
	}
	if channelID, ok := child.preferredChannelSnapshot(); ok || channelID != 0 {
		t.Fatalf("child inherited parent preferred channel=%d ok=%v", channelID, ok)
	}
	releaseChild()
	releaseParent()

	store.cleanup(time.Now().Add(defaultResponsesExecutionSessionTTL*time.Minute + time.Second))
	reopened, releaseReopened, err := store.acquire("subject", parentID)
	if err != nil {
		t.Fatalf("reacquire expired parent session: %v", err)
	}
	defer releaseReopened()
	if channelID, ok := reopened.preferredChannelSnapshot(); ok || channelID != 0 {
		t.Fatalf("expired session retained preferred channel=%d ok=%v", channelID, ok)
	}
}

type responsesWebsocketTestError struct {
	Type         string `json:"type"`
	Code         string `json:"code"`
	Param        string `json:"param"`
	Message      string `json:"message"`
	Setting      string `json:"setting"`
	SettingValue int64  `json:"setting_value"`
	Current      int64  `json:"current"`
	Unit         string `json:"unit"`
}

func readResponsesWebsocketRateLimit(t testing.TB, conn *websocket.Conn) responsesWebsocketTestError {
	t.Helper()
	var event struct {
		Type   string                      `json:"type"`
		Status int                         `json:"status"`
		Error  responsesWebsocketTestError `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read websocket rate limit: %v", err)
	}
	if event.Type != "error" || event.Status != http.StatusTooManyRequests ||
		event.Error.Type != "rate_limit_error" || event.Error.Code != "rate_limit" {
		t.Fatalf("unexpected websocket rate limit: %+v", event)
	}
	return event.Error
}

func readResponsesWebsocketUpgradeError(t testing.TB, response *http.Response) responsesWebsocketTestError {
	t.Helper()
	var payload struct {
		Error responsesWebsocketTestError `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode websocket upgrade error: %v", err)
	}
	return payload.Error
}

func TestResponsesWebsocketSessionCapacityPreservesStableReconnect(t *testing.T) {
	var upstreamCalls atomic.Int32
	requests := make(chan []byte, 2)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := upstreamCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read capacity upstream request: %v", err)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(
			w,
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-capacity-%d\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer-%d\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			turn,
			turn,
		)
	}))
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "stable-capacity", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL}, map[string]string{
		"responses_ws_max_sessions": "1",
	})

	first := dialResponsesWebsocketWithSessionID(t, env.engine, "stable-a")
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first capacity turn: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()
	waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)

	unrelated := dialResponsesWebsocketWithSessionID(t, env.engine, "stable-b")
	if err := unrelated.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "unrelated"}},
	}); err != nil {
		t.Fatalf("write capacity-rejected turn: %v", err)
	}
	capacityErr := readResponsesWebsocketRateLimit(t, unrelated)
	if capacityErr.Setting != responsesWebsocketMaxSessionsSetting ||
		capacityErr.SettingValue != 1 || capacityErr.Current != 1 || capacityErr.Unit != "sessions" ||
		capacityErr.Message != "Responses WebSocket execution session limit reached (current: 1, limit: 1). Reuse an existing Session-Id, wait for an idle session to expire, or increase responses_ws_max_sessions." {
		t.Fatalf("capacity error missing actionable setting details: %+v", capacityErr)
	}
	_ = unrelated.Close()
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("capacity-rejected work reached upstream; calls=%d", got)
	}

	continued := dialResponsesWebsocketWithSessionID(t, env.engine, "stable-a")
	if err := continued.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"previous_response_id": "resp-capacity-1",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write stable continuation after capacity pressure: %v", err)
	}
	readWebsocketUntilType(t, continued, "response.completed")
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("stable continuation upstream calls=%d, want 2", got)
	}
	<-requests
	secondRequest := <-requests
	if input := gjson.GetBytes(secondRequest, "input"); !input.IsArray() || len(input.Array()) != 3 {
		t.Fatalf("stable continuation did not replay complete transcript: %s", secondRequest)
	}
}

func TestResponsesWebsocketTranscriptBudgetRejectsNewWorkWithoutEvictingStableSession(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-budget","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "stable-budget", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL}, map[string]string{
		"responses_ws_max_transcript_bytes": "1",
	})

	first := dialResponsesWebsocketWithSessionID(t, env.engine, "budget-a")
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first budget turn: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"previous_response_id": "resp-budget",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write in-place budget-rejected continuation: %v", err)
	}
	firstBudgetErr := readResponsesWebsocketRateLimit(t, first)
	if firstBudgetErr.Setting != responsesWebsocketMaxTranscriptBytesSetting ||
		firstBudgetErr.SettingValue != 1 || firstBudgetErr.Current <= firstBudgetErr.SettingValue ||
		firstBudgetErr.Unit != "bytes" || firstBudgetErr.Message != fmt.Sprintf(
		"Responses WebSocket transcript budget exceeded (current: %d bytes, limit: 1 byte). Disconnect unused sessions and wait for them to expire, or increase responses_ws_max_transcript_bytes.",
		firstBudgetErr.Current,
	) {
		t.Fatalf("transcript budget error missing actionable setting details: %+v", firstBudgetErr)
	}
	_ = first.Close()
	waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)

	continued := dialResponsesWebsocketWithSessionID(t, env.engine, "budget-a")
	if err := continued.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"previous_response_id": "resp-budget",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write budget-rejected continuation: %v", err)
	}
	continuedBudgetErr := readResponsesWebsocketRateLimit(t, continued)
	if continuedBudgetErr != firstBudgetErr {
		t.Fatalf("reconnected budget error changed details: got %+v, want %+v", continuedBudgetErr, firstBudgetErr)
	}

	unrelated := dialResponsesWebsocketWithSessionID(t, env.engine, "budget-b")
	if err := unrelated.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "unrelated"}},
	}); err != nil {
		t.Fatalf("write budget-rejected new session: %v", err)
	}
	unrelatedBudgetErr := readResponsesWebsocketRateLimit(t, unrelated)
	if unrelatedBudgetErr != firstBudgetErr {
		t.Fatalf("new-session budget error changed details: got %+v, want %+v", unrelatedBudgetErr, firstBudgetErr)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("budget-rejected work reached upstream; calls=%d", got)
	}
	stats := env.server.responsesExecutionSessions.stats()
	if stats.Sessions != 1 || stats.TranscriptBytes <= stats.MaxTranscriptBytes || stats.BudgetRejected != 3 {
		t.Fatalf("budget pressure silently changed stable state: %+v", stats)
	}
}

func newBridgeWriterTestConn(t *testing.T) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial bridge writer test websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestResponsesWebsocketBridgeWriterCapsCollectedOutputBytes locks down the
// cumulative output item ceiling: the per-event pending limit clears after
// every parsed event, so a stream of many small response.output_item.done
// events must still fail once the collected transcript snapshot exceeds the
// request-side transcript limit, instead of accumulating without bound.
func TestResponsesWebsocketBridgeWriterCapsCollectedOutputBytes(t *testing.T) {
	writer := newResponsesWebsocketBridgeWriter(newBridgeWriterTestConn(t), 0)

	text := strings.Repeat("x", 512*1024)
	var failed error
	for i := 0; i < 64; i++ {
		event := fmt.Sprintf(
			`{"type":"response.output_item.done","output_index":%d,"item":{"id":"item_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`,
			i, i, text,
		)
		if _, err := writer.Write([]byte("data: " + event + "\n\n")); err != nil {
			failed = err
			break
		}
	}
	if failed == nil {
		t.Fatal("unbounded output item accumulation must fail once past the transcript limit")
	}
	if !strings.Contains(failed.Error(), "transcript limit") {
		t.Fatalf("unexpected error: %v", failed)
	}
}

func TestNativeCodexWebsocketReaderDetachesClosedConnectionImmediately(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade reader test websocket: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	session := newCodexUpstreamWebsocketSession(0, 0, newCodexWebsocketFailureTracker())
	defer session.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("build websocket request: %v", err)
	}
	target := codexWebsocketTarget{channelID: 1, url: req.URL.String()}
	conn, resp, err := session.dial(
		context.Background(),
		websocket.DefaultDialer,
		target,
		req,
		codexWebsocketTimeouts{idle: codexWebsocketReadTimeout, ping: codexWebsocketPingInterval},
	)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("dial reader test websocket: %v", err)
	}
	if conn == nil {
		t.Fatal("dial returned nil websocket connection")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, connected := session.targetSnapshot(); !connected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("closed upstream websocket remained attached to execution session")
}

func TestCodexWebsocketTargetChangesWithTransportConfiguration(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("build target request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer stable-key")
	base := &model.Config{ID: 1}
	withProxy := base.Clone()
	withProxy.ProxyURL = "http://proxy.example:8080"
	withHeader := base.Clone()
	withHeader.CustomRequestRules = &model.CustomRequestRules{Headers: []model.CustomHeaderRule{{
		Action: model.RuleActionOverride, Name: "OpenAI-Beta", Value: "changed",
	}}}

	baseTarget := codexWebsocketTargetForRequest(base, req, false, "https://example.com")
	if baseTarget == codexWebsocketTargetForRequest(withProxy, req, false, "https://example.com") {
		t.Fatal("proxy configuration change did not invalidate websocket target")
	}
	if baseTarget == codexWebsocketTargetForRequest(withHeader, req, false, "https://example.com") {
		t.Fatal("custom header configuration change did not invalidate websocket target")
	}
	if baseTarget == codexWebsocketTargetForRequest(base, req, true, "https://example.com") {
		t.Fatal("TLS verification configuration change did not invalidate websocket target")
	}
}

func TestResponsesWebsocketUpgradeAndRejectUnsupportedEvent(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unsupported downstream event must not reach upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:     "codex-http",
		models:   "gpt-test",
		apiKey:   "sk-upstream",
		priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)

	if err := conn.WriteJSON(map[string]any{"type": "unsupported"}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}

	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read websocket error event: %v", err)
	}
	if event.Type != "error" || event.Status != http.StatusBadRequest ||
		event.Error.Type != "invalid_request_error" || event.Error.Code != "unsupported_event" {
		t.Fatalf("unexpected websocket error event: %+v", event)
	}
}

// TestResponsesWebsocketAcceptsCodexDirectRouteAlias verifies the Codex CLI
// direct route (/backend-api/codex/responses, chatgpt_base_url compatible)
// upgrades and completes a turn exactly like the canonical /v1/responses
// path. This mirrors CLIProxyAPI's codexDirect route group.
func TestResponsesWebsocketAcceptsCodexDirectRouteAlias(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-alias\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-direct-alias", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocketAtPath(t, env.engine, "test-api-key", "/backend-api/codex/responses")

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
	}); err != nil {
		t.Fatalf("write turn over codex direct alias: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
}

func TestResponsesWebsocketRequiresAPIAuthentication(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	wsURL := "ws" + strings.TrimPrefix(appServer.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("unauthenticated websocket upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket status=%v, want 401", resp)
	}
}

func TestResponsesWebsocketRejectsUnknownPreviousResponseOnNewSession(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "unknown-previous", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set unknown previous response deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-missing",
		"input": []any{map[string]any{"role": "user", "content": "continue"}},
	}); err != nil {
		t.Fatalf("write unknown previous response request: %v", err)
	}

	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read unknown previous response error: %v", err)
	}
	if event.Type != "error" || event.Status != http.StatusBadRequest ||
		event.Error.Code != "previous_response_not_found" ||
		event.Error.Param != "previous_response_id" {
		t.Fatalf("unexpected unknown previous response error: %+v", event)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("unknown previous response reached upstream %d times", upstreamCalls.Load())
	}
}

func TestResponsesWebsocketRejectsStalePreviousResponse(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-current","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "stale-previous", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set stale previous response deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write first stale previous response request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-stale",
		"input": []any{map[string]any{"role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write stale previous response request: %v", err)
	}

	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read stale previous response error: %v", err)
	}
	if event.Type != "error" || event.Status != http.StatusBadRequest ||
		event.Error.Code != "previous_response_not_found" ||
		event.Error.Param != "previous_response_id" {
		t.Fatalf("unexpected stale previous response error: %+v", event)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("stale previous response reached upstream; calls=%d", upstreamCalls.Load())
	}
}

func TestResponsesWebsocketRejectsBinaryAndOversizedFrames(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid websocket frame must not reach upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-http", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	setMaxBodyBytesForTest(t, env.server, 256)

	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write binary websocket frame: %v", err)
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read unsupported frame error: %v", err)
	}
	if event.Type != "error" || event.Error.Code != "unsupported_frame" {
		t.Fatalf("unexpected binary frame response: %+v", event)
	}

	oversized := dialResponsesWebsocket(t, env.engine)
	if err := oversized.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set oversized frame read deadline: %v", err)
	}
	if err := oversized.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("x"), 257)); err != nil {
		t.Fatalf("write oversized websocket frame: %v", err)
	}
	_, _, err := oversized.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("oversized frame error=%v, want close code %d", err, websocket.CloseMessageTooBig)
	}
}

func TestResponsesWebsocketIdleConnectionsDoNotConsumeTokenConcurrency(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenMaxConns[tokenHash] = 1
	env.server.authService.authTokensMux.Unlock()

	first := dialResponsesWebsocket(t, env.engine)
	second := dialResponsesWebsocket(t, env.engine)
	if first == nil || second == nil {
		t.Fatal("expected both idle websocket connections to upgrade")
	}
}

func TestResponsesWebsocketConnectionLimitRejectsIdleUpgrades(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "connection-limit", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	const expectedPerTokenLimit = defaultResponsesWebsocketConnectionPerSubjectLimit
	for range expectedPerTokenLimit {
		dialResponsesWebsocketAtURL(t, appServer.URL, "test-api-key", "/v1/responses", nil)
	}

	headers := http.Header{"Authorization": []string{"Bearer test-api-key"}}
	conn, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(appServer.URL, "http")+"/v1/responses",
		headers,
	)
	if conn != nil {
		_ = conn.Close()
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err == nil {
		t.Fatalf("idle websocket %d unexpectedly upgraded", expectedPerTokenLimit+1)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("idle websocket %d status=%d, want %d", expectedPerTokenLimit+1, status, http.StatusTooManyRequests)
	}
	limitErr := readResponsesWebsocketUpgradeError(t, resp)
	if limitErr.Type != "rate_limit_error" ||
		limitErr.Code != "responses_websocket_connection_limit_exceeded" ||
		limitErr.Setting != responsesWebsocketMaxConnectionsPerTokenSetting ||
		limitErr.SettingValue != expectedPerTokenLimit || limitErr.Current != expectedPerTokenLimit ||
		limitErr.Unit != "connections" ||
		limitErr.Message != fmt.Sprintf(
			"Responses WebSocket downstream connection limit for this API token reached (current: %d, limit: %d). Close an existing connection for this token or increase responses_ws_max_connections_per_token, then try again.",
			expectedPerTokenLimit,
			expectedPerTokenLimit,
		) {
		t.Fatalf("per-token connection error missing actionable setting details: %+v", limitErr)
	}
}

func TestResponsesWebsocketGlobalConnectionLimitRejectsIdleUpgrades(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "global-connection-limit", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.responsesWebsocketConnections = newResponsesWebsocketConnectionLimiter(1, 1)
	injectAPIToken(env.server.authService, "second-api-key", 0, 2)
	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	dialResponsesWebsocketAtURL(t, appServer.URL, "test-api-key", "/v1/responses", nil)
	headers := http.Header{"Authorization": []string{"Bearer second-api-key"}}
	conn, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(appServer.URL, "http")+"/v1/responses",
		headers,
	)
	if conn != nil {
		_ = conn.Close()
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err == nil {
		t.Fatal("second global websocket unexpectedly upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("second global websocket status=%d, want %d", status, http.StatusTooManyRequests)
	}
	limitErr := readResponsesWebsocketUpgradeError(t, resp)
	if limitErr.Type != "rate_limit_error" ||
		limitErr.Code != "responses_websocket_connection_limit_exceeded" ||
		limitErr.Setting != responsesWebsocketMaxConnectionsSetting ||
		limitErr.SettingValue != 1 || limitErr.Current != 1 || limitErr.Unit != "connections" ||
		limitErr.Message != "Responses WebSocket process-wide downstream connection limit reached (current: 1, limit: 1). Close an existing connection or increase responses_ws_max_connections, then try again." {
		t.Fatalf("global connection error missing actionable setting details: %+v", limitErr)
	}
}

func TestResponsesWebsocketRevokedTokenClosesBeforeNextTurn(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "revoked-token", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	delete(env.server.authService.authTokens, tokenHash)
	env.server.authService.authTokensMux.Unlock()

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "must be rejected"}},
	}); err != nil {
		t.Fatalf("write revoked-token turn: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set revoked-token deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("revoked token close error=%v, want code %d", err, websocket.ClosePolicyViolation)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("revoked token reached upstream %d times", upstreamCalls.Load())
	}
}

func TestResponsesWebsocketExpiredTokenClosesWhileIdle(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "expired-token", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.responsesWebsocketPingIntervalOverride = 20 * time.Millisecond
	conn := dialResponsesWebsocket(t, env.engine)

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokens[tokenHash] = time.Now().Add(-time.Second).UnixMilli()
	env.server.authService.authTokensMux.Unlock()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set expired-token deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("expired token close error=%v, want code %d", err, websocket.ClosePolicyViolation)
	}
}

func TestResponsesWebsocketClosesWhenServerShutsDown(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := env.server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown server: %v", err)
	}

	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("websocket shutdown error=%v, want close code %d", err, websocket.CloseGoingAway)
	}
}

func TestResponsesWebsocketServerPingKeepsLongTurnAlive(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(180 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-long\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "long-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.responsesWebsocketIdleTimeoutOverride = 70 * time.Millisecond
	env.server.responsesWebsocketPingIntervalOverride = 20 * time.Millisecond

	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set long turn deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "long"}},
	}); err != nil {
		t.Fatalf("write long turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
}

func TestResponsesWebsocketClientDisconnectCancelsUpstreamTurn(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "cancel me"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream turn did not start")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close websocket client: %v", err)
	}

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request was not canceled after websocket client disconnected")
	}
}

func TestResponsesWebsocketNativeClientDisconnectStopsFailover(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	stopped := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade primary websocket: %v", err)
			return
		}
		defer close(stopped)
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Errorf("read primary websocket request: %v", err)
			return
		}
		close(started)
		_, _, _ = conn.ReadMessage()
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback-http", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	downstream := dialResponsesWebsocket(t, env.engine)

	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "cancel native turn"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("primary websocket turn did not start")
	}
	if err := downstream.Close(); err != nil {
		t.Fatalf("close downstream websocket: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("primary websocket was not closed after downstream cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for env.server.responsesWebsocketConnections.stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatal("downstream websocket handler did not stop after cancellation")
		}
		time.Sleep(time.Millisecond)
	}

	entry := waitForProxyLog(t, env, "gpt-test")
	if entry.StatusCode != StatusClientClosedRequest {
		t.Fatalf(
			"canceled websocket status=%d, want %d (channel=%q message=%q fallback_calls=%d)",
			entry.StatusCode, StatusClientClosedRequest, entry.ChannelName, entry.Message, fallbackCalls.Load(),
		)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback called %d times after downstream cancellation", fallbackCalls.Load())
	}
}

func TestResponsesWebsocketBridgesHTTPSSEResponse(t *testing.T) {
	requestSeen := make(chan map[string]any, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestSeen <- request

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}

	var eventTypes []string
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
		eventType, _ := event["type"].(string)
		eventTypes = append(eventTypes, eventType)
		if eventType == "error" {
			t.Fatalf("unexpected websocket error event: %#v", event)
		}
		if eventType == "response.completed" {
			break
		}
	}
	if strings.Join(eventTypes, ",") != "response.created,response.output_text.delta,response.completed" {
		t.Fatalf("unexpected websocket event sequence: %v", eventTypes)
	}

	request := <-requestSeen
	if request["type"] != nil {
		t.Fatalf("upstream HTTP request must not contain websocket event type: %#v", request)
	}
	if request["stream"] != true {
		t.Fatalf("upstream HTTP request must force stream=true: %#v", request)
	}
}

func TestResponsesWebsocketXAIOAuthAlwaysBridgesHTTPSSEResponse(t *testing.T) {
	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	requestSeen := make(chan map[string]any, 2)
	conversationIDs := make(chan string, 2)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		turn := httpCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("xAI upstream request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ws-access" {
			t.Errorf("Authorization = %q", got)
		}
		conversationIDs <- r.Header.Get("x-grok-conv-id")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestSeen <- request
		w.Header().Set("Content-Type", "text/event-stream")
		responseID := fmt.Sprintf("resp-xai-ws-%d", turn)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n", responseID)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello-xai-ws\"}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", responseID)
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "ws-access", RefreshToken: "ws-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-ws-bridge", upstreamProtocol: "codex", websockets: true,
		models: "grok-4.5", priority: 100, authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL + "/v1"})
	env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "grok-4.5",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	completed := readWebsocketUntilType(t, conn, "response.completed")
	response, _ := completed["response"].(map[string]any)
	if response["id"] != "resp-xai-ws-1" {
		t.Fatalf("completed event = %#v", completed)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-xai-ws-1",
		"input": []any{map[string]any{"role": "user", "content": "again"}},
	}); err != nil {
		t.Fatalf("write second websocket request: %v", err)
	}
	completed = readWebsocketUntilType(t, conn, "response.completed")
	response, _ = completed["response"].(map[string]any)
	if response["id"] != "resp-xai-ws-2" {
		t.Fatalf("second completed event = %#v", completed)
	}
	if websocketCalls.Load() != 0 || httpCalls.Load() != 2 {
		t.Fatalf("xAI upstream websocket/http calls=%d/%d, want 0/2", websocketCalls.Load(), httpCalls.Load())
	}
	firstConversationID := <-conversationIDs
	secondConversationID := <-conversationIDs
	if firstConversationID == "" || firstConversationID != secondConversationID {
		t.Fatalf("xAI websocket conversation IDs=%q/%q, want one stable identity", firstConversationID, secondConversationID)
	}
	request := <-requestSeen
	if request["type"] != nil || request["stream"] != true || request["model"] != "grok-4.5" {
		t.Fatalf("xAI upstream request = %#v", request)
	}
	if _, exists := request["previous_response_id"]; exists {
		t.Fatalf("xAI upstream retained previous_response_id: %#v", request)
	}
	request = <-requestSeen
	if _, exists := request["previous_response_id"]; exists {
		t.Fatalf("xAI upstream retained previous_response_id on continued turn: %#v", request)
	}
}

func TestResponsesWebsocketExpandsIncrementalTurnForHTTPUpstream(t *testing.T) {
	requests := make(chan map[string]any, 2)
	var turn int
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		turn++
		responseID := "resp-1"
		text := "B"
		if turn == 2 {
			responseID = "resp-2"
			text = "D"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\""+text+"\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\""+responseID+"\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\""+text+"\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "A"}},
	}); err != nil {
		t.Fatalf("write first websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type":                 "response.create",
		"previous_response_id": "resp-1",
		"input":                []any{map[string]any{"role": "user", "content": "C"}},
	}); err != nil {
		t.Fatalf("write second websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	<-requests
	second := <-requests
	if second["previous_response_id"] != nil {
		t.Fatalf("HTTP failover request must not retain previous_response_id: %#v", second)
	}
	if second["model"] != "gpt-test" {
		t.Fatalf("second request did not inherit model: %#v", second)
	}
	input, ok := second["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("second request input=%#v, want complete three-item transcript", second["input"])
	}
	roles := make([]string, 0, len(input))
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		role, _ := item["role"].(string)
		roles = append(roles, role)
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("second request roles=%v, want user,assistant,user", roles)
	}
}

func TestResponsesWebsocketBoundsAccumulatedTranscript(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-limit","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+strings.Repeat("B", 180)+`"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-http",
		upstreamProtocol: "codex",
		models:           "gpt-test",
		apiKey:           "sk-upstream",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	setMaxBodyBytesForTest(t, env.server, 600)
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": strings.Repeat("A", 180)}},
	}); err != nil {
		t.Fatalf("write first websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type":                 "response.create",
		"previous_response_id": "resp-limit",
		"input":                []any{map[string]any{"role": "user", "content": strings.Repeat("C", 180)}},
	}); err != nil {
		t.Fatalf("write second websocket request: %v", err)
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read transcript limit error: %v", err)
	}
	if event.Type != "error" || event.Error.Code != "invalid_request" {
		t.Fatalf("unexpected transcript limit event: %+v", event)
	}
	if calls.Load() != 1 {
		t.Fatalf("oversized accumulated transcript reached upstream; calls=%d", calls.Load())
	}
}

func TestResponsesWebsocketCompactionReplacesStaleTranscript(t *testing.T) {
	requests := make(chan map[string]any, 2)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode compacted request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		id := turn.Add(1)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-compact-%d\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"stale answer\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", id)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "compact-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set compact downstream deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "old prompt"}},
	}); err != nil {
		t.Fatalf("write pre-compaction request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "compacted prompt"},
			map[string]any{"type": "compaction", "encrypted_content": "opaque summary"},
		},
	}); err != nil {
		t.Fatalf("write compacted request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	<-requests
	compacted := <-requests
	input, ok := compacted["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("compacted input=%#v, want exactly replacement transcript", compacted["input"])
	}
	encoded, _ := json.Marshal(compacted)
	if bytes.Contains(encoded, []byte("old prompt")) || bytes.Contains(encoded, []byte("stale answer")) {
		t.Fatalf("compacted request contains stale transcript: %s", encoded)
	}
}

func TestResponsesCompactEndpointStaysOnHTTP(t *testing.T) {
	requestSeen := make(chan []byte, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			t.Error("/responses/compact must not use WebSocket")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/responses/compact" {
			t.Errorf("compact upstream path=%q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		requestSeen <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp-1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "compact-endpoint", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	response := doProxyRequest(t, env.engine, "/v1/responses/compact", map[string]any{
		"model": "gpt-test",
		"input": []any{
			map[string]any{"role": "user", "content": "long history"},
			map[string]any{"type": "compaction_trigger"},
		},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "response.compaction") {
		t.Fatalf("compact response status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.GetBytes(<-requestSeen, "input.1.type").String(); got != "compaction_trigger" {
		t.Fatalf("compact trigger=%q", got)
	}
}

func TestBuildCodexWebsocketRequestBodySanitizesInputItemIDs(t *testing.T) {
	longReasoningID := "rs_" + strings.Repeat("r", 64)
	longCallID := strings.Repeat("call-item-", 8)
	body := []byte(`{"model":"gpt-test","input":[` +
		`{"type":"message","id":"msg-1","role":"user","content":"before"},` +
		`{"type":"reasoning","id":"` + longReasoningID + `","encrypted_content":"opaque","summary":[]},` +
		`{"type":"function_call","id":"` + longCallID + `","call_id":"call-1","name":"lookup","arguments":"{}"}` +
		`]}`)

	first, err := buildCodexWebsocketRequestBody(body)
	if err != nil {
		t.Fatalf("build websocket request body: %v", err)
	}
	second, err := buildCodexWebsocketRequestBody(body)
	if err != nil {
		t.Fatalf("build websocket request body twice: %v", err)
	}
	input := gjson.GetBytes(first, "input").Array()
	if len(input) != 2 {
		t.Fatalf("wire input len=%d, want encrypted overlong reasoning item dropped: %s", len(input), first)
	}
	if input[0].Get("id").String() != "msg-1" {
		t.Fatalf("ordinary input item changed: %s", first)
	}
	shortened := input[1].Get("id").String()
	if shortened == longCallID || len([]rune(shortened)) != 64 {
		t.Fatalf("overlong wire item id was not deterministically shortened: %q", shortened)
	}
	if got := gjson.GetBytes(second, "input.1.id").String(); got != shortened {
		t.Fatalf("wire item id normalization is unstable: first=%q second=%q", shortened, got)
	}
}

func TestResponsesWebsocketCarriesCompletedToolCallIntoNextTurn(t *testing.T) {
	requests := make(chan map[string]any, 2)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		if turn.Add(1) == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-tool","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-after-tool","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\ndata: [DONE]\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "call the tool"}},
	}); err != nil {
		t.Fatalf("write first websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type":  "response.append",
		"input": []any{map[string]any{"role": "user", "content": "skip tool output"}},
	}); err != nil {
		t.Fatalf("write invalid tool continuation: %v", err)
	}
	var rejected map[string]any
	if err := conn.ReadJSON(&rejected); err != nil {
		t.Fatalf("read missing tool output error: %v", err)
	}
	if rejected["type"] != "error" {
		t.Fatalf("missing tool output was accepted: %#v", rejected)
	}
	if err := conn.WriteJSON(map[string]any{
		"type":                 "response.append",
		"previous_response_id": "resp-tool",
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call-1", "output": "42",
		}},
	}); err != nil {
		t.Fatalf("write tool output continuation: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	<-requests
	second := <-requests
	input, ok := second["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("tool continuation input=%#v, want three transcript items", second["input"])
	}
	call, _ := input[1].(map[string]any)
	output, _ := input[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call-1" ||
		output["type"] != "function_call_output" || output["call_id"] != "call-1" {
		t.Fatalf("tool call transcript pairing was lost: %#v", input)
	}
	if turn.Load() != 2 {
		t.Fatalf("invalid continuation reached upstream; calls=%d", turn.Load())
	}
}

func TestResponsesWebsocketDropsIncompleteCollectedToolCall(t *testing.T) {
	requests := make(chan []byte, 2)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if turn.Add(1) == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-incomplete","call_id":"call-incomplete","name":"lookup"}}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-after-incomplete","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "incomplete-tool-call", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set incomplete tool call deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write first incomplete tool call request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type":  "response.append",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "continue"}},
	}); err != nil {
		t.Fatalf("write continuation after incomplete tool call: %v", err)
	}
	completed := readWebsocketUntilType(t, conn, "response.completed")
	response, _ := completed["response"].(map[string]any)
	if response["id"] != "resp-after-incomplete" {
		t.Fatalf("continuation after incomplete tool call failed: %#v", completed)
	}
	<-requests
	second := <-requests
	if strings.Contains(string(second), "call-incomplete") {
		t.Fatalf("incomplete tool call leaked into replay transcript: %s", second)
	}
}

func TestResponsesWebsocketReconcilesCompletedToolCallBeforeReplay(t *testing.T) {
	requests := make(chan []byte, 2)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if turn.Add(1) == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-tool","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-after-tool","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "reconcile-tool-call", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set reconciled tool call deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "lookup"}},
	}); err != nil {
		t.Fatalf("write reconciled tool call request: %v", err)
	}
	completed := readWebsocketUntilType(t, conn, "response.completed")
	completedJSON, _ := json.Marshal(completed)
	if gjson.GetBytes(completedJSON, "response.output.0.arguments").Type != gjson.String {
		t.Fatalf("downstream completion did not expose reconciled tool call: %s", completedJSON)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.append", "previous_response_id": "resp-tool",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "ok"}},
	}); err != nil {
		t.Fatalf("write reconciled tool output: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	<-requests
	replay := <-requests
	call := gjson.GetBytes(replay, "input.1")
	if call.Get("call_id").String() != "call-1" || call.Get("arguments").Type != gjson.String {
		t.Fatalf("replayed tool call was not reconciled from output_item.done: %s", replay)
	}
}

// TestResponsesWebsocketRecoversCompletedToolCallAfterInterruptedStream locks
// down the side-effect boundary: once a complete tool call has reached the
// downstream client, losing the upstream stream before response.completed must
// not make the client's matching tool output an orphan. The next turn must
// replay the delivered call and its output without executing the tool again.
func TestResponsesWebsocketRecoversCompletedToolCallAfterInterruptedStream(t *testing.T) {
	requests := make(chan []byte, 3)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		switch turn.Add(1) {
		case 1:
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-base","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		case 2:
			partial := `data: {"type":"response.created","response":{"id":"resp-interrupted","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-interrupted","call_id":"call-interrupted","name":"lookup","arguments":"{}"}}` + "\n\n"
			w.Header().Set("Content-Length", fmt.Sprint(len(partial)+64))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, partial)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case 3:
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-recovered","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "interrupted-tool-call", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocketWithSessionID(t, env.engine, "interrupted-tool-recovery")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set interrupted tool call deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write base turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type": "response.append", "previous_response_id": "resp-base",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "use tool"}},
	}); err != nil {
		t.Fatalf("write interrupted turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.output_item.done")
	var retryEvent map[string]any
	if err := conn.ReadJSON(&retryEvent); err != nil {
		t.Fatalf("read interrupted turn retry error: %v", err)
	}
	errorObject, _ := retryEvent["error"].(map[string]any)
	if retryEvent["type"] != "error" || retryEvent["status"] != float64(http.StatusBadGateway) ||
		errorObject["type"] != "server_error" || errorObject["code"] != "upstream_stream_interrupted" {
		t.Fatalf("interrupted turn retry event=%#v", retryEvent)
	}
	_, _, errClose := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errClose, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
		t.Fatalf("interrupted turn close error=%v, want websocket 1011", errClose)
	}

	conn = dialResponsesWebsocketWithSessionID(t, env.engine, "interrupted-tool-recovery")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set interrupted tool replay deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "first"},
			map[string]any{"type": "message", "role": "user", "content": "use tool"},
			map[string]any{
				"type": "function_call", "id": "fc-interrupted", "call_id": "call-interrupted",
				"name": "lookup", "arguments": "{}",
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call-interrupted", "output": "42",
			},
		},
	}); err != nil {
		t.Fatalf("replay interrupted turn with tool output: %v", err)
	}
	recovered := readWebsocketUntilType(t, conn, "response.completed")
	response, _ := recovered["response"].(map[string]any)
	if response["id"] != "resp-recovered" {
		t.Fatalf("recovered response=%#v", recovered)
	}

	<-requests
	<-requests
	replay := <-requests
	input := gjson.GetBytes(replay, "input")
	if !input.IsArray() {
		t.Fatalf("recovered replay input is not an array: %s", replay)
	}
	var calls, outputs int
	for _, item := range input.Array() {
		switch item.Get("type").String() {
		case "function_call":
			if item.Get("call_id").String() == "call-interrupted" {
				calls++
			}
		case "function_call_output":
			if item.Get("call_id").String() == "call-interrupted" {
				outputs++
			}
		}
	}
	if calls != 1 || outputs != 1 {
		t.Fatalf("interrupted tool call pairing was not replayed exactly once: %s", replay)
	}
}

// TestResponsesWebsocketClientRetryAfterInterruptedTextStream locks down the
// auto-continue contract: once semantic text has reached the downstream client,
// losing the upstream stream before response.completed must still return a
// client-retryable server_error and close 1011 so Codex can reconnect and
// replay the turn instead of treating it as an invalid_request_error.
func TestResponsesWebsocketClientRetryAfterInterruptedTextStream(t *testing.T) {
	requests := make(chan []byte, 3)
	var turn atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		switch turn.Add(1) {
		case 1:
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-base","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		case 2:
			partial := `data: {"type":"response.created","response":{"id":"resp-interrupted-text","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"partial answer"}` + "\n\n"
			w.Header().Set("Content-Length", fmt.Sprint(len(partial)+64))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, partial)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case 3:
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-retried","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "interrupted-text-stream", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocketWithSessionID(t, env.engine, "interrupted-text-retry")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set interrupted text deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write base turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type": "response.append", "previous_response_id": "resp-base",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "continue"}},
	}); err != nil {
		t.Fatalf("write interrupted text turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.output_text.delta")
	var retryEvent map[string]any
	if err := conn.ReadJSON(&retryEvent); err != nil {
		t.Fatalf("read interrupted text retry error: %v", err)
	}
	errorObject, _ := retryEvent["error"].(map[string]any)
	if retryEvent["type"] != "error" || retryEvent["status"] != float64(http.StatusBadGateway) ||
		errorObject["type"] != "server_error" || errorObject["code"] != "upstream_stream_interrupted" {
		t.Fatalf("interrupted text retry event=%#v", retryEvent)
	}
	_, _, errClose := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errClose, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
		t.Fatalf("interrupted text close error=%v, want websocket 1011", errClose)
	}

	conn = dialResponsesWebsocketWithSessionID(t, env.engine, "interrupted-text-retry")
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set interrupted text replay deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "first"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}); err != nil {
		t.Fatalf("replay interrupted text turn: %v", err)
	}
	recovered := readWebsocketUntilType(t, conn, "response.completed")
	response, _ := recovered["response"].(map[string]any)
	if response["id"] != "resp-retried" {
		t.Fatalf("retried response=%#v", recovered)
	}

	<-requests
	<-requests
	replay := <-requests
	input := gjson.GetBytes(replay, "input").Array()
	if len(input) != 2 ||
		input[0].Get("type").String() != "message" || input[0].Get("role").String() != "user" ||
		input[0].Get("content").String() != "first" ||
		input[1].Get("type").String() != "message" || input[1].Get("role").String() != "user" ||
		input[1].Get("content").String() != "continue" {
		t.Fatalf("retried replay input=%s, want exactly [first, continue]", gjson.GetBytes(replay, "input").Raw)
	}
}

// TestResponsesWebsocketRejectsOrphanToolCallOutputOnInitialRequest locks down
// local rejection of a function_call_output whose call_id has no matching
// function_call anywhere in the same input array. Upstream would hard-reject
// this transcript with an HTTP 400, which classifier.go treats as a
// model-level failure — cooling down and retrying an unrelated channel for
// what is actually a malformed client request. Rejecting locally avoids that.
func TestResponsesWebsocketRejectsOrphanToolCallOutputOnInitialRequest(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "orphan-tool-output-initial", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set orphan tool call output deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call-never-issued", "output": "42",
		}},
	}); err != nil {
		t.Fatalf("write orphan tool call output request: %v", err)
	}

	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read orphan tool call output error: %v", err)
	}
	if event.Type != "error" || event.Error.Code != "invalid_request" {
		t.Fatalf("unexpected orphan tool call output error: %+v", event)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("orphan tool call output reached upstream %d times", upstreamCalls.Load())
	}
}

// TestResponsesWebsocketRejectsOrphanToolCallOutputOnIncrementalTurn covers
// the merge path: a second turn's function_call_output references a call_id
// that was never issued in the first turn's request or response, so the
// merged transcript still contains an orphan output that must be rejected
// before it reaches upstream.
func TestResponsesWebsocketRejectsOrphanToolCallOutputOnIncrementalTurn(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-plain","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "orphan-tool-output-incremental", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set orphan tool output incremental deadline: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
	}); err != nil {
		t.Fatalf("write first plain turn: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	if err := conn.WriteJSON(map[string]any{
		"type": "response.append",
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call-never-issued", "output": "42",
		}},
	}); err != nil {
		t.Fatalf("write orphan tool call output continuation: %v", err)
	}

	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read orphan tool call output continuation error: %v", err)
	}
	if event.Type != "error" || event.Error.Code != "invalid_request" {
		t.Fatalf("unexpected orphan tool call output continuation error: %+v", event)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("orphan tool call output continuation reached upstream; calls=%d", upstreamCalls.Load())
	}
}

func TestNativeCodexWebsocketReusesUpstreamConnection(t *testing.T) {
	var handshakes atomic.Int32
	requests := make(chan map[string]any, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || !websocket.IsWebSocketUpgrade(r) {
			t.Errorf("unexpected native upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			t.Errorf("native upstream authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("native upstream X-Api-Key=%q, want empty", r.Header.Get("X-Api-Key"))
		}
		if got := r.Header.Get("OpenAI-Beta"); got != codexResponsesWebsocketBeta {
			t.Errorf("native upstream beta header=%q, want %q", got, codexResponsesWebsocketBeta)
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != "codex-tui" {
			t.Errorf("native upstream identity headers=%v", r.Header)
		}
		for name, want := range map[string]string{
			"Conversation_id":                       "ws-session",
			"Session_id":                            "ws-session",
			"Version":                               "1.2.3",
			"X-Client-Request-Id":                   "request-1",
			"X-Codex-Beta-Features":                 "feature-1",
			"X-Codex-Turn-Metadata":                 `{"turn_id":"turn-1"}`,
			"X-Codex-Turn-State":                    "turn-state-1",
			"X-Responsesapi-Include-Timing-Metrics": "true",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("native upstream %s=%q, want %q; headers=%v", name, got, want, r.Header)
			}
		}
		for _, name := range []string{"Accept", "Content-Type", "X-Arbitrary-Client", "X-Forwarded-For"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("native upstream unexpected %s=%q; headers=%v", name, got, r.Header)
			}
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade native upstream: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		handshakes.Add(1)
		for turn := 1; turn <= 2; turn++ {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Errorf("read native upstream request: %v", err)
				return
			}
			requests <- request
			responseID := fmt.Sprintf("resp-native-%d", turn)
			text := fmt.Sprintf("native-%d", turn)
			if err := conn.WriteJSON(map[string]any{
				"type": "response.output_text.delta", "delta": text,
			}); err != nil {
				t.Errorf("write native delta: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": responseID,
					"output": []any{map[string]any{
						"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": text}},
					}},
					"usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
				},
			}); err != nil {
				t.Errorf("write native completion: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "native-codex", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", apiKey: "sk-upstream", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocketWithTokenAndHeaders(
		t,
		env.engine,
		"test-api-key",
		http.Header{
			"Content-Type":                          []string{"text/plain"},
			"OpenAI-Beta":                           []string{"other-feature"},
			"Originator":                            []string{"client-attacker"},
			"Session-Id":                            []string{"ws-session"},
			"User-Agent":                            []string{"client-attacker"},
			"Version":                               []string{"1.2.3"},
			"X-Arbitrary-Client":                    []string{"drop-me"},
			"X-Client-Request-Id":                   []string{"request-1"},
			"X-Codex-Beta-Features":                 []string{"feature-1"},
			"X-Codex-Turn-Metadata":                 []string{`{"turn_id":"turn-1"}`},
			"X-Codex-Turn-State":                    []string{"turn-state-1"},
			"X-Forwarded-For":                       []string{"203.0.113.10"},
			"X-ResponsesAPI-Include-Timing-Metrics": []string{"true"},
		},
	)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set downstream read deadline: %v", err)
	}

	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "parallel_tool_calls": true,
		"client_metadata": map[string]any{
			"ws_request_header_x_openai_internal_codex_responses_lite": "true",
		},
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first downstream turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-native-1",
		"input": []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write second downstream turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	if handshakes.Load() != 1 {
		t.Fatalf("native upstream handshakes=%d, want 1", handshakes.Load())
	}
	first := <-requests
	second := <-requests
	if first["type"] != "response.create" || second["type"] != "response.create" {
		t.Fatalf("native request types first=%#v second=%#v", first["type"], second["type"])
	}
	if parallel, ok := first["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("responses-lite parallel_tool_calls=%#v, want false", first["parallel_tool_calls"])
	}
	secondInput, ok := second["input"].([]any)
	if !ok || len(secondInput) != 1 {
		t.Fatalf("native incremental input=%#v, want only the current turn", second["input"])
	}
	if second["previous_response_id"] != "resp-native-1" {
		t.Fatalf("native previous_response_id=%#v, want resp-native-1", second["previous_response_id"])
	}
}

func TestNativeCodexWebsocketUsesOAuthCredentialAndIdentityHeaders(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	requestBody := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-ws" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-ws" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != "codex-tui" {
			t.Errorf("Codex identity headers = %v", r.Header)
		}
		if r.Header.Get("Session_id") == "" {
			t.Errorf("Codex Session_id header is missing: %v", r.Header)
		}
		if !strings.Contains(r.Header.Get("OpenAI-Beta"), "responses_websockets=") {
			t.Errorf("OpenAI-Beta = %q", r.Header.Get("OpenAI-Beta"))
		}
		conn, err := upgrader.Upgrade(w, r, http.Header{
			"X-Codex-Plan-Type":                   []string{"pro"},
			"X-Codex-Primary-Used-Percent":        []string{"25"},
			"X-Codex-Primary-Window-Minutes":      []string{"10080"},
			"X-Codex-Primary-Reset-After-Seconds": []string{"604800"},
		})
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		requestBody <- request
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-oauth-ws", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "native-codex-oauth", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", authType: model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-ws", "rt-ws", "account-ws"), priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
	}); err != nil {
		t.Fatalf("write downstream request: %v", err)
	}
	completed := readWebsocketUntilType(t, downstream, "response.completed")
	response, _ := completed["response"].(map[string]any)
	if response["id"] != "resp-oauth-ws" {
		t.Fatalf("completed response = %#v", completed)
	}
	request := <-requestBody
	instructions, _ := request["instructions"].(string)
	if request["stream"] != true || request["store"] != false ||
		!strings.HasPrefix(instructions, "You are Codex, an agent based on GPT-5.") {
		t.Fatalf("upstream Codex request = %#v", request)
	}
	if _, exists := request["include"]; exists {
		t.Fatalf("upstream include = %#v", request["include"])
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list persisted Codex WebSocket quota channel = (%#v, %v)", configs, err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
		persistedCredential.PassiveUsage.Windows[0].UsedPercent != 25 {
		t.Fatalf("persisted Codex WebSocket quota = (%#v, %v)", persistedCredential, err)
	}
}

func TestResponsesWebsocketReconnectResumesExplicitExecutionSession(t *testing.T) {
	var handshakes atomic.Int32
	requests := make(chan map[string]any, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade resumable websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		handshakes.Add(1)
		for turn := 1; turn <= 2; turn++ {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Errorf("read resumable request %d: %v", turn, err)
				return
			}
			requests <- request
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": fmt.Sprintf("resp-resume-%d", turn),
					"output": []any{map[string]any{
						"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("answer-%d", turn)}},
					}},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			}); err != nil {
				t.Errorf("write resumable response %d: %v", turn, err)
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "resumable-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	first := dialResponsesWebsocketWithSessionID(t, env.engine, "resume-me")
	if err := first.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set first resumable deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first resumable request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()

	second := dialResponsesWebsocketWithSessionID(t, env.engine, "resume-me")
	if err := second.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set second resumable deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"previous_response_id": "resp-resume-1",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write second resumable request: %v", err)
	}
	readWebsocketUntilType(t, second, "response.completed")

	if handshakes.Load() != 1 {
		t.Fatalf("resumable upstream handshakes=%d, want 1", handshakes.Load())
	}
	<-requests
	continued := <-requests
	if continued["previous_response_id"] != "resp-resume-1" {
		t.Fatalf("reconnected request did not resume incrementally: %#v", continued)
	}
}

func TestNativeCodexWebsocketIgnoresUnapprovedHandshakeHeaderChanges(t *testing.T) {
	organizations := make(chan string, 2)
	var handshakes atomic.Int32
	var responses atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade header-fingerprint websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		handshakes.Add(1)
		organizations <- r.Header.Get("OpenAI-Organization")
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			responseID := fmt.Sprintf("resp-header-%d", responses.Add(1))
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": responseID, "output": []any{},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			}); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "header-fingerprint", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	first := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", http.Header{
		"Session-Id":          []string{"header-fingerprint"},
		"OpenAI-Organization": []string{"org-a"},
	})
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write first header-fingerprint request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	if organization := <-organizations; organization != "" {
		t.Fatalf("first handshake organization=%q, want dropped", organization)
	}
	_ = first.Close()

	second := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", http.Header{
		"Session-Id":          []string{"header-fingerprint"},
		"OpenAI-Organization": []string{"org-b"},
	})
	if err := second.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-header-1",
		"input": []any{map[string]any{"role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write second header-fingerprint request: %v", err)
	}
	readWebsocketUntilType(t, second, "response.completed")

	if handshakes.Load() != 1 {
		t.Fatalf("unapproved handshake header split upstream sessions: handshakes=%d", handshakes.Load())
	}
	select {
	case organization := <-organizations:
		t.Fatalf("unapproved header forced a second handshake with organization=%q", organization)
	default:
	}
}

func TestResponsesWebsocketCacheHintsDoNotShareTranscript(t *testing.T) {
	tests := []struct {
		name         string
		bodyHint     map[string]any
		extraHeaders http.Header
	}{
		{
			name:     "shared prompt_cache_key",
			bodyHint: map[string]any{"prompt_cache_key": "shared-cache-bucket"},
		},
		{
			name:         "shared Session_id",
			bodyHint:     map[string]any{},
			extraHeaders: http.Header{"Session_id": []string{"shared-cache-session"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan map[string]any, 2)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn := calls.Add(1)
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode independent websocket request %d: %v", turn, err)
					return
				}
				requests <- request
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-independent-%d\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer-%d\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", turn, turn)
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "independent-websockets", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
			}}, map[int]string{0: upstream.URL})
			env.server.client = upstream.Client()

			for _, prompt := range []string{"one", "independent two"} {
				downstream := dialResponsesWebsocketWithTokenAndHeaders(
					t,
					env.engine,
					"test-api-key",
					tt.extraHeaders,
				)
				if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatalf("set independent websocket deadline: %v", err)
				}
				payload := map[string]any{
					"type": "response.create", "model": "gpt-test",
					"input": []any{map[string]any{"role": "user", "content": prompt}},
				}
				for key, value := range tt.bodyHint {
					payload[key] = value
				}
				if err := downstream.WriteJSON(payload); err != nil {
					t.Fatalf("write independent websocket request: %v", err)
				}
				readWebsocketUntilType(t, downstream, "response.completed")
				_ = downstream.Close()
			}

			<-requests
			second := <-requests
			input, ok := second["input"].([]any)
			if !ok || len(input) != 1 {
				t.Fatalf("independent websocket request inherited transcript: %#v", second["input"])
			}
			message, ok := input[0].(map[string]any)
			if !ok || message["content"] != "independent two" {
				t.Fatalf("independent websocket input=%#v, want only second prompt", input)
			}
		})
	}
}

func TestResponsesWebsocketExecutionSessionExpires(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-expire\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "expiring-session", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL}, map[string]string{
		responsesWebsocketSessionTTLSetting: "7",
	})
	first := dialResponsesWebsocketWithSessionID(t, env.engine, "expire-me")
	if err := first.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set expiring first deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write expiring first request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()
	waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)
	env.server.responsesExecutionSessions.cleanup(
		time.Now().Add(env.server.responsesExecutionSessions.sessionTTL() + time.Second),
	)

	second := dialResponsesWebsocketWithSessionID(t, env.engine, "expire-me")
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set expiring second deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type":                 "response.create",
		"model":                "gpt-test",
		"previous_response_id": "resp-expire",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write expired continuation: %v", err)
	}
	var event struct {
		Type   string                      `json:"type"`
		Status int                         `json:"status"`
		Error  responsesWebsocketTestError `json:"error"`
	}
	if err := second.ReadJSON(&event); err != nil {
		t.Fatalf("read expired continuation error: %v", err)
	}
	if event.Type != "error" || event.Status != http.StatusBadRequest ||
		event.Error.Type != "invalid_request_error" ||
		event.Error.Code != "previous_response_not_found" ||
		event.Error.Param != "previous_response_id" ||
		event.Error.Setting != responsesWebsocketSessionTTLSetting ||
		event.Error.SettingValue != 7 ||
		event.Error.Unit != "minutes" ||
		event.Error.Message != "previous_response_id is not available in this execution session. Resend the full conversation input without previous_response_id. Idle sessions are retained for 7 minutes by responses_ws_session_ttl_minutes." {
		t.Fatalf("expired continuation event=%+v", event)
	}
	if calls.Load() != 1 {
		t.Fatalf("expired continuation reached upstream; calls=%d", calls.Load())
	}
	if stats := env.server.responsesExecutionSessions.stats(); stats.TTLExpired != 1 || stats.PreviousResponseMisses != 1 {
		t.Fatalf("expired continuation metrics=%+v", stats)
	}
}

func TestResponsesWebsocketClosesDetachedUpstreamButKeepsTranscript(t *testing.T) {
	upstreamClosed := make(chan struct{}, 1)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade detached-transport websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read detached-transport request %d: %v", connection, err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": fmt.Sprintf("resp-detached-%d", connection), "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("write detached-transport response %d: %v", connection, err)
			return
		}
		if connection == 1 {
			if _, _, err := conn.ReadMessage(); err != nil {
				upstreamClosed <- struct{}{}
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "detached-transport", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	first := dialResponsesWebsocketWithSessionID(t, env.engine, "detached-transport")
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write detached-transport first turn: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()

	deadline := time.Now().Add(time.Second)
	for env.server.responsesExecutionSessions.stats().ActiveAttachments != 0 {
		if time.Now().After(deadline) {
			t.Fatal("downstream attachment was not released")
		}
		time.Sleep(time.Millisecond)
	}
	env.server.responsesExecutionSessions.cleanup(time.Now().Add(5*time.Minute + time.Second))
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("detached upstream websocket was not closed")
	}

	second := dialResponsesWebsocketWithSessionID(t, env.engine, "detached-transport")
	if err := second.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-detached-1",
		"input": []any{map[string]any{"role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write detached-transport continuation: %v", err)
	}
	readWebsocketUntilType(t, second, "response.completed")
	if handshakes.Load() != 2 {
		t.Fatalf("detached transport handshakes=%d, want 2", handshakes.Load())
	}
}

func TestResponsesWebsocketExecutionSessionIsolatedByAuthSubject(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-private\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "isolated-session", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	injectAPIToken(env.server.authService, "other-api-key", 0, 2)
	first := dialResponsesWebsocketWithTokenAndSessionID(t, env.engine, "test-api-key", "shared-name")
	if err := first.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set isolated first deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "private"}},
	}); err != nil {
		t.Fatalf("write isolated first request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")

	second := dialResponsesWebsocketWithTokenAndSessionID(t, env.engine, "other-api-key", "shared-name")
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set isolated second deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type":                 "response.append",
		"previous_response_id": "resp-private",
		"input":                []any{map[string]any{"role": "user", "content": "steal"}},
	}); err != nil {
		t.Fatalf("write cross-subject continuation: %v", err)
	}
	var event map[string]any
	if err := second.ReadJSON(&event); err != nil {
		t.Fatalf("read cross-subject rejection: %v", err)
	}
	if event["type"] != "error" {
		t.Fatalf("cross-subject session was shared: %#v", event)
	}
	if calls.Load() != 1 {
		t.Fatalf("cross-subject continuation reached upstream; calls=%d", calls.Load())
	}
}

func TestResponsesWebsocketThreadIsolationPreservesParentContinuation(t *testing.T) {
	var calls atomic.Int32
	requests := make(chan []byte, 3)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read thread-isolation upstream request: %v", err)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(
			w,
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-thread-%d\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer-%d\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			turn,
			turn,
		)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "thread-isolation", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

	parentHeaders := http.Header{
		"Session-Id": {"shared-session"},
		"Thread-Id":  {"parent-thread"},
	}
	parent := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", parentHeaders)
	if err := parent.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "parent-one"}},
	}); err != nil {
		t.Fatalf("write parent thread first turn: %v", err)
	}
	readWebsocketUntilType(t, parent, "response.completed")
	_ = parent.Close()

	child := dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", http.Header{
		"Session-Id": {"shared-session"},
		"Thread-Id":  {"child-thread"},
	})
	if err := child.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "developer", "content": "child-prewarm"}},
	}); err != nil {
		t.Fatalf("write child thread first turn: %v", err)
	}
	readWebsocketUntilType(t, child, "response.completed")

	parent = dialResponsesWebsocketWithTokenAndHeaders(t, env.engine, "test-api-key", parentHeaders)
	if err := parent.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"previous_response_id": "resp-thread-1",
		"input":                []any{map[string]any{"role": "user", "content": "parent-two"}},
	}); err != nil {
		t.Fatalf("write parent continuation after child turn: %v", err)
	}
	readWebsocketUntilType(t, parent, "response.completed")

	if got := calls.Load(); got != 3 {
		t.Fatalf("thread-isolated upstream calls=%d, want 3", got)
	}
	<-requests
	childRequest := <-requests
	if input := gjson.GetBytes(childRequest, "input"); !input.IsArray() || len(input.Array()) != 1 ||
		input.Array()[0].Get("content").String() != "child-prewarm" {
		t.Fatalf("child thread inherited parent transcript: %s", childRequest)
	}
	parentContinuation := <-requests
	if input := gjson.GetBytes(parentContinuation, "input"); !input.IsArray() || len(input.Array()) != 3 {
		t.Fatalf("parent thread continuation lost its transcript: %s", parentContinuation)
	}
}

func TestHTTPResponsesWithoutExistingUpstreamWebsocketUsesHTTP(t *testing.T) {
	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	requests := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		turn := httpCalls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode HTTP upstream request %d: %v", turn, err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http-%d\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", turn)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "http-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	first := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "prompt_cache_key": "http-resume",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}, nil)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "response.completed") {
		t.Fatalf("first HTTP response status=%d body=%s", first.Code, first.Body.String())
	}
	second := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "prompt_cache_key": "http-resume",
		"previous_response_id": "resp-http-1",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}, nil)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "response.completed") {
		t.Fatalf("second HTTP response status=%d body=%s", second.Code, second.Body.String())
	}
	if websocketCalls.Load() != 0 || httpCalls.Load() != 2 {
		t.Fatalf("HTTP downstream calls websocket=%d http=%d, want 0/2", websocketCalls.Load(), httpCalls.Load())
	}
	<-requests
	continued := <-requests
	if continued["previous_response_id"] != "resp-http-1" {
		t.Fatalf("HTTP continuation changed upstream previous_response_id: %#v", continued)
	}
}

func TestHTTPResponsesCacheHintsDoNotSerializeIndependentRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       map[string]any
		headerName string
		header     string
	}{
		{
			name: "shared prompt_cache_key",
			body: map[string]any{"prompt_cache_key": "shared-cache-bucket"},
		},
		{
			name:       "shared Session_id",
			body:       map[string]any{},
			headerName: "Session_id",
			header:     "shared-cache-session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseUpstream)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				arrived <- struct{}{}
				<-release
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-done\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "parallel-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
			}}, map[int]string{0: upstream.URL})
			env.server.client = upstream.Client()

			responses := make(chan *httptest.ResponseRecorder, 2)
			for index := 0; index < 2; index++ {
				payload := map[string]any{
					"model": "gpt-test", "stream": true,
					"input": []any{map[string]any{"role": "user", "content": fmt.Sprintf("request-%d", index)}},
				}
				for key, value := range tt.body {
					payload[key] = value
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatalf("marshal request %d: %v", index, err)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-api-key")
				if tt.headerName != "" {
					req.Header.Set(tt.headerName, tt.header)
				}
				recorder := httptest.NewRecorder()
				go func() {
					env.engine.ServeHTTP(recorder, req)
					responses <- recorder
				}()
			}

			arrivals := 0
			timer := time.NewTimer(time.Second)
			for arrivals < 2 {
				select {
				case <-arrived:
					arrivals++
				case <-timer.C:
					releaseUpstream()
					for completed := 0; completed < 2; completed++ {
						select {
						case <-responses:
						case <-time.After(2 * time.Second):
						}
					}
					t.Fatalf("upstream arrivals before release=%d, want 2; requests were serialized", arrivals)
				}
			}
			if !timer.Stop() {
				<-timer.C
			}
			releaseUpstream()

			for completed := 0; completed < 2; completed++ {
				select {
				case recorder := <-responses:
					if recorder.Code != http.StatusOK {
						t.Fatalf("HTTP response status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				case <-time.After(2 * time.Second):
					t.Fatal("parallel HTTP response did not finish")
				}
			}
		})
	}
}

func TestHTTPResponsesReportsActiveUpstreamLifecycle(t *testing.T) {
	requestArrived := make(chan struct{})
	allowResponse := make(chan struct{}, 1)
	responseFlushed := make(chan struct{})
	finishResponse := make(chan struct{}, 1)
	defer func() {
		select {
		case allowResponse <- struct{}{}:
		default:
		}
		select {
		case finishResponse <- struct{}{}:
		default:
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestArrived)
		<-allowResponse
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		w.(http.Flusher).Flush()
		close(responseFlushed)
		<-finishResponse
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-done\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "active-upstream", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()

	body, err := json.Marshal(map[string]any{
		"model": "gpt-test", "stream": true,
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		env.engine.ServeHTTP(recorder, req)
		close(done)
	}()

	select {
	case <-requestArrived:
	case <-time.After(time.Second):
		t.Fatal("request did not reach upstream")
	}
	active := env.server.activeRequests.List()
	if len(active) != 1 {
		t.Fatalf("active upstream requests=%d, want 1", len(active))
	}
	if active[0].UpstreamStatus != activeRequestStatusRequesting {
		t.Fatalf("upstream status before response=%q, want %q", active[0].UpstreamStatus, activeRequestStatusRequesting)
	}
	if active[0].ChannelName != "active-upstream" || active[0].BaseURL != upstream.URL {
		t.Fatalf("active upstream route=%+v", active[0])
	}

	allowResponse <- struct{}{}
	select {
	case <-responseFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream response was not flushed")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		active = env.server.activeRequests.List()
		if len(active) == 1 && active[0].UpstreamStatus == activeRequestStatusReceiving && active[0].BytesReceived > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(active) != 1 || active[0].UpstreamStatus != activeRequestStatusReceiving || active[0].BytesReceived == 0 {
		t.Fatalf("active upstream did not enter receiving state: %+v", active)
	}

	finishResponse <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP response did not finish")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesExecutionSessionSwitchesFromDownstreamWebsocketToHTTP(t *testing.T) {
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade cross-transport websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		handshakes.Add(1)
		for turn := 1; turn <= 2; turn++ {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Errorf("read cross-transport request %d: %v", turn, err)
				return
			}
			if turn == 2 && request["previous_response_id"] != "resp-cross-1" {
				t.Errorf("cross-transport continuation=%#v", request)
			}
			_ = conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": fmt.Sprintf("resp-cross-%d", turn), "output": []any{},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			})
		}
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cross-transport", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstreamWS := dialResponsesWebsocketWithSessionID(t, env.engine, "cross-mode")
	if err := downstreamWS.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set cross-transport WS deadline: %v", err)
	}
	if err := downstreamWS.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "prompt_cache_key": "cross-mode",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write cross-transport WS request: %v", err)
	}
	readWebsocketUntilType(t, downstreamWS, "response.completed")
	_ = downstreamWS.Close()

	downstreamHTTP := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true,
		"previous_response_id": "resp-cross-1",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}, map[string]string{"Session-Id": "cross-mode"})
	if downstreamHTTP.Code != http.StatusOK || !strings.Contains(downstreamHTTP.Body.String(), "resp-cross-2") {
		t.Fatalf("cross-transport HTTP status=%d body=%s", downstreamHTTP.Code, downstreamHTTP.Body.String())
	}
	if handshakes.Load() != 1 {
		t.Fatalf("cross-transport handshakes=%d, want 1", handshakes.Load())
	}
}

func TestHTTPResponsesUnknownPreviousIDStaysOnHTTP(t *testing.T) {
	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		httpCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp-owned-by-upstream" {
			t.Errorf("HTTP fallback previous_response_id=%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "unknown-previous", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "prompt_cache_key": "unknown-local-session",
		"previous_response_id": "resp-owned-by-upstream",
		"input":                []any{map[string]any{"role": "user", "content": "continue"}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("unknown previous response status=%d body=%s", response.Code, response.Body.String())
	}
	if websocketCalls.Load() != 0 || httpCalls.Load() != 1 {
		t.Fatalf("unknown previous calls websocket=%d http=%d, want 0/1", websocketCalls.Load(), httpCalls.Load())
	}
}

func TestHTTPResponsesWithoutPreviousIDReplacesSessionTranscript(t *testing.T) {
	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	requests := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		turn := httpCalls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode replacement HTTP request %d: %v", turn, err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-replace-%d\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", turn)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "replacement-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	for _, prompt := range []string{"one", "independent two"} {
		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": true, "prompt_cache_key": "reused-cache-bucket",
			"input": []any{map[string]any{"role": "user", "content": prompt}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("replacement HTTP response status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if websocketCalls.Load() != 0 || httpCalls.Load() != 2 {
		t.Fatalf("replacement calls websocket=%d http=%d, want 0/2", websocketCalls.Load(), httpCalls.Load())
	}
	<-requests
	second := <-requests
	if _, exists := second["previous_response_id"]; exists {
		t.Fatalf("independent HTTP request gained previous_response_id: %#v", second)
	}
	input, ok := second["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("independent HTTP request merged transcript: %#v", second["input"])
	}
}

func TestResponsesWebsocketClosesNativeConnectionWhenTransportSwitchesToHTTP(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamClosed := make(chan struct{}, 1)
	httpBodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade transport-switch websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Errorf("read transport-switch websocket request: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp-ws-before-switch", "output": []any{map[string]any{
						"type": "message", "role": "assistant", "content": "first",
					}},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			}); err != nil {
				t.Errorf("write transport-switch websocket response: %v", err)
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				upstreamClosed <- struct{}{}
			}
			return
		}
		body, _ := io.ReadAll(r.Body)
		httpBodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-http-after-switch","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "transport-switch", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set transport-switch deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write native turn before transport switch: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list transport-switch channel: configs=%d err=%v", len(configs), err)
	}
	configs[0].Websockets = false
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("disable websocket transport: %v", err)
	}
	env.server.InvalidateChannelListCache()

	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-ws-before-switch",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write HTTP turn after transport switch: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("native upstream connection remained open after switching to HTTP")
	}
	body := <-httpBodies
	if gjson.GetBytes(body, "previous_response_id").Exists() || len(gjson.GetBytes(body, "input").Array()) != 3 {
		t.Fatalf("HTTP transport did not receive full transcript replay: %s", body)
	}
}

func TestNativeCodexWebsocketPinsChannelKeyAndURLAcrossTurns(t *testing.T) {
	authorizations := make(chan string, 2)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade pinned websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		handshakes.Add(1)
		authorizations <- r.Header.Get("Authorization")
		for turn := 1; turn <= 2; turn++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": fmt.Sprintf("resp-pin-%d", turn), "output": []any{},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			}); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "pinned-native", upstreamProtocol: "codex", websockets: true,
			models: "gpt-test", apiKey: "sk-pin-0", priority: 100},
		{name: "globally-preferred-native", upstreamProtocol: "codex", websockets: true,
			models: "gpt-test", apiKey: "sk-other", priority: 90},
	}, map[int]string{0: upstream.URL, 1: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("list pinned config: configs=%d err=%v", len(configs), err)
	}
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: configs[0].ID, KeyIndex: 1, APIKey: "sk-pin-1", KeyStrategy: model.KeyStrategyRoundRobin,
	}}); err != nil {
		t.Fatalf("create second pinned key: %v", err)
	}
	if err := env.store.UpdateAPIKeysStrategy(context.Background(), configs[0].ID, model.KeyStrategyRoundRobin); err != nil {
		t.Fatalf("enable pinned key round robin: %v", err)
	}
	env.server.InvalidateAPIKeysCache(configs[0].ID)

	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set pinned downstream deadline: %v", err)
	}
	for turn := 1; turn <= 2; turn++ {
		request := map[string]any{
			"type": "response.create", "model": "gpt-test",
			"input": []any{map[string]any{"role": "user", "content": fmt.Sprintf("turn-%d", turn)}},
		}
		if turn == 2 {
			request["previous_response_id"] = "resp-pin-1"
		}
		if err := downstream.WriteJSON(request); err != nil {
			t.Fatalf("write pinned turn %d: %v", turn, err)
		}
		readWebsocketUntilType(t, downstream, "response.completed")
		if turn == 1 {
			configs[1].Priority = 200
			if _, err := env.store.UpdateConfig(context.Background(), configs[1].ID, configs[1]); err != nil {
				t.Fatalf("raise competing channel priority: %v", err)
			}
			env.server.InvalidateChannelListCache()
		}
	}

	if handshakes.Load() != 1 {
		t.Fatalf("pinned websocket handshakes=%d, want 1", handshakes.Load())
	}
	if authorization := <-authorizations; authorization != "Bearer sk-pin-1" {
		t.Fatalf("pinned authorization=%q, want first round-robin key", authorization)
	}
}

func TestNativeCodexWebsocketRetainsAffinityAfterPhysicalDisconnect(t *testing.T) {
	authorizations := make(chan string, 2)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade affinity websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)
		authorizations <- r.Header.Get("Authorization")
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read affinity request %d: %v", connection, err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": fmt.Sprintf("resp-affinity-%d", connection), "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("write affinity response %d: %v", connection, err)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "disconnect-affinity", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", apiKey: "sk-affinity-0", priority: 100,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list affinity config: configs=%d err=%v", len(configs), err)
	}
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: configs[0].ID, KeyIndex: 1, APIKey: "sk-affinity-1", KeyStrategy: model.KeyStrategyRoundRobin,
	}}); err != nil {
		t.Fatalf("create affinity key: %v", err)
	}
	if err := env.store.UpdateAPIKeysStrategy(
		context.Background(), configs[0].ID, model.KeyStrategyRoundRobin,
	); err != nil {
		t.Fatalf("enable affinity round robin: %v", err)
	}
	env.server.InvalidateAPIKeysCache(configs[0].ID)

	downstream := dialResponsesWebsocketWithSessionID(t, env.engine, "disconnect-affinity")
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set affinity downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write first affinity turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	deadline := time.Now().Add(time.Second)
	for env.server.responsesExecutionSessions.stats().UpstreamConnections != 0 {
		if time.Now().After(deadline) {
			t.Fatal("upstream disconnect was not observed by the execution session")
		}
		time.Sleep(time.Millisecond)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-affinity-1",
		"input": []any{map[string]any{"role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write second affinity turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	firstAuthorization := <-authorizations
	secondAuthorization := <-authorizations
	if secondAuthorization != firstAuthorization {
		t.Fatalf("authorization changed after physical disconnect: first=%q second=%q", firstAuthorization, secondAuthorization)
	}
	if handshakes.Load() != 2 {
		t.Fatalf("affinity handshakes=%d, want 2", handshakes.Load())
	}
}

func TestNativeCodexWebsocketProcessesPingBetweenTurns(t *testing.T) {
	pongReceived := make(chan bool, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade ping websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		pong := make(chan struct{}, 1)
		conn.SetPongHandler(func(string) error {
			select {
			case pong <- struct{}{}:
			default:
			}
			return nil
		})
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read ping first request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-ping-1", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("complete ping first request: %v", err)
			return
		}
		if err := conn.WriteControl(websocket.PingMessage, []byte("idle"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("send upstream ping: %v", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		go func() {
			_, _, _ = conn.ReadMessage()
		}()
		select {
		case <-pong:
			pongReceived <- true
		case <-time.After(250 * time.Millisecond):
			pongReceived <- false
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "ping-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set ping downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "ping"}},
	}); err != nil {
		t.Fatalf("write ping downstream request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if ok := <-pongReceived; !ok {
		t.Fatal("native upstream ping was not processed between turns")
	}
}

func TestNativeCodexWebsocketSendsPingBetweenTurns(t *testing.T) {
	pingReceived := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade active-ping websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetPingHandler(func(appData string) error {
			select {
			case pingReceived <- struct{}{}:
			default:
			}
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		for turn := 1; turn <= 2; turn++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Errorf("read active-ping request %d: %v", turn, err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": fmt.Sprintf("resp-active-ping-%d", turn), "output": []any{},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			}); err != nil {
				t.Errorf("write active-ping completion %d: %v", turn, err)
				return
			}
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "active-ping-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.responsesWebsocketPingIntervalOverride = 20 * time.Millisecond
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set active-ping downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "ping upstream"}},
	}); err != nil {
		t.Fatalf("write active-ping downstream request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	select {
	case <-pingReceived:
	case <-time.After(time.Second):
		t.Fatal("gateway did not actively ping the idle upstream websocket")
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-active-ping-1",
		"input": []any{map[string]any{"role": "user", "content": "reuse upstream"}},
	}); err != nil {
		t.Fatalf("write active-ping reused request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/runtime-metrics", nil))
	env.server.HandleRuntimeMetrics(c)
	metrics := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	ws, ok := metrics.Data["responses_websocket"].(map[string]any)
	if !ok {
		t.Fatalf("responses websocket runtime metrics missing: %#v", metrics.Data)
	}
	if ws["upstream_handshakes"] != float64(1) || ws["upstream_reuses"] != float64(1) {
		t.Fatalf("unexpected upstream websocket reuse metrics: %#v", ws)
	}
}

func TestNativeCodexWebsocketBackpressuresFullReadQueue(t *testing.T) {
	upstreamClosed := make(chan bool, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade queue-overflow websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read queue-overflow request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-queue-overflow", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("write queue-overflow completion: %v", err)
			return
		}
		for index := range 256 {
			if err := conn.WriteJSON(map[string]any{
				"type": "response.output_text.delta", "delta": fmt.Sprintf("late-%d", index),
			}); err != nil {
				upstreamClosed <- true
				return
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, err = conn.ReadMessage()
		var netErr net.Error
		upstreamClosed <- err != nil && (!errors.As(err, &netErr) || !netErr.Timeout())
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "queue-overflow-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set queue-overflow downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "bound the queue"}},
	}); err != nil {
		t.Fatalf("write queue-overflow request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	select {
	case closed := <-upstreamClosed:
		if closed {
			t.Fatal("gateway closed the upstream websocket instead of applying read backpressure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream backpressure observation")
	}
}

func TestNativeCodexWebsocketCancelUnblocksFullReadQueue(t *testing.T) {
	started := make(chan struct{})
	upstreamStopped := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade queue-cancel websocket: %v", err)
			return
		}
		defer close(upstreamStopped)
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read queue-cancel request: %v", err)
			return
		}
		close(started)
		payload, err := json.Marshal(map[string]any{
			"type":  "response.output_text.delta",
			"delta": strings.Repeat("x", 64*1024),
		})
		if err != nil {
			t.Errorf("marshal queue-cancel payload: %v", err)
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		for {
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "queue-cancel-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if tcpConn, ok := downstream.UnderlyingConn().(*net.TCPConn); ok {
		if err := tcpConn.SetReadBuffer(1024); err != nil {
			t.Fatalf("shrink downstream receive buffer: %v", err)
		}
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "cancel full queue"}},
	}); err != nil {
		t.Fatalf("write queue-cancel request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queue-cancel upstream turn did not start")
	}

	const queuedBytesTarget = 8 * 1024 * 1024
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/runtime-metrics", nil))
		env.server.HandleRuntimeMetrics(c)
		metrics := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
		websocketMetrics, ok := metrics.Data["responses_websocket"].(map[string]any)
		if !ok {
			t.Fatalf("responses websocket runtime metrics missing: %#v", metrics.Data)
		}
		queuedBytes, _ := websocketMetrics["upstream_queued_read_bytes"].(float64)
		if queuedBytes >= queuedBytesTarget {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream read queue did not fill: queued_bytes=%.0f", queuedBytes)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := downstream.Close(); err != nil {
		t.Fatalf("close queue-cancel downstream: %v", err)
	}
	select {
	case <-upstreamStopped:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock the full upstream read queue")
	}
}

func TestNativeCodexWebsocketAcceptsAllSuccessfulTerminalEvents(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		terminalType   string
		downstreamType string
		responseStatus string
	}{
		{name: "response done", terminalType: "response.done", downstreamType: "response.done", responseStatus: "completed"},
		{name: "response incomplete", terminalType: "response.incomplete", downstreamType: "response.incomplete", responseStatus: "incomplete"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade terminal websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				for turn := 1; turn <= 2; turn++ {
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Errorf("read terminal request %d: %v", turn, err)
						return
					}
					eventType := "response.completed"
					status := "completed"
					if turn == 1 {
						eventType = testCase.terminalType
						status = testCase.responseStatus
					}
					if err := conn.WriteJSON(map[string]any{
						"type": eventType,
						"response": map[string]any{
							"id": fmt.Sprintf("resp-terminal-%d", turn), "status": status,
							"output": []any{map[string]any{
								"type": "message", "role": "assistant",
								"content": []any{map[string]any{"type": "output_text", "text": status}},
							}},
							"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
						},
					}); err != nil {
						t.Errorf("write terminal response %d: %v", turn, err)
						return
					}
				}
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "terminal-native", upstreamProtocol: "codex", websockets: true,
				models: "gpt-test", priority: 100,
			}}, map[int]string{0: upstream.URL})
			downstream := dialResponsesWebsocket(t, env.engine)
			if err := downstream.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatalf("set terminal read deadline: %v", err)
			}
			if err := downstream.WriteJSON(map[string]any{
				"type": "response.create", "model": "gpt-test",
				"input": []any{map[string]any{"role": "user", "content": "first"}},
			}); err != nil {
				t.Fatalf("write first terminal request: %v", err)
			}
			readWebsocketUntilType(t, downstream, testCase.downstreamType)

			if err := downstream.WriteJSON(map[string]any{
				"type": "response.create", "previous_response_id": "resp-terminal-1",
				"input": []any{map[string]any{"role": "user", "content": "second"}},
			}); err != nil {
				t.Fatalf("write second terminal request: %v", err)
			}
			readWebsocketUntilType(t, downstream, "response.completed")
		})
	}
}

func TestResponsesWebsocketHandlesHTTPPrewarmLocally(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "http-prewarm", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set prewarm deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "generate": false,
	}); err != nil {
		t.Fatalf("write prewarm request: %v", err)
	}

	created := readWebsocketUntilType(t, conn, "response.created")
	createdResponse, _ := created["response"].(map[string]any)
	responseID, _ := createdResponse["id"].(string)
	if responseID == "" {
		t.Fatalf("prewarm response id is empty: %#v", created)
	}
	completed := readWebsocketUntilType(t, conn, "response.completed")
	completedResponse, _ := completed["response"].(map[string]any)
	if completedResponse["id"] != responseID {
		t.Fatalf("prewarm completed id=%v, want %q", completedResponse["id"], responseID)
	}
	usage, _ := completedResponse["usage"].(map[string]any)
	if usage["total_tokens"] != float64(0) {
		t.Fatalf("prewarm usage=%#v, want zero tokens", usage)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("HTTP prewarm reached upstream %d times", upstreamCalls.Load())
	}
}

func TestResponsesWebsocketOnlyHandlesInitialHTTPPrewarmLocally(t *testing.T) {
	var upstreamCalls atomic.Int32
	requestBodies := make(chan []byte, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		requestBodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-generated","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "initial-prewarm-only", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set initial prewarm deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "generate": false,
	}); err != nil {
		t.Fatalf("write initial prewarm request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.created")
	prewarm := readWebsocketUntilType(t, conn, "response.completed")
	prewarmResponse, _ := prewarm["response"].(map[string]any)
	prewarmID, _ := prewarmResponse["id"].(string)

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": prewarmID, "generate": false,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "generate now"}},
	}); err != nil {
		t.Fatalf("write post-prewarm request: %v", err)
	}
	completed := readWebsocketUntilType(t, conn, "response.completed")
	response, _ := completed["response"].(map[string]any)
	if response["id"] != "resp-generated" {
		t.Fatalf("post-prewarm response was handled locally again: %#v", completed)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("post-prewarm upstream calls=%d, want 1", upstreamCalls.Load())
	}
	if body := <-requestBodies; gjson.GetBytes(body, "generate").Exists() {
		t.Fatalf("generate leaked to HTTP upstream after prewarm: %s", body)
	}
}

func TestNativeCodexWebsocketPreservesFinalFailedEvent(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed-event websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read failed-event request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": "resp-final-failure", "status": "failed", "output": []any{},
				"error": map[string]any{"code": "upstream_final_failure", "message": "preserve me"},
			},
		})
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "failed-event-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set failed-event deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "fail"}},
	}); err != nil {
		t.Fatalf("write failed-event request: %v", err)
	}

	var event map[string]any
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read failed event: %v", err)
	}
	if event["type"] != "response.failed" {
		t.Fatalf("final event=%#v, want original response.failed", event)
	}
	response, _ := event["response"].(map[string]any)
	apiError, _ := response["error"].(map[string]any)
	if apiError["code"] != "upstream_final_failure" {
		t.Fatalf("final error payload was not preserved: %#v", event)
	}
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set duplicate-event deadline: %v", err)
	}
	if _, duplicate, err := conn.ReadMessage(); err == nil {
		t.Fatalf("response.failed was forwarded twice: %s", duplicate)
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("unexpected websocket state after response.failed: %v", err)
	}
}

func TestNativeCodexWebsocketReadFailureReconnectsWithReplay(t *testing.T) {
	testNativeCodexWebsocketReadFailureReconnectsWithReplay(t, "EOF", nil)
}

func TestNativeCodexWebsocketInterruptedEventReconnectsWithReplayBeforeSemanticOutput(t *testing.T) {
	testNativeCodexWebsocketReadFailureReconnectsWithReplay(
		t,
		responsesWebsocketInterruptedCode,
		func(conn *websocket.Conn) error {
			return conn.WriteJSON(map[string]any{
				"type":   "error",
				"status": http.StatusBadGateway,
				"error": map[string]any{
					"type":    "server_error",
					"code":    responsesWebsocketInterruptedCode,
					"message": responsesWebsocketInterruptedMessage,
				},
			})
		},
	)
}

func testNativeCodexWebsocketReadFailureReconnectsWithReplay(
	t *testing.T,
	failureName string,
	interrupt func(*websocket.Conn) error,
) {
	t.Helper()
	primaryRequests := make(chan map[string]any, 3)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade primary websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)

		var first map[string]any
		if err := conn.ReadJSON(&first); err != nil {
			t.Errorf("read first primary request: %v", err)
			return
		}
		primaryRequests <- first
		if connection == 2 {
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp-primary-2",
					"output": []any{map[string]any{
						"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "second"}},
					}},
					"usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
				},
			}); err != nil {
				t.Errorf("complete replayed primary turn: %v", err)
			}
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-primary-1",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "first"}},
				}},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("complete first primary turn: %v", err)
			return
		}

		var second map[string]any
		if err := conn.ReadJSON(&second); err != nil {
			t.Errorf("read second primary request: %v", err)
			return
		}
		primaryRequests <- second
		// No semantic event: the transport failure must permit a same-target full replay.
		if interrupt != nil {
			if err := interrupt(conn); err != nil {
				t.Errorf("interrupt %s websocket: %v", failureName, err)
			}
		}
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fallback-2\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"second\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback-http", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set downstream read deadline: %v", err)
	}

	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first downstream turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-primary-1",
		"input": []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write second downstream turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	<-primaryRequests
	primarySecond := <-primaryRequests
	if primarySecond["previous_response_id"] != "resp-primary-1" {
		t.Fatalf("primary incremental request=%#v", primarySecond)
	}

	replay := <-primaryRequests
	if _, exists := replay["previous_response_id"]; exists {
		t.Fatalf("same-target replay leaked stale previous_response_id: %#v", replay)
	}
	input, ok := replay["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("same-target replay input=%#v, want user+assistant+user", replay["input"])
	}
	if handshakes.Load() != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf("handshakes=%d fallback calls=%d, want 2/0", handshakes.Load(), fallbackCalls.Load())
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns after %s replay: %v", failureName, err)
	}
	if len(cooldowns) != 0 {
		t.Fatalf("%s replay created model cooldowns: %v", failureName, cooldowns)
	}
}

func TestNativeCodexWebsocketMaxAgeReconnectsWithTranscriptReplay(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 2)
	firstConnectionClosed := make(chan struct{}, 1)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade max-age websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)
		var request map[string]any
		if err = conn.ReadJSON(&request); err != nil {
			t.Errorf("read max-age request: %v", err)
			return
		}
		requests <- request
		responseID := "resp-max-age-2"
		outputText := "two"
		if connection == 1 {
			responseID = "resp-max-age-1"
			outputText = "one"
		}
		if err = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": responseID,
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": outputText}},
				}},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("write max-age completion: %v", err)
			return
		}
		if connection == 1 {
			if _, _, err = conn.ReadMessage(); err == nil {
				t.Error("max-age upstream connection accepted another message instead of closing")
			}
			firstConnectionClosed <- struct{}{}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "max-age-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL}, map[string]string{
		"upstream_connection_reuse_limit_seconds": "1",
	})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set max-age downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first max-age turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	select {
	case <-firstConnectionClosed:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("idle native websocket was not closed after max age")
	}

	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-max-age-1",
		"input": []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write second max-age turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")

	firstRequest := <-requests
	secondRequest := <-requests
	if _, exists := secondRequest["previous_response_id"]; exists {
		t.Fatalf("max-age reconnect leaked stale previous_response_id: %#v", secondRequest)
	}
	input, ok := secondRequest["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("max-age replay input=%#v, want user+assistant+user; first=%#v", secondRequest["input"], firstRequest)
	}
	if handshakes.Load() != 2 {
		t.Fatalf("max-age handshakes=%d, want 2", handshakes.Load())
	}
}

func TestNativeCodexWebsocketMaxAgeDrainsActiveTurnBeforeClosing(t *testing.T) {
	t.Parallel()

	turnStarted := make(chan struct{}, 1)
	releaseTurn := make(chan struct{})
	connectionClosed := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade active max-age websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Errorf("read active max-age request: %v", err)
			return
		}
		if err = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "still-running"}); err != nil {
			t.Errorf("write active max-age delta: %v", err)
			return
		}
		turnStarted <- struct{}{}
		<-releaseTurn
		if err = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-active-max-age", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("active max-age turn was interrupted: %v", err)
			return
		}
		if _, _, err = conn.ReadMessage(); err == nil {
			t.Error("expired websocket remained open after active turn completed")
		}
		connectionClosed <- struct{}{}
	}))
	defer upstream.Close()

	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "active-max-age-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL}, map[string]string{
		"upstream_connection_reuse_limit_seconds": "1",
	})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set active max-age deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "long turn"}},
	}); err != nil {
		t.Fatalf("write active max-age turn: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.output_text.delta")
	<-turnStarted
	<-time.After(1200 * time.Millisecond)
	close(releaseTurn)
	readWebsocketUntilType(t, downstream, "response.completed")
	select {
	case <-connectionClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("expired websocket did not close after active turn drained")
	}
}

func TestNativeCodexWebsocketSequentialKeyFailbackReplaysBetweenTurns(t *testing.T) {
	var phase atomic.Int32
	var keyAHandshakes, keyBHandshakes atomic.Int32
	keyAReplay := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if !websocket.IsWebSocketUpgrade(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"key A unavailable"}}`)
			return
		}

		if authorization == "Bearer sk-ws-a" && phase.Load() == 0 {
			keyAHandshakes.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade keyed upstream websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		switch authorization {
		case "Bearer sk-ws-a":
			keyAHandshakes.Add(1)
			var replay map[string]any
			if err := conn.ReadJSON(&replay); err != nil {
				t.Errorf("read key A replay: %v", err)
				return
			}
			keyAReplay <- replay
			_ = conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp-a-3", "output": []any{map[string]any{
						"type": "message", "role": "assistant", "content": "answer three",
					}},
					"usage": map[string]any{"input_tokens": 5, "output_tokens": 1, "total_tokens": 6},
				},
			})
		case "Bearer sk-ws-b":
			keyBHandshakes.Add(1)
			for responseNumber := 1; responseNumber <= 2; responseNumber++ {
				var request map[string]any
				if err := conn.ReadJSON(&request); err != nil {
					if responseNumber == 2 {
						return
					}
					t.Errorf("read key B request: %v", err)
					return
				}
				_ = conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id": fmt.Sprintf("resp-b-%d", responseNumber), "output": []any{map[string]any{
							"type": "message", "role": "assistant", "content": fmt.Sprintf("answer %d", responseNumber),
						}},
						"usage": map[string]any{"input_tokens": responseNumber, "output_tokens": 1, "total_tokens": responseNumber + 1},
					},
				})
			}
		default:
			t.Errorf("unexpected upstream authorization %q", authorization)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "keyed-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", apiKey: "sk-ws-a", priority: 100, retryOtherKeysOnFailure: true,
	}}, map[int]string{0: upstream.URL})
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: 1, KeyIndex: 1, APIKey: "sk-ws-b", KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("create websocket fallback key: %v", err)
	}
	env.server.maxKeyRetries = 1

	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set keyed websocket deadline: %v", err)
	}
	writeTurn := func(input, previousResponseID string) {
		t.Helper()
		request := map[string]any{
			"type": "response.create", "model": "gpt-test",
			"input": []any{map[string]any{"role": "user", "content": input}},
		}
		if previousResponseID != "" {
			request["previous_response_id"] = previousResponseID
		}
		if err := downstream.WriteJSON(request); err != nil {
			t.Fatalf("write keyed websocket turn: %v", err)
		}
		readWebsocketUntilType(t, downstream, "response.completed")
	}

	writeTurn("one", "")
	if keyAHandshakes.Load() != 1 || keyBHandshakes.Load() != 1 {
		t.Fatalf("first turn A/B handshakes=%d/%d, want 1/1",
			keyAHandshakes.Load(), keyBHandshakes.Load())
	}

	phase.Store(1)
	writeTurn("two", "resp-b-1")
	if keyAHandshakes.Load() != 1 || keyBHandshakes.Load() != 1 {
		t.Fatalf("cooled A must keep B socket: A/B handshakes=%d/%d, want 1/1",
			keyAHandshakes.Load(), keyBHandshakes.Load())
	}

	if err := env.store.ResetKeyCooldown(context.Background(), 1, 0); err != nil {
		t.Fatalf("recover websocket key A: %v", err)
	}
	env.server.invalidateChannelRelatedCache(1)
	writeTurn("three", "resp-b-2")

	replay := <-keyAReplay
	if _, exists := replay["previous_response_id"]; exists {
		t.Fatalf("new Key websocket received stale previous_response_id: %#v", replay)
	}
	input, ok := replay["input"].([]any)
	if !ok || len(input) != 5 {
		t.Fatalf("new Key replay input=%#v, want full five-item transcript", replay["input"])
	}
	if keyAHandshakes.Load() != 2 || keyBHandshakes.Load() != 1 {
		t.Fatalf("recovered A must replace idle B socket: A/B handshakes=%d/%d, want 2/1",
			keyAHandshakes.Load(), keyBHandshakes.Load())
	}
}

func TestNativeCodexWebsocketPreviousResponseNotFoundReconnectsWithReplay(t *testing.T) {
	requests := make(chan map[string]any, 3)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection := handshakes.Add(1)
		responseHeaders := http.Header{
			"X-Codex-Primary-Used-Percent":        []string{strconv.Itoa(int(connection * 25))},
			"X-Codex-Primary-Window-Minutes":      []string{"10080"},
			"X-Codex-Primary-Reset-After-Seconds": []string{"604800"},
		}
		conn, err := upgrader.Upgrade(w, r, responseHeaders)
		if err != nil {
			t.Errorf("upgrade previous-response websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if connection == 1 {
			var first map[string]any
			if err := conn.ReadJSON(&first); err != nil {
				t.Errorf("read first request: %v", err)
				return
			}
			requests <- first
			_ = conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp-first", "output": []any{map[string]any{
						"type": "message", "role": "assistant", "content": "first answer",
					}},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				},
			})
			var incremental map[string]any
			if err := conn.ReadJSON(&incremental); err != nil {
				t.Errorf("read incremental request: %v", err)
				return
			}
			requests <- incremental
			_ = conn.WriteJSON(map[string]any{
				"type": "error", "status": http.StatusBadRequest,
				"error": map[string]any{
					"type": "invalid_request_error", "code": "previous_response_not_found",
					"message": "No response found for previous_response_id resp-first",
				},
			})
			return
		}

		var replay map[string]any
		if err := conn.ReadJSON(&replay); err != nil {
			t.Errorf("read replay request: %v", err)
			return
		}
		requests <- replay
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-replayed", "output": []any{},
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
			},
		})
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "previous-response-replay", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100, authType: model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-reconnect", "rt-reconnect", "account-reconnect"),
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocketWithSessionID(t, env.engine, "quota-reconnect")
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set previous-response replay deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-first",
		"input": []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write continuation request: %v", err)
	}
	completed := readWebsocketUntilType(t, downstream, "response.completed")
	completedJSON, _ := json.Marshal(completed)
	if gjson.GetBytes(completedJSON, "response.id").String() != "resp-replayed" {
		t.Fatalf("unexpected replay completion: %#v", completed)
	}

	<-requests
	incremental := <-requests
	if incremental["previous_response_id"] != "resp-first" {
		t.Fatalf("incremental request did not use prior response id: %#v", incremental)
	}
	replay := <-requests
	if _, exists := replay["previous_response_id"]; exists {
		t.Fatalf("replay leaked invalid previous response id: %#v", replay)
	}
	input, _ := replay["input"].([]any)
	if len(input) != 3 || handshakes.Load() != 2 {
		t.Fatalf("replay input=%#v handshakes=%d, want full transcript and two handshakes", input, handshakes.Load())
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list reconnect quota channel = (%#v, %v)", configs, err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
		persistedCredential.PassiveUsage.Windows[0].UsedPercent != 50 {
		t.Fatalf("reconnect handshake quota = (%#v, %v)", persistedCredential, err)
	}
}

func TestNativeCodexWebsocketFailsOverToAnotherWebsocketAfterReconnectExhausted(t *testing.T) {
	var primaryHandshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failing primary websocket: %v", err)
			return
		}
		primaryHandshakes.Add(1)
		defer func() { _ = conn.Close() }()
		_, _, _ = conn.ReadMessage()
		// Close before any semantic event. The first close exercises same-target
		// reconnect; the second must release the attempt loop to another channel.
	}))
	defer primary.Close()

	var fallbackHandshakes atomic.Int32
	fallbackRequest := make(chan map[string]any, 1)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade fallback websocket: %v", err)
			return
		}
		fallbackHandshakes.Add(1)
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Errorf("read fallback websocket replay: %v", err)
			return
		}
		fallbackRequest <- request
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-ws-fallback", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
	defer fallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "failing-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set websocket-to-websocket failover deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "fail over"}},
	}); err != nil {
		t.Fatalf("write websocket-to-websocket failover request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	request := <-fallbackRequest
	if primaryHandshakes.Load() != 2 || fallbackHandshakes.Load() != 1 {
		t.Fatalf("WS failover handshakes primary=%d fallback=%d, want 2/1", primaryHandshakes.Load(), fallbackHandshakes.Load())
	}
	if _, exists := request["previous_response_id"]; exists {
		t.Fatalf("fallback websocket received stale previous_response_id: %#v", request)
	}
	modelCooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns after websocket failover: %v", err)
	}
	if len(modelCooldowns) != 0 {
		t.Fatalf("websocket transport failure created model cooldowns: %v", modelCooldowns)
	}
}

func TestResponsesWebsocketFailsOverBeforeSemanticOutput(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
	}))
	defer primary.Close()
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fallback\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fallback\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("upstream calls primary=%d fallback=%d, want 1/1", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestResponsesWebsocketRetryableErrorReplaysTranscriptToNativeFallback(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-http-first","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
	}))
	defer primary.Close()

	var secondaryCalls atomic.Int32
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"secondary temporarily unavailable"}}`)
	}))
	defer secondary.Close()

	var fallbackHandshakes atomic.Int32
	fallbackRequests := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade native fallback websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		fallbackHandshakes.Add(1)
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Errorf("read native fallback replay: %v", err)
			return
		}
		fallbackRequests <- request
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-native-replay", "output": []any{},
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
			},
		})
	}))
	defer fallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "http-primary", upstreamProtocol: "codex", models: "gpt-test", priority: 100},
		{name: "http-secondary", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
		{name: "native-fallback", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 1},
	}, map[int]string{0: primary.URL, 1: secondary.URL, 2: fallback.URL})
	env.server.client = primary.Client()

	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	first := dialResponsesWebsocketAtURL(
		t,
		appServer.URL,
		"test-api-key",
		"/v1/responses",
		http.Header{"Session-Id": []string{"retryable-replay"}},
	)
	if err := first.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set first websocket deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first turn: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "previous_response_id": "resp-http-first",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write failing second turn: %v", err)
	}
	var retryEvent map[string]any
	if err := first.ReadJSON(&retryEvent); err != nil {
		t.Fatalf("read retry event: %v", err)
	}
	if retryEvent["type"] != "error" || retryEvent["status"] != float64(http.StatusBadGateway) {
		t.Fatalf("retry event=%#v, want 502 error", retryEvent)
	}
	errorObject, ok := retryEvent["error"].(map[string]any)
	if !ok || errorObject["type"] != "server_error" || errorObject["code"] != "upstream_unavailable" {
		t.Fatalf("retry error payload=%#v, want server_error/upstream_unavailable", retryEvent["error"])
	}
	_, _, closeErrRaw := first.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(closeErrRaw, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
		t.Fatalf("first websocket close=%v, want code %d", closeErrRaw, websocket.CloseInternalServerErr)
	}
	_ = first.Close()
	if secondaryCalls.Load() != 1 || fallbackHandshakes.Load() != 0 {
		t.Fatalf(
			"secondary calls=%d native fallback handshakes=%d before reconnect, want 1/0",
			secondaryCalls.Load(),
			fallbackHandshakes.Load(),
		)
	}

	reconnected := dialResponsesWebsocketAtURL(
		t,
		appServer.URL,
		"test-api-key",
		"/v1/responses",
		http.Header{"Session-Id": []string{"retryable-replay"}},
	)
	defer func() { _ = reconnected.Close() }()
	if err := reconnected.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set replay websocket deadline: %v", err)
	}
	if err := reconnected.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "one"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "first answer"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "two"},
		},
	}); err != nil {
		t.Fatalf("write full replay request: %v", err)
	}
	readWebsocketUntilType(t, reconnected, "response.completed")

	request := <-fallbackRequests
	if fallbackHandshakes.Load() != 1 || primaryCalls.Load() != 2 || secondaryCalls.Load() != 1 {
		t.Fatalf(
			"fallback handshakes=%d primary calls=%d secondary calls=%d, want 1/2/1",
			fallbackHandshakes.Load(),
			primaryCalls.Load(),
			secondaryCalls.Load(),
		)
	}
	if _, exists := request["previous_response_id"]; exists {
		t.Fatalf("native fallback replay leaked previous_response_id: %#v", request)
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("native fallback replay input=%#v, want full three-item transcript", request["input"])
	}
}

func TestNativeCodexWebsocketRejectedHandshakeFallsBackToSameChannelHTTP(t *testing.T) {
	t.Parallel()

	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	longInputID := strings.Repeat("fallback-item-", 8)
	longCollisionID := strings.Repeat("a", codexInputItemIDLimit-2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			if r.Header.Get("X-Codex-Turn-State") != "turn-state" || r.Header.Get("X-ResponsesAPI-Include-Timing-Metrics") != "true" {
				t.Errorf("websocket-only headers missing from handshake: %v", r.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Codex-Primary-Used-Percent", "100")
			w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
			w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"websocket quota exhausted"}}`)
			return
		}
		httpCalls.Add(1)
		for _, name := range []string{"OpenAI-Beta", "X-Codex-Turn-State", "X-ResponsesAPI-Include-Timing-Metrics"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("websocket-only header leaked into HTTP fallback: %s=%q; headers=%v", name, got, r.Header)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			t.Errorf("same-channel HTTP replay body=%q err=%v", body, err)
		}
		if gjson.GetBytes(body, "generate").Exists() {
			t.Errorf("websocket-only generate leaked into HTTP fallback: %s", body)
		}
		if got := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery").String(); got != "sequential_cutoff" {
			t.Errorf("HTTP fallback reasoning summary delivery=%q, want sequential_cutoff: %s", got, body)
		}
		if gjson.GetBytes(body, "stream_options.include_usage").Exists() {
			t.Errorf("unsupported include_usage leaked into HTTP fallback: %s", body)
		}
		if got := gjson.GetBytes(body, "input.0.id").String(); len([]rune(got)) != codexInputItemIDLimit || !strings.HasPrefix(got, "msg_") {
			t.Errorf("HTTP fallback message id=%q, want msg_ prefix and length %d", got, codexInputItemIDLimit)
		}
		if got := gjson.GetBytes(body, "input.1.id").String(); got != "rs_item_reasoning" {
			t.Errorf("HTTP fallback reasoning id=%q, want rs_ prefix", got)
		}
		if got := gjson.GetBytes(body, "input.2.id").String(); got != "fc_item_function_call" {
			t.Errorf("HTTP fallback function call id=%q, want fc_ prefix", got)
		}
		if got := gjson.GetBytes(body, "input.4.id").String(); got != "ctc_item_custom_call" {
			t.Errorf("HTTP fallback custom tool call id=%q, want ctc_ prefix", got)
		}
		if got := gjson.GetBytes(body, "input.5.id").String(); got != "ctco_item_custom_output" {
			t.Errorf("HTTP fallback custom tool output id=%q, want ctco_ prefix", got)
		}
		collisionID := gjson.GetBytes(body, "input.6.id").String()
		preservedID := gjson.GetBytes(body, "input.7.id").String()
		if preservedID != "msg_collision" {
			t.Errorf("HTTP fallback valid message id=%q, want preserved msg_collision", preservedID)
		}
		if collisionID == preservedID || !strings.HasPrefix(collisionID, "msg_") ||
			len([]rune(collisionID)) > codexInputItemIDLimit {
			t.Errorf("HTTP fallback colliding message id=%q, want unique bounded msg_ id", collisionID)
		}
		longNormalizedID := gjson.GetBytes(body, "input.8.id").String()
		longPrefixedID := gjson.GetBytes(body, "input.9.id").String()
		if longNormalizedID == longPrefixedID ||
			len([]rune(longNormalizedID)) != codexInputItemIDLimit ||
			len([]rune(longPrefixedID)) != codexInputItemIDLimit ||
			!strings.HasPrefix(longNormalizedID, "msg_") ||
			!strings.HasPrefix(longPrefixedID, "msg_") {
			t.Errorf(
				"HTTP fallback long colliding ids=(%q, %q), want distinct bounded msg_ ids",
				longNormalizedID,
				longPrefixedID,
			)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http-fallback\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "same-channel-fallback", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100, authType: model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-fallback", "rt-fallback", "account-fallback"),
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	downstream := dialResponsesWebsocketWithTokenAndHeaders(
		t,
		env.engine,
		"test-api-key",
		http.Header{
			"OpenAI-Beta":                           []string{"other-feature"},
			"X-Codex-Turn-State":                    []string{"turn-state"},
			"X-ResponsesAPI-Include-Timing-Metrics": []string{"true"},
		},
	)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set same-channel fallback deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "generate": true,
		"stream_options": map[string]any{
			"reasoning_summary_delivery": "sequential_cutoff",
			"include_usage":              true,
		},
		"input": []any{
			map[string]any{"type": "message", "id": longInputID, "role": "user", "content": "fallback"},
			map[string]any{"type": "reasoning", "id": "item_reasoning", "summary": []any{}},
			map[string]any{
				"type": "function_call", "id": "item_function_call", "call_id": "call-function-1",
				"name": "lookup", "arguments": "{}",
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call-function-1", "output": "done",
			},
			map[string]any{
				"type": "custom_tool_call", "id": "item_custom_call", "call_id": "call-custom-1",
				"name": "apply_patch", "input": "{}",
			},
			map[string]any{
				"type": "custom_tool_call_output", "id": "item_custom_output",
				"call_id": "call-custom-1", "output": "done",
			},
			map[string]any{"type": "message", "id": "collision", "role": "user", "content": "normalize"},
			map[string]any{"type": "message", "id": "msg_collision", "role": "user", "content": "preserve"},
			map[string]any{"type": "message", "id": longCollisionID, "role": "user", "content": "long normalize"},
			map[string]any{"type": "message", "id": "msg_" + longCollisionID, "role": "user", "content": "long prefixed"},
		},
	}); err != nil {
		t.Fatalf("write same-channel fallback request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if websocketCalls.Load() != 1 || httpCalls.Load() != 1 {
		t.Fatalf("same-channel calls websocket=%d http=%d, want 1/1", websocketCalls.Load(), httpCalls.Load())
	}
	entry := waitForProxyLog(t, env, "gpt-test")
	if entry.UpstreamWebsocket {
		t.Fatal("HTTP fallback log must not be marked as upstream websocket")
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list persisted rejected-handshake quota channel = (%#v, %v)", configs, err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
		persistedCredential.PassiveUsage.Windows[0].UsedPercent != 100 {
		t.Fatalf("persisted rejected-handshake quota = (%#v, %v)", persistedCredential, err)
	}
}

func TestNativeCodexWebsocketEOFHandshakeFallsBackToSameChannelHTTP(t *testing.T) {
	t.Parallel()

	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack websocket handshake: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		httpCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-eof-fallback\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "eof-handshake-fallback", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set EOF fallback deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "fallback"}},
	}); err != nil {
		t.Fatalf("write EOF fallback request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")
	if websocketCalls.Load() != 1 || httpCalls.Load() != 1 {
		t.Fatalf("EOF fallback calls websocket=%d http=%d, want 1/1", websocketCalls.Load(), httpCalls.Load())
	}
	entry := waitForProxyLog(t, env, "gpt-test")
	if entry.UpstreamWebsocket {
		t.Fatal("EOF HTTP fallback log must not be marked as upstream websocket")
	}
}

func TestNativeCodexWebsocketReconnectRejectionFallsBackToSameChannelHTTP(t *testing.T) {
	var websocketHandshakes atomic.Int32
	var httpCalls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http-after-reconnect\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if websocketHandshakes.Add(1) > 1 {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade reconnect fallback websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read first request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{"id": "resp-first", "output": []any{}, "usage": map[string]any{
				"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
			}},
		})
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read second request: %v", err)
			return
		}
		// Drop the reused connection before semantic output. The internal reconnect
		// is rejected, so the same selected channel must replay over HTTP.
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "reconnect-http-fallback", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set reconnect fallback deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "first"}},
	}); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "previous_response_id": "resp-first",
		"input": []any{map[string]any{"role": "user", "content": "second"}},
	}); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	completed := readWebsocketUntilType(t, downstream, "response.completed")
	response, _ := completed["response"].(map[string]any)
	if response["id"] != "resp-http-after-reconnect" {
		t.Fatalf("second response did not use HTTP fallback: %#v", completed)
	}
	if websocketHandshakes.Load() != 2 || httpCalls.Load() != 1 {
		t.Fatalf("reconnect fallback calls ws=%d http=%d, want 2/1", websocketHandshakes.Load(), httpCalls.Load())
	}
}

func TestNativeCodexWebsocketMessageTooBigDoesNotFailOver(t *testing.T) {
	var primaryHandshakes atomic.Int32
	var fallbackCalls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHandshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade message-too-big websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read message-too-big request: %v", err)
			return
		}
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream websocket message too big"),
			time.Now().Add(time.Second),
		)
	}))
	defer primary.Close()
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-should-not-run","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "message-too-big-primary", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "message-too-big-fallback", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set message-too-big downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "too large"}},
	}); err != nil {
		t.Fatalf("write message-too-big request: %v", err)
	}
	_, _, err := downstream.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("downstream error=%v, want websocket close %d", err, websocket.CloseMessageTooBig)
	}
	if primaryHandshakes.Load() != 1 {
		t.Fatalf("message-too-big websocket handshakes=%d, want 1 without reconnect", primaryHandshakes.Load())
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("message-too-big request failed over %d times", fallbackCalls.Load())
	}
}

func TestNativeCodexWebsocketMessageTooBigAfterOutputClosesDownstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade committed message-too-big websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read committed message-too-big request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.output_text.delta", "delta": "partial",
		}); err != nil {
			t.Errorf("write committed message-too-big delta: %v", err)
			return
		}
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream websocket message too big"),
			time.Now().Add(time.Second),
		)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "committed-message-too-big", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set committed message-too-big deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "too large after output"}},
	}); err != nil {
		t.Fatalf("write committed message-too-big request: %v", err)
	}
	var delta map[string]any
	if err := downstream.ReadJSON(&delta); err != nil {
		t.Fatalf("read committed message-too-big delta: %v", err)
	}
	if delta["type"] != "response.output_text.delta" {
		t.Fatalf("unexpected event before committed message-too-big close: %#v", delta)
	}
	_, _, err := downstream.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("downstream error after output=%v, want websocket close %d", err, websocket.CloseMessageTooBig)
	}
}

func TestNativeCodexWebsocketUsesChannelHTTPProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("proxied upstream upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("proxied upstream read frame: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-proxy", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method=%q, want CONNECT", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamConn, err := net.Dial("tcp", r.Host)
		if err != nil {
			t.Errorf("proxy dial target %q: %v", r.Host, err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstreamConn.Close()
			t.Error("proxy response writer cannot hijack")
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			_ = upstreamConn.Close()
			t.Errorf("proxy hijack: %v", err)
			return
		}
		_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = upstreamConn.Close() }()
			_, _ = io.Copy(upstreamConn, clientConn)
		}()
		_, _ = io.Copy(clientConn, upstreamConn)
	}))
	defer proxy.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "proxied-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list proxied channel: configs=%d err=%v", len(configs), err)
	}
	configs[0].ProxyURL = proxy.URL
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("set channel proxy: %v", err)
	}
	env.server.InvalidateChannelListCache()

	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set proxied downstream deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "through proxy"}},
	}); err != nil {
		t.Fatalf("write proxied request: %v", err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if proxyCalls.Load() != 1 {
		t.Fatalf("channel proxy calls=%d, want 1", proxyCalls.Load())
	}
}

func TestResponsesWebsocketDoesNotFailOverAfterSemanticOutput(t *testing.T) {
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", upstreamProtocol: "codex", models: "gpt-test", priority: 100},
		{name: "fallback", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	env.server.client = primary.Client()
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}

	var event map[string]any
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read partial websocket event: %v", err)
	}
	if event["type"] != "response.output_text.delta" {
		t.Fatalf("first event=%#v, want output delta", event)
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read terminal websocket error: %v", err)
	}
	errorObject, _ := event["error"].(map[string]any)
	if event["type"] != "error" || event["status"] != float64(http.StatusBadGateway) ||
		errorObject["type"] != "server_error" || errorObject["code"] != responsesWebsocketInterruptedCode {
		t.Fatalf("terminal event=%#v, want interrupted-stream client retry", event)
	}
	_, _, errClose := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errClose, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
		t.Fatalf("terminal close error=%v, want websocket 1011", errClose)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback called %d times after committed output", fallbackCalls.Load())
	}
}

func TestNativeCodexWebsocketAbnormalCloseRequiresTwoPhysicalConnectionsBeforeTargetCooldown(t *testing.T) {
	t.Parallel()

	testNativeCodexWebsocketTransportFailureRequiresTwoPhysicalConnectionsBeforeTargetCooldown(
		t,
		"1006",
		func(conn *websocket.Conn) error { return conn.UnderlyingConn().Close() },
	)
}

func TestNativeCodexWebsocketInterruptedEventRequiresTwoPhysicalConnectionsBeforeTargetCooldown(t *testing.T) {
	t.Parallel()

	testNativeCodexWebsocketTransportFailureRequiresTwoPhysicalConnectionsBeforeTargetCooldown(
		t,
		responsesWebsocketInterruptedCode,
		func(conn *websocket.Conn) error {
			return conn.WriteJSON(map[string]any{
				"type":   "error",
				"status": http.StatusBadGateway,
				"error": map[string]any{
					"type":    "server_error",
					"code":    responsesWebsocketInterruptedCode,
					"message": responsesWebsocketInterruptedMessage,
				},
			})
		},
	)
}

func testNativeCodexWebsocketTransportFailureRequiresTwoPhysicalConnectionsBeforeTargetCooldown(
	t *testing.T,
	failureName string,
	interrupt func(*websocket.Conn) error,
) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var primaryHandshakes atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade %s websocket: %v", failureName, err)
			return
		}
		defer func() { _ = conn.Close() }()
		primaryHandshakes.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read %s request: %v", failureName, err)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type": "response.output_text.delta", "delta": "partial",
		}); err != nil {
			t.Errorf("write %s delta: %v", failureName, err)
			return
		}
		if err := interrupt(conn); err != nil {
			t.Errorf("interrupt %s websocket: %v", failureName, err)
		}
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallbackBodies := make(chan []byte, 1)
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fallback request: %v", err)
			return
		}
		fallbackBodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fallback\",\"status\":\"completed\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer fallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "abnormal-close-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "healthy-http-fallback", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	request := map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "full replay"}},
	}
	readInterruptedTurn := func(conn *websocket.Conn) {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set %s deadline: %v", failureName, err)
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatalf("write %s request: %v", failureName, err)
		}

		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read partial event: %v", err)
		}
		if event["type"] != "response.output_text.delta" {
			t.Fatalf("first event=%#v, want output delta", event)
		}
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read interrupted event: %v", err)
		}
		errorObject, _ := event["error"].(map[string]any)
		if event["type"] != "error" || event["status"] != float64(http.StatusBadGateway) ||
			errorObject["code"] != responsesWebsocketInterruptedCode {
			t.Fatalf("terminal event=%#v, want 502 error", event)
		}
		_, _, closeReadErr := conn.ReadMessage()
		var closeErr *websocket.CloseError
		if !errors.As(closeReadErr, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
			t.Fatalf("terminal close=%v, want websocket 1011", closeReadErr)
		}
		waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)
	}
	assertNoModelCooldown := func() {
		t.Helper()
		cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
		if err != nil {
			t.Fatalf("get model cooldowns: %v", err)
		}
		if len(cooldowns) != 0 {
			t.Fatalf("native websocket %s created model cooldowns: %v", failureName, cooldowns)
		}
	}

	first := dialResponsesWebsocketWithSessionID(t, env.engine, "two-physical-failures-a")
	readInterruptedTurn(first)
	if primaryHandshakes.Load() != 1 || fallbackCalls.Load() != 0 {
		t.Fatalf("after first %s primary/fallback=%d/%d, want 1/0", failureName, primaryHandshakes.Load(), fallbackCalls.Load())
	}
	assertNoModelCooldown()

	entry := waitForProxyLog(t, env, "gpt-test")
	if entry.StatusCode != util.StatusStreamIncomplete {
		t.Fatalf("proxy log status=%d message=%q, want %d", entry.StatusCode, entry.Message, util.StatusStreamIncomplete)
	}
	if !entry.UpstreamWebsocket {
		t.Fatal("proxy log did not mark native upstream websocket")
	}

	second := dialResponsesWebsocketWithSessionID(t, env.engine, "two-physical-failures-b")
	readInterruptedTurn(second)
	if primaryHandshakes.Load() != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf("after second %s primary/fallback=%d/%d, want 2/0", failureName, primaryHandshakes.Load(), fallbackCalls.Load())
	}
	assertNoModelCooldown()

	third := dialResponsesWebsocketWithSessionID(t, env.engine, "two-physical-failures-b")
	if err := third.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set fallback deadline: %v", err)
	}
	if err := third.WriteJSON(request); err != nil {
		t.Fatalf("write fallback replay request: %v", err)
	}
	readWebsocketUntilType(t, third, "response.completed")
	if primaryHandshakes.Load() != 2 || fallbackCalls.Load() != 1 {
		t.Fatalf("after target cooldown primary/fallback=%d/%d, want 2/1", primaryHandshakes.Load(), fallbackCalls.Load())
	}
	fallbackBody := <-fallbackBodies
	if gjson.GetBytes(fallbackBody, "previous_response_id").Exists() ||
		gjson.GetBytes(fallbackBody, "input.0.content").String() != "full replay" {
		t.Fatalf("fallback did not receive full replay: %s", fallbackBody)
	}
}

func TestNativeCodexWebsocketCompletedTurnResetsAbnormalCloseStreak(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var primaryHandshakes atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade reset-streak websocket: %v", err)
			return
		}
		connection := primaryHandshakes.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read reset-streak request: %v", err)
			_ = conn.Close()
			return
		}
		if connection%2 == 1 {
			_ = conn.WriteJSON(map[string]any{
				"type": "response.output_text.delta", "delta": "partial",
			})
			_ = conn.UnderlyingConn().Close()
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-reset", "status": "completed", "output": []any{},
			},
		})
		_ = conn.Close()
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"unexpected-fallback\",\"status\":\"completed\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer fallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "reset-streak-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "must-not-run", upstreamProtocol: "codex", models: "gpt-test", priority: 90},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	request := map[string]any{
		"type": "response.create", "model": "gpt-test",
		"input": []any{map[string]any{"role": "user", "content": "reset streak"}},
	}
	runInterrupted := func(sessionID string) {
		t.Helper()
		conn := dialResponsesWebsocketWithSessionID(t, env.engine, sessionID)
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set interrupted deadline: %v", err)
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatalf("write interrupted request: %v", err)
		}
		readWebsocketUntilType(t, conn, "response.output_text.delta")
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil || event["type"] != "error" {
			t.Fatalf("interrupted event=%#v err=%v", event, err)
		}
		_, _, _ = conn.ReadMessage()
		waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)
	}
	runCompleted := func(sessionID string) {
		t.Helper()
		conn := dialResponsesWebsocketWithSessionID(t, env.engine, sessionID)
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set completed deadline: %v", err)
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatalf("write completed request: %v", err)
		}
		readWebsocketUntilType(t, conn, "response.completed")
		_ = conn.Close()
		waitForResponsesWebsocketAttachments(t, env.server.responsesExecutionSessions, 0)
	}

	runInterrupted("reset-streak-1")
	runCompleted("reset-streak-2")
	runInterrupted("reset-streak-3")
	runCompleted("reset-streak-4")
	if primaryHandshakes.Load() != 4 || fallbackCalls.Load() != 0 {
		t.Fatalf("handshakes/fallback=%d/%d, want 4/0 after completed turn reset", primaryHandshakes.Load(), fallbackCalls.Load())
	}
}

func TestResponsesWebsocketPersistsUsageCostAndRedactedDebugContent(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var handshakes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, http.Header{"X-Upstream-Handshake": []string{"native-codex"}})
		if err != nil {
			t.Errorf("upgrade logging websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read logging websocket request: %v", err)
			return
		}
		if handshakes.Add(1) == 1 {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "logged"})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-log",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "logged"}},
				}},
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
			},
		})
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name:             "codex-native-ws",
		upstreamProtocol: "codex",
		websockets:       true,
		models:           "gpt-4o-mini",
		apiKey:           "sk-upstream-secret",
		priority:         100,
	}}, map[int]string{0: upstream.URL})
	env.server.configService.mu.Lock()
	env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	env.server.configService.mu.Unlock()

	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-4o-mini",
		"input": []any{map[string]any{"role": "user", "content": "audit me"}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	entry := waitForProxyLog(t, env, "gpt-4o-mini")
	if entry.InputTokens != 100 || entry.OutputTokens != 50 || entry.Cost <= 0 {
		t.Fatalf("unexpected websocket billing log: %+v", entry)
	}
	if !entry.IsStreaming {
		t.Fatal("websocket proxy log must be marked streaming")
	}
	if !entry.UpstreamWebsocket {
		t.Fatal("native websocket proxy log must be marked as upstream websocket")
	}
	debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("get websocket debug log: %v", err)
	}
	if debugLog == nil {
		t.Fatal("websocket request must persist debug content when debug logging is enabled")
	}
	if strings.Contains(debugLog.ReqHeaders, "sk-upstream-secret") {
		t.Fatalf("debug headers leaked upstream API key: %s", debugLog.ReqHeaders)
	}
	if !strings.Contains(debugLog.ReqHeaders, codexResponsesWebsocketBeta) {
		t.Fatalf("debug request headers do not reflect emitted beta header: %s", debugLog.ReqHeaders)
	}
	if debugLog.ReqMethod != "WEBSOCKET" || !strings.HasPrefix(debugLog.ReqURL, "ws://") {
		t.Fatalf("debug transport method=%q url=%q, want WebSocket wire request", debugLog.ReqMethod, debugLog.ReqURL)
	}
	if debugLog.RespStatus != http.StatusSwitchingProtocols ||
		!strings.Contains(debugLog.RespHeaders, "X-CCLoad-Upstream-Transport") ||
		!strings.Contains(debugLog.RespHeaders, "native-codex") ||
		gjson.Get(debugLog.RespHeaders, "X-CCLoad-WebSocket-Reconnects").Uint() != 1 {
		t.Fatalf("debug handshake status=%d headers=%s", debugLog.RespStatus, debugLog.RespHeaders)
	}
	if gjson.GetBytes(debugLog.ReqBody, "type").String() != "response.create" ||
		!gjson.GetBytes(debugLog.ReqBody, "stream").Bool() {
		t.Fatalf("debug request is not the emitted WebSocket frame: %s", debugLog.ReqBody)
	}
	if !strings.Contains(string(debugLog.ReqBody), "audit me") || !strings.Contains(string(debugLog.RespBody), "response.completed") {
		t.Fatalf("debug request/response content missing: request=%q response=%q", debugLog.ReqBody, debugLog.RespBody)
	}
}

func TestResponsesWebsocketExposesActualUpstreamTransportWhileActive(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	requestStarted := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseResponse:
		default:
			close(releaseResponse)
		}
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade active-request websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read active-request websocket payload: %v", err)
			return
		}
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		<-releaseResponse
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-active-transport", "output": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "active-native-ws", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set active-request websocket deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test",
		"reasoning": map[string]any{"effort": "high"},
		"input":     []any{map[string]any{"role": "user", "content": "show active transport"}},
	}); err != nil {
		t.Fatalf("write active-request websocket request: %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream websocket request did not start")
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))
	env.server.HandleActiveRequests(c)
	if w.Code != http.StatusOK {
		t.Fatalf("active requests status=%d, want %d", w.Code, http.StatusOK)
	}
	var activeResponse struct {
		Data []ActiveRequest `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &activeResponse)
	if len(activeResponse.Data) != 1 {
		t.Fatalf("active requests=%d, want 1", len(activeResponse.Data))
	}
	if !activeResponse.Data[0].UpstreamWebsocket {
		t.Fatal("active request must expose the actual upstream websocket transport")
	}
	if activeResponse.Data[0].ThinkingEffort != "high" {
		t.Fatalf("active request thinking_effort=%q, want high", activeResponse.Data[0].ThinkingEffort)
	}

	close(releaseResponse)
	readWebsocketUntilType(t, downstream, "response.completed")
}

func TestNativeCodexWebsocketFailedTerminalPersistsUsageWithoutCost(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed-terminal websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read failed-terminal request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": "resp-failed-cost", "status": "failed", "output": []any{},
				"error": map[string]any{"code": "server_error", "message": "failed after generation"},
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
			},
		})
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "failed-cost-native", upstreamProtocol: "codex", websockets: true,
		models: "gpt-4o-mini", priority: 100,
	}}, map[int]string{0: upstream.URL})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set failed-terminal deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-4o-mini",
		"input": []any{map[string]any{"role": "user", "content": "bill failure"}},
	}); err != nil {
		t.Fatalf("write failed-terminal request: %v", err)
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read failed-terminal event: %v", err)
		}
		if event["type"] == "response.failed" {
			break
		}
	}
	entry := waitForProxyLog(t, env, "gpt-4o-mini")
	if entry.InputTokens != 100 || entry.OutputTokens != 50 {
		t.Fatalf("failed-terminal usage log=%+v", entry)
	}
	if entry.Cost != 0 {
		t.Fatalf("failed-terminal cost=%v, want 0", entry.Cost)
	}
}

func TestResponsesWebsocketBridgesToGeminiHTTPChannel(t *testing.T) {
	requestSeen := make(chan struct {
		path string
		body []byte
	}, 4)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestSeen <- struct {
			path string
			body []byte
		}{path: r.URL.Path, body: body}
		if r.URL.Path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid URL (POST /v1/responses)"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello from Gemini\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3,\"totalTokenCount\":8}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:                  "gemini-http",
		upstreamProtocol:      "gemini",
		protocolTransformMode: model.ProtocolTransformModeAuto,
		models:                "gemini-2.5-pro",
		priority:              100,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list test channel: configs=%d err=%v", len(configs), err)
	}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("enable codex transform: %v", err)
	}
	env.server.InvalidateChannelListCache()

	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type":  "response.create",
		"model": "gemini-2.5-pro",
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
		}},
	}); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	readWebsocketUntilType(t, conn, "response.completed")

	attempts := make([]struct {
		path string
		body []byte
	}, 4)
	for idx := range attempts {
		attempts[idx] = <-requestSeen
	}
	paths := []string{attempts[0].path, attempts[1].path, attempts[2].path, attempts[3].path}
	if !slices.Equal(paths, []string{
		"/v1/responses",
		"/v1/chat/completions",
		"/v1/messages",
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent",
	}) {
		t.Fatalf("protocol attempts=%v, want client Codex then OpenAI, Anthropic, Gemini", paths)
	}
	if !bytes.Contains(attempts[3].body, []byte(`"contents"`)) {
		t.Fatalf("unexpected Gemini bridge request body=%s", attempts[3].body)
	}
}
