package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

func runHandleSuccessResponse(t *testing.T, body string, headers http.Header, isStreaming bool, upstreamProtocol string) (*fwResult, string) {
	t.Helper()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     headers,
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: isStreaming,
	}

	rec := newRecorder()
	s := &Server{}

	cfg := &model.Config{ID: 1}
	res, _, err := s.handleResponse(reqCtx, resp, rec, upstreamProtocol, cfg, "sk-test", nil)
	if err != nil {
		t.Fatalf("handleResponse returned error: %v", err)
	}

	return res, rec.Body.String()
}

func TestCodexOAuthRequestUsesRuntimeCredentialAndCodexWireContract(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{
		ID: 1, Name: "codex", AuthType: model.AuthTypeCodexOAuth,
		URLs:             model.ChannelURLs{{URL: "https://chatgpt.example.test/backend-api/codex/responses", Exact: true, Protocols: []string{"codex"}}},
		CodexAccessToken: "at-secret", CodexAccountID: "account-1",
		CustomRequestRules: &model.CustomRequestRules{Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionOverride, Name: "Authorization", Value: "Bearer attacker"},
			{Action: model.RuleActionOverride, Name: "User-Agent", Value: "attacker"},
			{Action: model.RuleActionOverride, Name: "X-Configured", Value: "kept"},
		}},
	}
	body := []byte(`{"model":"gpt-5.4-mini","stream":false,"input":[{"role":"system","content":"rules"}],"reasoning":{"effort":"minimal"},"max_output_tokens":12,"temperature":0.2,"truncation":"auto","context_management":{"type":"compaction"},"user":"u","previous_response_id":"resp-old","generate":true,"tools":[{"type":"web_search_preview"}]}`)
	reqCtx := &requestContext{
		ctx: context.Background(), startTime: time.Now(), isStreaming: false,
		clientProtocol: protocol.Codex, upstreamProtocol: protocol.Codex,
	}
	req, err := srv.buildProxyRequest(
		reqCtx, cfg, "must-not-be-used", http.MethodPost, body,
		http.Header{
			"Content-Type":                          []string{"application/json"},
			"OpenAI-Beta":                           []string{"http-must-drop"},
			"X-Codex-Beta-Features":                 []string{"feature-1"},
			"Version":                               []string{"1.2.3"},
			"X-Codex-Turn-State":                    []string{"turn-state-1"},
			"X-Codex-Turn-Metadata":                 []string{`{"turn_id":"turn-1"}`},
			"X-Client-Request-Id":                   []string{"request-1"},
			"X-Forwarded-For":                       []string{"203.0.113.10"},
			"X-Arbitrary-Client":                    []string{"drop-me"},
			"X-ResponsesAPI-Include-Timing-Metrics": []string{"true"},
		},
		"", "/v1/responses", cfg.GetURLs()[0],
	)
	if err != nil {
		t.Fatalf("buildProxyRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("ChatGPT-Account-ID"); got != "account-1" {
		t.Fatalf("ChatGPT-Account-ID = %q", got)
	}
	if req.Header.Get("User-Agent") != codexUserAgent || req.Header.Get("Originator") != "codex-tui" {
		t.Fatalf("Codex identity headers = %v", req.Header)
	}
	if req.Header.Get("Session_id") == "" {
		t.Fatalf("Codex Session_id header is missing: %v", req.Header)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
	if req.Header.Get("X-Api-Key") != "" || req.Header.Get("x-goog-api-key") != "" {
		t.Fatalf("static key headers leaked: %v", req.Header)
	}
	for _, name := range []string{
		"X-Codex-Beta-Features", "Version", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Configured",
	} {
		if req.Header.Get(name) == "" {
			t.Fatalf("missing passthrough header %s: %v", name, req.Header)
		}
	}
	for _, name := range []string{
		"OpenAI-Beta", "X-Codex-Turn-State", "X-Forwarded-For", "X-Arbitrary-Client",
		"X-ResponsesAPI-Include-Timing-Metrics",
	} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("unexpected HTTP header %s=%q: %v", name, got, req.Header)
		}
	}
	wireBody := reqCtx.translatedBody
	for _, field := range []string{"max_output_tokens", "temperature", "truncation", "context_management", "user"} {
		if gjson.GetBytes(wireBody, field).Exists() {
			t.Fatalf("unsupported field %s leaked: %s", field, wireBody)
		}
	}
	if !gjson.GetBytes(wireBody, "stream").Bool() || gjson.GetBytes(wireBody, "store").Bool() {
		t.Fatalf("required stream/store values missing: %s", wireBody)
	}
	if got := gjson.GetBytes(wireBody, "input.0.role").String(); got != "developer" {
		t.Fatalf("system role = %q, body=%s", got, wireBody)
	}
	if got := gjson.GetBytes(wireBody, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("tool type = %q, body=%s", got, wireBody)
	}
	if got := gjson.GetBytes(wireBody, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want minimal normalized to low; body=%s", got, wireBody)
	}
	if !gjson.GetBytes(wireBody, "instructions").Exists() ||
		gjson.GetBytes(wireBody, "include.0").String() != "reasoning.encrypted_content" {
		t.Fatalf("Codex required fields missing: %s", wireBody)
	}

	plan, err := protocol.BuildTransformPlan(
		protocol.Codex, protocol.Codex, "/v1/responses", "/v1/responses",
		body, wireBody, "gpt-5.6-sol", "gpt-5.6-sol", false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan() error = %v", err)
	}
	httpBody := responsesBodyForHTTPTransport(cfg, plan, wireBody)
	for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		if gjson.GetBytes(httpBody, field).Exists() {
			t.Fatalf("HTTP-only unsupported field %s leaked: %s", field, httpBody)
		}
	}
}

