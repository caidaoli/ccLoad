package app

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
	"ccLoad/internal/util"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
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

func headerValueFold(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
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
	if req.Header.Get("User-Agent") != codexUserAgent ||
		req.Header.Get("Originator") != codexOriginator ||
		req.Header.Get("Version") != codexVersion {
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
		"X-Codex-Beta-Features", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Configured",
	} {
		if req.Header.Get(name) == "" {
			t.Fatalf("missing passthrough header %s: %v", name, req.Header)
		}
	}
	for _, name := range []string{
		"OpenAI-Beta", "X-Forwarded-For", "X-Arbitrary-Client",
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
	if instructions := gjson.GetBytes(wireBody, "instructions").String(); !strings.HasPrefix(instructions, "You are Codex, a coding agent based on GPT-5.") {
		t.Fatalf("Codex model instructions missing: %s", wireBody)
	}
	if gjson.GetBytes(wireBody, "include.0").String() != "reasoning.encrypted_content" {
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

func TestCodexOAuthRequestInjectsModelInstructionsAndPreservesExplicitValue(t *testing.T) {
	cfg := &model.Config{AuthType: model.AuthTypeCodexOAuth}
	tests := []struct {
		name             string
		body             string
		wantPrefix       string
		wantInstructions string
	}{
		{
			name:       "gpt-5.1",
			body:       `{"model":"gpt-5.1","input":[]}`,
			wantPrefix: "You are GPT-5.1 running in the Codex CLI",
		},
		{
			name:       "gpt-5.2 blank instructions",
			body:       `{"model":"gpt-5.2","instructions":"  ","input":[]}`,
			wantPrefix: "You are GPT-5.2 running in the Codex CLI",
		},
		{
			name:       "gpt-5.6",
			body:       `{"model":"gpt-5.6-sol","input":[]}`,
			wantPrefix: "You are Codex, an agent based on GPT-5.",
		},
		{
			name:       "codex model",
			body:       `{"model":"gpt-5.3-codex","input":[]}`,
			wantPrefix: "You are Codex, based on GPT-5.",
		},
		{
			name:             "explicit instructions",
			body:             `{"model":"gpt-5.6-sol","instructions":"keep this","input":[]}`,
			wantInstructions: "keep this",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := prepareCodexOAuthResponsesBody(
				cfg, protocol.Codex, "/v1/responses", []byte(tt.body), make(http.Header),
			)
			instructions := gjson.GetBytes(body, "instructions").String()
			if tt.wantInstructions != "" {
				if instructions != tt.wantInstructions {
					t.Fatalf("instructions = %q, want %q", instructions, tt.wantInstructions)
				}
				return
			}
			if !strings.HasPrefix(instructions, tt.wantPrefix) {
				t.Fatalf("instructions prefix = %q, want %q; body=%s", instructions, tt.wantPrefix, body)
			}
			if strings.Contains(instructions, "{{ personality }}") {
				t.Fatalf("instructions retained template placeholder: %s", body)
			}
		})
	}
}

func TestCodexOAuthNonStreamReassemblesTerminalResponse(t *testing.T) {
	body := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","content":[{"type":"output_text","text":"ok"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}` + "\n\n"
	reqCtx := &requestContext{
		ctx: context.Background(), startTime: time.Now(), responsesSSEUpstreamNonStream: true,
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
		reqCtx := &requestContext{ctx: ctx, startTime: time.Now(), responsesSSEUpstreamNonStream: true}
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
		reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), responsesSSEUpstreamNonStream: true}
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

func TestCodexQuotaOverdraftRetryBody_AppendsCompletedToolPair(t *testing.T) {
	original := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	before := bytes.Clone(original)
	cfg := &model.Config{
		AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: `{"type":"codex","access_token":"at","refresh_token":"rt",` +
			`"expired":"2030-01-01T00:00:00Z","quota_overdraft":{"enabled":true}}`,
	}
	plan := protocol.TransformPlan{
		ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex,
		RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: original,
	}
	res := &fwResult{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"type":"usage_limit_reached","resets_at":4102444800}}`),
	}

	retryBody, retryTranscript, activeUntil, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPost, plan, res, original)
	if !ok {
		t.Fatal("codexQuotaOverdraftRetryBodies returned ok=false")
	}
	if activeUntil != 4102444800 {
		t.Fatalf("activeUntil=%d, want upstream reset", activeUntil)
	}
	if !bytes.Equal(retryBody, retryTranscript) {
		t.Fatalf("wire body and transcript must contain the same tool pair:\nwire=%s\ntranscript=%s", retryBody, retryTranscript)
	}
	if !bytes.Equal(original, before) {
		t.Fatalf("original request body was mutated: %s", original)
	}
	items := gjson.GetBytes(retryBody, "input").Array()
	if len(items) != 3 {
		t.Fatalf("input items=%d body=%s, want 3", len(items), retryBody)
	}
	callID := items[1].Get("call_id").String()
	if items[1].Get("type").String() != "custom_tool_call" || items[1].Get("name").String() != "exec" ||
		!strings.HasPrefix(callID, "call_ccload_overdraft_") ||
		items[2].Get("type").String() != "custom_tool_call_output" || items[2].Get("call_id").String() != callID ||
		items[2].Get("output.0.text").String() != codexQuotaOverdraftExecOutput {
		t.Fatalf("invalid overdraft tool pair: %s", retryBody)
	}

	if _, _, _, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPut, plan, res, original); ok {
		t.Fatal("non-POST Responses request must not be replayed internally")
	}

	sseResult := &fwResult{
		Status: http.StatusOK,
		SSEErrorEvent: []byte(`{"type":"error","error":{"type":"usage_limit_reached"},` +
			`"status_code":429}`),
	}
	if _, _, _, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPost, plan, sseResult, original); !ok {
		t.Fatal("uncommitted SSE usage_limit_reached must be replayed")
	}
	for name, event := range map[string][]byte{
		"missing embedded status": []byte(`{"type":"error","error":{"type":"usage_limit_reached"}}`),
		"wrong embedded status":   []byte(`{"type":"error","error":{"type":"usage_limit_reached"},"status_code":401}`),
	} {
		t.Run(name, func(t *testing.T) {
			result := &fwResult{Status: http.StatusOK, SSEErrorEvent: event}
			if _, _, _, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPost, plan, result, original); ok {
				t.Fatal("SSE usage limit without embedded 429 must not be replayed")
			}
		})
	}
	sseResult.ResponseCommitted = true
	if _, _, _, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPost, plan, sseResult, original); ok {
		t.Fatal("committed SSE error must not be replayed")
	}

	for name, body := range map[string][]byte{
		"implicit message type": []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`),
		"trailing Responses Lite tools": []byte(`{"model":"gpt-5.4","input":[` +
			`{"role":"user","content":"hello"},{"type":"additional_tools","tools":[]}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			variantPlan := plan
			variantPlan.TranslatedBody = body
			retryBody, _, _, ok := codexQuotaOverdraftRetryBodies(cfg, http.MethodPost, variantPlan, res, body)
			if !ok || len(gjson.GetBytes(retryBody, "input").Array()) != len(gjson.GetBytes(body, "input").Array())+2 {
				t.Fatalf("valid Responses user turn was not replayed: ok=%v body=%s", ok, retryBody)
			}
		})
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

func TestAnthropicOAuthFinalizerBuildsClaudeCodeWireContract(t *testing.T) {
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account-uuid",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}
	body, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-sonnet-4-5","system":"answer tersely","messages":[{"role":"user","content":"hello world"}],
		"thinking":{"type":"enabled"},"tool_choice":{"type":"auto"}
	}`), cfg, http.Header{"User-Agent": []string{"third-party-client"}})
	if err != nil {
		t.Fatalf("finalizeAnthropicOAuthMessagesBody() error = %v", err)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q", got)
	}
	if got := gjson.GetBytes(body, "system.0.text").String(); !strings.HasPrefix(got, "x-anthropic-billing-header:") {
		t.Fatalf("billing block = %q", got)
	} else if strings.Contains(got, "cch=00000;") || !strings.Contains(got, " cch=") {
		t.Fatalf("billing block is not signed = %q", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content").String(); got != "[System Instructions]\nanswer tersely" {
		t.Fatalf("moved system = %q", got)
	}
	if !gjson.GetBytes(body, "tools").IsArray() || gjson.GetBytes(body, "tool_choice").Exists() ||
		gjson.GetBytes(body, "temperature").Exists() || gjson.GetBytes(body, "max_tokens").Exists() ||
		gjson.GetBytes(body, "context_management.edits.0.type").String() != "clear_thinking_20251015" ||
		gjson.GetBytes(body, "metadata.user_id").String() == "" {
		t.Fatalf("normalized body = %s", body)
	}

	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer attacker")
	injectAnthropicOAuthHeaders(request, cfg, "oauth-access", body)
	if headerValueFold(request.Header, "authorization") != "Bearer oauth-access" || headerValueFold(request.Header, "x-api-key") != "" ||
		headerValueFold(request.Header, "anthropic-version") != "2023-06-01" ||
		!strings.Contains(headerValueFold(request.Header, "anthropic-beta"), "oauth-2025-04-20") ||
		headerValueFold(request.Header, "X-Claude-Code-Session-Id") == "" || headerValueFold(request.Header, "x-client-request-id") == "" ||
		headerValueFold(request.Header, "Accept-Encoding") != "gzip, deflate, br, zstd" ||
		headerValueFold(request.Header, "X-Stainless-Runtime-Version") != "v26.3.0" {
		t.Fatalf("Anthropic OAuth headers = %v", request.Header)
	}
	if got := buildAnthropicOAuthURL("https://api.anthropic.com", "/v1/messages", "foo=bar"); got != "https://api.anthropic.com/v1/messages?beta=true&foo=bar" {
		t.Fatalf("upstream URL = %q", got)
	}
}

