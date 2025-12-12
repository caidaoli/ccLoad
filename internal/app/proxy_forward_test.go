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

// TestHandleSuccessResponse_StreamDiagMsg_NoUsage 测试流式请求无usage时设置诊断消息
func TestHandleSuccessResponse_StreamDiagMsg_NoUsage(t *testing.T) {
	// 模拟流式响应但没有usage数据（anthropic渠道应该有usage）
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

	// anthropic渠道流式请求无usage应该设置诊断消息
	if res.StreamDiagMsg == "" {
		t.Error("expected StreamDiagMsg to be set for anthropic streaming without usage")
	}
	if !strings.Contains(res.StreamDiagMsg, "流响应不完整") {
		t.Errorf("StreamDiagMsg should contain '流响应不完整', got: %s", res.StreamDiagMsg)
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

// TestBuildStreamDiagnostics_HasUsageNoError 验证有usage时即使有streamErr也不触发诊断
func TestBuildStreamDiagnostics_HasUsageNoError(t *testing.T) {
	tests := []struct {
		name        string
		streamErr   error
		hasUsage    bool
		channelType string
		wantDiag    bool
		reason      string
	}{
		{
			name:        "http2_closed_with_usage",
			streamErr:   errors.New("http2: response body closed"),
			hasUsage:    true,
			channelType: "anthropic",
			wantDiag:    false,
			reason:      "有usage说明流已完整，http2关闭是正常结束",
		},
		{
			name:        "http2_closed_without_usage",
			streamErr:   errors.New("http2: response body closed"),
			hasUsage:    false,
			channelType: "anthropic",
			wantDiag:    true,
			reason:      "无usage时http2关闭是异常中断",
		},
		{
			name:        "unexpected_eof_with_usage",
			streamErr:   errors.New("unexpected EOF"),
			hasUsage:    true,
			channelType: "anthropic",
			wantDiag:    false,
			reason:      "有usage说明数据完整，EOF可能是正常关闭",
		},
		{
			name:        "stream_error_with_usage",
			streamErr:   errors.New("stream error: stream ID 7; INTERNAL_ERROR"),
			hasUsage:    true,
			channelType: "codex",
			wantDiag:    false,
			reason:      "codex渠道有usage也不应触发诊断",
		},
		{
			name:        "no_error_no_usage",
			streamErr:   nil,
			hasUsage:    false,
			channelType: "anthropic",
			wantDiag:    true,
			reason:      "anthropic无usage应触发'流响应不完整'诊断",
		},
		{
			name:        "no_error_no_usage_openai",
			streamErr:   nil,
			hasUsage:    false,
			channelType: "openai",
			wantDiag:    false,
			reason:      "openai不检查usage，无诊断",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readStats := &streamReadStats{totalBytes: 1024, readCount: 4}
			diag := buildStreamDiagnostics(tt.streamErr, readStats, tt.hasUsage, tt.channelType, "text/event-stream")

			hasDiag := diag != ""
			if hasDiag != tt.wantDiag {
				t.Errorf("%s: got diag=%q, wantDiag=%v", tt.reason, diag, tt.wantDiag)
			}
		})
	}
}
