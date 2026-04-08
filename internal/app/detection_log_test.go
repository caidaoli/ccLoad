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
