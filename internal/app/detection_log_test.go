package app

import (
	"testing"

	"ccLoad/internal/model"
)

func TestDetectionLogFromResult_AllowsNilConfig(t *testing.T) {
	t.Parallel()

	entry := detectionLogFromResult(nil, model.LogSourceManualTest, "request-model", "actual-model", "sk-test", "127.0.0.1", 42, map[string]any{
		"status_code":            200,
		"duration_ms":            int64(1500),
		"first_byte_duration_ms": int64(250),
		"cost_usd":               1.25,
		"message":                "ok",
	})

	if entry == nil {
		t.Fatal("expected non-nil log entry")
	}
	if entry.ChannelID != 0 {
		t.Fatalf("expected zero channel id for nil config, got %d", entry.ChannelID)
	}
	if entry.ActualModel != "actual-model" {
		t.Fatalf("expected actual model to be preserved, got %q", entry.ActualModel)
	}
	if entry.Message != "ok" {
		t.Fatalf("expected message to be preserved, got %q", entry.Message)
	}
}

func TestDetectionLogFromResult_NormalizesOpenAIChatMixedUsage(t *testing.T) {
	t.Parallel()

	cfg := &model.Config{
		ID:          212,
		ChannelType: "openai",
	}
	entry := detectionLogFromResult(cfg, model.LogSourceManualTest, "mimo-v2.5", "", "sk-test", "", 0, map[string]any{
		"status_code": 200,
		"api_response": map[string]any{
			"usage": map[string]any{
				"prompt_tokens":     float64(1340),
				"completion_tokens": float64(357),
				"prompt_tokens_details": map[string]any{
					"cached_tokens": float64(24576),
				},
				"input_tokens":  float64(0),
				"output_tokens": float64(0),
			},
		},
		"message": "API测试成功",
	})

	if entry.InputTokens != 1340 {
		t.Fatalf("expected normalized input tokens 1340, got %d", entry.InputTokens)
	}
	if entry.OutputTokens != 357 {
		t.Fatalf("expected normalized output tokens 357, got %d", entry.OutputTokens)
	}
	if entry.CacheReadInputTokens != 24576 {
		t.Fatalf("expected cache read tokens 24576, got %d", entry.CacheReadInputTokens)
	}
}
