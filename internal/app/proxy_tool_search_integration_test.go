package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

const (
	proxyToolSearchModel       = "tool-search-roundtrip"
	proxyToolSearchLookupName  = "mcp__public__lookup"
	proxyToolSearchLookupCall  = "call-lookup-1"
	proxyToolSearchResultToken = "TOOL_SEARCH_RESULT_MARKER"
)

func TestProxy_CodexToolSearchRoundTripThroughOpenAI(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "non-stream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			toolSearchRoundTripThroughOpenAI(t, streaming)
		})
	}
}

func toolSearchRoundTripThroughOpenAI(t *testing.T, streaming bool) {
	t.Helper()

	initialInput := proxyToolSearchInitialInput()
	requestBody := map[string]any{
		"model":  proxyToolSearchModel,
		"stream": streaming,
		"tools":  []any{proxyToolSearchDeclaration()},
		"input":  initialInput,
	}

	var upstreamRequests [][]byte
	var upstreamCalls atomic.Int32
	env := setupProxyTestEnv(t, []testChannel{
		{
			name: "openai-tool-search", upstreamProtocol: "openai",
			protocolTransformMode: model.ProtocolTransformModeLocal,
			models:                proxyToolSearchModel, apiKey: "sk-tool-search",
		},
	}, map[int]string{0: "https://openai-tool-search.invalid"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			return nil, fmt.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		upstreamRequests = append(upstreamRequests, body)
		switch upstreamCalls.Add(1) {
		case 1:
			return proxyToolSearchChatResponse(streaming, true), nil
		case 2:
			return proxyToolSearchChatResponse(streaming, false), nil
		default:
			return nil, fmt.Errorf("unexpected upstream request %d", upstreamCalls.Load())
		}
	})}

	firstResponse := doProxyRequest(t, env.engine, "/v1/responses", requestBody, nil)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("first request upstream calls=%d, want 1", got)
	}

	searchName := assertProxyToolSearchChatRequest(t, upstreamRequests[0], false, "")
	assertProxyToolSearchClientFunctionCall(t, firstResponse.Body.Bytes(), streaming, proxyToolSearchLookupCall)

	followupInput := append([]any{}, initialInput...)
	followupInput = append(followupInput,
		map[string]any{
			"type":      "function_call",
			"id":        "fc-lookup-1",
			"status":    "completed",
			"call_id":   proxyToolSearchLookupCall,
			"name":      "lookup",
			"namespace": "mcp__public",
			"arguments": `{"query":"needle"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": proxyToolSearchLookupCall,
			"output":  proxyToolSearchResultToken,
		},
		map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": "Return the tool result marker exactly.",
			}},
		},
	)
	followupBody := map[string]any{
		"model":  proxyToolSearchModel,
		"stream": streaming,
		"tools":  []any{proxyToolSearchDeclaration()},
		"input":  followupInput,
	}

	secondResponse := doProxyRequest(t, env.engine, "/v1/responses", followupBody, nil)
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("follow-up request status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("follow-up upstream calls=%d, want 2", got)
	}
	assertProxyToolSearchChatRequest(t, upstreamRequests[1], true, searchName)
	assertProxyToolSearchCompletedResponse(t, secondResponse.Body.Bytes(), streaming)
}

func proxyToolSearchInitialInput() []any {
	return []any{
		map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": "Find the lookup tool, then use it.",
			}},
		},
		map[string]any{
			"type": "tool_search_call", "execution": "client", "call_id": "search-1",
			"status": "completed", "arguments": map[string]any{"query": "lookup"},
		},
		map[string]any{
			"type": "tool_search_output", "execution": "client", "call_id": "search-1",
			"status": "completed", "tools": []any{proxyToolSearchLoadedNamespace()},
		},
		map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": "Use lookup with query needle.",
			}},
		},
	}
}

func proxyToolSearchDeclaration() map[string]any {
	return map[string]any{
		"type": "tool_search", "execution": "client", "description": "Load tools by query",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"},
		},
	}
}

func proxyToolSearchLoadedNamespace() map[string]any {
	return map[string]any{
		"type": "namespace", "name": "mcp__public",
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "description": "Look up a public record",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query"},
					"limit": map[string]any{"type": "integer", "minimum": 1},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		}},
	}
}

func proxyToolSearchChatResponse(streaming, functionCall bool) *http.Response {
	if functionCall {
		if streaming {
			body := strings.Join([]string{
				`data: {"id":"chatcmpl-search-1","object":"chat.completion.chunk","created":1773896263,"model":"tool-search-roundtrip","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-lookup-1","type":"function","function":{"name":"mcp__public__lookup","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-search-1","object":"chat.completion.chunk","created":1773896263,"model":"tool-search-roundtrip","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"needle\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}, "\n\n") + "\n\n"
			return proxyToolSearchHTTPResponse("text/event-stream", []byte(body))
		}
		return proxyToolSearchHTTPResponse("application/json", []byte(`{"id":"chatcmpl-search-1","object":"chat.completion","created":1773896263,"model":"tool-search-roundtrip","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-lookup-1","type":"function","function":{"name":"mcp__public__lookup","arguments":"{\"query\":\"needle\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	}

	if streaming {
		body := strings.Join([]string{
			`data: {"id":"chatcmpl-search-2","object":"chat.completion.chunk","created":1773896264,"model":"tool-search-roundtrip","choices":[{"index":0,"delta":{"role":"assistant","content":"TOOL_SEARCH_RESULT_MARKER"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-search-2","object":"chat.completion.chunk","created":1773896264,"model":"tool-search-roundtrip","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		return proxyToolSearchHTTPResponse("text/event-stream", []byte(body))
	}
	return proxyToolSearchHTTPResponse("application/json", []byte(`{"id":"chatcmpl-search-2","object":"chat.completion","created":1773896264,"model":"tool-search-roundtrip","choices":[{"index":0,"message":{"role":"assistant","content":"TOOL_SEARCH_RESULT_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24}}`))
}

func proxyToolSearchHTTPResponse(contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func assertProxyToolSearchChatRequest(t *testing.T, body []byte, followup bool, searchName string) string {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode translated Chat request: %v body=%s", err, body)
	}
	if got := gjson.GetBytes(body, "messages.#").Int(); got < 4 {
		t.Fatalf("translated Chat messages=%d, want at least 4: %s", got, body)
	}
	if got := gjson.GetBytes(body, "tools.#").Int(); got < 2 {
		t.Fatalf("translated Chat tools=%d, want search plus loaded lookup: %s", got, body)
	}

	var foundSearchCall, foundSearchOutput, foundLookupCall, foundLookupOutput, foundFinalUser bool
	searchCallIndex, searchOutputIndex := -1, -1
	lookupCallIndex, lookupOutputIndex := -1, -1
	searchInternalName := ""
	for messageIndex, message := range gjson.GetBytes(body, "messages").Array() {
		switch message.Get("role").String() {
		case "assistant":
			for _, toolCall := range message.Get("tool_calls").Array() {
				callID := toolCall.Get("id").String()
				name := toolCall.Get("function.name").String()
				switch callID {
				case "search-1":
					foundSearchCall = true
					searchCallIndex = messageIndex
					searchInternalName = name
					arguments := toolCall.Get("function.arguments")
					if arguments.String() == "" || !strings.Contains(arguments.Raw, "lookup") {
						t.Fatalf("search assistant tool call has empty arguments: %s", body)
					}
				case proxyToolSearchLookupCall:
					foundLookupCall = true
					lookupCallIndex = messageIndex
					if name != proxyToolSearchLookupName {
						t.Fatalf("lookup Chat function name=%q, want %q: %s", name, proxyToolSearchLookupName, body)
					}
				}
			}
		case "tool":
			callID := message.Get("tool_call_id").String()
			switch callID {
			case "search-1":
				foundSearchOutput = true
				searchOutputIndex = messageIndex
				content := message.Get("content")
				if !strings.Contains(content.Raw, "mcp__public") || !strings.Contains(content.Raw, "lookup") {
					t.Fatalf("search tool result did not preserve loaded lookup tool: %s", body)
				}
			case proxyToolSearchLookupCall:
				foundLookupOutput = true
				lookupOutputIndex = messageIndex
				content := message.Get("content")
				if content.String() != proxyToolSearchResultToken {
					t.Fatalf("lookup tool result=%q, want marker %q: %s", content.String(), proxyToolSearchResultToken, body)
				}
			}
		case "user":
			if strings.Contains(message.Get("content.0.text").String(), "Return the tool result marker") {
				foundFinalUser = true
			}
		}
	}
	if !foundSearchCall || !foundSearchOutput {
		t.Fatalf("Chat request lost search assistant/tool pair: %s", body)
	}
	if searchOutputIndex != searchCallIndex+1 {
		t.Fatalf("Chat search assistant/tool messages are not paired: call=%d output=%d body=%s", searchCallIndex, searchOutputIndex, body)
	}
	if followup && (!foundLookupCall || !foundLookupOutput || !foundFinalUser) {
		t.Fatalf("follow-up Chat request lost lookup tool pair or final user message: %s", body)
	}
	if followup && lookupOutputIndex != lookupCallIndex+1 {
		t.Fatalf("follow-up Chat lookup assistant/tool messages are not paired: call=%d output=%d body=%s", lookupCallIndex, lookupOutputIndex, body)
	}
	if !followup {
		if foundLookupCall || foundLookupOutput {
			t.Fatalf("initial Chat request unexpectedly contains lookup result pair: %s", body)
		}
	}
	if searchInternalName == "" {
		t.Fatalf("search assistant call did not expose internal function name: %s", body)
	}
	if followup && searchInternalName != searchName {
		t.Fatalf("follow-up search function name=%q changed from captured initial name=%q: %s", searchInternalName, searchName, body)
	}

	var foundLoadedLookup bool
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() != "function" || tool.Get("function.name").String() != proxyToolSearchLookupName {
			continue
		}
		foundLoadedLookup = true
		parameters := tool.Get("function.parameters")
		if parameters.Get("type").String() != "object" ||
			parameters.Get("properties.query.type").String() != "string" ||
			parameters.Get("properties.limit.type").String() != "integer" ||
			!parameters.Get("additionalProperties").Exists() ||
			parameters.Get("additionalProperties").Bool() {
			t.Fatalf("loaded lookup schema was not preserved: %s", body)
		}
		if parameters.Get("required.0").String() != "query" {
			t.Fatalf("loaded lookup required schema was not preserved: %s", body)
		}
	}
	if !foundLoadedLookup {
		t.Fatalf("Chat tools did not contain flattened loaded lookup %q: %s", proxyToolSearchLookupName, body)
	}

	return searchInternalName
}

func assertProxyToolSearchClientFunctionCall(t *testing.T, body []byte, streaming bool, wantCallID string) {
	t.Helper()
	if !streaming {
		if got := gjson.GetBytes(body, "object").String(); got != "response" {
			t.Fatalf("function-call response object=%q, want response: %s", got, body)
		}
		assertProxyToolSearchFunctionCallItem(t, gjson.GetBytes(body, "output.0"), wantCallID)
		return
	}

	for _, block := range bytes.Split(body, []byte("\n\n")) {
		eventType, data := parseSSEEventChunk(block)
		payload, ok := decodeSSEPayload(data)
		if !ok || eventType != "response.output_item.done" {
			continue
		}
		item, _ := payload["item"].(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		assertProxyToolSearchFunctionCall(t, gjson.ParseBytes(data).Get("item"), wantCallID)
		return
	}
	t.Fatalf("stream did not contain function_call output item: %s", body)
}

func assertProxyToolSearchFunctionCallItem(t *testing.T, item gjson.Result, wantCallID string) {
	t.Helper()
	if item.Get("type").String() != "function_call" ||
		item.Get("name").String() != "lookup" ||
		item.Get("namespace").String() != "mcp__public" ||
		item.Get("call_id").String() != wantCallID {
		t.Fatalf("Responses function call identity = %s, want namespace=mcp__public name=lookup call_id=%s", item.Raw, wantCallID)
	}
}

func assertProxyToolSearchFunctionCall(t *testing.T, item gjson.Result, wantCallID string) {
	t.Helper()
	assertProxyToolSearchFunctionCallItem(t, item, wantCallID)
	arguments := item.Get("arguments")
	if arguments.Type != gjson.String || gjson.Get(arguments.String(), "query").String() != "needle" {
		t.Fatalf("Responses function call arguments=%s, want query=needle", arguments.Raw)
	}
}

func assertProxyToolSearchCompletedResponse(t *testing.T, body []byte, streaming bool) {
	t.Helper()
	if !streaming {
		if got := gjson.GetBytes(body, "status").String(); got != "completed" {
			t.Fatalf("follow-up Responses status=%q, want completed: %s", got, body)
		}
		if got := gjson.GetBytes(body, "output.0.content.0.text").String(); got != proxyToolSearchResultToken {
			t.Fatalf("follow-up Responses marker=%q, want %q: %s", got, proxyToolSearchResultToken, body)
		}
		return
	}

	var marker, status string
	for _, block := range bytes.Split(body, []byte("\n\n")) {
		eventType, data := parseSSEEventChunk(block)
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "response.output_text.delta":
			delta, _ := payload["delta"].(string)
			marker += delta
		case "response.completed":
			response, _ := payload["response"].(map[string]any)
			status, _ = response["status"].(string)
		}
	}
	if marker != proxyToolSearchResultToken || status != "completed" {
		t.Fatalf("follow-up Responses stream marker=%q status=%q, want marker=%q completed: %s", marker, status, proxyToolSearchResultToken, body)
	}
}
