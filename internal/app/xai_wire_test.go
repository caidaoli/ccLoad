package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"ccLoad/internal/protocol"
	"ccLoad/internal/xaiauth"

	"github.com/tidwall/gjson"
)

func TestBuildXAIImagesResponsesRequestValidatesConsumedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		body            string
		wantUnsupported bool
	}{
		{name: "stream type", body: `{"model":"grok-4.6","prompt":"cat","stream":"true"}`},
		{name: "response format type", body: `{"model":"grok-4.6","prompt":"cat","response_format":123}`},
		{name: "invalid n", body: `{"model":"grok-4.6","prompt":"cat","n":0}`},
		{name: "bridge n limit", body: `{"model":"grok-4.6","prompt":"cat","n":2}`, wantUnsupported: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildXAIImagesResponsesRequest([]byte(test.body), "grok-4.6")
			if err == nil {
				t.Fatal("expected validation error")
			}
			if got := errors.Is(err, errXAIImagesBridgeUnsupported); got != test.wantUnsupported {
				t.Fatalf("unsupported error = %v, want %v: %v", got, test.wantUnsupported, err)
			}
		})
	}
}

func TestBuildXAIImagesResponsesRequestAcceptsStreaming(t *testing.T) {
	t.Parallel()

	got, err := buildXAIImagesResponsesRequest(
		[]byte(`{"model":"grok-4.6","prompt":"cat","stream":true,"partial_images":2}`),
		"grok-4.6",
	)
	if err != nil {
		t.Fatalf("buildXAIImagesResponsesRequest() error = %v", err)
	}
	if !gjson.GetBytes(got, "stream").Bool() || gjson.GetBytes(got, "tools.0.partial_images").Int() != 2 {
		t.Fatalf("streaming Images request mismatch: %s", got)
	}
	if gjson.GetBytes(got, "tool_choice").String() != "required" {
		t.Fatalf("streaming Images request must require its sole xAI tool: %s", got)
	}
}

func TestTranslateXAIImagesResponsesStreamEventSupportsURLFormat(t *testing.T) {
	t.Parallel()

	partial, terminal, err := translateXAIImagesResponsesStreamEvent(
		[]byte(`event: response.image_generation_call.partial_image
data: {"type":"response.image_generation_call.partial_image","partial_image_index":1,"partial_image_b64":"cGFydGlhbA==","output_format":"webp"}

`),
		[]byte(`{"response_format":"url"}`),
	)
	if err != nil || terminal || len(partial) != 1 ||
		!strings.Contains(string(partial[0]), `"url":"data:image/webp;base64,cGFydGlhbA=="`) {
		t.Fatalf("partial URL event = %q, terminal=%v, err=%v", partial, terminal, err)
	}

	completed, terminal, err := translateXAIImagesResponsesStreamEvent(
		[]byte(`data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"ZmluYWw=","output_format":"jpeg"}],"tool_usage":{"image_gen":{"total_tokens":9}}}}

`),
		[]byte(`{"response_format":"url"}`),
	)
	if err != nil || !terminal || len(completed) != 1 ||
		!strings.Contains(string(completed[0]), `"url":"data:image/jpeg;base64,ZmluYWw="`) ||
		!strings.Contains(string(completed[0]), `"usage":{"total_tokens":9}`) {
		t.Fatalf("completed URL event = %q, terminal=%v, err=%v", completed, terminal, err)
	}
}

func TestMergeXAIImageOutputsAppendsOutputItemDoneImage(t *testing.T) {
	t.Parallel()

	got := mergeXAIImageOutputs(
		[]xaiImageGenerationOutput{{Type: "message"}},
		[]xaiImageGenerationOutput{{Type: "image_generation_call", Result: "aW1hZ2U="}},
	)
	if len(got) != 2 || got[1].Type != "image_generation_call" || got[1].Result != "aW1hZ2U=" {
		t.Fatalf("merged output = %#v", got)
	}
}

func TestTranslateXAIImagesResponsesStreamEventAcceptsCompletedBeforeOutputItem(t *testing.T) {
	t.Parallel()

	state := &xaiImagesStreamState{}
	completed, terminal, err := translateXAIImagesResponsesStreamEventWithState(
		[]byte(`data: {"type":"response.completed","response":{"output":[]}}`+"\n\n"),
		[]byte(`{"response_format":"b64_json"}`), state,
	)
	if err != nil || terminal || len(completed) != 0 || state.completed == nil {
		t.Fatalf("completed-before-item result=%q terminal=%v err=%v state=%#v", completed, terminal, err, state)
	}
	completed, terminal, err = translateXAIImagesResponsesStreamEventWithState(
		[]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","result":"aW1hZ2U=","output_format":"png"}}`+"\n\n"),
		[]byte(`{"response_format":"b64_json"}`), state,
	)
	if err != nil || !terminal || len(completed) != 1 || !strings.Contains(string(completed[0]), `"b64_json":"aW1hZ2U="`) {
		t.Fatalf("output-item-after-completed result=%q terminal=%v err=%v", completed, terminal, err)
	}
}

