package app

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	"github.com/google/uuid"
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
		CodexAccessToken: "at-secret", CodexAccountID: "account-1", CodexAccountFedRAMP: true,
		CustomRequestRules: &model.CustomRequestRules{Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionOverride, Name: "Authorization", Value: "Bearer attacker"},
			{Action: model.RuleActionOverride, Name: "User-Agent", Value: "attacker"},
			{Action: model.RuleActionOverride, Name: "X-Configured", Value: "kept"},
		}, Body: []model.CustomBodyRule{
			{Action: model.RuleActionOverride, Path: "service_tier", Value: json.RawMessage(`"ultrafast"`)},
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
	if got := req.Header.Get("X-OpenAI-FedRAMP"); got != "true" {
		t.Fatalf("X-OpenAI-FedRAMP = %q", got)
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
	if got := gjson.GetBytes(wireBody, "service_tier").String(); got != "ultrafast" {
		t.Fatalf("service_tier = %q, want custom rule to survive Codex normalization; body=%s", got, wireBody)
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

// 上游给出 finish_reason 就是 OpenAI 的语义终态，[DONE] 只是可选尾巴。
// 客户端常在这一刻断开，此时数据已完整，必须记 200 并计费，而不是 499。
func TestOpenAIStreamCompleteWithoutDoneMarkerSurvivesClientCancel(t *testing.T) {
	t.Parallel()

	chunk := func(payload string) string { return "data: " + payload + "\n\n" }
	partial := chunk(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
	complete := partial +
		chunk(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		chunk(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":84932,"completion_tokens":2347,"total_tokens":87279,"prompt_tokens_details":{"cached_tokens":83968}}}`)

	// 上游数据读完后客户端断开：读取以 context.Canceled 收尾，且始终没有 [DONE]。
	newBody := func(sse string) io.ReadCloser {
		return io.NopCloser(io.MultiReader(
			strings.NewReader(sse),
			iotest.ErrReader(context.Canceled),
		))
	}

	t.Run("finish_reason without done marker", func(t *testing.T) {
		t.Parallel()
		reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), isStreaming: true}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newBody(complete),
		}
		result, _, err := (&Server{}).handleSuccessResponse(
			reqCtx, resp, resp.Header.Clone(), newRecorder(), string(protocol.OpenAI), &streamReadStats{}, nil,
		)
		if err != nil {
			t.Fatalf("上游已给出 finish_reason，客户端取消不得判为失败: %v", err)
		}
		if result.Status != http.StatusOK {
			t.Fatalf("status = %d, want %d", result.Status, http.StatusOK)
		}
		if result.OutputTokens != 2347 {
			t.Fatalf("usage 未计入: %#v", result)
		}
		if result.StreamDiagMsg != "" {
			t.Fatalf("流已完整不得写诊断（会被判为 599）: %q", result.StreamDiagMsg)
		}
	})

	t.Run("cancel before finish_reason", func(t *testing.T) {
		t.Parallel()
		reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), isStreaming: true}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newBody(partial),
		}
		_, _, err := (&Server{}).handleSuccessResponse(
			reqCtx, resp, resp.Header.Clone(), newRecorder(), string(protocol.OpenAI), &streamReadStats{}, nil,
		)
		if err == nil {
			t.Fatal("未见终态就取消必须保留失败语义，交给 499 路径")
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
	assertFieldOrder(t, text, `"model"`, `"include"`, `"input"`)
}

func TestResponsesRetryBodyForMissingRequiredParameter_DropsInputItem(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep-user"}]},
			{"type":"message","id":"msg_item_empty","role":"assistant","content":[{"type":"output_text"}],"status":"completed"},
			{"type":"agent_message","content":[{"type":"input_text","text":"follow-up"}]}
		]
	}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"code":"missing_required_parameter","message":"Missing required parameter: 'input[1].content[0].text'.","param":"input[1].content[0].text","type":"invalid_request_error"}}`),
	}
	plan := protocol.TransformPlan{TranslatedBody: body}

	got, strategy, ok := responsesRetryBodyForMissingRequiredParameter(plan, res)
	if !ok {
		t.Fatal("responsesRetryBodyForMissingRequiredParameter returned ok=false")
	}
	if strategy != stripMissingRequiredInputStrategy {
		t.Fatalf("strategy=%q, want %s", strategy, stripMissingRequiredInputStrategy)
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 {
		t.Fatalf("input items=%d body=%s, want 2", len(items), got)
	}
	if items[0].Get("role").String() != "user" || items[0].Get("content.0.text").String() != "keep-user" {
		t.Fatalf("user item lost: %s", got)
	}
	if items[1].Get("type").String() != "agent_message" || items[1].Get("content.0.text").String() != "follow-up" {
		t.Fatalf("follow-up item lost: %s", got)
	}
	assertFieldOrder(t, string(got), `"model"`, `"input"`)
}

func TestResponsesRetryBodyForMissingRequiredParameter_IgnoresNonMatchingErrors(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}]}`)
	plan := protocol.TransformPlan{TranslatedBody: body}

	cases := []struct {
		name string
		res  *fwResult
	}{
		{
			name: "not 400",
			res:  &fwResult{Status: http.StatusUnprocessableEntity, Body: []byte(`{"error":{"code":"missing_required_parameter","param":"input[0].content[0].text"}}`)},
		},
		{
			name: "other code",
			res:  &fwResult{Status: http.StatusBadRequest, Body: []byte(`{"error":{"code":"unsupported_parameter","param":"input[0].content[0].text"}}`)},
		},
		{
			name: "committed",
			res: &fwResult{
				Status: http.StatusBadRequest, ResponseCommitted: true,
				Body: []byte(`{"error":{"code":"missing_required_parameter","param":"input[0].content[0].text"}}`),
			},
		},
		{
			name: "param not input",
			res:  &fwResult{Status: http.StatusBadRequest, Body: []byte(`{"error":{"code":"missing_required_parameter","param":"reasoning.effort"}}`)},
		},
		{
			name: "index out of range",
			res:  &fwResult{Status: http.StatusBadRequest, Body: []byte(`{"error":{"code":"missing_required_parameter","param":"input[9].content[0].text"}}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := responsesRetryBodyForMissingRequiredParameter(plan, tc.res); ok {
				t.Fatalf("%s: expected no retry", tc.name)
			}
		})
	}
}

func TestRetryBodyForRejectedRequest_StripsMissingRequiredInput(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]},{"type":"message","role":"assistant","content":[{"type":"output_text"}]}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"code":"missing_required_parameter","message":"Missing required parameter: 'input[1].content[0].text'."}}`),
	}
	plan := protocol.TransformPlan{TranslatedBody: body}

	got, strategy, ok := retryBodyForRejectedRequest(protocol.OpenAI, nil, plan, res)
	if !ok {
		t.Fatal("retryBodyForRejectedRequest returned ok=false")
	}
	if strategy != stripMissingRequiredInputStrategy {
		t.Fatalf("strategy=%q, want %s", strategy, stripMissingRequiredInputStrategy)
	}
	if gjson.GetBytes(got, "input.#").Int() != 1 {
		t.Fatalf("expected one remaining input item, got %s", got)
	}
}

func TestAnthropicRetryBodyFor400PreservesOrderWhileDowngradingThinking(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-6","thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"keep this","cache_control":{"type":"ephemeral"}}]}],"output_config":{"effort":"high","format":{"type":"json"}},"metadata":{"keep":true}}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"type":"invalid_request_error","message":"thinking blocks are not supported"}}`),
	}

	got, strategy, ok := anthropicRetryBodyFor400(protocol.Anthropic, protocol.TransformPlan{TranslatedBody: body}, res)
	if !ok || strategy != "downgrade_anthropic_thinking" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "thinking").Exists() || gjson.GetBytes(got, "output_config.effort").Exists() {
		t.Fatalf("thinking controls survived downgrade: %s", got)
	}
	if gotFormat := gjson.GetBytes(got, "output_config.format.type").String(); gotFormat != "json" {
		t.Fatalf("output_config.format.type = %q, body=%s", gotFormat, got)
	}
	if blockType := gjson.GetBytes(got, "messages.0.content.0.type").String(); blockType != "text" {
		t.Fatalf("thinking block type = %q, body=%s", blockType, got)
	}
	assertFieldOrder(t, string(got), `"model"`, `"messages"`, `"output_config"`, `"metadata"`)
}

func TestNormalizeAnthropicMessagesBodyPatchesOnlyChangedMembers(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"thinking":{"type":"adaptive","budget_tokens":4096},"metadata":{"z":1,"a":2}}`)

	got, err := normalizeAnthropicMessagesBody(body)
	if err != nil {
		t.Fatalf("normalizeAnthropicMessagesBody() error = %v", err)
	}
	if effort := gjson.GetBytes(got, "output_config.effort").String(); effort != "medium" {
		t.Fatalf("output_config.effort = %q, body=%s", effort, got)
	}
	if gjson.GetBytes(got, "thinking.budget_tokens").Exists() {
		t.Fatalf("budget_tokens survived normalization: %s", got)
	}
	assertFieldOrder(t, string(got), `"model"`, `"messages"`, `"thinking"`, `"metadata"`, `"output_config"`)
	assertFieldOrder(t, gjson.GetBytes(got, "metadata").Raw, `"z"`, `"a"`)
}
func TestNormalizeAnthropicMessagesBodySanitizesOpaqueThinkingSignatureOnCanonicalBody(t *testing.T) {
	t.Parallel()
	// body 已满足 normalizeAnthropicMessagesRequest 的全部字段条件：
	// adaptive thinking 无 budget_tokens、cache_control 已存在、无采样字段，
	// normalize 不会改写任何成员。但 assistant 的 thinking block 携带 Claude-compatible
	// 上游不接受的外来 signature（opaque-deepseek-id）——canonical body 也必须被清洗。
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":4096,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"foreign reasoning","signature":"opaque-deepseek-id"},{"type":"text","text":"answer"}]}]}`)

	got, err := normalizeAnthropicMessagesBody(body)
	if err != nil {
		t.Fatalf("normalizeAnthropicMessagesBody() error = %v", err)
	}
	// 外来 signature 不兼容 Claude 上游，thinking block 必须整体删除，
	// 相邻的普通 text block 保持不变。
	if strings.Contains(string(got), "opaque-deepseek-id") {
		t.Fatalf("opaque-signature thinking block survived sanitization: %s", got)
	}
	blocks := gjson.GetBytes(got, "messages.1.content")
	if len(blocks.Array()) != 1 || blocks.Get("0.type").String() != "text" || blocks.Get("0.text").String() != "answer" {
		t.Fatalf("assistant content = %s, want single text block after sanitization", blocks.Raw)
	}
}

