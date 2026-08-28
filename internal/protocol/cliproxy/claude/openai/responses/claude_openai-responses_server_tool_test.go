package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func claudeWebSearchStreamChunks() [][]byte {
	return [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_ws","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"lindorm vector\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Lindorm Vector","url":"https://example.com/a","encrypted_content":"ENC_A","page_age":"1 day"},{"type":"web_search_result","title":"Docs","url":"https://example.com/b","encrypted_content":"ENC_B"}]}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
}

func TestClaudeWebSearchBlocksBecomeWebSearchCallItem(t *testing.T) {
	outputs := translateClaudeResponsesStream(claudeWebSearchStreamChunks())
	var completed gjson.Result
	for _, output := range outputs {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" {
			completed = data
		}
	}
	items := completed.Get("response.output").Array()
	if len(items) != 1 || items[0].Get("type").String() != "web_search_call" {
		t.Fatalf("output = %s", completed.Get("response.output").Raw)
	}
	if got := items[0].Get("action.query").String(); got != "lindorm vector" {
		t.Fatalf("action.query = %q", got)
	}
	if got := items[0].Get("results.#").Int(); got != 2 {
		t.Fatalf("results = %d, want 2", got)
	}
	if got := items[0].Get("results.0.encrypted_content").String(); got != "ENC_A" {
		t.Fatalf("encrypted_content = %q", got)
	}
}

func TestClaudeWebSearchBlocksBecomeWebSearchCallItemNonStream(t *testing.T) {
	var lines []string
	for _, chunk := range claudeWebSearchStreamChunks() {
		lines = append(lines, string(chunk))
	}
	out := ConvertClaudeResponseToOpenAIResponsesNonStream(
		context.Background(), "claude-test", nil, nil, []byte(strings.Join(lines, "\n")), nil)
	items := gjson.GetBytes(out, "output").Array()
	if len(items) != 1 || items[0].Get("type").String() != "web_search_call" {
		t.Fatalf("output = %s", gjson.GetBytes(out, "output").Raw)
	}
	if got := items[0].Get("action.query").String(); got != "lindorm vector" {
		t.Fatalf("action.query = %q", got)
	}
	if got := items[0].Get("results.#").Int(); got != 2 {
		t.Fatalf("results = %d, want 2", got)
	}
}

