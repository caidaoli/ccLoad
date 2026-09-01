package app

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"

	"github.com/tidwall/gjson"
)

func isXAIOAuthResponsesRequest(cfg *model.Config, upstreamProtocol protocol.Protocol, requestPath string) bool {
	if cfg == nil || !cfg.UsesXAIOAuth() || upstreamProtocol != protocol.Codex {
		return false
	}
	if protocol.DetectRequestFamily(requestPath) == protocol.RequestFamilyResponses {
		return true
	}
	return strings.HasSuffix(strings.TrimRight(strings.TrimSpace(requestPath), "/"), "/backend-api/codex/responses")
}

func buildXAIResponsesURL(baseURL, rawQuery string) string {
	return buildUpstreamURL(baseURL, "/responses", rawQuery)
}

func finalizeXAIResponsesBody(body []byte, actualModel, executionID string) ([]byte, error) {
	actualModel = strings.TrimSpace(actualModel)
	if actualModel == "" {
		return nil, errors.New("xAI Responses request is missing actual model")
	}
	if !isMutableJSONObject(body) {
		return nil, errors.New("xAI Responses request must be a JSON object")
	}

	body = setJSONValue(body, "model", actualModel)
	body = setJSONValue(body, "stream", true)
	if executionID = strings.TrimSpace(executionID); executionID == "" {
		body = deleteJSONPath(body, "prompt_cache_key")
	} else {
		body = setJSONValue(body, "prompt_cache_key", executionID)
	}
	for _, field := range []string{"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		body = deleteJSONPath(body, field)
	}
	// xAI 会对未知字段整体拒绝，而 external_web_access 可能出现在 input 项的任意嵌套
	// 位置，顶层删除兜不住。
	body, _ = rewriteJSONMembers(body, func(key string, _ gjson.Result) (string, bool) {
		return "", key == "external_web_access"
	})

	if strings.EqualFold(actualModel, "grok-4.5") {
		for _, field := range []string{"presence_penalty", "frequency_penalty", "stop"} {
			body = deleteJSONPath(body, field)
		}
	}
	body = normalizeXAIReasoning(body, actualModel)
	body = normalizeXAIInputReasoning(body)
	return normalizeXAIImageGeneration(body, actualModel), nil
}

func normalizeXAIReasoning(body []byte, modelName string) []byte {
	reasoning := gjson.GetBytes(body, "reasoning")
	if !reasoning.Exists() {
		return body
	}
	allowed := xaiReasoningEfforts(modelName)
	if !reasoning.IsObject() || len(allowed) == 0 {
		return deleteJSONPath(body, "reasoning")
	}
	effort := strings.ToLower(strings.TrimSpace(jsonStringValue(reasoning.Get("effort"))))
	switch effort {
	case "minimal":
		effort = "low"
	case "xhigh", "max":
		effort = "high"
	}
	if _, valid := allowed[effort]; valid {
		body = setJSONValue(body, "reasoning.effort", effort)
	} else {
		body = deleteJSONPath(body, "reasoning.effort")
	}
	if jsonMemberCount(gjson.GetBytes(body, "reasoning")) == 0 {
		body = deleteJSONPath(body, "reasoning")
	}
	return body
}

func normalizeXAIInputReasoning(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	var deletions []string
	for index, item := range input.Array() {
		if !item.IsObject() || jsonStringValue(item.Get("type")) != "reasoning" {
			continue
		}
		for _, field := range []string{"content", "encrypted_content"} {
			if value := item.Get(field); value.Type == gjson.Null {
				deletions = append(deletions, "input."+strconv.Itoa(index)+"."+field)
			}
		}
	}
	for _, path := range deletions {
		body = deleteJSONPath(body, path)
	}
	return body
}

func xaiReasoningEfforts(modelName string) map[string]struct{} {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	var efforts []string
	switch modelName {
	case "grok-4.3":
		efforts = []string{"none", "low", "medium", "high"}
	case "grok-4.5", "grok-4.20-multi-agent-0309", "grok-3-mini", "grok-3-mini-fast":
		efforts = []string{"low", "medium", "high"}
	default:
		return nil
	}
	allowed := make(map[string]struct{}, len(efforts))
	for _, effort := range efforts {
		allowed[effort] = struct{}{}
	}
	return allowed
}

const (
	xaiImageGenerationToolType = "image_generation"
	xaiImageGenerationMinMajor = 4
	xaiImageGenerationMinMinor = 6
)

type xaiGrokVersion struct {
	major int
	minor int
}

