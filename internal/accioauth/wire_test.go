package accioauth

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestBuildChatRequestNativeContract(t *testing.T) {
	const input = `{"messages":[{"role":"system","content":"s"},{"role":"developer","content":"d"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]},{"role":"assistant","content":null,"reasoning_content":"thought","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"},"extra_content":{"google":{"thought_signature":"sig"}}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"reasoning_effort":"medium","temperature":0.2,"top_p":0.9,"max_completion_tokens":23,"stop":"bye","stream":false}`
	for _, protocol := range []string{"responses", "openai", "", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			ctx := t.Context()
			s := &Service{BaseURL: "https://wire.example"}
			req, err := s.BuildChatRequest(ctx, &Credential{Region: "cn", AccessToken: "secret"}, []byte(input), Model{ID: "public", Protocol: protocol, Efforts: []string{"low", "high"}, SupportsImages: true})
			if err != nil {
				t.Fatal(err)
			}
			assertAccioRequestHeaders(t, req, "CN", "text/event-stream")
			if req.Header.Get("version") != "0.34.4" || req.Header.Get("appKey") != "OneChat" {
				t.Fatal("native request omitted Accio version/appKey")
			}
			defer func() { _ = req.Body.Close() }()
			if req.Context() != ctx || req.Method != http.MethodPost || req.URL.Path != "/api/adk/llm/generateContent" || req.Header.Get("appKey") == "" || req.Header.Get("Authorization") != "" || req.Header.Get("x-package-region") != "CN" || req.Header.Get("Accept") != "text/event-stream" {
				t.Fatalf("invalid native request: %v, %v", req.URL, req.Header)
			}
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			requestID := body["request_id"].(string)
			if requestID == "" || body["message_id"] == "" || req.URL.Query().Get("sg_k") != fmt.Sprintf("%x", md5.Sum([]byte(requestID))) {
				t.Fatal("missing native request markers")
			}
			if body["token"] != "secret" || body["model"] != "public" || body["system_instruction"] != "s\n\nd" || body["max_output_tokens"] != float64(23) || body["include_thoughts"] != true {
				t.Fatalf("invalid body: %v", body)
			}
			properties := body["properties"].(map[string]any)
			if protocol == "openai" || protocol == "responses" {
				if properties["reasoning_effort"] != "low" || body["reasoning_effort"] != nil {
					t.Fatal("wrong properties effort placement")
				}
			} else if body["reasoning_effort"] != "low" || properties["reasoning_effort"] != nil {
				t.Fatal("wrong top-level effort placement")
			}
			tools := body["tools"].([]any)
			var schema map[string]any
			if json.Unmarshal([]byte(tools[0].(map[string]any)["parameters_json"].(string)), &schema) != nil || schema["type"] != "object" || !json.Valid([]byte(body["tool_config"].(string))) {
				t.Fatal("tools must carry stringified JSON")
			}
			contents := body["contents"].([]any)
			userParts := contents[0].(map[string]any)["parts"].([]any)
			if len(userParts) != 3 || userParts[1].(map[string]any)["inline_data"] == nil || userParts[2].(map[string]any)["file_data"] == nil {
				t.Fatal("image parts lost")
			}
			assistant := contents[1].(map[string]any)["parts"].([]any)
			callPart := assistant[1].(map[string]any)
			if callPart["thought_signature"] != "sig" || callPart["function_call"].(map[string]any)["args_json"] != `{"path":"a"}` {
				t.Fatal("tool history lost")
			}
			response := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["function_response"].(map[string]any)
			var result map[string]any
			if response["name"] != "read" || json.Unmarshal([]byte(response["response_json"].(string)), &result) != nil || result["content"] != "ok" || result["is_error"] != false {
				t.Fatal("tool result lost")
			}
		})
	}
}

/**
 * @brief 验证会话字段的别名优先级、空值和类型边界，以及每次请求独立生成消息标识。
 * @param t 当前测试上下文。
 */