// Anthropic 改写全程用 sjson 就地写字节，而 sjson 对截断/带尾随数据的输入会静默
// 返回损坏结果且 err == nil。入口这一次语法校验是唯一防线，必须拒绝非法 body。
func TestNormalizeAnthropicMessagesBodyRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"model":"claude-opus-4-6"} {"unexpected":true}`,
		`{"model":"claude-opus-4-6"`,
		`[{"model":"claude-opus-4-6"}]`,
		``,
	} {
		if _, err := normalizeAnthropicMessagesBody([]byte(body)); err == nil {
			t.Fatalf("normalizeAnthropicMessagesBody accepted malformed body %q", body)
		}
	}
}

func TestAnthropicToolResultTextPreservesScalarContent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: `{"tool_use_id":"toolu_1","content":123}`, want: "\n123"},
		{input: `{"tool_use_id":"toolu_1","content":true}`, want: "\ntrue"},
	} {
		got := anthropicToolResultText(gjson.Parse(test.input))
		if !strings.HasSuffix(got, test.want) {
			t.Fatalf("scalar tool result lost: input=%s got=%q", test.input, got)
		}
	}
}

func TestRectifyAnthropicThinkingBudgetRejectsFractionalTokenCounts(t *testing.T) {
	t.Parallel()
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":32000.5},"max_tokens":64000.5}`)
	if got, ok := rectifyAnthropicThinkingBudget(body); !ok || gjson.GetBytes(got, "thinking.budget_tokens").Int() != 32000 || gjson.GetBytes(got, "max_tokens").Int() != 64000 {
		t.Fatalf("fractional token counts were not repaired exactly: ok=%v body=%s", ok, got)
	}
}

func TestDowngradeAnthropicThinkingBlocksRemovesEmptyOutputConfig(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-6","output_config":{"effort":"high"},"messages":[]}`)
	got, ok := downgradeAnthropicThinkingBlocks(body)
	if !ok {
		t.Fatal("expected thinking downgrade to apply")
	}
	if gjson.GetBytes(got, "output_config").Exists() {
		t.Fatalf("empty output_config survived: %s", got)
	}
}

func TestAntigravitySignatureRetryReturnsOrderedInnerRequest(t *testing.T) {
	t.Parallel()
	body := []byte(`{"project":"p","model":"claude-sonnet-4-6","request":{"generationConfig":{"thinkingConfig":{"includeThoughts":true},"keep":1},"contents":[{"role":"model","parts":[{"thought":true,"text":"keep thought","thoughtSignature":"sig"},{"text":"answer"}]}],"z":1}}`)

	got, strategy, ok := antigravitySignatureRetryBody(body, []byte(`{"error":{"message":"invalid thought signature"}}`), http.StatusBadRequest)
	if !ok || strategy != "strip_antigravity_thinking" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "request").Exists() {
		t.Fatalf("retry body must be the inner request: %s", got)
	}
	if gjson.GetBytes(got, "generationConfig.thinkingConfig").Exists() {
		t.Fatalf("thinkingConfig survived retry: %s", got)
	}
	if gotKeep := gjson.GetBytes(got, "generationConfig.keep").Int(); gotKeep != 1 {
		t.Fatalf("generationConfig.keep = %d, body=%s", gotKeep, got)
	}
	if gotText := gjson.GetBytes(got, "contents.0.parts.0.text").String(); gotText != "keep thought" {
		t.Fatalf("downgraded thought text = %q, body=%s", gotText, got)
	}
	assertFieldOrder(t, string(got), `"generationConfig"`, `"contents"`, `"z"`)
}

func TestAntigravityGeminiSignatureRetryPatchesNestedSignaturesInPlace(t *testing.T) {
	t.Parallel()
	body := []byte(`{"project":"p","model":"gemini-2.5-pro","request":{"contents":[{"parts":[{"text":"answer","thoughtSignature":"original"}]}],"metadata":{"z":1,"a":2}}}`)

	got, strategy, ok := antigravitySignatureRetryBody(body, []byte(`{"error":{"message":"thought signature is invalid"}}`), http.StatusBadRequest)
	if !ok || strategy != "replace_antigravity_thought_signatures" {
		t.Fatalf("retry = (%q, %v), body=%s", strategy, ok, got)
	}
	if signature := gjson.GetBytes(got, "contents.0.parts.0.thoughtSignature").String(); signature != "skip_thought_signature_validator" {
		t.Fatalf("thoughtSignature = %q, body=%s", signature, got)
	}
	assertFieldOrder(t, string(got), `"contents"`, `"metadata"`)
	assertFieldOrder(t, gjson.GetBytes(got, "metadata").Raw, `"z"`, `"a"`)
}
func TestReplaceAntigravityThoughtSignaturesAlreadySkipValidatorIsNoOp(t *testing.T) {
	t.Parallel()
	// thoughtSignature 已是 skip-validator 哨兵时不得再报告变更：
	// 重复替换会触发无意义的 retry 循环。
	body := []byte(`{"contents":[{"parts":[{"text":"answer","thoughtSignature":"skip_thought_signature_validator"}]}]}`)

	got, changed := replaceAntigravityThoughtSignatures(body)
	if changed {
		t.Fatalf("already-skip-validator body reported changed: %s", got)
	}
	if got != nil {
		if signature := gjson.GetBytes(got, "contents.0.parts.0.thoughtSignature").String(); signature != "skip_thought_signature_validator" {
			t.Fatalf("sentinel must be preserved: %s", got)
		}
	}
}