func TestCodexOAuthNonStreamReassemblesTerminalResponse(t *testing.T) {
	body := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","content":[{"type":"output_text","text":"ok"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}` + "\n\n"
	reqCtx := &requestContext{
		ctx: context.Background(), startTime: time.Now(), codexOAuthNonStream: true,
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	recorder := newRecorder()
	result, _, err := (&Server{}).handleSuccessResponse(
		reqCtx, resp, resp.Header.Clone(), recorder, string(protocol.Codex), &streamReadStats{}, nil,
	)
	if err != nil {
		t.Fatalf("handleSuccessResponse() error = %v", err)
	}
	if !result.ResponseCommitted || result.InputTokens != 10 || result.OutputTokens != 2 {
		t.Fatalf("result = %#v", result)
	}
	if got := gjson.Get(recorder.Body.String(), "id").String(); got != "resp-1" {
		t.Fatalf("response id = %q, body=%s", got, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "output.0.content.0.text").String(); got != "ok" {
		t.Fatalf("reassembled output = %q, body=%s", got, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "data:") || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("SSE framing leaked to non-stream client: %s", recorder.Body.String())
	}
}

// StreamDiagMsg 非空会让 forwardAttempt 把结果判为 599 并触发模型级冷却，
// 所以只有真实上游故障才允许写入：客户端取消必须留空，交给 499 路径。
func TestCodexOAuthNonStreamDiagnosticsOnlyForUpstreamFailure(t *testing.T) {
	partial := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","content":[{"type":"output_text","text":"ok"}]}}` + "\n\n"

	t.Run("client cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reqCtx := &requestContext{ctx: ctx, startTime: time.Now(), codexOAuthNonStream: true}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(partial)),
		}
		result, _, err := (&Server{}).handleSuccessResponse(
			reqCtx, resp, resp.Header.Clone(), newRecorder(), string(protocol.Codex), &streamReadStats{}, nil,
		)
		if err == nil {
			t.Fatalf("expected cancellation error")
		}
		if result.StreamDiagMsg != "" {
			t.Fatalf("客户端取消不得写入流诊断（会被误判为 599）: %q", result.StreamDiagMsg)
		}
	})

	t.Run("upstream failure", func(t *testing.T) {
		reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), codexOAuthNonStream: true}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(io.MultiReader(
				strings.NewReader(partial),
				iotest.ErrReader(errors.New("websocket: close 1006 (abnormal closure): unexpected EOF")),
			)),
		}
		result, _, err := (&Server{}).handleSuccessResponse(
			reqCtx, resp, resp.Header.Clone(), newRecorder(), string(protocol.Codex), &streamReadStats{}, nil,
		)
		if err == nil {
			t.Fatalf("expected upstream read error")
		}
		if result.StreamDiagMsg == "" {
			t.Fatalf("上游中断必须写入流诊断，否则不会归类为 599")
		}
		markIncompleteStreamForwardResult(result)
		if result.Status != util.StatusStreamIncomplete {
			t.Fatalf("status = %d, want %d", result.Status, util.StatusStreamIncomplete)
		}
	})
}

