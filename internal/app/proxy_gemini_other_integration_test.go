package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

/**
 * @brief Builds the same Gemini test route for either direct or wrapped Antigravity upstreams.
 * @param t Test context.
 * @param antigravity Whether the upstream uses the Antigravity OAuth wire.
 * @return A channel configured to serve the Claude compatibility tests.
 */
func geminiClaudeTestChannel(t *testing.T, antigravity bool) testChannel {
	t.Helper()
	channel := testChannel{
		name: "gemini-claude", upstreamProtocol: "gemini", models: "gemini-3-flash",
		priority: 100, apiKey: "gemini-test-key",
	}
	if antigravity {
		channel.authType = model.AuthTypeAntigravityOAuth
		channel.oauthCredential = antigravityProxyTestCredential(t, "gemini-test-access")
	}
	return channel
}

/**
 * @brief Checks the upstream context limit on the JSON, pre-commit SSE and committed SSE paths.
 * @param t Test context.
 */
func TestProxy_GoogleInputTokenLimitForClaude(t *testing.T) {
	t.Parallel()
	const inputLimit = "The input token count (1050377) exceeds the maximum number of tokens allowed (1048576)."
	const upstreamError = `{"error":{"code":400,"message":"` + inputLimit + `","status":"INVALID_ARGUMENT"}}`
	const textChunk = `{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3-flash"}`
	for _, provider := range []struct {
		name        string
		antigravity bool
	}{{"Gemini", false}, {"Antigravity", true}} {
		for _, scenario := range []struct {
			name      string
			stream    bool
			committed bool
		}{
			{name: "HTTP"},
			{name: "first SSE error", stream: true},
			{name: "committed SSE error", stream: true, committed: true},
		} {
			t.Run(provider.name+"/"+scenario.name, func(t *testing.T) {
				t.Parallel()
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if !scenario.stream {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, upstreamError)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario.committed {
						chunk := textChunk
						if provider.antigravity {
							chunk = `{"response":` + chunk + `}`
						}
						_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
					}
					_, _ = io.WriteString(w, "data: "+upstreamError+"\n\n")
				}))
				var fallbackCalls atomic.Int32
				fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					fallbackCalls.Add(1)
					w.WriteHeader(http.StatusBadGateway)
				}))
				primaryChannel := geminiClaudeTestChannel(t, provider.antigravity)
				fallbackChannel := geminiClaudeTestChannel(t, provider.antigravity)
				fallbackChannel.name, fallbackChannel.priority = "fallback", 10
				env := setupProxyTestEnv(t, []testChannel{primaryChannel, fallbackChannel},
					map[int]string{0: upstream.URL, 1: fallback.URL})
				adapter := antigravityProviderAdapterCases()[0]
				adapter.body["stream"] = scenario.stream
				response := doProxyRequest(t, env.engine, adapter.path, adapter.body, nil)

				wantStatus := http.StatusBadRequest
				payload := response.Body.Bytes()
				if scenario.committed {
					wantStatus = http.StatusOK
					var errorsSeen int
					for block := range strings.SplitSeq(response.Body.String(), "\n\n") {
						event, data := parseSSEEventChunk([]byte(block))
						if event == "error" {
							errorsSeen++
							payload = data
						}
					}
					if errorsSeen != 1 {
						t.Fatalf("got %d error events, want one: %s", errorsSeen, response.Body)
					}
				} else if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
					t.Fatalf("Content-Type = %q, want JSON", contentType)
				}
				if response.Code != wantStatus {
					t.Fatalf("status = %d, want %d: %s", response.Code, wantStatus, response.Body)
				}
				if got := gjson.GetBytes(payload, "type").String(); got != "error" {
					t.Fatalf("type = %q, want error: %s", got, payload)
				}
				if got := gjson.GetBytes(payload, "error.type").String(); got != "invalid_request_error" {
					t.Fatalf("error.type = %q, want invalid_request_error: %s", got, payload)
				}
				if got, want := gjson.GetBytes(payload, "error.message").String(), "prompt is too long: "+inputLimit; got != want {
					t.Fatalf("error.message = %q, want %q", got, want)
				}
				if calls := fallbackCalls.Load(); calls != 0 {
					t.Fatalf("fallback called %d times for a client input error", calls)
				}
				cooldowns, err := env.store.GetAllModelCooldowns(t.Context())
				if err != nil || len(cooldowns) != 0 {
					t.Fatalf("model cooldowns = %v, error = %v", cooldowns, err)
				}
			})
		}
	}
}

/**
 * @brief Checks native Gemini and Anthropic errors retain their respective wire bodies.
 * @param t Test context.
 */