func TestRetryBodyForRejectedRequest_StripsUnknownInputStatus(t *testing.T) {
	t.Parallel()
	// 两个 item 都带 status：一次性剥离全部，而不是只删上游点名的单个路径。
	body := []byte(`{"input":[{"type":"function_call","id":"fc_0","call_id":"call_0","name":"exec","arguments":"{}","status":"completed"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","arguments":"{}","status":"completed"}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"message":"Unknown parameter: 'input[1].status'. (request id: 202608290103048262047936468c0eZAfBH9NY)","type":"invalid_request_error","param":"input[1].status","code":"unknown_parameter"}}`),
	}
	plan := protocol.TransformPlan{
		ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex,
		RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: body,
	}
	got, strategy, ok := retryBodyForRejectedRequest(protocol.Codex, nil, plan, res)
	if !ok {
		t.Fatal("retryBodyForRejectedRequest returned ok=false")
	}
	if strategy != stripUnknownInputParameterStrategy {
		t.Fatalf("strategy=%q, want %q", strategy, stripUnknownInputParameterStrategy)
	}
	if gjson.GetBytes(got, "input.#").Int() != 2 {
		t.Fatalf("unknown-parameter retry dropped an input item: %s", got)
	}
	if gjson.GetBytes(got, "input.0.status").Exists() || gjson.GetBytes(got, "input.1.status").Exists() {
		t.Fatalf("status survived retry body: %s", got)
	}
	if gjson.GetBytes(got, "input.0.call_id").String() != "call_0" ||
		gjson.GetBytes(got, "input.1.call_id").String() != "call_1" {
		t.Fatalf("function_call lost fields: %s", got)
	}
}

func TestResponsesRetryBodyForUnknownParameter_Guards(t *testing.T) {
	t.Parallel()
	bodyWithStatus := []byte(`{"input":[{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}","status":"completed"}]}`)
	bodyWithoutStatus := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}]}`)
	tests := []struct {
		name      string
		body      []byte
		resStatus int
		errorBody []byte
		wantOK    bool
	}{
		{
			name:      "code unknown_parameter",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].status"}}`),
			wantOK:    true,
		},
		{
			name:      "code unsupported_parameter",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unsupported_parameter","param":"input[0].status"}}`),
			wantOK:    true,
		},
		{
			name:      "message only fallback",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"message":"Unknown parameter: 'input[0].status'."}}`),
			wantOK:    true,
		},
		{
			name:      "no status in body is noop",
			body:      bodyWithoutStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0]","message":"Unknown parameter: 'input[0]'."}}`),
			wantOK:    false,
		},
		{
			name:      "different unknown parameter does not replay",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].metadata"}}`),
			wantOK:    false,
		},
		{
			name:      "nested status path does not replay",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].status.detail"}}`),
			wantOK:    false,
		},
		{
			name:      "structured status prefix does not replay",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].status/child"}}`),
			wantOK:    false,
		},
		{
			name:      "thinking error delegated to strip_codex_thinking",
			body:      bodyWithStatus,
			resStatus: http.StatusBadRequest,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].reasoning","message":"Unknown parameter: 'input[0].reasoning'."}}`),
			wantOK:    false,
		},
		{
			name:      "non bad request does not retry",
			body:      bodyWithStatus,
			resStatus: http.StatusInternalServerError,
			errorBody: []byte(`{"error":{"code":"unknown_parameter","param":"input[0].status"}}`),
			wantOK:    false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := &fwResult{Status: tt.resStatus, Body: tt.errorBody}
			plan := protocol.TransformPlan{
				ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex,
				RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: tt.body,
			}
			got, strategy, ok := responsesRetryBodyForUnknownParameter(protocol.Codex, plan, res)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if strategy != stripUnknownInputParameterStrategy {
				t.Fatalf("strategy=%q", strategy)
			}
			if gjson.GetBytes(got, "input.0.status").Exists() {
				t.Fatalf("status survived retry body: %s", got)
			}
			if gjson.GetBytes(got, "input.0.call_id").String() != "call_1" {
				t.Fatalf("function_call lost fields: %s", got)
			}
		})
	}
}

func TestResponsesRetryBodyForUnknownParameter_RequiresCodexResponsesScope(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"function_call","status":"completed"}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"code":"unknown_parameter","param":"input[0].status"}}`),
	}
	tests := []struct {
		name     string
		upstream protocol.Protocol
		plan     protocol.TransformPlan
	}{
		{
			name:     "non Codex upstream",
			upstream: protocol.OpenAI,
			plan: protocol.TransformPlan{
				ClientProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: body,
			},
		},
		{
			name:     "non Codex client",
			upstream: protocol.Codex,
			plan: protocol.TransformPlan{
				ClientProtocol: protocol.OpenAI, RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: body,
			},
		},
		{
			name:     "non Responses request",
			upstream: protocol.Codex,
			plan: protocol.TransformPlan{
				ClientProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyChatCompletions, TranslatedBody: body,
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := responsesRetryBodyForUnknownParameter(tc.upstream, tc.plan, res); ok {
				t.Fatal("out-of-scope request must not be replayed")
			}
		})
	}
}

func TestResponsesBodyForHTTPTransport_StripsInputItemStatus(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-5.6-sol","seed":9007199254740993,"input":[{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}","status":"completed"},{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}]}`)
	want := []byte(`{"model":"gpt-5.6-sol","seed":9007199254740993,"input":[{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}]}`)
	plan := protocol.TransformPlan{
		ClientProtocol:   protocol.Codex,
		UpstreamProtocol: protocol.Codex,
		RequestFamily:    protocol.RequestFamilyResponses,
	}
	got := responsesBodyForHTTPTransport(&model.Config{}, plan, body)
	if gjson.GetBytes(got, "input.0.status").Exists() {
		t.Fatalf("HTTP Codex body kept input status: %s", got)
	}
	if gjson.GetBytes(got, "input.0.call_id").String() != "call_1" {
		t.Fatalf("HTTP Codex body lost function_call: %s", got)
	}
	if gjson.GetBytes(got, "seed").Raw != "9007199254740993" {
		t.Fatalf("HTTP Codex body changed large integer: %s", got)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("HTTP Codex body changed unrelated serialized bytes:\n got: %s\nwant: %s", got, want)
	}

	plan.UpstreamProtocol = protocol.OpenAI
	got = responsesBodyForHTTPTransport(&model.Config{}, plan, body)
	if !gjson.GetBytes(got, "input.0.status").Exists() {
		t.Fatalf("non-Codex upstream body lost input status: %s", got)
	}
}

