package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
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
	if got := proxyReq.Header.Get(opencodeSessionHeader); !regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`).MatchString(got) {
		t.Fatalf("proxy OpenCode session=%q is not descending encoded", got)
	}
	// Without a client session header, callers sharing one upstream key must not
	// share a session derived from that key.
	sessions := map[string]string{}
	for _, identity := range []string{"caller-a", "caller-b", "caller-a"} {
		callerCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), clientProtocol: protocol.OpenAI, upstreamProtocol: protocol.Codex, executionIdentity: identity,
			transformPlan: protocol.TransformPlan{ClientProtocol: protocol.OpenAI, UpstreamProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses, OriginalBody: body, TranslatedBody: body, OriginalModel: "gpt-5.4", ActualModel: "gpt-5.4"}}
		callerReq, err := srv.buildProxyRequest(callerCtx, cfg, "sk-test-key", http.MethodPost, body, nil, "", "/v1/responses", endpoint)
		if err != nil {
			t.Fatal(err)
		}
		got := callerReq.Header.Get(opencodeSessionHeader)
		if previous, seen := sessions[identity]; seen && previous != got {
			t.Fatalf("caller session changed: first=%q second=%q", previous, got)
		}
		sessions[identity] = got
	}
	if sessions["caller-a"] == sessions["caller-b"] {
		t.Fatalf("callers share the upstream key session: %q", sessions["caller-a"])
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
	if got := adminReq.Header.Get(opencodeSessionHeader); !regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`).MatchString(got) {
		t.Fatalf("admin OpenCode session=%q is not descending encoded", got)
	}
	adminAgain, _, cancelAgain, err := srv.buildTestUpstreamRequestForProtocol(
		context.Background(), cfg, "sk-test-key", adminTestReq,
		adminTestReq.Model, "openai", "codex", endpoint,
	)
	if cancelAgain != nil {
		defer cancelAgain()
	}
	if err != nil {
		t.Fatal(err)
	}
	if adminAgain.Header.Get(opencodeSessionHeader) != adminReq.Header.Get(opencodeSessionHeader) {
		t.Fatalf("admin session changed across requests: first=%q second=%q", adminReq.Header.Get(opencodeSessionHeader), adminAgain.Header.Get(opencodeSessionHeader))
	}
	for _, req := range []*http.Request{proxyReq, adminReq, adminAgain} {
		if req.Header.Get("Authorization") != "Bearer sk-test-key" || req.Header.Get("User-Agent") != "opencode/latest/2.0.29/cli" || req.Header.Get("X-Opencode-Request") != "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(req.Header.Get("X-Opencode-Project")) {
			t.Fatalf("unexpected builder headers: %v", req.Header)
		}
		for _, header := range []string{"X-Opencode-Session-Id", "X-Session-Affinity", "X-Session-Id"} {
			if req.Header.Get(header) != req.Header.Get(opencodeSessionHeader) {
				t.Fatalf("builder session aliases disagree: %v", req.Header)
			}
		}
	}
}

func TestOpenCodeRequestBodyResponsesBudget(t *testing.T) {
	for _, endpoint := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/go"} {
		for _, budget := range []string{"", "1", "15.5", "0", "-1", "16", "100", `"1"`} {
			t.Run(endpoint+"/budget="+budget, func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-test", apiKey: "test-key"}}, map[int]string{0: endpoint})
				request := map[string]any{"model": "gpt-test", "input": "hi", "stream": false, "provider_extension": json.Number("9007199254740993"), "tools": []any{map[string]any{"type": "function", "name": "custom", "parameters": map[string]any{"type": "object"}}}}
				if budget != "" {
					request["max_output_tokens"] = json.RawMessage(budget)
				}
				called := false
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					called = true
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					got := gjson.GetBytes(raw, "max_output_tokens")
					want := budget
					if budget == "1" || budget == "15.5" {
						want = "16"
					}
					if got.Raw != want || got.Exists() != (want != "") {
						t.Fatalf("wire budget=%s want=%s body=%s", got.Raw, want, raw)
					}
					if gjson.GetBytes(raw, "provider_extension").Raw != "9007199254740993" || gjson.GetBytes(raw, "stream").Bool() || len(gjson.GetBytes(raw, "tools").Array()) != 1 || gjson.GetBytes(raw, "tools.0.name").String() != "custom" {
						t.Fatalf("unrelated body fields changed: %s", raw)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response","status":"completed","output":[]}`))}, nil
				})}
				got := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
				if !called || got.Code != 200 {
					t.Fatalf("called=%v status=%d body=%s", called, got.Code, got.Body.String())
				}
			})
		}
	}
}

func TestOpenCodeRequestBodyChatReasoning(t *testing.T) {
	for _, tc := range []struct {
		name, model, setting string
		enabled              bool
	}{
		{"deepseek", "DeepSeek-chat", "", true},
		{"kimi", "kimi-test", "", true},
		{"moonshot", "moonshot-test", "", true},
		{"mimo", "xiaomimimo-test", "", true},
		{"effort", "other-model", `"reasoning_effort":"high"`, true},
		{"reasoning object", "other-model", `"reasoning":{"effort":"high"}`, true},
		{"thinking bool", "other-model", `"thinking":true`, true},
		{"top effort", "other-model", `"effort":"high"`, true},
		{"empty reasoning object", "other-model", `"reasoning":{}`, true},
		{"none", "other-model", `"reasoning_effort":" NONE "`, false},
		{"disabled object", "other-model", `"thinking":{"type":"disabled","effort":"high"}`, false},
		{"false", "other-model", `"thinking":false`, false},
		{"null", "other-model", `"reasoning":null`, false},
		{"native", "other-model", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeLocal, models: tc.model, apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen/go"})
			var request map[string]any
			raw := `{"model":"` + tc.model + `","stream":false,"provider_extension":{"opaque":true},"tools":[{"type":"function","function":{"name":"custom","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"custom","arguments":"{}"}}]},{"role":"assistant","reasoning":" original ","tool_calls":[{"id":"b","type":"function","function":{"name":"custom","arguments":"{}"}}]},{"role":"assistant","reasoning_content":" keep ","tool_calls":[{"id":"c","type":"function","function":{"name":"custom","arguments":"{}"}}]},{"role":"assistant","content":"plain"}]`
			if tc.setting != "" {
				raw += "," + tc.setting
			}
			raw += "}"
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				t.Fatal(err)
			}
			called := false
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				root := gjson.ParseBytes(body)
				if tc.enabled {
					if root.Get("messages.1.reasoning_content").String() != "tool call" || root.Get("messages.2.reasoning_content").String() != " original " {
						t.Fatalf("reasoning was not repaired: %s", body)
					}
				} else if root.Get("messages.1.reasoning_content").Exists() || root.Get("messages.2.reasoning_content").Exists() {
					t.Fatalf("reasoning repaired outside gate: %s", body)
				}
				if root.Get("messages.3.reasoning_content").String() != " keep " || root.Get("messages.4.reasoning_content").Exists() || root.Get("messages.0.reasoning_content").Exists() || !root.Get("provider_extension.opaque").Bool() || root.Get("stream").Bool() || len(root.Get("tools").Array()) != 1 || root.Get("tools.0.function.name").String() != "custom" {
					t.Fatalf("unrelated fields changed: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
			})}
			got := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if !called || got.Code != 200 {
				t.Fatalf("called=%v status=%d body=%s", called, got.Code, got.Body.String())
			}
		})
	}
}

