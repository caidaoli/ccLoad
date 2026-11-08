package util

import "testing"

func TestIsClaudeThinkingModel(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		expected bool
	}{
		// Claude thinking models - should return true
		{"claude-sonnet-4-5-thinking", "claude-sonnet-4-5-thinking", true},
		{"claude-opus-4-5-thinking", "claude-opus-4-5-thinking", true},
		{"claude-opus-4-6-thinking", "claude-opus-4-6-thinking", true},
		{"Claude-Sonnet-Thinking uppercase", "Claude-Sonnet-4-5-Thinking", true},
		{"claude thinking mixed case", "Claude-THINKING-Model", true},

		// Non-thinking Claude models - should return false
		{"claude-sonnet-4-5 (no thinking)", "claude-sonnet-4-5", false},
		{"claude-opus-4-5 (no thinking)", "claude-opus-4-5", false},
		{"claude-3-5-sonnet", "claude-3-5-sonnet-20240620", false},

		// Non-Claude models - should return false
		{"gemini-3-pro-preview", "gemini-3-pro-preview", false},
		{"gemini-thinking model", "gemini-3-pro-thinking", false}, // not Claude
		{"gpt-4o", "gpt-4o", false},
		{"empty string", "", false},

		// Edge cases
		{"thinking without claude", "thinking-model", false},
		{"claude without thinking", "claude-model", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsClaudeThinkingModel(tt.model)
			if result != tt.expected {
				t.Errorf("IsClaudeThinkingModel(%q) = %v, expected %v", tt.model, result, tt.expected)
			}
		})
	}
}

func TestIsClaudeModel(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		expected bool
	}{
		{"Claude model", "claude-sonnet-4-6", true},
		{"Claude thinking model", "claude-opus-4-6-thinking", true},
		{"case insensitive", "Claude-Sonnet-4-5", true},
		{"Gemini model", "gemini-3-pro-preview", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaudeModel(tt.model); got != tt.expected {
				t.Errorf("IsClaudeModel(%q) = %v, expected %v", tt.model, got, tt.expected)
			}
		})
	}
}

func TestIsClaudeLegacySystemReminderModel(t *testing.T) {
	for _, model := range []string{
		"claude-3-5-haiku-20241022", "claude-3-5-haiku-latest",
		"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest",
		"claude-haiku-4-5", "claude-haiku-4-5-20251001",
		"claude-opus-4", "claude-opus-4-20250514", "claude-opus-4-1",
		"claude-opus-4-1-20250805", "claude-opus-4-5", "claude-opus-4-5-20251101",
		"claude-opus-4-6", "claude-opus-4-7", "claude-sonnet-4",
		"claude-sonnet-4-20250514", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929", "claude-sonnet-4-6",
		" provider/Claude-Sonnet-4-5 ",
	} {
		if !IsClaudeLegacySystemReminderModel(model) {
			t.Errorf("legacy model %q must retain its system reminder path", model)
		}
	}
	for _, model := range []string{"claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1", "provider/claude-opus-5", ""} {
		if IsClaudeLegacySystemReminderModel(model) {
			t.Errorf("model %q unexpectedly uses legacy system reminders", model)
		}
	}
}
