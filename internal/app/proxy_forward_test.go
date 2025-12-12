package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleSuccessResponse_ExtractsUsageFromJSON(t *testing.T) {
	body := `{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: false,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	// 测试用的渠道信息
	testChannelID := int64(1)
	testAPIKey := "sk-test-xxx"

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec, "anthropic", &testChannelID, testAPIKey)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	if res.InputTokens != 10 || res.OutputTokens != 20 || res.CacheReadInputTokens != 5 || res.CacheCreationInputTokens != 7 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if rec.Body.String() != body {
		t.Fatalf("unexpected response body forwarded: %q", rec.Body.String())
	}
}

func TestHandleSuccessResponse_ExtractsUsageFromTextPlainSSE(t *testing.T) {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"cache_read_input_tokens\":1,\"cache_creation_input_tokens\":2}}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: true,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	// 测试用的渠道信息
	testChannelID := int64(1)
	testAPIKey := "sk-test-xxx"

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec, "anthropic", &testChannelID, testAPIKey)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	if res.InputTokens != 3 || res.OutputTokens != 4 || res.CacheReadInputTokens != 1 || res.CacheCreationInputTokens != 2 {
		t.Fatalf("unexpected usage extracted: %+v", res)
	}

	if rec.Body.String() != body {
		t.Fatalf("unexpected response body forwarded: %q", rec.Body.String())
	}
}

// TestHandleSuccessResponse_StreamDiagMsg_NormalEOF 测试正常EOF时不触发诊断
// 新逻辑：只有当 streamErr != nil 且未检测到流结束标志时才触发诊断
// 正常EOF（streamErr == nil）不触发诊断，即使没有流结束标志
func TestHandleSuccessResponse_StreamDiagMsg_NormalEOF(t *testing.T) {
	// 模拟流式响应，无流结束标志但正常EOF
	body := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: true,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	testChannelID := int64(1)
	testAPIKey := "sk-test-xxx"

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec, "anthropic", &testChannelID, testAPIKey)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	// 正常EOF不应触发诊断（新逻辑：只有 streamErr != nil 才触发）
	if res.StreamDiagMsg != "" {
		t.Errorf("expected empty StreamDiagMsg for normal EOF, got: %s", res.StreamDiagMsg)
	}
}

// TestHandleSuccessResponse_StreamDiagMsg_NonAnthropicNoUsage 测试非anthropic渠道无usage不设置诊断
func TestHandleSuccessResponse_StreamDiagMsg_NonAnthropicNoUsage(t *testing.T) {
	// 非anthropic渠道流式响应无usage是正常的
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	reqCtx := &requestContext{
		ctx:         context.Background(),
		startTime:   time.Now(),
		isStreaming: true,
	}

	rec := httptest.NewRecorder()
	s := &Server{}

	testChannelID := int64(1)
	testAPIKey := "sk-test-xxx"

	res, _, err := s.handleSuccessResponse(reqCtx, resp, 0, resp.Header.Clone(), rec, "openai", &testChannelID, testAPIKey)
	if err != nil {
		t.Fatalf("handleSuccessResponse returned error: %v", err)
	}

	// 非anthropic渠道无usage不应该设置诊断消息
	if res.StreamDiagMsg != "" {
		t.Errorf("expected empty StreamDiagMsg for non-anthropic channel, got: %s", res.StreamDiagMsg)
	}
}

// TestBuildStreamDiagnostics_StreamComplete 验证检测到流结束标志时即使有streamErr也不触发诊断
func TestBuildStreamDiagnostics_StreamComplete(t *testing.T) {
	tests := []struct {
		name           string
		streamErr      error
		streamComplete bool
		channelType    string
		wantDiag       bool
		reason         string
	}{
		{
			name:           "http2_closed_with_stream_complete",
			streamErr:      errors.New("http2: response body closed"),
			streamComplete: true,
			channelType:    "anthropic",
			wantDiag:       false,
			reason:         "检测到流结束标志，http2关闭是正常结束",
		},
		{
			name:           "http2_closed_without_stream_complete",
			streamErr:      errors.New("http2: response body closed"),
			streamComplete: false,
			channelType:    "anthropic",
			wantDiag:       true,
			reason:         "无流结束标志时http2关闭是异常中断",
		},
		{
			name:           "unexpected_eof_with_stream_complete",
			streamErr:      errors.New("unexpected EOF"),
			streamComplete: true,
			channelType:    "anthropic",
			wantDiag:       false,
			reason:         "检测到流结束标志，EOF可能是正常关闭",
		},
		{
			name:           "stream_error_with_stream_complete",
			streamErr:      errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			streamComplete: true,
			channelType:    "codex",
			wantDiag:       false,
			reason:         "codex渠道检测到流结束标志也不应触发诊断",
		},
		{
			name:           "no_error_no_stream_complete",
			streamErr:      nil,
			streamComplete: false,
			channelType:    "anthropic",
			wantDiag:       false,
			reason:         "无错误时不触发诊断（正常EOF情况）",
		},
		{
			name:           "no_error_with_stream_complete",
			streamErr:      nil,
			streamComplete: true,
			channelType:    "openai",
			wantDiag:       false,
			reason:         "无错误且有流结束标志，无诊断",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readStats := &streamReadStats{totalBytes: 1024, readCount: 4}
			diag := buildStreamDiagnostics(tt.streamErr, readStats, tt.streamComplete, tt.channelType, "text/event-stream")

			hasDiag := diag != ""
			if hasDiag != tt.wantDiag {
				t.Errorf("%s: got diag=%q, wantDiag=%v", tt.reason, diag, tt.wantDiag)
			}
		})
	}
}


// errorReader 模拟返回特定错误的 Reader
type errorReader struct {
	err error
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

// TestStreamCopySSE_ContextCanceledDuringRead 测试在 Read 期间 context 被取消的场景
// 场景：客户端取消请求 → HTTP/2 流关闭 → Read 返回 "http2: response body closed"
// 期望：返回 context.Canceled 而非原始错误，让上层正确识别为客户端断开（499）
func TestStreamCopySSE_ContextCanceledDuringRead(t *testing.T) {
	tests := []struct {
		name        string
		readErr     error
		ctxCanceled bool
		wantErr     error
		reason      string
	}{
		{
			name:        "http2_closed_with_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，应返回 context.Canceled 而非 http2 错误",
		},
		{
			name:        "http2_closed_without_ctx_canceled",
			readErr:     errors.New("http2: response body closed"),
			ctxCanceled: false,
			wantErr:     errors.New("http2: response body closed"),
			reason:      "context 未取消时，应返回原始错误",
		},
		{
			name:        "stream_error_with_ctx_canceled",
			readErr:     errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，stream error 也应转换为 context.Canceled",
		},
		{
			name:        "network_error_with_ctx_canceled",
			readErr:     errors.New("connection reset by peer"),
			ctxCanceled: true,
			wantErr:     context.Canceled,
			reason:      "context 已取消时，网络错误应转换为 context.Canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.ctxCanceled {
				cancel() // 模拟客户端取消
			} else {
				defer cancel()
			}

			// 创建模拟 Reader 返回指定错误
			reader := &errorReader{err: tt.readErr}
			recorder := httptest.NewRecorder()

			// 调用 streamCopySSE
			err := streamCopySSE(ctx, reader, recorder, nil)

			if tt.ctxCanceled {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s: got err=%v, want context.Canceled", tt.reason, err)
				}
			} else {
				if err == nil || err.Error() != tt.readErr.Error() {
					t.Errorf("%s: got err=%v, want %v", tt.reason, err, tt.readErr)
				}
			}
		})
	}
}

// TestStreamCopy_ContextCanceledDuringRead 测试非 SSE 流复制在 Read 期间 context 被取消的场景
func TestStreamCopy_ContextCanceledDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端取消

	reader := &errorReader{err: errors.New("http2: response body closed")}
	recorder := httptest.NewRecorder()

	err := streamCopy(ctx, reader, recorder, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("streamCopy should return context.Canceled when ctx is canceled, got: %v", err)
	}
}