func TestStripResponsesInputItemStatusPreservesTranscriptSerialization(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"function_call","id":"fc_0","call_id":"call_0","name":"exec","arguments":"{\"large\":9007199254740993}","status":"completed"},{"status":"completed","type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call","status":"in_progress","id":"fc_2","call_id":"call_2","name":"exec","arguments":"{}"},{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"},{"type":"metadata","status":"nested-keep"}]}],"metadata":{"status":"top-level-keep"},"seed":9007199254740993}`)
	want := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"function_call","id":"fc_0","call_id":"call_0","name":"exec","arguments":"{\"large\":9007199254740993}"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call","id":"fc_2","call_id":"call_2","name":"exec","arguments":"{}"},{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"},{"type":"metadata","status":"nested-keep"}]}],"metadata":{"status":"top-level-keep"},"seed":9007199254740993}`)

	for range 100 {
		got := stripResponsesInputItemStatus(body)
		if !bytes.Equal(got, want) {
			t.Fatalf("status stripping changed the serialized transcript prefix:\n got: %s\nwant: %s", got, want)
		}
	}
}

func TestStripResponsesInputItemStatusLeavesInvalidJSONUntouched(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"function_call","status":"completed"}`)
	if got := stripResponsesInputItemStatus(body); !bytes.Equal(got, body) {
		t.Fatalf("invalid JSON changed: got %s", got)
	}
}

func TestStripResponsesInputItemStatusPreservesUnrelatedWhitespace(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{ "status" : "completed" , "type" : "function_call" },{"type":"function_call" , "status" : "completed" },{"status":null}]}`)
	want := []byte(`{"input":[{  "type" : "function_call" },{"type":"function_call"  },{}]}`)
	if got := stripResponsesInputItemStatus(body); !bytes.Equal(got, want) {
		t.Fatalf("status stripping changed unrelated whitespace:\n got: %s\nwant: %s", got, want)
	}
}

func BenchmarkStripResponsesInputItemStatus(b *testing.B) {
	for _, itemCount := range []int{1, 100, 1000} {
		var body strings.Builder
		body.WriteString(`{"input":[`)
		for i := range itemCount {
			if i > 0 {
				body.WriteByte(',')
			}
			fmt.Fprintf(&body, `{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"exec","arguments":"{}","status":"completed"}`, i, i)
		}
		body.WriteString(`]}`)
		payload := []byte(body.String())

		b.Run(fmt.Sprintf("items_%d", itemCount), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				if got := stripResponsesInputItemStatus(payload); len(got) >= len(payload) {
					b.Fatal("status fields were not removed")
				}
			}
		})
	}
}

func TestResponsesRetryBodyForMissingStoredInputItem_StripsNamedReasoning(t *testing.T) {
	t.Parallel()
	const missingID = "rs_item_813dd000e22bc4aa5ed48884"
	body := []byte(`{"store":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]},` +
		`{"type":"reasoning","id":"` + missingID + `","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":null},` +
		`{"type":"message","id":"msg_item_keep","role":"assistant","content":[{"type":"output_text","text":"ok"}]}` +
		`]}`)
	errorEvent := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":null,"message":"Item with id '` + missingID + `' not found. Items are not persisted when store is set to false.","param":"input"},"status":404}`)
	plan := protocol.TransformPlan{TranslatedBody: body}

	got, strategy, ok := responsesRetryBodyForMissingStoredInputItem(plan, &fwResult{
		Status:        http.StatusOK,
		SSEErrorEvent: errorEvent,
	})
	if !ok {
		t.Fatal("expected SSE 404 missing-item retry")
	}
	if strategy != stripMissingStoredInputItemStrategy+":"+missingID {
		t.Fatalf("strategy=%q", strategy)
	}
	if gjson.GetBytes(got, "input.#").Int() != 2 {
		t.Fatalf("expected two remaining input items, got %s", got)
	}
	if bytes.Contains(got, []byte(missingID)) {
		t.Fatalf("missing stored item survived retry body: %s", got)
	}
	if gjson.GetBytes(got, "input.1.id").String() != "msg_item_keep" {
		t.Fatalf("unrelated item lost: %s", got)
	}

	got, strategy, ok = retryBodyForRejectedRequest(protocol.Codex, nil, plan, &fwResult{
		Status: http.StatusNotFound,
		Body:   errorEvent,
	})
	if !ok {
		t.Fatal("retryBodyForRejectedRequest returned ok=false for HTTP 404")
	}
	if strategy != stripMissingStoredInputItemStrategy+":"+missingID {
		t.Fatalf("strategy=%q", strategy)
	}
	if gjson.GetBytes(got, "input.#").Int() != 2 {
		t.Fatalf("HTTP 404 retry body=%s", got)
	}
}

func TestResponsesRetryBodyForMissingStoredInputItem_IgnoresNonMatchingErrors(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_item_813dd000e22bc4aa5ed48884","summary":[]}]}`)
	plan := protocol.TransformPlan{TranslatedBody: body}
	missing := []byte(`{"error":{"message":"Item with id 'rs_item_813dd000e22bc4aa5ed48884' not found"}}`)

	cases := []struct {
		name string
		res  *fwResult
	}{
		{
			name: "committed",
			res:  &fwResult{Status: http.StatusNotFound, ResponseCommitted: true, Body: missing},
		},
		{
			name: "other status",
			res:  &fwResult{Status: http.StatusTooManyRequests, Body: missing},
		},
		{
			name: "previous_response_not_found",
			res: &fwResult{
				Status: http.StatusBadRequest,
				Body:   []byte(`{"error":{"code":"previous_response_not_found","message":"No response found for previous_response_id resp-1"}}`),
			},
		},
		{
			name: "id not in body",
			res: &fwResult{
				Status: http.StatusNotFound,
				Body:   []byte(`{"error":{"message":"Item with id 'rs_item_missing' not found"}}`),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := responsesRetryBodyForMissingStoredInputItem(plan, tc.res); ok {
				t.Fatalf("%s: expected no retry", tc.name)
			}
		})
	}

	for _, itemType := range []string{"message", "function_call", "custom_tool_call"} {
		t.Run("preserve "+itemType, func(t *testing.T) {
			t.Parallel()
			const itemID = "item_must_survive"
			body := []byte(`{"input":[{"type":"` + itemType + `","id":"` + itemID + `"}]}`)
			res := &fwResult{
				Status: http.StatusNotFound,
				Body:   []byte(`{"error":{"message":"Item with id '` + itemID + `' not found"}}`),
			}
			if _, _, ok := responsesRetryBodyForMissingStoredInputItem(
				protocol.TransformPlan{TranslatedBody: body}, res,
			); ok {
				t.Fatalf("%s item must not be removed from a full replay", itemType)
			}
		})
	}
}

func TestWriteSyntheticSSEFrameRoundTripsMultilineJSON(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "code": null,
    "message": "Item with id 'rs_item_813dd000e22bc4aa5ed48884' not found",
    "param": "input"
  },
  "status": 404
}`)
	var buf bytes.Buffer
	if err := writeSyntheticSSEFrame(&buf, payload); err != nil {
		t.Fatalf("writeSyntheticSSEFrame: %v", err)
	}
	raw, ok := nextSSEEvent(&buf)
	if !ok {
		t.Fatal("expected one SSE event")
	}
	got := sseEventData(raw)
	if !gjson.ValidBytes(got) {
		t.Fatalf("reconstructed SSE payload is not JSON: %q", got)
	}
	if gjson.GetBytes(got, "type").String() != "error" || gjson.GetBytes(got, "status").Int() != 404 {
		t.Fatalf("reconstructed payload=%s", got)
	}
	id, ok := parseMissingStoredInputItemID(gjson.GetBytes(got, "error.message").String())
	if !ok || id != "rs_item_813dd000e22bc4aa5ed48884" {
		t.Fatalf("id=%q ok=%v", id, ok)
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

func TestCodexRetryBodyFor400_UsesSSEErrorStatusForEncryptedContent(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"compaction","encrypted_content":"drop-compaction"},
			{"type":"reasoning","summary":[],"encrypted_content":"drop-reasoning"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}
		]
	}`)
	res := &fwResult{
		Status:        http.StatusOK,
		SSEErrorEvent: []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."},"status":400}`),
	}
	plan := protocol.TransformPlan{TranslatedBody: body}

	got, strategy, ok := codexRetryBodyFor400(protocol.Codex, nil, plan, res)
	if !ok {
		t.Fatal("codexRetryBodyFor400 returned ok=false for an SSE 400 error")
	}
	if strategy != "strip_codex_encrypted_input" {
		t.Fatalf("strategy=%q, want strip_codex_encrypted_input", strategy)
	}
	if items := gjson.GetBytes(got, "input").Array(); len(items) != 1 || items[0].Get("type").String() != "message" {
		t.Fatalf("retry body should keep only the non-encrypted message, got %s", got)
	}
}