func TestOpenCodeRequestBodyAnthropicThinking(t *testing.T) {
	for _, modelName := range []string{"deepseek-test", "kimi-test", "moonshot-test", "mimo-test", "claude-test"} {
		t.Run(modelName, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeLocal, models: modelName, apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
			var request map[string]any
			if err := json.Unmarshal([]byte(`{"model":"`+modelName+`","max_tokens":64,"stream":false,"thinking":{"type":"enabled","budget_tokens":16},"provider_extension":{"opaque":true},"context_management":{"edits":[]},"tools":[{"name":"custom","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"kept","signature":"signed","provider_extension":"kept"},{"type":"tool_use","id":"a","name":"custom","input":{}}]},{"role":"assistant","content":[{"type":"thinking","thinking":" "},{"type":"tool_use","id":"b","name":"custom","input":{}}]},{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"c","name":"custom","input":{}}]},{"role":"assistant","content":[{"type":"tool_use","id":"d","name":"custom","input":{}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"plain","signature":"untouched"},{"type":"text","text":"plain"}]}]}`), &request); err != nil {
				t.Fatal(err)
			}
			called := false
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				root := gjson.ParseBytes(body)
				if modelName == "claude-test" {
					// Claude models get ordinary API-key Messages normalization:
					// thinking without a valid Claude signature is dropped.
					if root.Get("messages.1.content.0.type").String() != "tool_use" || root.Get("messages.2.content.0.type").String() != "tool_use" ||
						root.Get("messages.3.content.0.data").String() != "opaque" || len(root.Get("messages.4.content").Array()) != 1 ||
						root.Get("messages.5.content.0.type").String() != "text" {
						t.Fatalf("Claude thinking was not sanitized: %s", body)
					}
				} else if root.Get("messages.5.content.0.signature").String() != "untouched" {
					t.Fatalf("vendor thinking outside tool turns changed: %s", body)
				} else if root.Get("messages.1.content.0.signature").Exists() || root.Get("messages.1.content.0.thinking").String() != "kept" || root.Get("messages.1.content.0.provider_extension").String() != "kept" || root.Get("messages.2.content.0.thinking").String() != "tool call" || root.Get("messages.3.content.0.thinking").String() != "[redacted thinking]" || root.Get("messages.3.content.0.data").Exists() || root.Get("messages.4.content.0.thinking").String() != "tool call" || root.Get("messages.4.content.1.id").String() != "d" {
					t.Fatalf("vendor thinking not repaired: %s", body)
				}
				if !root.Get("provider_extension.opaque").Bool() || root.Get("stream").Bool() || len(root.Get("tools").Array()) != 1 || root.Get("tools.0.name").String() != "custom" {
					t.Fatalf("unrelated fields changed: %s", body)
				}
				// The fixed OpenCode Anthropic-Beta header lacks context-management.
				if root.Get("context_management").Exists() {
					t.Fatalf("beta-gated field survived without its token: %s", body)
				}
				if r.URL.Query().Has("beta") {
					t.Fatalf("OpenCode URL includes Claude Code beta query: %s", r.URL)
				}
				if r.Header.Get("X-Api-Key") != "test-key" || r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14" {
					t.Fatalf("Anthropic auth headers=%v", r.Header)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg-test","type":"message","role":"assistant","model":"` + modelName + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
			})}
			got := doProxyRequest(t, env.engine, "/v1/messages", request, nil)
			if !called || got.Code != 200 {
				t.Fatalf("called=%v status=%d body=%s", called, got.Code, got.Body.String())
			}
		})
	}
}

func TestOpenCodeRequestBodyAndHeadersEndpointScope(t *testing.T) {
	for _, target := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/go", "https://example.test/v1", "https://opencode.ai/zen"} {
		t.Run(target, func(t *testing.T) {
			srv := newInMemoryServer(t)
			cfg := &model.Config{ID: 4, URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}, {URL: "https://example.test/v1"}}}
			body := []byte(`{"model":"gpt-test","input":"hi","max_output_tokens":1}`)
			reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), clientProtocol: protocol.Codex, upstreamProtocol: protocol.Codex,
				transformPlan: protocol.TransformPlan{ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses, OriginalBody: body, TranslatedBody: body, ActualModel: "gpt-test"}}
			source := http.Header{"X-Session-Id": {"stable-source"}, "User-Agent": {"foreign-client/1"}, "X-Opencode-Request": {"must-not-leak"}, "X-Opencode-Project": {"project-signal"}}
			var sessions []string
			for attempt := 0; attempt < 2; attempt++ {
				req, err := srv.buildProxyRequest(reqCtx, cfg, "test-key", http.MethodPost, body, source, "", "/v1/responses", target)
				if err != nil {
					t.Fatal(err)
				}
				wire, err := io.ReadAll(req.Body)
				if closeErr := req.Body.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(target, "https://opencode.ai/zen") {
					if gjson.GetBytes(wire, "max_output_tokens").Int() != 1 || strings.HasPrefix(req.Header.Get("User-Agent"), "opencode/") || req.Header.Get("X-Opencode-Session-Id") != "" || req.Header.Get("X-Opencode-Project") != "" {
						t.Fatalf("OpenCode wire leaked into other selected URL: headers=%v body=%s", req.Header, wire)
					}
					continue
				}
				if gjson.GetBytes(wire, "max_output_tokens").Int() != 16 || req.Header.Get("User-Agent") != "opencode/latest/2.0.29/cli" || req.Header.Get("X-Opencode-Client") != "cli" || req.Header.Get("Authorization") != "Bearer test-key" || req.Header.Get("X-Api-Key") != "" || req.Header.Get("X-Opencode-Request") != "" {
					t.Fatalf("unexpected OpenCode wire: headers=%v body=%s", req.Header, wire)
				}
				session := req.Header.Get("X-Opencode-Session")
				if !regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`).MatchString(session) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(req.Header.Get("X-Opencode-Project")) {
					t.Fatalf("invalid session/project headers: %v", req.Header)
				}
				for _, header := range []string{"X-Opencode-Session-Id", "X-Session-Affinity", "X-Session-Id"} {
					if req.Header.Get(header) != session {
						t.Fatalf("session aliases disagree: %v", req.Header)
					}
				}
				sessions = append(sessions, session)
			}
			if len(sessions) == 2 && sessions[0] != sessions[1] {
				t.Fatalf("session changed across requests: %v", sessions)
			}
		})
	}
}