func TestAnthropicOAuthFinalizerReplacesForgedBillingPrefix(t *testing.T) {
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account-uuid",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	body, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"x-anthropic-billing-header: attacker-controlled"}],
		"messages":[{"role":"user","content":"hello"}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, nil)
	if err != nil {
		t.Fatalf("finalizeAnthropicOAuthMessagesBody() error = %v", err)
	}
	if got := gjson.GetBytes(body, "system.0.text").String(); got == "x-anthropic-billing-header: attacker-controlled" ||
		!strings.Contains(got, "cc_version=2.1.220.") {
		t.Fatalf("forged billing block survived: %q", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content").String(); got != "[System Instructions]\nx-anthropic-billing-header: attacker-controlled" {
		t.Fatalf("client system was not demoted to instructions: %q", got)
	}
}

func TestAnthropicOAuthPreservesNativeClaudeCodeBody(t *testing.T) {
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	parsedCredential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}
	nativeBody := []byte(fmt.Sprintf(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=00000;"}],
		"metadata":{"user_id":"{\"device_id\":\"%s\",\"account_uuid\":\"account\",\"session_id\":\"e03895ad-8b34-4a84-bbf6-002e8909b17b\"}"},
		"messages":[{"role":"user","content":"hello"}],"max_tokens":1024
	}`, parsedCredential.DeviceID))
	nativeHeaders := http.Header{
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"},
		"X-App":      {"cli"}, "Anthropic-Beta": {"claude-code-20250219"},
		"X-Claude-Code-Session-Id": {"e03895ad-8b34-4a84-bbf6-002e8909b17b"},
	}
	finalized, err := finalizeAnthropicOAuthMessagesBody(
		nativeBody, cfg, nativeHeaders,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(finalized, "system.0.text").String(); !strings.Contains(got, "cc_entrypoint=cli; cch=") || strings.Contains(got, "cch=00000;") {
		t.Fatalf("native billing block was not preserved and signed: %q", got)
	}
	if bytes.Contains(finalized, []byte(`"cache_control"`)) || gjson.GetBytes(finalized, "temperature").Exists() {
		t.Fatalf("native body was normalized instead of preserved: %s", finalized)
	}
}

func TestAnthropicOAuthPreservesMarkerlessHaikuHelper(t *testing.T) {
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "11111111-2222-4333-8444-555555555555"
	userID := fmt.Sprintf(`{"device_id":%q,"account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":%q}`,
		credential.DeviceID, sessionID)
	body := []byte(fmt.Sprintf(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"helper probe"}],"metadata":{"user_id":%q}}`, userID))
	betas := "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05"
	headers := http.Header{
		"Accept": {"application/json"}, "Accept-Encoding": {"gzip"}, "Content-Type": {"application/json"},
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {betas},
		"Anthropic-Version": {"2023-06-01"}, "Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"X-Claude-Code-Session-Id": {sessionID}, "X-Client-Request-Id": {"66666666-7777-4888-8999-aaaaaaaaaaaa"},
		"X-Stainless-Lang": {"js"}, "X-Stainless-Runtime": {"node"}, "X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v26.3.0"}, "X-Stainless-OS": {"MacOS"}, "X-Stainless-Arch": {"arm64"},
		"X-Stainless-Retry-Count": {"0"}, "X-Stainless-Timeout": {"600"},
	}
	cfg := &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}

	finalized, err := finalizeAnthropicOAuthMessagesBody(body, cfg, headers)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finalized, body) {
		t.Fatalf("markerless helper body changed:\n got %s\nwant %s", finalized, body)
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(finalized))
	if err != nil {
		t.Fatal(err)
	}
	injectAnthropicOAuthHeaders(req, cfg, "oauth-access", finalized, headers)
	if got := req.Header.Get("Anthropic-Beta"); got != betas {
		t.Fatalf("helper beta profile = %q, want exact %q", got, betas)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "gzip" {
		t.Fatalf("helper Accept-Encoding = %q, want gzip", got)
	}
	if strings.Contains(string(finalized), "cache_control") || gjson.GetBytes(finalized, "system").Exists() {
		t.Fatalf("helper gained synthetic native fields: %s", finalized)
	}
	otherCredentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "other-access", RefreshToken: "other-refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := finalizeAnthropicOAuthMessagesBody(body, &model.Config{
		AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: otherCredentialJSON,
	}, headers)
	if err != nil || !bytes.Equal(pooled, body) {
		t.Fatalf("native helper was tied to the selected pool credential: err=%v body=%s", err, pooled)
	}
	reordered := []byte(fmt.Sprintf(`{"max_tokens":1,"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"helper probe"}],"metadata":{"user_id":%q}}`, userID))
	cloaked, err := finalizeAnthropicOAuthMessagesBody(reordered, cfg, headers)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(cloaked, "system").Exists() {
		t.Fatalf("non-native helper member order bypassed cloaking: %s", cloaked)
	}
}

