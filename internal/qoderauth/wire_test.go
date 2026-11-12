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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

/**
 * @brief TestBuildChatRequestNativeWire verifies the signed desktop request contract.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
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
		if !envelope.Stream || len(envelope.Messages) != 4 || envelope.Messages[0]["role"] != "system" || envelope.Messages[0]["content"].([]any)[0].(map[string]any)["text"] != "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user.\n\npolicy\n\npolicy2" {
			t.Errorf("messages/system lost: %s", decoded)
		}
		if envelope.Context.Text != "看图片" || len(envelope.Messages[1]["content"].([]any)) != 2 || len(envelope.Messages[2]["tool_calls"].([]any)) != 1 || envelope.Messages[2]["content"] != "" || envelope.Messages[3]["tool_call_id"] != "call1" || len(envelope.Tools) != 1 {
			t.Errorf("multimodal/tool data lost: %s", decoded)
		}
		if envelope.Parameters["max_tokens"] != float64(48000) || envelope.Parameters["enable_thinking"] != true || envelope.Parameters["reasoning_effort"] != "xhigh" || envelope.Parameters["temperature"] != 0.4 {
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
	model := Model{ID: "PublicName", Key: "routing-key", Config: json.RawMessage(`{"source":"byok","is_reasoning":true,"max_output_tokens":48000,"thinking_config":{"enabled":{"efforts":{"low":{},"medium":{"is_default":true},"xhigh":{}}}}}`)}
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

/**
 * @brief TestBuildChatRequestReasoningEfforts verifies catalog defaults and nearest levels.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestReasoningEfforts(t *testing.T) {
	for _, tc := range []struct {
		name, effort, want string
		allowed            []string
	}{
		{"declared high", "high", "high", []string{"low", "medium", "high"}},
		{"high nearest", "high", "xhigh", []string{"low", "medium", "xhigh"}},
		{"max nearest", "max", "xhigh", []string{"low", "medium", "xhigh"}},
		{"unknown effort", "unsupported", "medium", []string{"low", "medium", "xhigh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}, "reasoning_effort": tc.effort})
			if err != nil {
				t.Fatal(err)
			}
			efforts := map[string]any{}
			for _, level := range tc.allowed {
				efforts[level] = map[string]bool{"is_default": level == "medium"}
			}
			config, _ := json.Marshal(map[string]any{"thinking_config": map[string]any{"enabled": map[string]any{"efforts": efforts}}})
			request, err := (&Service{}).BuildChatRequest(context.Background(), &Credential{Region: "global", UserID: "u", AccessToken: "token", MachineID: "m"}, body, Model{Key: "key", Config: config})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := request.Body.Close(); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Parameters map[string]any `json:"parameters"`
			}
			if err := json.Unmarshal(decodeNativeRequest(t, encoded), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Parameters["reasoning_effort"] != tc.want {
				t.Fatalf("reasoning effort = %v, want %s", envelope.Parameters["reasoning_effort"], tc.want)
			}
		})
	}
}

/**
 * @brief TestBuildChatRequestContextTiers verifies context selection and native overrides.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestContextTiers(t *testing.T) {
	for _, tc := range []struct {
		name, config, text string
		tools              json.RawMessage
		want               int
	}{
		{"numeric default", `{"max_input_tokens":100,"context_config":{"small":{"token_count":100},"large":{"token_count":400,"is_default":1}}}`, "hello", nil, 400},
		{"string default", `{"max_input_tokens":100,"context_config":{"small":{"token_count":100},"large":{"token_count":400,"is_default":"1"}}}`, "hello", nil, 400},
		{"alternate window fields", `{"max_input_tokens":100,"availableContextWindows":[100,400],"defaultContextWindow":400}`, "hello", nil, 400},
		{"default fits", `{"max_input_tokens":100,"context_config":{"small":{"token_count":100},"large":{"token_count":400}}}`, "hello", nil, 100},
		{"map tiers", `{"max_input_tokens":100,"context_config":{"small":{"token_count":100},"medium":{"token_count":200},"large":{"token_count":400}}}`, strings.Repeat("a", 400), nil, 200},
		{"array tiers and CJK", `{"context_config":[{"tokenCount":400},{"token_count":100,"isDefault":true},{"token_count":200}]}`, strings.Repeat("中", 180), nil, 400},
		{"fullwidth text", `{"max_input_tokens":100,"context_config":[{"token_count":100},{"token_count":200},{"token_count":400}]}`, strings.Repeat("Ａ", 180), nil, 400},
		{"CJK punctuation", `{"max_input_tokens":100,"context_config":[{"token_count":100},{"token_count":200},{"token_count":400}]}`, strings.Repeat("、。", 90), nil, 400},
		{"current tier retained", `{"max_input_tokens":400,"context_config":[{"token_count":100,"is_default":true},{"token_count":400}]}`, strings.Repeat("中", 180), nil, 400},
		{"largest tier", `{"max_input_tokens":100,"context_config":[{"token_count":100},{"token_count":400}]}`, strings.Repeat("中", 1000), nil, 400},
		{"tool definitions counted", `{"max_input_tokens":100,"context_config":[{"token_count":100},{"token_count":400}]}`, "hello", json.RawMessage(`[{"type":"function","function":{"name":"lookup","description":"` + strings.Repeat("a", 800) + `","parameters":{"type":"object"}}}]`), 400},
		{"no declared tiers", `{"max_input_tokens":100}`, strings.Repeat("中", 1000), nil, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"messages": []map[string]string{{"role": "user", "content": tc.text}}, "max_tokens": 1}
			if tc.tools != nil {
				input["tools"] = tc.tools
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request, err := (&Service{}).BuildChatRequest(context.Background(), &Credential{Region: "cn", UserID: "u", AccessToken: "token", MachineID: "m"}, body, Model{Key: "key", Config: json.RawMessage(tc.config)})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := request.Body.Close(); err != nil {
				t.Fatal(err)
			}
			assertCOSYRequest(t, request, encoded)
			var envelope struct {
				Parameters  map[string]any `json:"parameters"`
				ModelConfig map[string]any `json:"model_config"`
				Context     struct {
					Extra map[string]any `json:"extra"`
				} `json:"chat_context"`
			}
			if err := json.Unmarshal(decodeNativeRequest(t, encoded), &envelope); err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if envelope.Parameters["context_length"] != nil || envelope.ModelConfig["max_input_tokens"] != nil || envelope.Context.Extra["ideModelConfigOverride"] != nil {
					t.Fatalf("request changed context without an upgrade: %s", decodeNativeRequest(t, encoded))
				}
				return
			}
			if envelope.Parameters["context_length"] != float64(tc.want) || envelope.ModelConfig["max_input_tokens"] != float64(tc.want) || envelope.Context.Extra["ideModelConfigOverride"].(map[string]any)["max_input_tokens"] != float64(tc.want) {
				t.Fatalf("context tier missing from native fields: %s", decodeNativeRequest(t, encoded))
			}
		})
	}
}

func assertCOSYRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	if got := req.Header.Get("User-Agent"); got != "Qoder" {
		t.Errorf("User-Agent = %q, want Qoder", got)
	}
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
	stream := NormalizeStream(source, nil)
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
		{"explicit queue signal", `data: {"statusCodeValue":403,"body":{"isQueued":true,"message":"waiting","credit":0}}` + "\n\n", 503, false},
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
		stream := NormalizeStream(io.NopCloser(qoderFailureReader{err: cause}), nil)
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
	for _, config := range []string{
		`{"context_config":"invalid"}`,
		`{"context_config":{"small":{"token_count":"100"}}}`,
		`{"context_config":[{"token_count":100}],"max_input_tokens":"100"}`,
	} {
		if _, err := s.BuildChatRequest(context.Background(), &Credential{Region: "global", UserID: "u", AccessToken: "token", MachineID: "m"}, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), Model{Key: "key", Config: json.RawMessage(config)}); err == nil {
			t.Errorf("accepted invalid model context %s", config)
		}
	}
}

/**
 * @brief buildNativeChat exercises the exported builder and decodes its wire output.
 * @param t Current test.
 * @param body Logical Chat JSON.
 * @param model Selected model.
 * @param credential Account override, or nil for the test account.
 * @return The signed HTTP request and decoded native envelope.
 */
