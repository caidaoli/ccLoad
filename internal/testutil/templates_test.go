package testutil

import (
	"testing"

	"github.com/bytedance/sonic"
)

func TestBuildRequestFromTemplate_EscapesAnthropicContent(t *testing.T) {
	content := "quoted \"text\"\nbackslash \\ and <tag>"
	body, err := buildRequestFromTemplate("anthropic", map[string]any{
		"MODEL": "claude-sonnet-5", "CONTENT": content, "STREAM": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 1 || len(payload.Messages[0].Content) != 1 || payload.Messages[0].Content[0].Text != content {
		t.Fatalf("content was not preserved: %s", body)
	}
}
