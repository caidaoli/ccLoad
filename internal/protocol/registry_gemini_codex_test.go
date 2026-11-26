package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"

	"github.com/tidwall/gjson"
)

func TestRegistryGeminiResponsesModelMatchesAcrossJSONAndSSE(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	const payload = `{"responseId":"resp_model","modelVersion":"provider-build","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":0}}`
	for _, tc := range []struct {
		name, model, request, want string
	}{
		{"resolved", "gemini-2.5-pro", `{"model":"gemini-2.5-pro(high)"}`, "gemini-2.5-pro"},
		{"original request", "", `{"model":"client-alias"}`, "client-alias"},
		{"provider fallback", "", `{"model":null}`, "provider-build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := []byte(tc.request)
			result, err := reg.TranslateResponseNonStream(t.Context(), protocol.Gemini, protocol.Codex, tc.model, request, nil, []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(result, "model").Str; got != tc.want {
				t.Errorf("JSON model = %q, want %q", got, tc.want)
			}
			var state any
			chunks, err := reg.TranslateResponseStream(t.Context(), protocol.Gemini, protocol.Codex, tc.model, request, nil, []byte("data: "+payload+"\n\n"), &state)
			if err != nil {
				t.Fatal(err)
			}
			models := make(map[string]string)
			for _, event := range registryToolInputEvents(t, chunks) {
				if model := event.Get("response.model"); model.Exists() {
					models[event.Get("type").Str] = model.Str
				}
			}
			for _, name := range []string{"response.created", "response.in_progress", "response.completed"} {
				if got := models[name]; got != tc.want {
					t.Errorf("%s model = %q, want %q", name, got, tc.want)
				}
			}
		})
	}
}

func TestRegistryGeminiResponsesToolSearchMultilineSSE(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	request := []byte(`{"model":"gemini-3.8-flash","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}}],"input":"Find fixture tools","stream":true}`)
	const precise = "9007199254740993"
	frame := "data: {\"responseId\":\"resp_search\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"},{\"functionCall\":{\"id\":\"call_search\",\"name\":\"tool_search\",\"args\":{\n" +
		"data: \"query\":\"fixture tools\",\n" +
		"data: \"limit\":" + precise + "\n" +
		"data: }}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":14}}\n\n"
	var state any
	chunks, err := reg.TranslateResponseStream(t.Context(), protocol.Gemini, protocol.Codex, "gemini-3.8-flash", request, nil, []byte(frame), &state)
	if err != nil {
		t.Fatal(err)
	}
	searchItems := make(map[string]gjson.Result)
	for _, event := range registryToolInputEvents(t, chunks) {
		switch kind := event.Get("type").Str; kind {
		case "response.output_item.added", "response.output_item.done":
			if item := event.Get("item"); item.Get("type").Str == "tool_search_call" {
				searchItems[kind] = item
			}
		case "response.completed":
			if event.Get("response.output.0.content.0.text").Str != "answer" {
				t.Errorf("missing text alongside native search: %s", event.Raw)
			}
			searchItems[kind] = event.Get("response.output.1")
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			t.Errorf("native search emitted generic function arguments: %s", event.Raw)
		}
	}
	for kind, want := range map[string]string{
		"response.output_item.added": "in_progress",
		"response.output_item.done":  "completed",
		"response.completed":         "completed",
	} {
		item := searchItems[kind]
		args := item.Get("arguments")
		if item.Get("type").Str != "tool_search_call" || item.Get("call_id").Str != "call_search" || item.Get("status").Str != want ||
			!args.IsObject() || args.Get("query").Str != "fixture tools" || args.Get("limit").Raw != precise {
			t.Errorf("%s did not preserve native search arguments or identity: %s", kind, item.Raw)
		}
	}
}

