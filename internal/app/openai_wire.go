package app

import (
	"net/http"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

const remapOpenAIMaxCompletionTokensStrategy = "remap_openai_max_completion_tokens"

// normalizeOpenAIChatMaxTokens 把 Chat Completions 的 max_tokens 改名为
// max_completion_tokens。OpenAI/Azure 的 gpt-5 与 o 系列拒绝 max_tokens，而
// Anthropic→OpenAI 转换器和旧客户端仍按通用字段发送。它在渠道 BodyRules 之前
// 执行，用户规则仍可覆盖；未知型号由 openAIRetryBodyForMaxTokens 兜底。
func normalizeOpenAIChatMaxTokens(upstreamProtocol protocol.Protocol, requestPath string, body []byte) []byte {
	if upstreamProtocol != protocol.OpenAI ||
		protocol.DetectRequestFamily(requestPath) != protocol.RequestFamilyChatCompletions ||
		!openAIModelRequiresMaxCompletionTokens(gjson.GetBytes(body, "model").String()) {
		return body
	}
	if renamed, ok := renameOpenAIMaxTokens(body); ok {
		return renamed
	}
	return body
}

func openAIModelRequiresMaxCompletionTokens(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// renameOpenAIMaxTokens 删除 max_tokens；已有 max_completion_tokens 时以它为准。
func renameOpenAIMaxTokens(body []byte) ([]byte, bool) {
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if !maxTokens.Exists() || !isMutableJSONObject(body) {
		return nil, false
	}
	if !gjson.GetBytes(body, "max_completion_tokens").Exists() {
		body = setJSONRaw(body, "max_completion_tokens", maxTokens.Raw)
	}
	return deleteJSONPath(body, "max_tokens"), true
}

// openAIRetryBodyForMaxTokens 覆盖前置改名没识别的型号：上游明确拒绝
// max_tokens 时改名重发一次。
func openAIRetryBodyForMaxTokens(upstreamProtocol protocol.Protocol, plan protocol.TransformPlan, res *fwResult) ([]byte, string, bool) {
	if upstreamProtocol != protocol.OpenAI || res == nil || res.ResponseCommitted {
		return nil, "", false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest || !isOpenAIMaxTokensUnsupportedError(errorBody) {
		return nil, "", false
	}
	retryBody, ok := renameOpenAIMaxTokens(plan.TranslatedBody)
	if !ok {
		return nil, "", false
	}
	return retryBody, remapOpenAIMaxCompletionTokensStrategy, true
}

func isOpenAIMaxTokensUnsupportedError(body []byte) bool {
	errorValue := gjson.GetBytes(body, "error")
	param := strings.ToLower(strings.TrimSpace(errorValue.Get("param").String()))
	code := strings.ToLower(strings.TrimSpace(errorValue.Get("code").String()))
	message := strings.ToLower(errorValue.Get("message").String())
	if param != "max_tokens" && !strings.Contains(message, "'max_tokens'") && !strings.Contains(message, `"max_tokens"`) {
		return false
	}
	return code == "unsupported_parameter" ||
		strings.Contains(message, "max_completion_tokens") ||
		strings.Contains(message, "not supported")
}