func TestProxy_ContextLimitSameProtocolPassthrough(t *testing.T) {
	t.Parallel()
	adapters := antigravityProviderAdapterCases()
	for _, scenario := range []struct {
		name      string
		adapter   antigravityProviderAdapterCase
		errorBody string
	}{
		{"gemini", adapters[1], `{"error":{"code":400,"message":"The input token count exceeds the maximum","status":"INVALID_ARGUMENT"}}`},
		{"anthropic", adapters[0], `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 12 tokens > 10 maximum"},"request_id":"req_123"}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, scenario.errorBody)
			}))
			env := setupProxyTestEnv(t, []testChannel{{
				name: scenario.name, upstreamProtocol: scenario.name, models: "gemini-3-flash", apiKey: "test-key",
			}}, map[int]string{0: upstream.URL})
			response := doProxyRequest(t, env.engine, scenario.adapter.path, scenario.adapter.body, nil)
			if response.Code != http.StatusBadRequest || response.Body.String() != scenario.errorBody {
				t.Fatalf("status=%d body=%s, want HTTP 400 with the original error", response.Code, response.Body)
			}
		})
	}
}

/**
 * @brief Checks provider IDs and unique Claude fallbacks on both response modes.
 * @param t Test context.
 */
func TestProxy_GeminiClaudeMessageID(t *testing.T) {
	t.Parallel()
	const responseContent = `"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash"`
	for _, provider := range []struct {
		name        string
		antigravity bool
	}{{"Gemini", false}, {"Antigravity", true}} {
		for _, scenario := range []struct {
			name  string
			field string
			id    string
		}{
			{name: "missing"},
			{name: "blank", field: `"responseId":" ",`},
			{name: "nonstring", field: `"responseId":7,`},
			{name: "present", field: `"responseId":" req_from_upstream ",`, id: "req_from_upstream"},
		} {
			t.Run(provider.name+"/"+scenario.name, func(t *testing.T) {
				t.Parallel()
				payload := "{" + scenario.field + responseContent + "}"
				if provider.antigravity {
					payload = `{"response":` + payload + `}`
				}
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, ":streamGenerateContent") {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+payload+"\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, payload)
				}))
				env := setupProxyTestEnv(t, []testChannel{geminiClaudeTestChannel(t, provider.antigravity)}, map[int]string{0: upstream.URL})
				adapter := antigravityProviderAdapterCases()[0]
				ids := make(map[string]bool)
				for _, stream := range []bool{false, false, true, true} {
					adapter.body["stream"] = stream
					response := doProxyRequest(t, env.engine, adapter.path, adapter.body, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("stream = %v, status = %d: %s", stream, response.Code, response.Body)
					}
					id := gjson.GetBytes(response.Body.Bytes(), "id").String()
					if stream {
						for block := range strings.SplitSeq(response.Body.String(), "\n\n") {
							event, data := parseSSEEventChunk([]byte(block))
							if event == "message_start" {
								id = gjson.GetBytes(data, "message.id").String()
								break
							}
						}
					}
					if scenario.id != "" {
						if id != scenario.id {
							t.Fatalf("stream = %v, id = %q, want %q", stream, id, scenario.id)
						}
						continue
					}
					if !strings.HasPrefix(id, "msg_") || len(id) <= len("msg_") || ids[id] {
						t.Fatalf("stream = %v, expected a fresh Claude message ID, got %q", stream, id)
					}
					ids[id] = true
				}
			})
		}
	}
}

func assertGeminiFunctionCallStream(t *testing.T, body, name, query string) {
	t.Helper()
	for block := range strings.SplitSeq(body, "\n\n") {
		_, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		candidates, _ := payload["candidates"].([]any)
		if len(candidates) == 0 {
			continue
		}
		candidate, _ := candidates[0].(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			functionCall, _ := part["functionCall"].(map[string]any)
			if functionCall == nil {
				continue
			}
			args, _ := functionCall["args"].(map[string]any)
			if functionCall["name"] == name && args["query"] == query {
				return
			}
		}
	}
	t.Fatalf("expected Gemini functionCall name=%q query=%q, got %s", name, query, body)
}

func TestProxy_Success_Streaming_GeminiToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1beta/models/claude-3-5-sonnet:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"role":"user"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
	body := w.Body.String()
	assertGeminiFunctionCallStream(t, body, "lookup", "go")
	if !strings.Contains(body, `"finishReason":"STOP"`) || !strings.Contains(body, `"promptTokenCount":3`) || !strings.Contains(body, `"candidatesTokenCount":5`) {
		t.Fatalf("expected Gemini usage metadata, got %s", body)
	}
}

func TestProxy_Success_Streaming_GeminiToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-cdx"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/responses", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":{\"query\":\"go\"}}}\n\n" +
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1beta/models/gpt-5-codex:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"input"`)) || !bytes.Contains(gotBody, []byte(`"input_text"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected codex request body, got %s", gotBody)
	}
	body := w.Body.String()
	assertGeminiFunctionCallStream(t, body, "lookup", "go")
	if !strings.Contains(body, `"finishReason":"STOP"`) || !strings.Contains(body, `"promptTokenCount":3`) || !strings.Contains(body, `"candidatesTokenCount":5`) {
		t.Fatalf("expected Gemini completion payload, got %s", body)
	}
}