func TestOpenCodeRequestBodyInferenceFamilyScope(t *testing.T) {
	for _, tc := range []struct {
		path     string
		upstream protocol.Protocol
	}{
		{"/v1/messages/count_tokens", protocol.Anthropic},
		{"/v1/embeddings", protocol.OpenAI},
		{"/v1/completions", protocol.OpenAI},
		{"/v1/systemone", protocol.SystemOne},
	} {
		t.Run(tc.path, func(t *testing.T) {
			srv := newInMemoryServer(t)
			cfg := &model.Config{ID: 4, URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}}}
			body := []byte(`{"model":"deepseek-test","input":"hi","max_output_tokens":1,"messages":[{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"custom","arguments":"{}"}}]}]}`)
			reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), clientProtocol: tc.upstream, upstreamProtocol: tc.upstream,
				transformPlan: protocol.TransformPlan{ClientProtocol: tc.upstream, UpstreamProtocol: tc.upstream, RequestFamily: protocol.DetectRequestFamily(tc.path), OriginalBody: body, TranslatedBody: body, ActualModel: "deepseek-test"}}
			req, err := srv.buildProxyRequest(reqCtx, cfg, "test-key", http.MethodPost, body, nil, "", tc.path, "https://opencode.ai/zen/go")
			if err != nil {
				t.Fatal(err)
			}
			wire, err := io.ReadAll(req.Body)
			if closeErr := req.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(wire, "max_output_tokens").Int() != 1 || gjson.GetBytes(wire, "messages.0.reasoning_content").Exists() {
				t.Fatalf("non-inference family rewritten: %s", wire)
			}
		})
	}
}

