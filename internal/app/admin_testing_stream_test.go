package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

func TestTestChannelAPI_StreamIncludesUsageAndCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)

		// 模拟Claude风格SSE：usage在message_start/message_delta给出，内容在content_block_delta给出
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_read_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}}\n\n")

		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")

		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}\n\n")
		time.Sleep(20 * time.Millisecond)

		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	cfg := &model.Config{
		ID:           1,
		Name:         "test-channel",
		URL:          upstream.URL,
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-haiku", RedirectModel: ""}},
		ChannelType:  "anthropic",
		Enabled:      true,
	}

	req := &testutil.TestChannelRequest{
		Model:       "claude-3-haiku",
		Stream:      true,
		Content:     "hi",
		ChannelType: "anthropic",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected success, got: %#v", result)
	}

	if result["response_text"] != "hi" {
		t.Fatalf("expected response_text=hi, got: %#v", result["response_text"])
	}

	apiResp, ok := result["api_response"].(map[string]any)
	if !ok || apiResp == nil {
		t.Fatalf("expected api_response, got: %#v", result["api_response"])
	}

	usage, ok := apiResp["usage"].(map[string]any)
	if !ok || usage == nil {
		t.Fatalf("expected api_response.usage, got: %#v", apiResp["usage"])
	}

	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Fatalf("expected usage tokens, got: %#v", usage)
	}

	cost, ok := result["cost_usd"].(float64)
	if !ok {
		t.Fatalf("expected cost_usd(float64), got: %#v", result["cost_usd"])
	}
	if cost <= 0 {
		t.Fatalf("expected cost_usd > 0, got: %v", cost)
	}

	firstByteDurationMs, ok := result["first_byte_duration_ms"].(int64)
	if !ok || firstByteDurationMs <= 0 {
		t.Fatalf("expected first_byte_duration_ms(int64)>0, got: %#v", result["first_byte_duration_ms"])
	}

	totalDurationMs, ok := result["duration_ms"].(int64)
	if !ok || totalDurationMs <= 0 {
		t.Fatalf("expected duration_ms(int64)>0, got: %#v", result["duration_ms"])
	}
	if totalDurationMs < firstByteDurationMs {
		t.Fatalf("expected duration_ms>=first_byte_duration_ms, got %d < %d", totalDurationMs, firstByteDurationMs)
	}
}

func TestTestChannelAPI_GeminiStreamIncludesTTFBAndText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gemini 流式端点: /v1beta/models/{model}:streamGenerateContent
		if r.URL.Path != "/v1beta/models/gemini-2.5-flash-lite:streamGenerateContent" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)

		// Gemini SSE: candidates[0].content.parts[0].text, usage在usageMetadata中
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"}}],\"modelVersion\":\"gemini-2.5-flash-lite\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":20,\"totalTokenCount\":30},\"modelVersion\":\"gemini-2.5-flash-lite\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	cfg := &model.Config{
		ID:           1,
		Name:         "gemini-channel",
		URL:          upstream.URL,
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gemini-2.5-flash-lite"}},
		ChannelType:  "gemini",
		Enabled:      true,
	}

	req := &testutil.TestChannelRequest{
		Model:       "gemini-2.5-flash-lite",
		Stream:      true,
		Content:     "hi",
		ChannelType: "gemini",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "test-key", req)

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected success, got: %#v", result)
	}

	// 验证文本提取
	if result["response_text"] != "Hello world" {
		t.Fatalf("expected response_text='Hello world', got: %#v", result["response_text"])
	}

	// 验证 TTFB
	firstByteDurationMs, ok := result["first_byte_duration_ms"].(int64)
	if !ok || firstByteDurationMs <= 0 {
		t.Fatalf("expected first_byte_duration_ms(int64)>0, got: %#v", result["first_byte_duration_ms"])
	}

	// 验证总耗时
	totalDurationMs, ok := result["duration_ms"].(int64)
	if !ok || totalDurationMs <= 0 {
		t.Fatalf("expected duration_ms(int64)>0, got: %#v", result["duration_ms"])
	}
}
