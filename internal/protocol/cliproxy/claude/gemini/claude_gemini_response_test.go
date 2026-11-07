package gemini

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToGemini_StreamPreservesToolUseID(t *testing.T) {
	ctx := context.Background()
	var param any

	start := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_gateway","name":"lookup"}}`)
	out := ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, start, &param)
	if len(out) != 0 {
		t.Fatalf("expected content_block_start to be buffered, got %d chunks", len(out))
	}

	delta := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"status\"}"}}`)
	out = ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, delta, &param)
	if len(out) != 0 {
		t.Fatalf("expected input_json_delta to be buffered, got %d chunks", len(out))
	}

	stop := []byte(`data: {"type":"content_block_stop","index":0}`)
	out = ConvertClaudeResponseToGemini(ctx, "gemini-2.5-pro", nil, nil, stop, &param)
	if len(out) != 1 {
		t.Fatalf("expected content_block_stop to emit 1 chunk, got %d", len(out))
	}

	got := gjson.GetBytes(out[0], "candidates.0.content.parts.0.functionCall.id").String()
	if got != "toolu_gateway" {
		t.Fatalf("expected functionCall.id %q, got %q; chunk=%s", "toolu_gateway", got, string(out[0]))
	}
}

func TestConvertClaudeResponseToGeminiNonStreamPreservesToolUseID(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"id":"msg_gateway","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_gateway","name":"lookup","input":{"query":"status"}}],"model":"claude-test","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)

	out := ConvertClaudeResponseToGeminiNonStream(ctx, "gemini-2.5-pro", nil, nil, raw, nil)

	got := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.id").String()
	if got != "toolu_gateway" {
		t.Fatalf("expected functionCall.id %q, got %q; chunk=%s", "toolu_gateway", got, string(out))
	}
}

func TestConvertClaudeResponseToGemini_ThinkingSignature(t *testing.T) {
	const validGeminiSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	tests := []struct {
		name      string
		signature string
		want      string
	}{
		{name: "foreign Claude", signature: "foreign_claude_sig_123"},
		{name: "foreign base64", signature: "Y2xhdWRlLW9wYXF1ZS1zaWduYXR1cmU="},
		{name: "bypass sentinel", signature: "skip_thought_signature_validator"},
		{name: "invalid native base64", signature: "gemini#invalid@bytes"},
		{name: "native Gemini", signature: "gemini#" + validGeminiSignature, want: validGeminiSignature},
		{name: "unprefixed Gemini", signature: validGeminiSignature, want: validGeminiSignature},
		{name: "unpadded Gemini", signature: "gemini#EggKBgEMOdbHNA", want: "EggKBgEMOdbHNA=="},
		{name: "padded Gemini", signature: "gemini#EggKBgEMOdbHNA==", want: "EggKBgEMOdbHNA=="},
	}
	for _, tt := range tests {
		for _, format := range []string{"stream start", "stream delta", "SSE start", "SSE delta", "message"} {
			t.Run(tt.name+"/"+format, func(t *testing.T) {
				startSignature, deltaSignature := "", ""
				if strings.HasSuffix(format, "start") {
					startSignature = tt.signature
				} else {
					deltaSignature = tt.signature
				}
				events := []string{
					`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-test"}}`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"` + startSignature + `"}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking text"}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + deltaSignature + `"}}`,
					`data: {"type":"content_block_stop","index":0}`,
					`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
					`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
					`data: {"type":"content_block_stop","index":1}`,
					`data: {"type":"message_stop"}`,
				}
				var outputs [][]byte
				switch {
				case strings.HasPrefix(format, "stream"):
					var param any
					for _, event := range events {
						outputs = append(outputs, ConvertClaudeResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, []byte(event), &param)...)
					}
				case strings.HasPrefix(format, "SSE"):
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(strings.Join(events, "\n")), nil))
				case format == "message":
					raw := `{"type":"message","id":"msg_123","content":[{"type":"thinking","thinking":"thinking text","signature":"` + tt.signature + `"},{"type":"text","text":"final answer"}],"stop_reason":"end_turn"}`
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(raw), nil))
				}
				var thoughtText, answer strings.Builder
				var foundSignature string
				for _, out := range outputs {
					for _, part := range gjson.GetBytes(out, "candidates.0.content.parts").Array() {
						if part.Get("thought").Bool() {
							thoughtText.WriteString(part.Get("text").String())
						} else {
							answer.WriteString(part.Get("text").String())
						}
						if signature := part.Get("thoughtSignature"); signature.Exists() {
							if tt.want == "" {
								t.Fatalf("incompatible signature was emitted: %s", part.Raw)
							}
							foundSignature = signature.String()
							if _, err := base64.StdEncoding.DecodeString(foundSignature); err != nil {
								t.Fatalf("thoughtSignature %q is not valid base64: %v; response=%s", foundSignature, err, out)
							}
						}
					}
				}
				if thoughtText.String() != "thinking text" || answer.String() != "final answer" {
					t.Fatalf("thought=%q answer=%q; expected both texts to survive signature filtering", thoughtText.String(), answer.String())
				}
				if foundSignature != tt.want {
					t.Fatalf("thoughtSignature = %q, want %q", foundSignature, tt.want)
				}
			})
		}
	}
}

func TestConvertClaudeResponseToGemini_RedactedThinkingSignature(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		want      string
	}{
		{name: "foreign Claude", signature: "opaque"},
		{name: "invalid native", signature: "gemini#invalid@bytes"},
		{name: "native Gemini", signature: "gemini#EggKBgEMOdbHNA", want: "EggKBgEMOdbHNA=="},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "/message", true: "/stream"}[stream], func(t *testing.T) {
				var outputs [][]byte
				if stream {
					var param any
					for _, event := range []string{
						`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"` + tt.signature + `"}}`,
						`data: {"type":"content_block_stop","index":0}`,
						`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
					} {
						outputs = append(outputs, ConvertClaudeResponseToGemini(context.Background(), "gemini-2.5-pro", nil, nil, []byte(event), &param)...)
					}
				} else {
					raw := `{"type":"message","content":[{"type":"redacted_thinking","data":"` + tt.signature + `"},{"type":"text","text":"final answer"}]}`
					outputs = append(outputs, ConvertClaudeResponseToGeminiNonStream(context.Background(), "gemini-2.5-pro", nil, nil, []byte(raw), nil))
				}
				var foundSignature, answer string
				for _, out := range outputs {
					for _, part := range gjson.GetBytes(out, "candidates.0.content.parts").Array() {
						if part.Get("thought").Bool() {
							foundSignature = part.Get("thoughtSignature").String()
							if foundSignature == "" {
								t.Fatalf("redacted thinking emitted an empty part: %s", part.Raw)
							}
						} else {
							answer += part.Get("text").String()
						}
					}
				}
				if foundSignature != tt.want || answer != "final answer" {
					t.Fatalf("thoughtSignature=%q answer=%q, want %q and final answer", foundSignature, answer, tt.want)
				}
			})
		}
	}
}