func TestOpenCodeRequestBodyFreeToolsAndStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, upstream, path, response string
	}{
		{"chat", "openai", "/v1/chat/completions", "data: {\"id\":\"chat-free\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-free\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"},
		{"responses", "codex", "/v1/responses", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-free\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg-free\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"},
		{"messages", "anthropic", "/v1/messages", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-free\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-free\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		for _, endpoint := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/go"} {
			t.Run(tc.name+"/"+endpoint, func(t *testing.T) {
				const modelName = "model-free"
				env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: tc.upstream, protocolTransformMode: model.ProtocolTransformModeLocal, models: modelName, apiKey: "test-key"}}, map[int]string{0: endpoint})
				tool := func(name string) any {
					params := map[string]any{"type": "object", "properties": map[string]any{"opaque": map[string]any{"type": "integer", "default": json.Number("9007199254740993")}}}
					definition := map[string]any{"name": name, "description": "client stub", "parameters": params}
					switch tc.upstream {
					case "openai":
						return map[string]any{"type": "function", "function": definition}
					case "anthropic":
						delete(definition, "parameters")
						definition["input_schema"] = params
					default:
						definition["type"] = "function"
					}
					return definition
				}
				tools := []any{tool("shell"), tool("webfetch"), tool("client_custom")}
				if tc.upstream == "codex" {
					tools = append(tools, map[string]any{"type": "web_search"}, map[string]any{"type": "namespace", "name": "custom_namespace", "tools": []any{tool("shell")}})
				}
				request := map[string]any{"model": modelName, "stream": false, "provider_extension": json.Number("9007199254740993"), "tools": tools}
				if tc.upstream == "codex" {
					request["input"] = "hi"
				} else {
					request["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
				}
				if tc.upstream == "anthropic" {
					request["max_tokens"] = 64
				}
				if tc.upstream == "openai" {
					request["stream_options"] = map[string]any{"include_usage": false, "provider_extension": true}
				}
				var wires [][]byte
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					wires = append(wires, body)
					root := gjson.ParseBytes(body)
					if !root.Get("stream").Bool() || root.Get("provider_extension").Raw != "9007199254740993" {
						t.Fatalf("free streaming/opaque data mismatch: %s", body)
					}
					if tc.upstream == "openai" && (!root.Get("stream_options.include_usage").Bool() || !root.Get("stream_options.provider_extension").Bool()) {
						t.Fatalf("Chat usage options not preserved: %s", body)
					}
					seen := map[string]int{}
					for _, entry := range root.Get("tools").Array() {
						if tc.upstream == "openai" {
							entry = entry.Get("function")
						}
						name := entry.Get("name").String()
						seen[name]++
						parameters := entry.Get("parameters")
						if tc.upstream == "anthropic" {
							parameters = entry.Get("input_schema")
						}
						switch name {
						case "shell", "webfetch", "client_custom":
							// The caller parses tool-call arguments with its own schema.
							if entry.Get("description").String() != "client stub" || parameters.Get("properties.opaque.default").Raw != "9007199254740993" {
								t.Fatalf("client tool %s changed: %s", name, entry.Raw)
							}
						case "read":
							if !strings.HasPrefix(entry.Get("description").String(), "Read the contents of a file or directory") || parameters.Get("required.0").String() != "path" {
								t.Fatalf("official read definition missing: %s", entry.Raw)
							}
						case "edit":
							if required := parameters.Get("required").Array(); len(required) != 3 || required[0].String() != "path" || required[1].String() != "oldString" || required[2].String() != "newString" {
								t.Fatalf("official edit definition missing: %s", entry.Raw)
							}
						}
					}
					for _, name := range []string{"shell", "edit", "glob", "grep", "read", "webfetch", "client_custom"} {
						if seen[name] != 1 {
							t.Fatalf("tool %s count=%d, body=%s", name, seen[name], body)
						}
					}
					if tc.upstream == "codex" && (root.Get("tools.3.type").String() != "web_search" || root.Get("tools.4.tools.0.description").String() != "client stub") {
						t.Fatalf("opaque Responses tool changed: %s", body)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
				})}
				got := doProxyRequest(t, env.engine, tc.path, request, nil)
				if len(wires) != 1 || got.Code != 200 || !json.Valid(got.Body.Bytes()) || !strings.Contains(got.Body.String(), `"ok"`) {
					t.Fatalf("free SSE was not collapsed: attempts=%d status=%d body=%s", len(wires), got.Code, got.Body.String())
				}
				// Replaying a complete wire body through the proxy must not append
				// duplicate tools or change any request values on another pass.
				var replay map[string]any
				decoder := json.NewDecoder(bytes.NewReader(wires[0]))
				decoder.UseNumber()
				if err := decoder.Decode(&replay); err != nil {
					t.Fatal(err)
				}
				got = doProxyRequest(t, env.engine, tc.path, replay, nil)
				if len(wires) != 2 || got.Code != 200 {
					t.Fatalf("free replay failed: attempts=%d status=%d body=%s", len(wires), got.Code, got.Body.String())
				}
				var replayed map[string]any
				decoder = json.NewDecoder(bytes.NewReader(wires[1]))
				decoder.UseNumber()
				if err := decoder.Decode(&replayed); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(replay, replayed) {
					t.Fatalf("free wire shaping is not idempotent: first=%s second=%s", wires[0], wires[1])
				}
			})
		}
	}
}

func TestOpenCodeRequestBodyFreeRejectsMalformedTools(t *testing.T) {
	for _, tools := range []any{map[string]any{"name": "broken"}, []any{"broken"}, []any{nil}} {
		t.Run(fmt.Sprintf("%v", tools), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
			called := false
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				return nil, fmt.Errorf("malformed tools reached upstream")
			})}
			got := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "model-free", "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if called || got.Code != http.StatusBadRequest {
				t.Fatalf("malformed tools accepted: called=%v status=%d body=%s", called, got.Code, got.Body.String())
			}
		})
	}
}

func TestOpenCodeRequestBodyFreeWithoutTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream protocol.Protocol
		tools    string
	}{
		{"openai", protocol.OpenAI, ""}, {"codex", protocol.Codex, ""}, {"anthropic", protocol.Anthropic, ""},
		{"openai/null", protocol.OpenAI, `"tools":null,`}, {"anthropic/null", protocol.Anthropic, `"tools":null,`},
	} {
		upstream := tc.upstream
		t.Run(tc.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			cfg := &model.Config{ID: 4, URLs: model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}}}
			path := "/v1/chat/completions"
			switch upstream {
			case protocol.Codex:
				path = "/v1/responses"
			case protocol.Anthropic:
				path = "/v1/messages"
			}
			body := []byte(`{"model":"big-pickle",` + tc.tools + `"messages":[{"role":"user","content":"hi"}],"input":"hi","max_tokens":64,"stream":false}`)
			reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), clientProtocol: upstream, upstreamProtocol: upstream,
				transformPlan: protocol.TransformPlan{ClientProtocol: upstream, UpstreamProtocol: upstream, RequestFamily: protocol.DetectRequestFamily(path), OriginalBody: body, TranslatedBody: body, ActualModel: "big-pickle"}}
			req, err := srv.buildProxyRequest(reqCtx, cfg, "test-key", http.MethodPost, body, nil, "", path, "https://opencode.ai/zen/go")
			if err != nil {
				t.Fatal(err)
			}
			wire, err := io.ReadAll(req.Body)
			if closeErr := req.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !gjson.GetBytes(wire, "stream").Bool() || len(gjson.GetBytes(wire, "tools").Array()) != 5 {
				t.Fatalf("missing agent tools/stream: %s", wire)
			}
			for index, name := range []string{"shell", "edit", "glob", "grep", "read"} {
				tool := gjson.GetBytes(wire, fmt.Sprintf("tools.%d", index))
				if upstream == protocol.OpenAI {
					tool = tool.Get("function")
				}
				if tool.Get("name").String() != name || len(tool.Get("description").String()) < 30 {
					t.Fatalf("missing complete core tool %s: %s", name, tool.Raw)
				}
			}
		})
	}
}

