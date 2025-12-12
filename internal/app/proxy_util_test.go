package app

import "testing"

func TestBuildLogEntry_StreamDiagMsg(t *testing.T) {
	channelID := int64(1)

	t.Run("正常成功响应", func(t *testing.T) {
		res := &fwResult{
			Status:       200,
			InputTokens:  10,
			OutputTokens: 20,
		}
		entry := buildLogEntry("claude-3", channelID, 200, 1.5, true, "sk-test", 0, res, "")
		if entry.Message != "ok" {
			t.Errorf("expected Message='ok', got %q", entry.Message)
		}
	})

	t.Run("流传输中断诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断: 错误=unexpected EOF | 已读取=1024字节(分5次)",
		}
		entry := buildLogEntry("claude-3", channelID, 200, 1.5, true, "sk-test", 0, res, "")
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("流响应不完整诊断", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流响应不完整: 正常EOF但无usage | 已读取=512字节(分3次)",
		}
		entry := buildLogEntry("claude-3", channelID, 200, 1.5, true, "sk-test", 0, res, "")
		if entry.Message != res.StreamDiagMsg {
			t.Errorf("expected Message=%q, got %q", res.StreamDiagMsg, entry.Message)
		}
	})

	t.Run("errMsg优先于StreamDiagMsg", func(t *testing.T) {
		res := &fwResult{
			Status:        200,
			StreamDiagMsg: "[WARN] 流传输中断",
		}
		errMsg := "network error"
		entry := buildLogEntry("claude-3", channelID, 200, 1.5, true, "sk-test", 0, res, errMsg)
		if entry.Message != errMsg {
			t.Errorf("expected Message=%q, got %q", errMsg, entry.Message)
		}
	})
}

func TestDetectChannelTypeFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{name: "Gemini", path: "/v1beta/models/gemini-pro:streamGenerateContent", expected: "gemini"},
		{name: "OpenAIChat", path: "/v1/chat/completions", expected: "openai"},
		{name: "OpenAIEmbeddings", path: "/v1/embeddings", expected: "openai"},
		{name: "ClaudeMessages", path: "/v1/messages", expected: "anthropic"},
		{name: "ClaudeCountTokens", path: "/v1/messages/count_tokens", expected: "anthropic"},
		{name: "CodexResponses", path: "/v1/responses", expected: "codex"},
		{name: "Unknown", path: "/v2/internal", expected: ""},
	}

	for _, tt := range tests {
		if got := detectChannelTypeFromPath(tt.path); got != tt.expected {
			t.Errorf("%s: detectChannelTypeFromPath(%q) = %q, want %q", tt.name, tt.path, got, tt.expected)
		}
	}
}