func TestBuildChatRequestSessionFields(t *testing.T) {
	for _, tc := range []struct {
		name   string    ///< 用例名称。
		fields string    ///< 客户端会话字段 JSON。
		want   [3]string ///< 原生 session_key、conversation_id、conversation_name。
	}{
		{name: "missing"},
		{name: "native priority", fields: `,"session_key":" session ","sessionKey":"other","session_id":"other","sessionId":42,"conversation_id":" conversation ","conversationId":false,"conversation_name":" title ","conversationName":[]`, want: [3]string{"session", "conversation", "title"}},
		{name: "camel aliases", fields: `,"session_key":null,"sessionKey":" camel ","session_id":"other","conversation_id":" ","conversationId":" conversation ","conversation_name":null,"conversationName":" title "`, want: [3]string{"camel", "conversation", "title"}},
		{name: "snake session id", fields: `,"session_key":" ","sessionKey":null,"session_id":" snake ","sessionId":"other"`, want: [3]string{"snake", "", ""}},
		{name: "camel session id", fields: `,"session_key":null,"sessionKey":" ","session_id":"\t","sessionId":" camel-id "`, want: [3]string{"camel-id", "", ""}},
		{name: "empty", fields: `,"session_key":null,"sessionKey":" ","session_id":"","sessionId":"\t","conversation_id":null,"conversationId":" ","conversation_name":"\n","conversationName":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"messages":[{"role":"user","content":"hi"}],"request_id":"client-request","message_id":"client-message"` + tc.fields + `}`)
			var previousRequest, previousMessage string
			for range 2 {
				req, err := (&Service{}).BuildChatRequest(t.Context(), &Credential{Region: "global", AccessToken: "token"}, input, Model{ID: "native"})
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				err = json.NewDecoder(req.Body).Decode(&body)
				_ = req.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				for i, field := range []string{"session_key", "conversation_id", "conversation_name"} {
					value, exists := body[field]
					if tc.want[i] == "" {
						if exists {
							t.Errorf("empty %s sent as %#v", field, value)
						}
					} else if value != tc.want[i] {
						t.Errorf("%s = %#v, want %q", field, value, tc.want[i])
					}
				}
				for _, alias := range []string{"sessionKey", "session_id", "sessionId", "conversationId", "conversationName"} {
					if _, exists := body[alias]; exists {
						t.Errorf("native payload included alias %s", alias)
					}
				}
				requestID, messageID := body["request_id"].(string), body["message_id"].(string)
				if requestID == "" || messageID == "" || requestID == "client-request" || messageID == "client-message" || requestID == previousRequest || messageID == previousMessage {
					t.Fatalf("request markers were not generated independently: request=%q message=%q", requestID, messageID)
				}
				previousRequest, previousMessage = requestID, messageID
			}
		})
	}
	for field, value := range map[string]string{
		"session_key": "42", "sessionKey": "true", "session_id": "[]", "sessionId": "{}",
		"conversation_id": "42", "conversationId": "true", "conversation_name": "[]", "conversationName": "{}",
	} {
		t.Run("invalid/"+field, func(t *testing.T) {
			input := []byte(`{"messages":[{"role":"user","content":"hi"}],"` + field + `":` + value + `}`)
			if req, err := (&Service{}).BuildChatRequest(t.Context(), &Credential{Region: "global", AccessToken: "token"}, input, Model{ID: "native"}); err == nil {
				_ = req.Body.Close()
				t.Fatal("non-string session field accepted")
			} else if !strings.Contains(err.Error(), field) {
				t.Fatalf("error omitted the invalid field: %v", err)
			}
		})
	}
}