// 598 语义比 599 更精确（冷却时长不同），流诊断不得把它降级覆盖。
func TestMarkIncompleteStreamForwardResultKeepsFirstByteTimeout(t *testing.T) {
	res := &fwResult{Status: util.StatusFirstByteTimeout, StreamDiagMsg: "流传输中断"}
	markIncompleteStreamForwardResult(res)
	if res.Status != util.StatusFirstByteTimeout {
		t.Fatalf("status = %d, want %d", res.Status, util.StatusFirstByteTimeout)
	}

	committed := &fwResult{Status: http.StatusOK, StreamDiagMsg: "流传输中断"}
	markIncompleteStreamForwardResult(committed)
	if committed.Status != util.StatusStreamIncomplete {
		t.Fatalf("status = %d, want %d", committed.Status, util.StatusStreamIncomplete)
	}
}

func TestHandleSuccessResponse_ExtractsUsageFromJSON(t *testing.T) {
	body := `{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}`
	res, forwardedBody := runHandleSuccessResponse(
		t,
		body,
		http.Header{"Content-Type": []string{"application/json"}},
		false,
		"anthropic",
	)

	if res.InputTokens != 10 || res.OutputTokens != 20 || res.CacheReadInputTokens != 5 || res.CacheCreationInputTokens != 7 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if forwardedBody != body {
		t.Fatalf("unexpected response body forwarded: %q", forwardedBody)
	}
}

func TestHandleSuccessResponse_ExtractsUsageFromLargeCodexJSON(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"image_generation_call","result":"` +
		strings.Repeat("a", maxUsageBodySize+1) +
		`"}],"service_tier":"flex","usage":{"input_tokens":7765,"input_tokens_details":{"cached_tokens":0},"output_tokens":379,"total_tokens":8144}}`

	res, forwardedBody := runHandleSuccessResponse(
		t,
		body,
		http.Header{"Content-Type": []string{"application/json"}},
		false,
		"codex",
	)

	if res.InputTokens != 7765 || res.OutputTokens != 379 || res.CacheReadInputTokens != 0 || res.CacheCreationInputTokens != 0 {
		t.Fatalf("unexpected usage extracted from large JSON: %+v", res)
	}
	if res.ServiceTier != "flex" {
		t.Fatalf("unexpected service tier from large JSON: %q", res.ServiceTier)
	}

	if forwardedBody != body {
		t.Fatalf("large JSON response body was not forwarded unchanged")
	}
}

func TestHandleSuccessResponse_ExtractsUsageFromTextPlainSSE(t *testing.T) {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"cache_read_input_tokens\":1,\"cache_creation_input_tokens\":2}}}\n\n"
	res, forwardedBody := runHandleSuccessResponse(
		t,
		body,
		http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		true,
		"anthropic",
	)

	if res.InputTokens != 3 || res.OutputTokens != 4 || res.CacheReadInputTokens != 1 || res.CacheCreationInputTokens != 2 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if forwardedBody != body {
		t.Fatalf("unexpected response body forwarded: %q", forwardedBody)
	}
}

