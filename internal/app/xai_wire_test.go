package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/xaiauth"
)

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

	got, err := finalizeXAIResponsesBody(raw, "grok-4.5", "conv-parent", xaiauth.CLIBaseURL)
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
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("CLI chat-proxy tools = %#v, want one native web_search", tools)
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

	got, err := finalizeXAIResponsesBody(raw, "grok-4.5", "conv", xaiauth.CLIBaseURL)
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

func TestFinalizeXAIResponsesBodyNormalizesCLITools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		baseURL          string
		body             string
		wantToolTypes    []string
		wantToolNames    []string
		wantChoiceTypes  []string
		wantChoiceString string
		wantParallel     bool
		wantChoiceAbsent bool
		withoutCacheKey  bool
	}{
		{
			name:          "prepend native web search to explicit ordinary tools",
			baseURL:       xaiauth.CLIBaseURL + "/",
			body:          `{"tools":[{"type":"function","name":"lookup"}]}`,
			wantToolTypes: []string{"web_search", "function"},
			wantToolNames: []string{"", "lookup"},
		},
		{
			name:            "web search injection is independent of prompt cache key",
			baseURL:         xaiauth.CLIBaseURL,
			body:            `{"tools":[{"type":"function","name":"lookup"}]}`,
			wantToolTypes:   []string{"web_search", "function"},
			wantToolNames:   []string{"", "lookup"},
			withoutCacheKey: true,
		},
		{
			name:    "deduplicate native web search and replace same named client tools",
			baseURL: xaiauth.CLIBaseURL,
			body: `{
				"tools":[
					{"type":"web_search"},
					{"type":"web_search"},
					{"type":"function","name":"web_search"},
					{"type":"custom","name":" WEB_SEARCH "},
					{"type":"function","name":"lookup"}
				],
				"tool_choice":{"type":"allowed_tools","tools":[
					{"type":"function","name":"lookup"},
					{"type":"custom","name":"web_search"},
					{"type":"web_search"},
					{"type":"web_search"}
				]}
			}`,
			wantToolTypes:   []string{"web_search", "function"},
			wantToolNames:   []string{"", "lookup"},
			wantChoiceTypes: []string{"function", "web_search"},
		},
		{
			name:             "replace sole named web search without dropping active controls",
			baseURL:          xaiauth.CLIBaseURL,
			body:             `{"tools":[{"type":"function","name":"web_search"}],"tool_choice":"auto","parallel_tool_calls":true}`,
			wantToolTypes:    []string{"web_search"},
			wantToolNames:    []string{""},
			wantChoiceString: "auto",
			wantParallel:     true,
		},
		{
			name:             "prune choice without declared tools",
			baseURL:          xaiauth.CLIBaseURL,
			body:             `{"tool_choice":{"type":"function","name":"web_search"}}`,
			wantToolTypes:    []string{"web_search"},
			wantToolNames:    []string{""},
			wantChoiceAbsent: true,
		},
		{
			name:          "preserve explicit native x search without adding another",
			baseURL:       xaiauth.CLIBaseURL,
			body:          `{"tools":[{"type":"x_search"},{"type":"function","name":"lookup"}]}`,
			wantToolTypes: []string{"web_search", "x_search", "function"},
			wantToolNames: []string{"", "", "lookup"},
		},
		{
			name:          "custom base does not inject",
			baseURL:       "https://gateway.example/v1",
			body:          `{"tools":[{"type":"function","name":"lookup"}]}`,
			wantToolTypes: []string{"function"},
			wantToolNames: []string{"lookup"},
		},
		{
			name:             "public api base does not inject and prunes orphan controls",
			baseURL:          "https://api.x.ai/v1",
			body:             `{"tools":[],"tool_choice":"auto","parallel_tool_calls":true}`,
			wantChoiceAbsent: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executionID := "conv"
			if test.withoutCacheKey {
				executionID = ""
			}
			got, err := finalizeXAIResponsesBody([]byte(test.body), "grok-4.5", executionID, test.baseURL)
			if err != nil {
				t.Fatalf("finalizeXAIResponsesBody() error = %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("result is not JSON: %v\n%s", err, got)
			}
			tools, _ := payload["tools"].([]any)
			if len(tools) != len(test.wantToolTypes) {
				t.Fatalf("tools = %#v, want types %#v", tools, test.wantToolTypes)
			}
			for i, rawTool := range tools {
				tool := rawTool.(map[string]any)
				toolName, _ := tool["name"].(string)
				if tool["type"] != test.wantToolTypes[i] || toolName != test.wantToolNames[i] {
					t.Fatalf("tools[%d] = %#v, want type=%q name=%q", i, tool, test.wantToolTypes[i], test.wantToolNames[i])
				}
			}
			choice, choiceExists := payload["tool_choice"]
			if test.wantChoiceAbsent {
				if choiceExists {
					t.Fatalf("tool_choice survived: %#v", choice)
				}
				return
			}
			if test.wantChoiceString != "" && choice != test.wantChoiceString {
				t.Fatalf("tool_choice = %#v, want %q", choice, test.wantChoiceString)
			}
			if test.wantParallel && payload["parallel_tool_calls"] != true {
				t.Fatalf("parallel_tool_calls = %#v, want true", payload["parallel_tool_calls"])
			}
			if len(test.wantChoiceTypes) == 0 {
				return
			}
			choiceObject := choice.(map[string]any)
			allowed, _ := choiceObject["tools"].([]any)
			if len(allowed) != len(test.wantChoiceTypes) {
				t.Fatalf("allowed_tools = %#v, want types %#v", allowed, test.wantChoiceTypes)
			}
			for i, rawTool := range allowed {
				if rawTool.(map[string]any)["type"] != test.wantChoiceTypes[i] {
					t.Fatalf("allowed_tools[%d] = %#v, want type=%q", i, rawTool, test.wantChoiceTypes[i])
				}
			}
		})
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
				xaiauth.CLIBaseURL,
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
		"X-Grok-Client-Identifier":  {"wrong"},
		"X-Authenticateresponse":    {"wrong"},
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
		"Accept":                              "text/event-stream",
		xaiauth.CLITokenAuthHeader:            xaiauth.CLITokenAuthValue,
		xaiauth.CLIClientVersionHeader:        xaiauth.CLIClientVersion,
		"User-Agent":                          xaiauth.CLIUserAgent,
		xaiauth.CLIClientIdentifierHeader:     xaiauth.CLIClientIdentifier,
		xaiauth.CLIAuthenticateResponseHeader: xaiauth.CLIAuthenticateResponse,
		"x-grok-conv-id":                      "conv-derived",
		"X-Unrelated-Custom-Header":           "preserved",
	}
	for name, value := range want {
		if got := req.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{"X-Api-Key", "x-goog-api-key", "Session-Id", "Session_id", "Originator", "ChatGPT-Account-ID"} {
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