func TestAnthropicOAuthPreservesStructuredHaikuHelper(t *testing.T) {
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "11111111-2222-4333-8444-555555555555"
	userID := fmt.Sprintf(`{"device_id":%q,"account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":%q}`,
		credential.DeviceID, sessionID)
	body := []byte(fmt.Sprintf(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":[{"type":"text","text":"helper probe"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"tools":[],"metadata":{"user_id":%q},"max_tokens":32000,"thinking":{"type":"disabled"},"temperature":1,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`, userID))
	betas := "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15"
	headers := http.Header{
		"Accept": {"application/json"}, "Accept-Encoding": {"gzip, deflate, br, zstd"}, "Content-Type": {"application/json"},
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {betas},
		"Anthropic-Version": {"2023-06-01"}, "Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"X-Claude-Code-Session-Id": {sessionID}, "X-Client-Request-Id": {"66666666-7777-4888-8999-aaaaaaaaaaaa"},
		"X-Stainless-Async": {"async"}, "X-Stainless-Lang": {"js"}, "X-Stainless-Runtime": {"node"},
		"X-Stainless-Package-Version": {"0.94.0"}, "X-Stainless-Runtime-Version": {"v26.3.0"},
		"X-Stainless-OS": {"MacOS"}, "X-Stainless-Arch": {"arm64"}, "X-Stainless-Retry-Count": {"0"}, "X-Stainless-Timeout": {"600"},
	}
	finalized, err := finalizeAnthropicOAuthMessagesBody(
		body, &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(finalized, []byte(`"cache_control"`)) || !gjson.GetBytes(finalized, "thinking.type").Exists() ||
		gjson.GetBytes(finalized, "temperature").Num != 1 || !gjson.GetBytes(finalized, "stream").Bool() {
		t.Fatalf("structured helper shape changed: %s", finalized)
	}
	if got := gjson.GetBytes(finalized, "system.0.text").String(); strings.Contains(got, "cch=00000") || !strings.Contains(got, " cch=") {
		t.Fatalf("structured helper CCH was not refreshed: %q", got)
	}
}

func TestAnthropicOAuthDropsOnlyAutoContextManagementWithoutThinking(t *testing.T) {
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}
	autoBody, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-opus-4-6","messages":[{"role":"user","content":"run"}],
		"tools":[{"name":"run","description":"run","input_schema":{"type":"object"}}],
		"thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"any"}
	}`), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(autoBody, "thinking").Exists() || gjson.GetBytes(autoBody, "context_management").Exists() {
		t.Fatalf("forced tool choice retained invalid automatic thinking state: %s", autoBody)
	}

	callerBody, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-opus-4-6","messages":[{"role":"user","content":"run"}],
		"context_management":{"edits":[{"type":"caller-owned"}]}
	}`), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(callerBody, "context_management.edits.0.type").String(); got != "caller-owned" {
		t.Fatalf("caller context_management ownership was lost: %s", callerBody)
	}
}

