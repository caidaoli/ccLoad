package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/privacy"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func privacyRequest(t *testing.T, env *proxyTestEnv, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	env.engine.ServeHTTP(recorder, req)
	return recorder
}

func TestPrivacyProxyHTTPHotReloadAndProtocolConversion(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	received := make(chan string, 8)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		content := gjson.GetBytes(body, "messages.0.content").String()
		received <- content
		if r.Header.Get("Authorization") != "Bearer sk-test-0" {
			t.Error("upstream authentication changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-privacy", "object": "chat.completion", "model": "gpt-test", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
	}))
	env := setupProxyTestEnv(t, []testChannel{{name: "privacy", upstreamProtocol: "openai", models: "gpt-test"}}, map[int]string{0: upstream.URL})
	for _, enabled := range []bool{false, true, false} {
		if w := batchUpdateSetting(t, env.server, privacySettingKey, fmt.Sprintf(`{"enabled":%t}`, enabled)); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		for _, client := range []string{"openai", "anthropic"} {
			path := "/v1/chat/completions"
			body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": secret}}}
			if client == "anthropic" {
				path = "/v1/messages"
				body["max_tokens"] = 64
			}
			response := privacyRequest(t, env, path, body)
			if response.Code != 200 {
				t.Fatalf("response %s: %d %s", client, response.Code, response.Body.String())
			}
			sent := <-received
			if (sent == secret) == enabled {
				t.Fatalf("enabled=%t sent=%q", enabled, sent)
			}
			textPath := "choices.0.message.content"
			if client == "anthropic" {
				textPath = "content.0.text"
			}
			if got := gjson.GetBytes(response.Body.Bytes(), textPath).String(); got != secret {
				t.Fatalf("response content=%q want=%q: %s", got, secret, response.Body.String())
			}
		}
	}
}

func TestPrivacyProxySSESplitPlaceholder(t *testing.T) {
	t.Parallel()
	const secret = "private\\project\"name"
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		masked := gjson.GetBytes(body, "messages.0.content").String()
		if masked == secret {
			t.Error("unmasked request reached upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []string{masked[:7], masked[7:]} {
			data, _ := json.Marshal(map[string]any{"id": "chatcmpl-private", "object": "chat.completion.chunk", "model": "gpt-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	policy, _ := json.Marshal(map[string]any{"enabled": true, "words": []string{secret}})
	env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-stream", models: "gpt-test"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: string(policy)})
	response := privacyRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": secret}}})
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var text strings.Builder
	var total int64
	done := false
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		if !json.Valid([]byte(data)) {
			t.Fatalf("invalid event: %q", data)
		}
		text.WriteString(gjson.Get(data, "choices.0.delta.content").String())
		if n := gjson.Get(data, "usage.total_tokens").Int(); n > 0 {
			total = n
		}
	}
	if text.String() != secret || !done || total != 5 {
		t.Fatalf("text=%q done=%t usage=%d", text.String(), done, total)
	}
}

func TestPrivacyProxyResponsesWebsocket(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body []byte
				var conn *websocket.Conn
				if native {
					var err error
					conn, err = (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, body, err = conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
				} else {
					body, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
				}
				masked := gjson.GetBytes(body, "input.0.content.0.text").String()
				if masked == "" {
					masked = gjson.GetBytes(body, "input").String()
				}
				if strings.Contains(string(body), secret) || !strings.Contains(masked, "{{CCLOAD_") {
					t.Errorf("unexpected upstream body: %s", body)
				}
				events := []map[string]any{
					{"type": "response.output_text.delta", "item_id": "msg-private", "output_index": 0, "content_index": 0, "delta": masked[:min(9, len(masked))]},
					{"type": "response.output_text.delta", "item_id": "msg-private", "output_index": 0, "content_index": 0, "delta": masked[min(9, len(masked)):]},
					{"type": "response.completed", "response": map[string]any{"id": "resp-private", "status": "completed", "output": []any{map[string]any{"id": "msg-private", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": masked}}}}, "usage": map[string]int{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}}},
				}
				for _, event := range events {
					if native {
						if err := conn.WriteJSON(event); err != nil {
							t.Error(err)
							return
						}
					} else {
						data, _ := json.Marshal(event)
						_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
					}
				}
				if native {
					_, _, _ = conn.ReadMessage()
				}
			})
			var upstreamURL string
			if native {
				upstream := httptest.NewServer(handler)
				t.Cleanup(upstream.Close)
				upstreamURL = upstream.URL
			} else {
				upstreamURL = newTestHTTPServer(t, handler).URL
			}
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-ws", upstreamProtocol: "codex", websockets: native, models: "gpt-test", authType: model.AuthTypeCodexOAuth, oauthCredential: codexProxyTestCredential(t, "at-private", "rt-private", "account-private")}}, map[int]string{0: upstreamURL}, map[string]string{privacySettingKey: `{"enabled":true}`})
			conn := dialResponsesWebsocket(t, env.engine)
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-test", "input": secret}); err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				switch gjson.GetBytes(payload, "type").String() {
				case "response.output_text.delta":
					text.WriteString(gjson.GetBytes(payload, "delta").String())
				case "response.completed":
					if text.String() != secret || gjson.GetBytes(payload, "response.output.0.content.0.text").String() != secret {
						t.Fatalf("unrestored reply: %q %s", text.String(), payload)
					}
					return
				case "error", "response.failed":
					t.Fatalf("unexpected failure: %s", payload)
				}
			}
		})
	}
}

func TestPrivacyProxyChannelRulesAndDisableRestore(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-channelrule1234567890abcdefgh"
	var remembered string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent := gjson.GetBytes(body, "messages.0.content").String()
		if remembered == "" {
			if sent == secret || !strings.Contains(sent, "{{CCLOAD_") {
				t.Errorf("channel rule bypassed masking: %s", body)
			}
			remembered = sent
		} else if sent != secret {
			t.Errorf("disabled masking still changed new request: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-history", "model": "gpt-test", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": remembered}, "finish_reason": "stop"}}})
	}))
	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{Action: model.RuleActionOverride, Path: "messages.0.content", Value: json.RawMessage(fmt.Sprintf("%q", secret))}}}
	env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-rules", models: "gpt-test", customRequestRules: rules}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true}`})
	for _, enabled := range []bool{true, false} {
		if !enabled {
			if w := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":false}`); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		}
		response := privacyRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		if response.Code != 200 || gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String() != secret {
			t.Fatalf("enabled=%t status=%d body=%s", enabled, response.Code, response.Body.String())
		}
		// Disabling stops masking counts; restoring remembered values still counts.
		wantRestored := uint64(1)
		if !enabled {
			wantRestored = 2
		}
		if metrics := readPrivacyRuntime(t, env.server); metrics.MaskedRequests != 1 || metrics.MaskedSecret != 1 || metrics.RestoredPlaceholders != wantRestored {
			t.Fatalf("enabled=%t privacy runtime metrics=%+v", enabled, metrics)
		}
	}
	var masked int
	for _, entry := range waitForProxyLogs(t, env, 2) {
		if entry.PrivacyMasked != nil {
			masked++
		}
	}
	if masked != 1 {
		t.Fatalf("masked logs=%d, want only the enabled round", masked)
	}
}

