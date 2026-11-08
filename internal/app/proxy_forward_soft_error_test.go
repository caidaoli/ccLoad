package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

/**
 * @brief 通过响应处理入口运行启用成功正文规则的流式转发
 * @param ctx 请求上下文
 * @param body 上游响应体
 * @param writer 下游响应写入端
 * @param upstreamProtocol 上游协议
 * @return 转发结果，包含规则命中和提交状态
 * @return 上游读取或下游写入错误
 */
func handleSuccessBodyRuleResponse(ctx context.Context, body io.ReadCloser, writer http.ResponseWriter, upstreamProtocol string) (*fwResult, error) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	reqCtx := &requestContext{ctx: ctx, startTime: time.Now(), isStreaming: true}
	cfg := &model.Config{ID: 1, CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Low balance", StatusCodes: []int{http.StatusOK},
		MessagePattern: "^余额不够这次调用。当前余额", Scope: model.CooldownScopeKey,
		Mode: model.CooldownModeFixed, CooldownSeconds: 90,
	}}}}
	result, _, err := (&Server{}).handleResponse(reqCtx, resp, writer, upstreamProtocol, cfg, "test-key", nil)
	return result, err
}

/**
 * @brief 上游暂停时探测窗口仍会放行首段，续传字节与用量完整保留
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleReleasesPausedStream(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	recorder := newAsyncResponseRecorder()
	done := make(chan error, 1)
	var result *fwResult
	go func() {
		var err error
		result, err = handleSuccessBodyRuleResponse(context.Background(), reader, recorder, "openai")
		done <- err
	}()
	first := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"normal answer\"}}]}\n\n"
	if _, err := io.WriteString(writer, first); err != nil {
		t.Fatalf("write first upstream chunk: %v", err)
	}
	select {
	case <-recorder.flushes:
		if got := recorder.BodyString(); got != first {
			t.Fatalf("first visible response = %q, want %q while upstream is paused", got, first)
		}
	case err := <-done:
		t.Fatalf("stream ended before upstream completion: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first text was not released after the 250ms probe window while upstream was paused")
	}
	tail := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，继续回答\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n" +
		"data: [DONE]\n\n"
	for i := range len(tail) {
		if _, err := io.WriteString(writer, tail[i:i+1]); err != nil {
			t.Fatalf("write continuation byte %d: %v", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish upstream: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleResponse() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not finish after upstream EOF")
	}
	if result == nil || result.ConfiguredDetection != nil || !result.ResponseCommitted || result.StreamDiagMsg != "" {
		t.Fatalf("result = %+v, want a committed complete response without a rule match", result)
	}
	if got := recorder.BodyString(); got != first+tail {
		t.Fatalf("forwarded body = %q, want exact upstream bytes %q", got, first+tail)
	}
	if result.InputTokens != 9 || result.OutputTokens != 4 {
		t.Fatalf("usage = %d/%d, want 9/4", result.InputTokens, result.OutputTokens)
	}
}

/**
 * @brief 前缀读取的错误不能被探测吞掉，也不能把带读取错误的文本转为冷却命中
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRulePreservesReadError(t *testing.T) {
	readErr := errors.New("upstream interrupted during success probe")
	matching := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"余额不够这次调用。当前余额：0\"}}]}\n\n"
	normal := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"normal answer\"}}]}\n\n"
	for _, tc := range []struct {
		name string        ///< 读取错误位置
		body io.ReadCloser ///< 包含可识别读取错误的上游响应体
		want string        ///< 错误前应完整转发的原始字节
	}{
		{name: "without_data", body: &probeErrorReadCloser{err: readErr}},
		{name: "same_read_as_matching_text", body: &probeErrorReadCloser{data: []byte(matching), err: readErr}, want: matching},
		{name: "after_text", body: io.NopCloser(io.MultiReader(strings.NewReader(normal), &errorReader{err: readErr})), want: normal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newRecorder()
			result, err := handleSuccessBodyRuleResponse(context.Background(), tc.body, recorder, "openai")
			if !errors.Is(err, readErr) {
				t.Fatalf("handleResponse() error = %v, want %v", err, readErr)
			}
			if result == nil || result.ConfiguredDetection != nil {
				t.Fatalf("result = %+v, want no configured cooldown match", result)
			}
			if got := recorder.Body.String(); got != tc.want {
				t.Fatalf("forwarded body = %q, want %q", got, tc.want)
			}
		})
	}
}

/**
 * @brief 请求取消关闭上游后，探测中的待读取及时退出且不产生冷却匹配
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleCancellationUnblocksRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := newBlockingReadCloser()
	defer func() { _ = blocked.Close() }()
	first := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"余额不够这次\"}}]}\n\n"
	body := readerWithCloser{Reader: io.MultiReader(strings.NewReader(first), blocked), Closer: blocked}
	stop := context.AfterFunc(ctx, func() { _ = blocked.Close() })
	defer stop()
	recorder := newAsyncResponseRecorder()
	done := make(chan error, 1)
	var result *fwResult
	go func() {
		var err error
		result, err = handleSuccessBodyRuleResponse(ctx, body, recorder, "openai")
		done <- err
	}()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("upstream did not enter the pending read after the first text event")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handleResponse() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release the pending success probe read")
	}
	if result == nil || result.ConfiguredDetection != nil || result.ResponseCommitted {
		t.Fatalf("result = %+v, want an uncommitted cancellation without a rule match", result)
	}
	if got := recorder.BodyString(); got != "" {
		t.Fatalf("cancelled response body = %q, want no output", got)
	}
}

/**
 * @brief 仅在原始前缀 4KiB 内匹配正文，超出边界的文字原样透传
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleRawPrefixLimit(t *testing.T) {
	text := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"余额不够这次调用。当前余额：0\"}}]}\n\n"
	terminal := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":5,\"total_tokens\":13}}\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		name        string ///< 文本所处边界
		prefixBytes int    ///< 文本前的原始注释字节数
		wantMatch   bool   ///< 是否应在下游提交前命中
	}{
		{name: "text_ends_at_4KiB", prefixBytes: 4096 - len(text), wantMatch: true},
		{name: "text_begins_after_4KiB", prefixBytes: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := ":" + strings.Repeat(" ", tc.prefixBytes-3) + "\n\n" + text + terminal
			recorder := newRecorder()
			result, err := handleSuccessBodyRuleResponse(context.Background(), io.NopCloser(strings.NewReader(body)), recorder, "openai")
			if err != nil {
				t.Fatalf("handleResponse() error = %v", err)
			}
			if result == nil || (result.ConfiguredDetection != nil) != tc.wantMatch {
				t.Fatalf("result = %+v, want configured match %v", result, tc.wantMatch)
			}
			if tc.wantMatch {
				if result.ResponseCommitted || recorder.Body.Len() != 0 {
					t.Fatalf("matched response committed output: result=%+v body=%q", result, recorder.Body.String())
				}
				return
			}
			if !result.ResponseCommitted || result.Status != http.StatusOK || recorder.Body.String() != body {
				t.Fatalf("late match changed response: result=%+v body=%q", result, recorder.Body.String())
			}
			if result.InputTokens != 8 || result.OutputTokens != 5 {
				t.Fatalf("usage = %d/%d, want 8/5", result.InputTokens, result.OutputTokens)
			}
		})
	}
}

/**
 * @brief 各上游协议的正文增量可以跨 SSE 事件命中成功响应冷却规则
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleMatchesProtocolText(t *testing.T) {
	for _, tc := range []struct {
		name     string ///< 上游响应形状
		protocol string ///< 上游协议
		body     string ///< 含跨事件错误文本的成功 SSE
	}{
		{
			name: "anthropic", protocol: "anthropic",
			body: "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg1","role":"assistant","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"余额不够这次"}}` + "\n\n" +
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"调用。当前余额：0"}}` + "\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":3}}` + "\n\n",
		},
		{
			name: "responses", protocol: "codex",
			body: "event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","delta":"余额不够这次"}` + "\n\n" +
				"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","delta":"调用。当前余额：0"}` + "\n\n" +
				"event: response.completed\ndata: " + `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}` + "\n\n",
		},
		{
			name: "gemini", protocol: "gemini",
			body: "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"余额不够这次"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"调用。当前余额：0"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}` + "\n\n",
		},
		{
			name: "openai_empty_tool_calls", protocol: "openai",
			body: "data: " + `{"choices":[{"index":0,"delta":{"content":"余额不够这次调用。当前余额：0","tool_calls":[],"function_call":null}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
		},
		{
			name: "openai_null_tool_calls", protocol: "openai",
			body: "data: " + `{"choices":[{"index":0,"delta":{"content":"余额不够这次调用。当前余额：0","tool_calls":null,"function_call":null}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newRecorder()
			result, err := handleSuccessBodyRuleResponse(context.Background(), io.NopCloser(strings.NewReader(tc.body)), recorder, tc.protocol)
			if err != nil {
				t.Fatalf("handleResponse() error = %v", err)
			}
			if result == nil || result.ConfiguredDetection == nil || result.Status != util.StatusSSEError || result.UpstreamStatus != http.StatusOK || result.ResponseCommitted {
				t.Fatalf("result = %+v, want an uncommitted configured failure preserving upstream 200", result)
			}
			if result.ConfiguredDetection.Scope != model.CooldownScopeKey || !result.ConfiguredDetection.Actionable {
				t.Fatalf("rule match = %+v, want actionable key cooldown", result.ConfiguredDetection)
			}
			if got := gjson.GetBytes(result.Body, "message").String(); got != "余额不够这次调用。当前余额：0" {
				t.Fatalf("matched message = %q, want joined text deltas", got)
			}
			if result.InputTokens != 7 || result.OutputTokens != 3 {
				t.Fatalf("matched attempt usage = %d/%d, want already received 7/3", result.InputTokens, result.OutputTokens)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("matched response reached downstream: %q", recorder.Body.String())
			}
		})
	}
}

/**
 * @brief 工具调用或思考输出出现后放行，后面的错误样式回答不能再触发正文规则
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleStopsAfterNonTextOutput(t *testing.T) {
	text := "余额不够这次调用。当前余额：0"
	openAIText := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", text)
	anthropicText := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":%q}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", text)
	responsesText := fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n", text)
	geminiText := fmt.Sprintf("data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":%q}]},\"finishReason\":\"STOP\"}]}\n\n", text)
	for _, tc := range []struct {
		name     string ///< 非文本输出类型
		protocol string ///< 上游协议
		prefix   string ///< 先于回答的非文本语义输出
		tail     string ///< 后续包含错误样式回答的完整终态
	}{
		{name: "openai_tool", protocol: "openai", prefix: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n", tail: openAIText},
		{name: "openai_reasoning", protocol: "openai", prefix: "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n", tail: openAIText},
		{name: "anthropic_tool", protocol: "anthropic", prefix: "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call1\",\"name\":\"lookup\",\"input\":{}}}\n\n", tail: anthropicText},
		{name: "anthropic_thinking", protocol: "anthropic", prefix: "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thinking\"}}\n\n", tail: anthropicText},
		{
			name: "responses_tool", protocol: "codex",
			prefix: "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc1","call_id":"call1","name":"lookup","arguments":""}}` + "\n\n" +
				"event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{}"}` + "\n\n" +
				"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg1","role":"assistant","content":[]}}` + "\n\n",
			tail: responsesText,
		},
		{
			name: "responses_reasoning", protocol: "codex",
			prefix: "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}` + "\n\n" +
				"event: response.reasoning_summary_text.delta\ndata: " + `{"type":"response.reasoning_summary_text.delta","item_id":"rs1","summary_index":0,"delta":"thinking"}` + "\n\n" +
				"event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg1","role":"assistant","content":[]}}` + "\n\n",
			tail: responsesText,
		},
		{name: "gemini_tool", protocol: "gemini", prefix: "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{}}}]}}]}\n\n", tail: geminiText},
		{name: "gemini_thinking", protocol: "gemini", prefix: "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"thought\":true,\"text\":\"thinking\"}]}}]}\n\n", tail: geminiText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.prefix + tc.tail
			recorder := newRecorder()
			result, err := handleSuccessBodyRuleResponse(context.Background(), io.NopCloser(strings.NewReader(body)), recorder, tc.protocol)
			if err != nil {
				t.Fatalf("handleResponse() error = %v", err)
			}
			if result == nil || result.ConfiguredDetection != nil || result.Status != http.StatusOK || !result.ResponseCommitted {
				t.Fatalf("result = %+v, want an ordinary committed success after non-text output", result)
			}
			if got := recorder.Body.String(); got != body {
				t.Fatalf("forwarded body = %q, want %q", got, body)
			}
		})
	}
}

/**
 * @brief OAuth 渠道跳过无独立 Key 的成功正文规则，显式渠道规则仍可命中
 * @param t 测试上下文
 */