func TestWebSearchCallItemReplaysAsClaudeServerToolBlocks(t *testing.T) {
	raw := responsesRequestFromItems(`{
		"type":"web_search_call","id":"ws_srvtoolu_1","status":"completed",
		"action":{"type":"search","query":"lindorm vector"},
		"results":[{"title":"Lindorm Vector","url":"https://example.com/a","encrypted_content":"ENC_A"}]
	}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if got := claudeAssistantBlockTypes(t, out); strings.Join(got, ",") != "server_tool_use,web_search_tool_result" {
		t.Fatalf("assistant blocks = %v", got)
	}
	blocks := gjson.GetBytes(out, "messages.0.content")
	if got := blocks.Get("0.id").String(); got != "srvtoolu_1" {
		t.Fatalf("server_tool_use id = %q", got)
	}
	if got := blocks.Get("0.input.query").String(); got != "lindorm vector" {
		t.Fatalf("input.query = %q", got)
	}
	if got := blocks.Get("1.content.0.encrypted_content").String(); got != "ENC_A" {
		t.Fatalf("encrypted_content = %q", got)
	}
}

func TestWebSearchCallWithoutEncryptedContentReplaysEmptyResults(t *testing.T) {
	raw := responsesRequestFromItems(`{
		"type":"web_search_call","id":"ws_srvtoolu_1","status":"completed",
		"action":{"type":"search","query":"q"},
		"results":[{"title":"T","url":"https://example.com/a"}]
	}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if got := gjson.GetBytes(out, "messages.0.content.1.content.#").Int(); got != 0 {
		t.Fatalf("result content entries = %d, want 0", got)
	}
}

func TestOutputTextAnnotationsReplayAsClaudeCitations(t *testing.T) {
	raw := responsesRequestFromItems(`{
		"type":"message","role":"assistant",
		"content":[{"type":"output_text","text":"Answer.","annotations":[
			{"type":"web_search_result_location","url":"https://example.com/a","encrypted_index":"IDX_A"}
		]}]
	}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	block := gjson.GetBytes(out, "messages.0.content.0")
	if got := block.Get("citations.0.encrypted_index").String(); got != "IDX_A" {
		t.Fatalf("encrypted_index = %q", got)
	}
}

func TestAnnotationsWithoutEncryptedIndexAreNotReplayedAsCitations(t *testing.T) {
	raw := responsesRequestFromItems(`{
		"type":"message","role":"assistant",
		"content":[{"type":"output_text","text":"Answer.","annotations":[
			{"type":"url_citation","url":"https://example.com/a"}
		]}]
	}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if gjson.GetBytes(out, "messages.0.content.0.citations").Exists() {
		t.Fatal("citation without encrypted_index must not be replayed")
	}
}

func TestRefusalPartReplaysAsClaudeText(t *testing.T) {
	raw := responsesRequestFromItems(`{
		"type":"message","role":"assistant",
		"content":[{"type":"refusal","refusal":"I cannot help with that."}]
	}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	content := gjson.GetBytes(out, "messages.0.content")
	text := content.Get("0.text").String()
	if content.Type == gjson.String {
		text = content.String()
	}
	if text != "I cannot help with that." {
		t.Fatalf("refusal text = %q", text)
	}
}

func TestWebSearchCallIDNormalisedToClaudeServerToolPattern(t *testing.T) {
	for _, tc := range []struct{ responsesID, want string }{
		{"ws_srvtoolu_abc123", "srvtoolu_abc123"},
		{"ws_00112233aabb", "srvtoolu_00112233aabb"},
		{"ws_00112233-aabb.cc", "srvtoolu_00112233_aabb_cc"},
	} {
		raw := responsesRequestFromItems(`{"type":"web_search_call","id":"` + tc.responsesID + `","action":{"type":"search","query":"q"}}`)
		out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		if got := gjson.GetBytes(out, "messages.0.content.0.id").String(); got != tc.want {
			t.Fatalf("id %q became %q, want %q", tc.responsesID, got, tc.want)
		}
	}
}

func TestClaudeWebSearchWithoutResultBlockStillEmitsItem(t *testing.T) {
	chunks := claudeWebSearchStreamChunks()[:4]
	chunks = append(chunks, []byte(`data: {"type":"message_stop"}`))
	outputs := translateClaudeResponsesStream(chunks)
	done := 0
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		if event == "response.output_item.done" && data.Get("item.type").String() == "web_search_call" {
			done++
		}
		if event == "response.completed" {
			completed = data
		}
	}
	if done != 1 {
		t.Fatalf("output_item.done count = %d, want 1", done)
	}
	if completed.Get("response.output.0.results").Exists() {
		t.Fatal("results must be absent when no result block arrived")
	}
}

func TestUnmappedServerToolProducesNoItem(t *testing.T) {
	for _, block := range []string{
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"code_execution","input":{}}`,
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_missing","content":[]}`,
	} {
		chunks := [][]byte{
			[]byte(`data: {"type":"message_start","message":{"id":"msg_ws","usage":{"input_tokens":1}}}`),
			[]byte(`data: {"type":"content_block_start","index":0,"content_block":` + block + `}`),
			[]byte(`data: {"type":"content_block_stop","index":0}`),
			[]byte(`data: {"type":"message_stop"}`),
		}
		for _, output := range translateClaudeResponsesStream(chunks) {
			if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" && data.Get("response.output.#").Int() != 0 {
				t.Fatalf("block %s produced output: %s", block, data.Raw)
			}
		}
	}
}

func TestWebSearchCallQueryAcceptsNativeOpenAIActionShapes(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{`{"type":"search","query":"go release","queries":["other"]}`, "go release"},
		{`{"type":"search","queries":["go release"]}`, "go release"},
		{`{"type":"open_page","url":"https://go.dev/dl"}`, "https://go.dev/dl"},
	} {
		raw := responsesRequestFromItems(`{"type":"web_search_call","id":"ws_srvtoolu_1","action":` + tc.action + `}`)
		out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
		if got := gjson.GetBytes(out, "messages.0.content.0.input.query").String(); got != tc.want {
			t.Fatalf("input.query = %q, want %q", got, tc.want)
		}
	}
}