// Each prefix contains a complete, semantic first event but no terminal event.
// The suffix completes one tool call and reports eleven input/seven output tokens.
func openCodeFreeToolStreamFixture(upstream protocol.Protocol) (prefix, suffix string) {
	switch upstream {
	case protocol.OpenAI:
		return "data: " + `{"id":"chat-free","model":"model-free","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n",
			"data: " + `{"id":"chat-free","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"test\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: " + `{"id":"chat-free","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\ndata: [DONE]\n\n"
	case protocol.Codex:
		return "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n",
			"event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":"{\"q\":\"test\"}"}` + "\n\nevent: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"q\":\"test\"}","status":"completed"}}` + "\n\nevent: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp-free","object":"response","model":"model-free","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}` + "\n\n"
	case protocol.Anthropic:
		return "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-free","type":"message","role":"assistant","model":"model-free","content":[],"usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\nevent: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_a","name":"lookup","input":{}}}` + "\n\n",
			"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\nevent: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"test\"}"}}` + "\n\nevent: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\nevent: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}` + "\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	case protocol.Gemini:
		return "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"Let me "}]},"index":0}],"modelVersion":"model-free"}` + "\n\n",
			"data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"check."}]},"index":0}],"modelVersion":"model-free"}` + "\n\ndata: " + `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"lookup","args":{"q":"test"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"totalTokenCount":18},"modelVersion":"model-free"}` + "\n\n"
	default:
		panic("no OpenCode free stream fixture for " + string(upstream))
	}
}

func TestOpenCodeFreeNonStreamToolsAndUsageAcrossProtocols(t *testing.T) {
	for _, tc := range []struct {
		client, upstream protocol.Protocol
	}{
		{protocol.OpenAI, protocol.OpenAI}, {protocol.Codex, protocol.Codex}, {protocol.Anthropic, protocol.Anthropic},
		{protocol.OpenAI, protocol.Codex}, {protocol.Codex, protocol.OpenAI},
		{protocol.OpenAI, protocol.Anthropic}, {protocol.Anthropic, protocol.OpenAI},
		{protocol.Gemini, protocol.Gemini}, {protocol.OpenAI, protocol.Gemini}, {protocol.Codex, protocol.Gemini}, {protocol.Anthropic, protocol.Gemini},
	} {
		t.Run(string(tc.client)+"->"+string(tc.upstream), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: string(tc.upstream), protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
			prefix, suffix := openCodeFreeToolStreamFixture(tc.upstream)
			called := false
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if tc.upstream == protocol.Gemini {
					// The free gate accepts only the streaming action of the unversioned route.
					if r.URL.Path != "/zen/v1/models/model-free:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "test-key" || len(gjson.GetBytes(body, "tools.0.functionDeclarations").Array()) != 5 {
						t.Fatalf("free Gemini request was not shaped: url=%s headers=%v body=%s", r.URL, r.Header, body)
					}
				} else if !gjson.GetBytes(body, "stream").Bool() {
					t.Fatalf("free request was not streamed upstream: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + suffix))}, nil
			})}
			path := "/v1/chat/completions"
			request := map[string]any{"model": "model-free", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			switch tc.client {
			case protocol.Codex:
				path = "/v1/responses"
				delete(request, "messages")
				request["input"] = "hi"
			case protocol.Anthropic:
				path = "/v1/messages"
				request["max_tokens"] = 64
			case protocol.Gemini:
				path = "/v1beta/models/model-free:generateContent"
				request = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
			}
			got := doProxyRequest(t, env.engine, path, request, nil)
			if !called || got.Code != 200 || !json.Valid(got.Body.Bytes()) || !strings.Contains(got.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("nonstream completion failed: called=%v status=%d body=%s", called, got.Code, got.Body.String())
			}
			root := gjson.ParseBytes(got.Body.Bytes())
			// Gemini to Claude/Responses translators mint their own tool ids.
			callID := func(id string) bool { return id == "call_a" || (tc.upstream == protocol.Gemini && id != "") }
			switch tc.client {
			case protocol.OpenAI:
				calls := root.Get("choices.0.message.tool_calls").Array()
				if len(calls) != 1 || calls[0].Get("id").String() != "call_a" || calls[0].Get("function.name").String() != "lookup" || gjson.Get(calls[0].Get("function.arguments").String(), "q").String() != "test" || root.Get("usage.prompt_tokens").Int() != 11 || root.Get("usage.completion_tokens").Int() != 7 {
					t.Fatalf("Chat tool/usage lost: %s", got.Body.String())
				}
			case protocol.Codex:
				var calls []gjson.Result
				for _, item := range root.Get("output").Array() {
					if item.Get("type").String() == "function_call" {
						calls = append(calls, item)
					}
				}
				if len(calls) != 1 || !callID(calls[0].Get("call_id").String()) || calls[0].Get("name").String() != "lookup" || gjson.Get(calls[0].Get("arguments").String(), "q").String() != "test" || root.Get("usage.input_tokens").Int() != 11 || root.Get("usage.output_tokens").Int() != 7 {
					t.Fatalf("Responses tool/usage lost: %s", got.Body.String())
				}
			case protocol.Anthropic:
				var calls []gjson.Result
				for _, block := range root.Get("content").Array() {
					if block.Get("type").String() == "tool_use" {
						calls = append(calls, block)
					}
				}
				if len(calls) != 1 || !callID(calls[0].Get("id").String()) || calls[0].Get("name").String() != "lookup" || calls[0].Get("input.q").String() != "test" || root.Get("usage.input_tokens").Int() != 11 || root.Get("usage.output_tokens").Int() != 7 {
					t.Fatalf("Anthropic tool/usage lost: %s", got.Body.String())
				}
			case protocol.Gemini:
				parts := root.Get("candidates.0.content.parts").Array()
				if len(parts) != 2 || parts[0].Get("text").String() != "Let me check." || parts[1].Get("functionCall.id").String() != "call_a" || parts[1].Get("functionCall.name").String() != "lookup" || parts[1].Get("functionCall.args.q").String() != "test" || root.Get("candidates.0.finishReason").String() != "STOP" || root.Get("usageMetadata.promptTokenCount").Int() != 11 || root.Get("usageMetadata.candidatesTokenCount").Int() != 7 {
					t.Fatalf("Gemini parts/usage lost: %s", got.Body.String())
				}
			}
		})
	}
}