func TestFinalizeXAIResponsesBodyAppliesProviderContract(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"client-model",
		"stream":false,
		"previous_response_id":"resp-old",
		"prompt_cache_retention":"24h",
		"safety_identifier":"unsafe",
		"stream_options":{"include_usage":true},
		"presence_penalty":0.5,
		"frequency_penalty":0.25,
		"stop":["END"],
		"reasoning":{"effort":"xhigh","summary":"auto"},
		"tools":[],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello","external_web_access":true}]}],
		"metadata":{"nested":{"external_web_access":false,"keep":"yes"}}
	}`)

	got, err := finalizeXAIResponsesBody(raw, "grok-4.5", "conv-parent")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, got)
	}
	if payload["model"] != "grok-4.5" || payload["stream"] != true || payload["prompt_cache_key"] != "conv-parent" {
		t.Fatalf("required xAI fields = %#v", payload)
	}
	for _, field := range []string{
		"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options",
		"presence_penalty", "frequency_penalty", "stop",
	} {
		if _, exists := payload[field]; exists {
			t.Fatalf("field %q survived xAI finalization: %s", field, got)
		}
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want normalized high with summary preserved", reasoning)
	}
	assertFieldOrder(t, string(got), `"model"`, `"stream"`, `"reasoning"`, `"input"`, `"metadata"`, `"prompt_cache_key"`)
	tools, _ := payload["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("tools = %#v, want no injected tools", tools)
	}
	for _, field := range []string{"tool_choice", "parallel_tool_calls"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("orphaned tool field %q survived: %s", field, got)
		}
	}
	assertNoJSONKey(t, payload, "external_web_access")
}

func TestFinalizeXAIResponsesBodyNormalizesReasoningInputItems(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"input":[
			{"type":"reasoning","summary":[],"content":null,"encrypted_content":"grok-state"},
			{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"kept"}],"encrypted_content":null},
			{"role":"user","content":"continue"}
		]
	}`)

	got, err := finalizeXAIResponsesBody(raw, "grok-4.5", "conv")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, got)
	}
	input := payload["input"].([]any)
	first := input[0].(map[string]any)
	if _, exists := first["content"]; exists {
		t.Fatalf("null reasoning content survived xAI finalization: %s", got)
	}
	if first["encrypted_content"] != "grok-state" {
		t.Fatalf("valid encrypted state changed: %#v", first)
	}
	second := input[1].(map[string]any)
	if _, exists := second["encrypted_content"]; exists {
		t.Fatalf("null encrypted_content survived xAI finalization: %s", got)
	}
	if _, exists := second["content"]; !exists {
		t.Fatalf("non-null reasoning content was removed: %s", got)
	}
}

