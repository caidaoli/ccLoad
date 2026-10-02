package responses

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func claudeWireToolNames(t *testing.T, request []byte) []string {
	t.Helper()
	translated := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", request, false)
	var names []string
	for _, tool := range gjson.GetBytes(translated, "tools").Array() {
		name := tool.Get("name").String()
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(name) {
			t.Fatalf("invalid Claude tool name %q in %s", name, translated)
		}
		names = append(names, name)
	}
	return names
}

func assertClaudeToolNameRestored(t *testing.T, request []byte, wireName, expectedName, namespace, kind string) {
	t.Helper()
	response := []byte(fmt.Sprintf(`{"type":"message","id":"msg_names","content":[{"type":"tool_use","id":"call_names","name":%q,"input":{"input":"payload"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, wireName))
	converted := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-sonnet-4-5", request, nil, response, nil)
	item := gjson.GetBytes(converted, "output.0")
	if item.Get("name").String() != expectedName || item.Get("namespace").String() != namespace || item.Get("type").String() != kind {
		t.Fatalf("tool identity was not restored: %s", converted)
	}
}

func TestBuildClaudeToolNames_RoundTripAndStability(t *testing.T) {
	identities := []struct{ name, namespace, kind string }{
		{"normal_tool", "", "function_call"},
		{"very_long_tool_name_that_exceeds_sixty_four_characters_and_needs_truncation_alpha", "", "function_call"},
		{"very_long_tool_name_that_exceeds_sixty_four_characters_and_needs_truncation_beta", "", "function_call"},
		{"custom.tool.with.dots", "", "custom_tool_call"},
		{"get_something", "mcp__my_server", "function_call"},
	}
	request := []byte(`{"tools":[{"type":"function","name":"normal_tool"},{"type":"function","name":"very_long_tool_name_that_exceeds_sixty_four_characters_and_needs_truncation_alpha"},{"type":"function","name":"very_long_tool_name_that_exceeds_sixty_four_characters_and_needs_truncation_beta"},{"type":"custom","name":"custom.tool.with.dots"},{"type":"namespace","name":"mcp__my_server","tools":[{"type":"function","name":"get_something"}]}],"input":[]}`)
	names := claudeWireToolNames(t, request)
	if len(names) != len(identities) || names[0] != "normal_tool" || names[4] != "mcp__my_server__get_something" {
		t.Fatalf("unexpected declared tools: %v", names)
	}
	seen := map[string]bool{}
	for index, identity := range identities {
		if seen[names[index]] {
			t.Fatalf("tool names collide: %v", names)
		}
		seen[names[index]] = true
		assertClaudeToolNameRestored(t, request, names[index], identity.name, identity.namespace, identity.kind)
	}
	if repeated := claudeWireToolNames(t, request); strings.Join(repeated, ",") != strings.Join(names, ",") {
		t.Fatalf("unstable wire names: %v vs %v", names, repeated)
	}
}

func TestBuildClaudeToolNames_DeclarationOrderInvariance(t *testing.T) {
	first := "mcp__example_apps__acme_inventory_service__acme_inventory_service_get_item_prices"
	second := "mcp__example_apps__acme_inventory_service__acme_inventory_service_get_item_metrics"
	request := func(a, b string) []byte {
		return []byte(fmt.Sprintf(`{"tools":[{"type":"function","name":%q},{"type":"function","name":%q}],"input":[]}`, a, b))
	}
	a := claudeWireToolNames(t, request(first, second))
	b := claudeWireToolNames(t, request(second, first))
	if len(a) != 2 || len(b) != 2 || a[0] != b[1] || a[1] != b[0] {
		t.Fatalf("declaration order changed wire identity: %v vs %v", a, b)
	}
}

func TestBuildClaudeToolNames_SingleLongName(t *testing.T) {
	name := "a_single_very_long_tool_name_that_exceeds_sixty_four_characters_cleanly"
	request := []byte(fmt.Sprintf(`{"tools":[{"type":"function","name":%q}],"input":[]}`, name))
	names := claudeWireToolNames(t, request)
	if len(names) != 1 {
		t.Fatalf("missing long-name tool: %v", names)
	}
	assertClaudeToolNameRestored(t, request, names[0], name, "", "function_call")
}

func TestBuildClaudeToolNames_CustomToolCollision(t *testing.T) {
	first := "custom__long_namespace_path_exceeding_sixty_four_characters_long__operation_query_v1_execute_handler"
	second := "custom__long_namespace_path_exceeding_sixty_four_characters_long__operation_query_v1_execute_stream"
	request := []byte(fmt.Sprintf(`{"tools":[{"type":"custom","name":%q},{"type":"custom","name":%q}],"input":[]}`, first, second))
	names := claudeWireToolNames(t, request)
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("custom tool collision: %v", names)
	}
	assertClaudeToolNameRestored(t, request, names[0], first, "", "custom_tool_call")
	assertClaudeToolNameRestored(t, request, names[1], second, "", "custom_tool_call")
}

func TestBuildClaudeToolNames_DeclaredToolsPrecedeHistory(t *testing.T) {
	request := []byte(`{"tools":[{"type":"function","name":"tool_x"}],"input":[{"type":"function_call","call_id":"call_history","name":"tool.x","arguments":"{}"},{"type":"function_call_output","call_id":"call_history","output":"ok"}]}`)
	names := claudeWireToolNames(t, request)
	if len(names) != 1 || names[0] != "tool_x" {
		t.Fatalf("history replaced declared tool: %v", names)
	}
	translated := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", request, false)
	var historyName string
	for _, message := range gjson.GetBytes(translated, "messages").Array() {
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() == "tool_use" {
				historyName = block.Get("name").String()
			}
		}
	}
	if historyName == "tool_x" || !strings.HasPrefix(historyName, "tool_x_") {
		t.Fatalf("history tool identity collided: %s", translated)
	}
	assertClaudeToolNameRestored(t, request, historyName, "tool.x", "", "function_call")
}
