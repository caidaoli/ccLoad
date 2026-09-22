package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"

	"github.com/tidwall/gjson"
)

func TestProxyOpenCodeResponsesNamespaceRoundTrip(t *testing.T) {
	for _, tc := range []struct{ streaming, retry, jsonResponse bool }{{}, {streaming: true}, {retry: true}, {streaming: true, retry: true}, {streaming: true, jsonResponse: true}} {
		t.Run(fmt.Sprintf("stream=%v/retry=%v/json=%v", tc.streaming, tc.retry, tc.jsonResponse), func(t *testing.T) {
			streaming := tc.streaming
			const modelName = "muse-spark-1.3-contributor"
			env := setupProxyTestEnv(t, []testChannel{{name: "muse", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: modelName, apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen/go"})
			tool := func(name string) map[string]any {
				return map[string]any{"type": "function", "name": name, "strict": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{"keyword": map[string]any{"type": "string"}}, "required": []string{"keyword"}, "additionalProperties": false}}
			}
			longNamespace := strings.Repeat("n", 70)
			request := map[string]any{
				"model": modelName, "stream": streaming,
				"reasoning": map[string]any{"effort": "high"},
				"tools": []any{
					map[string]any{"type": "namespace", "name": "papers", "tools": []any{tool("search")}},
					tool("papers__search"),
					map[string]any{"type": "namespace", "name": longNamespace, "tools": []any{tool("search")}},
					map[string]any{"type": "web_search"},
				},
				"tool_choice": map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "function", "namespace": "papers", "name": "search"}}},
				"input": []any{
					map[string]any{"role": "user", "content": "Find papers"},
					map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "namespace", "name": "dynamic", "tools": []any{tool("search")}}}},
					map[string]any{"type": "function_call", "namespace": "papers", "name": "search", "call_id": "previous", "status": "completed", "arguments": `{"keyword":"carbon"}`},
					map[string]any{"type": "function_call_output", "call_id": "previous", "output": "no data"},
				},
			}
			var wireName string
			attempts := 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				root := gjson.ParseBytes(raw)
				if tc.retry && attempts == 1 {
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"reasoning is unsupported","type":"invalid_request_error","param":"reasoning","code":"unsupported_parameter"}}`))}, nil
				}
				tools := root.Get("tools").Array()
				if len(tools) != 4 || tools[0].Get("type").String() != "function" || tools[3].Get("type").String() != "web_search" {
					t.Fatalf("wire tools: %s", raw)
				}
				wireName = tools[0].Get("name").String()
				seen := map[string]bool{}
				for _, tool := range tools[:3] {
					name := tool.Get("name").String()
					if name == "" || len(name) > 64 || seen[name] {
						t.Fatalf("colliding or overlong wire name %q", name)
					}
					seen[name] = true
					if !tool.Get("strict").Bool() || tool.Get("parameters.required.0").String() != "keyword" {
						t.Fatalf("schema changed: %s", tool.Raw)
					}
				}
				if tools[1].Get("name").String() != "papers__search" {
					t.Fatal("direct tool renamed")
				}
				if root.Get("input.1.tools.0.type").String() != "function" || root.Get("input.1.tools.0.name").String() != "dynamic__search" {
					t.Fatalf("dynamic declaration: %s", raw)
				}
				if root.Get("input.2.name").String() != wireName || root.Get("input.2.namespace").Exists() || root.Get("input.2.call_id").String() != "previous" || root.Get("input.3.call_id").String() != "previous" || root.Get("input.2.arguments").String() != `{"keyword":"carbon"}` {
					t.Fatalf("history corrupted: %s", raw)
				}
				if root.Get("tool_choice.tools.0.name").String() != wireName || root.Get("tool_choice.tools.0.namespace").Exists() {
					t.Fatalf("tool choice: %s", raw)
				}
				item := map[string]any{"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "status": "completed", "name": wireName, "arguments": `{"keyword":"electricity"}`}
				response := map[string]any{"id": "resp_probe", "object": "response", "status": "completed", "model": modelName, "output": []any{item}}
				payload, _ := json.Marshal(response)
				contentType := "application/json"
				if streaming && !tc.jsonResponse {
					contentType = "text/event-stream"
					var out bytes.Buffer
					for _, event := range []map[string]any{
						{"type": "response.output_item.added", "output_index": 0, "item": item},
						{"type": "response.function_call_arguments.done", "item_id": "fc_probe", "name": wireName, "arguments": item["arguments"]},
						{"type": "response.output_item.done", "output_index": 0, "item": item},
						{"type": "response.completed", "response": response},
					} {
						data, _ := json.Marshal(event)
						fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", event["type"], data)
					}
					payload = out.Bytes()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(payload))}, nil
			})}
			got := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
			if got.Code != 200 {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
			wantAttempts := 1
			if tc.retry {
				wantAttempts = 2
			}
			if attempts != wantAttempts {
				t.Fatalf("attempts=%d want=%d", attempts, wantAttempts)
			}
			assertItem := func(item gjson.Result) {
				if item.Get("name").String() != "search" || item.Get("namespace").String() != "papers" || item.Get("call_id").String() != "call_probe" || item.Get("arguments").String() != `{"keyword":"electricity"}` {
					t.Fatalf("client identity/arguments: %s", item.Raw)
				}
			}
			if !streaming || tc.jsonResponse {
				assertItem(gjson.GetBytes(got.Body.Bytes(), "output.0"))
				return
			}
			completed := false
			for _, frame := range bytes.Split(got.Body.Bytes(), []byte("\n\n")) {
				_, data := parseSSEEventChunk(frame)
				event := gjson.ParseBytes(data)
				switch event.Get("type").String() {
				case "response.output_item.added", "response.output_item.done":
					assertItem(event.Get("item"))
				case "response.function_call_arguments.done":
					if event.Get("name").String() != "search" || event.Get("namespace").String() != "papers" {
						t.Fatalf("argument event: %s", data)
					}
				case "response.completed":
					assertItem(event.Get("response.output.0"))
					completed = true
				}
			}
			if !completed {
				t.Fatal("missing response.completed")
			}
		})
	}
}

func TestProxyOpenCodeResponsesPreservesInvalidArgumentsWithDiagnostic(t *testing.T) {
	const modelName = "muse-spark-1.3-contributor"
	env := setupProxyTestEnv(t, []testChannel{{name: "muse", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: modelName, apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen/go"})
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		payload := "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_probe\",\"arguments\":\"{}\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_probe\",\"call_id\":\"call_probe\",\"name\":\"search\",\"arguments\":\"\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_probe\",\"name\":\"search\",\"arguments\":\"\",\"status\":\"incomplete\"}]}}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": modelName, "stream": true, "input": "search"}, nil)
	log.SetOutput(previous)
	if got.Code != 200 {
		t.Fatalf("status=%d: %s", got.Code, got.Body.String())
	}
	found := false
	for _, frame := range bytes.Split(got.Body.Bytes(), []byte("\n\n")) {
		_, data := parseSSEEventChunk(frame)
		event := gjson.ParseBytes(data)
		if event.Get("type").String() == "response.output_item.done" {
			found = true
			if event.Get("item.arguments").String() != "" {
				t.Fatalf("invented arguments: %s", data)
			}
		}
	}
	if !found || !strings.Contains(logs.String(), "inconsistent terminal function arguments") {
		t.Fatalf("missing raw output or diagnostic: body=%s logs=%s", got.Body.String(), logs.String())
	}
}

func TestProxyOpenCodeResponsesCompatibilityScope(t *testing.T) {
	for _, tc := range []struct{ name, target, model string }{
		{"other provider", "https://example.test/zen/go", "muse-spark-1.3-contributor"},
		{"other OpenCode product", "https://opencode.ai/zen/v1", "muse-spark-1.3-contributor"},
		{"other model", "https://opencode.ai/zen/go", "another-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode Go", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: tc.model, apiKey: "test-key"}}, map[int]string{0: tc.target})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if gjson.GetBytes(body, "tools.0.type").String() != "namespace" || gjson.GetBytes(body, "tools.0.name").String() != "papers" {
					t.Fatalf("out of scope namespace rewritten: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","status":"completed","output":[]}`))}, nil
			})}
			got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": tc.model, "input": "hi", "tools": []any{map[string]any{"type": "namespace", "name": "papers", "tools": []any{map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}}}}}}, nil)
			if got.Code != 200 {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestOpenCodeResponsesReplayAndIncrementalUseSameWireNames(t *testing.T) {
	const endpoint = "https://opencode.ai/zen/go"
	const modelName = "muse-spark-1.3-contributor"
	srv := newInMemoryServer(t)
	cfg := &model.Config{ID: 4, URLs: model.ChannelURLs{{URL: endpoint}}}
	incremental := []byte(`{"model":"muse-spark-1.3-contributor","tools":[{"type":"namespace","name":"papers","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}],"input":[{"role":"user","content":"continue"}]}`)
	replay := setJSONRaw(incremental, "input", `[{"type":"function_call","name":"papers__search","call_id":"old","arguments":"{}"},{"type":"function_call_output","call_id":"old","output":"done"},{"role":"user","content":"continue"}]`)
	reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), clientProtocol: protocol.Codex, upstreamProtocol: protocol.Codex,
		transformPlan: protocol.TransformPlan{ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses, OriginalBody: replay, TranslatedBody: replay, ActualModel: modelName}}
	var names []string
	for _, body := range [][]byte{replay, incremental} {
		req, err := srv.buildProxyRequest(reqCtx, cfg, "test-key", http.MethodPost, body, http.Header{}, "", "/v1/responses", endpoint)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(req.Body)
		if closeErr := req.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, gjson.GetBytes(raw, "tools.0.name").String())
	}
	if names[0] == "papers__search" || names[0] != names[1] {
		t.Fatalf("replay/incremental aliases diverge or collide with history: %v", names)
	}
}