func TestOpenCodeFreeNonStreamRejectsFailedOrTruncatedSSE(t *testing.T) {
	for _, upstream := range []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini} {
		for _, failure := range []string{"truncated", "rate-limit"} {
			t.Run(string(upstream)+"/"+failure, func(t *testing.T) {
				env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "OpenCode", upstreamProtocol: string(upstream), protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"}, map[string]string{"cooldown_fallback_enabled": "false"})
				prefix, _ := openCodeFreeToolStreamFixture(upstream)
				wire := prefix
				if failure == "rate-limit" {
					wire += "event: error\ndata: " + `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"model quota exceeded"}}` + "\n\n"
				}
				called := false
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					called = true
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
				})}
				response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "model-free", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
				if !called || response.Code == 200 {
					t.Fatalf("failed stream reported success: called=%v status=%d body=%s", called, response.Code, response.Body.String())
				}
				entry := waitForProxyLog(t, env, "model-free")
				wantStatus := util.StatusStreamIncomplete
				if failure == "rate-limit" {
					wantStatus = http.StatusTooManyRequests
				}
				if entry.StatusCode != wantStatus {
					t.Fatalf("stream failure classification=%d want=%d log=%+v", entry.StatusCode, wantStatus, entry)
				}
			})
		}
	}
}

func TestOpenCodeFreeNonStreamStopsFirstByteDeadlineAfterEvent(t *testing.T) {
	for _, upstream := range []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini} {
		t.Run(string(upstream), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: string(upstream), protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
			const firstByteTimeout = 100 * time.Millisecond
			const completionDelay = 3 * firstByteTimeout
			env.server.firstByteTimeout = firstByteTimeout
			prefix, suffix := openCodeFreeToolStreamFixture(upstream)
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				reader, writer := io.Pipe()
				go func() {
					defer func() { _ = writer.Close() }()
					if _, err := io.WriteString(writer, prefix); err != nil {
						return
					}
					timer := time.NewTimer(completionDelay)
					defer timer.Stop()
					select {
					case <-r.Context().Done():
						return
					case <-timer.C:
						_, _ = io.WriteString(writer, suffix)
					}
				}()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
			})}
			started := time.Now()
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "model-free", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if response.Code != 200 || time.Since(started) < completionDelay || gjson.GetBytes(response.Body.Bytes(), "choices.0.message.tool_calls.0.function.name").String() != "lookup" {
				t.Fatalf("first-byte deadline canceled a progressing completion: elapsed=%v status=%d body=%s", time.Since(started), response.Code, response.Body.String())
			}
			entry := waitForProxyLog(t, env, "model-free")
			if entry.StatusCode != 200 || entry.Duration < completionDelay.Seconds() {
				t.Fatalf("successful long generation not preserved: %+v", entry)
			}
		})
	}
}

func TestOpenCodeFreeResponsesPreservesOpaqueToolsWithCoreNames(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
	called := false
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		tools := gjson.GetBytes(body, "tools").Array()
		if len(tools) != 7 || tools[0].Get("type").String() != "namespace" || tools[0].Get("name").String() != "shell" || tools[0].Get("tools.0.name").String() != "client_tool" || tools[1].Get("type").String() != "custom" || tools[1].Get("name").String() != "read" || tools[1].Get("format.type").String() != "text" || tools[1].Get("description").String() != "client custom tool" {
			t.Fatalf("opaque tool was replaced by a core definition: %s", body)
		}
		seen := map[string]bool{}
		for _, tool := range tools[2:] {
			if tool.Get("type").String() != "function" {
				t.Fatalf("core tool not appended as function: %s", tool.Raw)
			}
			seen[tool.Get("name").String()] = true
		}
		if !seen["shell"] || !seen["read"] {
			t.Fatalf("opaque names suppressed mandatory core functions: %s", body)
		}
		prefix, suffix := openCodeFreeToolStreamFixture(protocol.Codex)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + suffix))}, nil
	})}
	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "model-free", "input": "hi", "stream": false, "tools": []any{
		map[string]any{"type": "namespace", "name": "shell", "tools": []any{map[string]any{"type": "function", "name": "client_tool", "parameters": map[string]any{"type": "object"}}}},
		map[string]any{"type": "custom", "name": "read", "description": "client custom tool", "format": map[string]any{"type": "text"}},
	}}, nil)
	if !called || response.Code != 200 {
		t.Fatalf("opaque-tool request failed: called=%v status=%d body=%s", called, response.Code, response.Body.String())
	}
}

const openCodeUnsupportedProtocolResponse = `{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`

func assertOpenCodeNoPersistedCooldown(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	keys, err := srv.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	models, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || len(models) != 0 || len(channels) != 0 {
		t.Fatalf("protocol negotiation applied cooldown: keys=%v models=%v channels=%v", keys, models, channels)
	}
}

