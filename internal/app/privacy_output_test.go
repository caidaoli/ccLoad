package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/privacy"

	"github.com/tidwall/gjson"
)

func privacyOutputTestSession(t *testing.T) *privacy.Session {
	t.Helper()
	config := privacy.DefaultConfig()
	config.Enabled, config.Personal = true, true
	policy, err := privacy.NewPolicy(config)
	if err != nil {
		t.Fatal(err)
	}
	return privacy.NewEngine().Session(t.Name(), policy)
}

func privacyOutputTestToken(t *testing.T, session *privacy.Session, secret string) string {
	t.Helper()
	// A tool payload's secret field supports real multiline credentials too;
	// configured word-list entries deliberately reject newlines.
	body := privacyOutputTestJSON(t, map[string]any{"arguments": map[string]string{"password": secret}})
	masked, err := session.MaskJSON([]byte(body))
	if err != nil {
		t.Fatalf("MaskJSON = %v", err)
	}
	token := gjson.GetBytes(masked, "arguments.password").String()
	if token == secret || !strings.HasPrefix(token, "{{CCLOAD_") {
		t.Fatalf("MaskJSON = %q; wanted a placeholder", token)
	}
	return token
}

func privacyOutputTestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func privacyOutputTestWrite(t *testing.T, writer http.ResponseWriter, wire string, chunk int) {
	t.Helper()
	for len(wire) > 0 {
		length := min(chunk, len(wire))
		if n, err := writer.Write([]byte(wire[:length])); n != length || err != nil {
			t.Fatalf("Write = %d, %v; wanted %d", n, err, length)
		}
		wire = wire[length:]
	}
}

// Parse the emitted protocol independently of the production SSE parser.
func privacyOutputTestEvents(t *testing.T, wire string) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	var data []string
	emit := func() {
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "" || payload == "[DONE]" {
			return
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("invalid SSE JSON: %q", payload)
		}
		events = append(events, gjson.Parse(payload))
	}
	scanner := bufio.NewScanner(strings.NewReader(wire))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			emit()
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	emit()
	return events
}

func TestPrivacyResponseWriterJSON(t *testing.T) {
	secret := "private\"\\value\n中文"
	session := privacyOutputTestSession(t)
	token := privacyOutputTestToken(t, session, secret)
	arguments := privacyOutputTestJSON(t, map[string]string{"value": token})
	wire := `{"id":` + privacyOutputTestJSON(t, token) + `,"choices":[{"message":{"content":` + privacyOutputTestJSON(t, token) + `,"tool_calls":[{"id":"call_1","function":{"name":"lookup","arguments":` + privacyOutputTestJSON(t, arguments) + `}}]}}],"usage":{"total_tokens":9007199254740993}}`
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", "1234")
	privacyOutputTestWrite(t, writer, wire, 3)
	writer.Flush()
	if recorder.Body.Len() != 0 || recorder.Flushed {
		t.Fatal("partial JSON was committed before Finish")
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	result := gjson.Parse(recorder.Body.String())
	if result.Get("choices.0.message.content").String() != secret || result.Get("id").String() != token {
		t.Fatalf("content/identifier contract changed: %s", recorder.Body.String())
	}
	var restoredArguments map[string]string
	if err := json.Unmarshal([]byte(result.Get("choices.0.message.tool_calls.0.function.arguments").String()), &restoredArguments); err != nil || restoredArguments["value"] != secret {
		t.Fatalf("restored tool arguments = %v, %v", restoredArguments, err)
	}
	if result.Get("usage.total_tokens").Raw != "9007199254740993" || writer.Header().Get("Content-Length") != "" {
		t.Fatal("usage lost precision or stale Content-Length survived")
	}
	before := recorder.Body.String()
	if err := writer.Finish(); err != nil || recorder.Body.String() != before {
		t.Fatalf("Finish is not idempotent: %v", err)
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after Finish = %v", err)
	}

	t.Run("array", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer := newPrivacyResponseWriter(recorder, session)
		writer.Header().Set("Content-Type", "application/json")
		privacyOutputTestWrite(t, writer, `[{"message":{"content":`+privacyOutputTestJSON(t, token)+`}}]`, 3)
		if err := writer.Finish(); err != nil {
			t.Fatal(err)
		}
		if got := gjson.Get(recorder.Body.String(), "0.message.content").String(); got != secret {
			t.Fatalf("JSON array content = %q, want %q", got, secret)
		}
	})
}

func TestPrivacyResponseWriterChatSplitEveryPosition(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "owner@example.com"
	token := privacyOutputTestToken(t, session, secret)
	for split := 1; split < len(token); split++ {
		t.Run(token[:split], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			writer.Header().Set("Content-Type", "text/event-stream")
			first := "id: first\r\nevent: message\r\ndata: " + `{"choices":[{"index":0,"delta":{"content":` + privacyOutputTestJSON(t, "before "+token[:split]) + `}}]}` + "\r\n\r\n"
			privacyOutputTestWrite(t, writer, first, 1)
			writer.Flush()
			if recorder.Body.Len() != 0 || recorder.Flushed {
				t.Fatal("partial placeholder was committed")
			}
			second := ": heartbeat\r\n\r\nid: second\r\ndata: " + `{"choices":[{"index":0,"delta":{"content":` + privacyOutputTestJSON(t, token[split:]+" after") + `}}]}` + "\r\n\r\n"
			usage := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":42}}\r\n\r\ndata: [DONE]\r\n\r\n"
			privacyOutputTestWrite(t, writer, second+usage, 7)
			writer.Flush()
			if err := writer.Finish(); err != nil {
				t.Fatal(err)
			}
			if got := writer.observer.UnrestoredTokens(); got != 0 {
				t.Fatalf("valid split placeholder counted as unrestored: %d", got)
			}
			events := privacyOutputTestEvents(t, recorder.Body.String())
			if len(events) != 3 || events[0].Get("choices.0.delta.content").String()+events[1].Get("choices.0.delta.content").String() != "before "+secret+" after" {
				t.Fatalf("split delta was not restored: %s", recorder.Body.String())
			}
			if events[2].Get("usage.total_tokens").Int() != 42 || events[2].Get("choices.0.finish_reason").String() != "stop" {
				t.Fatal("usage/finish was changed")
			}
			if !strings.HasPrefix(recorder.Body.String(), "id: first\r\nevent: message\r\ndata: ") || !strings.Contains(recorder.Body.String(), ": heartbeat\r\n\r\nid: second\r\n") || !strings.HasSuffix(recorder.Body.String(), usage) {
				t.Fatalf("SSE framing/order changed: %q", recorder.Body.String())
			}
		})
	}
}

