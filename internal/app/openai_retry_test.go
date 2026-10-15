package app

import (
	"net/http"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

func TestOpenAIRetryBodyForMaxTokens(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"gpt-5.6-sol","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`)
	res := &fwResult{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`),
	}
	got, strategy, ok := openaiRetryBodyForMaxTokens(protocol.OpenAI, protocol.TransformPlan{TranslatedBody: body}, res)
	if !ok || strategy != remapOpenAIMaxCompletionTokensStrategy {
		t.Fatalf("retry = (%q, %v) body=%s", strategy, ok, got)
	}
	if gjson.GetBytes(got, "max_tokens").Exists() {
		t.Fatalf("max_tokens survived: %s", got)
	}
	if gjson.GetBytes(got, "max_completion_tokens").Int() != 64000 {
		t.Fatalf("max_completion_tokens missing: %s", got)
	}
	if _, _, ok := openaiRetryBodyForMaxTokens(protocol.Anthropic, protocol.TransformPlan{TranslatedBody: body}, res); ok {
		t.Fatal("Anthropic protocol used the OpenAI max_tokens remap")
	}
}
