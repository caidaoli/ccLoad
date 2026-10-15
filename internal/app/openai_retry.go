package app

import (
	"net/http"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const remapOpenAIMaxCompletionTokensStrategy = "remap_openai_max_completion_tokens"

// openaiRetryBodyForMaxTokens rewrites Chat Completions max_tokens to
// max_completion_tokens after Azure/OpenAI 400 unsupported_parameter.
// GPT-5.x and o-series reject max_tokens; the Anthropic→OpenAI converter
// already remaps known families, and this retry covers passthrough bodies
// and unknown model names.
func openaiRetryBodyForMaxTokens(
	upstreamProtocol protocol.Protocol,
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if upstreamProtocol != protocol.OpenAI || res == nil || res.ResponseCommitted || res.Status != http.StatusBadRequest {
		return nil, "", false
	}
	if !isOpenAIMaxTokensUnsupportedError(res.Body) {
		return nil, "", false
	}
	body := plan.TranslatedBody
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if !maxTokens.Exists() {
		return nil, "", false
	}
	updated, err := sjson.DeleteBytes(body, "max_tokens")
	if err != nil {
		return nil, "", false
	}
	if !gjson.GetBytes(updated, "max_completion_tokens").Exists() {
		updated, err = sjson.SetBytes(updated, "max_completion_tokens", maxTokens.Int())
		if err != nil {
			return nil, "", false
		}
	}
	return updated, remapOpenAIMaxCompletionTokensStrategy, true
}

func isOpenAIMaxTokensUnsupportedError(body []byte) bool {
	param := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.param").String()))
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if param != "max_tokens" && !strings.Contains(message, "'max_tokens'") && !strings.Contains(message, `"max_tokens"`) {
		return false
	}
	if !strings.Contains(message, "max_completion_tokens") &&
		!strings.Contains(message, "not supported") &&
		code != "unsupported_parameter" {
		return false
	}
	return true
}