// xaiSupportsImageGeneration reports whether a Grok conversation model accepts
// xAI's native Responses image_generation tool. grok-4.20 is an older product
// line whose dotted suffix is not comparable with grok-4.6.
func xaiSupportsImageGeneration(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if !strings.HasPrefix(name, "grok-") {
		return false
	}
	rest := strings.TrimPrefix(name, "grok-")
	if rest == "4.20" || strings.HasPrefix(rest, "4.20-") {
		return false
	}
	version, ok := parseXAIGrokVersion(rest)
	return ok && (version.major > xaiImageGenerationMinMajor ||
		version.major == xaiImageGenerationMinMajor && version.minor >= xaiImageGenerationMinMinor)
}

func parseXAIGrokVersion(value string) (xaiGrokVersion, bool) {
	majorEnd := 0
	for majorEnd < len(value) && value[majorEnd] >= '0' && value[majorEnd] <= '9' {
		majorEnd++
	}
	if majorEnd == 0 {
		return xaiGrokVersion{}, false
	}
	major, err := strconv.Atoi(value[:majorEnd])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	if majorEnd == len(value) || value[majorEnd] != '.' {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	minorEnd := majorEnd + 1
	for minorEnd < len(value) && value[minorEnd] >= '0' && value[minorEnd] <= '9' {
		minorEnd++
	}
	if minorEnd == majorEnd+1 {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	minor, err := strconv.Atoi(value[majorEnd+1 : minorEnd])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	return xaiGrokVersion{major: major, minor: minor}, true
}

func normalizeXAIImageGeneration(body []byte, modelName string) []byte {
	toolsResult := gjson.GetBytes(body, "tools")
	toolsIsArray := toolsResult.IsArray()
	var tools []string
	if toolsIsArray {
		for _, tool := range toolsResult.Array() {
			tools = append(tools, tool.Raw)
		}
	}
	toolsChanged := false

	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		items := input.Array()
		remaining := make([]string, 0, len(items))
		for _, item := range items {
			if item.IsObject() && strings.TrimSpace(jsonStringValue(item.Get("type"))) == "additional_tools" {
				if additional := item.Get("tools"); additional.IsArray() {
					for _, tool := range additional.Array() {
						tools = append(tools, tool.Raw)
					}
				}
				toolsChanged = true
				continue
			}
			remaining = append(remaining, item.Raw)
		}
		if len(remaining) != len(items) {
			body = setJSONRaw(body, "input", joinJSONRaw(remaining))
		}
	}

	if !xaiSupportsImageGeneration(modelName) {
		filtered := make([]string, 0, len(tools))
		for _, raw := range tools {
			tool := gjson.Parse(raw)
			if tool.IsObject() && strings.TrimSpace(jsonStringValue(tool.Get("type"))) == xaiImageGenerationToolType {
				toolsChanged = true
				continue
			}
			filtered = append(filtered, raw)
		}
		tools = filtered
	}

	if len(tools) == 0 {
		for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
			body = deleteJSONPath(body, field)
		}
		return body
	}
	if toolsChanged || !toolsIsArray {
		body = setJSONRaw(body, "tools", joinJSONRaw(tools))
	}
	return normalizeXAIToolChoice(body, tools)
}

func normalizeXAIToolChoice(body []byte, tools []string) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return body
	}
	choiceTypeValue := choice.Get("type")
	if choiceTypeValue.Type != gjson.String {
		return deleteJSONPath(body, "tool_choice")
	}
	choiceType := strings.TrimSpace(choiceTypeValue.String())
	if choiceType == xaiImageGenerationToolType {
		if !xaiToolChoiceMatches(choice, tools) {
			return deleteJSONPath(body, "tool_choice")
		}
		// 客户端强制选择 image_generation 时改写成 xAI 接受的 allowed_tools+required。
		wrapper := newJSONObjectBuilder()
		wrapper.Set("type", `"allowed_tools"`)
		wrapper.Set("mode", `"required"`)
		wrapper.Set("tools", joinJSONRaw([]string{choice.Raw}))
		return setJSONRaw(body, "tool_choice", wrapper.String())
	}
	if choiceType != "allowed_tools" {
		if !xaiToolChoiceMatches(choice, tools) {
			body = deleteJSONPath(body, "tool_choice")
		}
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.IsArray() {
		return deleteJSONPath(body, "tool_choice")
	}
	filtered := make([]string, 0, jsonMemberCount(allowed))
	for _, tool := range allowed.Array() {
		if tool.IsObject() && xaiToolChoiceMatches(tool, tools) {
			filtered = append(filtered, tool.Raw)
		}
	}
	if len(filtered) == 0 {
		return deleteJSONPath(body, "tool_choice")
	}
	return setJSONRaw(body, "tool_choice.tools", joinJSONRaw(filtered))
}

