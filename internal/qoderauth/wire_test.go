package qoderauth

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestBuildChatRequestNativeWire(t *testing.T) {
	credential := &Credential{UserID: "u1", AccessToken: "dt-secret", MachineID: "machine"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		assertCOSYRequest(t, request, body)
		if request.Method != http.MethodPost || request.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || request.URL.Query().Get("Encode") != "1" || request.URL.Query().Get("FetchKeys") != "llm_model_result" {
			t.Errorf("unexpected target: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("X-Model-Key") != "routing-key" || request.Header.Get("X-Model-Source") != "byok" || request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("missing native routing headers: %v", request.Header)
		}
		decoded := decodeNativeRequest(t, body)
		var envelope struct {
			Stream      bool             `json:"stream"`
			Messages    []map[string]any `json:"messages"`
			Tools       []map[string]any `json:"tools"`
			Parameters  map[string]any   `json:"parameters"`
			ModelConfig map[string]any   `json:"model_config"`
			Context     struct {
				Text string `json:"text"`
			} `json:"chat_context"`
		}
		if err := json.Unmarshal(decoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if !envelope.Stream || len(envelope.Messages) != 5 || envelope.Messages[0]["role"] != "system" || envelope.Messages[0]["content"] != "policy" || envelope.Messages[1]["role"] != "system" {
			t.Errorf("messages/system lost: %s", decoded)
		}
		if envelope.Context.Text != "看图片" || len(envelope.Messages[2]["content"].([]any)) != 2 || len(envelope.Messages[3]["tool_calls"].([]any)) != 1 || envelope.Messages[3]["content"] != " " || envelope.Messages[4]["tool_call_id"] != "call1" || len(envelope.Tools) != 1 {
			t.Errorf("multimodal/tool data lost: %s", decoded)
		}
		if envelope.Parameters["max_tokens"] != float64(32768) || envelope.Parameters["enable_thinking"] != true || envelope.Parameters["reasoning_effort"] != "xhigh" || envelope.Parameters["temperature"] != 0.4 {
			t.Errorf("generation parameters lost: %v", envelope.Parameters)
		}
		if envelope.ModelConfig["key"] != "routing-key" || envelope.ModelConfig["thinking_config"] != nil || envelope.ModelConfig["source"] != "byok" {
			t.Errorf("bad model configuration: %v", envelope.ModelConfig)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	service := &Service{Client: server.Client(), BaseURL: server.URL}
	body := []byte(`{"model":"Public Name","stream":false,"messages":[{"role":"system","content":"policy"},{"role":"user","content":[{"type":"text","text":"看图片"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},{"role":"tool","tool_call_id":"call1","content":"result"},{"role":"developer","content":"policy2"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"max_tokens":50000,"reasoning_effort":"high","temperature":0.4}`)
	model := Model{ID: "PublicName", Key: "routing-key", Config: json.RawMessage(`{"source":"byok","is_reasoning":true,"thinking_config":{"enabled":true}}`)}
	request, err := service.BuildChatRequest(context.Background(), credential, body, model)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertCOSYRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer COSY."), ".")
	if len(parts) != 2 {
		t.Fatalf("not COSY authorization: %q", req.Header.Get("Authorization"))
	}
	path := strings.TrimPrefix(req.URL.Path, "/algo")
	signature := md5.Sum([]byte(parts[0] + "\n" + req.Header.Get("Cosy-Key") + "\n" + req.Header.Get("Cosy-Date") + "\n" + string(body) + "\n" + path))
	if parts[1] != hex.EncodeToString(signature[:]) || req.Header.Get("Cosy-Sigpath") != path {
		t.Error("COSY signature does not cover the actual HTTP request")
	}
	bodyHash := md5.Sum(body)
	if req.Header.Get("Cosy-Bodyhash") != hex.EncodeToString(bodyHash[:]) || req.Header.Get("Cosy-Bodylength") != strconv.Itoa(len(body)) {
		t.Error("incorrect encoded body digest/length")
	}
	key, err := base64.StdEncoding.DecodeString(req.Header.Get("Cosy-Key"))
	if err != nil || len(key) != 128 {
		t.Errorf("invalid RSA-wrapped key: %d bytes, %v", len(key), err)
	}
	payload, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Info      string `json:"info"`
		RequestID string `json:"requestId"`
		Version   string `json:"version"`
	}
	if err := json.Unmarshal(payload, &identity); err != nil || identity.RequestID == "" || identity.Version != "v1" {
		t.Errorf("invalid COSY payload: %s (%v)", payload, err)
	}
	info, err := base64.StdEncoding.DecodeString(identity.Info)
	if err != nil || len(info) == 0 || len(info)%16 != 0 || bytes.Contains(payload, []byte("dt-secret")) {
		t.Errorf("identity is not AES-CBC ciphertext: %s (%v)", payload, err)
	}
}

func decodeNativeRequest(t *testing.T, encoded []byte) []byte {
	t.Helper()
	const alphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	mapped := make([]byte, len(encoded))
	for i, b := range encoded {
		if b == '$' {
			mapped[i] = '='
		} else if index := strings.IndexByte(alphabet, b); index >= 0 {
			mapped[i] = standard[index]
		} else {
			t.Fatalf("invalid native encoding byte: %q", b)
		}
	}
	n := len(mapped) / 3
	standardBody := append(bytes.Clone(mapped[len(mapped)-n:]), mapped[n:len(mapped)-n]...)
	standardBody = append(standardBody, mapped[:n]...)
	decoded, err := base64.StdEncoding.DecodeString(string(standardBody))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func nativeEvent(value any) string {
	body, _ := json.Marshal(value)
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(body)})
	return "data: " + string(envelope) + "\n\n"
}

func nativeDelta(content string, finish any) string {
	return nativeEvent(map[string]any{"id": "chat1", "model": "native", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": finish}}})
}

func readNormalized(t *testing.T, text string) ([]map[string]any, bool) {
	t.Helper()
	source := &stepCloser{reader: strings.NewReader(text)}
	stream := NormalizeStream(source)
	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil || !source.closed {
		t.Fatalf("response was not closed: %v", err)
	}
	var chunks []map[string]any
	done := false
	for _, event := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		if event == "data: [DONE]" {
			done = true
			continue
		}
		if !strings.HasPrefix(event, "data: ") {
			t.Fatalf("invalid SSE event %q", event)
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &chunk); err != nil {
			t.Fatalf("invalid output JSON: %s (%v)", event, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, done
}

type stepCloser struct {
	reader *strings.Reader
	closed bool
}

func (r *stepCloser) Read(p []byte) (int, error) { return r.reader.Read(p[:min(len(p), 1)]) }
func (r *stepCloser) Close() error               { r.closed = true; return nil }

func TestNormalizeStreamPreservesReasoningToolsUsageAndUTF8(t *testing.T) {
	tools := []any{map[string]any{"index": 0, "id": "tool1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":"}}}
	input := ": heartbeat\r\n\r\n" + nativeDelta("之前<thi", nil) + nativeDelta("nking>推理</think", nil) + nativeDelta("ing>之后", nil)
	input += nativeEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": tools, "reasoning_content": "native reasoning"}, "finish_reason": nil}}})
	input += nativeEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, "finish_reason": "tool_calls"}}})
	input += nativeEvent(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 7, "total_tokens": 17, "prompt_tokens_details": map[string]any{"cached_tokens": 3}}})
	input += "data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"
	chunks, done := readNormalized(t, input)
	if !done {
		t.Fatal("missing completion marker")
	}
	var content, reasoning, arguments string
	var usage map[string]any
	for _, chunk := range chunks {
		if chunk["error"] != nil {
			t.Fatalf("unexpected error: %v", chunk)
		}
		if value, ok := chunk["usage"].(map[string]any); ok {
			usage = value
		}
		for _, raw := range chunk["choices"].([]any) {
			choice := raw.(map[string]any)
			delta := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				content += text
			}
			if text, ok := delta["reasoning_content"].(string); ok {
				reasoning += text
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, call := range calls {
					if text, ok := call.(map[string]any)["function"].(map[string]any)["arguments"].(string); ok {
						arguments += text
					}
				}
			}
		}
	}
	if content != "之前之后" || reasoning != "推理native reasoning" || arguments != `{"q":1}` || usage["total_tokens"] != float64(17) || usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(3) {
		t.Fatalf("lost data: content=%q reasoning=%q tools=%q usage=%v", content, reasoning, arguments, usage)
	}
}

func TestNormalizeStreamTerminalAndErrors(t *testing.T) {
	cases := []struct {
		name, input string
		status      int
		done        bool
	}{
		{"finished EOF", nativeDelta("ok", "stop"), 0, true},
		{"final line without newline", strings.TrimRight(nativeDelta("ok", "stop"), "\n"), 0, true},
		{"empty EOF", "", 502, false},
		{"unfinished EOF", nativeDelta("partial", nil), 502, false},
		{"malformed JSON", "data: {broken}\n\n", 502, false},
		{"truncated JSON", "data: {\"statusCodeValue\":200,\"body\":", 502, false},
		{"invalid UTF8", "data: {\"body\":\"\xff\"}\n\n", 502, false},
		{"wrong delta shape", nativeEvent(map[string]any{"choices": []any{map[string]any{"delta": "bad"}}}), 502, false},
		{"auth", `data: {"statusCodeValue":403,"body":"{\"code\":105,\"message\":\"expired\"}"}` + "\n\n", 401, false},
		{"quota", `data: {"statusCodeValue":403,"body":{"code":112,"message":"credits exhausted"}}` + "\n\n", 429, false},
		{"queue", `data: {"statusCodeValue":403,"body":{"code":10605,"message":"queued","credit":0}}` + "\n\n", 503, false},
		{"nested business error", nativeEvent(map[string]any{"code": 105, "message": "expired"}), 401, false},
		{"malformed after finish", nativeDelta("ok", "stop") + "data: broken\n\n", 502, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, done := readNormalized(t, tc.input)
			if done != tc.done {
				t.Fatalf("DONE=%v, want %v; chunks=%v", done, tc.done, chunks)
			}
			if tc.status != 0 {
				last := chunks[len(chunks)-1]
				err, ok := last["error"].(map[string]any)
				if !ok || last["status"] != float64(tc.status) || err["status"] != float64(tc.status) || err["type"] == "" {
					t.Fatalf("bad error: %v", last)
				}
			}
		})
	}
}

