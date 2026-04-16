package protocol_test

import (
	"testing"

	"ccLoad/internal/protocol"
)

func TestBuildTransformPlan_SupportsGeminiToAnthropic(t *testing.T) {
	t.Parallel()

	plan, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.Anthropic,
		"/v1beta/models/gemini-2.5-pro:generateContent",
		"/v1/messages",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		nil,
		"gemini-2.5-pro",
		"claude-3-5-sonnet",
		false,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyGenerateContent {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildTransformPlan_SupportsGeminiToCodex(t *testing.T) {
	t.Parallel()

	plan, err := protocol.BuildTransformPlan(
		protocol.Gemini,
		protocol.Codex,
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent",
		"/v1/responses",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		nil,
		"gemini-2.5-pro",
		"gpt-5-codex",
		true,
	)
	if err != nil {
		t.Fatalf("BuildTransformPlan failed: %v", err)
	}
	if !plan.NeedsTransform || plan.RequestFamily != protocol.RequestFamilyGenerateContent || !plan.Streaming {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}