func TestProxyOpenCodeAutoNegotiatesProtocolPerModel(t *testing.T) {
	for _, endpoint := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/go"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, streaming), func(t *testing.T) {
				const muse = "muse-spark-1.3-contributor-free"
				env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", protocolTransformMode: model.ProtocolTransformModeAuto, apiKey: "personal-key", modelEntries: []model.ModelEntry{
					{Model: "muse-alias", RedirectModel: muse}, {Model: "big-pickle"}, {Model: "qwen3.6-plus-free"},
				}}}, map[int]string{0: endpoint})
				nativePaths := map[string]string{muse: "/v1/responses", "big-pickle": "/v1/chat/completions", "qwen3.6-plus-free": "/v1/messages"}
				var paths []string
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					actual := gjson.GetBytes(body, "model").String()
					path := strings.TrimPrefix(r.URL.Path, strings.TrimPrefix(endpoint, "https://opencode.ai"))
					paths = append(paths, path)
					if nativePaths[actual] != path {
						return jsonResponseStatus(r, http.StatusBadRequest, openCodeUnsupportedProtocolResponse)
					}
					if !gjson.GetBytes(body, "stream").Bool() {
						t.Fatalf("free request must stream upstream: %s", body)
					}
					upstream := protocol.OpenAI
					switch path {
					case "/v1/responses":
						upstream = protocol.Codex
						if !gjson.GetBytes(body, "input").IsArray() || gjson.GetBytes(body, "messages").Exists() {
							t.Fatalf("Responses request was not translated: %s", body)
						}
					case "/v1/messages":
						upstream = protocol.Anthropic
						if !gjson.GetBytes(body, "messages").IsArray() || gjson.GetBytes(body, "max_tokens").Int() <= 0 {
							t.Fatalf("Anthropic request was not translated: %s", body)
						}
					}
					prefix, suffix := openCodeFreeToolStreamFixture(upstream)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + suffix)), Request: r}, nil
				})}
				for requestIndex, name := range []string{"muse-alias", "big-pickle", "qwen3.6-plus-free", "muse-alias", "big-pickle", "qwen3.6-plus-free"} {
					before := len(paths)
					got := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
						"model": name, "stream": streaming, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}, nil)
					if got.Code != http.StatusOK {
						t.Fatalf("model=%s status=%d paths=%v body=%s", name, got.Code, paths, got.Body.String())
					}
					if requestIndex >= 3 && len(paths)-before != 1 {
						t.Fatalf("model=%s repeated protocol probes: %v", name, paths[before:])
					}
					if streaming {
						tool, usage, done := false, false, false
						for _, line := range strings.Split(got.Body.String(), "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
							if data == "[DONE]" {
								done = true
								continue
							}
							if !json.Valid([]byte(data)) {
								t.Fatalf("invalid client SSE data: %s", line)
							}
							root := gjson.Parse(data)
							tool = tool || root.Get("choices.0.delta.tool_calls.0.function.name").String() == "lookup"
							usage = usage || root.Get("usage.prompt_tokens").Int() == 11 && root.Get("usage.completion_tokens").Int() == 7
						}
						if !tool || !usage || !done {
							t.Fatalf("model=%s lost client Chat SSE contract: %s", name, got.Body.String())
						}
					} else if root := gjson.ParseBytes(got.Body.Bytes()); !json.Valid(got.Body.Bytes()) || root.Get("choices.0.message.tool_calls.0.function.name").String() != "lookup" || root.Get("usage.prompt_tokens").Int() != 11 || root.Get("usage.completion_tokens").Int() != 7 {
						t.Fatalf("model=%s lost client Chat completion: %s", name, got.Body.String())
					}
				}
				assertOpenCodeNoPersistedCooldown(t, env.server)
			})
		}
	}
}

func TestHandleChannelTestOpenCodeAutoNegotiatesModelProtocol(t *testing.T) {
	for _, endpoint := range []string{"https://opencode.ai/zen", "https://opencode.ai/zen/go"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, streaming), func(t *testing.T) {
				const actualModel = "muse-spark-1.3-contributor-free"
				env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", protocolTransformMode: model.ProtocolTransformModeAuto, modelEntries: []model.ModelEntry{{Model: "alias", RedirectModel: actualModel}}}}, map[int]string{0: endpoint})
				var paths []string
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					paths = append(paths, r.URL.Path)
					if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
						return jsonResponseStatus(r, http.StatusBadRequest, openCodeUnsupportedProtocolResponse)
					}
					if gjson.GetBytes(body, "model").String() != actualModel || !gjson.GetBytes(body, "input").IsArray() || !gjson.GetBytes(body, "stream").Bool() {
						t.Fatalf("admin Responses request was not translated: %s", body)
					}
					response := `{"type":"response.completed","response":{"id":"resp-admin","object":"response","status":"completed","model":"` + actualModel + `","output":[{"id":"msg-admin","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp-admin","model":"` + actualModel + `","object":"response","status":"in_progress","output":[]}}` + "\n\nevent: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg-admin","delta":"answer"}` + "\n\nevent: response.completed\ndata: " + response + "\n\n")), Request: r}, nil
				})}
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/test", map[string]any{
					"model": "alias", "client_protocol": "openai", "stream": streaming, "content": "hello",
				}))
				c.Params = gin.Params{{Key: "id", Value: "1"}}
				env.server.HandleChannelTest(c)
				result := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
				if w.Code != http.StatusOK || !result.Success || result.Data["success"] != true || result.Data["client_protocol"] != "openai" || result.Data["upstream_protocol"] != "codex" || result.Data["actual_model"] != actualModel || result.Data["response_text"] != "answer" {
					t.Fatalf("admin negotiation failed: paths=%v response=%s", paths, w.Body.String())
				}
				if len(paths) != 3 || !strings.HasSuffix(paths[0], "/v1/chat/completions") || !strings.HasSuffix(paths[1], "/v1/messages") || !strings.HasSuffix(paths[2], "/v1/responses") {
					t.Fatalf("unexpected admin protocol probes: %v", paths)
				}
				assertOpenCodeNoPersistedCooldown(t, env.server)
			})
		}
	}
}

func TestHandleChannelChatOpenCodeAutoNegotiatesModelProtocol(t *testing.T) {
	const actualModel = "muse-spark-1.3-contributor-free"
	env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", protocolTransformMode: model.ProtocolTransformModeAuto, modelEntries: []model.ModelEntry{{Model: actualModel}}}}, map[int]string{0: "https://opencode.ai/zen/go"})
	var paths []string
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			return jsonResponseStatus(r, http.StatusBadRequest, openCodeUnsupportedProtocolResponse)
		}
		response := `{"type":"response.completed","response":{"id":"resp-chat","object":"response","status":"completed","model":"` + actualModel + `","output":[{"id":"msg-chat","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg-chat","delta":"answer"}` + "\n\nevent: response.completed\ndata: " + response + "\n\n")), Request: r}, nil
	})}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/chat", map[string]any{
		"model": actualModel, "client_protocol": "openai", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}))
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	env.server.HandleChannelChat(c)
	if len(paths) != 3 || !strings.HasSuffix(paths[2], "/v1/responses") {
		t.Fatalf("chat did not negotiate to Responses: paths=%v body=%s", paths, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"delta":"answer"`) || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected frontend SSE: %s", w.Body.String())
	}
	assertOpenCodeNoPersistedCooldown(t, env.server)
}