func TestNormalizeStreamMultilineSSE(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\n" + "data: \"body\":{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}}\n\n"
	chunks, done := readNormalized(t, input)
	if !done || len(chunks) != 1 || chunks[0]["error"] != nil {
		t.Fatalf("bad multiline result: %v DONE %v", chunks, done)
	}
}

type qoderFailureReader struct{ err error }

func (r qoderFailureReader) Read([]byte) (int, error) { return 0, r.err }

func TestNormalizeStreamPreservesCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		stream := NormalizeStream(io.NopCloser(qoderFailureReader{err: cause}))
		data, err := io.ReadAll(stream)
		if !errors.Is(err, cause) || len(data) != 0 {
			t.Fatalf("cancellation became a business error: %q %v", data, err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildChatRequestRejectsInvalidInput(t *testing.T) {
	s := &Service{}
	for _, body := range []string{`not-json`, `null`, `{}`, `{"messages":{}}`, `{"messages":[]}`, `{"messages":[{"role":"invalid","content":"x"}]}`, `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"YQ==","format":"wav"}}]}]}`, `{"messages":[{"role":"assistant","tool_calls":[{"type":"custom"}]}]}`, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"custom"}]}`} {
		if _, err := s.BuildChatRequest(context.Background(), &Credential{Region: "global", UserID: "u", AccessToken: "token", MachineID: "m"}, []byte(body), Model{Key: "key"}); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}