func TestRegistry_TranslateRequest_GeminiToCodex(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{"systemInstruction":{"parts":[{"text":"be careful"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"query":"go"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"done"}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","description":"lookup docs","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.Codex, "gpt-5-codex", raw, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Instructions string           `json:"instructions"`
		Stream       bool             `json:"stream"`
		Input        []map[string]any `json:"input"`
		Tools        []map[string]any `json:"tools"`
		ToolChoice   map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal codex request: %v", err)
	}
	if req.Instructions != "be careful" || !req.Stream {
		t.Fatalf("unexpected codex request metadata: %+v", req)
	}
	if len(req.Input) != 3 {
		t.Fatalf("unexpected codex input: %+v", req.Input)
	}
	if req.Input[0]["type"] != "message" || req.Input[0]["role"] != "user" {
		t.Fatalf("unexpected codex message input: %+v", req.Input[0])
	}
	content, ok := req.Input[0]["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected codex message content: %+v", req.Input[0]["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("unexpected codex message part: %+v", content[0])
	}
	callID, _ := req.Input[1]["call_id"].(string)
	if req.Input[1]["type"] != "function_call" || callID == "" || req.Input[1]["name"] != "lookup" {
		t.Fatalf("unexpected codex function_call: %+v", req.Input[1])
	}
	args, ok := req.Input[1]["arguments"].(string)
	if !ok || args != `{"query":"go"}` {
		t.Fatalf("unexpected codex function_call args: %+v", req.Input[1]["arguments"])
	}
	if req.Input[2]["type"] != "function_call_output" || req.Input[2]["call_id"] != callID || req.Input[2]["output"] != "done" {
		t.Fatalf("unexpected codex function_call_output: %+v", req.Input[2])
	}
	if len(req.Tools) != 1 || req.Tools[0]["type"] != "function" || req.Tools[0]["name"] != "lookup" || req.Tools[0]["description"] != "lookup docs" {
		t.Fatalf("unexpected codex tools: %+v", req.Tools)
	}
	if req.ToolChoice["type"] != "function" || req.ToolChoice["name"] != "lookup" {
		t.Fatalf("unexpected codex tool choice: %+v", req.ToolChoice)
	}
}

func TestRegistry_TranslateRequest_GeminiToCodex_PreservesThinkingLevel(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	raw := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"think hard"}]}],
		"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}
	}`)
	got, err := reg.TranslateRequest(protocol.Gemini, protocol.Codex, "gpt-5-codex", raw, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}
	var req struct {
		Reasoning map[string]any `json:"reasoning"`
		Include   []string       `json:"include"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal codex request: %v", err)
	}
	if req.Reasoning["effort"] != "high" || req.Reasoning["summary"] != "auto" {
		t.Fatalf("expected high codex reasoning config, got %s", got)
	}
	if len(req.Include) != 1 || req.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("expected reasoning include, got %s", got)
	}
}

