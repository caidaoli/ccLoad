package testutil

import (
	"regexp"
	"strings"
	"testing"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

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

func TestOpenAITesterBuild_AppliesThinkingEffortAndBuiltinSearch(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model:          "gpt-test",
		Content:        "hello",
		ThinkingEffort: "high",
		BuiltinSearch:  true,
	}

	_, _, body, err := (&OpenAITester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	if got, _ := payload["reasoning_effort"].(string); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high; body=%s", got, body)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools invalid: %#v; body=%s", payload["tools"], body)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %#v; body=%s", tools[0], body)
	}
	if got, _ := tool["type"].(string); got != "web_search" {
		t.Fatalf("tools[0].type = %q, want web_search; body=%s", got, body)
	}
	if got, _ := payload["tool_choice"].(string); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", got, body)
	}
}

func TestOpenAITesterBuild_AppliesSamplingAndSystemPrompt(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	temperature := 0.7
	topP := 0.9
	req := &TestChannelRequest{
		Model:        "gpt-test",
		Content:      "hello",
		SystemPrompt: "answer tersely",
		Temperature:  &temperature,
		TopP:         &topP,
		MaxTokens:    2048,
	}

	_, _, body, err := (&OpenAITester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	if got, _ := payload["temperature"].(float64); got != 0.7 {
		t.Fatalf("temperature = %v, want 0.7; body=%s", got, body)
	}
	if got, _ := payload["top_p"].(float64); got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9; body=%s", got, body)
	}
	if got, _ := payload["max_tokens"].(float64); got != 2048 {
		t.Fatalf("max_tokens = %v, want 2048; body=%s", got, body)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("messages invalid: %#v; body=%s", payload["messages"], body)
	}
	system, ok := messages[0].(map[string]any)
	if !ok || system["role"] != "system" || system["content"] != "answer tersely" {
		t.Fatalf("first message should be system prompt, got %#v; body=%s", messages[0], body)
	}
}