func TestBuildChatRequestDeviceIdentity(t *testing.T) {
	service := NewService(nil)
	credential := &Credential{Region: "global", UserID: "account-a", AccessToken: "access"}
	device := func(c *Credential) string {
		t.Helper()
		req, err := service.BuildChatRequest(context.Background(), c, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model{ID: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = req.Body.Close() }()
		return req.Header.Get("utdid")
	}
	original := device(credential)
	if !strings.HasPrefix(original, "desktop-") || original != device(credential) {
		t.Fatal("missing stable device identity")
	}
	credential.AccessToken = "rotated"
	if device(credential) != original {
		t.Fatal("token rotation changed device identity")
	}
	credential.Region = "cn"
	if device(credential) == original {
		t.Fatal("device identity shared across regions")
	}
	credential.Region = "global"
	credential.UserID = "account-b"
	if device(credential) == original {
		t.Fatal("device identity shared across accounts")
	}
	credential.DeviceID = "official-device"
	if device(credential) != "official-device" {
		t.Fatal("imported device identity ignored")
	}
	for _, cookie := range []string{"", "official-session-cookie"} {
		credential.SGCookie = cookie
		req, err := service.BuildChatRequest(context.Background(), credential, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model{ID: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		_ = req.Body.Close()
		if req.Header.Get("sgcookie") != cookie {
			t.Fatal("imported session cookie ignored")
		}
	}
}

func TestBuildChatRequestRejectsUnsupportedPayload(t *testing.T) {
	for name, input := range map[string]string{
		"content":     `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
		"tools":       `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search"}]}`,
		"choice":      `{"messages":[{"role":"user","content":"x"}],"tool_choice":"required"}`,
		"arguments":   `{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{"}}]}]}`,
		"tool result": `{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`,
		"image":       `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		"effort":      `{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"high"}`,
		"format":      `{"messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Service{}).BuildChatRequest(context.Background(), &Credential{Region: "global", AccessToken: "t"}, []byte(input), Model{ID: "native"}); err == nil {
				t.Fatal("unsupported input accepted")
			}
		})
	}
}

func readADKStream(t *testing.T, source io.ReadCloser) ([]map[string]any, bool, error) {
	t.Helper()
	stream := NormalizeStream(source, "public-model")
	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(stream)
	if err != nil {
		return nil, false, err
	}
	var chunks []map[string]any
	done := false
	for event := range strings.SplitSeq(string(body), "\n\n") {
		if event == "" {
			continue
		}
		data := strings.TrimPrefix(event, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("invalid normalized JSON: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, done, nil
}

func adkEvent(value any) string {
	encoded, _ := json.Marshal(value)
	return "data: " + string(encoded) + "\n\n"
}

func TestNormalizeStreamPreservesTextReasoningToolsAndUsage(t *testing.T) {
	frames := []any{
		map[string]any{"content": map[string]any{"parts": []any{map[string]any{"thought": true, "text": "想"}, map[string]any{"text": "回答"}}}},
		map[string]any{"content": map[string]any{"parts": []any{map[string]any{"function_call": map[string]any{"id": "a", "name": "read", "args_json": `{"path":"`}, "thought_signature": "opaque"}}}},
		map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"id": "b", "name": "read", "args": map[string]any{"path": "b"}}}}}},
		map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"id": "a", "argsJson": `a"}`}}}}},
		map[string]any{"finishReason": "TOOL_CALLS", "usageMetadata": map[string]any{"promptTokenCount": 8, "candidatesTokenCount": 3, "thoughtsTokenCount": 2, "cachedContentTokenCount": 4}},
	}
	var input strings.Builder
	input.WriteString(":heartbeat\n\n")
	for _, frame := range frames {
		input.WriteString(adkEvent(frame))
	}
	input.WriteString("data: [DONE]\n\n")
	chunks, done, err := readADKStream(t, io.NopCloser(&adkByteReader{text: input.String()}))
	if err != nil || !done {
		t.Fatalf("stream failed: %v done=%v", err, done)
	}
	text, reasoning := "", ""
	args, names, ids := map[int]string{}, map[int]string{}, map[int]string{}
	var usage map[string]any
	signature, finish := "", ""
	for _, chunk := range chunks {
		if chunk["error"] != nil || chunk["model"] != "public-model" {
			t.Fatalf("unexpected chunk: %v", chunk)
		}
		if value, ok := chunk["usage"].(map[string]any); ok {
			usage = value
		}
		for _, raw := range chunk["choices"].([]any) {
			choice := raw.(map[string]any)
			delta := choice["delta"].(map[string]any)
			text += wireText(delta["content"])
			reasoning += wireText(delta["reasoning_content"])
			if value := wireText(choice["finish_reason"]); value != "" {
				finish = value
			}
			calls, _ := delta["tool_calls"].([]any)
			for _, raw := range calls {
				call := raw.(map[string]any)
				index := int(call["index"].(float64))
				function := call["function"].(map[string]any)
				args[index] += wireText(function["arguments"])
				if name := wireText(function["name"]); name != "" {
					names[index] = name
					ids[index] = wireText(call["id"])
				}
				if extra, ok := call["extra_content"].(map[string]any); ok {
					signature = extra["google"].(map[string]any)["thought_signature"].(string)
				}
			}
		}
	}
	if text != "回答" || reasoning != "想" || len(args) != 2 || args[0] != `{"path":"a"}` || args[1] != `{"path":"b"}` || names[0] != "read" || ids[0] != "a" || signature != "opaque" || finish != "tool_calls" {
		t.Fatalf("lost content/tools: %q %q %v %v %v %s %s", text, reasoning, args, names, ids, signature, finish)
	}
	if usage["prompt_tokens"] != float64(8) || usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(13) || usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(4) || usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("invalid usage: %v", usage)
	}
}

