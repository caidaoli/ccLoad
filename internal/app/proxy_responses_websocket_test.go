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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func dialResponsesWebsocket(t testing.TB, handler http.Handler) *websocket.Conn {
	return dialResponsesWebsocketWithToken(t, handler, "test-api-key")
}

func dialResponsesWebsocketWithToken(t testing.TB, handler http.Handler, token string) *websocket.Conn {
	return dialResponsesWebsocketAtPath(t, handler, token, "/v1/responses")
}

// dialResponsesWebsocketAtPath dials a Responses WebSocket at an arbitrary
// upgrade path, so tests can cover route aliases (e.g. the Codex CLI direct
// route /backend-api/codex/responses) alongside the canonical /v1/responses.
func dialResponsesWebsocketAtPath(t testing.TB, handler http.Handler, token, path string) *websocket.Conn {
	t.Helper()
	appServer := httptest.NewServer(handler)
	t.Cleanup(appServer.Close)
	return dialResponsesWebsocketAtURL(t, appServer.URL, token, path, "")
}

func dialResponsesWebsocketAtURL(
	t testing.TB,
	serverURL string,
	token string,
	path string,
	sessionID string,
) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	if sessionID != "" {
		headers.Set("Session-Id", sessionID)
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

func TestResponsesExecutionSessionStoreRejectsUnboundedActiveSessions(t *testing.T) {
	store := newResponsesExecutionSessionStore(nil)
	store.maxSessions = 1
	defer store.close()

	_, release, err := store.acquire("token-a", "session-a")
	if err != nil {
		t.Fatalf("acquire first session: %v", err)
	}
	defer release()

	if _, _, err = store.acquire("token-a", "session-b"); !errors.Is(err, errResponsesExecutionSessionCapacity) {
		t.Fatalf("second active session error=%v, want capacity error", err)
	}
}

// TestResponsesExecutionSessionStoreEvictsIdleSessionAtCapacity locks down the
// capacity contract: once the flat ceiling is hit, acquire evicts the
// least-recently-used *idle* session instead of rejecting, so one subject's
// idle sessions can never starve every other subject for a full TTL. The
// evicted client recovers through the documented full-transcript replay path.
func TestResponsesExecutionSessionStoreEvictsIdleSessionAtCapacity(t *testing.T) {
	store := newResponsesExecutionSessionStore(nil)
	store.maxSessions = 2
	defer store.close()

	oldest, releaseOldest, err := store.acquire("token-a", "session-a")
	if err != nil {
		t.Fatalf("acquire first session: %v", err)
	}
	releaseOldest()
	newer, releaseNewer, err := store.acquire("token-a", "session-b")
	if err != nil {
		t.Fatalf("acquire second session: %v", err)
	}
	releaseNewer()
	oldest.lastAccess = time.Now().Add(-time.Minute)

	third, releaseThird, err := store.acquire("token-b", "session-c")
	if err != nil {
		t.Fatalf("acquire at capacity with idle sessions should evict LRU, got %v", err)
	}
	releaseThird()
	if third == oldest || third == newer {
		t.Fatal("eviction must create a fresh session, not reuse the victim")
	}

	stats := store.stats()
	if stats.Sessions != 2 {
		t.Fatalf("unexpected session store stats: %+v", stats)
	}

	// LRU 语义:被逐出的是最久未访问的 session-a,session-b 仍在。
	survivor, releaseSurvivor, err := store.acquire("token-a", "session-b")
	if err != nil {
		t.Fatalf("reacquire surviving session: %v", err)
	}
	releaseSurvivor()
	if survivor != newer {
		t.Fatal("LRU eviction removed the wrong session")
	}
}

func TestResponsesExecutionSessionStoreCountsTransientWebsocketSessions(t *testing.T) {
	store := newResponsesExecutionSessionStore(nil)
	store.maxSessions = 1
	defer store.close()

	_, release, err := store.acquire("token-a", "")
	if err != nil {
		t.Fatalf("acquire transient session: %v", err)
	}
	if _, _, err = store.acquire("token-a", ""); !errors.Is(err, errResponsesExecutionSessionCapacity) {
		t.Fatalf("second transient session error=%v, want capacity error", err)
	}
	release()

	_, releaseAgain, err := store.acquire("token-a", "")
	if err != nil {
		t.Fatalf("acquire transient session after release: %v", err)
	}
	releaseAgain()
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
	writer := newResponsesWebsocketBridgeWriter(newBridgeWriterTestConn(t))

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

	session := newCodexUpstreamWebsocketSession()
	defer session.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("build websocket request: %v", err)
	}
	target := codexWebsocketTarget{channelID: 1, url: req.URL.String()}
	conn, resp, err := session.dial(context.Background(), websocket.DefaultDialer, target, req)
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

	baseTarget := codexWebsocketTargetForRequest(base, req, false)
	if baseTarget == codexWebsocketTargetForRequest(withProxy, req, false) {
		t.Fatal("proxy configuration change did not invalidate websocket target")
	}
	if baseTarget == codexWebsocketTargetForRequest(withHeader, req, false) {
		t.Fatal("custom header configuration change did not invalidate websocket target")
	}
	if baseTarget == codexWebsocketTargetForRequest(base, req, true) {
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
		name: "codex-direct-alias", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "codex-http", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "unknown-previous", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "stale-previous", channelType: "codex", models: "gpt-test", priority: 100,
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
	// 必须在建 Server 之后收紧：NewServer 会用系统设置重置包级上限
	withMaxBodyBytes(t, 256)

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
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
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

func TestResponsesWebsocketClosesWhenServerShutsDown(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
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
		name: "long-http", channelType: "codex", models: "gpt-test", priority: 100,
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
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
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
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
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
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
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
		name:        "codex-http",
		channelType: "codex",
		models:      "gpt-test",
		apiKey:      "sk-upstream",
		priority:    100,
	}}, map[int]string{0: upstream.URL})
	// 必须在建 Server 之后收紧：NewServer 会用系统设置重置包级上限
	withMaxBodyBytes(t, 600)
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
		name: "compact-http", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "compact-endpoint", channelType: "codex", websockets: true,
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
		name: "codex-http", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "incomplete-tool-call", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "reconcile-tool-call", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "orphan-tool-output-initial", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "orphan-tool-output-incremental", channelType: "codex", models: "gpt-test", priority: 100,
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
		if !strings.Contains(r.Header.Get("OpenAI-Beta"), "responses_websockets=") {
			t.Errorf("native upstream beta header=%q", r.Header.Get("OpenAI-Beta"))
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
		name: "native-codex", channelType: "codex", websockets: true,
		models: "gpt-test", apiKey: "sk-upstream", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
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
		name: "resumable-native", channelType: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	first := dialResponsesWebsocket(t, env.engine)
	if err := first.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set first resumable deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "prompt_cache_key": "resume-me",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write first resumable request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()

	second := dialResponsesWebsocket(t, env.engine)
	if err := second.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set second resumable deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "prompt_cache_key": "resume-me",
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

func TestResponsesWebsocketExecutionSessionExpires(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-expire\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "expiring-session", channelType: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.responsesExecutionSessions.ttlOverride = 20 * time.Millisecond
	first := dialResponsesWebsocket(t, env.engine)
	if err := first.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set expiring first deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "prompt_cache_key": "expire-me",
		"input": []any{map[string]any{"role": "user", "content": "one"}},
	}); err != nil {
		t.Fatalf("write expiring first request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")
	_ = first.Close()
	time.Sleep(80 * time.Millisecond)

	second := dialResponsesWebsocket(t, env.engine)
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set expiring second deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type": "response.append", "prompt_cache_key": "expire-me",
		"previous_response_id": "resp-expire",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}); err != nil {
		t.Fatalf("write expired continuation: %v", err)
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := second.ReadJSON(&event); err != nil {
		t.Fatalf("read expired continuation error: %v", err)
	}
	if event.Type != "error" || event.Error.Code != "invalid_request" {
		t.Fatalf("expired continuation event=%+v", event)
	}
	if calls.Load() != 1 {
		t.Fatalf("expired continuation reached upstream; calls=%d", calls.Load())
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
		name: "isolated-session", channelType: "codex", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	injectAPIToken(env.server.authService, "other-api-key", 0, 2)
	first := dialResponsesWebsocketWithToken(t, env.engine, "test-api-key")
	if err := first.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set isolated first deadline: %v", err)
	}
	if err := first.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "prompt_cache_key": "shared-name",
		"input": []any{map[string]any{"role": "user", "content": "private"}},
	}); err != nil {
		t.Fatalf("write isolated first request: %v", err)
	}
	readWebsocketUntilType(t, first, "response.completed")

	second := dialResponsesWebsocketWithToken(t, env.engine, "other-api-key")
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set isolated second deadline: %v", err)
	}
	if err := second.WriteJSON(map[string]any{
		"type": "response.append", "prompt_cache_key": "shared-name",
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
		name: "http-native", channelType: "codex", websockets: true,
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
		name: "cross-transport", channelType: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstreamWS := dialResponsesWebsocket(t, env.engine)
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
		"model": "gpt-test", "stream": true, "prompt_cache_key": "cross-mode",
		"previous_response_id": "resp-cross-1",
		"input":                []any{map[string]any{"role": "user", "content": "two"}},
	}, nil)
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
		name: "unknown-previous", channelType: "codex", websockets: true,
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
		name: "replacement-native", channelType: "codex", websockets: true,
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
		name: "transport-switch", channelType: "codex", websockets: true,
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

	env := setupProxyTestEnv(t, []testChannel{{
		name: "pinned-native", channelType: "codex", websockets: true,
		models: "gpt-test", apiKey: "sk-pin-0", priority: 100,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
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
	}

	if handshakes.Load() != 1 {
		t.Fatalf("pinned websocket handshakes=%d, want 1", handshakes.Load())
	}
	if authorization := <-authorizations; authorization != "Bearer sk-pin-1" {
		t.Fatalf("pinned authorization=%q, want first round-robin key", authorization)
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
		name: "ping-native", channelType: "codex", websockets: true,
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
				name: "terminal-native", channelType: "codex", websockets: true,
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
		name: "http-prewarm", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "initial-prewarm-only", channelType: "codex", models: "gpt-test", priority: 100,
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
		name: "failed-event-native", channelType: "codex", websockets: true,
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
		// No semantic event: closing the transport must permit a replay on another channel.
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
		{name: "primary-native", channelType: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback-http", channelType: "codex", models: "gpt-test", priority: 90},
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
}

func TestNativeCodexWebsocketPreviousResponseNotFoundReconnectsWithReplay(t *testing.T) {
	requests := make(chan map[string]any, 3)
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade previous-response websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := handshakes.Add(1)
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
		name: "previous-response-replay", channelType: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocket(t, env.engine)
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
		{name: "failing-native", channelType: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback-native", channelType: "codex", websockets: true, models: "gpt-test", priority: 90},
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
		{name: "primary", channelType: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "fallback", channelType: "codex", models: "gpt-test", priority: 90},
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
		{name: "http-primary", channelType: "codex", models: "gpt-test", priority: 100},
		{name: "http-secondary", channelType: "codex", models: "gpt-test", priority: 90},
		{name: "native-fallback", channelType: "codex", websockets: true, models: "gpt-test", priority: 1},
	}, map[int]string{0: primary.URL, 1: secondary.URL, 2: fallback.URL})
	env.server.client = primary.Client()

	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	first := dialResponsesWebsocketAtURL(
		t, appServer.URL, "test-api-key", "/v1/responses", "retryable-replay",
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
		t, appServer.URL, "test-api-key", "/v1/responses", "retryable-replay",
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
	var websocketCalls atomic.Int32
	var httpCalls atomic.Int32
	longInputID := strings.Repeat("fallback-item-", 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			websocketCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUpgradeRequired)
			_, _ = io.WriteString(w, `{"error":{"message":"websocket disabled"}}`)
			return
		}
		httpCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			t.Errorf("same-channel HTTP replay body=%q err=%v", body, err)
		}
		if gjson.GetBytes(body, "generate").Exists() {
			t.Errorf("websocket-only generate leaked into HTTP fallback: %s", body)
		}
		if got := gjson.GetBytes(body, "input.0.id").String(); len([]rune(got)) != codexInputItemIDLimit {
			t.Errorf("HTTP fallback input id length=%d, want %d: %q", len([]rune(got)), codexInputItemIDLimit, got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http-fallback\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "same-channel-fallback", channelType: "codex", websockets: true,
		models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.client = upstream.Client()
	downstream := dialResponsesWebsocket(t, env.engine)
	if err := downstream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set same-channel fallback deadline: %v", err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "generate": true,
		"input": []any{map[string]any{"type": "message", "id": longInputID, "role": "user", "content": "fallback"}},
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
}

func TestNativeCodexWebsocketEOFHandshakeFallsBackToSameChannelHTTP(t *testing.T) {
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
		name: "eof-handshake-fallback", channelType: "codex", websockets: true,
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
		name: "reconnect-http-fallback", channelType: "codex", websockets: true,
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
		{name: "message-too-big-primary", channelType: "codex", websockets: true, models: "gpt-test", priority: 100},
		{name: "message-too-big-fallback", channelType: "codex", models: "gpt-test", priority: 90},
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
		name: "committed-message-too-big", channelType: "codex", websockets: true,
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
		name: "proxied-native", channelType: "codex", websockets: true,
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
		{name: "primary", channelType: "codex", models: "gpt-test", priority: 100},
		{name: "fallback", channelType: "codex", websockets: true, models: "gpt-test", priority: 90},
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
	if event["type"] != "error" || event["status"] == float64(http.StatusBadGateway) {
		t.Fatalf("terminal event=%#v, want non-retryable error", event)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback called %d times after committed output", fallbackCalls.Load())
	}
}

func TestResponsesWebsocketPersistsUsageCostAndRedactedDebugContent(t *testing.T) {
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
		name:        "codex-native-ws",
		channelType: "codex",
		websockets:  true,
		models:      "gpt-4o-mini",
		apiKey:      "sk-upstream-secret",
		priority:    100,
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
		name: "active-native-ws", channelType: "codex", websockets: true,
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
		name: "failed-cost-native", channelType: "codex", websockets: true,
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
	}, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestSeen <- struct {
			path string
			body []byte
		}{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello from Gemini\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3,\"totalTokenCount\":8}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:        "gemini-http",
		channelType: "gemini",
		models:      "gemini-2.5-pro",
		priority:    100,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list test channel: configs=%d err=%v", len(configs), err)
	}
	configs[0].ProtocolTransformMode = model.ProtocolTransformModeLocal
	configs[0].ProtocolTransforms = []string{"codex"}
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

	seen := <-requestSeen
	if seen.path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" || !bytes.Contains(seen.body, []byte(`"contents"`)) {
		t.Fatalf("unexpected Gemini bridge request path=%q body=%s", seen.path, seen.body)
	}
}