func TestPrivacyResponseWriterParallelToolArguments(t *testing.T) {
	secrets := []string{"private\"\\first\n值", "second\"\\private", "third\tprivate"}
	session := privacyOutputTestSession(t)
	tokens := make([]string, len(secrets))
	for i, secret := range secrets {
		tokens[i] = privacyOutputTestToken(t, session, secret)
	}
	tool := func(index int, argument string, initial bool) map[string]any {
		value := map[string]any{"index": index, "function": map[string]any{"arguments": argument}}
		if initial {
			value["id"] = "call_" + tokens[index]
			value["function"].(map[string]any)["name"] = "lookup"
		}
		return value
	}
	choice := func(index int, tools ...map[string]any) map[string]any {
		return map[string]any{"index": index, "delta": map[string]any{"tool_calls": tools}}
	}
	frame := func(choices ...map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": choices}) + "\n\n"
	}
	split := 15
	first := frame(choice(0, tool(0, `{"value":"`+tokens[0][:split], true), tool(1, `{"value":"`+tokens[1][:split], true)), choice(1, tool(0, `{"value":"`+tokens[2][:split], true)))
	second := frame(choice(0, tool(1, tokens[1][split:]+`"}`, false)), choice(1, tool(0, tokens[2][split:]+`"}`, false)))
	third := frame(choice(0, tool(0, tokens[0][split:]+`"}`, false)))
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, first+second+third+"data: [DONE]\n\n", 2)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	arguments := make(map[string]string)
	for eventIndex, event := range privacyOutputTestEvents(t, recorder.Body.String()) {
		for _, outputChoice := range event.Get("choices").Array() {
			for _, outputTool := range outputChoice.Get("delta.tool_calls").Array() {
				key := outputChoice.Get("index").String() + "/" + outputTool.Get("index").String()
				arguments[key] += outputTool.Get("function.arguments").String()
				if eventIndex == 0 && (outputTool.Get("function.name").String() != "lookup" || !strings.Contains(outputTool.Get("id").String(), "{{CCLOAD_")) {
					t.Fatal("tool name/identifier was restored or dropped")
				}
			}
		}
	}
	for i, key := range []string{"0/0", "0/1", "1/0"} {
		var value map[string]string
		if err := json.Unmarshal([]byte(arguments[key]), &value); err != nil || value["value"] != secrets[i] {
			t.Fatalf("tool %s arguments = %q (%v); wanted %q", key, arguments[key], err, secrets[i])
		}
	}
}

