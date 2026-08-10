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