func xaiToolChoiceMatches(choice gjson.Result, tools []string) bool {
	choiceTypeValue := choice.Get("type")
	if choiceTypeValue.Type != gjson.String {
		return false
	}
	choiceType := strings.TrimSpace(choiceTypeValue.String())
	if choiceType == "" || choiceType == "allowed_tools" {
		return false
	}
	choiceName := strings.TrimSpace(jsonStringValue(choice.Get("name")))
	for _, raw := range tools {
		tool := gjson.Parse(raw)
		if !tool.IsObject() {
			continue
		}
		toolType := tool.Get("type")
		if toolType.Type != gjson.String || strings.TrimSpace(toolType.String()) != choiceType {
			continue
		}
		if choiceType != "function" && choiceType != "custom" {
			return true
		}
		toolName := tool.Get("name")
		if choiceName != "" && toolName.Type == gjson.String && choiceName == strings.TrimSpace(toolName.String()) {
			return true
		}
	}
	return false
}

func injectXAIResponsesHeaders(req *http.Request, accessToken, conversationID string) {
	if req == nil {
		return
	}
	for _, name := range []string{
		"Authorization", "X-Api-Key", "x-goog-api-key",
		xaiauth.CLITokenAuthHeader, xaiauth.CLIClientVersionHeader,
		"User-Agent", xaiauth.CLIClientModeHeader,
		"x-grok-client-identifier", "x-authenticateresponse",
		"x-grok-conv-id", "Session-Id", "Session_id", "Originator", "ChatGPT-Account-ID",
	} {
		req.Header.Del(name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set(xaiauth.CLITokenAuthHeader, xaiauth.CLITokenAuthValue)
	req.Header.Set(xaiauth.CLIClientVersionHeader, xaiauth.CLIClientVersion)
	req.Header.Set("User-Agent", xaiauth.CLIUserAgent)
	req.Header.Set(xaiauth.CLIClientModeHeader, xaiauth.CLIClientMode)
	req.Header.Set(xaiauth.CLIClientIdentifierHeader, xaiauth.CLIClientIdentifierValue)
	req.Header.Set(xaiauth.CLIAuthenticateResponseHeader, xaiauth.CLIAuthenticateResponseValue)
	if conversationID = strings.TrimSpace(conversationID); conversationID != "" {
		req.Header.Set("x-grok-conv-id", conversationID)
	}
}

// injectXAIAPIResponsesHeaders builds the standard public API identity used by
// api.x.ai. Hosted tools such as image_generation are unavailable on the Grok
// CLI chat proxy, so its CLI-only headers must not leak onto this request.
func injectXAIAPIResponsesHeaders(req *http.Request, accessToken string) {
	if req == nil {
		return
	}
	for _, name := range []string{
		"Authorization", "X-Api-Key", "x-goog-api-key",
		xaiauth.CLITokenAuthHeader, xaiauth.CLIClientVersionHeader,
		xaiauth.CLIClientModeHeader, xaiauth.CLIClientIdentifierHeader,
		xaiauth.CLIAuthenticateResponseHeader, "x-grok-conv-id",
		"Session-Id", "Session_id", "Originator", "ChatGPT-Account-ID",
	} {
		req.Header.Del(name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("User-Agent", xaiauth.CLIUserAgent)
}

func deriveXAIExecutionID(subject string, headers http.Header) string {
	subject = strings.TrimSpace(subject)
	sessionID := responsesExecutionSessionID(headers)
	if sessionID != "" {
		sessionID = "responses:" + sessionID
	} else if claudeSessionID := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id")); claudeSessionID != "" {
		sessionID = "claude:" + claudeSessionID
		if threadID := strings.TrimSpace(headers.Get("Thread-Id")); threadID != "" {
			sessionID += "\x00thread\x00" + threadID
		}
	}
	if subject == "" || sessionID == "" {
		return ""
	}
	return util.NewUUIDv5(util.NameSpaceOID, "ccload:xai:execution:"+subject+"\x00"+sessionID)
}

func deriveXAIExecutionIDForRequest(reqCtx *proxyRequestContext) string {
	if reqCtx == nil {
		return ""
	}
	if stable := deriveXAIExecutionID(reqCtx.tokenHash, reqCtx.header); stable != "" {
		return stable
	}
	if reqCtx.routingSession != nil {
		if sessionKey := strings.TrimSpace(reqCtx.routingSession.storeKey); sessionKey != "" {
			return util.NewUUIDv5(
				util.NameSpaceOID,
				"ccload:xai:execution:"+strings.TrimSpace(reqCtx.tokenHash)+"\x00session\x00"+sessionKey,
			)
		}
	}
	seed := strings.TrimSpace(reqCtx.tokenHash) + "\x00" + strconv.FormatInt(reqCtx.startTime.UnixNano(), 10)
	return util.NewUUIDv5(util.NameSpaceOID, "ccload:xai:execution:"+seed)
}

func xaiCredentialRejected(status int, headers http.Header, body []byte) bool {
	return xaiauth.ClassifyBillingResponse(status, headers, body) == xaiauth.BillingBadCredential
}