func TestPrivacyResponseWriterProtocolChannels(t *testing.T) {
	secret := "private\"\\value\n值"
	session := privacyOutputTestSession(t)
	token := privacyOutputTestToken(t, session, secret)
	tests := []struct {
		name, path, terminal string
		arguments            bool
		payload              func(string) any
	}{
		{"anthropic text", "delta.text", `{"type":"content_block_stop","index":3}`, false, func(text string) any {
			return map[string]any{"type": "content_block_delta", "index": 3, "delta": map[string]any{"type": "text_delta", "text": text}}
		}},
		{"anthropic tool", "delta.partial_json", `{"type":"content_block_stop","index":4}`, true, func(text string) any {
			return map[string]any{"type": "content_block_delta", "index": 4, "delta": map[string]any{"type": "input_json_delta", "partial_json": text}}
		}},
		{"responses text", "delta", `{"type":"response.completed","response":{"id":"resp_1","output":[],"usage":{"total_tokens":9}}}`, false, func(text string) any {
			return map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 2, "content_index": 1, "delta": text}
		}},
		{"responses tool", "delta", `{"type":"response.completed","response":{"id":"resp_1","output":[]}}`, true, func(text string) any {
			return map[string]any{"type": "response.function_call_arguments.delta", "item_id": "call_1", "output_index": 3, "delta": text}
		}},
		{"gemini text", "candidates.0.content.parts.0.text", `{"candidates":[{"index":2,"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":9}}`, false, func(text string) any {
			return map[string]any{"candidates": []any{map[string]any{"index": 2, "content": map[string]any{"parts": []any{map[string]any{"text": text}}}}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			// Exercise content sniffing used by the WebSocket SSE bridge too.
			left, right := token[:12], token[12:]
			if test.arguments {
				left, right = `{"value":"`+left, right+`"}`
			}
			wire := "event: delta\ndata: " + privacyOutputTestJSON(t, test.payload(left)) + "\n\ndata: " + privacyOutputTestJSON(t, test.payload(right)) + "\n\ndata: " + test.terminal + "\n\n"
			privacyOutputTestWrite(t, writer, wire, 1)
			if err := writer.Finish(); err != nil {
				t.Fatal(err)
			}
			events := privacyOutputTestEvents(t, recorder.Body.String())
			if len(events) != 3 {
				t.Fatalf("events = %d", len(events))
			}
			value := events[0].Get(test.path).String() + events[1].Get(test.path).String()
			if test.arguments {
				var arguments map[string]string
				if err := json.Unmarshal([]byte(value), &arguments); err != nil {
					t.Fatalf("invalid nested JSON %q: %v", value, err)
				}
				value = arguments["value"]
			}
			if value != secret || !strings.HasSuffix(recorder.Body.String(), "data: "+test.terminal+"\n\n") {
				t.Fatalf("protocol content/terminal changed: %s", recorder.Body.String())
			}
		})
	}
}

func TestPrivacyResponseWriterFinishIncompletePrefix(t *testing.T) {
	for _, terminal := range []string{"", "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n", "data: [DONE]\n\n"} {
		t.Run(terminal, func(t *testing.T) {
			session := privacyOutputTestSession(t)
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			writer.Header().Set("Content-Type", "text/event-stream")
			wire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"literal {{CCLOAD_\"}}]}\n\n"
			privacyOutputTestWrite(t, writer, wire, 10)
			if recorder.Body.Len() != 0 {
				t.Fatal("incomplete prefix was not retained")
			}
			privacyOutputTestWrite(t, writer, terminal, 20)
			if terminal != "" && recorder.Body.String() != wire+terminal {
				t.Fatal("terminal did not release pending content")
			}
			if err := writer.Finish(); err != nil || recorder.Body.String() != wire+terminal {
				t.Fatalf("Finish changed incomplete/literal content: %q, %v", recorder.Body.String(), err)
			}
		})
	}
}

func TestPrivacyResponseWriterPreservesUnchangedSSE(t *testing.T) {
	for _, text := range []string{"ordinary text", `ordinary \u0026 \u4f60 {{CCLOAD_EMAIL_0123456789abcdef}}`} {
		session := privacyOutputTestSession(t)
		wire := ": keepalive\r\n\r\nid: original\r\nretry: 1500\r\nevent: response.output_text.delta\r\ndata: { \"type\": \"response.output_text.delta\",\r\ndata: \"output_index\":0,\"content_index\":0, \"delta\":\"" + text + "\", \"number\":9007199254740993 }\r\n\r\ndata: [DONE]\r\n\r\n"
		recorder := httptest.NewRecorder()
		writer := newPrivacyResponseWriter(recorder, session)
		writer.Header().Set("Content-Type", "text/event-stream")
		privacyOutputTestWrite(t, writer, wire, 5)
		if err := writer.Finish(); err != nil || recorder.Body.String() != wire {
			t.Fatalf("unchanged SSE was reformatted: %q, %v", recorder.Body.String(), err)
		}
	}
}

/**
 * @brief 验证 Gemini 思考与回答按不同通道拼接并还原，签名按所属 part 安全回放。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterGeminiThoughts(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "owner@example.com"
	token := privacyOutputTestToken(t, session, secret)
	signature := "opaque-thought-signature"
	gemini := func(parts ...map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{
			"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": parts}}},
		}) + "\n\n"
	}
	signedPart := gemini(map[string]any{"text": "签名 " + token, "thought": true, "thought_signature": signature})
	signedPart = strings.Replace(signedPart, "{{CCLOAD_", "\\u007b\\u007bCCLOAD_", 1)
	wire := gemini(map[string]any{"text": "思考 " + token[:12], "thought": true}) +
		gemini(map[string]any{"text": "答复 " + token[:15]}) +
		gemini(map[string]any{"text": token[12:], "thought": true, "thoughtSignature": signature}, map[string]any{"text": token[15:]}) +
		signedPart +
		gemini(map[string]any{"text": "前段 " + token[:7], "thought": true, "thoughtSignature": signature}) +
		gemini(map[string]any{"text": "", "thought": true, "thoughtSignature": token}) +
		gemini(map[string]any{"text": token[7:] + " and " + token, "thought": true, "thought_signature": signature}) +
		"data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}\n\n"
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, wire, 7)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 8 {
		t.Fatalf("events = %d, want 8", len(events))
	}
	if got := events[0].Get("candidates.0.content.parts.0.text").String() + events[2].Get("candidates.0.content.parts.0.text").String(); got != "思考 "+token {
		t.Fatalf("thought = %q", got)
	}
	if got := events[1].Get("candidates.0.content.parts.0.text").String() + events[2].Get("candidates.0.content.parts.1.text").String(); got != "答复 "+secret {
		t.Fatalf("answer = %q", got)
	}
	if got := events[5].Get("candidates.0.content.parts.0"); got.Get("text").String() != "" || got.Get("thoughtSignature").String() != token {
		t.Fatalf("signature-only part changed: %s", got.Raw)
	}
	if got := events[2].Get("candidates.0.content.parts.0"); got.Get("text").String() != token[12:] || got.Get("thoughtSignature").String() != signature {
		t.Fatalf("mixed-signature continuation changed: %s", got.Raw)
	}
	for _, signed := range []struct {
		event                      int
		key, visible, upstreamText string
	}{
		{3, "thought_signature", "签名 " + secret, "签名 " + token},
		{4, "thoughtSignature", "前段 " + secret, "前段 " + token[:7]},
		{6, "thought_signature", " and " + secret, token[7:] + " and " + token},
	} {
		part := events[signed.event].Get("candidates.0.content.parts.0")
		if got := part.Get("text").String(); got != signed.visible {
			t.Fatalf("event %d signed thought = %q, want %q", signed.event, got, signed.visible)
		}
		originalSignature, originalText, err := privacy.UnwrapReasoningSignature(part.Get(signed.key).String(), part.Get("text").String())
		if err != nil || originalSignature != signature || originalText != signed.upstreamText {
			t.Fatalf("event %d signed thought replay = (%q, %q, %v)", signed.event, originalSignature, originalText, err)
		}
	}
	if restored, unrestored := writer.observer.RestoredTokens(), writer.observer.UnrestoredTokens(); restored != 4 || unrestored != 1 {
		t.Fatalf("token observations = (%d, %d), want (4, 1)", restored, unrestored)
	}
}

/**
 * @brief 验证 Gemini 混合签名占位符精确保留各片段，同一 part 的独立值仍可还原。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterGeminiMixedSignatureReplay(t *testing.T) {
	session := privacyOutputTestSession(t)
	const mixedSecret = "mixed fragment secret"
	const independentSecret = "independent unsigned secret"
	token := privacyOutputTestToken(t, session, mixedSecret)
	independentToken := privacyOutputTestToken(t, session, independentSecret)
	prefix, suffix := token[:13], token[13:]
	const signature = "YQ=="
	unsigned := map[string]any{"thought": true, "text": independentToken + " " + prefix}
	signed := map[string]any{"thought": true, "text": suffix + " " + token, "thoughtSignature": signature}
	gemini := func(parts ...map[string]any) string {
		return privacyOutputTestJSON(t, map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"parts": parts}}},
		})
	}
	for _, format := range []string{"object", "sse", "vertex_array"} {
		t.Run(format, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			wire := gemini(unsigned, signed)
			contentType := "application/json"
			switch format {
			case "sse":
				contentType = "text/event-stream"
				wire = "data: " + gemini(unsigned) + "\n\ndata: " + gemini(signed) + "\n\n"
			case "vertex_array":
				wire = "[" + gemini(unsigned) + "," + gemini(signed) + "]"
			}
			writer.Header().Set("Content-Type", contentType)
			privacyOutputTestWrite(t, writer, wire, 11)
			if err := writer.Finish(); err != nil {
				t.Fatal(err)
			}
			var events []gjson.Result
			switch format {
			case "sse":
				events = privacyOutputTestEvents(t, recorder.Body.String())
			case "vertex_array":
				events = gjson.Parse(recorder.Body.String()).Array()
			default:
				events = []gjson.Result{gjson.Parse(recorder.Body.String())}
			}
			var output []gjson.Result
			for _, event := range events {
				output = append(output, event.Get("candidates.0.content.parts").Array()...)
			}
			if len(output) != 2 || output[0].Get("text").String() != independentSecret+" "+prefix || output[1].Get("text").String() != suffix+" "+mixedSecret {
				t.Fatalf("unexpected visible parts: %v", output)
			}
			if restored, unrestored := writer.observer.RestoredTokens(), writer.observer.UnrestoredTokens(); restored != 2 || unrestored != 1 {
				t.Fatalf("token observations = (%d, %d), want (2, 1)", restored, unrestored)
			}
			replay := []byte(`{"contents":[{"role":"model","parts":[` + output[0].Raw + "," + output[1].Raw + `]}]}`)
			for _, check := range []struct {
				name        string
				session     *privacy.Session
				independent string
			}{
				{"enabled", session, independentToken},
				{"disabled", nil, independentSecret},
			} {
				got, err := maskPrivacyBody(check.session, replay, "application/json")
				if err != nil {
					t.Fatalf("%s replay: %v", check.name, err)
				}
				parts := gjson.GetBytes(got, "contents.0.parts").Array()
				if len(parts) != 2 || parts[0].Get("text").String() != check.independent+" "+prefix ||
					parts[1].Get("text").String() != suffix+" "+token || parts[1].Get("thoughtSignature").String() != signature {
					t.Fatalf("%s replay parts = %v", check.name, parts)
				}
			}
		})
	}
}

/**
 * @brief 验证 Vertex JSON 分块数组跨块还原签名思考和回答，并保留候选索引、用量和终态。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterVertexJSONChunks(t *testing.T) {
	session := privacyOutputTestSession(t)
	const secret = "signed Vertex thought"
	token := privacyOutputTestToken(t, session, secret)
	chunk := func(finishReason string, part ...map[string]any) string {
		return privacyOutputTestJSON(t, map[string]any{
			"candidates": []any{map[string]any{"index": 3, "finishReason": finishReason, "content": map[string]any{"parts": part}}},
		})
	}
	wire := " \n[\n" + chunk("",
		map[string]any{"thought": true, "text": "思考 " + token[:11], "thoughtSignature": "YQ=="},
		map[string]any{"text": "回答 " + token[:17]},
	) + ",\n" + `{"usageMetadata":{"promptTokenCount":123}}` + ",\n" + chunk("STOP",
		map[string]any{"thought": true, "text": token[11:], "thoughtSignature": "Yg=="},
		map[string]any{"text": token[17:] + " " + token[:12]},
	) + "\n]\n"

	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "application/json")
	privacyOutputTestWrite(t, writer, wire, 11)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	out := gjson.Parse(recorder.Body.String())
	if !out.IsArray() || len(out.Array()) != 3 || out.Get("0.candidates.0.index").Int() != 3 || out.Get("1.usageMetadata.promptTokenCount").Int() != 123 ||
		out.Get("2.candidates.0.index").Int() != 3 || out.Get("2.candidates.0.finishReason").String() != "STOP" {
		t.Fatalf("Vertex response metadata changed: %s", out.Raw)
	}
	first := out.Get("0.candidates.0.content.parts.0")
	last := out.Get("2.candidates.0.content.parts.0")
	if first.Get("text").String() != "思考 "+secret || last.Get("text").String() != "" {
		t.Fatalf("visible signed parts = (%s, %s)", first.Raw, last.Raw)
	}
	answer := out.Get("0.candidates.0.content.parts.1.text").String() + out.Get("2.candidates.0.content.parts.1.text").String()
	if answer != "回答 "+secret+" "+token[:12] {
		t.Fatalf("answer = %q", answer)
	}
	if restored, unrestored := writer.observer.RestoredTokens(), writer.observer.UnrestoredTokens(); restored != 2 || unrestored != 1 {
		t.Fatalf("token observations = (%d, %d), want (2, 1)", restored, unrestored)
	}
	replay := []byte(`{"contents":[{"role":"model","parts":[` + first.Raw + "," + last.Raw + `]}]}`)
	got, err := maskPrivacyBody(nil, replay, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(got, "contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("text").String() != "思考 "+token[:11] || parts[0].Get("thoughtSignature").String() != "YQ==" ||
		parts[1].Get("text").String() != token[11:] || parts[1].Get("thoughtSignature").String() != "Yg==" {
		t.Fatalf("replayed signed parts = %v", parts)
	}
}

/**
 * @brief 验证 Chat content 中 thinking part 的占位符能跨事件还原。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterChatThinkingParts(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "chat private thought"
	token := privacyOutputTestToken(t, session, secret)
	chat := func(part map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": []map[string]any{part}},
		}}}) + "\n\n"
	}
	wire := chat(map[string]any{"type": "thinking", "thinking": "思考 " + token[:7]}) +
		chat(map[string]any{"type": "text", "text": "答复 " + token[:15]}) +
		chat(map[string]any{"type": "thinking", "thinking": token[7:], "signature": token}) +
		chat(map[string]any{"type": "text", "text": token[15:]}) + "data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, wire, 7)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	const part = "choices.0.delta.content.0."
	if got := events[0].Get(part+"thinking").String() + events[2].Get(part+"thinking").String(); got != "思考 "+secret {
		t.Fatalf("thinking = %q", got)
	}
	if got := events[1].Get(part+"text").String() + events[3].Get(part+"text").String(); got != "答复 "+secret {
		t.Fatalf("answer = %q", got)
	}
	signature, raw, err := privacy.UnwrapReasoningSignature(events[2].Get(part+"signature").String(), events[2].Get(part+"thinking").String())
	if err != nil || signature != token || raw != token[7:] {
		t.Fatalf("signed thinking replay = (%q, %q, %v)", signature, raw, err)
	}
}

/**
 * @brief 验证 Claude 思考开头和 delta 跨帧还原，并能通过尾签名回放原文。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterAnthropicThinking(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "多字节原值"
	token := privacyOutputTestToken(t, session, secret)
	anthropic := func(event, field string, index int, value map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"type": event, "index": index, field: value}) + "\n\n"
	}
	wire := anthropic("content_block_start", "content_block", 0, map[string]any{"type": "thinking", "thinking": "思考 " + token[:12], "signature": ""}) +
		anthropic("content_block_delta", "delta", 1, map[string]any{"type": "text_delta", "text": "答复 " + token[:15]}) +
		anthropic("content_block_delta", "delta", 0, map[string]any{"type": "thinking_delta", "thinking": token[12:] + " and " + token}) +
		anthropic("content_block_delta", "delta", 1, map[string]any{"type": "text_delta", "text": token[15:]}) +
		anthropic("content_block_delta", "delta", 0, map[string]any{"type": "signature_delta", "signature": token}) +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		anthropic("content_block_start", "content_block", 2, map[string]any{"type": "redacted_thinking", "data": token})
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, wire, 7)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 7 {
		t.Fatalf("events = %d, want 7", len(events))
	}
	thinking := events[0].Get("content_block.thinking").String() + events[2].Get("delta.thinking").String()
	if thinking != "思考 "+secret+" and "+secret {
		t.Fatalf("thinking = %q", thinking)
	}
	if got := events[1].Get("delta.text").String() + events[3].Get("delta.text").String(); got != "答复 "+secret {
		t.Fatalf("answer = %q", got)
	}
	originalSignature, originalText, err := privacy.UnwrapReasoningSignature(events[4].Get("delta.signature").String(), thinking)
	if err != nil || originalSignature != token || originalText != "思考 "+token+" and "+token {
		t.Fatalf("thinking replay = (%q, %q, %v)", originalSignature, originalText, err)
	}
	if got := events[6].Get("content_block.data").String(); got != token {
		t.Fatalf("redacted thinking = %q", got)
	}
}

/**
 * @brief 验证 OpenRouter 按 reasoning detail index 恢复思考正文和摘要，保留加密数据。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterOpenRouterReasoningDetails(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "private reasoning value"
	token := privacyOutputTestToken(t, session, secret)
	chat := func(details ...map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"reasoning_details": details},
		}}}) + "\n\n"
	}
	wire := chat(
		map[string]any{"type": "reasoning.text", "index": 0, "text": "thought " + token[:12]},
		map[string]any{"type": "reasoning.summary", "index": 1, "summary": "summary " + token[:16]},
	) + chat(
		map[string]any{"type": "reasoning.summary", "index": 1, "summary": token[16:]},
		map[string]any{"type": "reasoning.text", "index": 0, "text": token[12:]},
	) + chat(
		map[string]any{"index": 0, "signature": token},
		map[string]any{"type": "reasoning.encrypted", "index": 2, "data": token},
		map[string]any{"type": "reasoning.summary", "index": 1, "signature": token},
	) + "data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, wire, 7)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	const detail = "choices.0.delta.reasoning_details."
	thinking := events[0].Get(detail+"0.text").String() + events[1].Get(detail+"1.text").String()
	if thinking != "thought "+secret {
		t.Fatalf("text = %q", thinking)
	}
	summary := events[0].Get(detail+"1.summary").String() + events[1].Get(detail+"0.summary").String()
	if summary != "summary "+secret {
		t.Fatalf("summary = %q", summary)
	}
	originalSignature, originalText, err := privacy.UnwrapReasoningSignature(events[2].Get(detail+"0.signature").String(), thinking)
	if err != nil || originalSignature != token || originalText != "thought "+token {
		t.Fatalf("reasoning replay = (%q, %q, %v)", originalSignature, originalText, err)
	}
	if got := events[2].Get(detail + "1.data").String(); got != token {
		t.Fatalf("encrypted reasoning = %q", got)
	}
	originalSignature, originalText, err = privacy.UnwrapReasoningSignature(events[2].Get(detail+"2.signature").String(), summary)
	if err != nil || originalSignature != token || originalText != "summary "+token {
		t.Fatalf("summary replay = (%q, %q, %v)", originalSignature, originalText, err)
	}
}

/**
 * @brief 验证 OpenRouter 无索引的交错 detail 依 ID 拼接，且不与索引或位置通道冲突。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterOpenRouterReasoningDetailIDs(t *testing.T) {
	session := privacyOutputTestSession(t)
	const secret = "private reasoning value"
	token := privacyOutputTestToken(t, session, secret)
	chat := func(detail map[string]any, content string, final bool) string {
		detail["type"], detail["text"] = "reasoning.text", content
		if final {
			detail["signature"] = "signature"
		}
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"reasoning_details": []any{detail}},
		}}}) + "\n\n"
	}
	channels := []struct {
		first, final map[string]any
		label        string
		split        int
	}{
		{map[string]any{"id": "0"}, map[string]any{"id": "0"}, "id ", 7},
		{map[string]any{"index": 0, "id": "indexed"}, map[string]any{}, "position ", 12},
		{map[string]any{"index": 1, "id": "linked"}, map[string]any{"id": "linked"}, "linked ", 13},
		{map[string]any{"id": "other"}, map[string]any{"index": 2}, "other ", 15},
	}
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	for _, channel := range channels {
		privacyOutputTestWrite(t, writer, chat(channel.first, channel.label+token[:channel.split], false), 11)
	}
	privacyOutputTestWrite(t, writer, chat(map[string]any{"index": 2, "id": "other"}, "", false), 11)
	for _, channel := range channels {
		privacyOutputTestWrite(t, writer, chat(channel.final, token[channel.split:], true), 11)
	}
	privacyOutputTestWrite(t, writer, "data: [DONE]\n\n", 11)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 2*len(channels)+1 {
		t.Fatalf("events = %d, want %d", len(events), 2*len(channels)+1)
	}
	const path = "choices.0.delta.reasoning_details.0."
	for index, channel := range channels {
		final := events[index+len(channels)+1]
		visible := events[index].Get(path+"text").String() + final.Get(path+"text").String()
		if visible != channel.label+secret {
			t.Fatalf("%s visible = %q", channel.label, visible)
		}
		signature, text, err := privacy.UnwrapReasoningSignature(final.Get(path+"signature").String(), visible)
		if err != nil || signature != "signature" || text != channel.label+token {
			t.Fatalf("%s replay = (%q, %q, %v)", channel.label, signature, text, err)
		}
	}
}

/**
 * @brief 验证签名流在回放编辑数或还原字节数超限的帧上拒绝输出。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterSignedReasoningBudget(t *testing.T) {
	session := privacyOutputTestSession(t)
	large := "private:" + strings.Repeat("x", 1<<20)
	largeToken := privacyOutputTestToken(t, session, large)
	smallToken := privacyOutputTestToken(t, session, "small private value")
	for _, test := range []struct {
		budget string
		text   string
	}{
		{"bytes", strings.Repeat(largeToken, privacy.MaxReasoningTextBytes/len(large)+1)},
		{"edits", strings.Repeat(smallToken, privacy.MaxReasoningEdits+1)},
	} {
		for _, protocol := range []struct {
			name string
			body string
		}{
			{"claude", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%s}}`},
			{"gemini", `{"candidates":[{"content":{"parts":[{"thought":true,"text":%s,"thoughtSignature":"YQ=="}]}}]}`},
		} {
			t.Run(test.budget+"/"+protocol.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				writer := newPrivacyResponseWriter(recorder, session)
				writer.Header().Set("Content-Type", "text/event-stream")
				body := strings.Replace(protocol.body, "%s", privacyOutputTestJSON(t, test.text), 1)
				_, err := writer.Write([]byte("data: " + body + "\n\n"))
				if !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
					t.Fatalf("Write error = %v, want %v", err, privacy.ErrInvalidReasoningSignature)
				}
				if recorder.Body.Len() != 0 {
					t.Fatalf("wrote %d bytes from over-limit frame", recorder.Body.Len())
				}
			})
		}
	}

	half := privacyOutputTestJSON(t, strings.Repeat(largeToken, privacy.MaxReasoningTextBytes/(2*len(large))+1))
	part := `{"thought":true,"text":` + half + `,"thoughtSignature":"YQ=="}`
	gemini := `{"candidates":[{"content":{"parts":[` + part + "," + part + `]}}]}`
	chunk := `{"candidates":[{"content":{"parts":[` + part + `]}}]}`
	openRouter := `{"choices":[{"delta":{"reasoning_details":[` +
		`{"type":"reasoning.text","index":0,"text":` + half + `},` +
		`{"type":"reasoning.text","index":1,"text":` + half + `}]}}]}`
	for _, test := range []struct {
		name, body string
		streaming  bool
	}{
		{"aggregate/gemini_stream", gemini, true},
		{"aggregate/gemini_json", gemini, false},
		{"aggregate/vertex_array", "[" + chunk + "," + chunk + "]", false},
		{"aggregate/openrouter", openRouter, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			body := test.body
			if test.streaming {
				writer.Header().Set("Content-Type", "text/event-stream")
				body = "data: " + body + "\n\n"
			} else {
				writer.Header().Set("Content-Type", "application/json")
			}
			_, err := writer.Write([]byte(body))
			if !test.streaming && err == nil {
				err = writer.Finish()
			}
			if !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
				t.Fatalf("output error = %v, want %v", err, privacy.ErrInvalidReasoningSignature)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("wrote %d bytes from over-limit frame", recorder.Body.Len())
			}
		})
	}
}

func TestPrivacyResponseWriterChannelSwitchKeepsUnmatchedPrefix(t *testing.T) {
	chat := func(delta map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}) + "\n\n"
	}
	gemini := func(part map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": []any{part}}}}}) + "\n\n"
	}
	for name, wire := range map[string]string{
		"chat reasoning to content": chat(map[string]any{"reasoning_content": "write func main() {"}) + chat(map[string]any{"content": "Here it is"}),
		"chat content to tool":      chat(map[string]any{"content": "calling {"}) + chat(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `{"q":1}`}}}}),
		"gemini text to call":       gemini(map[string]any{"text": "calling {"}) + gemini(map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{}}}),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, privacyOutputTestSession(t))
			writer.Header().Set("Content-Type", "text/event-stream")
			privacyOutputTestWrite(t, writer, wire, 9)
			// The trailing "{" could still start a placeholder; once the stream
			// ends without one, every event leaves unchanged and in order.
			if err := writer.Finish(); err != nil || recorder.Body.String() != wire {
				t.Fatalf("Finish output = %q, %v", recorder.Body.String(), err)
			}
		})
	}
}

// Clients concatenate a choice's text around tool calls, so a held prefix that
// is already a placeholder must survive the switch and join its continuation.
func TestPrivacyResponseWriterPlaceholderResumesAfterToolCall(t *testing.T) {
	const secret = "owner@example.com"
	chat := func(t *testing.T, delta map[string]any, finish any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}) + "\n\n"
	}
	gemini := func(t *testing.T, part map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": []any{part}}}}}) + "\n\n"
	}
	for _, test := range []struct {
		name  string
		wire  func(t *testing.T, head, rest string) string
		text  func(events []gjson.Result) string
		calls string
	}{
		{
			name: "chat",
			wire: func(t *testing.T, head, rest string) string {
				return chat(t, map[string]any{"content": "mail " + head}, nil) +
					chat(t, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `{"q":1}`}}}}, nil) +
					chat(t, map[string]any{"content": rest}, nil) + chat(t, map[string]any{}, "stop") + "data: [DONE]\n\n"
			},
			text: func(events []gjson.Result) string {
				var text strings.Builder
				for _, event := range events {
					text.WriteString(event.Get("choices.0.delta.content").String())
				}
				return text.String()
			},
			calls: "choices.0.delta.tool_calls",
		},
		{
			name: "gemini",
			wire: func(t *testing.T, head, rest string) string {
				return gemini(t, map[string]any{"text": "mail " + head}) +
					gemini(t, map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{}}}) +
					gemini(t, map[string]any{"text": rest})
			},
			text: func(events []gjson.Result) string {
				var text strings.Builder
				for _, event := range events {
					text.WriteString(event.Get("candidates.0.content.parts.0.text").String())
				}
				return text.String()
			},
			calls: "candidates.0.content.parts.0.functionCall",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := privacyOutputTestSession(t)
			token := privacyOutputTestToken(t, session, secret)
			// Every cut matters: a short head such as "{" or "{{CC" is as much
			// a placeholder prefix as one already reading "{{CCLOAD_".
			for cut := 1; cut < len(token); cut++ {
				recorder := httptest.NewRecorder()
				writer := newPrivacyResponseWriter(recorder, session)
				writer.Header().Set("Content-Type", "text/event-stream")
				privacyOutputTestWrite(t, writer, test.wire(t, token[:cut], token[cut:]), 7)
				if err := writer.Finish(); err != nil {
					t.Fatal(err)
				}
				events := privacyOutputTestEvents(t, recorder.Body.String())
				if got := test.text(events); got != "mail "+secret {
					t.Fatalf("cut %d: client text = %q", cut, got)
				}
				if !slices.ContainsFunc(events, func(event gjson.Result) bool { return event.Get(test.calls).Exists() }) {
					t.Fatalf("cut %d: tool call lost: %q", cut, recorder.Body.String())
				}
			}
		})
	}
}

func TestPrivacyResponseWriterRestoresPlainTextBody(t *testing.T) {
	session := privacyOutputTestSession(t)
	token := privacyOutputTestToken(t, session, "owner@example.com")
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusBadGateway)
	const unknown = "{{CCLOAD_EMAIL_0123456789abcdef}}"
	privacyOutputTestWrite(t, writer, "upstream rejected "+token+" "+unknown, 4)
	if err := writer.Finish(); err != nil || recorder.Body.String() != "upstream rejected owner@example.com "+unknown {
		t.Fatalf("plain-text body = %q, %v", recorder.Body.String(), err)
	}
	if got := writer.observer.UnrestoredTokens(); got != 1 {
		t.Fatalf("UnrestoredTokens = %d, want 1", got)
	}
}

func TestPrivacyResponseWriterModifiedMultilineData(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "owner@example.com"
	token := privacyOutputTestToken(t, session, secret)
	wire := "id: preserved\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\r\ndata: \"output_index\":0,\"content_index\":0,\"delta\":" + privacyOutputTestJSON(t, token) + "}\r\nretry: 100\r\n\r\n"
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, wire, 3)
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := privacyOutputTestEvents(t, recorder.Body.String())
	if len(events) != 1 || events[0].Get("delta").String() != secret || !strings.HasPrefix(recorder.Body.String(), "id: preserved\r\nevent: response.output_text.delta\r\ndata: ") || !strings.HasSuffix(recorder.Body.String(), "\r\nretry: 100\r\n\r\n") {
		t.Fatalf("modified multiline event lost content/metadata: %q", recorder.Body.String())
	}
}

type privacyOutputDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

func (w *privacyOutputDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.err
}

func TestPrivacyResponseWriterController(t *testing.T) {
	target := &privacyOutputDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := newPrivacyResponseWriter(target, privacyOutputTestSession(t))
	controller := http.NewResponseController(writer)
	deadline := time.Now().Add(time.Second)
	if err := controller.SetWriteDeadline(deadline); err != nil || !target.deadline.Equal(deadline) || writer.Unwrap() != target {
		t.Fatalf("deadline/unwrap contract failed: %v", err)
	}
	if err := controller.Flush(); err != nil || target.Flushed {
		t.Fatal("Flush committed an empty response")
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	privacyOutputTestWrite(t, writer, ": heartbeat\n\n", 100)
	if err := controller.Flush(); err != nil || !target.Flushed {
		t.Fatal("Flush did not reach the downstream after output")
	}
	target.err = errors.New("deadline failure")
	if err := controller.SetWriteDeadline(deadline); !errors.Is(err, target.err) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestPrivacyResponseWriterExplicitHeaderFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, privacyOutputTestSession(t))
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Content-Length", "10")
	writer.WriteHeader(http.StatusAccepted)
	writer.Flush()
	if recorder.Code != http.StatusAccepted || !recorder.Flushed || recorder.Header().Get("Content-Length") != "" {
		t.Fatal("explicit header/flush boundary or Content-Length changed")
	}
}

/**
 * @brief 验证响应统计包含可见思考里的未还原占位符，跳过协议标识。
 * @param t 测试句柄。
 */
func TestPrivacyResponseWriterUnrestoredJSON(t *testing.T) {
	session := privacyOutputTestSession(t)
	known := privacyOutputTestToken(t, session, "owner@example.com")
	unknown := "{{CCLOAD_EMAIL_0123456789abcdef}}"
	damaged, truncated := "{{CCLOAD_EMAIL_bad!}}", "{{CCLOAD_EMAIL_0123"
	body := privacyOutputTestJSON(t, map[string]any{
		"id": unknown,
		"content": []any{
			map[string]any{"type": "text", "text": known + " " + unknown + " " + unknown + " " + damaged + " " + truncated},
			map[string]any{"type": "thinking", "thinking": unknown},
		},
		"arguments": privacyOutputTestJSON(t, map[string]string{"value": unknown}),
	})
	// Restoring internal replay/history must not affect client output counts.
	for range 3 {
		if _, err := session.RestoreJSON([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	writer := newPrivacyResponseWriter(recorder, session)
	privacyOutputTestWrite(t, writer, body, 5)
	for range 2 {
		if err := writer.Finish(); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.observer.UnrestoredTokens(); got != 6 {
		t.Fatalf("UnrestoredTokens = %d, want 6", got)
	}
	result := gjson.ParseBytes(recorder.Body.Bytes())
	if got := result.Get("content.0.text").String(); got != "owner@example.com "+unknown+" "+unknown+" "+damaged+" "+truncated {
		t.Fatalf("unexpected text: %q", got)
	}
	if result.Get("id").String() != unknown || result.Get("content.1.thinking").String() != unknown || gjson.Get(result.Get("arguments").String(), "value").String() != unknown {
		t.Fatalf("opaque or unresolved content changed: %s", recorder.Body.String())
	}
}

func TestPrivacyResponseWriterUnrestoredStream(t *testing.T) {
	for _, text := range []string{
		"{{CCLOAD_EMAIL_0123456789abcdef}}",
		"{{CCLOAD_EMAIL_bad!}}",
		"{{CCLOAD_EMAIL_0123",
	} {
		for split := 1; split < len(text); split++ {
			t.Run(text+"/"+strconv.Itoa(split), func(t *testing.T) {
				session := privacyOutputTestSession(t)
				recorder := httptest.NewRecorder()
				writer := newPrivacyResponseWriter(recorder, session)
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, delta := range []string{text[:split], text[split:]} {
					wire := "data: " + privacyOutputTestJSON(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": delta}}}}) + "\n\n"
					privacyOutputTestWrite(t, writer, wire, 3)
				}
				for range 2 {
					if err := writer.Finish(); err != nil {
						t.Fatal(err)
					}
				}
				var emitted strings.Builder
				for _, event := range privacyOutputTestEvents(t, recorder.Body.String()) {
					emitted.WriteString(event.Get("choices.0.delta.content").String())
				}
				if emitted.String() != text || writer.observer.UnrestoredTokens() != 1 {
					t.Fatalf("output=%q count=%d, want %q count=1", emitted.String(), writer.observer.UnrestoredTokens(), text)
				}
			})
		}
	}
}

func TestPrivacyResponseWriterUnrestoredTerminalAndOtherFields(t *testing.T) {
	const unknown = "{{CCLOAD_EMAIL_0123456789abcdef}}"
	for _, terminal := range []string{"", "data: [DONE]\n\n", "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"} {
		t.Run(terminal, func(t *testing.T) {
			writer := newPrivacyResponseWriter(httptest.NewRecorder(), privacyOutputTestSession(t))
			writer.Header().Set("Content-Type", "text/event-stream")
			wire := "data: " + privacyOutputTestJSON(t, map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "{{CCLOAD_EMAIL_"}}},
				"error":   map[string]string{"message": unknown},
			}) + "\n\n" + terminal
			privacyOutputTestWrite(t, writer, wire, 4)
			for range 2 {
				if err := writer.Finish(); err != nil {
					t.Fatal(err)
				}
			}
			if got := writer.observer.UnrestoredTokens(); got != 2 {
				t.Fatalf("UnrestoredTokens = %d, want 2", got)
			}
		})
	}
}

type privacyOutputFailedWriter struct {
	http.ResponseWriter
}

func (w privacyOutputFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPrivacyResponseWriterCountsObservedFailedOutput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(strconv.FormatBool(streaming), func(t *testing.T) {
			writer := newPrivacyResponseWriter(privacyOutputFailedWriter{httptest.NewRecorder()}, privacyOutputTestSession(t))
			wire := `{"content":"{{CCLOAD_EMAIL_0123456789abcdef}}"}`
			if streaming {
				writer.Header().Set("Content-Type", "text/event-stream")
				wire = "data: " + wire + "\n\n"
			}
			_, _ = writer.Write([]byte(wire))
			for range 2 {
				if err := writer.Finish(); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("Finish = %v, want closed pipe", err)
				}
			}
			if got := writer.observer.UnrestoredTokens(); got != 1 {
				t.Fatalf("UnrestoredTokens = %d, want 1 for observed output", got)
			}
		})
	}
}
