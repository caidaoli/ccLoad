package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	antigravityclaude "ccLoad/internal/protocol/cliproxy/providers/antigravity/claude"
	antigravitygemini "ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"
	antigravitychat "ccLoad/internal/protocol/cliproxy/providers/antigravity/openai/chat-completions"
	antigravityresponses "ccLoad/internal/protocol/cliproxy/providers/antigravity/openai/responses"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

const (
	zeroWidthSpace                    = "\u200B"
	antigravityWebSearchFallbackModel = "gemini-2.5-flash"
	antigravityBaseURLFallbackDelay   = time.Second
	antigravityModelCapacityAttempts  = 3
	antigravityIdentityPrompt         = `<identity>
You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.
You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.
The USER will send you requests, which you must always prioritize addressing. Along with each USER request, we will attach additional metadata about their current state, such as what files they have open and where their cursor is.
This information may or may not be relevant to the coding task, it is up for you to decide.
</identity>
<communication_style>
- **Proactiveness**. As an agent, you are allowed to be proactive, but only in the course of completing the user's task. For example, if the user asks you to add a new component, you can edit the code, verify build and test statuses, and take any other obvious follow-up actions, such as performing additional research. However, avoid surprising the user. For example, if the user asks HOW to approach something, you should answer their question and instead of jumping into editing a file.</communication_style>`
)

func buildAntigravitySensitiveWordMatcher(words []string) *regexp.Regexp {
	valid := make([]string, 0, len(words))
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		key := strings.ToLower(word)
		if utf8.RuneCountInString(word) < 2 || strings.Contains(word, zeroWidthSpace) {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		valid = append(valid, word)
	}
	if len(valid) == 0 {
		return nil
	}
	slices.SortFunc(valid, func(a, b string) int { return len(b) - len(a) })
	escaped := make([]string, len(valid))
	for i, word := range valid {
		escaped[i] = regexp.QuoteMeta(word)
	}
	matcher, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return nil
	}
	return matcher
}

func translateAntigravityRequest(clientProtocol protocol.Protocol, modelName string, body []byte, stream bool) ([]byte, error) {
	var translated []byte
	switch clientProtocol {
	case protocol.Anthropic:
		translated = antigravityclaude.ConvertClaudeRequestToAntigravity(modelName, body, stream)
	case protocol.Codex:
		translated = antigravityresponses.ConvertOpenAIResponsesRequestToAntigravity(modelName, body, stream)
	case protocol.OpenAI:
		translated = antigravitychat.ConvertOpenAIRequestToAntigravity(modelName, body, stream)
	case protocol.Gemini:
		translated = antigravitygemini.ConvertGeminiRequestToAntigravity(modelName, body, stream)
	default:
		return nil, &protocol.RequestTranslationError{From: clientProtocol, To: protocol.Gemini, Err: fmt.Errorf("unsupported Antigravity client protocol")}
	}
	if !gjson.ValidBytes(translated) {
		return nil, &protocol.RequestTranslationError{From: clientProtocol, To: protocol.Gemini, Err: fmt.Errorf("antigravity adapter produced invalid JSON")}
	}
	return translated, nil
}

func translateAntigravityResponseNonStream(
	ctx context.Context,
	clientProtocol protocol.Protocol,
	modelName string,
	originalRequest, translatedRequest, response []byte,
) ([]byte, error) {
	state := any(nil)
	var translated []byte
	switch clientProtocol {
	case protocol.Anthropic:
		translated = antigravityclaude.ConvertAntigravityResponseToClaudeNonStream(ctx, modelName, originalRequest, translatedRequest, response, &state)
	case protocol.Codex:
		translated = antigravityresponses.ConvertAntigravityResponseToOpenAIResponsesNonStream(ctx, modelName, originalRequest, translatedRequest, response, &state)
	case protocol.OpenAI:
		translated = antigravitychat.ConvertAntigravityResponseToOpenAINonStream(ctx, modelName, originalRequest, translatedRequest, response, &state)
	case protocol.Gemini:
		translated = antigravitygemini.ConvertAntigravityResponseToGeminiNonStream(ctx, modelName, originalRequest, translatedRequest, response, &state)
	default:
		return nil, fmt.Errorf("unsupported Antigravity client protocol %q", clientProtocol)
	}
	if !gjson.ValidBytes(translated) {
		return nil, fmt.Errorf("antigravity %s response adapter produced invalid JSON", clientProtocol)
	}
	return translated, nil
}