func TestFinalizeXAIResponsesBodyPreservesExplicitTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		body         string
		wantControls bool
	}{
		{
			name:         "ordinary function tools",
			body:         `{"tools":[{"type":"function","name":"lookup"}],"tool_choice":"auto","parallel_tool_calls":true}`,
			wantControls: true,
		},
		{
			name:         "explicit native searches",
			body:         `{"tools":[{"type":"web_search"},{"type":"x_search"},{"type":"function","name":"lookup"}]}`,
			wantControls: true,
		},
		{
			name:         "ordinary function named web search",
			body:         `{"tools":[{"type":"function","name":"web_search"}],"tool_choice":{"type":"function","name":"web_search"}}`,
			wantControls: true,
		},
		{
			name:         "allowed tools choice",
			body:         `{"tools":[{"type":"function","name":"lookup"}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"lookup"}]}}`,
			wantControls: true,
		},
		{
			name: "empty tools prune orphan controls",
			body: `{"tools":[],"tool_choice":"auto","parallel_tool_calls":true}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var want map[string]any
			if err := json.Unmarshal([]byte(test.body), &want); err != nil {
				t.Fatal(err)
			}
			got, err := finalizeXAIResponsesBody([]byte(test.body), "grok-4.5", "conv")
			if err != nil {
				t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("result is not JSON: %v\n%s", err, got)
			}
			for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
				gotValue, gotExists := payload[field]
				wantValue, wantExists := want[field]
				if test.wantControls {
					if gotExists != wantExists || !reflect.DeepEqual(gotValue, wantValue) {
						t.Fatalf("%s = %#v, want %#v", field, gotValue, wantValue)
					}
				} else if gotExists {
					t.Fatalf("orphaned field %s survived: %#v", field, gotValue)
				}
			}
		})
	}
}

func TestFinalizeXAIResponsesBodyNormalizesImageGenerationByModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     string
		wantImage bool
		wantCount int
	}{
		{name: "grok 4.5 strips", model: "grok-4.5", wantCount: 1},
		{name: "grok 4.20 stays on old product line", model: "grok-4.20-0309-reasoning", wantCount: 1},
		{name: "grok 4.20 with unknown suffix stays old", model: "grok-4.20(foo)", wantCount: 1},
		{name: "grok 4.6 keeps", model: "grok-4.6", wantImage: true, wantCount: 2},
		{name: "grok 4.7 keeps", model: "grok-4.7", wantImage: true, wantCount: 2},
		{name: "provider prefix and thinking suffix", model: "xai/grok-4.6(high)", wantImage: true, wantCount: 2},
		{name: "future major keeps", model: "grok-5", wantImage: true, wantCount: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := finalizeXAIResponsesBody([]byte(`{
				"tools":[
					{"type":"image_generation","action":"generate"},
					{"type":"function","name":"lookup"}
				],
				"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"},{"type":"function","name":"lookup"}]}
			}`), test.model, "conv")
			if err != nil {
				t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			tools := payload["tools"].([]any)
			if gotCount := len(tools); gotCount != test.wantCount {
				t.Fatalf("tools length = %d, want %d; body=%s", gotCount, test.wantCount, got)
			}
			if test.wantImage {
				imageTool := tools[0].(map[string]any)
				if imageTool["type"] != "image_generation" || imageTool["action"] != "generate" {
					t.Fatalf("image_generation tool changed: %#v", imageTool)
				}
			}
			choice := payload["tool_choice"].(map[string]any)
			allowed := choice["tools"].([]any)
			if gotCount := len(allowed); gotCount != test.wantCount {
				t.Fatalf("allowed tools length = %d, want %d; body=%s", gotCount, test.wantCount, got)
			}
		})
	}
}

func TestFinalizeXAIResponsesBodyNormalizesXHighReasoningByModel(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ model, want string }{
		{"grok-4.6", "xhigh"}, {"grok-4.7", "xhigh"},
		{"grok-4.6-latest", "xhigh"}, {"grok-4.7-latest", "xhigh"},
		{"xai/grok-4.6-latest", "xhigh"}, {"grok-4.5-latest", "high"},
	} {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()
			got, err := finalizeXAIResponsesBody([]byte(`{"reasoning":{"effort":"xhigh"},"input":"hello"}`), test.model, "conv")
			if err != nil {
				t.Fatal(err)
			}
			if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != test.want {
				t.Fatalf("reasoning.effort = %q, want %q: %s", effort, test.want, got)
			}
		})
	}
}

func TestXAIResponsesToolsNamespaceChoiceAndRetry(t *testing.T) {
	t.Parallel()

	for _, choice := range []string{
		`{"type":"function","namespace":"functions","name":"exec"}`,
		`{"type":"allowed_tools","mode":"required","tools":[{"type":"function","namespace":"functions","name":"exec"}]}`,
	} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			original := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}],"tool_choice":` + choice + `,"input":[{"type":"function_call","namespace":"functions","name":"exec","call_id":"call-1","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`)
			wire, plan, err := prepareXAIResponsesToolsRequest(original, nil)
			if err != nil {
				t.Fatal(err)
			}
			wire, err = finalizeXAIResponsesBody(wire, "grok-4.6", "session")
			if err != nil {
				t.Fatal(err)
			}
			root := gjson.ParseBytes(wire)
			name := root.Get("tools.0.name").String()
			selected := root.Get("tool_choice")
			if selected.Get("type").String() == "allowed_tools" {
				selected = selected.Get("tools.0")
			}
			if root.Get("tools.0.type").String() != "function" || selected.Get("name").String() != name || selected.Get("namespace").Exists() || selected.Get("type").String() != "function" {
				t.Fatalf("forced namespace choice no longer selects its declaration: %s", wire)
			}
			if root.Get("input.0.name").String() != name || root.Get("input.0.namespace").Exists() || root.Get("input.0.call_id").String() != root.Get("input.1.call_id").String() {
				t.Fatalf("history diverged from wire declarations: %s", wire)
			}
			for _, retryBody := range [][]byte{original, wire} {
				retry, retryPlan, err := prepareXAIResponsesToolsRequest(retryBody, plan)
				if err != nil || retryPlan != plan || gjson.GetBytes(retry, "tools.0.name").String() != name {
					t.Fatalf("retry mapping changed: %s, %v", retry, err)
				}
				response := fmt.Sprintf(`{"object":"response","output":[{"type":"function_call","name":%q,"call_id":"new-call","arguments":"{}"}]}`, name)
				restored := readXAIToolsResponse(t, retryPlan, response, false)
				item := gjson.Get(restored, "output.0")
				if item.Get("name").String() != "exec" || item.Get("namespace").String() != "functions" || item.Get("call_id").String() != "new-call" {
					t.Fatalf("namespace identity lost after retry: %s", restored)
				}
			}
		})
	}
}

