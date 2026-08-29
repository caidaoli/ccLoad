package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be set to false by conversion")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be false")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	include := gjson.Get(outputStr, "include")
	if include.Exists() {
		t.Errorf("include should be absent without reasoning, got %s", include.Raw)
	}
}

func TestConvertOpenAIResponsesRequestToCodexReusesNormalizedPayload(t *testing.T) {
	inputJSON := []byte(`{"model":"gpt-5.6","stream":true,"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","input":[{"type":"message","role":"user","content":"hello"}]}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)

	if &output[0] != &inputJSON[0] {
		t.Fatal("normalized request payload was copied")
	}
	if string(output) != string(inputJSON) {
		t.Fatalf("normalized request changed:\n got: %s\nwant: %s", output, inputJSON)
	}
}

func TestConvertOpenAIResponsesRequestToCodexNormalizesRequiredFields(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.6",
		"stream":"true",
		"store":true,
		"parallel_tool_calls":false,
		"include":["file_search_call.results","reasoning.encrypted_content"],
		"max_output_tokens":4096,
		"max_completion_tokens":4096,
		"temperature":0.2,
		"top_p":0.9,
		"service_tier":"standard",
		"truncation":"auto",
		"prompt_cache_options":{"mode":"implicit"},
		"prompt_cache_retention":"24h",
		"user":"request-owner",
		"input":[{"type":"message","role":"system","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)

	if stream := gjson.GetBytes(output, "stream"); stream.Type != gjson.True {
		t.Fatalf("stream = %s, want true", stream.Raw)
	}
	if store := gjson.GetBytes(output, "store"); store.Type != gjson.False {
		t.Fatalf("store = %s, want false", store.Raw)
	}
	if parallel := gjson.GetBytes(output, "parallel_tool_calls"); parallel.Type != gjson.True {
		t.Fatalf("parallel_tool_calls = %s, want true", parallel.Raw)
	}
	include := gjson.GetBytes(output, "include").Array()
	if len(include) != 2 || include[0].String() != "file_search_call.results" || include[1].String() != "reasoning.encrypted_content" {
		t.Fatalf("include = %s, want existing values preserved", gjson.GetBytes(output, "include").Raw)
	}
	if role := gjson.GetBytes(output, "input.0.role").String(); role != "developer" {
		t.Fatalf("input.0.role = %q, want developer", role)
	}
	for _, path := range []string{
		"max_output_tokens",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"service_tier",
		"truncation",
		"prompt_cache_options",
		"prompt_cache_retention",
		"user",
	} {
		if gjson.GetBytes(output, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, output)
		}
	}
}

func TestConvertOpenAIResponsesRequestToCodexPreservesUltrafastServiceTier(t *testing.T) {
	inputJSON := []byte(`{"model":"gpt-5.3-codex","service_tier":"ultrafast","input":[]}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.3-codex", inputJSON, false)
	if got := gjson.GetBytes(output, "service_tier").String(); got != "ultrafast" {
		t.Fatalf("service_tier = %q, want ultrafast; body=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_FiltersPromptCacheRetention(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.6-terra",
		"prompt_cache_retention": "24h",
		"input": [
			{"type": "message", "role": "user", "content": "hello"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6-terra", inputJSON, true)
	if gjson.GetBytes(output, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed: %s", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToCodexAddsReasoningIncludeIncrementally(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantInclude string
	}{
		{
			name:        "missing include",
			input:       `{"model":"gpt-5.6","reasoning":{"effort":"medium"},"input":[]}`,
			wantInclude: `["reasoning.encrypted_content"]`,
		},
		{
			name:        "append to existing include",
			input:       `{"model":"gpt-5.6","reasoning":{"effort":"medium"},"include":["file_search_call.results"],"input":[]}`,
			wantInclude: `["file_search_call.results","reasoning.encrypted_content"]`,
		},
		{
			name:        "do not duplicate",
			input:       `{"model":"gpt-5.6","reasoning":{"effort":"medium"},"include":["reasoning.encrypted_content","file_search_call.results"],"input":[]}`,
			wantInclude: `["reasoning.encrypted_content","file_search_call.results"]`,
		},
		{
			name:        "preserve malformed include",
			input:       `{"model":"gpt-5.6","reasoning":{"effort":"medium"},"include":"file_search_call.results","input":[]}`,
			wantInclude: `"file_search_call.results"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", []byte(tt.input), true)
			if got := gjson.GetBytes(output, "include").Raw; got != tt.wantInclude {
				t.Fatalf("include = %s, want %s; body=%s", got, tt.wantInclude, output)
			}
		})
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesWebSearchPreview(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tools": [
			{"type": "web_search_preview_2025_03_11"}
		],
		"tool_choice": {
			"type": "allowed_tools",
			"tools": [
				{"type": "web_search_preview"},
				{"type": "web_search_preview_2025_03_11"}
			]
		}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "allowed_tools", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.0.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.0.type = %q, want %q: %s", got, "web_search", string(output))
	}
	if got := gjson.GetBytes(output, "tool_choice.tools.1.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.tools.1.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesTopLevelToolChoicePreviewAlias(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4-mini",
		"input": "find latest OpenAI model news",
		"tool_choice": {"type": "web_search_preview_2025_03_11"}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4-mini", inputJSON, false)

	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want %q: %s", got, "web_search", string(output))
	}
}

func TestUserFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"user": "test-user",
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify user field is deleted
	userField := gjson.Get(outputStr, "user")
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

func TestContextManagementCompactionCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"context_management": [
			{
				"type": "compaction",
				"compact_threshold": 12000
			}
		],
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "context_management").Exists() {
		t.Fatalf("context_management should be removed for Codex compatibility")
	}
	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestTruncationRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"truncation": "disabled",
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestStripCodexResponsesCacheBreakpoints(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.2",
		"prompt_cache_options":{"mode":"implicit"},
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"Hello world","prompt_cache_breakpoint":{"mode":"explicit"}},
			{"type":"input_text","text":"Second part"}
		]}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	if strings.Contains(string(output), "prompt_cache") {
		t.Fatalf("cache hints should not exist in output: %s", string(output))
	}
	if got := gjson.GetBytes(output, "input.0.content.0.text").String(); got != "Hello world" {
		t.Fatalf("first text = %q, want Hello world", got)
	}
	if got := gjson.GetBytes(output, "input.0.content.1.text").String(); got != "Second part" {
		t.Fatalf("second text = %q, want Second part", got)
	}
}

func TestStripCodexResponsesCacheBreakpoints_WithSystemRole(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.2",
		"input":[
			{"type":"message","role":"system","content":[{"type":"input_text","text":"System prompt","prompt_cache_breakpoint":{"mode":"explicit"}}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"User query","prompt_cache_breakpoint":{"mode":"explicit"}}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	if got := gjson.GetBytes(output, "input.0.role").String(); got != "developer" {
		t.Fatalf("role = %q, want developer", got)
	}
	if strings.Contains(string(output), "prompt_cache_breakpoint") {
		t.Fatalf("prompt_cache_breakpoint should not exist in output: %s", string(output))
	}
	if got := gjson.GetBytes(output, "input.0.content.0.text").String(); got != "System prompt" {
		t.Fatalf("system text = %q, want System prompt", got)
	}
	if got := gjson.GetBytes(output, "input.1.content.0.text").String(); got != "User query" {
		t.Fatalf("user text = %q, want User query", got)
	}
}