func translateAntigravityResponseStream(
	ctx context.Context,
	clientProtocol protocol.Protocol,
	modelName string,
	originalRequest, translatedRequest, response []byte,
	state *any,
) ([][]byte, error) {
	var chunks [][]byte
	switch clientProtocol {
	case protocol.Anthropic:
		chunks = antigravityclaude.ConvertAntigravityResponseToClaude(ctx, modelName, originalRequest, translatedRequest, response, state)
	case protocol.Codex:
		chunks = antigravityresponses.ConvertAntigravityResponseToOpenAIResponses(ctx, modelName, originalRequest, translatedRequest, response, state)
	case protocol.OpenAI:
		chunks = antigravitychat.ConvertAntigravityResponseToOpenAI(ctx, modelName, originalRequest, translatedRequest, response, state)
	case protocol.Gemini:
		// The upstream Antigravity executor always installs this legacy context
		// value. Its Gemini stream converter treats a missing key as no output.
		//nolint:staticcheck // match the synchronized converter's public contract
		ctx = context.WithValue(ctx, "alt", "")
		chunks = antigravitygemini.ConvertAntigravityResponseToGemini(ctx, modelName, originalRequest, translatedRequest, response, state)
	default:
		return nil, fmt.Errorf("unsupported Antigravity client protocol %q", clientProtocol)
	}
	return frameAntigravityStreamChunks(chunks), nil
}

func frameAntigravityStreamChunks(chunks [][]byte) [][]byte {
	framed := make([][]byte, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = bytes.TrimSpace(chunk)
		if len(chunk) == 0 {
			continue
		}
		if bytes.HasPrefix(chunk, []byte("event:")) || bytes.HasPrefix(chunk, []byte("data:")) {
			event := append([]byte(nil), chunk...)
			event = append(event, '\n', '\n')
			framed = append(framed, event)
			continue
		}
		event := make([]byte, 0, len(chunk)+8)
		event = append(event, "data: "...)
		event = append(event, chunk...)
		event = append(event, '\n', '\n')
		framed = append(framed, event)
	}
	return framed
}

func antigravitySSEData(event []byte) ([]byte, error) {
	normalized := bytes.ReplaceAll(event, []byte("\r\n"), []byte("\n"))
	for _, line := range bytes.Split(normalized, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if len(data) > 0 {
			return data, nil
		}
	}
	return nil, errors.New("stream: Antigravity SSE event is missing data")
}

func prepareAntigravityRequestBody(
	cfg *model.Config,
	modelName string,
	body []byte,
	sourceBody []byte,
	headers http.Header,
	matcher *regexp.Regexp,
) ([]byte, error) {
	if cfg == nil || !cfg.UsesAntigravityOAuth() {
		return body, nil
	}
	if strings.TrimSpace(cfg.AntigravityProjectID) == "" {
		return nil, errors.New("request: Antigravity credential is missing project_id")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Antigravity Gemini request: %w", err)
	}
	request := payload
	if nested, ok := payload["request"].(map[string]any); ok {
		request = nested
	}
	delete(request, "model")
	if instruction, exists := request["system_instruction"]; exists {
		if _, camelExists := request["systemInstruction"]; !camelExists {
			request["systemInstruction"] = instruction
		}
		delete(request, "system_instruction")
	}
	injectAntigravityIdentityPrompt(request)
	delete(request, "safetySettings")
	normalizeAntigravityContentsRoles(request)
	restoreAntigravityAnthropicToolIDs(request, sourceBody)
	normalizeAntigravitySchemas(request, modelName)
	normalizeAntigravityThinkingLevel(request)
	if strings.Contains(strings.ToLower(modelName), "claude") {
		ensureAntigravityValidatedToolMode(request)
	} else if generationConfig, ok := request["generationConfig"].(map[string]any); ok {
		delete(generationConfig, "maxOutputTokens")
	}

	requestType := "agent"
	requestID := "agent-" + util.NewUUIDv4()
	if hasAntigravityWebSearchTool(sourceBody) || hasAntigravityWebSearchTool(body) {
		requestType = "web_search"
		modelName = antigravityWebSearchFallbackModel
	} else if strings.Contains(strings.ToLower(modelName), "image") {
		requestType = "image_gen"
		requestID = fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), util.NewUUIDv4())
	}
	if requestType != "image_gen" {
		if _, exists := request["sessionId"]; !exists {
			request["sessionId"] = antigravitySessionID(headers, sourceBody, body)
		}
	}
	envelope := map[string]any{
		"project":     cfg.AntigravityProjectID,
		"request":     request,
		"model":       strings.TrimSpace(modelName),
		"userAgent":   "antigravity",
		"requestType": requestType,
		"requestId":   requestID,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode Antigravity request: %w", err)
	}
	return obfuscateAntigravitySystemInstruction(raw, matcher), nil
}