func buildNativeChat(t *testing.T, body string, model Model, credential *Credential) (*http.Request, map[string]any) {
	t.Helper()
	if credential == nil {
		credential = &Credential{Region: "global", UserID: "wire-user", AccessToken: "token", MachineID: "machine"}
	}
	request, err := (&Service{}).BuildChatRequest(context.Background(), credential, []byte(body), model)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Body.Close(); err != nil {
		t.Fatal(err)
	}
	assertCOSYRequest(t, request, encoded)
	var envelope map[string]any
	if err := json.Unmarshal(decodeNativeRequest(t, encoded), &envelope); err != nil {
		t.Fatal(err)
	}
	return request, envelope
}

/**
 * @brief TestBuildChatRequestDesktopIdentity verifies the app fingerprint and slim model.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestDesktopIdentity(t *testing.T) {
	model := Model{Key: "native", Config: json.RawMessage(`{"key":"native","display_name":"Native Model","outer_provider":"vendor","format":"openai","is_vl":true,"is_reasoning":true,"max_input_tokens":100000,"source":"byok","minimal_version":"2","strategies":{},"thinking_config":{"enabled":{},"disabled":{}}}`)}
	request, envelope := buildNativeChat(t, `{"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"disabled"}}`, model, nil)
	for key, want := range map[string]string{"Cosy-Version": "1.1.69", "Cosy-ClientType": "10", "Cosy-Business-Product": "app", "Cosy-Business-Type": "agent", "Cosy-Scene": "app"} {
		if got := request.Header.Get(key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"Cosy-ClientIp", "Cosy-MachineToken", "Cosy-MachineType"} {
		if request.Header.Get(key) != "" {
			t.Errorf("invented desktop device identity %s", key)
		}
	}
	if envelope["session_type"] != "app" {
		t.Errorf("wrong session type: %v", envelope["session_type"])
	}
	want := map[string]any{"key": "native", "display_name": "Native Model", "outer_provider": "vendor", "format": "openai", "is_vl": true, "is_reasoning": false, "max_input_tokens": float64(100000), "api_key": "", "url": "", "source": "byok"}
	if !reflect.DeepEqual(envelope["model_config"], want) {
		t.Fatalf("model config = %v, want %v", envelope["model_config"], want)
	}
	business := envelope["business"].(map[string]any)
	if business["model_key"] != "native" || business["product"] != "app" || business["version"] != "1.1.69" {
		t.Fatalf("wrong business identity: %v", business)
	}
	messages := envelope["messages"].([]any)
	if !reflect.DeepEqual(envelope["system"], messages[0].(map[string]any)["content"]) {
		t.Fatal("system fields differ")
	}
	block := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(block["cache_control"], map[string]any{"type": "ephemeral"}) || !reflect.DeepEqual(messages[1].(map[string]any)["contents"], messages[1].(map[string]any)["content"]) {
		t.Fatalf("missing user prompt cache: %v", messages[1])
	}
}

/**
 * @brief TestBuildChatRequestOutputAndThinkingBudget verifies output caps and small asks.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestOutputAndThinkingBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input, config, key string
		limit                    int
		thinking                 bool
		effort                   string
	}{
		{"default budget", ``, `{}`, "model", 32000, false, ""},
		{"unlisted ceiling", `,"max_tokens":80000`, `{}`, "model", 80000, false, ""},
		{"directory ceiling", `,"max_tokens":80000`, `{"max_output_tokens":64000}`, "model", 64000, false, ""},
		{"camel ceiling", `,"max_tokens":80000`, `{"maxOutputTokens":48000}`, "model", 48000, false, ""},
		{"model ceiling", `,"max_tokens":200000`, `{}`, "lmodel", 131072, false, ""},
		{"completion precedence", `,"max_tokens":200000,"max_completion_tokens":3000`, `{}`, "model", 3000, false, ""},
		{"toggleable short ask", `,"max_tokens":1024`, `{"is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"low":{},"high":{"is_default":true}}},"disabled":{}}}`, "model", 1024, false, ""},
		{"always thinks short ask", `,"max_tokens":1024`, `{"is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"low":{},"high":{"is_default":true}}}}}`, "model", 1024, true, "low"},
		{"explicit effort short ask", `,"max_tokens":32,"reasoning_effort":"high"`, `{"is_reasoning":true,"reasoning_efforts":["low","high"]}`, "model", 32, true, "high"},
		{"directory default", ``, `{"thinking_config":{"enabled":{"efforts":{"low":{},"max":{"is_default":true}}},"disabled":{}}}`, "model", 32000, true, "max"},
		{"none lowest effort", `,"reasoning_effort":"none"`, `{"thinking_config":{"enabled":{"efforts":{"max":{},"low":{}}}}}`, "model", 32000, true, "low"},
		{"ultra nearest", `,"reasoning_effort":"ultra"`, `{"is_reasoning":true,"reasoning_efforts":["low","high","xhigh"]}`, "model", 32000, true, "xhigh"},
		{"nonthinking model", `,"reasoning_effort":"high"`, `{}`, "model", 32000, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, envelope := buildNativeChat(t, `{"messages":[{"role":"user","content":"hello"}]`+tc.input+`}`, Model{Key: tc.key, Config: json.RawMessage(tc.config)}, nil)
			params := envelope["parameters"].(map[string]any)
			if params["max_tokens"] != float64(tc.limit) || params["enable_thinking"] != tc.thinking {
				t.Fatalf("parameters = %v", params)
			}
			if effort, _ := params["reasoning_effort"].(string); effort != tc.effort {
				t.Errorf("effort = %q, want %q", effort, tc.effort)
			}
		})
	}
	_, envelope := buildNativeChat(t, `{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`, Model{Key: "model", Config: json.RawMessage(`{"max_input_tokens":100,"context_config":{"small":{"token_count":100,"is_default":true},"large":{"token_count":200}}}`)}, nil)
	if envelope["parameters"].(map[string]any)["context_length"] != float64(200) {
		t.Fatalf("reply was not counted in context: %v", envelope["parameters"])
	}
}

/**
 * @brief TestBuildChatRequestSessionAndTask verifies stable sessions and tool-loop tasks.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestSessionAndTask(t *testing.T) {
	seed := uuid.NewString()
	first := `{"session_id":"` + seed + `","messages":[{"role":"user","content":"0123456789 task"}]}`
	loop := `{"session_id":"` + seed + `","messages":[{"role":"user","content":"0123456789 task"},{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call","content":"result"}]}`
	_, a := buildNativeChat(t, first, Model{Key: "a"}, nil)
	_, b := buildNativeChat(t, loop, Model{Key: "b"}, nil)
	businessA, businessB := a["business"].(map[string]any), b["business"].(map[string]any)
	if a["session_id"] != b["session_id"] || a["request_set_id"] != b["request_set_id"] || businessA["id"] != a["request_set_id"] || businessA["begin_at"] != businessB["begin_at"] || businessA["name"] != "0123456789" || businessA["stage"] != "start" || businessB["stage"] != "processing" {
		t.Fatalf("task changed in tool loop: %v / %v", businessA, businessB)
	}
	_, replay := buildNativeChat(t, first, Model{Key: "a"}, nil)
	if replay["business"].(map[string]any)["stage"] != "start" {
		t.Fatal("replayed initial request changed stage")
	}
	if a["request_id"] == b["request_id"] || a["request_id"] != a["chat_record_id"] || b["request_id"] != b["chat_record_id"] {
		t.Fatal("each call must have its own matching request/chat record")
	}
	_, next := buildNativeChat(t, strings.Replace(first, "0123456789 task", "new prompt", 1), Model{Key: "a"}, nil)
	if next["session_id"] != a["session_id"] || next["request_set_id"] == a["request_set_id"] {
		t.Fatal("new prompt did not start its own task")
	}
	for _, credential := range []*Credential{{Region: "cn", UserID: "wire-user", AccessToken: "token", MachineID: "machine"}, {Region: "global", UserID: "another-user", AccessToken: "token", MachineID: "machine"}} {
		_, other := buildNativeChat(t, first, Model{Key: "a"}, credential)
		if other["request_set_id"] == a["request_set_id"] {
			t.Fatal("different accounts shared task state")
		}
	}
	for _, field := range []string{"session_id", "prompt_cache_key", "user", ""} {
		prefix := ""
		if field != "" {
			prefix = `"` + field + `":"` + uuid.NewString() + `",`
		}
		opening := `{"role":"system","content":"policy"},{"role":"user","content":"first"}`
		_, start := buildNativeChat(t, `{`+prefix+`"messages":[`+opening+`]}`, Model{Key: "a"}, nil)
		_, continuation := buildNativeChat(t, `{`+prefix+`"messages":[`+opening+`,{"role":"assistant","content":"answer"},{"role":"user","content":"second"}]}`, Model{Key: "a"}, nil)
		if start["session_id"] != continuation["session_id"] {
			t.Errorf("session changed for seed %q", field)
		}
	}
	_, precedence := buildNativeChat(t, strings.TrimSuffix(first, "}")+`,"prompt_cache_key":"other","user":"third"}`, Model{Key: "a"}, nil)
	if precedence["session_id"] != a["session_id"] {
		t.Fatal("explicit session lost precedence")
	}
}

/**
 * @brief TestBuildChatRequestConcurrentTask verifies one task identity under concurrency.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestConcurrentTask(t *testing.T) {
	body := `{"session_id":"` + uuid.NewString() + `","messages":[{"role":"user","content":"concurrent"}]}`
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			_, envelope := buildNativeChat(t, body, Model{Key: "model"}, nil)
			ids <- envelope["request_set_id"].(string)
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("concurrent calls created separate tasks")
		}
	}
}

/**
 * @brief TestBuildChatRequestToolHistory verifies ID repair, image relocation and thinking.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestToolHistory(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"look"},{"role":"assistant","reasoning":"thinking","tool_calls":[{"type":"function","function":{"name":"first","arguments":{"q":1}}},{"id":"kept","type":"function","function":{"name":"second","arguments":"{}"}}]},{"role":"tool","content":[{"type":"text","text":"image result"},{"type":"image_url","image_url":"data:image/png;base64,YQ=="}]},{"role":"tool","tool_call_id":"kept","content":"kept result"},{"role":"tool","tool_call_id":"orphan","content":"orphan result"},{"role":"user","content":"continue"},{"role":"assistant","content":"answer","reasoning_details":[{"text":"detail"},{"summary":"summary"}]}]}`
	_, envelope := buildNativeChat(t, body, Model{Key: "model"}, nil)
	messages := envelope["messages"].([]any)
	if len(messages) != 7 {
		t.Fatalf("message count = %d: %v", len(messages), messages)
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	generated := calls[0].(map[string]any)["id"]
	if generated == "" || messages[3].(map[string]any)["tool_call_id"] != generated || messages[4].(map[string]any)["tool_call_id"] != "kept" || assistant["reasoning_content"] != "thinking" {
		t.Fatalf("lost call pairing/thinking: %v", messages)
	}
	if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"q":1}` {
		t.Fatal("object arguments were not serialized")
	}
	user := messages[5].(map[string]any)
	encoded, _ := json.Marshal(user["content"])
	if !bytes.Contains(encoded, []byte("orphan result")) || !bytes.Contains(encoded, []byte("image_url")) || !bytes.Contains(encoded, []byte("continue")) {
		t.Fatalf("tool result content lost: %s", encoded)
	}
	if !strings.Contains(messages[3].(map[string]any)["content"].(string), "The tool returned an image") || messages[6].(map[string]any)["reasoning_content"] != "detailsummary" {
		t.Fatalf("image note/reasoning alias lost: %v", messages)
	}
	for _, message := range messages {
		if message.(map[string]any)["role"] == "user" {
			if !reflect.DeepEqual(message.(map[string]any)["content"], message.(map[string]any)["contents"]) {
				t.Fatal("user contents differ")
			}
		}
	}
	_, again := buildNativeChat(t, body, Model{Key: "model"}, nil)
	if again["messages"].([]any)[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"] != generated {
		t.Fatal("repaired tool IDs changed between calls")
	}
	_, reserved := buildNativeChat(t, `{"messages":[{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"two","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","content":"missing"},{"role":"tool","tool_call_id":"one","content":"named"}]}`, Model{Key: "model"}, nil)
	paired := reserved["messages"].([]any)
	if paired[2].(map[string]any)["tool_call_id"] != "two" || paired[3].(map[string]any)["tool_call_id"] != "one" {
		t.Fatalf("unnamed result consumed a named result's call: %v", paired)
	}
}

/**
 * @brief TestBuildChatRequestToolSchemas verifies object unions and tool-choice behavior.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestToolSchemas(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"call"}],"tool_choice":"required","tools":[{"type":"function","function":{"name":"f","parameters":{"$defs":{"a":{"type":"object"},"b":{"$ref":"#/$defs/a"},"cycle":{"$ref":"#/$defs/cycle"}},"oneOf":[{"$ref":"#/$defs/b"},{"type":"object"}],"properties":{"mixed":{"oneOf":[{"type":"object"},{"type":"string"}]},"nested":{"items":{"oneOf":[{"type":"object"},{"$ref":"#/$defs/a"}]}},"cycle":{"oneOf":[{"$ref":"#/$defs/cycle"}]}},"default":{"oneOf":[{"type":"object"}]}}}}]}`
	_, envelope := buildNativeChat(t, body, Model{Key: "model"}, nil)
	function := envelope["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	schema := function["parameters"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if schema["type"] != "object" || properties["nested"].(map[string]any)["items"].(map[string]any)["type"] != "object" || properties["mixed"].(map[string]any)["type"] != nil || properties["cycle"].(map[string]any)["type"] != nil || schema["default"].(map[string]any)["type"] != nil {
		t.Fatalf("schema normalization changed wrong branches: %v", schema)
	}
	if function["description"] != "" {
		t.Fatal("missing description must be empty")
	}
	system := envelope["system"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(system, "You must call an available function in this response.") {
		t.Fatal("required tool directive missing")
	}
	_, disabled := buildNativeChat(t, strings.Replace(body, `"required"`, `"none"`, 1), Model{Key: "model"}, nil)
	if disabled["tools"] != nil {
		t.Fatal("tool_choice none still sent tools")
	}
}

/**
 * @brief TestBuildChatRequestTaskCacheBound verifies task eviction through request IDs.
 * @param t Current test.
 * @return No value; reports contract violations through the test.
 */
func TestBuildChatRequestTaskCacheBound(t *testing.T) {
	seed := uuid.NewString()
	body := `{"session_id":"` + seed + `","messages":[{"role":"user","content":"bounded task"}]}`
	_, first := buildNativeChat(t, body, Model{Key: "model"}, nil)
	for i := range 512 {
		other := strings.Replace(body, seed, seed+strconv.Itoa(i), 1)
		buildNativeChat(t, other, Model{Key: "model"}, nil)
	}
	_, after := buildNativeChat(t, body, Model{Key: "model"}, nil)
	if first["request_set_id"] == after["request_set_id"] {
		t.Fatal("task cache retained more than its bounded capacity")
	}
}