func TestCodexRetryBodyFor400_DoesNotRetryCommittedSSEError(t *testing.T) {
	t.Parallel()

	res := &fwResult{
		Status:            http.StatusOK,
		ResponseCommitted: true,
		SSEErrorEvent:     []byte(`{"type":"error","error":{"code":"invalid_encrypted_content"},"status":400}`),
	}
	plan := protocol.TransformPlan{TranslatedBody: []byte(`{"input":[{"type":"reasoning","encrypted_content":"drop"}]}`)}
	if _, _, ok := codexRetryBodyFor400(protocol.Codex, nil, plan, res); ok {
		t.Fatal("committed SSE error must not be retried")
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

// openai→anthropic 转换器只在 [DONE] 时吐终止事件。部分 OpenAI 兼容上游给完
// finish_reason 就断流，客户端会一直等不到 message_stop——上游语义已完整时
// 必须由网关补出完整终止序列，且不能在上游自带 [DONE] 时补重。
func TestHandleTranslatedStreamSuccessResponse_SynthesizesTerminatorWhenUpstreamOmitsDone(t *testing.T) {
	t.Parallel()

	const (
		finishChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"tool_calls\"}]}\n\n"
		usageChunk  = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n"
		doneChunk   = "data: [DONE]\n\n"
	)

	cases := []struct {
		name string
		sse  string
	}{
		{name: "finish_reason only", sse: finishChunk},
		{name: "finish_reason then usage", sse: finishChunk + usageChunk},
		{name: "upstream sends done", sse: finishChunk + usageChunk + doneChunk},
	}

	clients := []struct {
		protocol protocol.Protocol
		model    string
		terminal string
	}{
		{protocol: protocol.Anthropic, model: "claude-3-5-sonnet", terminal: "event: message_stop"},
		{protocol: protocol.Codex, model: "gpt-5-codex", terminal: "event: response.completed"},
	}

	for _, client := range clients {
		for _, tc := range cases {
			t.Run(string(client.protocol)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				reg := protocol.NewRegistry()
				builtin.Register(reg)
				s := &Server{protocolRegistry: reg}
				reqCtx := &requestContext{
					ctx:         context.Background(),
					startTime:   time.Now(),
					isStreaming: true,
					transformPlan: protocol.TransformPlan{
						ClientProtocol:   client.protocol,
						UpstreamProtocol: protocol.OpenAI,
						OriginalModel:    client.model,
						ActualModel:      "gpt-4o",
						NeedsTransform:   true,
					},
				}
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(tc.sse)),
				}

				rec := newRecorder()
				res, _, err := s.handleTranslatedStreamSuccessResponse(
					reqCtx, resp, resp.Header.Clone(), rec, string(protocol.OpenAI), &streamReadStats{}, nil,
				)
				if err != nil {
					t.Fatalf("上游语义完整不得报错: %v", err)
				}
				if res.StreamDiagMsg != "" {
					t.Fatalf("流已完整不得写诊断（会被判为 599）: %q", res.StreamDiagMsg)
				}

				body := rec.Body.String()
				if got := strings.Count(body, client.terminal); got != 1 {
					t.Fatalf("%q 出现 %d 次，want 1；body=%s", client.terminal, got, body)
				}
			})
		}
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
	body, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-sonnet-4-5","system":"answer tersely","messages":[{"role":"user","content":"hello world"}],
		"thinking":{"type":"enabled"},"tool_choice":{"type":"auto"}
	}`), cfg, "", http.Header{"User-Agent": []string{"third-party-client"}}, anthropicOfficialTestURL)
	if err != nil {
		t.Fatalf("finalizeAnthropicClaudeCodeMessagesBody(, anthropicOfficialTestURL) error = %v", err)
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
	body, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"x-anthropic-billing-header: attacker-controlled"}],
		"messages":[{"role":"user","content":"hello"}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, "", nil, anthropicOfficialTestURL)
	if err != nil {
		t.Fatalf("finalizeAnthropicClaudeCodeMessagesBody(, anthropicOfficialTestURL) error = %v", err)
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
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "3f2b7c18-9d4e-4a6b-8c51-7e0a2d9b4f36",
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
		"metadata":{"user_id":"{\"device_id\":\"%s\",\"account_uuid\":\"3f2b7c18-9d4e-4a6b-8c51-7e0a2d9b4f36\",\"session_id\":\"e03895ad-8b34-4a84-bbf6-002e8909b17b\"}"},
		"messages":[{"role":"user","content":"hello"}],"max_tokens":1024
	}`, parsedCredential.DeviceID))
	nativeHeaders := http.Header{
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"},
		"X-App":      {"cli"}, "Anthropic-Beta": {"claude-code-20250219"},
		"X-Claude-Code-Session-Id": {"e03895ad-8b34-4a84-bbf6-002e8909b17b"},
	}
	finalized, err := finalizeAnthropicClaudeCodeMessagesBody(
		nativeBody, cfg, "", nativeHeaders, anthropicOfficialTestURL,
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

	finalized, err := finalizeAnthropicClaudeCodeMessagesBody(body, cfg, "", headers, anthropicOfficialTestURL)
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
	pooled, err := finalizeAnthropicClaudeCodeMessagesBody(body, &model.Config{
		AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: otherCredentialJSON,
	}, "", headers, anthropicOfficialTestURL)
	if err != nil || !bytes.Equal(pooled, body) {
		t.Fatalf("native helper was tied to the selected pool credential: err=%v body=%s", err, pooled)
	}
	reordered := []byte(fmt.Sprintf(`{"max_tokens":1,"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"helper probe"}],"metadata":{"user_id":%q}}`, userID))
	cloaked, err := finalizeAnthropicClaudeCodeMessagesBody(reordered, cfg, "", headers, anthropicOfficialTestURL)
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
	finalized, err := finalizeAnthropicClaudeCodeMessagesBody(
		body, &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, "", headers, anthropicOfficialTestURL,
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
	autoBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-opus-4-6","messages":[{"role":"user","content":"run"}],
		"tools":[{"name":"run","description":"run","input_schema":{"type":"object"}}],
		"thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"any"}
	}`), cfg, "", nil, anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(autoBody, "thinking").Exists() || gjson.GetBytes(autoBody, "context_management").Exists() {
		t.Fatalf("forced tool choice retained invalid automatic thinking state: %s", autoBody)
	}

	callerBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-opus-4-6","messages":[{"role":"user","content":"run"}],
		"context_management":{"edits":[{"type":"caller-owned"}]}
	}`), cfg, "", nil, anthropicOfficialTestURL)
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
	body, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-opus-4-6",
		"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"second"}],
		"tools":[{"name":"search","description":"search","input_schema":{"type":"object"}}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}, "", nil, anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	// 缓存窗口归调用方所有：请求没声明 ttl，网关只打 breakpoint，不注入 1h。
	if got := gjson.GetBytes(body, "system.2.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system cache_control.type = %q, want ephemeral: %s", got, body)
	}
	if gjson.GetBytes(body, "system.2.cache_control.ttl").Exists() ||
		gjson.GetBytes(body, "messages.2.content.0.cache_control.ttl").Exists() {
		t.Fatalf("gateway must not set a cache TTL the caller did not ask for: %s", body)
	}
	if got := gjson.GetBytes(body, "messages.2.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("rolling message cache_control.type = %q, want ephemeral: %s", got, body)
	}
	if bytes.Count(body, []byte(`"cache_control":{"type":"ephemeral"}`)) != 2 {
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
	body, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"x-anthropic-billing-header: forged; cc_entrypoint=cli; cch=00000;"}],
		"metadata":{"user_id":"x"},
		"messages":[{"role":"user","content":"hello"}]
	}`), &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON},
		"", http.Header{"User-Agent": []string{"claude-cli/fake"}}, anthropicOfficialTestURL)
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

// 指纹路径清空并重建整个请求头，但渠道自定义 header 规则必须最终生效：
// 只有认证头由黑名单守住，其余头（含 CLI 身份头）允许被渠道配置改写。
func TestAnthropicOAuthBuildProxyRequestKeepsCustomHeaderRules(t *testing.T) {
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
		headerValueFold(request.Header, "User-Agent") != "attacker" ||
		headerValueFold(request.Header, "X-Configured") != "must-drop" ||
		strings.Contains(headerValueFold(request.Header, "Anthropic-Beta"), "attacker-beta") ||
		!strings.Contains(headerValueFold(request.Header, "Anthropic-Beta"), "oauth-2025-04-20") ||
		strings.Contains(headerValueFold(request.Header, "Anthropic-Beta"), "extended-cache-ttl-2025-04-11") {
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

// TestAnthropicAPIKeyFingerprintMatchesOAuthWireExceptCCH 守住凭证边界：OAuth 与
// API Key 共用 CLI wire/beta 形状，差异只落在 CCH 上——OAuth 无条件签，API Key 只在
// 第一方 origin 签（第三方网关把 billing 块当 prompt 文本，每请求变化会打散 prompt
// cache）。判据见 anthropicCCHSigningEnabled。
func TestAnthropicAPIKeyFingerprintMatchesOAuthWireExceptCCH(t *testing.T) {
	const requestBody = `{
		"model":"claude-sonnet-4-5","system":"answer tersely",
		"messages":[{"role":"user","content":"hello world"}],
		"thinking":{"type":"enabled"},"temperature":0.3
	}`
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "account-uuid", DeviceID: "device-id",
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	oauthCfg := &model.Config{AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON}
	apiKeyCfg := &model.Config{Name: "anthropic-api-key"}
	callerHeaders := http.Header{"User-Agent": []string{"third-party-client"}}

	oauthBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(requestBody), oauthCfg, "", callerHeaders, anthropicOfficialTestURL)
	if err != nil {
		t.Fatalf("OAuth finalize: %v", err)
	}
	apiKeyBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(requestBody), apiKeyCfg, "sk-ant-key", callerHeaders, anthropicOfficialTestURL)
	if err != nil {
		t.Fatalf("API key finalize: %v", err)
	}

	// 身份值必然不同（两套凭证），其余 body 结构必须完全一致。
	for _, path := range []string{"system.1.text", "system.2.text", "messages.0.content", "thinking.type",
		"context_management.edits.0.type", "system.2.cache_control"} {
		oauthValue := gjson.GetBytes(oauthBody, path).String()
		if apiKeyValue := gjson.GetBytes(apiKeyBody, path).String(); oauthValue != apiKeyValue {
			t.Fatalf("%s: OAuth=%q API key=%q", path, oauthValue, apiKeyValue)
		}
	}
	if gjson.GetBytes(apiKeyBody, "temperature").Exists() ||
		!strings.HasPrefix(gjson.GetBytes(apiKeyBody, "system.0.text").String(), "x-anthropic-billing-header:") {
		t.Fatalf("API key body did not adopt the CLI wire shape: %s", apiKeyBody)
	}
	oauthBilling := gjson.GetBytes(oauthBody, "system.0.text").String()
	apiKeyBilling := gjson.GetBytes(apiKeyBody, "system.0.text").String()
	if !strings.Contains(oauthBilling, " cch=") || strings.Contains(oauthBilling, "cch=00000;") {
		t.Fatalf("OAuth billing is not signed: %q", oauthBilling)
	}
	if !strings.Contains(apiKeyBilling, " cch=") || strings.Contains(apiKeyBilling, "cch=00000;") {
		t.Fatalf("API-key billing on first-party origin must be signed: %q", apiKeyBilling)
	}
	// 同一份请求发往第三方网关时，billing 必须保持无 cch 的稳定形态。
	thirdPartyBody, err := finalizeAnthropicClaudeCodeMessagesBody(
		[]byte(requestBody), apiKeyCfg, "sk-ant-key", callerHeaders, anthropicThirdPartyTestURL)
	if err != nil {
		t.Fatalf("third-party finalize: %v", err)
	}
	if got := gjson.GetBytes(thirdPartyBody, "system.0.text").String(); strings.Contains(got, "cch=") {
		t.Fatalf("API-key billing on a third-party gateway must stay unsigned: %q", got)
	}
	if anthropicClaudeCodeBetas(oauthBody) != anthropicClaudeCodeBetas(apiKeyBody) {
		t.Fatalf("beta sets diverged: OAuth=%q API key=%q",
			anthropicClaudeCodeBetas(oauthBody), anthropicClaudeCodeBetas(apiKeyBody))
	}

	oauthRequest, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(string(oauthBody)))
	if err != nil {
		t.Fatal(err)
	}
	apiKeyRequest, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(string(apiKeyBody)))
	if err != nil {
		t.Fatal(err)
	}
	injectAnthropicOAuthHeaders(oauthRequest, oauthCfg, "oauth-access", oauthBody, callerHeaders)
	injectAnthropicAPIKeyHeaders(apiKeyRequest, apiKeyCfg, "sk-ant-key", apiKeyBody, callerHeaders)

	if headerValueFold(apiKeyRequest.Header, "x-api-key") != "sk-ant-key" ||
		headerValueFold(apiKeyRequest.Header, "authorization") != "" {
		t.Fatalf("API key auth headers = %v", apiKeyRequest.Header)
	}
	if headerValueFold(oauthRequest.Header, "authorization") != "Bearer oauth-access" ||
		headerValueFold(oauthRequest.Header, "x-api-key") != "" {
		t.Fatalf("OAuth auth headers = %v", oauthRequest.Header)
	}
	// 认证头之外的指纹头必须逐项相同；session/request id 随请求变化，只校验非空。
	for _, name := range []string{"User-Agent", "anthropic-version", "anthropic-beta", "x-app",
		"Accept-Encoding", "X-Stainless-Runtime-Version", "X-Stainless-Package-Version", "X-Stainless-Timeout"} {
		oauthValue := headerValueFold(oauthRequest.Header, name)
		apiKeyValue := headerValueFold(apiKeyRequest.Header, name)
		if oauthValue == "" || oauthValue != apiKeyValue {
			t.Fatalf("%s: OAuth=%q API key=%q", name, oauthValue, apiKeyValue)
		}
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "x-client-request-id"} {
		if headerValueFold(apiKeyRequest.Header, name) == "" {
			t.Fatalf("API key %s is empty: %v", name, apiKeyRequest.Header)
		}
	}
}

// TestAnthropicClaudeCodeCacheTTLFollowsCaller 守住缓存窗口的归属：5m 还是 1h 由
// 原始请求决定，网关不主动升级；extended-cache-ttl-2025-04-11 与 body 里实际存在的
// cache_control.ttl 双向同源——body 用了才声明，没用就不发。
func TestAnthropicClaudeCodeCacheTTLFollowsCaller(t *testing.T) {
	cfg := &model.Config{Name: "anthropic-api-key"}
	headers := http.Header{"User-Agent": []string{"third-party-client"}}

	defaultBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]
	}`), cfg, "sk-ant-key", headers, anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if hits := gjson.GetBytes(defaultBody, `@dig:ttl`).Array(); len(hits) != 0 {
		t.Fatalf("gateway injected a cache TTL the caller did not ask for: %s", defaultBody)
	}
	if betas := anthropicClaudeCodeBetas(defaultBody); strings.Contains(betas, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("extended-cache-ttl beta declared without any cache TTL in body: %q", betas)
	}

	longBody, err := finalizeAnthropicClaudeCodeMessagesBody([]byte(`{
		"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[
			{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}
		]}]
	}`), cfg, "sk-ant-key", headers, anthropicOfficialTestURL)
	if err != nil {
		t.Fatal(err)
	}
	usesLongTTL := false
	for _, hit := range gjson.GetBytes(longBody, `@dig:ttl`).Array() {
		if hit.String() == "1h" {
			usesLongTTL = true
		}
	}
	if !usesLongTTL {
		t.Fatalf("caller-owned 1h cache TTL was dropped: %s", longBody)
	}
	// 网关注入的 system breakpoint 排在调用方 block 前面，按 Anthropic 的评估顺序，
	// 它保持 5m 就会把调用方的 1h 一起降级——跟随是保住调用方选择的唯一方式。
	if got := gjson.GetBytes(longBody, "system.2.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("gateway system breakpoint ttl=%q, want 1h: %s", got, longBody)
	}
	if betas := anthropicClaudeCodeBetas(longBody); !strings.Contains(betas, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("1h cache TTL without extended-cache-ttl beta: %q", betas)
	}
}

