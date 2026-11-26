package util

import "strings"

// IsClaudeModel reports whether the model name identifies a Claude model.
func IsClaudeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

// IsClaudeThinkingModel checks if the model is a Claude thinking model
// that requires the interleaved-thinking beta header.
func IsClaudeThinkingModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "claude") && strings.Contains(lower, "thinking")
}

// ResolveClaude45ModelAlias returns the dated Claude 4.5 ID for an official short name.
func ResolveClaude45ModelAlias(model string) string {
	switch model {
	case "claude-sonnet-4-5":
		return "claude-sonnet-4-5-20250929"
	case "claude-opus-4-5":
		return "claude-opus-4-5-20251101"
	case "claude-haiku-4-5":
		return "claude-haiku-4-5-20251001"
	default:
		return model
	}
}

// ResolveClaudeThinkingModelAlias returns the catalog ID for thinking capability lookup.
func ResolveClaudeThinkingModelAlias(model string) string {
	switch model {
	case "claude-opus-4-1":
		return "claude-opus-4-1-20250805"
	case "claude-opus-4", "claude-opus-4-0":
		return "claude-opus-4-20250514"
	case "claude-sonnet-4", "claude-sonnet-4-0":
		return "claude-sonnet-4-20250514"
	case "claude-3-7-sonnet-latest":
		return "claude-3-7-sonnet-20250219"
	default:
		return ResolveClaude45ModelAlias(model)
	}
}

// IsClaudeLegacySystemReminderModel reports whether the model rejects
// mid-conversation system messages and requires the legacy reminder path.
func IsClaudeLegacySystemReminderModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	switch model {
	case "claude-3-5-haiku-20241022", "claude-3-5-haiku-latest",
		"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest",
		"claude-haiku-4-5", "claude-haiku-4-5-20251001",
		"claude-opus-4", "claude-opus-4-20250514", "claude-opus-4-1",
		"claude-opus-4-1-20250805", "claude-opus-4-5", "claude-opus-4-5-20251101",
		"claude-opus-4-6", "claude-opus-4-7", "claude-sonnet-4",
		"claude-sonnet-4-20250514", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929",
		"claude-sonnet-4-6":
		return true
	default:
		return false
	}
}
