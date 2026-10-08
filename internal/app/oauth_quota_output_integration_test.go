package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"

	"github.com/gorilla/websocket"
)

func setQuotaOutputTestChannel(t *testing.T, env *proxyTestEnv, enabled bool) int64 {
	t.Helper()
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: count=%d err=%v", len(configs), err)
	}
	cfg := configs[0]
	cfg.OAuthQuotaPassthrough = enabled
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	return cfg.ID
}

func TestOAuthQuotaOutputResponsesWebsocketIntegration(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("native_upstream=%t/passthrough=%t", native, enabled), func(t *testing.T) {
				events := []map[string]any{
					{"type": "codex.rate_limits", "rate_limits": map[string]any{"primary": map[string]any{"used_percent": 25, "limit_window_seconds": 604800, "reset_after_seconds": 604800}}},
					{"type": "codex.response.metadata", "headers": map[string]any{"x-codex-primary-used-percent": "25", "x-codex-turn-state": "state-output"}, "metadata": map[string]any{"headers": map[string]any{"x-ratelimit-remaining-requests": "75", "x-request-id": "trace-output"}}},
					{"type": "response.output_text.delta", "delta": "quota response"},
					{"type": "response.completed", "response": map[string]any{"id": "resp-quota-output", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
				}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers := http.Header{"X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"604800"}}
					if native {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, headers)
						if err != nil {
							t.Errorf("upstream upgrade: %v", err)
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Errorf("upstream request: %v", err)
							return
						}
						for _, event := range events {
							if err := conn.WriteJSON(event); err != nil {
								t.Errorf("upstream event: %v", err)
								return
							}
						}
						_, _, _ = conn.ReadMessage()
						return
					}
					for name, values := range headers {
						w.Header()[name] = values
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range events {
						data, err := json.Marshal(event)
						if err != nil {
							t.Errorf("marshal event: %v", err)
							return
						}
						if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
							t.Errorf("write SSE: %v", err)
							return
						}
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
				env := setupProxyTestEnv(t, []testChannel{{name: "quota-output-ws", upstreamProtocol: "codex", websockets: native, models: "gpt-test", authType: model.AuthTypeCodexOAuth, oauthCredential: codexProxyTestCredential(t, "at-output", "rt-output", "account-output")}}, map[int]string{0: upstreamURL})
				channelID := setQuotaOutputTestChannel(t, env, enabled)
				downstream := dialResponsesWebsocket(t, env.engine)
				if err := downstream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := downstream.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-test", "input": "hello"}); err != nil {
					t.Fatal(err)
				}
				sawQuota, sawQuotaHeader, sawState, sawTrace, sawText := false, false, false, false, false
				for {
					var event map[string]any
					if err := downstream.ReadJSON(&event); err != nil {
						t.Fatal(err)
					}
					switch event["type"] {
					case "error", "response.failed":
						t.Fatalf("unexpected error: %#v", event)
					case "codex.rate_limits":
						sawQuota = true
					case "codex.response.metadata":
						headers, _ := event["headers"].(map[string]any)
						for name, value := range headers {
							switch strings.ToLower(name) {
							case "x-codex-primary-used-percent":
								sawQuotaHeader = value == "25"
								if !enabled {
									t.Fatalf("quota header leaked: %#v", event)
								}
							case "x-codex-turn-state":
								state, _ := value.(string)
								sawState = strings.HasSuffix(state, "state-output")
							}
						}
						metadata, _ := event["metadata"].(map[string]any)
						nested, _ := metadata["headers"].(map[string]any)
						if _, exists := nested["x-ratelimit-remaining-requests"]; exists != enabled {
							t.Fatalf("metadata quota presence=%t want %t: %#v", exists, enabled, event)
						}
						sawTrace = nested["x-request-id"] == "trace-output"
					case "response.output_text.delta":
						sawText = event["delta"] == "quota response"
					}
					if event["type"] == "response.completed" {
						response, _ := event["response"].(map[string]any)
						usage, _ := response["usage"].(map[string]any)
						if response["id"] != "resp-quota-output" || usage["total_tokens"] != float64(2) {
							t.Fatalf("completion changed: %#v", event)
						}
						break
					}
				}
				if sawQuota != enabled || sawQuotaHeader != enabled || !sawState || !sawTrace || !sawText {
					t.Fatalf("quota=%t header=%t want %t state=%t trace=%t text=%t", sawQuota, sawQuotaHeader, enabled, sawState, sawTrace, sawText)
				}
				if err := downstream.Close(); err != nil {
					t.Fatal(err)
				}
				cfg, err := env.store.GetConfig(context.Background(), channelID)
				if err != nil {
					t.Fatal(err)
				}
				credential, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil || credential.PassiveUsage == nil || !quotaOutputHasUsedPercent(credential, 25) {
					t.Fatalf("internal quota sampling lost: credential=%#v err=%v", credential, err)
				}
			})
		}
	}
}