/**
 * @brief 思考标签跨原生帧时仍与正文分离，结构化思考、签名和未完成文本保持完整。
 * @param t 测试上下文。
 */
func TestNormalizeStreamThinkingTags(t *testing.T) {
	for _, tc := range []struct {
		name, input, content, reasoning string
	}{
		{"mixed", "前<think>推理</think>后<thinking>分析</thinking>答", "前后答", "推理分析"},
		{"plain", "x < y <this> z", "x < y <this> z", ""},
		{"partial opening", "answer<thi", "answer<thi", ""},
		{"partial closing", "<think>why</thi", "", "why</thi"},
		{"unclosed", "<think>why", "", "why"},
	} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", tc.name, split), func(t *testing.T) {
				pieces := []string{tc.input}
				if split {
					pieces = strings.Split(tc.input, "")
				}
				var input strings.Builder
				for i, text := range pieces {
					part := map[string]any{"text": text}
					if i == 0 {
						part["thought_signature"] = "opaque"
					}
					input.WriteString(adkEvent(map[string]any{"content": map[string]any{"parts": []any{part}}}))
				}
				input.WriteString("data: [DONE]\n\n")
				chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(input.String())))
				if err != nil || !done {
					t.Fatalf("stream failed: %v done=%v", err, done)
				}
				var content, reasoning string
				var signatures []string
				for _, chunk := range chunks {
					if chunk["error"] != nil {
						t.Fatalf("unexpected error: %v", chunk)
					}
					for _, raw := range chunk["choices"].([]any) {
						delta := raw.(map[string]any)["delta"].(map[string]any)
						content += wireText(delta["content"])
						reasoning += wireText(delta["reasoning_content"])
						if extra, ok := delta["extra_content"].(map[string]any); ok {
							signatures = append(signatures, extra["google"].(map[string]any)["thought_signature"].(string))
						}
					}
				}
				if content != tc.content || reasoning != tc.reasoning {
					t.Fatalf("content=%q reasoning=%q; want %q / %q", content, reasoning, tc.content, tc.reasoning)
				}
				if !slices.Equal(signatures, []string{"opaque"}) {
					t.Fatalf("signatures lost or duplicated: %v", signatures)
				}
			})
		}
	}
}

/**
 * @brief 标签缓冲在结构化思考、工具和流失败之前排空，保留输出顺序与原生签名。
 * @param t 测试上下文。
 */
func TestNormalizeStreamThinkingBoundaries(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			parts := []any{
				map[string]any{"text": "before<thi"},
				map[string]any{"text": "native<think>literal</think>", "thought": true, "thought_signature": "opaque"},
				map[string]any{"text": "nk>after<thinking>reason</thinking><"},
				map[string]any{"function_call": map[string]any{"id": "call", "name": "lookup", "args_json": "{}"}},
				map[string]any{"text": "tail<thi"},
			}
			input := adkEvent(map[string]any{"content": map[string]any{"parts": parts}})
			if terminal {
				input += "data: [DONE]\n\n"
			}
			chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(input)))
			if err != nil || done != terminal {
				t.Fatalf("stream failed: %v done=%v", err, done)
			}
			/**
			 * @brief 客户端可见的语义内容，忽略同类型文本的传输分片。
			 */
			type event struct{ kind, text string }
			var events []event
			appendText := func(kind, text string) {
				if text == "" {
					return
				}
				if len(events) > 0 && events[len(events)-1].kind == kind {
					events[len(events)-1].text += text
				} else {
					events = append(events, event{kind, text})
				}
			}
			signature := ""
			for _, chunk := range chunks {
				if chunk["error"] != nil {
					events = append(events, event{kind: "error"})
				}
				choices, _ := chunk["choices"].([]any)
				for _, raw := range choices {
					delta := raw.(map[string]any)["delta"].(map[string]any)
					appendText("text", wireText(delta["content"]))
					appendText("thought", wireText(delta["reasoning_content"]))
					if extra, ok := delta["extra_content"].(map[string]any); ok {
						signature = extra["google"].(map[string]any)["thought_signature"].(string)
					}
					if delta["tool_calls"] != nil {
						events = append(events, event{kind: "tool"})
					}
				}
			}
			want := []event{
				{"text", "before<thi"},
				{"thought", "native<think>literal</think>"},
				{"text", "nk>after"},
				{"thought", "reason"},
				{"text", "<"},
				{"tool", ""},
				{"text", "tail<thi"},
			}
			if !terminal {
				want = append(want, event{kind: "error"})
			}
			if !slices.Equal(events, want) || signature != "opaque" {
				t.Fatalf("events=%v signature=%q; want %v", events, signature, want)
			}
		})
	}
}

