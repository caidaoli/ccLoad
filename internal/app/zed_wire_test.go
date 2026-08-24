package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
	"ccLoad/internal/zedauth"
)

func newZedWireTestRegistry() *protocol.Registry {
	registry := protocol.NewRegistry()
	builtin.Register(registry)
	return registry
}

func TestFinalizeZedResponsesBodyWrapsProviderRequest(t *testing.T) {
	body, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ThreadID        string         `json:"thread_id"`
		PromptID        string         `json:"prompt_id"`
		Intent          string         `json:"intent"`
		Provider        string         `json:"provider"`
		Model           string         `json:"model"`
		ProviderRequest map[string]any `json:"provider_request"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ThreadID == "" || envelope.PromptID == "" || envelope.Intent != "user_prompt" || envelope.Provider != "open_ai" || envelope.Model != "gpt-5.6-sol" {
		t.Fatalf("envelope = %+v", envelope)
	}
	input, _ := envelope.ProviderRequest["input"].([]any)
	if envelope.ProviderRequest["stream"] != true || len(input) != 1 {
		t.Fatalf("provider_request = %v", envelope.ProviderRequest)
	}
	reasoning, _ := envelope.ProviderRequest["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || envelope.ProviderRequest["max_output_tokens"] != float64(32768) {
		t.Fatalf("reasoning policy = %v", envelope.ProviderRequest)
	}
}

func TestFinalizeZedResponsesBodyNormalizesCodexOnlyFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"},{"type":"namespace","name":"collaboration"}]},{"role":"developer","content":"rules"},{"type":"reasoning","content":null}],"tools":[{"type":"function","name":"wait"}],"tool_choice":{"type":"function","name":"wait"}}`)
	finalized, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ProviderRequest map[string]any `json:"provider_request"`
	}
	if err := json.Unmarshal(finalized, &envelope); err != nil {
		t.Fatal(err)
	}
	input, _ := envelope.ProviderRequest["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["role"] != "system" {
		t.Fatalf("normalized input = %#v", input)
	}
	tools, _ := envelope.ProviderRequest["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "wait" || envelope.ProviderRequest["tool_choice"] != "required" {
		t.Fatalf("normalized tools = %#v choice=%v", tools, envelope.ProviderRequest["tool_choice"])
	}
}

func TestFinalizeZedResponsesBodySelectsNativeProvider(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		wantProvider  string
		assertRequest func(*testing.T, map[string]any)
	}{
		{
			name: "anthropic", model: "claude-sonnet-4-5", wantProvider: zedauth.ProviderAnthropic,
			assertRequest: func(t *testing.T, request map[string]any) {
				t.Helper()
				if request["model"] != "claude-sonnet-4-5" || request["stream"] != nil || request["max_tokens"] != float64(8192) {
					t.Fatalf("Anthropic provider_request = %v", request)
				}
				messages, _ := request["messages"].([]any)
				if len(messages) != 1 {
					t.Fatalf("Anthropic messages = %v", messages)
				}
			},
		},
		{
			name: "google", model: "gemini-3.5-flash", wantProvider: zedauth.ProviderGoogle,
			assertRequest: func(t *testing.T, request map[string]any) {
				t.Helper()
				if request["model"] != "models/gemini-3.5-flash" {
					t.Fatalf("Google provider_request = %v", request)
				}
				contents, _ := request["contents"].([]any)
				config, _ := request["generationConfig"].(map[string]any)
				if len(contents) != 1 || config["candidateCount"] != float64(1) {
					t.Fatalf("Google request contents=%v config=%v", contents, config)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, plan, err := finalizeZedResponsesBody(
				newZedWireTestRegistry(),
				[]byte(`{"model":"`+test.model+`","input":"hello","stream":false}`),
			)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Provider        string         `json:"provider"`
				Model           string         `json:"model"`
				ProviderRequest map[string]any `json:"provider_request"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if plan == nil || envelope.Provider != test.wantProvider || envelope.Model != test.model {
				t.Fatalf("envelope=%+v plan=%+v", envelope, plan)
			}
			test.assertRequest(t, envelope.ProviderRequest)
		})
	}
}

func TestZedAnthropicWirePreservesProviderError(t *testing.T) {
	registry := newZedWireTestRegistry()
	_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"claude-sonnet-5","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	upstream := strings.Join([]string{
		`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
		`{"status":"stream_ended"}`,
		"",
	}, "\n")
	response := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(upstream)),
	}
	if err := prepareZedResponsesResponse(response, plan, registry); err != nil {
		t.Fatal(err)
	}
	converted, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, "event: error") || !strings.Contains(text, `"type":"overloaded_error"`) || strings.Contains(text, "response.completed") {
		t.Fatalf("converted SSE = %q", text)
	}
}