func TestOpenAITesterBuild_SupportsStructuredImageMessages(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model: "gpt-test",
		Messages: []ChatMessage{
			{
				Role: "user",
				ContentBlocks: []chatContentBlock{
					{Type: "text", Text: "describe"},
					{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64,aW1n"}},
				},
			},
		},
	}

	_, _, body, err := (&OpenAITester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	bodyText := string(body)
	if !strings.Contains(bodyText, `"type":"text"`) || !strings.Contains(bodyText, `"text":"describe"`) {
		t.Fatalf("openai body missing text block: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"type":"image_url"`) || !strings.Contains(bodyText, `"url":"data:image/png;base64,aW1n"`) {
		t.Fatalf("openai body missing image_url block: %s", bodyText)
	}
}

func TestCodexTesterBuild_UsesCurrentCodexClientHeaders(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "gpt-5.5", Content: "hello", Stream: true}

	fullURL, headers, _, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if fullURL != "https://api.example.com/v1/responses" {
		t.Fatalf("fullURL = %q, want %q", fullURL, "https://api.example.com/v1/responses")
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", got)
	}
	if got := headers.Get("X-Api-Key"); got != "sk-test" {
		t.Fatalf("X-Api-Key = %q, want sk-test", got)
	}
	if got := headers.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q, want codex-tui", got)
	}
	sessionID := headers.Get("Session-Id")
	if !uuidPattern.MatchString(sessionID) {
		t.Fatalf("Session-Id header missing or invalid: %q", sessionID)
	}
	if got := headers.Get("Thread-Id"); got != sessionID {
		t.Fatalf("Thread-Id = %q, want session id %q", got, sessionID)
	}
	if got := headers.Get("X-Client-Request-Id"); got != sessionID {
		t.Fatalf("X-Client-Request-Id = %q, want session id %q", got, sessionID)
	}
	if got := headers.Get("X-Codex-Window-Id"); got != sessionID+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, sessionID+":0")
	}
	if got := headers.Get("X-Codex-Beta-Features"); got != "terminal_resize_reflow" {
		t.Fatalf("X-Codex-Beta-Features = %q, want terminal_resize_reflow", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); !strings.Contains(got, sessionID) {
		t.Fatalf("X-Codex-Turn-Metadata should contain session id %q, got %q", sessionID, got)
	}
	if got := headers.Get("Openai-Beta"); got != "" {
		t.Fatalf("Openai-Beta header should be omitted, got %q", got)
	}
	if got := headers.Get("User-Agent"); !strings.HasPrefix(got, "codex-tui/0.137.0 ") {
		t.Fatalf("User-Agent = %q, want codex-tui/0.137.0 prefix", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
}

func TestCodexTesterBuild_UsesCurrentCodexClientBodyShape(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "gpt-5.5", Content: "hello", Stream: true}

	_, headers, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}

	sessionID := headers.Get("Session-Id")
	if got, _ := payload["prompt_cache_key"].(string); got != sessionID {
		t.Fatalf("prompt_cache_key = %q, want session id %q; body=%s", got, sessionID, body)
	}
	if got, _ := payload["tool_choice"].(string); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", got, body)
	}
	if got, _ := payload["parallel_tool_calls"].(bool); !got {
		t.Fatalf("parallel_tool_calls = %v, want true; body=%s", got, body)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing or invalid; body=%s", body)
	}
	if got, _ := reasoning["effort"].(string); got != "low" {
		t.Fatalf("reasoning.effort = %q, want low; body=%s", got, body)
	}

	textConfig, ok := payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("text missing or invalid; body=%s", body)
	}
	if got, _ := textConfig["verbosity"].(string); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; body=%s", got, body)
	}

	clientMetadata, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("client_metadata missing or invalid; body=%s", body)
	}
	if got, _ := clientMetadata["x-codex-installation-id"].(string); !uuidPattern.MatchString(got) {
		t.Fatalf("x-codex-installation-id missing or invalid: %q; body=%s", got, body)
	}

	tools, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing or invalid; body=%s", body)
	}
	if len(tools) != 0 {
		t.Fatalf("tools length = %d, want 0; body=%s", len(tools), body)
	}
}

func TestCodexTesterBuild_AppliesThinkingEffortAndBuiltinSearch(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model:          "gpt-5.5",
		Content:        "hello",
		Stream:         true,
		ThinkingEffort: "medium",
		BuiltinSearch:  true,
	}

	_, _, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing or invalid; body=%s", body)
	}
	if got, _ := reasoning["effort"].(string); got != "medium" {
		t.Fatalf("reasoning.effort = %q, want medium; body=%s", got, body)
	}
	if got, _ := reasoning["summary"].(string); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, body)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools invalid: %#v; body=%s", payload["tools"], body)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %#v; body=%s", tools[0], body)
	}
	if got, _ := tool["type"].(string); got != "web_search" {
		t.Fatalf("tools[0].type = %q, want web_search; body=%s", got, body)
	}
	if got, _ := payload["tool_choice"].(string); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", got, body)
	}
}

func TestCodexTesterBuild_SupportsStructuredImageMessages(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model: "gpt-5.5",
		Messages: []ChatMessage{
			{
				Role: "user",
				ContentBlocks: []chatContentBlock{
					{Type: "text", Text: "describe"},
					{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64,aW1n"}},
				},
			},
		},
	}

	_, _, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	bodyText := string(body)
	if !strings.Contains(bodyText, `"type":"input_text"`) || !strings.Contains(bodyText, `"text":"describe"`) {
		t.Fatalf("codex body missing input_text block: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"type":"input_image"`) || !strings.Contains(bodyText, `"image_url":"data:image/png;base64,aW1n"`) {
		t.Fatalf("codex body missing input_image block: %s", bodyText)
	}
}

func TestCodexTesterBuild_DisablesThinkingWhenRequested(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "gpt-5.5", Content: "hello", ThinkingEffort: "none"}

	_, _, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatalf("reasoning should be removed when thinking_effort=none; body=%s", body)
	}
	if _, ok := payload["include"]; ok {
		t.Fatalf("include should be removed when thinking_effort=none; body=%s", body)
	}
}