func TestPrivacyAutomaticReasoningRequestIsMasked(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-judge1234567890abcdefghijkl"
	judge := newReasoningAutoJudge(t, "high")
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{name: "private-business", models: "gpt-5.6-sol"}}, []string{business.server.URL})
	if w := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	response := privacyRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Investigate the production incident using "+secret))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests := judge.snapshot()
	if len(requests) != 1 || strings.Contains(requests[0].Conversation, secret) || !strings.Contains(requests[0].Conversation, "{{CCLOAD_") {
		t.Fatalf("judge request was not masked: %+v", requests)
	}
	if decision := waitForDecisionLog(t, env); decision.PrivacyMasked == nil || decision.PrivacyMasked.Secret != 1 {
		t.Fatalf("decision log privacy counts=%+v, want secret 1", decision.PrivacyMasked)
	}
}

// A decision call rejected by the channel's local limit never leaves the
// gateway, so its log must not claim redacted content was sent.
func TestPrivacyLocallyLimitedReasoningDecisionCountsNothing(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t)
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{name: "private-business", models: "gpt-5.6-sol"}}, []string{business.server.URL})
	if w := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ctx := context.Background()
	cfg, err := env.store.GetConfig(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConcurrency = 1
	if cfg, err = env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	release, err := env.server.reserveUpstreamRequest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	response := privacyRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Investigate the production incident using sk-proj-judge1234567890abcdefghijkl"))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if requests := judge.snapshot(); len(requests) != 0 {
		t.Fatalf("judge received %d requests, want 0", len(requests))
	}
	if decision := waitForDecisionLog(t, env); decision.PrivacyMasked != nil {
		t.Fatalf("locally limited decision log privacy counts=%+v, want none", decision.PrivacyMasked)
	}
}

type privacyDiagnosticCapture chan string

func (c privacyDiagnosticCapture) Write(data []byte) (int, error) {
	if line := string(data); strings.Contains(line, "privacy response has unrestored placeholders") {
		c <- line
	}
	return len(data), nil
}