// TestSynthesizedAnthropicAPIKeyIdentityIsStable 保证 API Key 渠道的合成身份可复现：
// 同一个 Key 永远派生同一台「设备」，换 Key 才换身份。身份漂移会让上游把每次请求
// 当成新设备。
func TestSynthesizedAnthropicAPIKeyIdentityIsStable(t *testing.T) {
	first := synthesizeAnthropicAPIKeyCredential("sk-ant-stable")
	again := synthesizeAnthropicAPIKeyCredential("  sk-ant-stable  ")
	other := synthesizeAnthropicAPIKeyCredential("sk-ant-other")
	if first == nil || again == nil || other == nil {
		t.Fatal("synthesized credential is nil")
	}
	if first.DeviceID != again.DeviceID || first.AccountUUID != again.AccountUUID {
		t.Fatalf("identity drifted across calls: %+v vs %+v", first, again)
	}
	if first.DeviceID == other.DeviceID || first.AccountUUID == other.AccountUUID {
		t.Fatalf("distinct API keys share an identity: %+v vs %+v", first, other)
	}
	if _, err := uuid.Parse(first.AccountUUID); err != nil {
		t.Fatalf("account_uuid=%q is not a UUID: %v", first.AccountUUID, err)
	}
	if synthesizeAnthropicAPIKeyCredential("   ") != nil {
		t.Fatal("blank API key must not synthesize an identity")
	}
}

