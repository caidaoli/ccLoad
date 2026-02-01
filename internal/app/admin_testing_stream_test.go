package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

		// 模拟Claude风格SSE：usage在message_start/message_delta给出，内容在content_block_delta给出
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_read_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}}\n\n")

		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")

		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}\n\n")

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

	result := srv.testChannelAPI(cfg, "sk-test", req)

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
}