func capturePrivacyDiagnostics(t *testing.T) privacyDiagnosticCapture {
	t.Helper()
	capture := make(privacyDiagnosticCapture, 16)
	previous := log.Writer()
	log.SetOutput(capture)
	t.Cleanup(func() { log.SetOutput(previous) })
	return capture
}

func readPrivacyDiagnostic(t *testing.T, capture privacyDiagnosticCapture, wantCount uint64) int64 {
	t.Helper()
	select {
	case line := <-capture:
		_, fields, ok := strings.Cut(line, "request_id=")
		var requestID int64
		var count uint64
		if !ok {
			t.Fatalf("missing request_id: %q", line)
		}
		if n, err := fmt.Sscanf(fields, "%d count=%d", &requestID, &count); err != nil || n != 2 || requestID <= 0 || count != wantCount {
			t.Fatalf("unexpected diagnostic: %q (%v)", line, err)
		}
		if strings.Contains(line, "{{CCLOAD_") || strings.Contains(line, "test-api-key") || strings.Contains(line, "sk-proj-") {
			t.Fatalf("diagnostic exposed content: %q", line)
		}
		return requestID
	case <-time.After(5 * time.Second):
		t.Fatal("missing privacy diagnostic")
		return 0
	}
}

func TestPrivacyProxyUnrestoredHTTPDiagnostics(t *testing.T) {
	const text = "{{CCLOAD_EMAIL_0123456789abcdef}} {{CCLOAD_EMAIL_bad!}} {{CCLOAD_EMAIL_0123"
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			capture := capturePrivacyDiagnostics(t)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, delta := range []string{text[:5], text[5:]} {
						data := privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": delta}}}})
						_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
					}
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-observe", "model": "gpt-test", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
			}))
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-observe", models: "gpt-test"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true}`})
			response := privacyRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-test", "stream": streaming, "messages": []any{map[string]string{"role": "user", "content": "hello"}}})
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			got := gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String()
			if streaming {
				got = ""
				for _, event := range privacyOutputTestEvents(t, response.Body.String()) {
					got += event.Get("choices.0.delta.content").String()
				}
			}
			if got != text {
				t.Fatalf("unrestored text changed: %q", got)
			}
			readPrivacyDiagnostic(t, capture, 3)
			if got := readPrivacyRuntime(t, env.server).UnrestoredPlaceholders; got != 3 {
				t.Fatalf("unrestored placeholders=%d, want 3", got)
			}
			select {
			case line := <-capture:
				t.Fatalf("duplicate request diagnostic: %s", line)
			default:
			}
		})
	}
}

func TestPrivacyProxyUnrestoredWebsocketDiagnostics(t *testing.T) {
	const unknown = "{{CCLOAD_EMAIL_0123456789abcdef}}"
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			capture := capturePrivacyDiagnostics(t)
			var turn atomic.Int64
			respond := func(write func(any) error) {
				index := turn.Add(1)
				for _, delta := range []string{unknown[:5], unknown[5:]} {
					if err := write(map[string]any{"type": "response.output_text.delta", "item_id": "msg-observe", "output_index": 0, "content_index": 0, "delta": delta}); err != nil {
						t.Error(err)
						return
					}
				}
				if err := write(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp-observe-%d", index), "status": "completed", "output": []any{map[string]any{"id": "msg-observe", "type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": unknown}}}}, "usage": map[string]int{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}}}); err != nil {
					t.Error(err)
				}
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if native {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					for range 2 {
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Error(err)
							return
						}
						respond(conn.WriteJSON)
					}
					_, _, _ = conn.ReadMessage()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				respond(func(event any) error {
					data, err := json.Marshal(event)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(w, "data: %s\n\n", data)
					return err
				})
			})
			var upstreamURL string
			if native {
				upstream := httptest.NewServer(handler)
				t.Cleanup(upstream.Close)
				upstreamURL = upstream.URL
			} else {
				upstreamURL = newTestHTTPServer(t, handler).URL
			}
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-observe-ws", upstreamProtocol: "codex", websockets: native, models: "gpt-test", authType: model.AuthTypeCodexOAuth, oauthCredential: codexProxyTestCredential(t, "at-private", "rt-private", "account-private")}}, map[int]string{0: upstreamURL}, map[string]string{privacySettingKey: `{"enabled":true}`})
			conn := dialResponsesWebsocketWithSessionID(t, env.engine, "privacy-observe")
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var previousRequestID int64
			for turnIndex := 1; turnIndex <= 2; turnIndex++ {
				request := map[string]any{"type": "response.create", "model": "gpt-test", "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "hello"}}}}}
				if turnIndex == 2 {
					request["previous_response_id"] = "resp-observe-1"
				}
				if err := conn.WriteJSON(request); err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				for {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					event := gjson.ParseBytes(payload)
					switch event.Get("type").String() {
					case "response.output_text.delta":
						text.WriteString(event.Get("delta").String())
					case "response.completed":
						if text.String() != unknown || event.Get("response.output.0.content.0.text").String() != unknown {
							t.Fatalf("unrestored content changed: %s", payload)
						}
					case "error", "response.failed":
						t.Fatalf("unexpected failure: %s", payload)
					default:
						continue
					}
					if event.Get("type").String() == "response.completed" {
						break
					}
				}
				requestID := readPrivacyDiagnostic(t, capture, 2)
				if got := readPrivacyRuntime(t, env.server).UnrestoredPlaceholders; got != uint64(2*turnIndex) {
					t.Fatalf("unrestored placeholders=%d, want %d", got, 2*turnIndex)
				}
				if requestID == previousRequestID {
					t.Fatalf("WS turns reused request_id=%d", requestID)
				}
				previousRequestID = requestID
			}
			select {
			case line := <-capture:
				t.Fatalf("duplicate turn diagnostic: %s", line)
			default:
			}
		})
	}
}