// normalizeAntigravityThinkingLevel replaces client-facing effort aliases that
// are not valid Antigravity ThinkingLevel enum values. CLIProxyAPI normally does
// this in its excluded runtime ApplyThinking stage, so ccLoad must enforce the
// provider wire contract at the shared finalization boundary.
func normalizeAntigravityThinkingLevel(request map[string]any) {
	for _, generationConfigKey := range []string{"generationConfig", "generation_config"} {
		generationConfig, _ := request[generationConfigKey].(map[string]any)
		if generationConfig == nil {
			continue
		}
		for _, thinkingConfigKey := range []string{"thinkingConfig", "thinking_config"} {
			thinkingConfig, _ := generationConfig[thinkingConfigKey].(map[string]any)
			if thinkingConfig == nil {
				continue
			}
			levelKey := "thinkingLevel"
			level, _ := thinkingConfig[levelKey].(string)
			if level == "" {
				levelKey = "thinking_level"
				level, _ = thinkingConfig[levelKey].(string)
			}
			switch normalized := strings.ToLower(strings.TrimSpace(level)); normalized {
			case "minimal":
				thinkingConfig[levelKey] = "low"
			case "xhigh", "max":
				thinkingConfig[levelKey] = "high"
			case "low", "medium", "high":
				thinkingConfig[levelKey] = normalized
			}
		}
	}
}

func restoreAntigravityAnthropicToolIDs(request map[string]any, sourceBody []byte) {
	messages := gjson.GetBytes(sourceBody, "messages")
	if !messages.IsArray() {
		return
	}

	var callIDs, responseIDs []string
	messages.ForEach(func(_, message gjson.Result) bool {
		parts := message.Get("content")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "tool_use":
				if id := part.Get("id").String(); id != "" {
					callIDs = append(callIDs, id)
				}
			case "tool_result":
				if id := part.Get("tool_use_id").String(); id != "" {
					responseIDs = append(responseIDs, id)
				}
			}
			return true
		})
		return true
	})

	var calls, responses []map[string]any
	contents, _ := request["contents"].([]any)
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if call, ok := part["functionCall"].(map[string]any); ok {
				calls = append(calls, call)
			}
			if response, ok := part["functionResponse"].(map[string]any); ok {
				responses = append(responses, response)
			}
		}
	}

	restoreAntigravityToolIDs(calls, callIDs)
	restoreAntigravityToolIDs(responses, responseIDs)
}

func restoreAntigravityToolIDs(parts []map[string]any, ids []string) {
	if len(parts) != len(ids) {
		return
	}
	for i, part := range parts {
		if id, _ := part["id"].(string); id == "" {
			part["id"] = ids[i]
		}
	}
}

