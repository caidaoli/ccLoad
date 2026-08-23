package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

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