// TestZAICodingPlanSkipsClaudeCodeFingerprint 守住两套指纹的互斥：Z.ai Coding Plan
// 也走 anthropic 协议 + /v1/messages，但它有自己的 ZCode 设备指纹契约，叠加 Claude
// Code 指纹会互相破坏（ZCode 覆盖 metadata.user_id，1h cache TTL 又配不上 ZCode 的
// beta 头）。
func TestZAICodingPlanSkipsClaudeCodeFingerprint(t *testing.T) {
	tests := []struct {
		name string
		cfg  *model.Config
		want bool
	}{
		{name: "API key channel", cfg: &model.Config{Name: "anthropic"}, want: true},
		{name: "Anthropic OAuth channel", cfg: &model.Config{AuthType: model.AuthTypeAnthropicOAuth}, want: true},
		{name: "Z.ai Coding Plan channel", cfg: &model.Config{AuthType: model.AuthTypeZAIOAuth}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isAnthropicClaudeCodeMessagesRequest(test.cfg, protocol.Anthropic, "/v1/messages"); got != test.want {
				t.Fatalf("isAnthropicClaudeCodeMessagesRequest = %t, want %t", got, test.want)
			}
		})
	}
}

// TestAnthropicClaudeCodeRetryReplaysWirePerSigningPolicy 守住重试重放路径：body 已在
// 首次尝试时最终化，重放只允许按凭证/origin 决定要不要重签 CCH，不允许再跑一轮归一。
//
// 这一支是 CCH 条件化之后最容易回归的地方：判据一旦写成 isNativeAnthropicClaudeCodeRequest，
// 「本渠道不签名」产出的无 cch body 就会被网关判成非原生，自己不认自己。
func TestAnthropicClaudeCodeRetryReplaysWirePerSigningPolicy(t *testing.T) {
	headers := http.Header{"User-Agent": []string{"third-party-client"}}
	cfg := &model.Config{Name: "anthropic-api-key"}
	const requestBody = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`

	server := &Server{}
	for _, testCase := range []struct {
		name     string
		target   *url.URL
		wantSign bool
	}{
		{name: "third_party_stays_unsigned", target: anthropicThirdPartyTestURL},
		{name: "first_party_signs", target: anthropicOfficialTestURL, wantSign: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			finalized, err := finalizeAnthropicClaudeCodeMessagesBody(
				[]byte(requestBody), cfg, "sk-ant-key", headers, testCase.target)
			if err != nil {
				t.Fatal(err)
			}
			// 网关自己的产物必须通过出站身份判据，否则重放会被误判成第三方 body。
			outboundHeaders := http.Header{
				"User-Agent":               {"claude-cli/" + anthropicCLIVersion + " (external, cli)"},
				"X-App":                    {"cli"},
				"Anthropic-Beta":           {"claude-code-20250219"},
				"X-Claude-Code-Session-Id": {anthropicSessionIDFromRequest(finalized)},
			}
			if !isNativeAnthropicClaudeCodeRequest(finalized, outboundHeaders) {
				t.Fatalf("gateway-owned wire failed its own outbound identity check: %s", finalized)
			}
			replayed, err := server.prepareTranslatedUpstreamBody(
				cfg, protocol.Anthropic, "/v1/messages", finalized, finalized,
				"sk-ant-key", headers, true, testCase.target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(replayed, finalized) {
				t.Fatalf("retry replay rewrote an already finalized body:\n got %s\nwant %s", replayed, finalized)
			}
			billing := gjson.GetBytes(replayed, "system.0.text").String()
			if signed := strings.Contains(billing, " cch="); signed != testCase.wantSign {
				t.Fatalf("signed=%v want %v: %q", signed, testCase.wantSign, billing)
			}
		})
	}
}

// TestAnthropicNativeClaudeCodeWithoutCCHPassesThrough 守住入站判据不看 CCH。
//
// 下游 Claude Code 指向 ccLoad 时看到的是非第一方 base URL，native gate
// (`s = firstParty || vertex ? " cch=00000;" : ""`) 直接省略 cch，但 X-App/UA/beta/
// metadata.user_id 四个身份信号一个不少。把 ` cch=` 当必要条件会让**所有**真实
// Claude Code 请求落进重写路径：system 被重建成 CLI 三段式、客户端 system block 上
// 的 cache_control 随 anthropicSystemText 降级整段丢弃、剩余断点再被
// enforceAnthropicCacheControlLimit 裁剪——客户端自管的 prompt cache 就此失效。
func TestAnthropicNativeClaudeCodeWithoutCCHPassesThrough(t *testing.T) {
	const sessionID = "f2e293f7-b6ee-48f7-9258-95be092aae58"
	identity := fmt.Sprintf(`{"device_id":%q,"account_uuid":%q,"session_id":%q}`,
		"94a1bc03ba56d8895e3f6f33010c88d32fc9b3165576727d163261ada4af99d1",
		"00d2be77-53ea-52f8-8a66-bfc5c4b195e9", sessionID)
	body := []byte(fmt.Sprintf(`{"model":"claude-opus-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.746; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":%q},"messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"tools":[{"name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"max_tokens":1024,"temperature":0.4}`, identity))
	headers := http.Header{
		"User-Agent":               {"claude-cli/" + anthropicCLIVersion + " (external, cli)"},
		"X-App":                    {"cli"},
		"Anthropic-Beta":           {"claude-code-20250219,oauth-2025-04-20"},
		"X-Claude-Code-Session-Id": {sessionID},
	}
	cfg := &model.Config{Name: "anthropic-third-party"}

	if !isNativeAnthropicClaudeCodeRequest(body, headers) {
		t.Fatal("a real Claude Code request without cch was rejected by the native detector")
	}
	finalized, err := finalizeAnthropicClaudeCodeMessagesBody(body, cfg, "sk-ant-key", headers, anthropicThirdPartyTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finalized, body) {
		t.Fatalf("native body was rewritten:\n got %s\nwant %s", finalized, body)
	}
	// 逐项钉死重写路径最先破坏的那几处。
	if strings.Contains(string(finalized), "[System Instructions]") {
		t.Fatalf("caller system was demoted into messages: %s", finalized)
	}
	for _, path := range []string{
		"system.1.cache_control", "messages.0.content.0.cache_control", "tools.0.cache_control",
	} {
		if !gjson.GetBytes(finalized, path).Exists() {
			t.Fatalf("caller-owned %s was stripped: %s", path, finalized)
		}
	}
	if gjson.GetBytes(finalized, "metadata.user_id").String() != identity {
		t.Fatalf("caller identity was replaced with a synthesized one: %s", finalized)
	}
}

// TestAnthropicNativeClaudeCodeEmptyAccountUUIDPassesThrough 守住身份判据允许空
// account_uuid，对齐上游 isValidUserID（cloak_utils.go:52）。
//
// Claude Code 用 API Key 指向非第一方 base URL 时没有 Anthropic 账号，metadata.user_id
// 里的 account_uuid 就是空串。要求它非空会让这类真实请求全部误判成第三方调用方并被重写，
// 症状与把 ` cch=` 当必要条件时完全一致：客户端自管的 cache_control 断点被打散。
func TestAnthropicNativeClaudeCodeEmptyAccountUUIDPassesThrough(t *testing.T) {
	const sessionID = "f2e293f7-b6ee-48f7-9258-95be092aae58"
	const deviceID = "94a1bc03ba56d8895e3f6f33010c88d32fc9b3165576727d163261ada4af99d1"
	headers := http.Header{
		"User-Agent":               {"claude-cli/" + anthropicCLIVersion + " (external, cli)"},
		"X-App":                    {"cli"},
		"Anthropic-Beta":           {"claude-code-20250219"},
		"X-Claude-Code-Session-Id": {sessionID},
	}
	cfg := &model.Config{Name: "anthropic-third-party"}

	bodyFor := func(accountUUID string) []byte {
		identity := fmt.Sprintf(`{"device_id":%q,"account_uuid":%q,"session_id":%q}`,
			deviceID, accountUUID, sessionID)
		return []byte(fmt.Sprintf(`{"model":"claude-opus-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.746; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":%q},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],"max_tokens":1024}`, identity))
	}

	t.Run("empty account_uuid is native", func(t *testing.T) {
		body := bodyFor("")
		if !isNativeAnthropicClaudeCodeRequest(body, headers) {
			t.Fatal("Claude Code request with an empty account_uuid was rejected by the native detector")
		}
		finalized, err := finalizeAnthropicClaudeCodeMessagesBody(body, cfg, "sk-ant-key", headers, anthropicThirdPartyTestURL)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(finalized, body) {
			t.Fatalf("native body was rewritten:\n got %s\nwant %s", finalized, body)
		}
		for _, path := range []string{"system.1.cache_control", "messages.0.content.0.cache_control"} {
			if !gjson.GetBytes(finalized, path).Exists() {
				t.Fatalf("caller-owned %s was stripped: %s", path, finalized)
			}
		}
	})

	// 空是合法的，垃圾不是：非空 account_uuid 仍必须是 UUID，device_id 仍必须是
	// 64 位小写 hex。放宽判据不等于取消判据。
	for _, badCase := range []struct{ name, accountUUID string }{
		{"non-uuid account_uuid", "not-a-uuid"},
		{"whitespace account_uuid", " "},
	} {
		t.Run(badCase.name+" is not native", func(t *testing.T) {
			if isNativeAnthropicClaudeCodeRequest(bodyFor(badCase.accountUUID), headers) {
				t.Fatalf("account_uuid %q was accepted as a native identity", badCase.accountUUID)
			}
		})
	}

	t.Run("malformed device_id is not native", func(t *testing.T) {
		identity := fmt.Sprintf(`{"device_id":"short","account_uuid":"","session_id":%q}`, sessionID)
		body := []byte(fmt.Sprintf(`{"model":"claude-opus-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.746; cc_entrypoint=cli;"}],"metadata":{"user_id":%q},"messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`, identity))
		if isNativeAnthropicClaudeCodeRequest(body, headers) {
			t.Fatal("a malformed device_id was accepted as a native identity")
		}
	})
}