func TestNormalizeStreamMultilineAndTermination(t *testing.T) {
	for name, input := range map[string]string{
		"multiline":     "data: {\"content\":{\"parts\":[{\"text\":\"a\"}]},\r\ndata: \"turn_complete\":true,\"finish_reason\":\"STOP\"}\r\n\r\n",
		"finish at eof": "data: {\"finish_reason\":\"STOP\"}",
		"done":          "data: [DONE]\n\n",
		"turn complete": "data: {\"turnComplete\":true}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(input)))
			if err != nil || !done || chunks[len(chunks)-1]["error"] != nil {
				t.Fatalf("expected terminal success: %v %v %v", chunks, done, err)
			}
		})
	}
}

func TestNormalizeStreamEmptyContentPreservesCompletionAndUsage(t *testing.T) {
	for name, content := range map[string]any{
		"empty object": map[string]any{},
		"role only":    map[string]any{"role": "model"},
		"null parts":   map[string]any{"role": "model", "parts": nil},
		"empty parts":  map[string]any{"role": "model", "parts": []any{}},
	} {
		for _, termination := range []string{"turn_complete", "finish_reason", "done"} {
			t.Run(name+"/"+termination, func(t *testing.T) {
				frame := map[string]any{
					"content":        content,
					"usage_metadata": map[string]any{"prompt_token_count": 10, "candidates_token_count": 7, "total_token_count": 17},
				}
				switch termination {
				case "turn_complete":
					frame["turn_complete"] = true
				case "finish_reason":
					frame["finish_reason"] = "STOP"
				}
				input := adkEvent(map[string]any{"content": content}) +
					adkEvent(map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "回答"}}}}) + adkEvent(frame)
				if termination == "done" {
					input += "data: [DONE]\n\n"
				}
				chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(input)))
				if err != nil || !done {
					t.Fatalf("expected completed stream: chunks=%v done=%v err=%v", chunks, done, err)
				}
				var text string
				var usage map[string]any
				finishes := 0
				for _, chunk := range chunks {
					if chunk["error"] != nil {
						t.Fatalf("unexpected SSE error: %v", chunk)
					}
					if value, ok := chunk["usage"].(map[string]any); ok {
						usage = value
					}
					for _, raw := range chunk["choices"].([]any) {
						choice := raw.(map[string]any)
						text += wireText(choice["delta"].(map[string]any)["content"])
						if choice["finish_reason"] != nil {
							if choice["finish_reason"] != "stop" {
								t.Fatalf("unexpected finish: %v", choice)
							}
							finishes++
						}
					}
				}
				if text != "回答" || finishes != 1 || usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(17) {
					t.Fatalf("lost content/completion/usage: text=%q finishes=%d usage=%v", text, finishes, usage)
				}
			})
		}
	}
}

func TestNormalizeStreamToolArgumentsWithoutInitialIDs(t *testing.T) {
	part := func(call map[string]any) string {
		return adkEvent(map[string]any{"content": map[string]any{"parts": []any{map[string]any{"function_call": call}}}})
	}
	input := part(map[string]any{"name": "f", "args_json": `{"a":`}) +
		part(map[string]any{"id": "late", "args_json": `1}`}) +
		part(map[string]any{"name": "f", "args": map[string]any{"a": 2}}) +
		part(map[string]any{"name": "f", "args": `{"a":3}`}) +
		adkEvent(map[string]any{"turn_complete": true})
	chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(input)))
	if err != nil || !done {
		t.Fatalf("tool normalization failed: %v %v", done, err)
	}
	arguments, identities := map[int]string{}, map[int]string{}
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			delta := raw.(map[string]any)["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call := rawCall.(map[string]any)
				index := int(call["index"].(float64))
				arguments[index] += call["function"].(map[string]any)["arguments"].(string)
				if id := wireText(call["id"]); id != "" {
					if identities[index] != "" {
						t.Fatal("tool identity announced more than once")
					}
					identities[index] = id
				}
			}
		}
	}
	if len(arguments) != 3 || len(identities) != 3 || arguments[0] != `{"a":1}` || arguments[1] != `{"a":2}` || arguments[2] != `{"a":3}` {
		t.Fatalf("arguments corrupted or calls merged: %v %v", arguments, identities)
	}
}

