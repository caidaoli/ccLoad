package testutil

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestOpenAITesterBuild_ExactURLMarkerSkipsEndpointPath(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com/custom/chat#"}
	req := &TestChannelRequest{Model: "gpt-test", Content: "hello"}

	fullURL, _, _, err := (&OpenAITester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if fullURL != "https://api.example.com/custom/chat" {
		t.Fatalf("fullURL = %q, want %q", fullURL, "https://api.example.com/custom/chat")
	}
	if strings.Contains(fullURL, "/v1/chat/completions") {
		t.Fatalf("fullURL should not append OpenAI endpoint path: %q", fullURL)
	}
}

func TestAnthropicTesterBuild_ExactURLMarkerSkipsEndpointPath(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com/custom/messages#"}
	req := &TestChannelRequest{Model: "claude-test", Content: "hello"}

	fullURL, _, _, err := (&AnthropicTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if fullURL != "https://api.example.com/custom/messages" {
		t.Fatalf("fullURL = %q, want %q", fullURL, "https://api.example.com/custom/messages")
	}
	if strings.Contains(fullURL, "/v1/messages") {
		t.Fatalf("fullURL should not append Anthropic endpoint path: %q", fullURL)
	}
}