// TestValidAnthropicClaudeCLIUserAgent 钉死 UA 判据对齐上游
// claudeCodeNativeUserAgentPattern + nativeClaudeEntrypoints。
//
// 写死 " (external, cli)" 后缀会把带 Agent SDK 的 CLI 和 VSCode 扩展整条拒掉，
// 后果不是报错而是静默降级到重写路径——客户端的 prompt cache 断点被打散。
func TestValidAnthropicClaudeCLIUserAgent(t *testing.T) {
	t.Parallel()
	version := anthropicCLIVersion
	for _, testCase := range []struct {
		name      string
		userAgent string
		want      bool
	}{
		{"cli", "claude-cli/" + version + " (external, cli)", true},
		{"cli with agent sdk", "claude-cli/" + version + " (external, cli, agent-sdk/0.1.5)", true},
		{"sdk-cli", "claude-cli/" + version + " (external, sdk-cli)", true},
		{"claude-vscode", "claude-cli/" + version + " (external, claude-vscode)", true},
		{"surrounding whitespace", "  claude-cli/" + version + " (external, cli)  ", true},
		// 版本号刻意不参与判定：锁死它等于给客户端每次升级埋一颗静默降级地雷。
		{"newer version", "claude-cli/9.9.9 (external, cli)", true},
		{"older version", "claude-cli/1.0.0 (external, cli)", true},
		{"unknown entrypoint", "claude-cli/" + version + " (external, sdk-ts)", false},
		{"non-numeric version", "claude-cli/latest (external, cli)", false},
		{"missing external marker", "claude-cli/" + version + " (cli)", false},
		{"trailing junk", "claude-cli/" + version + " (external, cli) extra", false},
		{"malformed agent sdk", "claude-cli/" + version + " (external, cli, agent-sdk/x)", false},
		{"empty", "", false},
		{"unrelated client", "python-httpx/0.27.0", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := validAnthropicClaudeCLIUserAgent(testCase.userAgent); got != testCase.want {
				t.Fatalf("validAnthropicClaudeCLIUserAgent(%q) = %v, want %v",
					testCase.userAgent, got, testCase.want)
			}
		})
	}
}