func TestClassifySSEErrorStatus_RateLimits(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "openai_tokens_rate_limit_exceeded",
			body: []byte(`{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5.5 in organization org-test on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 29693. Please try again in 44ms.","param":null},"sequence_number":2}`),
		},
		{
			name: "too_many_requests",
			body: []byte(`{"type":"error","error":{"type":"too_many_requests","code":"too_many_requests","headers":{"x-ms-fe-error":"true"},"message":"Too Many Requests","param":null},"sequence_number":2}`),
		},
		{
			name: "responses_api_response_failed_nested_rate_limit",
			body: []byte(`{"type":"response.failed","response":{"id":"resp_5ca0fb7943504d6a93576c7fb7e3a760","object":"response","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later"}}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySSEErrorStatus(tt.body); got != http.StatusTooManyRequests {
				t.Fatalf("classifySSEErrorStatus()=%d, want %d", got, http.StatusTooManyRequests)
			}
		})
	}
}

func TestClassifySSEErrorStatus_ContextLengthExceeded(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "error_event",
			body: []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`),
		},
		{
			name: "response_failed",
			body: []byte(`{"type":"response.failed","response":{"error":{"code":"context_too_large","message":"Your input exceeds the context window of this model."}}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySSEErrorStatus(tt.body); got != http.StatusBadRequest {
				t.Fatalf("classifySSEErrorStatus()=%d, want %d", got, http.StatusBadRequest)
			}
		})
	}
}

// TestHandleSuccessResponse_StreamDiagMsg_NormalEOF 测试正常EOF时不触发诊断
// 新逻辑：只有当 streamErr != nil 且未检测到流结束标志时才触发诊断
// 正常EOF（streamErr == nil）不触发诊断，即使没有流结束标志
func TestHandleSuccessResponse_StreamDiagMsg_NormalEOF(t *testing.T) {
	// 模拟流式响应，无流结束标志但正常EOF
	body := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n"
	res, _ := runHandleSuccessResponse(
		t,
		body,
		http.Header{"Content-Type": []string{"text/event-stream"}},
		true,
		"anthropic",
	)

	// 正常EOF不应触发诊断（新逻辑：只有 streamErr != nil 才触发）
	if res.StreamDiagMsg != "" {
		t.Errorf("expected empty StreamDiagMsg for normal EOF, got: %s", res.StreamDiagMsg)
	}
}

// TestHandleSuccessResponse_StreamDiagMsg_NonAnthropicNoUsage 测试非anthropic渠道无usage不设置诊断
func TestHandleSuccessResponse_StreamDiagMsg_NonAnthropicNoUsage(t *testing.T) {
	// 非anthropic渠道流式响应无usage是正常的
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	res, _ := runHandleSuccessResponse(
		t,
		body,
		http.Header{"Content-Type": []string{"text/event-stream"}},
		true,
		"openai",
	)

	// 非anthropic渠道无usage不应该设置诊断消息
	if res.StreamDiagMsg != "" {
		t.Errorf("expected empty StreamDiagMsg for non-anthropic channel, got: %s", res.StreamDiagMsg)
	}
}

// TestBuildStreamDiagnostics_StreamComplete 验证检测到流结束标志时即使有streamErr也不触发诊断
func TestBuildStreamDiagnostics_StreamComplete(t *testing.T) {
	tests := []struct {
		name             string
		streamErr        error
		streamComplete   bool
		upstreamProtocol string
		wantDiag         bool
		reason           string
	}{
		{
			name:             "http2_closed_with_stream_complete",
			streamErr:        errors.New("http2: response body closed"),
			streamComplete:   true,
			upstreamProtocol: "anthropic",
			wantDiag:         false,
			reason:           "检测到流结束标志，http2关闭是正常结束",
		},
		{
			name:             "http2_closed_without_stream_complete",
			streamErr:        errors.New("http2: response body closed"),
			streamComplete:   false,
			upstreamProtocol: "anthropic",
			wantDiag:         true,
			reason:           "无流结束标志时http2关闭是异常中断",
		},
		{
			name:             "unexpected_eof_with_stream_complete",
			streamErr:        errors.New("unexpected EOF"),
			streamComplete:   true,
			upstreamProtocol: "anthropic",
			wantDiag:         false,
			reason:           "检测到流结束标志，EOF可能是正常关闭",
		},
		{
			name:             "stream_error_with_stream_complete",
			streamErr:        errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			streamComplete:   true,
			upstreamProtocol: "codex",
			wantDiag:         false,
			reason:           "codex渠道检测到流结束标志也不应触发诊断",
		},
		{
			name:             "no_error_no_stream_complete",
			streamErr:        nil,
			streamComplete:   false,
			upstreamProtocol: "anthropic",
			wantDiag:         false,
			reason:           "无错误时不触发诊断（正常EOF情况）",
		},
		{
			name:             "no_error_with_stream_complete",
			streamErr:        nil,
			streamComplete:   true,
			upstreamProtocol: "openai",
			wantDiag:         false,
			reason:           "无错误且有流结束标志，无诊断",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readStats := &streamReadStats{totalBytes: 1024, readCount: 4}
			diag := buildStreamDiagnostics(tt.streamErr, readStats, tt.streamComplete, tt.upstreamProtocol, "text/event-stream")

			hasDiag := diag != ""
			if hasDiag != tt.wantDiag {
				t.Errorf("%s: got diag=%q, wantDiag=%v", tt.reason, diag, tt.wantDiag)
			}
		})
	}
}

func TestCodexBodyWithoutThinking_RemovesReasoningControls(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"reasoning":{"effort":"medium","summary":"auto"},
		"include":["reasoning.encrypted_content","file_search_call.results"],
		"input":[
			{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"drop"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}
		]
	}`)

	got, ok := codexBodyWithoutThinking(body)
	if !ok {
		t.Fatal("codexBodyWithoutThinking returned ok=false")
	}
	text := string(got)
	if strings.Contains(text, `"reasoning"`) {
		t.Fatalf("retry body should remove reasoning controls, got %s", text)
	}
	if strings.Contains(text, `reasoning.encrypted_content`) {
		t.Fatalf("retry body should remove reasoning include, got %s", text)
	}
	if !strings.Contains(text, `file_search_call.results`) ||
		!strings.Contains(text, `"type":"message"`) {
		t.Fatalf("retry body should preserve unrelated include and message input, got %s", text)
	}
}