func TestZedResponsesWireRebuildsHeadersAndUnwrapsEvents(t *testing.T) {
	registry := newZedWireTestRegistry()
	_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, zedauth.CompletionsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer foreign")
	request.Header.Set("X-Stainless-Lang", "js")
	injectZedResponsesHeaders(request, "zed-jwt")
	if request.Header.Get("Authorization") != "Bearer zed-jwt" || request.Header.Get("X-Stainless-Lang") != "" {
		t.Fatalf("headers = %v", request.Header)
	}
	if request.Header.Get("User-Agent") != zedauth.UserAgent() ||
		request.Header.Get("x-zed-version") != zedauth.ZedVersion ||
		request.Header.Get("x-zed-client-supports-status-messages") != "true" {
		t.Fatalf("Zed identity headers = %v", request.Header)
	}

	upstream := strings.Join([]string{
		`{"status":"started"}`,
		`{"event":{"type":"response.output_text.delta","delta":"hello"}}`,
		`{"event":{"type":"response.completed","response":{"status":"completed"}}}`,
		`{"status":"stream_ended"}`,
		"",
	}, "\n")
	response := &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(upstream)),
	}
	if err := prepareZedResponsesResponse(response, plan, registry); err != nil {
		t.Fatal(err)
	}
	converted, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n") ||
		!strings.Contains(text, "event: response.completed") || strings.Contains(text, "stream_ended") {
		t.Fatalf("converted SSE = %q", text)
	}
	if response.Header.Get("Content-Type") != "text/event-stream" || response.ContentLength != -1 {
		t.Fatalf("response framing = headers=%v length=%d", response.Header, response.ContentLength)
	}
}

func TestZedResponsesWireTranslatesNativeProviderEvents(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		upstream string
	}{
		{
			name: "anthropic", model: "claude-sonnet-5",
			upstream: strings.Join([]string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
				`{"status":"stream_ended"}`,
				"",
			}, "\n"),
		},
		{
			name: "google", model: "gemini-3.5-flash",
			upstream: strings.Join([]string{
				`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-3.5-flash","responseId":"resp_zed"}`,
				`{"status":"stream_ended"}`,
				"",
			}, "\n"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := newZedWireTestRegistry()
			_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"`+test.model+`","input":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(test.upstream)),
			}
			if err := prepareZedResponsesResponse(response, plan, registry); err != nil {
				t.Fatal(err)
			}
			converted, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			text := string(converted)
			if !strings.Contains(text, "event: response.output_text.delta") ||
				!strings.Contains(text, `"delta":"hello"`) ||
				!strings.Contains(text, "event: response.completed") ||
				strings.Contains(text, "stream_ended") {
				t.Fatalf("converted SSE = %q", text)
			}
		})
	}
}

func TestZedPlanRejectionIsModelScopedNotCredentialScoped(t *testing.T) {
	body := []byte(`{"error":{"message":"model is not included in your plan"}}`)
	if !zedModelPlanRejected(http.StatusForbidden, body) {
		t.Fatal("plan rejection must be model scoped")
	}
	if zedCredentialRejected(http.StatusForbidden, body) {
		t.Fatal("plan rejection must not refresh the account credential")
	}
	if !zedCredentialRejected(http.StatusForbidden, []byte(`{"error":"trial_blocked"}`)) {
		t.Fatal("non-plan forbidden response must reject the credential")
	}
}
