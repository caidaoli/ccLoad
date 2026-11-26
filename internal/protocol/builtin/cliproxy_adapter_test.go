package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"

	"ccLoad/internal/protocol"
)

func TestRegistryOpenAIChatDeepSeekCache(t *testing.T) {
	reg := protocol.NewRegistry()
	Register(reg)
	tests := []struct {
		name      string
		fields    string
		wantCache int64
	}{
		{"deepseek", `"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20`, 80},
		{"standard wins", `"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":64}`, 64},
		{"standard zero wins", `"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":0}`, 0},
		{"missing standard", `"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"audio_tokens":2}`, 80},
		{"null standard", `"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":null}`, 80},
		{"nonnumeric standard", `"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":"64"}`, 80},
		{"negative hit", `"prompt_cache_hit_tokens":-1`, 0},
		{"overflow hit", `"prompt_cache_hit_tokens":1e300`, 0},
		{"nonnumeric hit", `"prompt_cache_hit_tokens":"80"`, 0},
	}
	for _, target := range []protocol.Protocol{protocol.Anthropic, protocol.Codex, protocol.Gemini} {
		for _, tt := range tests {
			t.Run(string(target)+"/"+tt.name, func(t *testing.T) {
				usage := `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,` + tt.fields + `}`
				t.Run("JSON", func(t *testing.T) {
					raw := []byte(`{"id":"chatcmpl-deepseek","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + usage + `}`)
					out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, target, "deepseek-chat", nil, nil, raw)
					if err != nil {
						t.Fatal(err)
					}
					assertTranslatedCacheUsage(t, target, out, tt.wantCache)
				})
				t.Run("SSE trailing usage", func(t *testing.T) {
					frames := []string{
						`{"id":"chatcmpl-deepseek","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
						`{"id":"chatcmpl-deepseek","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
						`{"id":"chatcmpl-deepseek","model":"deepseek-chat","choices":[],"usage":` + usage + `}`,
						`[DONE]`,
					}
					var state any
					var result []byte
					for _, frame := range frames {
						out, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, target, "deepseek-chat", nil, nil, []byte("data: "+frame+"\n\n"), &state)
						if err != nil {
							t.Fatal(err)
						}
						for _, chunk := range out {
							for line := range bytes.SplitSeq(chunk, []byte{'\n'}) {
								if !bytes.HasPrefix(line, []byte("data: ")) {
									continue
								}
								payload := bytes.TrimPrefix(line, []byte("data: "))
								root := gjson.ParseBytes(payload)
								if root.Get("usage").Exists() || root.Get("usageMetadata").Exists() {
									result = bytes.Clone(payload)
								} else if root.Get("type").String() == "response.completed" {
									result = []byte(root.Get("response").Raw)
								}
							}
						}
					}
					assertTranslatedCacheUsage(t, target, result, tt.wantCache)
				})
			})
		}
	}
}

func assertTranslatedCacheUsage(t *testing.T, target protocol.Protocol, body []byte, wantCache int64) {
	t.Helper()
	var inputPath, outputPath, cachePath string
	wantInput := int64(100)
	switch target {
	case protocol.Anthropic:
		inputPath, outputPath, cachePath = "usage.input_tokens", "usage.output_tokens", "usage.cache_read_input_tokens"
		wantInput -= wantCache
	case protocol.Codex:
		inputPath, outputPath, cachePath = "usage.input_tokens", "usage.output_tokens", "usage.input_tokens_details.cached_tokens"
	case protocol.Gemini:
		inputPath, outputPath, cachePath = "usageMetadata.promptTokenCount", "usageMetadata.candidatesTokenCount", "usageMetadata.cachedContentTokenCount"
	default:
		t.Fatalf("unexpected target %s", target)
	}
	if !gjson.ValidBytes(body) {
		t.Fatalf("invalid translated JSON: %s", body)
	}
	if input, output, cache := gjson.GetBytes(body, inputPath).Int(), gjson.GetBytes(body, outputPath).Int(), gjson.GetBytes(body, cachePath).Int(); input != wantInput || output != 5 || cache != wantCache {
		t.Fatalf("usage input/output/cache = %d/%d/%d, want %d/5/%d: %s", input, output, cache, wantInput, wantCache, body)
	}
}

/**
 * @brief 验证终态封装保留原始 Responses 正文的 JSON 字段顺序。
 * @param t 测试上下文。
 */
func TestCliproxyCodexTerminalResponsePreservesInnerObjectOrder(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"z":1,"a":{"y":2,"x":3}}`)
	got, err := cliproxyCodexTerminalResponse(raw)
	if err != nil {
		t.Fatalf("cliproxyCodexTerminalResponse() error = %v", err)
	}
	want := `{"type":"response.completed","response":{"z":1,"a":{"y":2,"x":3}}}`
	if string(got) != want {
		t.Fatalf("wrapped response = %s, want %s", got, want)
	}
}

/**
 * @brief 验证 Chat 思考内容数组经 Registry 转换为有序的 Anthropic 思考与正文块
 * @param t 测试上下文
 */
func TestRegistryOpenAIThinkingContentParts(t *testing.T) {
	reg := protocol.NewRegistry()
	Register(reg)
	content := `[{"type":"thinking","thinking":[{"type":"text","text":"First "},{"type":"text","text":"thought."}]},{"type":"text","text":"Answer"}]`
	raw := []byte(`{"id":"chat_1","model":"glm","choices":[{"message":{"role":"assistant","content":` + content + `},"finish_reason":"stop"}]}`)
	out, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "glm", nil, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	blocks := gjson.GetBytes(out, "content").Array()
	if len(blocks) != 2 || blocks[0].Get("type").String() != "thinking" || blocks[0].Get("thinking").String() != "First thought." || blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != "Answer" {
		t.Fatalf("unexpected converted content: %s", out)
	}

	frames := []string{
		`{"id":"chat_1","model":"glm","choices":[{"delta":{"content":` + content + `},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	var state any
	var starts, values []string
	for _, frame := range frames {
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Anthropic, "glm", []byte(`{"stream":true}`), nil, []byte("data: "+frame+"\n\n"), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			for line := range bytes.SplitSeq(chunk, []byte{'\n'}) {
				if !bytes.HasPrefix(line, []byte("data: ")) {
					continue
				}
				event := gjson.ParseBytes(bytes.TrimPrefix(line, []byte("data: ")))
				switch event.Get("type").String() {
				case "content_block_start":
					starts = append(starts, event.Get("content_block.type").String())
				case "content_block_delta":
					if event.Get("delta.type").String() == "thinking_delta" {
						values = append(values, event.Get("delta.thinking").String())
					} else {
						values = append(values, event.Get("delta.text").String())
					}
				}
			}
		}
	}
	if len(starts) != 2 || starts[0] != "thinking" || starts[1] != "text" || len(values) != 2 || values[0] != "First thought." || values[1] != "Answer" {
		t.Fatalf("unexpected converted stream starts=%v values=%v", starts, values)
	}
}

/**
 * @brief 验证终态封装拒绝被截断的上游 JSON。
 * @param t 测试上下文。
 */
func TestCliproxyCodexTerminalResponseRejectsTruncatedJSON(t *testing.T) {
	t.Parallel()
	if _, err := cliproxyCodexTerminalResponse([]byte(`{"z":1`)); err == nil {
		t.Fatal("accepted truncated JSON")
	}
}

/**
 * @brief 验证上游终态事件已经带有封装时保留原样。
 * @param t 测试上下文。
 */
func TestCliproxyCodexTerminalResponseDoesNotDoubleWrap(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)
	got, err := cliproxyCodexTerminalResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("already wrapped response was modified: %s", got)
	}
}

/**
 * @brief 验证 Responses 终态别名与标准终态的正文、用量及停止原因转换一致。
 * @param t 测试上下文。
 */
func TestRegistryCodexDoneTerminal(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	Register(reg)
	decode := func(t *testing.T, raw []byte) any {
		t.Helper()
		if bytes.Equal(raw, []byte("[DONE]")) {
			return "[DONE]"
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("invalid translated JSON %q: %v", raw, err)
		}
		return value
	}
	decodeStream := func(t *testing.T, chunks [][]byte) []any {
		t.Helper()
		var values []any
		for _, chunk := range chunks {
			for line := range bytes.SplitSeq(chunk, []byte{'\n'}) {
				if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
					values = append(values, decode(t, data))
				}
			}
		}
		return values
	}
	const responseTemplate = `"response":{"id":"resp_1","created_at":1,"model":"gpt-test","status":%q,%s"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
	for _, tc := range []struct {
		status, details, canonicalEvent string
		aliasHeader, aliasPayloadType   bool
	}{
		{status: "completed", canonicalEvent: "response.completed", aliasPayloadType: true},
		{status: "incomplete", details: `"incomplete_details":{"reason":"max_output_tokens"},`, canonicalEvent: "response.incomplete", aliasHeader: true},
		{status: "cancelled", details: `"incomplete_details":null,`, canonicalEvent: "response.incomplete", aliasHeader: true, aliasPayloadType: true},
	} {
		for _, target := range []protocol.Protocol{protocol.OpenAI, protocol.Anthropic, protocol.Gemini} {
			t.Run(tc.status+"/"+string(target), func(t *testing.T) {
				response := fmt.Sprintf(responseTemplate, tc.status, tc.details)
				alias := []byte(`{"type":"response.done",` + response + `}`)
				canonicalStatus, canonicalDetails := tc.status, tc.details
				if tc.status == "cancelled" {
					canonicalStatus = "incomplete"
					canonicalDetails = `"incomplete_details":{"reason":"cancelled"},`
				}
				canonical := []byte(fmt.Sprintf(`{"type":%q,`+responseTemplate+`}`, tc.canonicalEvent, canonicalStatus, canonicalDetails))
				want, err := reg.TranslateResponseNonStream(t.Context(), protocol.Codex, target, "gpt-test", nil, nil, canonical)
				if err != nil {
					t.Fatal(err)
				}
				got, err := reg.TranslateResponseNonStream(t.Context(), protocol.Codex, target, "gpt-test", nil, nil, alias)
				if err != nil || !reflect.DeepEqual(decode(t, got), decode(t, want)) {
					t.Fatalf("response.done translation = %s (%v), want %s", got, err, want)
				}
				var aliasFrame string
				if tc.aliasHeader {
					aliasFrame = "event: response.done\n"
				}
				if !tc.aliasPayloadType {
					alias = []byte(`{` + response + `}`)
				}
				aliasFrame += "data: " + string(alias) + "\n\n"
				canonicalFrame := []byte("event: " + tc.canonicalEvent + "\ndata: " + string(canonical) + "\n\n")
				var state any
				wantChunks, err := reg.TranslateResponseStream(t.Context(), protocol.Codex, target, "gpt-test", nil, nil, canonicalFrame, &state)
				if err != nil || len(wantChunks) == 0 {
					t.Fatalf("canonical stream translation = %q (%v)", wantChunks, err)
				}
				state = nil
				gotChunks, err := reg.TranslateResponseStream(t.Context(), protocol.Codex, target, "gpt-test", nil, nil, []byte(aliasFrame), &state)
				if err != nil || !reflect.DeepEqual(decodeStream(t, gotChunks), decodeStream(t, wantChunks)) {
					t.Fatalf("response.done stream translation = %q (%v), want %q", gotChunks, err, wantChunks)
				}
				if target != protocol.OpenAI {
					state = nil
					gotChunks, err = reg.TranslateResponseStream(t.Context(), protocol.OpenAI, target, "gpt-test", nil, nil, []byte(aliasFrame), &state)
					if err != nil || !reflect.DeepEqual(decodeStream(t, gotChunks), decodeStream(t, wantChunks)) {
						t.Fatalf("response.done in OpenAI channel = %q (%v), want %q", gotChunks, err, wantChunks)
					}
				}
			})
		}
	}
}

/**
 * @brief 验证名义 Chat 渠道实际发来的 Responses 终态对 Responses 客户端保持原样。
 * @param t 测试上下文。
 */
func TestRegistryCodexDonePreservesResponsesWire(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	Register(reg)
	const payload = `{"type":"response.done","response":{"id":"resp-failed","status":"failed","error":{"code":"server_error","message":"upstream failed"}}}`
	frame := []byte("event: response.done\ndata: " + payload + "\n\n")
	for _, source := range []protocol.Protocol{protocol.OpenAI, protocol.Codex} {
		t.Run(string(source), func(t *testing.T) {
			var state any
			chunks, err := reg.TranslateResponseStream(t.Context(), source, protocol.Codex, "gpt-test", nil, nil, frame, &state)
			if err != nil || len(chunks) != 1 {
				t.Fatalf("Responses wire = %q (%v), want one event", chunks, err)
			}
			header, data, _ := bytes.Cut(chunks[0], []byte("\ndata: "))
			root := gjson.ParseBytes(bytes.TrimSpace(data))
			if string(header) != "event: response.done" || root.Get("type").String() != "response.done" || root.Get("response.status").String() != "failed" || root.Get("response.error.message").String() != "upstream failed" {
				t.Fatalf("Responses wire lost passthrough event: %s", chunks[0])
			}
		})
	}
}

/**
 * @brief 验证 Responses SSE 转 Gemini 时优先使用 JSON 的字符串类型。
 * @param t 测试上下文。
 */
func TestRegistryResponsesSSETerminalTypePrecedence(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	Register(reg)
	for _, source := range []protocol.Protocol{protocol.Codex, protocol.OpenAI} {
		for _, tc := range []struct{ name, payload, want string }{
			{"valid JSON type wins", `"type":"response.completed",`, "STOP"},
			{"missing type falls back", "", "OTHER"},
			{"non-string type falls back", `"type":123,`, "OTHER"},
		} {
			t.Run(string(source)+"/"+tc.name, func(t *testing.T) {
				frame := "event: response.done\ndata: {" + tc.payload + `"response":{"id":"r","status":"cancelled"}}` + "\n\n"
				var state any
				chunks, err := reg.TranslateResponseStream(t.Context(), source, protocol.Gemini, "gpt-test", nil, nil, []byte(frame), &state)
				if err != nil || len(chunks) != 1 {
					t.Fatalf("stream = %q, err = %v", chunks, err)
				}
				data := bytes.TrimSpace(bytes.TrimPrefix(chunks[0], []byte("data:")))
				if got := gjson.GetBytes(data, "candidates.0.finishReason").String(); got != tc.want {
					t.Fatalf("finishReason = %q, want %q: %s", got, tc.want, data)
				}
			})
		}
	}
}