func TestXAIResponsesToolsCustomRoundTrip(t *testing.T) {
	t.Parallel()
	const input = "*** Begin Patch\n+你好 \"quoted\"\\path\n*** End Patch"
	raw := fmt.Sprintf(`{"tools":[{"type":"namespace","name":"editing","tools":[{"type":"custom","name":"apply_patch","description":"Edit files","format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"},"defer_loading":true}]}],"tool_choice":{"type":"custom","namespace":"editing","name":"apply_patch"},"input":[{"type":"custom_tool_call","id":"ctc_old","namespace":"editing","name":"apply_patch","call_id":"call-old","input":%q},{"type":"custom_tool_call_output","call_id":"call-old","output":[{"type":"input_text","text":"done"}]}]}`, input)
	wire, plan, err := prepareXAIResponsesToolsRequest([]byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(wire)
	name := root.Get("tools.0.name").String()
	if root.Get("tools.0.type").String() != "function" || root.Get("tools.0.parameters.properties.input.type").String() != "string" || root.Get("tools.0.format").Exists() || root.Get("tools.0.defer_loading").Exists() {
		t.Fatalf("custom declaration was not lowered: %s", wire)
	}
	if !strings.Contains(root.Get("tools.0.description").String(), "start: PATCH") || root.Get("tool_choice.name").String() != name || root.Get("tool_choice.type").String() != "function" {
		t.Fatalf("custom grammar/choice semantics lost: %s", wire)
	}
	call := root.Get("input.0")
	if call.Get("type").String() != "function_call" || call.Get("name").String() != name || gjson.Get(call.Get("arguments").String(), "input").String() != input || call.Get("id").String() != "fc_old" || call.Get("input").Exists() {
		t.Fatalf("custom replay failed: %s", wire)
	}
	if root.Get("input.1.type").String() != "function_call_output" || !root.Get("input.1.output").IsArray() || root.Get("input.1.call_id").String() != "call-old" {
		t.Fatalf("custom output contract lost: %s", wire)
	}
	arguments := `{"input":` + jsonEscapedString(input) + `}`
	response := fmt.Sprintf(`{"object":"response","output":[{"type":"function_call","id":"fc_new","name":%q,"call_id":"call-new","arguments":%q,"status":"completed"}]}`, name, arguments)
	restored := readXAIToolsResponse(t, plan, response, false)
	item := gjson.Get(restored, "output.0")
	if item.Get("type").String() != "custom_tool_call" || item.Get("name").String() != "apply_patch" || item.Get("namespace").String() != "editing" || item.Get("input").String() != input || item.Get("arguments").Exists() || item.Get("id").String() != "ctc_new" || item.Get("call_id").String() != "call-new" {
		t.Fatalf("custom roundtrip changed its identity/input: %s", restored)
	}
}

func TestXAIResponsesToolsSearchAndDiscoveries(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"tools":[{"type":"function","name":"tool_search"},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"needle":{"type":"string"}},"required":["needle"]}}],"tool_choice":{"type":"tool_search"},"input":[{"type":"tool_search_call","id":"tsc_old","call_id":"search-old","execution":"client","arguments":{"needle":"files"}},{"type":"tool_search_output","call_id":"search-old","execution":"client","status":"completed","tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","defer_loading":true,"parameters":{"type":"object"}}]}]},{"type":"function_call","namespace":"files","name":"read","call_id":"read-old","arguments":"{}"}]}`)
	wire, plan, err := prepareXAIResponsesToolsRequest(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(wire)
	search := root.Get("tools.1")
	searchName := search.Get("name").String()
	if search.Get("type").String() != "function" || searchName == "tool_search" || search.Get("parameters.required.0").String() != "needle" || root.Get("tool_choice.name").String() != searchName {
		t.Fatalf("search declaration/choice collided with ordinary function: %s", wire)
	}
	if root.Get("input.0.type").String() != "function_call" || root.Get("input.0.id").String() != "fc_old" || root.Get("input.0.name").String() != searchName || !gjson.Parse(root.Get("input.0.arguments").String()).IsObject() {
		t.Fatalf("search call history not lowered: %s", wire)
	}
	output := root.Get("input.1")
	if output.Get("type").String() != "function_call_output" || output.Get("call_id").String() != "search-old" || !gjson.Parse(output.Get("output").String()).IsArray() || output.Get("tools").Exists() || output.Get("execution").Exists() || output.Get("status").Exists() {
		t.Fatalf("search results not preserved as function output: %s", wire)
	}
	if len(root.Get("tools").Array()) != 3 || root.Get("tools.2.name").String() != root.Get("input.2.name").String() || root.Get("input.2.namespace").Exists() || root.Get("tools.2.defer_loading").Exists() {
		t.Fatalf("discovered declarations did not join the same mapping: %s", wire)
	}
	response := fmt.Sprintf(`{"object":"response","output":[{"type":"function_call","id":"fc_new","name":%q,"call_id":"search-new","arguments":"{\"needle\":\"files\"}","status":"completed"},{"type":"function_call","name":"tool_search","call_id":"ordinary-call","arguments":"{}"}]}`, searchName)
	restored := readXAIToolsResponse(t, plan, response, false)
	item := gjson.Get(restored, "output.0")
	if item.Get("type").String() != "tool_search_call" || item.Get("id").String() != "tsc_new" || item.Get("name").Exists() || item.Get("execution").String() != "client" || !item.Get("arguments").IsObject() || item.Get("arguments.needle").String() != "files" || item.Get("call_id").String() != "search-new" {
		t.Fatalf("search roundtrip contract failed: %s", restored)
	}
	if gjson.Get(restored, "output.1.type").String() != "function_call" || gjson.Get(restored, "output.1.name").String() != "tool_search" {
		t.Fatalf("ordinary search-named function was misidentified: %s", restored)
	}
}

func TestXAIResponsesToolsRejectInvalidHistory(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"tools":[{"type":"tool_search","execution":"server"}]}`,
		`{"tools":[{"type":"tool_search","execution":"client","parameters":[]}]}`,
		`{"input":[{"type":"tool_search_call","call_id":"c","arguments":[]}]}`,
		`{"input":[{"type":"tool_search_output","tools":[]}]}`,
		`{"input":[{"type":"tool_search_output","call_id":"c"}]}`,
		`{"input":[{"type":"custom_tool_call","name":"exec","call_id":"c","input":42}]}`,
	} {
		_, _, err := prepareXAIResponsesToolsRequest([]byte(raw), nil)
		var invalid *protocol.RequestTranslationError
		if !errors.As(err, &invalid) {
			t.Fatalf("invalid history must produce local request error, got %v: %s", err, raw)
		}
	}
}