func TestAnthropicOAuthCloakOwnsSystemAndRollingMessageCache(t *testing.T) {
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	body, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-opus-4-6",
		"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"second"}],
		"tools":[{"name":"search","description":"search","input_schema":{"type":"object"}}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "system.2.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("system cache ttl = %q, want 1h: %s", got, body)
	}
	if got := gjson.GetBytes(body, "messages.2.content.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("rolling message cache ttl = %q, want 1h: %s", got, body)
	}
	if bytes.Count(body, []byte(`"cache_control":{"type":"ephemeral","ttl":"1h"}`)) != 2 ||
		bytes.Contains(body, []byte(`"cache_control":{"ttl":"1h","type":"ephemeral"}`)) {
		t.Fatalf("cache_control wire order does not match native shape: %s", body)
	}
	resigned, err := finalizeAnthropicCCH(body)
	if err != nil || !bytes.Equal(resigned, body) {
		t.Fatalf("cache wire rewrite invalidated CCH: err=%v\n got %s\nwant %s", err, resigned, body)
	}
	if gjson.GetBytes(body, "tools.0.cache_control").Exists() {
		t.Fatalf("tools should remain unstamped when system owns the prefix: %s", body)
	}
}

func TestAnthropicOAuthRejectsForgedNativeFingerprint(t *testing.T) {
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "real-account",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	body, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"x-anthropic-billing-header: forged; cc_entrypoint=cli; cch=00000;"}],
		"metadata":{"user_id":"x"},
		"messages":[{"role":"user","content":"hello"}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON},
		http.Header{"User-Agent": []string{"claude-cli/fake"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "system.0.text").String(); strings.Contains(got, "forged") || !strings.Contains(got, "cc_version=2.1.220.") {
		t.Fatalf("forged native fingerprint bypassed cloaking: %q", got)
	}
	identity := gjson.GetBytes(body, "metadata.user_id").String()
	if gjson.Get(identity, "account_uuid").String() != "real-account" || gjson.Get(identity, "session_id").String() == "" {
		t.Fatalf("credential identity was not rebuilt: %q", identity)
	}
}