func TestCodexTesterBuild_PrependsSystemPromptAsDeveloperInputAndAppliesSampling(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	temperature := 0.3
	topP := 0.95
	req := &TestChannelRequest{
		Model:        "gpt-5.5",
		Content:      "hello",
		SystemPrompt: "prefer short answers",
		Temperature:  &temperature,
		TopP:         &topP,
		MaxTokens:    4096,
	}

	_, _, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	instructions, _ := payload["instructions"].(string)
	if !strings.Contains(instructions, "You are Codex") {
		t.Fatalf("instructions should preserve template prompt, got %q; body=%s", instructions, body)
	}
	if strings.Contains(instructions, "prefer short answers") {
		t.Fatalf("user system prompt should not be appended to instructions, instructions=%q; body=%s", instructions, body)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) < 2 {
		t.Fatalf("input invalid: %#v; body=%s", payload["input"], body)
	}
	developer, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("first input invalid: %#v; body=%s", input[0], body)
	}
	if developer["role"] != "developer" || developer["content"] != "prefer short answers" {
		t.Fatalf("first input should be developer system prompt, got %#v; body=%s", developer, body)
	}
	user, _ := input[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("second input should remain the user message, got %#v; body=%s", user, body)
	}
	if got, _ := payload["temperature"].(float64); got != 0.3 {
		t.Fatalf("temperature = %v, want 0.3; body=%s", got, body)
	}
	if got, _ := payload["top_p"].(float64); got != 0.95 {
		t.Fatalf("top_p = %v, want 0.95; body=%s", got, body)
	}
	if got, _ := payload["max_output_tokens"].(float64); got != 4096 {
		t.Fatalf("max_output_tokens = %v, want 4096; body=%s", got, body)
	}
}

func TestCodexTesterBuild_UsesMessagesAsResponsesInput(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model:  "gpt-5.5",
		Stream: true,
		Messages: []ChatMessage{
			{Role: "user", Content: "macbook m5有几款"},
			{Role: "assistant", Content: "Test received. How can I help?"},
			{Role: "user", Content: "联网搜索一下"},
		},
	}

	_, _, body, err := (&CodexTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}

	input, ok := payload["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input length = %d, want 3; body=%s", len(input), body)
	}

	want := []struct {
		role     string
		partType string
		text     string
	}{
		{"user", "input_text", "macbook m5有几款"},
		{"assistant", "output_text", "Test received. How can I help?"},
		{"user", "input_text", "联网搜索一下"},
	}
	for i, tc := range want {
		item, ok := input[i].(map[string]any)
		if !ok {
			t.Fatalf("input[%d] invalid: %#v; body=%s", i, input[i], body)
		}
		if got, _ := item["type"].(string); got != "message" {
			t.Fatalf("input[%d].type = %q, want message; body=%s", i, got, body)
		}
		if got, _ := item["role"].(string); got != tc.role {
			t.Fatalf("input[%d].role = %q, want %q; body=%s", i, got, tc.role, body)
		}
		content, ok := item["content"].([]any)
		if !ok || len(content) != 1 {
			t.Fatalf("input[%d].content invalid: %#v; body=%s", i, item["content"], body)
		}
		part, ok := content[0].(map[string]any)
		if !ok {
			t.Fatalf("input[%d].content[0] invalid: %#v; body=%s", i, content[0], body)
		}
		if got, _ := part["type"].(string); got != tc.partType {
			t.Fatalf("input[%d].content[0].type = %q, want %q; body=%s", i, got, tc.partType, body)
		}
		if got, _ := part["text"].(string); got != tc.text {
			t.Fatalf("input[%d].content[0].text = %q, want %q; body=%s", i, got, tc.text, body)
		}
	}

	if strings.Contains(string(body), `"text":"test"`) {
		t.Fatalf("body should not fall back to test content when messages are present: %s", body)
	}
}

