package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func runHandleSuccessResponse(t *testing.T, body string, headers http.Header, isStreaming bool, channelType string) (*fwResult, string) {
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
	res, _, err := s.handleResponse(reqCtx, resp, rec, channelType, cfg, "sk-test", nil)
	if err != nil {
		t.Fatalf("handleResponse returned error: %v", err)
	}

	return res, rec.Body.String()
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
	firstByte := 0.0

	res, _, err := s.handleTranslatedStreamSuccessResponse(reqCtx, resp, resp.Header.Clone(), rec, "openai", readStats, &firstByte)
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

	firstByte := 1.234
	res, _, err := s.handleErrorResponse(reqCtx, resp, http.Header{}, &firstByte)
	if err != nil {
		t.Fatalf("expected err=nil, got %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Fatalf("expected Status=%d, got %d", http.StatusForbidden, res.Status)
	}
	if got := string(res.Body); got != `{"error":"余额不足"}` {
		t.Fatalf("expected Body preserved, got %q", got)
	}
	if res.FirstByteTime != firstByte {
		t.Fatalf("expected FirstByteTime=%.3f, got %.3f", firstByte, res.FirstByteTime)
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
