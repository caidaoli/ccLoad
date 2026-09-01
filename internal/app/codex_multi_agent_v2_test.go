package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOptimizeCodexMultiAgentV2RequestRenamesAndSanitizesTools(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","description":"Spawns an agent to work on a task.","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]}]
	}`)
	headers := http.Header{"User-Agent": []string{"codex_cli_rs/0.145.0"}}
	got, optimized := optimizeCodexMultiAgentV2Request(headers, payload, []string{"gpt-5.5", "claude-sonnet-4-6"})
	if !optimized {
		t.Fatal("request was not optimized")
	}
	if name := gjsonString(got, "input.0.tools.0.name"); name != codexOptimizedCollaboration {
		t.Fatalf("namespace = %q, want %q", name, codexOptimizedCollaboration)
	}
	if gjsonExists(got, "input.0.tools.0.tools.0.parameters.properties.message.encrypted") {
		t.Fatal("collaboration message encrypted marker was not removed")
	}
	description := gjsonString(got, "input.0.tools.0.tools.0.description")
	if !strings.Contains(description, "`gpt-5.5`") || !strings.Contains(description, "`claude-sonnet-4-6`") {
		t.Fatalf("model overrides missing from description: %q", description)
	}
}

func TestRewriteCodexMultiAgentV2InputConvertsAgentMessages(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"worker result"}]}]}`)
	got := rewriteCodexMultiAgentV2Input(http.Header{"User-Agent": []string{"Codex Desktop/0.146.0"}}, payload)
	if gotType := gjsonString(got, "input.0.type"); gotType != "message" {
		t.Fatalf("input type = %q, want message", gotType)
	}
	if gotRole := gjsonString(got, "input.0.role"); gotRole != "user" {
		t.Fatalf("input role = %q, want user", gotRole)
	}
	if gotText := gjsonString(got, "input.0.content.0.text"); gotText != "worker result" {
		t.Fatalf("input text = %q, want worker result", gotText)
	}
}

func TestPrepareCodexMultiAgentV2ToolsKeepsNamespace(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","description":"Spawns an agent.","parameters":{"properties":{"message":{"encrypted":true}}}}]}]}`)
	got := prepareCodexMultiAgentV2Tools(http.Header{"User-Agent": []string{"codex-tui/0.145.0"}}, payload, []string{"gpt-5.5"})
	if namespace := gjsonString(got, "tools.0.name"); namespace != codexCollaborationNamespace {
		t.Fatalf("namespace = %q, want %q", namespace, codexCollaborationNamespace)
	}
	if gjsonExists(got, "tools.0.tools.0.parameters.properties.message.encrypted") {
		t.Fatal("collaboration message encrypted marker was not removed")
	}
	if !strings.Contains(gjsonString(got, "tools.0.tools.0.description"), "`gpt-5.5`") {
		t.Fatal("spawn_agent model override was not added")
	}
}

func TestRestoreCodexMultiAgentV2ResponseRestoresToolIdentity(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"type":"function_call","namespace":"collaboration-optimize","name":"collaboration-optimize__send_message","arguments":"{\"namespace\":\"collaboration-optimize\"}"}`)
	got := string(restoreCodexMultiAgentV2Response(payload, true))
	want := `{"type":"function_call","namespace":"collaboration","name":"collaboration__send_message","arguments":"{\"namespace\":\"collaboration-optimize\"}"}`
	if got != want {
		t.Fatalf("response was re-encoded or tool identity changed unexpectedly: got %s, want %s", got, want)
	}
	if !strings.Contains(got, `"namespace":"collaboration"`) || !strings.Contains(got, `"name":"collaboration__send_message"`) {
		t.Fatalf("tool identity was not restored: %s", got)
	}
	if !strings.Contains(got, `collaboration-optimize`) {
		t.Fatal("arguments were unexpectedly rewritten")
	}

	event := restoreCodexMultiAgentV2SSEEvent([]byte("event: response.output_item.done\ndata: "+string(payload)+"\n\n"), true)
	if !strings.Contains(string(event), `"name":"collaboration__send_message"`) {
		t.Fatalf("SSE tool identity was not restored: %s", event)
	}
}

func TestRestoreCodexMultiAgentV2ResponsePreservesNestedOutputOrder(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"response":{"output":[{"id":"call-1","type":"function_call","arguments":"{\"name\":\"collaboration-optimize\"}","namespace":"collaboration-optimize","name":"collaboration-optimize__spawn_agent"}]},"trace_id":"trace-1"}`)
	got := restoreCodexMultiAgentV2Response(payload, true)
	want := `{"response":{"output":[{"id":"call-1","type":"function_call","arguments":"{\"name\":\"collaboration-optimize\"}","namespace":"collaboration","name":"collaboration__spawn_agent"}]},"trace_id":"trace-1"}`
	if string(got) != want {
		t.Fatalf("nested response was re-encoded or restored incorrectly: got %s, want %s", got, want)
	}
}

func TestCodexMultiAgentV2SkipsNonCodexClientsAndReservedNamespace(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"tools":[{"type":"namespace","name":"collaboration-optimize","tools":[{"type":"function","name":"send_message"}]}]}`)
	if got, optimized := optimizeCodexMultiAgentV2Request(http.Header{"User-Agent": []string{"curl/8.0"}}, payload, []string{"gpt-5.5"}); optimized || string(got) != string(payload) {
		t.Fatalf("non-Codex caller changed payload: optimized=%v payload=%s", optimized, got)
	}
	if got, optimized := optimizeCodexMultiAgentV2Request(http.Header{"User-Agent": []string{"codex_cli_rs"}}, payload, []string{"gpt-5.5"}); optimized || string(got) != string(payload) {
		t.Fatalf("reserved namespace conflict changed payload: optimized=%v payload=%s", optimized, got)
	}
}

func gjsonString(payload []byte, path string) string {
	return gjson.GetBytes(payload, path).String()
}

func gjsonExists(payload []byte, path string) bool {
	return gjson.GetBytes(payload, path).Exists()
}