func TestCodexRetryBodyFor400_FallsThroughToThinkingWhenAnyrouterBodyUnchanged(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"reasoning":{"effort":"medium"},
		"input":[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}
		]
	}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"message":"invalid_responses_request: reasoning is unsupported","code":"invalid_responses_request","param":"reasoning","type":"invalid_request_error"}}`),
	}
	plan := protocol.TransformPlan{TranslatedBody: body}
	cfg := &model.Config{Name: "anyrouter-codex"}

	got, strategy, ok := codexRetryBodyFor400(protocol.Codex, cfg, plan, res)
	if !ok {
		t.Fatal("codexRetryBodyFor400 returned ok=false")
	}
	if strategy != "strip_codex_thinking" {
		t.Fatalf("strategy=%q, want strip_codex_thinking", strategy)
	}
	text := string(got)
	if strings.Contains(text, `"reasoning"`) ||
		!strings.Contains(text, `"type":"message"`) {
		t.Fatalf("unexpected retry body: %s", text)
	}
}

func TestPrepareCodexResponsesBodyForUpstream_StripsAnyrouterUnsupportedInputBeforeForward(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]},
			{"type":"tool_search_call","arguments":{"query":"drop"}},
			{"type":"tool_search_output","result":"drop"},
			{"type":"compaction"},
			{"type":"reasoning","summary":[]}
		]
	}`)
	cfg := &model.Config{Name: "regular-codex", URLs: model.ChannelURLs{{URL: "https://anyrouter.top"}}}

	got := prepareCodexResponsesBodyForUpstream(cfg, protocol.Codex, "/v1/responses", body)
	text := string(got)
	if strings.Contains(text, `"tool_search_call"`) ||
		strings.Contains(text, `"tool_search_output"`) {
		t.Fatalf("anyrouter codex body should drop tool search input items before forward, got %s", text)
	}
	if !strings.Contains(text, `"type":"message"`) ||
		!strings.Contains(text, `"type":"reasoning"`) ||
		!strings.Contains(text, `"compaction"`) {
		t.Fatalf("anyrouter codex body should preserve non-tool-search input items, got %s", text)
	}
}