func injectAntigravityIdentityPrompt(request map[string]any) {
	if request == nil || antigravitySystemInstructionContainsIdentity(request["systemInstruction"]) {
		return
	}
	identityPart := map[string]any{"text": antigravityIdentityPrompt}
	switch instruction := request["systemInstruction"].(type) {
	case map[string]any:
		parts, _ := instruction["parts"].([]any)
		instruction["parts"] = append([]any{identityPart}, parts...)
	case string:
		parts := []any{identityPart}
		if instruction != "" {
			parts = append(parts, map[string]any{"text": instruction})
		}
		request["systemInstruction"] = map[string]any{"parts": parts}
	default:
		request["systemInstruction"] = map[string]any{"parts": []any{identityPart}}
	}
}

func antigravitySystemInstructionContainsIdentity(instruction any) bool {
	containsIdentity := func(text string) bool {
		return strings.Contains(strings.ReplaceAll(text, zeroWidthSpace, ""), "You are Antigravity")
	}
	switch value := instruction.(type) {
	case string:
		return containsIdentity(value)
	case map[string]any:
		parts, _ := value["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if text, _ := part["text"].(string); containsIdentity(text) {
				return true
			}
		}
	}
	return false
}

func hasAntigravityWebSearchTool(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	tools, _ := payload["tools"].([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if isAntigravityWebSearchName(tool["type"]) || isAntigravityWebSearchName(tool["name"]) {
			return true
		}
		if _, ok := tool["googleSearch"]; ok {
			return true
		}
		if _, ok := tool["google_search"]; ok {
			return true
		}
		function, _ := tool["function"].(map[string]any)
		if isAntigravityWebSearchName(function["name"]) {
			return true
		}
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			declarations, _ := tool[key].([]any)
			for _, rawDeclaration := range declarations {
				declaration, _ := rawDeclaration.(map[string]any)
				if isAntigravityWebSearchName(declaration["name"]) {
					return true
				}
			}
		}
	}
	return false
}

func isAntigravityWebSearchName(value any) bool {
	name, _ := value.(string)
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, "web_search") || name == "google_search"
}

func normalizeAntigravityContentsRoles(request map[string]any) {
	contents, _ := request["contents"].([]any)
	previousRole := ""
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		role, _ := content["role"].(string)
		if role != "user" && role != "model" {
			if previousRole == "" || previousRole == "model" {
				role = "user"
			} else {
				role = "model"
			}
			content["role"] = role
		}
		previousRole = role
	}
}

func normalizeAntigravitySchemas(request map[string]any, modelName string) {
	useAntigravitySchema := strings.Contains(strings.ToLower(modelName), "claude") ||
		strings.Contains(strings.ToLower(modelName), "gemini-3-pro") ||
		strings.Contains(strings.ToLower(modelName), "gemini-3.1-pro")
	tools, _ := request["tools"].([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			declarations, _ := tool[key].([]any)
			for _, rawDeclaration := range declarations {
				declaration, _ := rawDeclaration.(map[string]any)
				parameters, exists := firstAntigravityMapValue(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema")
				if exists {
					declaration["parameters"] = cleanAntigravitySchema(parameters, useAntigravitySchema, false)
					delete(declaration, "parametersJsonSchema")
					delete(declaration, "parameters_json_schema")
				}
				for _, schemaKey := range []string{"response", "responseJsonSchema", "response_json_schema"} {
					if schema, ok := declaration[schemaKey].(map[string]any); ok {
						declaration[schemaKey] = cleanAntigravitySchema(schema, useAntigravitySchema, false)
					}
				}
			}
		}
	}
	for _, configKey := range []string{"generationConfig", "generation_config"} {
		config, _ := request[configKey].(map[string]any)
		for _, schemaKey := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"} {
			if schema, ok := config[schemaKey].(map[string]any); ok {
				config[schemaKey] = cleanAntigravitySchema(schema, true, true)
			}
		}
	}
}

func firstAntigravityMapValue(values map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := values[key].(map[string]any); ok {
			return value, true
		}
	}
	return nil, false
}