func TestGeminiTesterBuild_AppliesThinkingEffortAndBuiltinSearch(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model:          "gemini-test",
		Content:        "hello",
		ThinkingEffort: "medium",
		BuiltinSearch:  true,
	}

	_, _, body, err := (&GeminiTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	generationConfig, ok := payload["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing or invalid; body=%s", body)
	}
	thinkingConfig, ok := generationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing or invalid; body=%s", body)
	}
	if got, _ := thinkingConfig["includeThoughts"].(bool); !got {
		t.Fatalf("includeThoughts = %v, want true; body=%s", got, body)
	}
	if got, _ := thinkingConfig["thinkingBudget"].(float64); got != 4096 {
		t.Fatalf("thinkingBudget = %v, want 4096; body=%s", got, body)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools invalid: %#v; body=%s", payload["tools"], body)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %#v; body=%s", tools[0], body)
	}
	if _, ok := tool["googleSearch"].(map[string]any); !ok {
		t.Fatalf("tools[0].googleSearch missing; body=%s", body)
	}
}

func TestGeminiTesterBuild_AppliesSamplingAndSystemPrompt(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	temperature := 0.5
	topP := 0.8
	req := &TestChannelRequest{
		Model:        "gemini-test",
		Content:      "hello",
		SystemPrompt: "use metric units",
		Temperature:  &temperature,
		TopP:         &topP,
		MaxTokens:    1024,
	}

	_, _, body, err := (&GeminiTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	systemInstruction, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing or invalid; body=%s", body)
	}
	parts, ok := systemInstruction["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("systemInstruction.parts invalid: %#v; body=%s", systemInstruction["parts"], body)
	}
	part, ok := parts[0].(map[string]any)
	if !ok || part["text"] != "use metric units" {
		t.Fatalf("systemInstruction text invalid: %#v; body=%s", parts[0], body)
	}
	generationConfig, ok := payload["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing or invalid; body=%s", body)
	}
	if got, _ := generationConfig["temperature"].(float64); got != 0.5 {
		t.Fatalf("temperature = %v, want 0.5; body=%s", got, body)
	}
	if got, _ := generationConfig["topP"].(float64); got != 0.8 {
		t.Fatalf("topP = %v, want 0.8; body=%s", got, body)
	}
	if got, _ := generationConfig["maxOutputTokens"].(float64); got != 1024 {
		t.Fatalf("maxOutputTokens = %v, want 1024; body=%s", got, body)
	}
}

func TestGeminiTesterBuild_DisablesThinkingWhenRequested(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "gemini-test", Content: "hello", ThinkingEffort: "none"}

	_, _, body, err := (&GeminiTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	generationConfig, ok := payload["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing or invalid; body=%s", body)
	}
	thinkingConfig, ok := generationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing or invalid; body=%s", body)
	}
	if got, _ := thinkingConfig["thinkingBudget"].(float64); got != 0 {
		t.Fatalf("thinkingBudget = %v, want 0; body=%s", got, body)
	}
	if _, ok := thinkingConfig["includeThoughts"]; ok {
		t.Fatalf("includeThoughts should be omitted when thinking is disabled; body=%s", body)
	}
}