func TestAnthropicCCHMatchesClaudeCodeKnownVector(t *testing.T) {
	base := `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=sdk-cli; cch=00000;"},{"type":"text","text":"system-x"}],"tools":[],"metadata":{"user_id":"meta-x"},"max_tokens":1,"thinking":{"type":"adaptive","display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"high"},"stream":true}`
	tests := []struct{ body, want string }{
		{body: base, want: "7ee87"},
		{
			body: strings.Replace(base, `"metadata":{"user_id":"meta-x"}`, `"metadata":{"user_id":"meta-x","max_tokens":999,"fallbacks":[{"model":"fallback-model"}]}`, 1),
			want: "4589b",
		},
	}
	for _, test := range tests {
		signed, err := finalizeAnthropicCCH([]byte(test.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(signed, "system.0.text").String(); !strings.Contains(got, "cch="+test.want+";") {
			t.Fatalf("Claude CCH vector mismatch: got %q, want %s", got, test.want)
		}
	}
}

func TestAnthropicOAuthDecodesAdvertisedClaudeCodeResponseEncodings(t *testing.T) {
	const want = `{"type":"message","content":[{"type":"text","text":"hello"}]}`
	encoders := map[string]func(*bytes.Buffer) io.WriteCloser{
		"gzip": func(buffer *bytes.Buffer) io.WriteCloser { return gzip.NewWriter(buffer) },
		"deflate": func(buffer *bytes.Buffer) io.WriteCloser {
			writer := zlib.NewWriter(buffer)
			return writer
		},
		"br": func(buffer *bytes.Buffer) io.WriteCloser { return brotli.NewWriter(buffer) },
		"zstd": func(buffer *bytes.Buffer) io.WriteCloser {
			writer, err := zstd.NewWriter(buffer)
			if err != nil {
				t.Fatalf("create zstd writer: %v", err)
			}
			return writer
		},
	}
	for encoding, newWriter := range encoders {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			writer := newWriter(&compressed)
			if _, err := io.WriteString(writer, want); err != nil {
				t.Fatalf("compress response: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close compressor: %v", err)
			}
			response := &http.Response{
				Header: http.Header{"Content-Encoding": []string{encoding}, "Content-Length": []string{"123"}},
				Body:   io.NopCloser(bytes.NewReader(compressed.Bytes())), ContentLength: int64(compressed.Len()),
			}
			if err := decodeAnthropicResponse(response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			decoded, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read decoded response: %v", err)
			}
			_ = response.Body.Close()
			if string(decoded) != want || response.Header.Get("Content-Encoding") != "" ||
				response.Header.Get("Content-Length") != "" || response.ContentLength != -1 || !response.Uncompressed {
				t.Fatalf("decoded response body=%q headers=%v length=%d", decoded, response.Header, response.ContentLength)
			}
		})
	}
}

