package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"

	"github.com/tidwall/gjson"
)

func TestProxy_XAIOAuthStreamsThroughAllClientProtocols(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    map[string]any
		headers map[string]string
		text    func([]byte) string
	}{
		{
			name: "Codex", path: "/v1/responses",
			body: map[string]any{"model": "grok-4.5", "stream": true, "input": "hello"},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if ok && eventType == "response.output_text.delta" {
						text, _ := payload["delta"].(string)
						return text
					}
				}
				return ""
			},
		},
		{
			name: "OpenAI", path: "/v1/chat/completions",
			body: map[string]any{"model": "grok-4.5", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					choices, _ := payload["choices"].([]any)
					if len(choices) == 0 {
						continue
					}
					choice, _ := choices[0].(map[string]any)
					delta, _ := choice["delta"].(map[string]any)
					if text, _ := delta["content"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
		{
			name: "Anthropic", path: "/v1/messages",
			body:    map[string]any{"model": "grok-4.5", "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
			headers: map[string]string{"anthropic-version": "2023-06-01"},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok || eventType != "content_block_delta" {
						continue
					}
					delta, _ := payload["delta"].(map[string]any)
					if text, _ := delta["text"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
		{
			name: "Gemini", path: "/v1beta/models/grok-4.5:streamGenerateContent?alt=sse",
			body: map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
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
					if len(parts) == 0 {
						continue
					}
					part, _ := parts[0].(map[string]any)
					if text, _ := part["text"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
				}
				wireBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read xAI request: %v", err)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer stream-access" {
					t.Errorf("Authorization = %q", got)
				}
				var wire map[string]any
				if err := json.Unmarshal(wireBody, &wire); err != nil {
					t.Errorf("decode xAI request: %v", err)
				}
				if wire["model"] != "grok-4.5" || wire["stream"] != true {
					t.Errorf("xAI request = %#v", wire)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\n"+`data: {"type":"response.created","response":{"id":"resp-stream","model":"grok-4.5"}}`+"\n\n")
				_, _ = io.WriteString(w, "event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","delta":"hello-xai"}`+"\n\n")
				_, _ = io.WriteString(w, "event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp-stream","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream.Close()

			credential := mustXAICredentialJSON(t, &xaiauth.Credential{
				Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "stream-access", RefreshToken: "stream-refresh",
				Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			env := setupProxyTestEnv(t, []testChannel{{
				name: "xai-stream", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
				authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
			}}, map[int]string{0: upstream.URL + "/v1"})
			env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

			response := doProxyRequest(t, env.engine, tt.path, tt.body, tt.headers)
			if response.Code != http.StatusOK {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			if got := tt.text(response.Body.Bytes()); got != "hello-xai" {
				t.Fatalf("translated stream text=%q body=%s", got, response.Body.String())
			}
		})
	}
}

func TestProxy_CodexCompactionSummarizesAcrossProtocols(t *testing.T) {
	tests := []struct {
		protocol string
		model    string
		path     string
		summary  string
		response string
	}{
		{"openai", "gpt-test", "/v1/chat/completions", "summary from openai", "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"summary from openai\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4,\"total_tokens\":15}}"},
		{"anthropic", "claude-test", "/v1/messages", "summary from anthropic", "{\"id\":\"msg-summary\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"summary from anthropic\"}],\"model\":\"claude-test\",\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":11,\"output_tokens\":4}}"},
		{"gemini", "gemini-test", "/v1beta/models/gemini-test:generateContent", "summary from gemini", "{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"summary from gemini\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":11,\"candidatesTokenCount\":4,\"totalTokenCount\":15},\"modelVersion\":\"gemini-test\"}"},
	}

	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tt.protocol+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				var calls atomic.Int32
				var summaryRequest []byte
				var continuedRequest []byte
				transport := automaticFallbackToPath(tt.path, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(r.Body)
					if calls.Add(1) == 1 {
						summaryRequest = body
						if gjson.GetBytes(body, "stream").Bool() {
							t.Errorf("summary call must be non-streaming: %s", body)
						}
						var summaryRequestJSON map[string]any
						_ = json.Unmarshal(body, &summaryRequestJSON)
						summaryTools, _ := summaryRequestJSON["tools"].([]any)
						_, hasToolChoice := summaryRequestJSON["tool_choice"]
						if len(summaryTools) != 0 || hasToolChoice {
							t.Errorf("summary call retained tools/tool_choice: %s", body)
						}
						for _, want := range []string{"prior conversation", "lookup", "tool result"} {
							if !bytes.Contains(body, []byte(want)) {
								t.Errorf("summary call lost %q: %s", want, body)
							}
						}
					} else {
						continuedRequest = body
						if !bytes.Contains(body, []byte(tt.summary)) {
							t.Errorf("continued call did not restore summary: %s", body)
						}
						if bytes.Contains(body, []byte("ccload-compact-v1:")) {
							t.Errorf("local capsule leaked upstream: %s", body)
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tt.response))}, nil
				}))
				env := setupProxyTestEnv(t, []testChannel{{name: tt.protocol + "-compact", upstreamProtocol: tt.protocol, models: tt.model, apiKey: "test-upstream-key", priority: 100}}, map[int]string{0: "https://" + tt.protocol + ".example.test"})
				env.server.client = &http.Client{Transport: transport}
				response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model": tt.model, "stream": stream,
					"tools":       []any{map[string]any{"type": "function", "name": "unused", "parameters": map[string]any{"type": "object"}}},
					"tool_choice": "auto",
					"input": []any{
						map[string]any{"type": "message", "role": "user", "content": "prior conversation"},
						map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{\"q\":\"old\"}"},
						map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "tool result"},
						map[string]any{"type": "compaction_trigger"},
					},
				}, nil)
				if response.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("compaction status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
				}
				if len(summaryRequest) == 0 {
					t.Fatal("summary request did not reach the selected upstream")
				}
				capsule := ""
				usageBody := response.Body.Bytes()
				if stream {
					var eventTypes []string
					for _, block := range strings.Split(response.Body.String(), "\n\n") {
						eventType, data := parseSSEEventChunk([]byte(block))
						payload, ok := decodeSSEPayload(data)
						if !ok {
							continue
						}
						eventTypes = append(eventTypes, eventType)
						if eventType == "response.completed" {
							usageBody, _ = json.Marshal(payload)
						}
						if eventType == "response.output_item.done" {
							item, _ := payload["item"].(map[string]any)
							if item["type"] == "compaction" {
								capsule, _ = item["encrypted_content"].(string)
							}
						}
					}
					want := "response.created,response.in_progress,response.output_item.added,response.output_item.done,response.completed"
					if strings.Join(eventTypes, ",") != want {
						t.Fatalf("compaction stream events=%v body=%s", eventTypes, response.Body.String())
					}
				} else {
					if got := len(gjson.GetBytes(response.Body.Bytes(), "output").Array()); got != 1 ||
						gjson.GetBytes(response.Body.Bytes(), "output.0.type").String() != "compaction" {
						t.Fatalf("compaction JSON output=%s", response.Body.String())
					}
					capsule = gjson.GetBytes(response.Body.Bytes(), "output.0.encrypted_content").String()
				}
				if !strings.HasPrefix(capsule, "ccload-compact-v1:") {
					t.Fatalf("unexpected capsule %q: %s", capsule, response.Body.String())
				}
				usageRoot := "usage"
				if stream {
					usageRoot = "response.usage"
				}
				for field, want := range map[string]int64{"input_tokens": 11, "output_tokens": 4, "total_tokens": 15} {
					if got := gjson.GetBytes(usageBody, usageRoot+"."+field).Int(); got != want {
						t.Fatalf("compaction %s=%d, want %d body=%s", field, got, want, response.Body.String())
					}
				}
				continued := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model": tt.model,
					"input": []any{
						map[string]any{"type": "compaction", "encrypted_content": capsule},
						map[string]any{"type": "message", "role": "user", "content": "continue"},
					},
				}, nil)
				if continued.Code != http.StatusOK || calls.Load() != 2 || len(continuedRequest) == 0 {
					t.Fatalf("continued status=%d calls=%d body=%s", continued.Code, calls.Load(), continued.Body.String())
				}
			})
		}
	}
}

func TestProxy_CodexCompactEndpointAndFailedSummaries(t *testing.T) {
	t.Run("compact endpoint without trigger", func(t *testing.T) {
		var gotBody []byte
		var gotPath string
		env := setupProxyTestEnv(t, []testChannel{{name: "openai-compact", upstreamProtocol: "openai", models: "gpt-test", apiKey: "test-key", priority: 100}}, map[int]string{0: "https://openai.example.test"})
		env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1/chat/completions", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"endpoint summary\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}"))}, nil
		}))}
		response := doProxyRequest(t, env.engine, "/v1/responses/compact", map[string]any{
			"model": "gpt-test",
			"input": []any{map[string]any{"type": "message", "role": "user", "content": "compact endpoint history"}},
		}, nil)
		if response.Code != http.StatusOK || gotPath != "/v1/chat/completions" {
			t.Fatalf("compact endpoint status=%d upstream=%q body=%s", response.Code, gotPath, response.Body.String())
		}
		if gjson.GetBytes(gotBody, "stream").Bool() || gjson.GetBytes(gotBody, "tools").Exists() ||
			gjson.GetBytes(gotBody, "tool_choice").Exists() || !bytes.Contains(gotBody, []byte("compact endpoint history")) {
			t.Fatalf("unexpected compact endpoint summary request: %s", gotBody)
		}
		if got := gjson.GetBytes(response.Body.Bytes(), "output.0.type").String(); got != "compaction" {
			t.Fatalf("compact endpoint output type=%q body=%s", got, response.Body.String())
		}
	})

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty summary", http.StatusOK, "{\"choices\":[{\"message\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}"},
		{"length truncated", http.StatusOK, "{\"choices\":[{\"message\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}"},
		{"upstream bad request", http.StatusBadRequest, "{\"error\":{\"message\":\"bad summary request\"}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "openai-compact-failure", upstreamProtocol: "openai", models: "gpt-test", apiKey: "test-key", priority: 100}}, map[int]string{0: "https://openai.example.test"})
			env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1/chat/completions", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))}
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "gpt-test", "stream": true,
				"input": []any{map[string]any{"type": "message", "role": "user", "content": "history"}, map[string]any{"type": "compaction_trigger"}},
			}, nil)
			if bytes.Contains(response.Body.Bytes(), []byte("\"type\":\"response.completed\"")) {
				t.Fatalf("failed summary emitted response.completed: %s", response.Body.String())
			}
			if response.Code < http.StatusBadRequest {
				t.Fatalf("failed summary returned HTTP %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestProxy_CodexCompactionReplaysFullSessionAcrossRetry(t *testing.T) {
	t.Parallel()
	var requests [][]byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, body)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("OpenAI path=%q", r.URL.Path)
		}
		switch len(requests) {
		case 1:
			if !gjson.GetBytes(body, "stream").Bool() {
				t.Errorf("initial generation stream=false: %s", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-first\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-first\",\"choices\":[{\"delta\":{\"content\":\"first answer from upstream\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-first\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			if gjson.GetBytes(body, "stream").Bool() {
				t.Errorf("first summary request stream=true: %s", body)
			}
			if got := gjson.GetBytes(body, "max_tokens").Int(); got != 128 {
				t.Errorf("first summary max_tokens=%d, want 128: %s", got, body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "{\"error\":{\"message\":\"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.\",\"type\":\"invalid_request_error\",\"param\":\"max_tokens\",\"code\":\"unsupported_parameter\"}}")
		case 3:
			if gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "max_tokens").Exists() ||
				gjson.GetBytes(body, "max_completion_tokens").Int() != 128 {
				t.Errorf("max_tokens repair did not preserve nonstream summary request: %s", body)
			}
			for _, want := range []string{"first prompt", "first answer from upstream", "lookup", "historical tool result"} {
				if !bytes.Contains(body, []byte(want)) {
					t.Errorf("retried summary lost %q: %s", want, body)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"complete session summary\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":6,\"total_tokens\":23}}")
		default:
			t.Errorf("unexpected OpenAI call %d", len(requests))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "openai-session-compact-retry", upstreamProtocol: "openai",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-test", apiKey: "test-key",
	}}, map[int]string{0: upstream.URL})
	headers := map[string]string{"Session-Id": "compact-retry-session"}
	initial := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true,
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "first prompt"},
			map[string]any{"type": "function_call", "call_id": "call_first", "name": "lookup", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_first", "output": "historical tool result"},
		},
	}, headers)
	if initial.Code != http.StatusOK {
		t.Fatalf("initial generation status=%d body=%s", initial.Code, initial.Body.String())
	}
	var previousResponseID string
	for _, block := range strings.Split(initial.Body.String(), "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if ok && eventType == "response.completed" {
			response, _ := payload["response"].(map[string]any)
			previousResponseID, _ = response["id"].(string)
		}
	}
	if previousResponseID == "" {
		t.Fatalf("initial stream did not return a response id: %s", initial.Body.String())
	}
	compacted := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "previous_response_id": previousResponseID,
		"max_output_tokens": 128,
		"tools":             []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
		"tool_choice":       "required",
		"input":             []any{map[string]any{"type": "compaction_trigger"}},
	}, headers)
	if compacted.Code != http.StatusOK || len(requests) != 3 {
		t.Fatalf("compaction status=%d upstream calls=%d bodies=%q body=%s", compacted.Code, len(requests), requests, compacted.Body.String())
	}
	compactionItemDone := 0
	completedOutputCount := 0
	for _, block := range strings.Split(compacted.Body.String(), "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		if eventType == "response.output_item.done" {
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "compaction" {
				compactionItemDone++
			}
		}
		if eventType == "response.completed" {
			response, _ := payload["response"].(map[string]any)
			output, _ := response["output"].([]any)
			completedOutputCount = len(output)
		}
	}
	if compactionItemDone != 1 || completedOutputCount != 1 {
		t.Fatalf("compaction retry result items done=%d completed output=%d body=%s", compactionItemDone, completedOutputCount, compacted.Body.String())
	}
	if !bytes.Contains(requests[1], []byte("historical tool result")) ||
		!bytes.Contains(requests[2], []byte("historical tool result")) ||
		!bytes.Contains(compacted.Body.Bytes(), []byte("ccload-compact-v1:")) {
		t.Fatalf("compaction retry lost replay history or capsule: first=%s retry=%s response=%s", requests[1], requests[2], compacted.Body.String())
	}
}

func TestProxy_CodexCompactionHistoryReplaysToNativeCodex(t *testing.T) {
	t.Parallel()
	var openAIRequest []byte
	var codexRequest []byte
	openAIUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAIRequest, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || gjson.GetBytes(openAIRequest, "stream").Bool() {
			t.Errorf("OpenAI summary request path=%q body=%s", r.URL.Path, openAIRequest)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"cross-protocol compact summary\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}")
	}))
	codexUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexRequest, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("native Codex path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-native-next\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	}))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "switch-protocol-session", upstreamProtocol: "openai",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-test", apiKey: "test-key",
	}}, map[int]string{0: openAIUpstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list channel config: count=%d err=%v", len(configs), err)
	}
	sessionHeader := map[string]string{"Session-Id": "switch-after-compact"}
	compacted := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "old precompact history"},
			map[string]any{"type": "compaction_trigger"},
		},
	}, sessionHeader)
	if compacted.Code != http.StatusOK {
		t.Fatalf("OpenAI compaction status=%d body=%s", compacted.Code, compacted.Body.String())
	}
	var compactResponseID string
	for _, block := range strings.Split(compacted.Body.String(), "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if ok && eventType == "response.completed" {
			response, _ := payload["response"].(map[string]any)
			compactResponseID, _ = response["id"].(string)
		}
	}
	if compactResponseID == "" || openAIRequest == nil {
		t.Fatalf("missing compact response id or OpenAI summary request: id=%q request=%s body=%s", compactResponseID, openAIRequest, compacted.Body.String())
	}
	configs[0].URLs[0].Protocols = []string{"codex"}
	configs[0].URLs[0].URL = codexUpstream.URL
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("switch channel to native Codex: %v", err)
	}
	env.server.InvalidateChannelListCache()
	continued := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "previous_response_id": compactResponseID,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "new postcompact user"}},
	}, sessionHeader)
	if continued.Code != http.StatusOK {
		t.Fatalf("native continuation status=%d body=%s", continued.Code, continued.Body.String())
	}
	if codexRequest == nil {
		t.Fatalf("native Codex upstream was not called: response=%s", continued.Body.String())
	}
	input := gjson.GetBytes(codexRequest, "input").Array()
	if len(input) != 2 ||
		!bytes.Contains(codexRequest, []byte("cross-protocol compact summary")) ||
		!bytes.Contains(codexRequest, []byte("new postcompact user")) ||
		bytes.Contains(codexRequest, []byte("old precompact history")) ||
		bytes.Contains(codexRequest, []byte("compaction_trigger")) ||
		gjson.GetBytes(codexRequest, "previous_response_id").Exists() {
		t.Fatalf("native Codex did not receive expanded compact transcript and new user: %s", codexRequest)
	}
}

func TestProxy_AnthropicToXAIOAuthRecoversInvalidEncryptedStateInPlace(t *testing.T) {
	const grokEncryptedState = "HmlYdr2aCAqCYP/m9mr8PS6KOsdMs72FGDigmydR+Jsmuv8KX97yWPlbOwmXJgWn0CbHaCacdQD3+n5EvpgLfPNmafS3kdICBjRuDf4bzHy7uBiUhNVhqPtp/ee1y9q4imPE4LYgD1VZ4J+bp9mTeqA1+nC9Oue58CiNEMV9SVaGenCD+aBnVuSTzQhD32Y+68i6HLJW0Dx6ifaRfb8hxYtA/sPM+/FTvAMW11nRho5a2BBSkpnzfqqAz/e/vGJ77/bygpXM823QA9wL9i0X"
	tests := []struct {
		name        string
		firstBody   string
		withHistory bool
		wantRetries int32
	}{
		{
			name:        "flat compaction error",
			firstBody:   `{"code":"invalid-argument","error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`,
			withHistory: true,
			wantRetries: 2,
		},
		{
			name:        "opaque 400 with encrypted request",
			firstBody:   `{"error":"Upstream error: 400"}`,
			withHistory: true,
			wantRetries: 2,
		},
		{
			name:        "ordinary 400 without encrypted request",
			firstBody:   `{"error":"invalid tools"}`,
			withHistory: false,
			wantRetries: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			var firstExecutionID string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := attempts.Add(1)
				wireBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read xAI request: %v", err)
				}
				executionID := r.Header.Get("X-Grok-Conv-Id")
				if attempt == 1 {
					firstExecutionID = executionID
					if tt.withHistory && !bytes.Contains(wireBody, []byte(`"encrypted_content":"`+grokEncryptedState+`"`)) {
						t.Fatalf("first xAI request lost encrypted state: %s", wireBody)
					}
					for _, item := range gjson.GetBytes(wireBody, "input").Array() {
						content := item.Get("content")
						if item.Get("type").String() == "reasoning" && content.Exists() && content.Type == gjson.Null {
							t.Fatalf("first xAI request retained null reasoning content: %s", wireBody)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, tt.firstBody)
					return
				}
				if executionID == "" || executionID != firstExecutionID {
					t.Fatalf("retry changed xAI execution identity: first=%q retry=%q", firstExecutionID, executionID)
				}
				if bytes.Contains(wireBody, []byte(`"encrypted_content"`)) || bytes.Contains(wireBody, []byte(`"type":"reasoning"`)) {
					t.Fatalf("retry retained rejected encrypted state: %s", wireBody)
				}
				if !bytes.Contains(wireBody, []byte(`"type":"message"`)) {
					t.Fatalf("retry dropped ordinary message history: %s", wireBody)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\n")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-recovered","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream.Close()

			credential := mustXAICredentialJSON(t, &xaiauth.Credential{
				Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "recover-access", RefreshToken: "recover-refresh",
				Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			env := setupProxyTestEnv(t, []testChannel{{
				name: "xai-recover", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
				authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
			}}, map[int]string{0: xaiauth.CLIBaseURL})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				clone := req.Clone(req.Context())
				clone.URL.Scheme = "http"
				clone.URL.Host = upstream.host
				return dispatchTestHTTPRequest(clone)
			})}
			env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

			messages := []any{map[string]any{"role": "user", "content": "continue"}}
			if tt.withHistory {
				messages = append([]any{
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "prior state", "signature": grokEncryptedState},
					}},
				}, messages...)
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model": "grok-4.5", "stream": true, "max_tokens": 64, "messages": messages,
			}, map[string]string{
				"Anthropic-Version":        "2023-06-01",
				"X-Claude-Code-Session-Id": "claude-recovery-session",
			})

			if got := attempts.Load(); got != tt.wantRetries {
				t.Fatalf("upstream attempts=%d, want %d; response=%d %s", got, tt.wantRetries, response.Code, response.Body.String())
			}
			if tt.wantRetries == 2 && response.Code != http.StatusOK {
				t.Fatalf("recovery response=%d body=%s", response.Code, response.Body.String())
			}
			if tt.wantRetries == 1 && response.Code != http.StatusBadRequest {
				t.Fatalf("ordinary 400 response=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestProxy_AnthropicToXAIOAuthKeepsClaudeSessionCacheWireContract(t *testing.T) {
	type capturedRequest struct {
		executionID string
		body        []byte
	}
	var captured []capturedRequest
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read xAI request: %v", err)
		}
		captured = append(captured, capturedRequest{
			executionID: r.Header.Get("X-Grok-Conv-Id"),
			body:        bytes.Clone(wireBody),
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-cache","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "cache-access", RefreshToken: "cache-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-cache", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
		authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
	}}, map[int]string{0: xaiauth.CLIBaseURL})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = upstream.host
		return dispatchTestHTTPRequest(clone)
	})}
	env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

	body := map[string]any{
		"model": "grok-4.5", "stream": true, "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{
			"name": "lookup", "description": "lookup a value",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}
	for _, sessionID := range []string{"claude-session-a", "claude-session-a", "claude-session-b"} {
		response := doProxyRequest(t, env.engine, "/v1/messages", body, map[string]string{
			"Anthropic-Version":        "2023-06-01",
			"X-Claude-Code-Session-Id": sessionID,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("session %q response=%d body=%s", sessionID, response.Code, response.Body.String())
		}
	}

	if len(captured) != 3 {
		t.Fatalf("captured requests=%d, want 3", len(captured))
	}
	if captured[0].executionID == "" || captured[0].executionID != captured[1].executionID || captured[0].executionID == captured[2].executionID {
		t.Fatalf("xAI execution identity is not session-stable: %#v", captured)
	}
	for i, request := range captured {
		if got := gjson.GetBytes(request.body, "prompt_cache_key").String(); got != request.executionID {
			t.Fatalf("request %d cache/header identity mismatch: body=%q header=%q", i, got, request.executionID)
		}
		tools := gjson.GetBytes(request.body, "tools").Array()
		if len(tools) != 1 || tools[0].Get("type").String() != "function" || tools[0].Get("name").String() != "lookup" {
			t.Fatalf("request %d CLI chat-proxy tools=%s", i, gjson.GetBytes(request.body, "tools").Raw)
		}
		if gjson.GetBytes(request.body, "tool_choice").String() == "none" {
			t.Fatalf("request %d disabled client function tools: %s", i, request.body)
		}
	}
}

func TestProxy_StructuredGeminiResponseToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "gemini-ch",
		upstreamProtocol: "gemini",
		models:           "gemini-2.5-pro",
		apiKey:           "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`,
			))),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"tool_use"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected anthropic tool_use response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredGeminiResponseToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "gemini-ch",
		upstreamProtocol: "gemini",
		models:           "gemini-2.5-pro",
		apiKey:           "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"},{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"modelVersion":"gemini-2.5-pro"}`,
			))),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gemini-2.5-pro",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"function_call"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected codex function_call response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredAnthropicResponseToOpenAITransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "anthropic-ch",
		upstreamProtocol: "anthropic",
		models:           "claude-3-5-sonnet",
		apiKey:           "sk-ant",
	}}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`,
			))),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"tool_calls"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected OpenAI tool_calls response, got %s", w.Body.String())
	}
}

func TestProxy_StructuredAnthropicResponseToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "anthropic-ch",
		upstreamProtocol: "anthropic",
		models:           "claude-3-5-sonnet",
		apiKey:           "sk-ant",
	}}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"query":"go"}}],"model":"claude-3-5-sonnet","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`,
			))),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "claude-3-5-sonnet",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"function_call"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"name":"lookup"`)) {
		t.Fatalf("expected Codex function_call response, got %s", w.Body.String())
	}
}

func TestProxy_StreamingGeminiResponseToAnthropicTransform_MultipleToolCallsAcrossChunks(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "gemini-ch",
		upstreamProtocol: "gemini",
		models:           "gemini-2.5-pro",
		apiKey:           "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		body := bytes.NewBufferString(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"one\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"query\":\"two\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: [DONE]\n\n",
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(body),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	var toolBlocks []map[string]any
	var partialJSON []string
	var stopReason string
	messageStopped := false
	for _, block := range strings.Split(body, "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "content_block_start":
			contentBlock, _ := payload["content_block"].(map[string]any)
			if contentBlock != nil && contentBlock["type"] == "tool_use" {
				toolBlocks = append(toolBlocks, contentBlock)
			}
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta != nil && delta["type"] == "input_json_delta" {
				partial, _ := delta["partial_json"].(string)
				partialJSON = append(partialJSON, partial)
			}
		case "message_delta":
			delta, _ := payload["delta"].(map[string]any)
			stopReason, _ = delta["stop_reason"].(string)
		case "message_stop":
			messageStopped = true
		}
	}
	if len(toolBlocks) != 2 {
		t.Fatalf("expected two anthropic tool_use blocks, got %s", body)
	}
	firstID, _ := toolBlocks[0]["id"].(string)
	secondID, _ := toolBlocks[1]["id"].(string)
	if firstID == "" || secondID == "" || firstID == secondID || toolBlocks[0]["name"] != "lookup" || toolBlocks[1]["name"] != "search" {
		t.Fatalf("unexpected anthropic tool identities, got %s", body)
	}
	if len(partialJSON) != 2 || partialJSON[0] != `{"query":"one"}` || partialJSON[1] != `{"query":"two"}` {
		t.Fatalf("unexpected anthropic tool arguments, got %s", body)
	}
	if stopReason != "tool_use" || !messageStopped {
		t.Fatalf("expected anthropic terminal events, got %s", body)
	}
}

func TestProxy_StreamingGeminiResponseToCodexTransform_MultipleToolCallsAcrossChunks(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name:             "gemini-ch",
		upstreamProtocol: "gemini",
		models:           "gemini-2.5-pro",
		apiKey:           "sk-gem",
	}}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		body := bytes.NewBufferString(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"one\"}}}]}}],\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"search\",\"args\":{\"query\":\"two\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5,\"totalTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro\"}\n\n" +
				"data: [DONE]\n\n",
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(body),
		}, nil
	}))}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	type functionCall struct {
		outputIndex int
		item        map[string]any
	}
	var calls []functionCall
	var completed map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		eventType, data := parseSSEEventChunk([]byte(block))
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			outputIndex, _ := payload["output_index"].(float64)
			if item != nil && item["type"] == "function_call" {
				calls = append(calls, functionCall{outputIndex: int(outputIndex), item: item})
			}
		case "response.completed":
			completed = payload
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected two completed Codex function calls, got %s", body)
	}
	firstID, _ := calls[0].item["id"].(string)
	firstCallID, _ := calls[0].item["call_id"].(string)
	secondID, _ := calls[1].item["id"].(string)
	secondCallID, _ := calls[1].item["call_id"].(string)
	if calls[0].outputIndex != 0 || calls[1].outputIndex != 1 ||
		firstID == "" || firstCallID == "" || secondID == "" || secondCallID == "" ||
		firstID == secondID || firstCallID == secondCallID ||
		calls[0].item["name"] != "lookup" || calls[1].item["name"] != "search" {
		t.Fatalf("unexpected Codex function call identities, got %s", body)
	}
	for index, wantQuery := range []string{"one", "two"} {
		arguments, _ := calls[index].item["arguments"].(string)
		var got map[string]any
		if err := json.Unmarshal([]byte(arguments), &got); err != nil || got["query"] != wantQuery {
			t.Fatalf("unexpected Codex function call arguments at index %d: %q", index, arguments)
		}
	}
	response, _ := completed["response"].(map[string]any)
	usage, _ := response["usage"].(map[string]any)
	if completed == nil || usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(5) {
		t.Fatalf("expected Codex completion event with usage, got %s", body)
	}
}