func TestRegistry_TranslateResponseNonStream_CodexToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawResp := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":{"query":"go"}}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}

	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		ResponseID    string `json:"responseId"`
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal gemini response: %v", err)
	}
	if len(resp.Candidates) != 1 || len(resp.Candidates[0].Content.Parts) != 2 {
		t.Fatalf("unexpected candidates: %+v", resp)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("unexpected text part: %+v", resp.Candidates[0].Content.Parts[0])
	}
	if resp.Candidates[0].Content.Parts[1].FunctionCall.Name != "lookup" || resp.Candidates[0].Content.Parts[1].FunctionCall.Args["query"] != "go" {
		t.Fatalf("unexpected function call part: %+v", resp.Candidates[0].Content.Parts[1])
	}
	if resp.Candidates[0].FinishReason != "STOP" || resp.ResponseID != "resp_1" || resp.ModelVersion != "gemini-2.5-pro" {
		t.Fatalf("unexpected gemini metadata: %+v", resp)
	}
	if resp.UsageMetadata.PromptTokenCount != 3 || resp.UsageMetadata.CandidatesTokenCount != 5 || resp.UsageMetadata.TotalTokenCount != 8 {
		t.Fatalf("unexpected gemini usage: %+v", resp.UsageMetadata)
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	textChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.output_text.delta failed: %v", err)
	}
	if len(textChunk) != 1 || !strings.Contains(string(textChunk[0]), `"text":"hello"`) {
		t.Fatalf("unexpected gemini text chunk: %#v", textChunk)
	}

	toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":{\"query\":\"go\"}}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.output_item.done failed: %v", err)
	}
	if len(toolChunk) != 1 {
		t.Fatalf("unexpected gemini tool chunk: %#v", toolChunk)
	}
	toolPayload := mustJSONMap(t, bytes.TrimPrefix(bytes.TrimSpace(toolChunk[0]), []byte("data: ")))
	toolCandidate := mustMap(t, mustSlice(t, toolPayload["candidates"])[0])
	toolContent := mustMap(t, toolCandidate["content"])
	functionCall := mustMap(t, mustMap(t, mustSlice(t, toolContent["parts"])[0])["functionCall"])
	args := mustMap(t, functionCall["args"])
	if functionCall["name"] != "lookup" || functionCall["id"] != "call_1" || args["query"] != "go" {
		t.Fatalf("unexpected gemini tool semantics: %#v", toolChunk)
	}

	done, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.completed failed: %v", err)
	}
	if len(done) != 1 || !strings.Contains(string(done[0]), `"promptTokenCount":3`) || !strings.Contains(string(done[0]), `"candidatesTokenCount":5`) || !strings.Contains(string(done[0]), `"finishReason":"STOP"`) {
		t.Fatalf("unexpected gemini done chunk: %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini_StringArguments(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	toolChunk, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_skill_1\",\"name\":\"Skill\",\"arguments\":\"{\\\"args\\\":\\\"skill: \\\\\\\"superpowers:using-superpowers\\\\\\\"\\\",\\\"skill\\\":\\\"superpowers:using-superpowers\\\"}\"}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.output_item.done failed: %v", err)
	}
	if len(toolChunk) != 1 {
		t.Fatalf("expected one gemini tool chunk, got %#v", toolChunk)
	}

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(toolChunk[0]), []byte("data: ")), &payload); err != nil {
		t.Fatalf("unmarshal gemini chunk: %v", err)
	}
	candidates := payload["candidates"].([]any)
	content := candidates[0].(map[string]any)["content"].(map[string]any)
	parts := content["parts"].([]any)
	functionCall := parts[0].(map[string]any)["functionCall"].(map[string]any)
	args := functionCall["args"].(map[string]any)
	if functionCall["name"] != "Skill" || args["args"] != `skill: "superpowers:using-superpowers"` || args["skill"] != "superpowers:using-superpowers" {
		t.Fatalf("expected gemini functionCall args object, got %s", toolChunk[0])
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini_CompletionWithoutUsageStillStops(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	done, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n"), &state)
	if err != nil {
		t.Fatalf("response.completed failed: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("expected one gemini completion chunk, got %#v", done)
	}
	if !strings.Contains(string(done[0]), `"finishReason":"STOP"`) || !strings.Contains(string(done[0]), `"responseId":"resp_1"`) {
		t.Fatalf("unexpected gemini done chunk: %#v", done)
	}
	if strings.Contains(string(done[0]), `"usageMetadata"`) {
		t.Fatalf("expected completion without usage metadata, got %#v", done)
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini_ReasoningWithText(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	chunk := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_01\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"step by step\"}]}}\n\n"

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("expected gemini text chunk for reasoning, got none")
	}
	result := string(bytes.Join(out, nil))
	if !strings.Contains(result, `"step by step"`) {
		t.Fatalf("expected reasoning text in output, got:\n%s", result)
	}
}

func TestRegistry_TranslateResponseStream_CodexToGemini_ReasoningEmpty(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	// reasoning item 无 summary text（空 summary 数组）
	chunk := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_02\",\"summary\":[]}}\n\n"

	var state any
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Gemini, "gemini-2.5-pro", nil, nil, []byte(chunk), &state)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	// 空 reasoning → 静默忽略，无输出
	if len(out) != 0 {
		t.Fatalf("expected no output for empty reasoning, got: %v", out)
	}
}