func TestXAIResponsesToolsCollisionAndLengthRoundTrip(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("long", 20)
	raw := fmt.Sprintf(`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"run"}]},{"type":"function","name":"ns__run"},{"type":"custom","name":"exec"},{"type":"function","name":"exec"},{"type":"namespace","name":%q,"tools":[{"type":"function","name":%q}]},{"type":"namespace","name":"ns__run","tools":[{"type":"function","name":"more"}]},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"run__more"}]}]}`, long, long)
	wire, plan, err := prepareXAIResponsesToolsRequest([]byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	var output []string
	for _, tool := range gjson.GetBytes(wire, "tools").Array() {
		name := tool.Get("name").String()
		if seen[name] || name == "" || len(name) > 64 || strings.IndexFunc(name, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
		}) >= 0 {
			t.Fatalf("invalid or ambiguous wire name %q: %s", name, wire)
		}
		seen[name] = true
		output = append(output, fmt.Sprintf(`{"type":"function_call","name":%q,"call_id":%q,"arguments":"{\"input\":\"text\"}"}`, name, name))
	}
	restored := readXAIToolsResponse(t, plan, `{"object":"response","output":`+joinJSONRaw(output)+`}`, false)
	want := []struct{ kind, namespace, name string }{
		{"function_call", "ns", "run"}, {"function_call", "", "ns__run"},
		{"custom_tool_call", "", "exec"}, {"function_call", "", "exec"},
		{"function_call", long, long}, {"function_call", "ns__run", "more"}, {"function_call", "ns", "run__more"},
	}
	for i, item := range gjson.Get(restored, "output").Array() {
		if item.Get("type").String() != want[i].kind || item.Get("namespace").String() != want[i].namespace || item.Get("name").String() != want[i].name {
			t.Fatalf("collision restoration[%d] wrong: %s", i, restored)
		}
	}
}

