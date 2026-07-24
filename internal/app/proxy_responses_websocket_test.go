package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gorilla/websocket"
)

func dialResponsesWebsocket(t testing.TB, handler http.Handler) *websocket.Conn {
	t.Helper()
	appServer := httptest.NewServer(handler)
	t.Cleanup(appServer.Close)

	headers := http.Header{"Authorization": []string{"Bearer test-api-key"}}
	wsURL := "ws" + strings.TrimPrefix(appServer.URL, "http") + "/v1/responses"
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
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read websocket error event: %v", err)
	}
	if event.Type != "error" || event.Error.Type != "invalid_request_error" || event.Error.Code != "unsupported_event" {
		t.Fatalf("unexpected websocket error event: %+v", event)
	}
}

func TestResponsesWebsocketRejectsNonHTTPOrigin(t *testing.T) {
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
	appServer := httptest.NewServer(env.engine)
	defer appServer.Close()

	headers := http.Header{
		"Authorization": []string{"Bearer test-api-key"},
		"Origin":        []string{"ftp://" + strings.TrimPrefix(appServer.URL, "http://")},
	}
	wsURL := "ws" + strings.TrimPrefix(appServer.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("websocket upgrade accepted a non-HTTP Origin")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("websocket origin rejection status=%v, want 403", resp)
	}
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

func TestResponsesWebsocketRejectsBinaryAndOversizedFrames(t *testing.T) {
	t.Setenv("CCLOAD_MAX_BODY_BYTES", "256")
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid websocket frame must not reach upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-http", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

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
	t.Setenv("CCLOAD_MAX_BODY_BYTES", "600")
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

func TestResponsesWebsocketFailsOverBeforeSemanticOutput(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
	}))
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fallback\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fallback\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", channelType: "codex", models: "gpt-test", priority: 100},
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

func TestResponsesWebsocketDoesNotFailOverAfterSemanticOutput(t *testing.T) {
	var fallbackCalls atomic.Int32
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", channelType: "codex", models: "gpt-test", priority: 100},
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
	if event["type"] != "error" {
		t.Fatalf("terminal event=%#v, want error", event)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback called %d times after committed output", fallbackCalls.Load())
	}
}

func TestResponsesWebsocketPersistsUsageCostAndRedactedDebugContent(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"logged\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-log\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"logged\"}]}],\"usage\":{\"input_tokens\":100,\"output_tokens\":50,\"total_tokens\":150}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name:        "codex-http",
		channelType: "codex",
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
	if !strings.Contains(string(debugLog.ReqBody), "audit me") || !strings.Contains(string(debugLog.RespBody), "response.completed") {
		t.Fatalf("debug request/response content missing: request=%q response=%q", debugLog.ReqBody, debugLog.RespBody)
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
