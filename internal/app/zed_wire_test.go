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
	body, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":false}`), nil)
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
	finalized, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), body, nil)
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

func TestFinalizeZedResponsesBodyFlattensAdditionalToolNamespaces(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"custom","name":"exec"},
					{"type":"function","name":"wait","parameters":{"type":"object"}},
					{"type":"function","name":"request_user_input"}
				]},
				{"type":"namespace","name":"collaboration","tools":[
					{"type":"function","name":"followup_task"},
					{"type":"function","name":"interrupt_agent"},
					{"type":"function","name":"list_agents"},
					{"type":"function","name":"send_message"},
					{"type":"function","name":"spawn_agent"},
					{"type":"function","name":"wait_agent"}
				]}
			]},
			{"role":"user","content":"run"},
			{"type":"function_call","call_id":"call_1","name":"wait","namespace":"functions","arguments":"{}"}
		],
		"tool_choice":"auto"
	}`)
	finalized, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), body, nil)
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
	if len(input) != 2 {
		t.Fatalf("additional_tools input was not removed: %#v", input)
	}
	history, _ := input[1].(map[string]any)
	if history["name"] != "functions__wait" || history["namespace"] != nil {
		t.Fatalf("normalized tool call history = %#v", history)
	}
	tools, _ := envelope.ProviderRequest["tools"].([]any)
	wantNames := []string{
		"functions__exec", "functions__wait", "functions__request_user_input",
		"collaboration__followup_task", "collaboration__interrupt_agent", "collaboration__list_agents",
		"collaboration__send_message", "collaboration__spawn_agent", "collaboration__wait_agent",
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("flattened tools = %#v, want %d tools", tools, len(wantNames))
	}
	for index, wantName := range wantNames {
		if got := tools[index].(map[string]any)["name"]; got != wantName {
			t.Fatalf("flattened tool %d name = %v, want %q", index, got, wantName)
		}
	}
}

func TestZedResponsesWireRestoresNamespaceToolIdentity(t *testing.T) {
	registry := newZedWireTestRegistry()
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}]}`)
	_, plan, err := finalizeZedResponsesBody(registry, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream := strings.Join([]string{
		`{"event":{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"call_1","name":"functions__exec","input":""}}}`,
		`{"event":{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"call_1","name":"functions__exec","input":"pwd"}}}`,
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
	blocks := strings.Split(strings.TrimSpace(string(converted)), "\n\n")
	if len(blocks) != 2 {
		t.Fatalf("converted event count = %d, want 2", len(blocks))
	}
	for _, block := range blocks {
		_, data := parseSSEEventChunk([]byte(block + "\n\n"))
		var event struct {
			Item map[string]any `json:"item"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Item["name"] != "exec" || event.Item["namespace"] != "functions" {
			t.Fatalf("restored tool identity = %#v", event.Item)
		}
	}
}

func TestFinalizeZedResponsesBodyQualifiesNamespaceToolChoice(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait"}]}]}],"tool_choice":{"type":"function","name":"wait","namespace":"functions"}}`)
	finalized, _, err := finalizeZedResponsesBody(newZedWireTestRegistry(), body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ProviderRequest map[string]any `json:"provider_request"`
	}
	if err := json.Unmarshal(finalized, &envelope); err != nil {
		t.Fatal(err)
	}
	tools, _ := envelope.ProviderRequest["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "functions__wait" || envelope.ProviderRequest["tool_choice"] != "required" {
		t.Fatalf("selected namespace tool = %#v, choice=%v", tools, envelope.ProviderRequest["tool_choice"])
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
				message, _ := messages[0].(map[string]any)
				content, _ := message["content"].([]any)
				if len(content) != 1 || content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hello" {
					t.Fatalf("Anthropic message content = %#v", message["content"])
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
				nil,
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

func TestFinalizeZedAnthropicProviderRequestNormalizesNativeFields(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","system":"rules","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"kept"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"failed","is_error":true}]}],"tools":[{"name":"lookup","description":"look up"}],"stream":true}`)
	originalAnthropicRequest := []byte(`{"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral","ttl":"1h"}}],"tools":[{"name":"lookup","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	finalized, err := finalizeZedAnthropicProviderRequest(body, []byte(`{"model":"claude-sonnet-4-6","max_output_tokens":64}`), originalAnthropicRequest)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(finalized, &request); err != nil {
		t.Fatal(err)
	}
	if request["stream"] != nil {
		t.Fatalf("stream must be removed: %#v", request["stream"])
	}
	system, _ := request["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["type"] != "text" || system[0].(map[string]any)["text"] != "rules" {
		t.Fatalf("system = %#v", request["system"])
	}
	messages, _ := request["messages"].([]any)
	firstContent, _ := messages[0].(map[string]any)["content"].([]any)
	secondContent, _ := messages[1].(map[string]any)["content"].([]any)
	if len(firstContent) != 1 || firstContent[0].(map[string]any)["text"] != "hello" ||
		len(secondContent) != 1 || secondContent[0].(map[string]any)["text"] != "kept" {
		t.Fatalf("messages = %#v", messages)
	}
	toolResults, _ := messages[2].(map[string]any)["content"].([]any)
	if len(toolResults) != 2 || toolResults[0].(map[string]any)["is_error"] != false || toolResults[1].(map[string]any)["is_error"] != true {
		t.Fatalf("tool results = %#v", toolResults)
	}
	if request["cache_control"].(map[string]any)["type"] != "ephemeral" ||
		system[0].(map[string]any)["cache_control"].(map[string]any)["ttl"] != "1h" {
		t.Fatalf("request cache controls = %#v", request)
	}
	tools, _ := request["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["cache_control"].(map[string]any)["ttl"] != "1h" {
		t.Fatalf("tool cache controls = %#v", tools)
	}
}

func TestZedAnthropicWirePreservesProviderError(t *testing.T) {
	registry := newZedWireTestRegistry()
	_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"claude-sonnet-5","input":"hello"}`), nil)
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
	_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"gpt-5.6-sol","input":"hello"}`), nil)
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
			_, plan, err := finalizeZedResponsesBody(registry, []byte(`{"model":"`+test.model+`","input":"hello"}`), nil)
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
