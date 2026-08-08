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
		"presence_penalty", "frequency_penalty", "stop", "tools", "tool_choice", "parallel_tool_calls",
	} {
		if _, exists := payload[field]; exists {
			t.Fatalf("field %q survived xAI finalization: %s", field, got)
		}
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want normalized high with summary preserved", reasoning)
	}
	assertNoJSONKey(t, payload, "external_web_access")
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
