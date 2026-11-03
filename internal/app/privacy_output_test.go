package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestPrivacyResponseWriterReasoningKeepsPlaceholders(t *testing.T) {
	session := privacyOutputTestSession(t)
	secret := "owner@example.com"
	token := privacyOutputTestToken(t, session, secret)
	gemini := func(parts ...map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{
			"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": parts}}},
		}) + "\n\n"
	}
	anthropic := func(index int, delta map[string]any) string {
		return "data: " + privacyOutputTestJSON(t, map[string]any{"type": "content_block_delta", "index": index, "delta": delta}) + "\n\n"
	}
	tests := []struct {
		name, reasoning, answer string
		answerText              func(events []gjson.Result) string
	}{
		{
			// The answer restarts at part zero after the thought, and a
			// placeholder may continue in the candidate's next part.
			name:      "gemini",
			reasoning: gemini(map[string]any{"text": "thinking " + token, "thought": true}) + gemini(map[string]any{"text": token[:15], "thought": true}),
			answer: gemini(map[string]any{"text": "mail "}, map[string]any{"text": token[:12]}) + gemini(map[string]any{"text": token[12:]}) +
				"data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}\n\n",
			answerText: func(events []gjson.Result) string {
				var text strings.Builder
				for _, event := range events[2:] {
					for _, part := range event.Get("candidates.0.content.parts").Array() {
						text.WriteString(part.Get("text").String())
					}
				}
				return text.String()
			},
		},
		{
			name: "anthropic",
			reasoning: anthropic(0, map[string]any{"type": "thinking_delta", "thinking": "thinking " + token}) +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			answer: anthropic(1, map[string]any{"type": "text_delta", "text": "mail " + token[:12]}) + anthropic(1, map[string]any{"type": "text_delta", "text": token[12:]}) +
				"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			answerText: func(events []gjson.Result) string {
				return events[2].Get("delta.text").String() + events[3].Get("delta.text").String()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newPrivacyResponseWriter(recorder, session)
			writer.Header().Set("Content-Type", "text/event-stream")
			privacyOutputTestWrite(t, writer, test.reasoning+test.answer, 7)
			if err := writer.Finish(); err != nil {
				t.Fatal(err)
			}
			// Clients replay reasoning to the vendor that signed it.
			if !strings.HasPrefix(recorder.Body.String(), test.reasoning) {
				t.Fatalf("reasoning changed: %q", recorder.Body.String())
			}
			if got := test.answerText(privacyOutputTestEvents(t, recorder.Body.String())); got != "mail "+secret {
				t.Fatalf("answer = %q", got)
			}
		})
	}
}

func TestPrivacyResponseWriterChannelSwitchReleasesHeldText(t *testing.T) {
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
			// The trailing "{" could start a placeholder, but the model moved
			// on; the rest of the stream must not wait for the finish event.
			if recorder.Body.String() != wire {
				t.Fatalf("stream held after channel switch: %q", recorder.Body.String())
			}
			if err := writer.Finish(); err != nil || recorder.Body.String() != wire {
				t.Fatalf("Finish changed output: %q, %v", recorder.Body.String(), err)
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
	if got := writer.observer.UnrestoredTokens(); got != 5 {
		t.Fatalf("UnrestoredTokens = %d, want 5", got)
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