func TestPrivacyProxyUnrestoredWebsocketFinalErrorDiagnostics(t *testing.T) {
	const unknown = "{{CCLOAD_EMAIL_0123456789abcdef}}"
	for _, test := range []struct {
		name, response, event string
	}{
		{"generic", `{"error":{"message":"` + unknown + `"}}`, "error"},
		{"terminal", `{"type":"response.failed","response":{"id":"resp-private-failed","error":{"message":"` + unknown + `"}}}`, "response.failed"},
		{"retry", `{"type":"error","error":{"message":"` + unknown + `"}}`, "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := capturePrivacyDiagnostics(t)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, test.response)
			}))
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-ws-error", upstreamProtocol: "codex", models: "gpt-test"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true}`})
			conn := dialResponsesWebsocket(t, env.engine)
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-test", "input": "hello"}); err != nil {
				t.Fatal(err)
			}
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			event := gjson.ParseBytes(payload)
			if event.Get("type").String() != test.event {
				t.Fatalf("event type=%q, want %q: %s", event.Get("type").String(), test.event, payload)
			}
			message := event.Get("error.message").String()
			if test.event == "response.failed" {
				message = event.Get("response.error.message").String()
			}
			if !strings.Contains(message, unknown) {
				t.Fatalf("unrestored error content changed: %s", payload)
			}
			readPrivacyDiagnostic(t, capture, 1)
			select {
			case line := <-capture:
				t.Fatalf("duplicate final error diagnostic: %s", line)
			default:
			}
		})
	}
}

func waitForProxyLogs(t *testing.T, env *proxyTestEnv, want int) []*model.LogEntry {
	t.Helper()
	var logs []*model.LogEntry
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		logs, err = env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) >= want {
			break
		}
	}
	if len(logs) != want {
		t.Fatalf("proxy logs=%d, want %d", len(logs), want)
	}
	return logs
}

func privacyCompletionServer(t *testing.T, status func() int) *testHTTPServer {
	t.Helper()
	return newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if code := status(); code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream failed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-privacy","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
}