func TestHandleResponse_SuccessBodyRuleOAuthScope(t *testing.T) {
	body := "data: " + `{"choices":[{"index":0,"delta":{"content":"余额不够这次调用。当前余额：0"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\ndata: [DONE]\n\n"
	for _, scope := range []string{model.CooldownScopeKey, model.CooldownScopeChannel} {
		t.Run(scope, func(t *testing.T) {
			cfg := &model.Config{
				ID: 1, AuthType: model.AuthTypeCodexOAuth,
				CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Low balance", StatusCodes: []int{http.StatusOK},
					MessagePattern: "^余额不够这次调用。当前余额", Scope: scope,
					Mode: model.CooldownModeFixed, CooldownSeconds: 90,
				}}},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			}
			reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now(), isStreaming: true}
			recorder := newRecorder()
			result, _, err := (&Server{}).handleResponse(reqCtx, resp, recorder, "openai", cfg, "", nil)
			if err != nil || result == nil {
				t.Fatalf("handleResponse() = %+v, %v", result, err)
			}
			if scope == model.CooldownScopeKey {
				if result.ConfiguredDetection != nil || result.Status != http.StatusOK || !result.ResponseCommitted || recorder.Body.String() != body {
					t.Fatalf("OAuth key rule changed successful response: result=%+v body=%q", result, recorder.Body.String())
				}
				return
			}
			if result.ConfiguredDetection == nil || result.ConfiguredDetection.Scope != scope || result.Status != util.StatusSSEError || result.ResponseCommitted || recorder.Body.Len() != 0 {
				t.Fatalf("OAuth channel rule did not stop response before output: result=%+v body=%q", result, recorder.Body.String())
			}
		})
	}
}

func TestCheckSoftError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		data        []byte
		want        bool
	}{
		{
			name:        "json_top_level_type_error",
			contentType: "application/json; charset=utf-8",
			data:        []byte(`{"type":"error","message":"boom"}`),
			want:        true,
		},
		{
			name:        "json_top_level_error_field",
			contentType: "application/json",
			data:        []byte(`{"error":{"message":"boom"}}`),
			want:        true,
		},
		{
			name:        "json_success_contains_keywords_should_not_match",
			contentType: "application/json",
			data:        []byte(`{"type":"message","content":"api_error 当前模型负载过高"}`),
			want:        false,
		},
		{
			name:        "json_truncated_object_should_not_guess",
			contentType: "application/json",
			data:        []byte(`{"type":"error"`),
			want:        false,
		},
		{
			name:        "json_content_type_but_plain_text_prefix_can_match",
			contentType: "application/json",
			data:        []byte("当前模型负载过高，请稍后再试"),
			want:        true,
		},
		{
			name:        "text_plain_prefix_short_match",
			contentType: "text/plain; charset=utf-8",
			data:        []byte("当前模型负载过高，请稍后再试"),
			want:        true,
		},
		{
			name:        "text_plain_contains_but_not_prefix_should_not_match",
			contentType: "text/plain",
			data:        []byte("回答里提到 当前模型负载过高 但这不是错误"),
			want:        false,
		},
		{
			name:        "text_plain_sse_should_not_match",
			contentType: "text/plain",
			data:        []byte("data: {\"type\":\"message\"}\n\n"),
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := checkSoftError(tt.data, tt.contentType); got != tt.want {
				t.Fatalf("checkSoftError()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldCheckSoftErrorForUpstreamProtocol(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		upstreamProtocol string
		want             bool
	}{
		{name: "anthropic", upstreamProtocol: "anthropic", want: true},
		{name: "codex", upstreamProtocol: "codex", want: true},
		{name: "empty", upstreamProtocol: "", want: false},
		{name: "openai", upstreamProtocol: "openai", want: false},
		{name: "gemini", upstreamProtocol: "gemini", want: false},
		{name: "unknown", upstreamProtocol: "something", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldCheckSoftErrorForUpstreamProtocol(tt.upstreamProtocol); got != tt.want {
				t.Fatalf("shouldCheckSoftErrorForUpstreamProtocol()=%v, want %v", got, tt.want)
			}
		})
	}
}
