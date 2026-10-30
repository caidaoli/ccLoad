package builtin

import (
	"bytes"
	"context"
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
							for _, line := range bytes.Split(chunk, []byte{'\n'}) {
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

func TestCliproxyCodexCompletedResponsePreservesInnerObjectOrder(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"z":1,"a":{"y":2,"x":3}}`)
	got, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		t.Fatalf("cliproxyCodexCompletedResponse() error = %v", err)
	}
	want := `{"type":"response.completed","response":{"z":1,"a":{"y":2,"x":3}}}`
	if string(got) != want {
		t.Fatalf("wrapped response = %s, want %s", got, want)
	}
}

func TestCliproxyCodexCompletedResponseRejectsTruncatedJSON(t *testing.T) {
	t.Parallel()
	if _, err := cliproxyCodexCompletedResponse([]byte(`{"z":1`)); err == nil {
		t.Fatal("accepted truncated JSON")
	}
}

func TestCliproxyCodexCompletedResponseDoesNotDoubleWrap(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)
	got, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("already wrapped response was modified: %s", got)
	}
}