func TestNormalizeStreamErrorsNeverFinishSuccessfully(t *testing.T) {
	for name, tc := range map[string]struct {
		input  string
		status int
	}{
		"empty eof":         {"", 502},
		"empty content eof": {`data: {"content":{}}` + "\n\n", 502},
		"null parts eof":    {`data: {"content":{"parts":null}}` + "\n\n", 502},
		"object parts":      {`data: {"content":{"parts":{}},"turn_complete":true}` + "\n\n", 502},
		"string parts":      {`data: {"content":{"parts":""},"turn_complete":true}` + "\n\n", 502},
		"content eof":       {`data: {"content":{"parts":[{"text":"partial"}]}}` + "\n\n", 502},
		"invalid json":      {"data: {\n\n", 502},
		"auth":              {`data: {"error_code":"401","error_message":"denied"}` + "\n\n", 401},
		"quota":             {`data: {"errorMessage":"积分不足"}` + "\n\n", 429},
		"unknown":           {`data: {"errorCode":"500","errorMessage":"broken"}` + "\n\n", 502},
		"unsupported part":  {`data: {"content":{"parts":[{"inline_data":{"data":"a"}}]},"turn_complete":true}` + "\n\n", 502},
		"partial tool":      {`data: {"content":{"parts":[{"function_call":{"name":"f","args_json":"{"}}]},"turn_complete":true}` + "\n\n", 502},
	} {
		t.Run(name, func(t *testing.T) {
			chunks, done, err := readADKStream(t, io.NopCloser(strings.NewReader(tc.input)))
			if err != nil || done || len(chunks) == 0 {
				t.Fatalf("expected SSE error: %v %v %v", chunks, done, err)
			}
			last := chunks[len(chunks)-1]["error"].(map[string]any)
			if last["status"] != float64(tc.status) || chunks[len(chunks)-1]["status"] != float64(tc.status) {
				t.Fatalf("status=%v want %d", last, tc.status)
			}
			for _, chunk := range chunks {
				choices, _ := chunk["choices"].([]any)
				for _, choice := range choices {
					if choice.(map[string]any)["finish_reason"] != nil {
						t.Fatal("error stream emitted success finish")
					}
				}
			}
		})
	}
}

func TestBuildChatRequestAllowsParallelTools(t *testing.T) {
	req, err := (&Service{}).BuildChatRequest(context.Background(), &Credential{Region: "global", AccessToken: "t"}, []byte(`{"messages":[{"role":"user","content":"x"}],"parallel_tool_calls":true}`), Model{ID: "native"})
	if err != nil {
		t.Fatal(err)
	}
	_ = req.Body.Close()
	if _, err := (&Service{}).BuildChatRequest(context.Background(), &Credential{Region: "global", AccessToken: "t"}, []byte(`{"messages":[{"role":"user","content":"x"}],"parallel_tool_calls":false}`), Model{ID: "native"}); err == nil {
		t.Fatal("unsupported serial tool execution contract accepted")
	}
}

func TestClassifyError(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{{403, "denied", 401}, {400, "invalid token", 401}, {500, "quota exceeded", 429}, {429, "busy", 429}, {400, `{"error":{"message":"bad input"}}`, 502}, {200, `{"error_code":"429"}`, 429}} {
		if got := ClassifyError(tc.status, []byte(tc.body)); got.StatusCode != tc.want || got.Message == "" {
			t.Fatalf("classify %d %q = %v", tc.status, tc.body, got)
		}
	}
}

type adkByteReader struct{ text string }

func (r *adkByteReader) Read(p []byte) (int, error) {
	if len(r.text) == 0 {
		return 0, io.EOF
	}
	p[0], r.text = r.text[0], r.text[1:]
	return 1, nil
}

type adkErrorReader struct{ cause error }

func (r adkErrorReader) Read([]byte) (int, error) { return 0, r.cause }

func TestNormalizeStreamCancellationAndClose(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		_, _, err := readADKStream(t, io.NopCloser(adkErrorReader{cause: cause}))
		if !errors.Is(err, cause) {
			t.Fatalf("cancellation lost: %v", err)
		}
	}
	source := &adkBlockingReader{started: make(chan struct{}), closed: make(chan struct{})}
	stream := NormalizeStream(source, "m")
	result := make(chan error, 1)
	go func() { _, err := io.ReadAll(stream); result <- err }()
	<-source.started
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("closing did not unblock source: %v", err)
	}
}

type adkBlockingReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *adkBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, context.Canceled
}

func (r *adkBlockingReader) Close() error { close(r.closed); return nil }
