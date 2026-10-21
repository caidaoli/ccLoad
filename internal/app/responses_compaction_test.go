package app

import (
	"encoding/base64"
	"strings"
	"testing"

	"ccLoad/internal/config"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

func TestResponsesCompactionRoundTrip(t *testing.T) {
	response := []byte(`{"id":"resp_summary","model":"summary-model","status":"completed","created_at":123,"error":null,"incomplete_details":null,"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"private reasoning"}]},{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"目标：修复工具调用。\n保留路径 /tmp/file.go"}]}],"usage":{"input_tokens":15,"output_tokens":9,"total_tokens":24,"input_tokens_details":{"cached_tokens":4}}}`)
	for _, compactEndpoint := range []bool{false, true} {
		compact, err := buildResponsesCompactionResponse(response, compactEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		object := "response"
		if compactEndpoint {
			object = "response.compaction"
		}
		if gjson.GetBytes(compact, "object").String() != object || gjson.GetBytes(compact, "id").String() != "resp_summary" ||
			gjson.GetBytes(compact, "model").String() != "summary-model" || gjson.GetBytes(compact, "usage").Raw != gjson.GetBytes(response, "usage").Raw {
			t.Fatalf("response metadata changed: %s", compact)
		}
		item := gjson.GetBytes(compact, "output.0")
		if item.Get("type").String() != "compaction" || item.Get("status").String() != "completed" || item.Get("id").String() == "" {
			t.Fatalf("invalid compaction item: %s", item.Raw)
		}
		request := []byte(`{"model":"next-model","input":[` + item.Raw + `,{"role":"user","content":"continue"}]}`)
		expanded, err := expandResponsesCompaction(request)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(expanded, "input.0.role").String() != "user" ||
			!strings.Contains(gjson.GetBytes(expanded, "input.0.content.0.text").String(), "目标：修复工具调用。\n保留路径 /tmp/file.go") ||
			strings.Contains(string(expanded), "private reasoning") || gjson.GetBytes(expanded, "input.1.content").String() != "continue" {
			t.Fatalf("invalid expanded conversation: %s", expanded)
		}
		frames, err := responsesCompactionStream(compact)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"}
		if len(frames) != len(want) {
			t.Fatalf("got %d SSE frames", len(frames))
		}
		for i, frame := range frames {
			lines := strings.Split(strings.TrimSpace(string(frame)), "\n")
			if len(lines) != 2 || lines[0] != "event: "+want[i] || !strings.HasPrefix(lines[1], "data: ") {
				t.Fatalf("invalid SSE framing: %q", frame)
			}
			payload := gjson.Parse(strings.TrimPrefix(lines[1], "data: "))
			if payload.Get("type").String() != want[i] || payload.Get("sequence_number").Int() != int64(i) {
				t.Fatalf("invalid SSE event: %s", payload.Raw)
			}
			if i < 2 && (payload.Get("response.status").String() != "in_progress" || len(payload.Get("response.output").Array()) != 0) {
				t.Fatalf("invalid progress response: %s", payload.Raw)
			}
			if i == 4 && (payload.Get("response.output.0.encrypted_content").String() != item.Get("encrypted_content").String() || payload.Get("response.usage.total_tokens").Int() != 24) {
				t.Fatalf("final response lost capsule or usage: %s", payload.Raw)
			}
		}
	}
}

func TestResponsesCompactionPrepareSummary(t *testing.T) {
	body := []byte(`{"model":"target","stream":true,"instructions":"keep instructions","max_output_tokens":1200,"tools":[{"type":"function","name":"x"}],"tool_choice":{"type":"function","name":"x"},"parallel_tool_calls":true,"additional_tools":[],"metadata":{"x":"y"},"previous_response_id":null,"response_format":{"type":"json_object"},"text":{"format":{"type":"json_schema"},"verbosity":"low"},"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/img"},{"type":"input_text","text":"inspect"}]},{"type":"function_call","call_id":"call1","name":"x","arguments":"{\"path\":\"/tmp/a\"}"},{"type":"function_call_output","call_id":"call1","output":"bad exit=2"},{"type":"additional_tools","tools":[{"name":"extra"}]},{"type":"reasoning","encrypted_content":"foreign","summary":[{"type":"summary_text","text":"useful finding"}]},{"type":"compaction_trigger"},{"type":"future_unknown","content":"retain for converter"}]}`)
	prepared, err := prepareResponsesCompactionSummary(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls", "additional_tools", "metadata", "previous_response_id", "response_format", "text.format"} {
		if gjson.GetBytes(prepared, field).Exists() {
			t.Errorf("conflicting generation field %s remains", field)
		}
	}
	if gjson.GetBytes(prepared, "stream").Bool() || gjson.GetBytes(prepared, "max_output_tokens").Int() != 1200 ||
		gjson.GetBytes(prepared, "instructions").String() != "keep instructions" || gjson.GetBytes(prepared, "text.verbosity").String() != "low" ||
		gjson.GetBytes(prepared, "input.0.content.0.image_url").String() != "https://example.com/img" {
		t.Fatalf("summary preparation changed unrelated input: %s", prepared)
	}
	input := gjson.GetBytes(prepared, "input").Array()
	for _, item := range input {
		if item.Get("type").String() == "compaction_trigger" || item.Get("type").String() == "function_call" {
			t.Fatalf("live tool/trigger remains: %s", item.Raw)
		}
	}
	for _, text := range []string{"call1", "/tmp/a", "bad exit=2", "extra", "useful finding", "retain for converter"} {
		if !strings.Contains(string(prepared), text) {
			t.Errorf("history lost %q: %s", text, prepared)
		}
	}
	if input[len(input)-2].Get("type").String() != "future_unknown" || input[len(input)-1].Get("role").String() != "user" {
		t.Fatalf("unknown item or final summary prompt lost: %s", prepared)
	}
}

func TestResponsesCompactionRejectsInvalidCapsulesAndHistory(t *testing.T) {
	for _, encoded := range []string{"%%%", base64.RawURLEncoding.EncodeToString([]byte(`{"summary":"  "}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"summary":42}`))} {
		body := []byte(`{"input":[{"type":"compaction","encrypted_content":"ccload-compact-v1:` + encoded + `"}]}`)
		if _, err := expandResponsesCompaction(body); err == nil {
			t.Errorf("accepted invalid capsule %q", encoded)
		}
	}
	for _, body := range []string{`{"previous_response_id":"resp_missing","input":[]}`, `{"input":[{"type":"compaction","encrypted_content":"foreign"}]}`, `{"input":[{"type":"compaction_summary","summary":"unknown"}]}`, `{"input":42}`} {
		if _, err := prepareResponsesCompactionSummary([]byte(body)); err == nil {
			t.Errorf("accepted unsupported history: %s", body)
		}
	}
	for _, body := range []string{` {"input":[{"role":"user","content":"hello"}]} `, `{"input":[{"type":"compaction","encrypted_content":"foreign"}]}`} {
		got, err := expandResponsesCompaction([]byte(body))
		if err != nil || string(got) != body {
			t.Fatalf("unrelated request changed: %s, %v", got, err)
		}
	}
}

func TestResponsesCompactionRejectsUnsuccessfulSummary(t *testing.T) {
	for _, body := range []string{
		`{"status":"failed","output":[]}`,
		`{"status":"incomplete","output":[]}`,
		`{"status":"completed","error":{"message":"bad"},"output":[]}`,
		`{"status":"completed","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
		`{"status":"completed","output":[]}`,
		`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":" "}]}]}`,
		`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]},{"type":"function_call","name":"x"}]}`,
		`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`,
	} {
		if _, err := buildResponsesCompactionResponse([]byte(body), false); err == nil {
			t.Errorf("accepted unsuccessful summary: %s", body)
		}
	}
}

func TestResponsesCompactionRequiresSourceCompletion(t *testing.T) {
	for _, test := range []struct {
		source protocol.Protocol
		body   string
		want   bool
	}{
		{protocol.OpenAI, `{"choices":[{"finish_reason":"stop"}]}`, true},
		{protocol.OpenAI, `{"choices":[{"message":{"content":"partial"}}]}`, false},
		{protocol.OpenAI, `{"choices":[{"finish_reason":"length"}]}`, false},
		{protocol.Anthropic, `{"stop_reason":"end_turn"}`, true},
		{protocol.Anthropic, `{"stop_reason":"stop_sequence"}`, true},
		{protocol.Anthropic, `{"stop_reason":"max_tokens"}`, false},
		{protocol.Gemini, `{"candidates":[{"finishReason":"STOP"}]}`, true},
		{protocol.Gemini, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, false},
		{protocol.Codex, `{"status":"completed","error":null,"incomplete_details":null}`, true},
		{protocol.Codex, `{"status":"completed","error":{"message":"bad"}}`, false},
	} {
		if got := responsesCompactionSummaryComplete(test.source, []byte(test.body)); got != test.want {
			t.Errorf("completion(%s, %s) = %v, want %v", test.source, test.body, got, test.want)
		}
	}
}

func TestResponsesCompactionRequested(t *testing.T) {
	if !responsesCompactionRequested("/v1/responses/compact/", nil) || !responsesCompactionRequested("/v1/responses", []byte(`{"input":[{"type":"compaction_trigger"}]}`)) ||
		responsesCompactionRequested("/v1/responses", []byte(`{"input":[{"role":"user","content":"compaction_trigger"}]}`)) {
		t.Fatal("incorrect compaction request detection")
	}
}

func TestResponsesCompactionRemovesTopLevelOutputText(t *testing.T) {
	response, err := buildResponsesCompactionResponse([]byte(`{"status":"completed","output_text":"plaintext summary","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plaintext summary"}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(response, "output_text").Exists() || strings.Contains(string(response), "plaintext summary") {
		t.Fatalf("plaintext summary leaked outside the capsule: %s", response)
	}
}

func TestResponsesCompactionRejectsEncodedResponseTooLargeForReplay(t *testing.T) {
	// The summary itself fits, but base64 and the response envelope exceed
	// the request limit and would make the capsule impossible to replay.
	body := []byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("x", config.DefaultMaxBodyBytes*3/4) + `"}]}]}`)
	if _, err := buildResponsesCompactionResponse(body, false); err == nil {
		t.Fatal("accepted a capsule larger than the request body limit")
	}
}
