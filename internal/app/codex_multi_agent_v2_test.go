package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

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
	if namespace := gjsonString(got, "tools.0.name"); namespace != "collaboration" {
		t.Fatalf("namespace = %q, want collaboration", namespace)
	}
	if gjsonExists(got, "tools.0.tools.0.parameters.properties.message.encrypted") {
		t.Fatal("collaboration message encrypted marker was not removed")
	}
	if !strings.Contains(gjsonString(got, "tools.0.tools.0.description"), "`gpt-5.5`") {
		t.Fatal("spawn_agent model override was not added")
	}
}

// 非官方 Codex 客户端的请求必须原样保留：spawn_agent 不是保留名，第三方编排
// 框架也会用它，改写会篡改调用方语义并把网关模型目录写进请求体。
func TestPrepareCodexMultiAgentV2ToolsLeavesNonCodexCallersUntouched(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"tools":[{"type":"function","name":"spawn_agent","description":"Spawn a worker in my own orchestrator.","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}`)
	for _, userAgent := range []string{"curl/8.0", "openai-python/1.0", ""} {
		headers := http.Header{}
		if userAgent != "" {
			headers.Set("User-Agent", userAgent)
		}
		got := prepareCodexMultiAgentV2Tools(headers, payload, []string{"gpt-5.5", "internal-only-model"})
		if string(got) != string(payload) {
			t.Fatalf("User-Agent %q: payload was rewritten: %s", userAgent, got)
		}
	}
}

func gjsonString(payload []byte, path string) string {
	return gjson.GetBytes(payload, path).String()
}

func gjsonExists(payload []byte, path string) bool {
	return gjson.GetBytes(payload, path).Exists()
}