func TestPrepareCodexResponsesBodyForUpstream_KeepsRegularCodexToolSearch(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]},
			{"type":"tool_search_call","arguments":{"query":"keep"}}
		]
	}`)
	cfg := &model.Config{Name: "regular-codex", URLs: model.ChannelURLs{{URL: "https://api.openai.com"}}}

	got := prepareCodexResponsesBodyForUpstream(cfg, protocol.Codex, "/v1/responses", body)
	if !strings.Contains(string(got), `"tool_search_call"`) {
		t.Fatalf("regular codex body should keep tool_search input items, got %s", got)
	}
}

func TestTranslatedStreamChunkCompletes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		clientProtocol protocol.Protocol
		chunk          []byte
		want           bool
	}{
		{
			name:           "anthropic message_stop event",
			clientProtocol: protocol.Anthropic,
			chunk:          []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			want:           true,
		},
		{
			name:           "anthropic content delta",
			clientProtocol: protocol.Anthropic,
			chunk:          []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"),
			want:           false,
		},
		{
			name:           "codex response completed",
			clientProtocol: protocol.Codex,
			chunk:          []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"),
			want:           true,
		},
		{
			name:           "codex text delta",
			clientProtocol: protocol.Codex,
			chunk:          []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"),
			want:           false,
		},
		{
			name:           "openai finish reason stop",
			clientProtocol: protocol.OpenAI,
			chunk:          []byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"),
			want:           true,
		},
		{
			name:           "openai done sentinel",
			clientProtocol: protocol.OpenAI,
			chunk:          []byte("data: [DONE]\n\n"),
			want:           true,
		},
		{
			name:           "openai intermediate chunk",
			clientProtocol: protocol.OpenAI,
			chunk:          []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"),
			want:           false,
		},
		{
			name:           "gemini finish reason stop",
			clientProtocol: protocol.Gemini,
			chunk:          []byte("data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n"),
			want:           true,
		},
		{
			name:           "gemini intermediate chunk",
			clientProtocol: protocol.Gemini,
			chunk:          []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"),
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translatedStreamChunkCompletes(tt.clientProtocol, tt.chunk)
			if got != tt.want {
				t.Fatalf("translatedStreamChunkCompletes(%s) = %v, want %v", tt.clientProtocol, got, tt.want)
			}
		})
	}
}

func TestParseSSEEventChunkJoinsDataLinesWithNewline(t *testing.T) {
	t.Parallel()

	eventType, data := parseSSEEventChunk([]byte("event: test\ndata: first\ndata: second\n\n"))
	if eventType != "test" {
		t.Fatalf("eventType=%q, want test", eventType)
	}
	if got, want := string(data), "first\nsecond"; got != want {
		t.Fatalf("data=%q, want %q", got, want)
	}
}

func TestDetectProtocolFromSSEPrefix_SkipsUndecisiveEvents(t *testing.T) {
	t.Parallel()

	prefix := []byte(
		"event: ping\n" +
			"data: {\"type\":\"ping\"}\n\n" +
			"event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n",
	)

	if got := detectProtocolFromSSEPrefix(prefix); got != protocol.Anthropic {
		t.Fatalf("detectProtocolFromSSEPrefix() = %s, want %s", got, protocol.Anthropic)
	}
}

func TestDetectProtocolFromSSEPrefix_AnthropicPing(t *testing.T) {
	t.Parallel()

	prefix := []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")

	if got := detectProtocolFromSSEPrefix(prefix); got != protocol.Anthropic {
		t.Fatalf("detectProtocolFromSSEPrefix() = %s, want %s", got, protocol.Anthropic)
	}
}

type partialErrReadCloser struct {
	data []byte
	err  error
	read bool
}

func (rc *partialErrReadCloser) Read(p []byte) (int, error) {
	if rc.read {
		return 0, io.EOF
	}
	rc.read = true
	n := copy(p, rc.data)
	return n, rc.err
}

func (rc *partialErrReadCloser) Close() error { return nil }

type errAfterDataReadCloser struct {
	data  []byte
	err   error
	stage int
}

func (rc *errAfterDataReadCloser) Read(p []byte) (int, error) {
	switch rc.stage {
	case 0:
		rc.stage++
		n := copy(p, rc.data)
		return n, nil
	case 1:
		rc.stage++
		return 0, rc.err
	default:
		return 0, io.EOF
	}
}

func (rc *errAfterDataReadCloser) Close() error { return nil }

func TestHandleTranslatedStreamSuccessResponse_TreatsTranslatedStopAsComplete(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	s := &Server{protocolRegistry: reg}
	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: true,
		transformPlan: protocol.TransformPlan{
			ClientProtocol:   protocol.Anthropic,
			UpstreamProtocol: protocol.OpenAI,
			OriginalModel:    "claude-3-5-sonnet",
			ActualModel:      "gpt-4o",
			NeedsTransform:   true,
		},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: &errAfterDataReadCloser{
			data: []byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n"),
			err:  errors.New("http2: response body closed"),
		},
	}

	rec := newRecorder()
	readStats := &streamReadStats{}

	res, _, err := s.handleTranslatedStreamSuccessResponse(reqCtx, resp, resp.Header.Clone(), rec, "openai", readStats, nil)
	if err != nil {
		t.Fatalf("expected translated completed stream to ignore trailing close error, got %v", err)
	}
	if res.StreamDiagMsg != "" {
		t.Fatalf("expected no incomplete-stream diagnostics after translated stop, got %s", res.StreamDiagMsg)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("expected translated output to include message_stop, got %s", body)
	}
}

func TestHandleErrorResponse_MergesBodyReadErrorIntoResult(t *testing.T) {
	s := &Server{} // 关键：logService 为 nil，若 handleErrorResponse 仍写 DB 日志会直接 panic

	reqCtx := &requestContext{
		startTime: time.Now(),
	}

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body: &partialErrReadCloser{
			data: []byte(`{"error":"余额不足"}`),
			err:  errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer"),
		},
	}

	readStats := &streamReadStats{firstByteSec: 1.234}
	res, _, err := s.handleErrorResponse(reqCtx, resp, http.Header{}, readStats)
	if err != nil {
		t.Fatalf("expected err=nil, got %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Fatalf("expected Status=%d, got %d", http.StatusForbidden, res.Status)
	}
	if got := string(res.Body); got != `{"error":"余额不足"}` {
		t.Fatalf("expected Body preserved, got %q", got)
	}
	if res.FirstByteTime != readStats.firstByteSec {
		t.Fatalf("expected FirstByteTime=%.3f, got %.3f", readStats.firstByteSec, res.FirstByteTime)
	}
	if res.StreamDiagMsg == "" {
		t.Fatalf("expected StreamDiagMsg not empty")
	}
	if !strings.Contains(res.StreamDiagMsg, "error reading upstream body") {
		t.Fatalf("expected StreamDiagMsg to include read error prefix, got %q", res.StreamDiagMsg)
	}
	if !strings.Contains(res.StreamDiagMsg, "INTERNAL_ERROR") {
		t.Fatalf("expected StreamDiagMsg to include upstream error, got %q", res.StreamDiagMsg)
	}
}
