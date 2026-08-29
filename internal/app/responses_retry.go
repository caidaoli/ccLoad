package app

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
)

const (
	stripMissingRequiredInputStrategy    = "strip_missing_required_input"
	stripMissingStoredInputItemStrategy  = "strip_missing_stored_input_item"
	stripUnknownInputParameterStrategy   = "strip_unknown_input_parameter"
	responsesMissingStoredItemRetryLimit = 1
)

// responsesRetryBodyForMissingRequiredParameter 在上游 HTTP 400 且
// error.code=missing_required_parameter 时，按 param 指向的 input[N] 丢掉该项，
// 供同渠道同 Key 重试一次。响应已提交则不能换 body。
func responsesRetryBodyForMissingRequiredParameter(
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if res == nil || res.ResponseCommitted || res.Status != http.StatusBadRequest {
		return nil, "", false
	}
	index, ok := missingRequiredInputIndex(res.Body)
	if !ok {
		return nil, "", false
	}
	retryBody, ok := responsesBodyWithoutInputIndex(plan.TranslatedBody, index)
	if !ok {
		return nil, "", false
	}
	return retryBody, stripMissingRequiredInputStrategy, true
}

// responsesRetryBodyForUnknownParameter 在上游报 unknown/unsupported parameter 时，
// 一次性剥离所有 input item 的 status 字段（与 HTTP 传输前置剥离同一逻辑），
// 供同渠道同 Key 重试一次。策略串是常量，重试循环按完整串去重，天然限 1 轮。
// 只接受明确指向 input[N].status 的错误，避免因其他参数拒绝而重放 POST。
// thinking/reasoning 相关错误交给 strip_codex_thinking 专门策略。响应已提交则不能换 body。
func responsesRetryBodyForUnknownParameter(
	upstreamProtocol protocol.Protocol,
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if res == nil || res.ResponseCommitted || upstreamProtocol != protocol.Codex ||
		plan.ClientProtocol != protocol.Codex || plan.RequestFamily != protocol.RequestFamilyResponses {
		return nil, "", false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest {
		return nil, "", false
	}
	if !matchesUnknownInputStatusError(errorBody) {
		return nil, "", false
	}
	retryBody := stripResponsesInputItemStatus(plan.TranslatedBody)
	if bytes.Equal(retryBody, plan.TranslatedBody) {
		return nil, "", false
	}
	return retryBody, stripUnknownInputParameterStrategy, true
}

// matchesUnknownInputStatusError only accepts errors that identify
// input[N].status. Retrying a POST for some other rejected parameter is not a
// harmless fallback: it can duplicate an upstream side effect.
func matchesUnknownInputStatusError(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	code := strings.ToLower(strings.TrimSpace(firstNonEmptyJSONString(
		root.Get("error.code"),
		root.Get("code"),
	)))
	if code != "" && code != "unknown_parameter" && code != "unsupported_parameter" {
		return false
	}
	param := firstNonEmptyJSONString(root.Get("error.param"), root.Get("param"))
	message := firstNonEmptyJSONString(
		root.Get("error.message"),
		root.Get("message"),
	)
	if code == "" {
		lowerMessage := strings.ToLower(message)
		if !strings.Contains(lowerMessage, "unknown parameter") &&
			!strings.Contains(lowerMessage, "unsupported parameter") {
			return false
		}
	}
	if param != "" {
		return isResponsesInputStatusPath(param, true)
	}
	return isResponsesInputStatusPath(message, false)
}

func isResponsesInputStatusPath(value string, exact bool) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	start := strings.Index(lower, "input[")
	if start < 0 {
		return false
	}
	rest := lower[start+len("input["):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return false
	}
	if _, err := strconv.Atoi(rest[:end]); err != nil {
		return false
	}
	rest = rest[end+1:]
	if !strings.HasPrefix(rest, ".status") {
		return false
	}
	rest = rest[len(".status"):]
	if rest == "" {
		return true
	}
	if exact {
		return false
	}
	// 嵌套路径不是 status 字段本身。引号、空格等则是错误文案边界。
	return rest[0] != '.' && rest[0] != '[' && !isResponsesPathIdentifierByte(rest[0])
}

func isResponsesPathIdentifierByte(value byte) bool {
	return value == '_' || value >= '0' && value <= '9' || value >= 'a' && value <= 'z'
}