func TestOpenCodeProtocolNegotiationScopeAndExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, mode, errorBody string
		protocols                       []string
		wantAttempts                    int
		wantNoCooldown                  bool
	}{
		{name: "exhausted auto", endpoint: "https://opencode.ai/zen", mode: model.ProtocolTransformModeAuto, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 4, wantNoCooldown: true},
		{name: "declared auto", endpoint: "https://opencode.ai/zen", mode: model.ProtocolTransformModeAuto, protocols: []string{"openai"}, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 1, wantNoCooldown: true},
		{name: "declared local", endpoint: "https://opencode.ai/zen/go", mode: model.ProtocolTransformModeLocal, protocols: []string{"openai"}, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 1, wantNoCooldown: true},
		{name: "upstream mode", endpoint: "https://opencode.ai/zen", mode: model.ProtocolTransformModeUpstream, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 1, wantNoCooldown: true},
		{name: "other host", endpoint: "https://gateway.example/inference", mode: model.ProtocolTransformModeAuto, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 1},
		{name: "mixed URLs actual other host", endpoint: "https://gateway.example/inference", mode: model.ProtocolTransformModeAuto, errorBody: openCodeUnsupportedProtocolResponse, wantAttempts: 1},
		{name: "ordinary data error", endpoint: "https://opencode.ai/zen", mode: model.ProtocolTransformModeAuto, errorBody: `{"error":{"type":"invalid_request_error","message":"ModelProtocolUnsupported in prompt is invalid"}}`, wantAttempts: 1},
		{name: "wrong typed field", endpoint: "https://opencode.ai/zen", mode: model.ProtocolTransformModeAuto, errorBody: `{"error":{"code":"ModelProtocolUnsupported","message":"invalid input"}}`, wantAttempts: 1},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%v", tc.name, admin), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode scope", protocolTransformMode: tc.mode, models: "model-free", apiKey: "personal-key"}}, map[int]string{0: tc.endpoint})
				ctx := context.Background()
				cfg, err := env.store.GetConfig(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				cfg.URLs[0].Protocols = tc.protocols
				if tc.name == "mixed URLs actual other host" {
					cfg.URLs = append(cfg.URLs, model.ChannelURL{URL: "https://opencode.ai/zen"})
					// Keep OpenCode configured while selecting only the ordinary URL.
					env.server.urlSelector.DisableURL(cfg.ID, "https://opencode.ai/zen")
				}
				if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
				requests := 0
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					if tc.name == "mixed URLs actual other host" && r.URL.Hostname() == "opencode.ai" {
						t.Fatalf("ordinary 400 from first URL triggered protocol/URL fallback: %s", r.URL)
					}
					return jsonResponseStatus(r, http.StatusBadRequest, tc.errorBody)
				})}
				// A typed 400 must never become a negative capability cache entry.
				repeats := 1
				if tc.name == "exhausted auto" {
					repeats = 2
				}
				for range repeats {
					if admin {
						c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/test", map[string]any{"model": "model-free", "client_protocol": "openai", "stream": true}))
						c.Params = gin.Params{{Key: "id", Value: "1"}}
						env.server.HandleChannelTest(c)
						result := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
						if w.Code != http.StatusOK || result.Data["success"] != false || result.Data["status_code"] != float64(http.StatusBadRequest) {
							t.Fatalf("unexpected admin failure contract: %s", w.Body.String())
						}
					} else {
						got := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "model-free", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
						if got.Code != http.StatusBadRequest {
							t.Fatalf("unexpected proxy failure contract: status=%d body=%s", got.Code, got.Body.String())
						}
					}
				}
				if requests != tc.wantAttempts*repeats {
					t.Fatalf("requests=%d, want %d", requests, tc.wantAttempts*repeats)
				}
				if tc.wantNoCooldown {
					assertOpenCodeNoPersistedCooldown(t, env.server)
					for _, stat := range env.server.urlSelector.GetURLStats(cfg.ID, cfg.GetURLs()) {
						if stat.CooledDown {
							t.Fatalf("protocol negotiation cooled URL: %+v", stat)
						}
					}
				} else {
					models, err := env.store.GetAllModelCooldowns(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if !models[cfg.ID]["model-free"].After(time.Now()) {
						t.Fatalf("ordinary 400 classification changed: %v", models)
					}
				}
			})
		}
	}
}

func TestOpenCodeZenGeminiWire(t *testing.T) {
	const reply = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`
	native := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
	for _, tc := range []struct {
		name, path, wantAction, wantQuery string
		request                           map[string]any
	}{
		{"chat", "/v1/chat/completions", ":generateContent", "", map[string]any{"model": "gemini-3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"chat stream", "/v1/chat/completions", ":streamGenerateContent", "alt=sse", map[string]any{"model": "gemini-3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"native", "/v1beta/models/gemini-3-flash:generateContent", ":generateContent", "", native},
		{"native stream", "/v1beta/models/gemini-3-flash:streamGenerateContent?alt=sse", ":streamGenerateContent", "alt=sse", native},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "OpenCode", upstreamProtocol: "gemini", protocolTransformMode: model.ProtocolTransformModeLocal, models: "gemini-3-flash", apiKey: "test-key"}}, map[int]string{0: "https://opencode.ai/zen"})
			called := false
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				// Zen serves Gemini without a version segment and reads only x-goog-api-key.
				if r.URL.Path != "/zen/v1/models/gemini-3-flash"+tc.wantAction || r.URL.RawQuery != tc.wantQuery || r.Header.Get("x-goog-api-key") != "test-key" || r.Header.Get("Authorization") != "" ||
					!strings.HasPrefix(r.Header.Get("User-Agent"), "opencode/") || gjson.GetBytes(body, "tools").Exists() {
					t.Fatalf("unexpected Zen Gemini wire: url=%s headers=%v body=%s", r.URL, r.Header, body)
				}
				if tc.wantQuery != "" {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + reply + "\n\n")), Request: r}, nil
				}
				return jsonResponseStatus(r, http.StatusOK, reply)
			})}
			got := doProxyRequest(t, env.engine, tc.path, tc.request, nil)
			if !called || got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"ok"`) {
				t.Fatalf("Zen Gemini request failed: called=%v status=%d body=%s", called, got.Code, got.Body.String())
			}
		})
	}
}
