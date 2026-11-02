package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

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
}