func missingRequiredInputIndex(body []byte) (int, bool) {
	if !gjson.ValidBytes(body) {
		return 0, false
	}
	root := gjson.ParseBytes(body)
	code := strings.TrimSpace(firstNonEmptyJSONString(
		root.Get("error.code"),
		root.Get("code"),
	))
	if !strings.EqualFold(code, "missing_required_parameter") {
		return 0, false
	}
	param := firstNonEmptyJSONString(
		root.Get("error.param"),
		root.Get("param"),
		root.Get("error.message"),
		root.Get("message"),
	)
	return parseResponsesInputIndex(param)
}

func firstNonEmptyJSONString(values ...gjson.Result) string {
	for _, value := range values {
		if text := strings.TrimSpace(value.String()); text != "" {
			return text
		}
	}
	return ""
}

func parseResponsesInputIndex(param string) (int, bool) {
	param = strings.TrimSpace(param)
	start := strings.Index(strings.ToLower(param), "input[")
	if start < 0 {
		return 0, false
	}
	rest := param[start+len("input["):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func responsesBodyWithoutInputIndex(body []byte, index int) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	input, ok := root["input"].([]any)
	if !ok || index < 0 || index >= len(input) {
		return nil, false
	}
	filtered := make([]any, 0, len(input)-1)
	filtered = append(filtered, input[:index]...)
	filtered = append(filtered, input[index+1:]...)
	root["input"] = filtered
	encoded, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// responsesRetryBodyForMissingStoredInputItem 在上游按 ID 找不到 reasoning 项时
// （store=false 的典型 404，也可能落在 SSE/WS 的 HTTP 200 错误事件里），
// 丢掉该 id 对应的 reasoning 项，供同渠道重试。assistant message 和工具项的
// id 是完整 replay 的 wire contract，不能删除。响应已提交则不能换 body。
func responsesRetryBodyForMissingStoredInputItem(
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if res == nil || res.ResponseCommitted {
		return nil, "", false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return nil, "", false
	}
	id, ok := missingStoredInputItemID(errorBody)
	if !ok {
		return nil, "", false
	}
	retryBody, ok := responsesBodyWithoutMissingReasoningID(plan.TranslatedBody, id)
	if !ok {
		return nil, "", false
	}
	return retryBody, stripMissingStoredInputItemStrategy + ":" + id, true
}

func forwardResultErrorPayload(res *fwResult) ([]byte, int) {
	if res == nil {
		return nil, 0
	}
	if len(res.SSEErrorEvent) > 0 {
		return res.SSEErrorEvent, classifySSEErrorStatus(res.SSEErrorEvent)
	}
	return res.Body, res.Status
}

func missingStoredInputItemID(body []byte) (string, bool) {
	if !gjson.ValidBytes(body) {
		return "", false
	}
	root := gjson.ParseBytes(body)
	message := firstNonEmptyJSONString(
		root.Get("error.message"),
		root.Get("message"),
		root.Get("response.error.message"),
	)
	return parseMissingStoredInputItemID(message)
}

func parseMissingStoredInputItemID(message string) (string, bool) {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "not found") {
		return "", false
	}
	for _, marker := range []string{"item with id '", `item with id "`} {
		start := strings.Index(lower, marker)
		if start < 0 {
			continue
		}
		rest := message[start+len(marker):]
		end := strings.IndexByte(rest, marker[len(marker)-1])
		if end <= 0 {
			continue
		}
		id := strings.TrimSpace(rest[:end])
		if id == "" {
			continue
		}
		return id, true
	}
	return "", false
}

func responsesBodyWithoutMissingReasoningID(body []byte, id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	input, ok := root["input"].([]any)
	if !ok {
		return nil, false
	}
	filtered := make([]any, 0, len(input))
	removed := false
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		itemID, _ := obj["id"].(string)
		itemType, _ := obj["type"].(string)
		// reasoning 可以在 stateless replay 中省略；message/tool item 不能，
		// 否则会丢上下文或制造不符合 Responses Create schema 的对象。
		if itemID == id && itemType == "reasoning" {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return nil, false
	}
	root["input"] = filtered
	encoded, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func codexWebsocketMissingStoredInputRetryBody(replayBody, payload []byte) ([]byte, bool) {
	id, ok := missingStoredInputItemID(payload)
	if !ok {
		return nil, false
	}
	return responsesBodyWithoutMissingReasoningID(replayBody, id)
}
