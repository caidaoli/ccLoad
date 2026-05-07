package testutil

import (
	"regexp"
	"strings"
	"testing"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
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

func TestOpenAITesterBuild_AddsSessionIDHeader(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "gpt-test", Content: "hello"}

	_, headers, body, err := (&OpenAITester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	sessionID := headers.Get("Session_id")
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidPattern.MatchString(sessionID) {
		t.Fatalf("Session_id header missing or invalid: %q", sessionID)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id header should be omitted, got %q", got)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	if got, _ := payload["user"].(string); got != sessionID {
		t.Fatalf("body user = %q, want session id %q; body=%s", got, sessionID, body)
	}
	if got, _ := payload["prompt_cache_key"].(string); got != sessionID {
		t.Fatalf("body prompt_cache_key = %q, want session id %q; body=%s", got, sessionID, body)
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
