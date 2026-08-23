package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"
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

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("decode xAI Responses request")
	}
	if payload == nil {
		return nil, errors.New("xAI Responses request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("xAI Responses request contains trailing JSON")
	}

	payload["model"] = actualModel
	payload["stream"] = true
	if executionID = strings.TrimSpace(executionID); executionID != "" {
		payload["prompt_cache_key"] = executionID
	} else {
		delete(payload, "prompt_cache_key")
	}
	for _, field := range []string{
		"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options",
	} {
		delete(payload, field)
	}
	deleteJSONKeyRecursive(payload, "external_web_access")

	if strings.EqualFold(actualModel, "grok-4.5") {
		delete(payload, "presence_penalty")
		delete(payload, "frequency_penalty")
		delete(payload, "stop")
	}
	normalizeXAIReasoning(payload, actualModel)
	normalizeXAIInputReasoningItems(payload)
	normalizeXAIImageGenerationTools(payload, actualModel)
	normalizeXAIOrphanedToolControls(payload)

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode xAI Responses request")
	}
	return encoded, nil
}

func normalizeXAIInputReasoningItems(payload map[string]any) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		if content, exists := item["content"]; exists && content == nil {
			delete(item, "content")
		}
		if encryptedContent, exists := item["encrypted_content"]; exists && encryptedContent == nil {
			delete(item, "encrypted_content")
		}
	}
}

func deleteJSONKeyRecursive(value any, key string) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, key)
		for _, child := range typed {
			deleteJSONKeyRecursive(child, key)
		}
	case []any:
		for _, child := range typed {
			deleteJSONKeyRecursive(child, key)
		}
	}
}

func normalizeXAIReasoning(payload map[string]any, modelName string) {
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		delete(payload, "reasoning")
		return
	}
	allowed := xaiReasoningEfforts(modelName)
	if len(allowed) == 0 {
		delete(payload, "reasoning")
		return
	}
	effort, _ := reasoning["effort"].(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "minimal":
		effort = "low"
	case "xhigh", "max":
		effort = "high"
	}
	if _, valid := allowed[effort]; valid {
		reasoning["effort"] = effort
	} else {
		delete(reasoning, "effort")
	}
	if len(reasoning) == 0 {
		delete(payload, "reasoning")
	}
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

func normalizeXAIImageGenerationTools(payload map[string]any, modelName string) {
	tools := promoteXAIAdditionalTools(payload)
	supported := xaiSupportsImageGeneration(modelName)
	filtered := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, isObject := rawTool.(map[string]any)
		isImageGeneration := isObject && strings.TrimSpace(xaiStringValue(tool["type"])) == xaiImageGenerationToolType
		if isImageGeneration && !supported {
			continue
		}
		filtered = append(filtered, rawTool)
	}
	payload["tools"] = filtered
	normalizeXAIToolChoice(payload, filtered)
}

func promoteXAIAdditionalTools(payload map[string]any) []any {
	tools, _ := payload["tools"].([]any)
	input, ok := payload["input"].([]any)
	if !ok {
		return tools
	}
	promoted := append([]any(nil), tools...)
	remaining := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, isObject := rawItem.(map[string]any)
		if !isObject || strings.TrimSpace(xaiStringValue(item["type"])) != "additional_tools" {
			remaining = append(remaining, rawItem)
			continue
		}
		additional, _ := item["tools"].([]any)
		promoted = append(promoted, additional...)
	}
	if len(remaining) != len(input) {
		payload["input"] = remaining
	}
	return promoted
}

func normalizeXAIToolChoice(payload map[string]any, tools []any) {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if strings.TrimSpace(xaiStringValue(choice["type"])) == xaiImageGenerationToolType {
		if !xaiToolChoiceMatches(choice, tools) {
			delete(payload, "tool_choice")
			return
		}
		payload["tool_choice"] = map[string]any{
			"type":  "allowed_tools",
			"mode":  "required",
			"tools": []any{choice},
		}
		choice = payload["tool_choice"].(map[string]any)
	}
	if strings.TrimSpace(xaiStringValue(choice["type"])) != "allowed_tools" {
		if !xaiToolChoiceMatches(choice, tools) {
			delete(payload, "tool_choice")
		}
		return
	}
	allowed, ok := choice["tools"].([]any)
	if !ok {
		delete(payload, "tool_choice")
		return
	}
	filtered := make([]any, 0, len(allowed))
	for _, rawTool := range allowed {
		tool, isObject := rawTool.(map[string]any)
		if !isObject || !xaiToolChoiceMatches(tool, tools) {
			continue
		}
		filtered = append(filtered, rawTool)
	}
	if len(filtered) == 0 {
		delete(payload, "tool_choice")
		return
	}
	choice["tools"] = filtered
}

func xaiToolChoiceMatches(choice map[string]any, tools []any) bool {
	choiceType := strings.TrimSpace(xaiStringValue(choice["type"]))
	if choiceType == "" || choiceType == "allowed_tools" {
		return false
	}
	choiceName := strings.TrimSpace(xaiStringValue(choice["name"]))
	for _, rawTool := range tools {
		tool, isObject := rawTool.(map[string]any)
		if !isObject || strings.TrimSpace(xaiStringValue(tool["type"])) != choiceType {
			continue
		}
		named := choiceType == "function" || choiceType == "custom"
		if !named || choiceName != "" && choiceName == strings.TrimSpace(xaiStringValue(tool["name"])) {
			return true
		}
	}
	return false
}

func xaiStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func normalizeXAIOrphanedToolControls(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if ok && len(tools) > 0 {
		return
	}
	delete(payload, "tools")
	delete(payload, "tool_choice")
	delete(payload, "parallel_tool_calls")
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