func TestPrivacyLogRecordsMaskedCounts(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	upstream := privacyCompletionServer(t, func() int { return http.StatusOK })
	env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-log", upstreamProtocol: "openai", models: "gpt-test"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true,"personal":true}`})
	for _, content := range []string{"use " + secret + " and mail alice@mycompany.com", "nothing sensitive here"} {
		if response := privacyRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": content}}}); response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	var masked, plain int
	for _, entry := range waitForProxyLogs(t, env, 2) {
		switch {
		case entry.PrivacyMasked == nil:
			plain++
		case *entry.PrivacyMasked == privacy.Counts{Secret: 1, Personal: 1}:
			masked++
		default:
			t.Fatalf("unexpected privacy counts %+v", *entry.PrivacyMasked)
		}
	}
	if masked != 1 || plain != 1 {
		t.Fatalf("masked logs=%d plain logs=%d, want 1 and 1", masked, plain)
	}
}

func TestPrivacyLogCountsEachAttemptWithoutInflation(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	var calls atomic.Int32
	upstream := privacyCompletionServer(t, func() int {
		if calls.Add(1) == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	env := setupProxyTestEnvWithSettings(t, []testChannel{
		{name: "privacy-retry-a", upstreamProtocol: "openai", models: "gpt-test"},
		{name: "privacy-retry-b", upstreamProtocol: "openai", models: "gpt-test"},
	}, map[int]string{0: upstream.URL, 1: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true}`})
	if response := privacyRequest(t, env, "/v1/chat/completions", map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "use " + secret}}}); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	statuses := map[int]bool{}
	for _, entry := range waitForProxyLogs(t, env, 2) {
		statuses[entry.StatusCode] = true
		if entry.PrivacyMasked == nil || *entry.PrivacyMasked != (privacy.Counts{Secret: 1}) {
			t.Fatalf("status %d privacy counts=%+v, want secret 1", entry.StatusCode, entry.PrivacyMasked)
		}
	}
	if !statuses[http.StatusInternalServerError] || !statuses[http.StatusOK] {
		t.Fatalf("attempt statuses=%v, want 500 and 200", statuses)
	}
	metrics := readPrivacyRuntime(t, env.server)
	if !metrics.Enabled || metrics.MaskedRequests != 1 || metrics.MaskedSecret != 1 || metrics.BlockedRequests != 0 ||
		metrics.DictionaryScopes != 1 || metrics.DictionaryEntries != 1 || metrics.DictionaryBytes <= 0 {
		t.Fatalf("privacy runtime metrics=%+v, want one masked request with one secret", metrics)
	}
}

func TestPrivacyLogNativeWebsocketCountsOnlyIncrement(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for turn := 1; turn <= 2; turn++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Error(err)
				return
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp-private-%d", turn), "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}}); err != nil {
				t.Error(err)
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(upstream.Close)
	env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-native", upstreamProtocol: "codex", websockets: true, models: "gpt-test", apiKey: "sk-upstream"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true,"personal":true}`})
	conn := dialResponsesWebsocket(t, env.engine)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []map[string]any{
		{"type": "response.create", "model": "gpt-test", "input": []any{map[string]any{"role": "user", "content": "use " + secret}}},
		{"type": "response.create", "previous_response_id": "resp-private-1", "input": []any{map[string]any{"role": "user", "content": "mail alice@mycompany.com"}}},
	} {
		if err := conn.WriteJSON(turn); err != nil {
			t.Fatal(err)
		}
		readWebsocketUntilType(t, conn, "response.completed")
	}
	got := map[privacy.Counts]int{}
	for _, entry := range waitForProxyLogs(t, env, 2) {
		if entry.PrivacyMasked == nil {
			t.Fatalf("log without privacy counts: %+v", entry)
		}
		got[*entry.PrivacyMasked]++
	}
	if got[privacy.Counts{Secret: 1}] != 1 || got[privacy.Counts{Personal: 1}] != 1 {
		t.Fatalf("privacy counts per turn=%v, want one secret turn and one personal increment", got)
	}
}

func readPrivacyRuntime(t *testing.T, srv *Server) privacyRuntimeMetrics {
	t.Helper()
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/runtime-metrics", nil))
	srv.HandleRuntimeMetrics(c)
	raw := mustParseAPIResponse[map[string]json.RawMessage](t, w.Body.Bytes()).Data["privacy"]
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("privacy runtime metrics: %v: %s", err, raw)
	}
	for _, key := range []string{"enabled", "masked_requests", "masked_secret", "masked_personal", "masked_word", "masked_custom", "restored_placeholders", "unrestored_placeholders", "blocked_requests", "dictionary_scopes", "dictionary_entries", "dictionary_bytes"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("privacy runtime metrics missing %q: %s", key, raw)
		}
		delete(keys, key)
	}
	if len(keys) != 0 {
		t.Fatalf("unexpected privacy runtime metrics keys: %s", raw)
	}
	var metrics privacyRuntimeMetrics
	if err := json.Unmarshal(raw, &metrics); err != nil {
		t.Fatal(err)
	}
	return metrics
}

func TestPrivacyRuntimeCountsBlockedRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := privacyCompletionServer(t, func() int {
		calls.Add(1)
		return http.StatusOK
	})
	env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "privacy-blocked", upstreamProtocol: "openai", models: "gpt-test"}}, map[int]string{0: upstream.URL}, map[string]string{privacySettingKey: `{"enabled":true}`})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("Content-Type", "application/octet-stream")
	response := httptest.NewRecorder()
	env.engine.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status=%d upstream calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
	if metrics := readPrivacyRuntime(t, env.server); metrics.BlockedRequests != 1 || metrics.MaskedRequests != 0 {
		t.Fatalf("privacy runtime metrics=%+v, want one blocked and no masked request", metrics)
	}
}