func TestOAuthQuotaOutputFinalHTTPErrorIntegration(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", enabled), func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Codex-Primary-Used-Percent", "100")
				w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
				w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "7260")
				w.Header().Set("X-Request-Id", "error-trace")
				w.Header().Set("X-Codex-Turn-State", "error-state")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7260,"resets_at":2000000000,"rate_limits":{"primary":{"used_percent":100}},"credits":{"remaining":0}},"rate_limits":{"primary":{"used_percent":100}},"credits":{"remaining":0}}`)
			}))
			defer upstream.Close()
			credential := strings.TrimSuffix(codexProxyTestCredential(t, "at-error", "rt-error", "account-error"), "}") + `,"quota_overdraft":{"enabled":true}}`
			env := setupProxyTestEnv(t, []testChannel{{name: "quota-output-error", upstreamProtocol: "codex", models: "gpt-test", authType: model.AuthTypeCodexOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL})
			channelID := setQuotaOutputTestChannel(t, env, enabled)
			before := time.Now()
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "gpt-test", "stream": false, "input": "hello"}, nil)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("X-Codex-Primary-Used-Percent"); (got != "") != enabled {
				t.Fatalf("quota header=%q passthrough=%t", got, enabled)
			}
			if response.Header().Get("X-Request-Id") != "error-trace" || !strings.HasSuffix(response.Header().Get("X-Codex-Turn-State"), "error-state") {
				t.Fatalf("routing/tracing headers lost: %v", response.Header())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			errorBody, _ := body["error"].(map[string]any)
			if errorBody["code"] != "usage_limit_reached" || errorBody["message"] != "The usage limit has been reached" {
				t.Fatalf("error changed: %#v", body)
			}
			for _, key := range []string{"resets_at", "resets_in_seconds", "plan_type"} {
				if _, exists := errorBody[key]; exists != enabled {
					t.Fatalf("error quota %s present=%t want %t", key, exists, enabled)
				}
			}
			for _, object := range []map[string]any{body, errorBody} {
				for _, key := range []string{"rate_limits", "credits"} {
					if _, exists := object[key]; exists != enabled {
						t.Fatalf("error quota %s present=%t want %t body=%#v", key, exists, enabled, body)
					}
				}
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			duration := cooldowns[channelID]["gpt-test"].Sub(before)
			if duration < 7250*time.Second || duration > 7270*time.Second {
				t.Fatalf("internal cooldown=%v want about 7260s", duration)
			}
			cfg, err := env.store.GetConfig(context.Background(), channelID)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil || persisted.PassiveUsage == nil || len(persisted.PassiveUsage.Windows) != 1 || persisted.PassiveUsage.Windows[0].UsedPercent != 100 {
				t.Fatalf("internal error quota sampling lost: credential=%#v err=%v", persisted, err)
			}
		})
	}
}

func quotaOutputHasUsedPercent(credential *codexauth.Credential, percent float64) bool {
	if credential == nil || credential.PassiveUsage == nil {
		return false
	}
	for _, window := range credential.PassiveUsage.Windows {
		if window.UsedPercent == percent {
			return true
		}
	}
	return false
}

func TestOAuthQuotaOutputGoogleHTTPErrorIntegration(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", enabled), func(t *testing.T) {
			const upstreamError = `{"error":{"code":429,"message":"Individual quota reached.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"model":"gemini-3.8-flash-high","quotaResetDelay":"7260s","quotaResetTime":"2030-01-01T00:00:00Z","uiMessage":"true"}}]}}`
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, upstreamError)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{name: "google-quota-output", upstreamProtocol: "gemini", models: "gemini-3.8-flash-high", authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-google-output")}}, map[int]string{0: upstream.URL})
			channelID := setQuotaOutputTestChannel(t, env, enabled)
			before := time.Now()
			response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3.8-flash-high:generateContent", map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}}, nil)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body struct {
				Error struct {
					Code    int
					Message string
					Status  string
					Details []struct {
						Type     string `json:"@type"`
						Reason   string
						Domain   string
						Metadata map[string]any
					}
				}
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != 429 || body.Error.Message != "Individual quota reached." || body.Error.Status != "RESOURCE_EXHAUSTED" || len(body.Error.Details) != 1 {
				t.Fatalf("Google error changed: %s", response.Body.String())
			}
			detail := body.Error.Details[0]
			if detail.Type != "type.googleapis.com/google.rpc.ErrorInfo" || detail.Reason != "QUOTA_EXHAUSTED" || detail.Domain != "cloudcode-pa.googleapis.com" || detail.Metadata["model"] != "gemini-3.8-flash-high" || detail.Metadata["uiMessage"] != "true" {
				t.Fatalf("Google error detail changed: %s", response.Body.String())
			}
			// Antigravity has no quota output switch and keeps its original error body.
			for _, key := range []string{"quotaResetDelay", "quotaResetTime"} {
				if _, exists := detail.Metadata[key]; !exists {
					t.Fatalf("Google quota %s dropped with stored passthrough=%t", key, enabled)
				}
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !cooldowns[channelID]["gemini-3.8-flash-high"].After(before.Add(7250 * time.Second)) {
				t.Fatalf("Google reset metadata no longer controls internal cooldown: %v", cooldowns)
			}
		})
	}
}