func cleanAntigravitySchema(schema map[string]any, antigravity, response bool) map[string]any {
	input := any(schema)
	if !response {
		input = map[string]any{"schema": schema}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return schema
	}
	cleaned := ""
	switch {
	case response:
		cleaned = cliproxyutil.CleanJSONSchemaForAntigravityResponse(string(raw))
	case antigravity:
		cleaned = cliproxyutil.CleanJSONSchemaForAntigravity(string(raw))
	default:
		cleaned = cliproxyutil.CleanJSONSchemaForGemini(string(raw))
	}
	var result map[string]any
	if json.Unmarshal([]byte(cleaned), &result) != nil {
		return schema
	}
	if !response {
		if nested, ok := result["schema"].(map[string]any); ok {
			return nested
		}
		return schema
	}
	return result
}

func ensureAntigravityValidatedToolMode(request map[string]any) {
	toolConfig, _ := request["toolConfig"].(map[string]any)
	if toolConfig == nil {
		toolConfig = make(map[string]any)
		request["toolConfig"] = toolConfig
	}
	functionCallingConfig, _ := toolConfig["functionCallingConfig"].(map[string]any)
	if functionCallingConfig == nil {
		functionCallingConfig = make(map[string]any)
		toolConfig["functionCallingConfig"] = functionCallingConfig
	}
	functionCallingConfig["mode"] = "VALIDATED"
}

func antigravitySessionID(headers http.Header, sourceBody, body []byte) string {
	seed := ""
	for _, name := range []string{"Session-Id", "Session_id", "X-Claude-Code-Session-Id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			seed = value
			break
		}
	}
	if seed == "" {
		seed = anthropicSessionIDFromBody(sourceBody)
	}
	if seed == "" {
		for _, path := range []string{"session_id", "sessionId", "conversation_id", "prompt_cache_key"} {
			if value := strings.TrimSpace(gjson.GetBytes(sourceBody, path).String()); value != "" {
				seed = value
				break
			}
		}
	}
	if seed == "" {
		for _, path := range []string{"contents", "request.contents"} {
			for _, content := range gjson.GetBytes(body, path).Array() {
				if content.Get("role").String() != "user" {
					continue
				}
				if text := content.Get("parts.0.text").String(); text != "" {
					seed = text
					break
				}
			}
			if seed != "" {
				break
			}
		}
	}
	if seed == "" {
		seed = util.NewUUIDv4()
	}
	if threadID := strings.TrimSpace(headers.Get("Thread-Id")); threadID != "" {
		seed += "\x00thread\x00" + threadID
	}
	return antigravityNegativeSessionID(seed)
}

func antigravityNegativeSessionID(seed string) string {
	seed = strings.TrimSpace(seed)
	if strings.HasPrefix(seed, "-") {
		if _, err := strconv.ParseUint(strings.TrimPrefix(seed, "-"), 10, 63); err == nil {
			return seed
		}
	}
	digest := sha256.Sum256([]byte(seed))
	value := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff)
	return "-" + strconv.FormatInt(value, 10)
}

func obfuscateAntigravitySystemInstruction(body []byte, matcher *regexp.Regexp) []byte {
	if matcher == nil {
		return body
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return body
	}
	request, _ := envelope["request"].(map[string]any)
	instruction, exists := request["systemInstruction"]
	if !exists {
		return body
	}
	switch value := instruction.(type) {
	case string:
		request["systemInstruction"] = obfuscateAntigravityText(value, matcher)
	case map[string]any:
		parts, _ := value["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if text, ok := part["text"].(string); ok {
				part["text"] = obfuscateAntigravityText(text, matcher)
			}
		}
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return raw
}

func obfuscateAntigravityText(text string, matcher *regexp.Regexp) string {
	return matcher.ReplaceAllStringFunc(text, func(word string) string {
		if strings.Contains(word, zeroWidthSpace) {
			return word
		}
		_, size := utf8.DecodeRuneInString(word)
		if size <= 0 || size >= len(word) {
			return word
		}
		return word[:size] + zeroWidthSpace + word[size:]
	})
}