func TestProxyToolArguments400DoesNotProbeOtherProtocols(t *testing.T) {
	const modelName = "muse-spark-1.3-contributor"
	env := setupProxyTestEnv(t, []testChannel{{name: "muse", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeAuto, models: modelName, apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen/go"})
	var attempts int
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"param":"arguments","type":"invalid_request_error","message":"arguments must be valid JSON"}}`))}, nil
	})}
	got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": modelName, "input": "hi"}, nil)
	if got.Code != 400 || attempts != 1 || gjson.GetBytes(got.Body.Bytes(), "error.param").String() != "arguments" {
		t.Fatalf("status=%d attempts=%d body=%s", got.Code, attempts, got.Body.String())
	}
	var logs []*model.LogEntry
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		var err error
		logs, err = env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(logs) == 0 {
		t.Fatal("missing proxy failure log")
	}
	for _, entry := range logs {
		if strings.Contains(entry.Message, "protocol capability fallback") {
			t.Fatalf("misleading diagnostic: %s", entry.Message)
		}
	}
}

func TestOpenCodeSessionHeaderUsesStableInputOrExecutionIdentity(t *testing.T) {
	dst := http.Header{}
	src := http.Header{"X-Session-Id": {"stable-session"}}
	ensureOpenCodeSessionHeader(dst, src, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "stable-session" {
		t.Fatalf("session header=%q, want stable-session", got)
	}

	dst = http.Header{}
	ensureOpenCodeSessionHeader(dst, nil, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "execution-fallback" {
		t.Fatalf("session header=%q, want execution fallback", got)
	}

	dst = http.Header{opencodeSessionHeader: {"explicit"}}
	ensureOpenCodeSessionHeader(dst, nil, "other-session")
	if got := dst.Get(opencodeSessionHeader); got != "explicit" {
		t.Fatalf("explicit session header=%q, want explicit", got)
	}
}

func TestOpenCodeSessionHeaderUsesDeterministicKnownHeaderFallback(t *testing.T) {
	src := http.Header{
		"X-Zeta-Session-Id":   {"zeta"},
		"X-Alpha-Session-Id":  {"alpha"},
		"X-Parent-Session-Id": {"parent"},
	}
	dst := http.Header{}
	ensureOpenCodeSessionHeader(dst, src, "execution-fallback")
	if got := dst.Get(opencodeSessionHeader); got != "alpha" {
		t.Fatalf("session header=%q, want alphabetically first session id", got)
	}
}

func TestOpenCodeChannelRecognizesOfficialGoEndpoint(t *testing.T) {
	tests := []struct {
		name string
		cfg  *model.Config
		want bool
	}{
		{
			name: "base endpoint",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}},
			},
			want: true,
		},
		{
			name: "responses endpoint",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go/v1/responses"}},
			},
			want: true,
		},
		{
			name: "exact URL marker",
			cfg: &model.Config{
				Name: "unrelated channel name",
				URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go#"}},
			},
			want: true,
		},
		{
			name: "name without endpoint",
			cfg:  &model.Config{Name: "OpenCode Go"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpenCodeChannel(tt.cfg); got != tt.want {
				t.Fatalf("isOpenCodeChannel()=%v, want %v for %+v", got, tt.want, tt.cfg)
			}
		})
	}
}

func TestOpenCodeChannelRejectsLookalikeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://opencode.ai.evil.example/zen/go",
		"https://proxy-opencode.ai/zen/go",
		"https://opencode.ai:8443/zen/go",
		"http://opencode.ai/zen/go",
		"https://opencode.ai/zen/gopher",
		"https://opencode.ai/zen/go-extra",
		"https://opencode.ai/other/zen/go",
		"https://opencode.ai/zen%2Fgo",
		"https://opencode.ai/zen/go/../other",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := &model.Config{
				Name: "OpenCode Go",
				URLs: model.ChannelURLs{{URL: endpoint}},
			}
			if isOpenCodeChannel(cfg) {
				t.Fatalf("lookalike endpoint was recognized: %q", endpoint)
			}
		})
	}
}

func TestOpenCodeSessionHeaderIsStableForRepeatedBuilders(t *testing.T) {
	src := http.Header{
		"X-Session-Id":       {"native-session"},
		"X-OpenCode-Session": {"explicit-session"},
	}
	first := http.Header{}
	second := http.Header{}
	ensureOpenCodeSessionHeader(first, src, "execution-session")
	ensureOpenCodeSessionHeader(second, src, "execution-session")
	if got := first.Get(opencodeSessionHeader); got != "explicit-session" {
		t.Fatalf("explicit source header=%q, want explicit-session", got)
	}
	if got := second.Get(opencodeSessionHeader); got != first.Get(opencodeSessionHeader) {
		t.Fatalf("same session was not stable: first=%q second=%q", first.Get(opencodeSessionHeader), got)
	}

	dst := http.Header{
		"X-OpenCode-Session": {"destination-explicit"},
		"X-Session-Id":       {"destination-native"},
	}
	ensureOpenCodeSessionHeader(dst, src, "other-session")
	if got := dst.Get(opencodeSessionHeader); got != "destination-explicit" {
		t.Fatalf("explicit destination header=%q, want destination-explicit", got)
	}
}

func TestOpenCodeSessionHeaderFallsBackToUUID(t *testing.T) {
	dst := http.Header{}
	ensureOpenCodeSessionHeader(dst, nil, "")
	got := dst.Get(opencodeSessionHeader)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got) {
		t.Fatalf("session header=%q is not a UUIDv4", got)
	}
}

func TestOpenCodeSessionHeaderIsUsedByProxyAndAdminBuilders(t *testing.T) {
	const endpoint = "https://opencode.ai/zen/go"
	cfg := &model.Config{
		ID:   4,
		Name: "OpenCode Go",
		URLs: model.ChannelURLs{{URL: endpoint}},
	}

	srv := newInMemoryServer(t)
	body := []byte(`{"model":"gpt-5.4","input":[]}`)
	proxyCtx := &requestContext{
		ctx:              context.Background(),
		startTime:        time.Now(),
		clientProtocol:   protocol.OpenAI,
		upstreamProtocol: protocol.Codex,
		transformPlan: protocol.TransformPlan{
			ClientProtocol:   protocol.OpenAI,
			UpstreamProtocol: protocol.Codex,
			RequestFamily:    protocol.RequestFamilyResponses,
			OriginalBody:     body,
			TranslatedBody:   body,
			OriginalModel:    "gpt-5.4",
			ActualModel:      "gpt-5.4",
		},
	}
	proxyReq, err := srv.buildProxyRequest(
		proxyCtx,
		cfg,
		"sk-test-key",
		http.MethodPost,
		body,
		http.Header{"X-Session-Id": {"proxy-session"}},
		"",
		"/v1/responses",
		endpoint,
	)
	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}
	if got := proxyReq.Header.Get(opencodeSessionHeader); got != "proxy-session" {
		t.Fatalf("proxy OpenCode session=%q, want proxy-session", got)
	}

	adminTestReq := &testutil.TestChannelRequest{
		Model:          "gpt-5.4",
		ClientProtocol: "openai",
		SessionID:      "admin-session",
	}
	adminReq, _, cancel, err := srv.buildTestUpstreamRequestForProtocol(
		context.Background(), cfg, "sk-test-key", adminTestReq,
		adminTestReq.Model, "openai", "codex", endpoint,
	)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		t.Fatalf("buildTestUpstreamRequestForProtocol failed: %v", err)
	}
	if got, want := adminReq.Header.Get(opencodeSessionHeader), adminTestReq.ResolveSessionID(); got != want {
		t.Fatalf("admin OpenCode session=%q, want resolved session %q", got, want)
	}
}