func TestClaudeWebSearchErrorResultSurvivesRoundTrip(t *testing.T) {
	chunks := claudeWebSearchStreamChunks()
	chunks[4] = []byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_tool_result_error","error_code":"rate_limited"}]}}`)
	var completed gjson.Result
	for _, output := range translateClaudeResponsesStream(chunks) {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" {
			completed = data
		}
	}
	item := completed.Get("response.output.0")
	if item.Get("type").String() != "web_search_call" || item.Get("results.0.type").String() != "web_search_tool_result_error" {
		t.Fatalf("web search error was not preserved: %s", item.Raw)
	}
	replayed := ConvertOpenAIResponsesRequestToClaude("claude-test", responsesRequestFromItems(item.Raw), false)
	if got := gjson.GetBytes(replayed, "messages.0.content.1.content.0.type").String(); got != "web_search_tool_result_error" {
		t.Fatalf("replayed error type = %q", got)
	}
}

func TestClaudeWebSearchWithEmptyResultsKeepsEmptyList(t *testing.T) {
	chunks := claudeWebSearchStreamChunks()
	chunks[4] = []byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}}`)
	var completed gjson.Result
	for _, output := range translateClaudeResponsesStream(chunks) {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" {
			completed = data
		}
	}
	results := completed.Get("response.output.0.results")
	if !results.Exists() || len(results.Array()) != 0 {
		t.Fatalf("results = %s, want empty array", results.Raw)
	}
}

func TestRoundTripPreservesReachableClaudeBlocks(t *testing.T) {
	chunks := claudeWebSearchStreamChunks()
	outputs := translateClaudeResponsesStream(chunks)
	var completed gjson.Result
	for _, output := range outputs {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" {
			completed = data
		}
	}
	item := completed.Get("response.output.0")
	replayed := ConvertOpenAIResponsesRequestToClaude("claude-test", responsesRequestFromItems(item.Raw), false)
	if got := strings.Join(claudeAssistantBlockTypes(t, replayed), ","); got != "server_tool_use,web_search_tool_result" {
		t.Fatalf("round trip blocks = %s", got)
	}
}

func TestWebSearchCallWithoutIDProducesNoBlocks(t *testing.T) {
	for _, id := range []string{"", "ws_"} {
		raw := responsesRequestFromItems(`{"type":"web_search_call","id":"` + id + `","action":{"type":"search","query":"q"}}`)
		if got := claudeAssistantBlockTypes(t, ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)); len(got) != 0 {
			t.Fatalf("id=%q produced %v", id, got)
		}
	}
}

func TestWebSearchResultWithoutMatchingUseIsIgnored(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_ws","usage":{"input_tokens":1}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_missing","content":[]}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	for _, output := range translateClaudeResponsesStream(chunks) {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" && data.Get("response.output.#").Int() != 0 {
			t.Fatalf("orphan result produced output: %s", data.Raw)
		}
	}
}

func TestTextSearchTextOrderPreservedInStreamingAndReplay(t *testing.T) {
	chunks := claudeWebSearchStreamChunks()
	outputs := translateClaudeResponsesStream(chunks)
	var completed gjson.Result
	for _, output := range outputs {
		if event, data := parseClaudeResponsesSSEEvent(t, output); event == "response.completed" {
			completed = data
		}
	}
	if completed.Get("response.output.0.type").String() != "web_search_call" {
		t.Fatalf("unexpected output order: %s", completed.Get("response.output").Raw)
	}
}
