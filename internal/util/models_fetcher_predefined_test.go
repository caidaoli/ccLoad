package util

import "testing"

func TestPredefinedModels_CopyAndNormalize(t *testing.T) {
	models1 := PredefinedModels("  CODEX  ")
	if len(models1) == 0 {
		t.Fatalf("expected predefined models for codex")
	}

	// 必须返回副本：外部修改不应污染全局预设列表
	models1[0] = "MUTATED"
	models2 := PredefinedModels("codex")
	if len(models2) == 0 {
		t.Fatalf("expected predefined models for codex")
	}
	if models2[0] == "MUTATED" {
		t.Fatalf("expected PredefinedModels to return a copy, got shared backing array")
	}
}

func TestPredefinedModels_UnknownReturnsNil(t *testing.T) {
	if got := PredefinedModels("unknown"); got != nil {
		t.Fatalf("expected nil for unknown protocol, got %#v", got)
	}
}