func TestXAIResponsesToolsSSEIncrementalCustomAndSearch(t *testing.T) {
	t.Parallel()
	wire, plan, err := prepareXAIResponsesToolsRequest([]byte(`{"tools":[{"type":"namespace","name":"editing","tools":[{"type":"custom","name":"patch"}]},{"type":"tool_search","execution":"client"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	customName, searchName := gjson.GetBytes(wire, "tools.0.name").String(), gjson.GetBytes(wire, "tools.1.name").String()
	var source strings.Builder
	sequence := 0
	writeEvent := func(payload map[string]any) {
		payload["sequence_number"] = sequence
		sequence++
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&source, "event: %s\ndata: %s\n\n", payload["type"], encoded)
	}
	writeEvent(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_custom", "call_id": "call-custom", "name": customName, "arguments": ""}})
	for _, delta := range []string{`{"input":"first`, `\n\"quote\"\\path `, `\uD83D`, `\uDE3C`, `"}`} {
		writeEvent(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_custom", "delta": delta})
	}
	arguments := `{"input":"first\n\"quote\"\\path \uD83D\uDE3C"}`
	writeEvent(map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_custom", "arguments": arguments})
	writeEvent(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_custom", "call_id": "call-custom", "name": customName}})
	writeEvent(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_search", "call_id": "call-search", "name": searchName, "arguments": ""}})
	writeEvent(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "fc_search", "delta": `{"query":"files"}`})
	writeEvent(map[string]any{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": "fc_search", "arguments": `{"query":"files"}`})
	writeEvent(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_search", "call_id": "call-search", "name": searchName}})
	writeEvent(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "function_call", "id": "fc_custom", "call_id": "call-custom", "name": customName, "arguments": arguments}, map[string]any{"type": "function_call", "id": "fc_search", "call_id": "call-search", "name": searchName, "arguments": `{"query":"files"}`}}}})
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(source.String())))}
	prepareXAIResponsesToolsResponse(response, plan, true)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var deltaInput strings.Builder
	seenCustomDone, seenSearchDone := false, false
	for i, frame := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("invalid SSE frame: %q", frame)
		}
		payload := strings.TrimPrefix(lines[1], "data: ")
		if !gjson.Valid(payload) {
			t.Fatalf("invalid SSE JSON: %q", payload)
		}
		event := gjson.Parse(payload)
		typ := event.Get("type").String()
		if strings.TrimPrefix(lines[0], "event: ") != typ || event.Get("sequence_number").Int() != int64(i) {
			t.Fatalf("SSE event/sequence disagrees with payload: %s", frame)
		}
		if strings.HasPrefix(typ, "response.function_call_arguments.") {
			t.Fatalf("lowered argument envelope leaked: %s", frame)
		}
		switch typ {
		case "response.custom_tool_call_input.delta":
			deltaInput.WriteString(event.Get("delta").String())
			if event.Get("item_id").String() != "ctc_custom" || event.Get("namespace").String() != "editing" {
				t.Fatalf("custom delta identity lost: %s", frame)
			}
		case "response.custom_tool_call_input.done":
			seenCustomDone = event.Get("input").String() == "first\n\"quote\"\\path 😼"
		case "response.output_item.done":
			if item := event.Get("item"); item.Get("type").String() == "tool_search_call" {
				seenSearchDone = item.Get("arguments.query").String() == "files" && item.Get("call_id").String() == "call-search" && item.Get("id").String() == "tsc_search"
			}
		case "response.completed":
			if event.Get("response.output.0.type").String() != "custom_tool_call" || event.Get("response.output.1.type").String() != "tool_search_call" {
				t.Fatalf("terminal response identities lost: %s", frame)
			}
		}
	}
	if deltaInput.String() != "first\n\"quote\"\\path 😼" || !seenCustomDone || !seenSearchDone {
		t.Fatalf("incremental lifecycle incomplete: input=%q custom=%v search=%v\n%s", deltaInput.String(), seenCustomDone, seenSearchDone, body)
	}
}

func readXAIToolsResponse(t *testing.T, plan *xaiResponsesToolsPlan, body string, streaming bool) string {
	t.Helper()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
	prepareXAIResponsesToolsResponse(response, plan, streaming)
	defer func() { _ = response.Body.Close() }()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(result) {
		t.Fatalf("restored response is invalid JSON: %s", result)
	}
	return string(result)
}

func TestFinalizeXAIResponsesBodyPromotesAdditionalImageGenerationTools(t *testing.T) {
	t.Parallel()

	got, err := finalizeXAIResponsesBody([]byte(`{
		"input":[
			{"role":"user","content":"draw a cat"},
			{"type":"additional_tools","tools":[{"type":"image_generation","action":"generate"}]}
		],
		"tool_choice":{"type":"image_generation"}
	}`), "grok-4.6", "conv")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v, want additional_tools removed", input)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "image_generation" {
		t.Fatalf("tools = %#v, want promoted image_generation", tools)
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "allowed_tools" || choice["mode"] != "required" {
		t.Fatalf("tool_choice = %#v, want required allowed_tools", choice)
	}

	got, err = finalizeXAIResponsesBody([]byte(`{
		"input":[
			{"role":"user","content":"draw a cat"},
			{"type":"additional_tools","tools":[{"type":"image_generation"}]}
		],
		"tool_choice":{"type":"image_generation"}
	}`), "grok-4.5", "conv")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() old model error = %v", err)
	}
	payload = nil
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["tools"]; exists {
		t.Fatalf("unsupported promoted tools survived: %s", got)
	}
	if _, exists := payload["tool_choice"]; exists {
		t.Fatalf("unsupported promoted tool_choice survived: %s", got)
	}
}

func TestFinalizeXAIResponsesBodyPrunesOrphanedAllowedTools(t *testing.T) {
	t.Parallel()

	got, err := finalizeXAIResponsesBody([]byte(`{
		"tools":[{"type":"function","name":"lookup"},{"type":"image_generation"}],
		"tool_choice":{"type":"allowed_tools","tools":[{"type":"image_generation"},{"type":"function","name":"missing"}]}
	}`), "grok-4.5", "conv")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["tool_choice"]; exists {
		t.Fatalf("orphaned tool_choice survived: %s", got)
	}
}

func TestFinalizeXAIResponsesBodyRewritesForcedImageGenerationChoice(t *testing.T) {
	t.Parallel()

	got, err := finalizeXAIResponsesBody([]byte(`{
		"tools":[{"type":"image_generation","action":"generate"}],
		"tool_choice":{"type":"image_generation"}
	}`), "grok-4.6", "conv")
	if err != nil {
		t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "allowed_tools" || choice["mode"] != "required" {
		t.Fatalf("tool_choice = %#v, want required allowed_tools", choice)
	}
	allowed := choice["tools"].([]any)
	if len(allowed) != 1 || allowed[0].(map[string]any)["type"] != "image_generation" {
		t.Fatalf("tool_choice.tools = %#v, want image_generation", allowed)
	}
}

func TestFinalizeXAIResponsesBodyDropsUnsupportedReasoning(t *testing.T) {
	t.Parallel()

	for _, modelName := range []string{"grok-composer-2.5-fast", "grok-4.20-0309-non-reasoning", "grok-build-0.1"} {
		modelName := modelName
		t.Run(modelName, func(t *testing.T) {
			t.Parallel()
			got, err := finalizeXAIResponsesBody(
				[]byte(`{"model":"old","input":"hi","reasoning":{"effort":"high"}}`),
				modelName,
				"conv",
			)
			if err != nil {
				t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			if _, exists := payload["reasoning"]; exists {
				t.Fatalf("unsupported reasoning survived: %s", got)
			}
		})
	}
}

func TestInjectXAIResponsesHeadersRebuildsIdentityAfterRules(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{
		"Authorization":             {"Bearer client-secret"},
		"X-Api-Key":                 {"client-key"},
		"X-Goog-Api-Key":            {"google-key"},
		"X-Xai-Token-Auth":          {"wrong"},
		"X-Grok-Client-Version":     {"wrong"},
		"User-Agent":                {"wrong"},
		"X-Grok-Client-Identifier":  {"must-be-removed"},
		"X-Authenticateresponse":    {"must-be-removed"},
		"X-Grok-Client-Mode":        {"wrong"},
		"X-Grok-Conv-Id":            {"client-conversation"},
		"Session-Id":                {"raw-session"},
		"Session_id":                {"raw-session-legacy"},
		"Originator":                {"codex-tui"},
		"Chatgpt-Account-Id":        {"account"},
		"Content-Type":              {"text/plain"},
		"Accept":                    {"application/json"},
		"X-Unrelated-Custom-Header": {"preserved"},
	}

	injectXAIResponsesHeaders(req, "access-token", "conv-derived")

	want := map[string]string{
		"Authorization":                       "Bearer access-token",
		"Content-Type":                        "application/json",
		"Accept":                              "application/json, text/event-stream",
		xaiauth.CLITokenAuthHeader:            xaiauth.CLITokenAuthValue,
		xaiauth.CLIClientVersionHeader:        xaiauth.CLIClientVersion,
		xaiauth.CLIClientIdentifierHeader:     xaiauth.CLIClientIdentifierValue,
		xaiauth.CLIAuthenticateResponseHeader: xaiauth.CLIAuthenticateResponseValue,
		"User-Agent":                          xaiauth.CLIUserAgent,
		xaiauth.CLIClientModeHeader:           xaiauth.CLIClientMode,
		"x-grok-conv-id":                      "conv-derived",
		"X-Unrelated-Custom-Header":           "preserved",
	}
	for name, value := range want {
		if got := req.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{
		"X-Api-Key", "x-goog-api-key", "Session-Id", "Session_id", "Originator", "ChatGPT-Account-ID",
	} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("conflicting header %s survived with %q", name, got)
		}
	}
}

func TestDeriveXAIExecutionIDStableAndThreadIsolated(t *testing.T) {
	t.Parallel()

	parentHeaders := http.Header{"Session-Id": {"session"}, "Thread-Id": {"parent"}}
	childHeaders := http.Header{"Session-Id": {"session"}, "Thread-Id": {"child"}}
	first := deriveXAIExecutionID("subject-a", parentHeaders)
	second := deriveXAIExecutionID("subject-a", parentHeaders)
	child := deriveXAIExecutionID("subject-a", childHeaders)
	otherSubject := deriveXAIExecutionID("subject-b", parentHeaders)
	if first == "" || first != second {
		t.Fatalf("stable execution ID mismatch: first=%q second=%q", first, second)
	}
	if child == first || otherSubject == first {
		t.Fatalf("execution identity not isolated: parent=%q child=%q other=%q", first, child, otherSubject)
	}
	claudeHeaders := http.Header{"X-Claude-Code-Session-Id": {"claude-session"}}
	claudeFirst := deriveXAIExecutionID("subject-a", claudeHeaders)
	claudeSecond := deriveXAIExecutionID("subject-a", claudeHeaders)
	claudeOtherSession := deriveXAIExecutionID("subject-a", http.Header{
		"X-Claude-Code-Session-Id": {"other-claude-session"},
	})
	if claudeFirst == "" || claudeFirst != claudeSecond || claudeFirst == claudeOtherSession {
		t.Fatalf(
			"Claude Code execution identity is not stable and isolated: first=%q second=%q other=%q",
			claudeFirst,
			claudeSecond,
			claudeOtherSession,
		)
	}
	if transient := deriveXAIExecutionID("subject-a", http.Header{}); transient != "" {
		t.Fatalf("missing explicit session must not invent cross-request identity, got %q", transient)
	}
}

func TestXAICredentialRejectedIsSchemaStrict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`, want: true},
		{name: "structured bad credential", status: http.StatusForbidden, body: `{"error":{"type":"authentication_error","code":"invalid_token"}}`, want: true},
		{name: "ordinary forbidden", status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`},
		{name: "entitlement", status: http.StatusForbidden, body: `{"error":{"type":"entitlement_error","code":"not_entitled"}}`},
		{name: "quota", status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_error","code":"quota_exceeded"}}`},
		{name: "server error", status: http.StatusBadGateway, body: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := xaiCredentialRejected(test.status, nil, []byte(test.body)); got != test.want {
				t.Fatalf("xaiCredentialRejected() = %v, want %v", got, test.want)
			}
		})
	}
}

func assertNoJSONKey(t *testing.T, value any, forbidden string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == forbidden {
				t.Fatalf("found forbidden recursive key %q", forbidden)
			}
			assertNoJSONKey(t, child, forbidden)
		}
	case []any:
		for _, child := range typed {
			assertNoJSONKey(t, child, forbidden)
		}
	}
}
