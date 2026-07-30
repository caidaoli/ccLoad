package app

import (
	"testing"

	"ccLoad/internal/model"
)

func TestDetectionLogFromResult_AllowsNilConfig(t *testing.T) {
	t.Parallel()

	entry := detectionLogFromResult(nil, model.LogSourceManualTest, "request-model", "actual-model", "sk-test", "127.0.0.1", "", map[string]any{
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
	entry := detectionLogFromResult(cfg, model.LogSourceManualTest, "mimo-v2.5", "", "sk-test", "", "", map[string]any{
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

func TestDetectionLogFromResult_UsesRequestThinkingEffort(t *testing.T) {
	t.Parallel()

	entry := detectionLogFromResult(nil, model.LogSourceManualTest, "gpt-5.5", "", "sk-test", "", "High", map[string]any{
		"status_code": 200,
		"message":     "API测试成功",
	})

	if entry.ThinkingEffort != "high" {
		t.Fatalf("thinking_effort=%q, want high", entry.ThinkingEffort)
	}
}

func TestSelectScheduledCheckModel(t *testing.T) {
	t.Parallel()

	t.Run("empty falls back to concrete wildcard redirect", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{ModelEntries: []model.ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}}}
		got, reason := selectScheduledCheckModel(cfg)
		if reason != "" || got != "glm-5.2" {
			t.Fatalf("got (%q,%q), want (glm-5.2,\"\")", got, reason)
		}
	})

	t.Run("empty with passthrough wildcard errors", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{ModelEntries: []model.ModelEntry{{Model: "gpt-*"}}}
		got, reason := selectScheduledCheckModel(cfg)
		if reason == "" || got != "" {
			t.Fatalf("got (%q,%q), want error (cannot send gpt-* verbatim)", got, reason)
		}
	})

	t.Run("nonempty wildcard match allowed", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*"}},
			ScheduledCheckModel: "gpt-4.1",
		}
		got, reason := selectScheduledCheckModel(cfg)
		if reason != "" || got != "gpt-4.1" {
			t.Fatalf("got (%q,%q), want (gpt-4.1,\"\")", got, reason)
		}
	})

	t.Run("nonempty unsupported rejected", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*"}},
			ScheduledCheckModel: "claude-x",
		}
		got, reason := selectScheduledCheckModel(cfg)
		if reason == "" || got != "" {
			t.Fatalf("got (%q,%q), want error", got, reason)
		}
	})

	t.Run("nonempty wildcard literal rejected", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*"}},
			ScheduledCheckModel: "gpt-*",
		}
		got, reason := selectScheduledCheckModel(cfg)
		if reason == "" || got != "" {
			t.Fatalf("got (%q,%q), want error (wildcard literal not allowed as check model)", got, reason)
		}
	})
}

func TestReconcileScheduledCheckModel_Wildcard(t *testing.T) {
	t.Parallel()

	t.Run("supported_by_wildcard_retained", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}},
			ScheduledCheckModel: "gpt-4.1",
		}
		changed := reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if changed || cfg.ScheduledCheckModel != "gpt-4.1" {
			t.Fatalf("expected retained gpt-4.1 (supported by wildcard), got changed=%v model=%q", changed, cfg.ScheduledCheckModel)
		}
	})

	t.Run("redirect_target_not_mapped_to_wildcard_literal", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}},
			ScheduledCheckModel: "glm-5.2",
		}
		changed := reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if cfg.ScheduledCheckModel != "" || !changed {
			t.Fatalf("expected cleared \"\" + changed=true, got %q changed=%v", cfg.ScheduledCheckModel, changed)
		}
	})

	t.Run("unsupported_cleared", func(t *testing.T) {
		t.Parallel()
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*"}},
			ScheduledCheckModel: "claude-x",
		}
		changed := reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if !changed || cfg.ScheduledCheckModel != "" {
			t.Fatalf("expected cleared, got changed=%v model=%q", changed, cfg.ScheduledCheckModel)
		}
	})

	t.Run("precise_redirect_target_remapped_to_entry_model", func(t *testing.T) {
		t.Parallel()
		// 非通配既有纠错行为保留：原值=条目重定向目标，映射到条目 Model
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-4", RedirectModel: "gpt-4-new"}},
			ScheduledCheckModel: "gpt-4-new",
		}
		reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if cfg.ScheduledCheckModel != "gpt-4" {
			t.Fatalf("expected remap to gpt-4, got %q", cfg.ScheduledCheckModel)
		}
	})

	t.Run("wildcard_literal_cleared", func(t *testing.T) {
		t.Parallel()
		// 存量通配字面 ScheduledCheckModel="gpt-*" 应被前置守卫清空
		cfg := &model.Config{
			ModelEntries:        []model.ModelEntry{{Model: "gpt-*", RedirectModel: "glm-5.2"}},
			ScheduledCheckModel: "gpt-*",
		}
		changed := reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if cfg.ScheduledCheckModel != "" || !changed {
			t.Fatalf("expected cleared \"\" + changed=true for wildcard literal, got %q changed=%v", cfg.ScheduledCheckModel, changed)
		}
	})

	t.Run("wildcard_then_concrete_same_redirect_remaps_to_concrete", func(t *testing.T) {
		t.Parallel()
		// 多条目共享同一 RedirectModel 时，通配条目命中后 continue，后续具体条目应被映射，
		// 保留用户显式设置而非清空。
		cfg := &model.Config{
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-*", RedirectModel: "glm-5.2"},
				{Model: "gpt-4", RedirectModel: "glm-5.2"},
			},
			ScheduledCheckModel: "glm-5.2",
		}
		changed := reconcileScheduledCheckModel(cfg, modelNormalizationOptions{})
		if cfg.ScheduledCheckModel != "gpt-4" || !changed {
			t.Fatalf("expected remap to concrete gpt-4, got %q changed=%v", cfg.ScheduledCheckModel, changed)
		}
	})
}

func TestDetectionLogFromResult_UpstreamThinkingEffortOverridesRequest(t *testing.T) {
	t.Parallel()

	entry := detectionLogFromResult(nil, model.LogSourceManualChat, "gpt-5.5", "", "sk-test", "", "low", map[string]any{
		"status_code": 200,
		"api_response": map[string]any{
			"response": map[string]any{
				"reasoning": map[string]any{
					"effort": "xhigh",
				},
			},
		},
		"message": "ok",
	})

	if entry.ThinkingEffort != "xhigh" {
		t.Fatalf("thinking_effort=%q, want xhigh", entry.ThinkingEffort)
	}
}
