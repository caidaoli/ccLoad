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

func finalizeXAIResponsesBody(body []byte, actualModel, executionID, baseURL string) ([]byte, error) {
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
	normalizeXAIWebSearch(payload, baseURL)

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

func normalizeXAIWebSearch(payload map[string]any, baseURL string) {
	if !isXAICLIChatProxyBaseURL(baseURL) {
		normalizeXAIOrphanedToolControls(payload)
		return
	}
	if !hasXAIResponsesToolIntent(payload) {
		cacheKey, _ := payload["prompt_cache_key"].(string)
		if strings.TrimSpace(cacheKey) == "" {
			normalizeXAIOrphanedToolControls(payload)
			return
		}
		payload["tools"] = []any{
			map[string]any{"type": "web_search"},
			map[string]any{"type": "x_search"},
		}
		payload["tool_choice"] = "none"
		return
	}

	rawTools, toolsExist := payload["tools"]
	tools, toolsAreArray := rawTools.([]any)
	if toolsExist && !toolsAreArray {
		return
	}

	tools = normalizeXAIWebSearchTools(tools)
	cacheKey, _ := payload["prompt_cache_key"].(string)
	if len(tools) > 0 && strings.TrimSpace(cacheKey) != "" {
		tools = appendMissingXAINativeSearchTools(tools)
	}
	payload["tools"] = tools
	normalizeXAIWebSearchToolChoice(payload, strings.TrimSpace(cacheKey) != "")
	normalizeXAIOrphanedToolControls(payload)
}

func hasXAIResponsesToolIntent(payload map[string]any) bool {
	if _, exists := payload["tools"]; exists {
		return true
	}
	if _, exists := payload["tool_choice"]; exists {
		return true
	}
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if strings.TrimSpace(itemType) != "additional_tools" {
			continue
		}
		rawTools, exists := item["tools"]
		tools, isArray := rawTools.([]any)
		if !exists || !isArray || len(tools) > 0 {
			return true
		}
	}
	return false
}

func isXAICLIChatProxyBaseURL(baseURL string) bool {
	normalize := func(value string) string {
		return strings.TrimRight(strings.TrimSpace(value), "/")
	}
	return normalize(baseURL) == normalize(xaiauth.CLIBaseURL)
}

func normalizeXAIWebSearchTools(tools []any) []any {
	kept := make([]any, 0, len(tools))
	seenNativeSearch := make(map[string]bool, 2)
	for _, rawTool := range tools {
		tool, isObject := rawTool.(map[string]any)
		if !isObject {
			kept = append(kept, rawTool)
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType == "web_search" || toolType == "x_search" {
			if seenNativeSearch[toolType] {
				continue
			}
			seenNativeSearch[toolType] = true
			kept = append(kept, rawTool)
			continue
		}
		kept = append(kept, rawTool)
	}
	return kept
}

func appendMissingXAINativeSearchTools(tools []any) []any {
	present := make(map[string]bool, 2)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType == "web_search" || toolType == "x_search" {
			present[toolType] = true
		}
	}
	for _, toolType := range []string{"web_search", "x_search"} {
		if !present[toolType] {
			tools = append(tools, map[string]any{"type": toolType})
		}
	}
	return tools
}

func normalizeXAIWebSearchToolChoice(payload map[string]any, enableCacheRoute bool) {
	choice, isObject := payload["tool_choice"].(map[string]any)
	if !isObject {
		return
	}
	choiceType, _ := choice["type"].(string)
	if choiceType != "allowed_tools" {
		return
	}
	rawAllowed, exists := choice["tools"]
	allowed, isArray := rawAllowed.([]any)
	if !exists || !isArray {
		return
	}
	allowed = normalizeXAIWebSearchTools(allowed)
	if enableCacheRoute && len(allowed) > 0 {
		allowed = appendMissingXAINativeSearchTools(allowed)
	}
	choice["tools"] = allowed
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
		"User-Agent", xaiauth.CLIClientIdentifierHeader, xaiauth.CLIAuthenticateResponseHeader,
		"x-grok-conv-id", "Session-Id", "Session_id", "Originator", "ChatGPT-Account-ID",
	} {
		req.Header.Del(name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set(xaiauth.CLITokenAuthHeader, xaiauth.CLITokenAuthValue)
	req.Header.Set(xaiauth.CLIClientVersionHeader, xaiauth.CLIClientVersion)
	req.Header.Set("User-Agent", xaiauth.CLIUserAgent)
	req.Header.Set(xaiauth.CLIClientIdentifierHeader, xaiauth.CLIClientIdentifier)
	req.Header.Set(xaiauth.CLIAuthenticateResponseHeader, xaiauth.CLIAuthenticateResponse)
	if conversationID = strings.TrimSpace(conversationID); conversationID != "" {
		req.Header.Set("x-grok-conv-id", conversationID)
	}
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