func antigravityUpstreamURL(baseURL string, streaming bool) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(model.StripExactUpstreamURLMarker(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid Antigravity base URL")
	}
	if streaming {
		parsed.Path = "/v1internal:streamGenerateContent"
		parsed.RawQuery = "alt=sse"
	} else {
		parsed.Path = "/v1internal:generateContent"
		parsed.RawQuery = ""
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isAntigravityCountTokensPath(path string) bool {
	return strings.Contains(strings.TrimSpace(path), ":countTokens")
}

func isAntigravityDefaultBaseURL(rawURL string) bool {
	baseURL := strings.TrimRight(strings.TrimSpace(model.StripExactUpstreamURLMarker(rawURL)), "/")
	switch baseURL {
	case antigravityDailyBaseURL, antigravityProdBaseURL, antigravitySandboxDailyBaseURL:
		return true
	default:
		return false
	}
}

func usesAntigravityDefaultBaseURLs(urls model.ChannelURLs) bool {
	if len(urls) == 0 {
		return false
	}
	for _, entry := range urls {
		if !isAntigravityDefaultBaseURL(entry.URL) {
			return false
		}
	}
	return true
}

func withAntigravityDefaultFallbackURLs(cfg *model.Config) *model.Config {
	if cfg == nil || !cfg.UsesAntigravityOAuth() {
		return cfg
	}
	if !usesAntigravityDefaultBaseURLs(cfg.URLs) {
		return cfg
	}
	runtimeCfg := cfg.Clone()
	runtimeCfg.URLs = antigravityOAuthDefaultURLs()
	return runtimeCfg
}

type antigravityGoogleRPCErrorEnvelope struct {
	Error struct {
		Status  string `json:"status"`
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

func isAntigravityModelCapacityExhausted(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable || len(body) == 0 {
		return false
	}
	var envelope antigravityGoogleRPCErrorEnvelope
	if json.Unmarshal(body, &envelope) != nil || envelope.Error.Status != "UNAVAILABLE" {
		return false
	}
	for _, detail := range envelope.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" &&
			detail.Reason == "MODEL_CAPACITY_EXHAUSTED" {
			return true
		}
	}
	return false
}

func shouldFallbackAntigravityBaseURL(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusNotFound, http.StatusTooManyRequests:
		return true
	case http.StatusServiceUnavailable:
		return isAntigravityModelCapacityExhausted(statusCode, body) ||
			strings.Contains(strings.ToLower(string(body)), "no capacity available")
	default:
		return false
	}
}

func injectAntigravityOAuthHeaders(req *http.Request, cfg *model.Config, userAgent string) {
	if req == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return
	}
	req.Header = make(http.Header, 3)
	req.Header.Set("Authorization", "Bearer "+cfg.AntigravityAccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", strings.TrimSpace(userAgent))
}

func unwrapAntigravityResponse(raw []byte) ([]byte, error) {
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode Antigravity response: %w", err)
	}
	if len(envelope.Response) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Response), []byte("null")) {
		return nil, errors.New("response: Antigravity payload is missing response")
	}
	return envelope.Response, nil
}

func unwrapAntigravityRequest(raw []byte) ([]byte, error) {
	var envelope struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode Antigravity request envelope: %w", err)
	}
	if len(envelope.Request) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Request), []byte("null")) {
		return nil, errors.New("request: Antigravity envelope is missing request")
	}
	return envelope.Request, nil
}

func unwrapAntigravitySSEEvent(event []byte) ([]byte, error) {
	normalized := bytes.ReplaceAll(event, []byte("\r\n"), []byte("\n"))
	lines := bytes.Split(normalized, []byte("\n"))
	var output bytes.Buffer
	foundData := false
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			output.WriteString("data: [DONE]\n\n")
			foundData = true
			continue
		}
		inner, err := unwrapAntigravityResponse(data)
		if err != nil {
			return nil, err
		}
		output.WriteString("data: ")
		output.Write(bytes.TrimSpace(inner))
		output.WriteString("\n\n")
		foundData = true
	}
	if !foundData {
		return nil, errors.New("stream: Antigravity SSE event is missing data")
	}
	return output.Bytes(), nil
}