func TestAnthropicOAuthBuildProxyRequestUsesOAuthWireAfterCustomRules(t *testing.T) {
	srv := newInMemoryServer(t)
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account-uuid",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{
		ID: 91, Name: "Anthropic", AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON,
		URLs: model.ChannelURLs{{URL: "https://api.anthropic.com", Protocols: []string{"anthropic"}}},
		CustomRequestRules: &model.CustomRequestRules{Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionOverride, Name: "Authorization", Value: "Bearer attacker"},
			{Action: model.RuleActionOverride, Name: "User-Agent", Value: "attacker"},
			{Action: model.RuleActionOverride, Name: "X-Configured", Value: "must-drop"},
		}},
	}
	reqCtx := &requestContext{
		ctx: context.Background(), startTime: time.Now(), isStreaming: true,
		clientProtocol: protocol.Anthropic, upstreamProtocol: protocol.Anthropic,
	}
	request, err := srv.buildProxyRequest(
		reqCtx, cfg, "oauth-access", http.MethodPost,
		[]byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
		http.Header{"Content-Type": []string{"application/json"}, "Anthropic-Beta": []string{"attacker-beta"}},
		"client=true", "/v1/messages", cfg.GetURLs()[0],
	)
	if err != nil {
		t.Fatalf("buildProxyRequest() error = %v", err)
	}
	if request.URL.String() != "https://api.anthropic.com/v1/messages?beta=true&client=true" {
		t.Fatalf("URL = %s", request.URL)
	}
	if headerValueFold(request.Header, "Authorization") != "Bearer oauth-access" ||
		headerValueFold(request.Header, "User-Agent") != "claude-cli/2.1.220 (external, cli)" ||
		headerValueFold(request.Header, "X-Configured") != "" ||
		!strings.Contains(headerValueFold(request.Header, "Anthropic-Beta"), "oauth-2025-04-20") ||
		!strings.Contains(headerValueFold(request.Header, "Anthropic-Beta"), "extended-cache-ttl-2025-04-11") {
		t.Fatalf("headers = %v", request.Header)
	}
	if !strings.HasPrefix(gjson.GetBytes(reqCtx.translatedBody, "system.0.text").String(), "x-anthropic-billing-header:") {
		t.Fatalf("translated body = %s", reqCtx.translatedBody)
	}
}

func TestAnthropicAPIKeyAuthenticationUsesOfficialOriginBoundary(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		official bool
	}{
		{name: "default HTTPS", target: "https://api.anthropic.com/v1/messages", official: true},
		{name: "HTTPS 443", target: "https://api.anthropic.com:443/v1/messages", official: true},
		{name: "HTTP", target: "http://api.anthropic.com/v1/messages"},
		{name: "custom port", target: "https://api.anthropic.com:8443/v1/messages"},
		{name: "userinfo", target: "https://caller@api.anthropic.com/v1/messages"},
		{name: "lookalike", target: "https://api.anthropic.com.example/v1/messages"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, test.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			injectAPIKeyHeaders(request, "sk-ant", util.ProtocolAnthropic)
			if got := request.Header.Get("x-api-key"); got != "sk-ant" {
				t.Fatalf("x-api-key=%q", got)
			}
			gotAuthorization := request.Header.Get("Authorization")
			if test.official && gotAuthorization != "" {
				t.Fatalf("official Authorization=%q, want empty", gotAuthorization)
			}
			if !test.official && gotAuthorization != "Bearer sk-ant" {
				t.Fatalf("compatible gateway Authorization=%q", gotAuthorization)
			}
		})
	}
}
