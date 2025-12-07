package app

import (
	"context"
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
