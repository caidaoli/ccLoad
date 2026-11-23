package responses

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaude_KeepsLatestConsecutiveReasoning(t *testing.T) {
	firstRaw, _ := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-first")
	secondRaw, _ := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-second")
	thirdRaw, thirdSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-third")

	raw := responsesRequestFromItems(
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prefix"}]}`,
		responsesReasoningItem(firstRaw, "first reasoning"),
		responsesReasoningItem(secondRaw, "second reasoning"),
		responsesReasoningItem(thirdRaw, "third reasoning"),
		responsesFunctionCallItem("call_latest", "latest_tool"),
		responsesFunctionCallOutputItem("call_latest", "done"),
	)

	out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	content := gjson.GetBytes(out, "messages.0.content").Array()
	wantTypes := []string{"text", "thinking", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
	}
	for index, wantType := range wantTypes {
		if got := content[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
		}
	}
	if got := content[0].Get("text").String(); got != "prefix" {
		t.Fatalf("prefix text = %q, want prefix. Output: %s", got, out)
	}
	if got := content[1].Get("thinking").String(); got != "third reasoning" {
		t.Fatalf("thinking text = %q, want latest consecutive reasoning. Output: %s", got, out)
	}
	if got := content[1].Get("signature").String(); got != thirdSignature {
		t.Fatalf("thinking signature = %q, want latest signature %q. Output: %s", got, thirdSignature, out)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReplaysSameResponseThinkingInOrder(t *testing.T) {
	firstRaw, firstSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-first")
	secondRaw, secondSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-second")
	lastRaw, lastSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-last")
	item := func(id, signature, text string) string {
		return fmt.Sprintf(`{"type":"reasoning","id":%q,"encrypted_content":%q,"summary":[{"type":"summary_text","text":%q}]}`, id, signature, text)
	}
	first := item("rs_msg_same_0", firstRaw, "first")
	second := item("rs_msg_same_1", secondRaw, "second")
	for _, tt := range []struct {
		name       string
		items      []string
		texts      []string
		signatures []string
	}{
		{"ascending indices", []string{first, second}, []string{"first", "second"}, []string{firstSignature, secondSignature}},
		{"duplicate tail", []string{first, second, item("rs_msg_same_1", lastRaw, "last")}, []string{"first", "last"}, []string{firstSignature, lastSignature}},
		{"descending index", []string{first, second, item("rs_msg_same_0", lastRaw, "last")}, []string{"last"}, []string{lastSignature}},
		{"different response", []string{first, second, item("rs_msg_other_2", lastRaw, "last")}, []string{"last"}, []string{lastSignature}},
		{"invalid index", []string{first, second, item("rs_msg_same_+2", lastRaw, "last")}, []string{"last"}, []string{lastSignature}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := responsesRequestFromItems(append(tt.items,
				responsesFunctionCallItem("call_same", "tool"),
				responsesFunctionCallOutputItem("call_same", "done"),
			)...)
			out, err := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
			if err != nil {
				t.Fatal(err)
			}
			parts := gjson.GetBytes(out, "messages.0.content").Array()
			if len(parts) != len(tt.texts)+1 || parts[len(parts)-1].Get("type").String() != "tool_use" {
				t.Fatalf("unexpected assistant content: %s", out)
			}
			for i, want := range tt.texts {
				if part := parts[i]; part.Get("type").String() != "thinking" || part.Get("thinking").String() != want || part.Get("signature").String() != tt.signatures[i] {
					t.Fatalf("unexpected thinking block at index %d: %s", i, part.Raw)
				}
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ToolCallsSeparateReasoningBlocks(t *testing.T) {
	firstRaw, firstSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-first")
	secondRaw, secondSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-second")

	raw := responsesRequestFromItems(
		responsesReasoningItem(firstRaw, "first reasoning"),
		responsesFunctionCallItem("call_first", "first_tool"),
		responsesReasoningItem(secondRaw, "second reasoning"),
		responsesFunctionCallItem("call_second", "second_tool"),
		responsesFunctionCallOutputItem("call_first", "first result"),
		responsesFunctionCallOutputItem("call_second", "second result"),
	)

	out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	content := gjson.GetBytes(out, "messages.0.content").Array()
	wantTypes := []string{"thinking", "tool_use", "thinking", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
	}
	for index, wantType := range wantTypes {
		if got := content[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
		}
	}
	if got := content[0].Get("signature").String(); got != firstSignature {
		t.Fatalf("first thinking signature = %q, want %q", got, firstSignature)
	}
	if got := content[2].Get("signature").String(); got != secondSignature {
		t.Fatalf("second thinking signature = %q, want %q", got, secondSignature)
	}
	if got := content[1].Get("id").String(); got != "call_first" {
		t.Fatalf("first tool_use id = %q, want call_first", got)
	}
	if got := content[3].Get("id").String(); got != "call_second" {
		t.Fatalf("second tool_use id = %q, want call_second", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_NonThinkingBlocksSeparateReasoning(t *testing.T) {
	firstRaw, firstSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-first")
	secondRaw, secondSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-second")
	const redactedData = "opaque-redacted-data"

	raw := responsesRequestFromItems(
		responsesReasoningItem(firstRaw, "first reasoning"),
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible separator"}]}`,
		responsesReasoningItem(secondRaw, "second reasoning"),
		`{"type":"reasoning","encrypted_content":"`+ClaudeResponsesRedactedThinkingPrefix+redactedData+`","summary":[]}`,
		responsesReasoningItem(firstRaw, "third reasoning"),
		responsesFunctionCallItem("call_separator", "separator_tool"),
		responsesFunctionCallOutputItem("call_separator", "done"),
	)

	out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	content := gjson.GetBytes(out, "messages.0.content").Array()
	wantTypes := []string{"thinking", "text", "thinking", "redacted_thinking", "thinking", "tool_use"}
	if len(content) != len(wantTypes) {
		t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
	}
	for index, wantType := range wantTypes {
		if got := content[index].Get("type").String(); got != wantType {
			t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
		}
	}
	if got := content[0].Get("signature").String(); got != firstSignature {
		t.Fatalf("first thinking signature = %q, want %q", got, firstSignature)
	}
	if got := content[2].Get("signature").String(); got != secondSignature {
		t.Fatalf("second thinking signature = %q, want %q", got, secondSignature)
	}
	if got := content[3].Get("data").String(); got != redactedData {
		t.Fatalf("redacted data = %q, want %q", got, redactedData)
	}
	if got := content[4].Get("signature").String(); got != firstSignature {
		t.Fatalf("third thinking signature = %q, want %q", got, firstSignature)
	}
	if got := content[5].Get("id").String(); got != "call_separator" {
		t.Fatalf("tool_use id = %q, want call_separator", got)
	}
}

func responsesReasoningItem(signature, text string) string {
	return fmt.Sprintf(`{"type":"reasoning","encrypted_content":%q,"summary":[{"type":"summary_text","text":%q}]}`, signature, text)
}

func responsesFunctionCallItem(callID, name string) string {
	return fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":%q,"arguments":"{}"}`, callID, name)
}

func responsesFunctionCallOutputItem(callID, output string) string {
	return fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":%q}`, callID, output)
}

func responsesWebSearchCallItem(id, query string) string {
	return fmt.Sprintf(`{"type":"web_search_call","id":%q,"status":"completed","action":{"type":"search","query":%q}}`, id, query)
}

func TestConvertOpenAIResponsesRequestToClaude_WebSearchSeparatesToolUseWithThinking(t *testing.T) {
	rawSig, signature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-test")

	t.Run("function_call then web_search_call", func(t *testing.T) {
		raw := responsesRequestFromItems(
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
			responsesReasoningItem(rawSig, "I should search and then run a command."),
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Searching, then running."}]}`,
			responsesFunctionCallItem("call_00_abc", "exec_command"),
			responsesWebSearchCallItem("ws_srvtoolu_12_x", "hello world"),
			responsesFunctionCallOutputItem("call_00_abc", "hi"),
		)

		out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		content := gjson.GetBytes(out, "messages.1.content").Array()
		wantTypes := []string{"thinking", "text", "server_tool_use", "web_search_tool_result", "thinking", "tool_use"}
		if len(content) != len(wantTypes) {
			t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
		}
		for index, wantType := range wantTypes {
			if got := content[index].Get("type").String(); got != wantType {
				t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
			}
		}
		if got := content[0].Get("signature").String(); got != signature {
			t.Fatalf("first thinking signature = %q, want %q", got, signature)
		}
		if got := content[4].Get("signature").String(); got != signature {
			t.Fatalf("second thinking signature = %q, want %q", got, signature)
		}
		if got := content[4].Get("thinking").String(); got != "I should search and then run a command." {
			t.Fatalf("second thinking text = %q, want original thinking text", got)
		}
		if got := content[5].Get("id").String(); got != "call_00_abc" {
			t.Fatalf("tool_use id = %q, want call_00_abc", got)
		}
	})

	t.Run("web_search_call then function_call", func(t *testing.T) {
		raw := responsesRequestFromItems(
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
			responsesReasoningItem(rawSig, "I should search and then run a command."),
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Searching, then running."}]}`,
			responsesWebSearchCallItem("ws_srvtoolu_12_x", "hello world"),
			responsesFunctionCallItem("call_00_abc", "exec_command"),
			responsesFunctionCallOutputItem("call_00_abc", "hi"),
		)

		out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		content := gjson.GetBytes(out, "messages.1.content").Array()
		wantTypes := []string{"thinking", "text", "server_tool_use", "web_search_tool_result", "thinking", "tool_use"}
		if len(content) != len(wantTypes) {
			t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
		}
		for index, wantType := range wantTypes {
			if got := content[index].Get("type").String(); got != wantType {
				t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
			}
		}
		if got := content[4].Get("signature").String(); got != signature {
			t.Fatalf("second thinking signature = %q, want %q", got, signature)
		}
		if got := content[5].Get("id").String(); got != "call_00_abc" {
			t.Fatalf("tool_use id = %q, want call_00_abc", got)
		}
	})

	t.Run("no thinking item present", func(t *testing.T) {
		raw := responsesRequestFromItems(
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Searching, then running."}]}`,
			responsesWebSearchCallItem("ws_srvtoolu_12_x", "hello world"),
			responsesFunctionCallItem("call_00_abc", "exec_command"),
			responsesFunctionCallOutputItem("call_00_abc", "hi"),
		)

		out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		content := gjson.GetBytes(out, "messages.1.content").Array()
		wantTypes := []string{"text", "server_tool_use", "web_search_tool_result", "tool_use"}
		if len(content) != len(wantTypes) {
			t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
		}
		for index, wantType := range wantTypes {
			if got := content[index].Get("type").String(); got != wantType {
				t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
			}
		}
	})

	t.Run("selects latest thinking block when multiple exist", func(t *testing.T) {
		firstRaw, _ := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-first")
		secondRaw, secondSignature := testClaudeResponsesThinkingSignatureForModel(t, "claude-opus-5-second")

		raw := responsesRequestFromItems(
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
			responsesReasoningItem(firstRaw, "first reasoning"),
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thought once"}]}`,
			responsesReasoningItem(secondRaw, "second reasoning"),
			responsesWebSearchCallItem("ws_srvtoolu_12_x", "hello world"),
			responsesFunctionCallItem("call_00_abc", "exec_command"),
			responsesFunctionCallOutputItem("call_00_abc", "hi"),
		)

		out, _ := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		content := gjson.GetBytes(out, "messages.1.content").Array()
		wantTypes := []string{"thinking", "text", "thinking", "server_tool_use", "web_search_tool_result", "thinking", "tool_use"}
		if len(content) != len(wantTypes) {
			t.Fatalf("assistant content count = %d, want %d. Output: %s", len(content), len(wantTypes), out)
		}
		for index, wantType := range wantTypes {
			if got := content[index].Get("type").String(); got != wantType {
				t.Fatalf("assistant content[%d].type = %q, want %q. Output: %s", index, got, wantType, out)
			}
		}
		if got := content[5].Get("signature").String(); got != secondSignature {
			t.Fatalf("separated thinking signature = %q, want latest %q", got, secondSignature)
		}
		if got := content[5].Get("thinking").String(); got != "second reasoning" {
			t.Fatalf("separated thinking text = %q, want latest 'second reasoning'", got)
		}
	})
}