func TestGeminiTesterBuild_SupportsStructuredImageMessages(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model: "gemini-test",
		Messages: []ChatMessage{
			{
				Role: "user",
				ContentBlocks: []chatContentBlock{
					{Type: "text", Text: "describe"},
					{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64,aW1n"}},
				},
			},
		},
	}

	_, _, body, err := (&GeminiTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	bodyText := string(body)
	if !strings.Contains(bodyText, `"text":"describe"`) {
		t.Fatalf("gemini body missing text part: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"inlineData"`) || !strings.Contains(bodyText, `"mimeType":"image/png"`) || !strings.Contains(bodyText, `"data":"aW1n"`) {
		t.Fatalf("gemini body missing inlineData image part: %s", bodyText)
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

func TestAnthropicTesterBuild_AppliesThinkingEffortAndBuiltinSearch(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model:          "claude-test",
		Content:        "hello",
		ThinkingEffort: "high",
		BuiltinSearch:  true,
	}

	_, _, body, err := (&AnthropicTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing or invalid; body=%s", body)
	}
	if got, _ := thinking["type"].(string); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, body)
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Fatalf("thinking.budget_tokens should not be sent; body=%s", body)
	}
	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing or invalid; body=%s", body)
	}
	if got, _ := outputConfig["effort"].(string); got != "high" {
		t.Fatalf("output_config.effort = %q, want high; body=%s", got, body)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools invalid: %#v; body=%s", payload["tools"], body)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %#v; body=%s", tools[0], body)
	}
	if got, _ := tool["type"].(string); got != "web_search_20250305" {
		t.Fatalf("tools[0].type = %q, want web_search_20250305; body=%s", got, body)
	}
	if got, _ := tool["name"].(string); got != "web_search" {
		t.Fatalf("tools[0].name = %q, want web_search; body=%s", got, body)
	}
}

func TestAnthropicTesterBuild_AppendsSystemPromptAndAppliesSampling(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	temperature := 0.2
	topP := 0.85
	req := &TestChannelRequest{
		Model:        "claude-test",
		Content:      "hello",
		SystemPrompt: "keep the answer direct",
		Temperature:  &temperature,
		TopP:         &topP,
		MaxTokens:    8192,
	}

	_, _, body, err := (&AnthropicTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	if got, _ := payload["temperature"].(float64); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2; body=%s", got, body)
	}
	if got, _ := payload["top_p"].(float64); got != 0.85 {
		t.Fatalf("top_p = %v, want 0.85; body=%s", got, body)
	}
	if got, _ := payload["max_tokens"].(float64); got != 8192 {
		t.Fatalf("max_tokens = %v, want 8192; body=%s", got, body)
	}
	system, ok := payload["system"].([]any)
	if !ok || len(system) < 3 {
		t.Fatalf("system invalid: %#v; body=%s", payload["system"], body)
	}
	first, _ := system[0].(map[string]any)
	last, _ := system[len(system)-1].(map[string]any)
	if !strings.Contains(first["text"].(string), "Claude Code") {
		t.Fatalf("template system prompt should be preserved, first=%#v; body=%s", first, body)
	}
	if last["type"] != "text" || last["text"] != "keep the answer direct" {
		t.Fatalf("user system prompt should be appended last, last=%#v; body=%s", last, body)
	}
}

func TestAnthropicTesterBuild_DisablesThinkingWhenRequested(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{Model: "claude-test", Content: "hello", ThinkingEffort: "none"}

	_, _, body, err := (&AnthropicTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing or invalid; body=%s", body)
	}
	if got, _ := thinking["type"].(string); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, body)
	}
}

func TestAnthropicTesterBuild_SupportsStructuredImageMessages(t *testing.T) {
	cfg := &model.Config{URL: "https://api.example.com"}
	req := &TestChannelRequest{
		Model: "claude-test",
		Messages: []ChatMessage{
			{
				Role: "user",
				ContentBlocks: []chatContentBlock{
					{Type: "text", Text: "describe"},
					{Type: "image_url", ImageURL: &chatImageURL{URL: "data:image/png;base64,aW1n"}},
				},
			},
		},
	}

	_, _, body, err := (&AnthropicTester{}).Build(cfg, "sk-test", req)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body failed: %v; body=%s", err, body)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages missing or invalid: %s", body)
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message missing or invalid: %s", body)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content missing or invalid: %s", body)
	}
	textPart, ok := content[0].(map[string]any)
	if !ok || textPart["type"] != "text" || textPart["text"] != "describe" {
		t.Fatalf("anthropic body missing text part: %s", body)
	}
	imagePart, ok := content[1].(map[string]any)
	if !ok || imagePart["type"] != "image" {
		t.Fatalf("anthropic body missing image part: %s", body)
	}
	source, ok := imagePart["source"].(map[string]any)
	if !ok || source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "aW1n" {
		t.Fatalf("anthropic body missing image source block: %s", body)
	}
}
