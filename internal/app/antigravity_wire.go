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
	"github.com/tidwall/sjson"
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
	if !isMutableJSONObject(body) {
		return nil, errors.New("decode Antigravity Gemini request: body is not an object")
	}

	// 如果 body 是 {"request":{...}} 信封，取出内层作为操作目标。
	request := body
	if gjson.GetBytes(body, "request").IsObject() {
		request = []byte(gjson.GetBytes(body, "request").Raw)
	}

	request = deleteJSONPath(request, "model")

	// system_instruction → systemInstruction（snake_case 到 camelCase）
	if sysInst := gjson.GetBytes(request, "system_instruction"); sysInst.Exists() {
		if !gjson.GetBytes(request, "systemInstruction").Exists() {
			request = setJSONRaw(request, "systemInstruction", sysInst.Raw)
		}
		request = deleteJSONPath(request, "system_instruction")
	}

	request = injectAntigravityIdentityPrompt(request)
	request = deleteJSONPath(request, "safetySettings")
	request = normalizeAntigravityContentsRoles(request)
	request = restoreAntigravityAnthropicToolIDs(request, sourceBody)
	request = normalizeAntigravitySchemas(request, modelName)
	request = normalizeAntigravityThinkingLevel(request)
	if strings.Contains(strings.ToLower(modelName), "claude") {
		request = ensureAntigravityValidatedToolMode(request)
	} else {
		if gjson.GetBytes(request, "generationConfig").IsObject() {
			request = deleteJSONPath(request, "generationConfig.maxOutputTokens")
		}
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
		if !gjson.GetBytes(request, "sessionId").Exists() {
			request = setJSONValue(request, "sessionId", antigravitySessionID(headers, sourceBody, body))
		}
	}

	envelope := struct {
		Project     string          `json:"project"`
		Request     json.RawMessage `json:"request"`
		Model       string          `json:"model"`
		UserAgent   string          `json:"userAgent"`
		RequestType string          `json:"requestType"`
		RequestID   string          `json:"requestId"`
	}{
		Project: cfg.AntigravityProjectID, Request: json.RawMessage(request),
		Model: strings.TrimSpace(modelName), UserAgent: "antigravity",
		RequestType: requestType, RequestID: requestID,
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
func normalizeAntigravityThinkingLevel(request []byte) []byte {
	for _, generationConfigKey := range []string{"generationConfig", "generation_config"} {
		if !gjson.GetBytes(request, generationConfigKey).IsObject() {
			continue
		}
		for _, thinkingConfigKey := range []string{"thinkingConfig", "thinking_config"} {
			configPath := generationConfigKey + "." + thinkingConfigKey
			if !gjson.GetBytes(request, configPath).IsObject() {
				continue
			}
			levelKey := "thinkingLevel"
			levelPath := configPath + "." + levelKey
			level := jsonStringValue(gjson.GetBytes(request, levelPath))
			if level == "" {
				levelKey = "thinking_level"
				levelPath = configPath + "." + levelKey
				level = jsonStringValue(gjson.GetBytes(request, levelPath))
			}
			switch normalized := strings.ToLower(strings.TrimSpace(level)); normalized {
			case "minimal":
				request = setJSONValue(request, levelPath, "low")
			case "xhigh", "max":
				request = setJSONValue(request, levelPath, "high")
			case "low", "medium", "high":
				request = setJSONValue(request, levelPath, normalized)
			}
		}
	}
	return request
}

func restoreAntigravityAnthropicToolIDs(request []byte, sourceBody []byte) []byte {
	messages := gjson.GetBytes(sourceBody, "messages")
	if !messages.IsArray() {
		return request
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

	callIndex := 0
	responseIndex := 0
	contents := gjson.GetBytes(request, "contents")
	if !contents.IsArray() {
		return request
	}
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for pi, part := range parts.Array() {
			if !part.IsObject() {
				continue
			}
			if part.Get("functionCall").IsObject() {
				if part.Get("functionCall.id").String() == "" && callIndex < len(callIDs) {
					request = setJSONValue(request,
						fmt.Sprintf("contents.%d.parts.%d.functionCall.id", ci, pi),
						callIDs[callIndex])
				}
				callIndex++
			}
			if part.Get("functionResponse").IsObject() {
				if part.Get("functionResponse.id").String() == "" && responseIndex < len(responseIDs) {
					request = setJSONValue(request,
						fmt.Sprintf("contents.%d.parts.%d.functionResponse.id", ci, pi),
						responseIDs[responseIndex])
				}
				responseIndex++
			}
		}
	}
	return request
}

func injectAntigravityIdentityPrompt(request []byte) []byte {
	if antigravitySystemInstructionContainsIdentity(request) {
		return request
	}
	identityPartRaw := `{"text":` + jsonEscapedString(antigravityIdentityPrompt) + `}`
	instruction := gjson.GetBytes(request, "systemInstruction")
	switch {
	case instruction.IsObject():
		parts := instruction.Get("parts")
		if parts.IsArray() {
			existing := make([]string, 0, len(parts.Array())+1)
			existing = append(existing, identityPartRaw)
			for _, part := range parts.Array() {
				existing = append(existing, part.Raw)
			}
			request = setJSONRaw(request, "systemInstruction.parts", joinJSONRaw(existing))
		} else {
			request = setJSONRaw(request, "systemInstruction.parts", joinJSONRaw([]string{identityPartRaw}))
		}
	case instruction.Type == gjson.String:
		parts := []string{identityPartRaw}
		if text := instruction.String(); text != "" {
			parts = append(parts, `{"text":`+jsonEscapedString(text)+`}`)
		}
		request = setJSONRaw(request, "systemInstruction", `{"parts":`+joinJSONRaw(parts)+`}`)
	default:
		request = setJSONRaw(request, "systemInstruction", `{"parts":`+joinJSONRaw([]string{identityPartRaw})+`}`)
	}
	return request
}

func antigravitySystemInstructionContainsIdentity(request []byte) bool {
	containsIdentity := func(text string) bool {
		return strings.Contains(strings.ReplaceAll(text, zeroWidthSpace, ""), "You are Antigravity")
	}
	instruction := gjson.GetBytes(request, "systemInstruction")
	switch {
	case instruction.Type == gjson.String:
		return containsIdentity(instruction.String())
	case instruction.IsObject():
		for _, part := range instruction.Get("parts").Array() {
			if part.Get("text").Type == gjson.String && containsIdentity(part.Get("text").String()) {
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
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		if isAntigravityWebSearchName(jsonStringValue(tool.Get("type"))) ||
			isAntigravityWebSearchName(jsonStringValue(tool.Get("name"))) {
			return true
		}
		if tool.Get("googleSearch").Exists() || tool.Get("google_search").Exists() {
			return true
		}
		if isAntigravityWebSearchName(jsonStringValue(tool.Get("function.name"))) {
			return true
		}
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			for _, declaration := range tool.Get(key).Array() {
				if isAntigravityWebSearchName(jsonStringValue(declaration.Get("name"))) {
					return true
				}
			}
		}
	}
	return false
}

func isAntigravityWebSearchName(value string) bool {
	name := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(name, "web_search") || name == "google_search"
}

func normalizeAntigravityContentsRoles(request []byte) []byte {
	contents := gjson.GetBytes(request, "contents")
	if !contents.IsArray() {
		return request
	}
	previousRole := ""
	for index, content := range contents.Array() {
		if !content.IsObject() {
			continue
		}
		role := jsonStringValue(content.Get("role"))
		if role != "user" && role != "model" {
			if previousRole == "" || previousRole == "model" {
				role = "user"
			} else {
				role = "model"
			}
			request = setJSONValue(request, fmt.Sprintf("contents.%d.role", index), role)
		}
		previousRole = role
	}
	return request
}

func firstAntigravityJSONObject(request []byte, paths []string) (path string, raw string, ok bool) {
	for _, candidate := range paths {
		if value := gjson.GetBytes(request, candidate); value.IsObject() {
			return candidate, value.Raw, true
		}
	}
	return "", "", false
}

func consolidateAntigravitySchemaAliases(request []byte, canonicalPath string, aliasPaths []string, cleaned string) []byte {
	if cleaned == "" {
		return request
	}
	request = setJSONRaw(request, canonicalPath, cleaned)
	for _, aliasPath := range aliasPaths {
		if aliasPath == canonicalPath {
			continue
		}
		request = deleteJSONPath(request, aliasPath)
	}
	return request
}

func normalizeAntigravitySchemas(request []byte, modelName string) []byte {
	useAntigravitySchema := strings.Contains(strings.ToLower(modelName), "claude") ||
		strings.Contains(strings.ToLower(modelName), "gemini-3-pro") ||
		strings.Contains(strings.ToLower(modelName), "gemini-3.1-pro")
	parameterKeys := []string{"parameters", "parametersJsonSchema", "parameters_json_schema"}
	responseKeys := []string{"response", "responseJsonSchema", "response_json_schema"}
	generationConfigKeys := []string{"generationConfig", "generation_config"}
	generationSchemaKeys := []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"}

	tools := gjson.GetBytes(request, "tools")
	if tools.IsArray() {
		for ti, tool := range tools.Array() {
			if !tool.IsObject() {
				continue
			}
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				declarations := tool.Get(key)
				if !declarations.IsArray() {
					continue
				}
				for di, declaration := range declarations.Array() {
					if !declaration.IsObject() {
						continue
					}
					declPath := fmt.Sprintf("tools.%d.%s.%d", ti, key, di)
					paramPaths := make([]string, 0, len(parameterKeys))
					for _, paramKey := range parameterKeys {
						paramPaths = append(paramPaths, declPath+"."+paramKey)
					}
					if _, paramRaw, found := firstAntigravityJSONObject(request, paramPaths); found {
						cleaned := cleanAntigravitySchemaRaw(paramRaw, useAntigravitySchema, false)
						request = consolidateAntigravitySchemaAliases(request, declPath+".parameters", paramPaths, cleaned)
					}
					responsePaths := make([]string, 0, len(responseKeys))
					for _, schemaKey := range responseKeys {
						responsePaths = append(responsePaths, declPath+"."+schemaKey)
					}
					if _, responseRaw, found := firstAntigravityJSONObject(request, responsePaths); found {
						cleaned := cleanAntigravitySchemaRaw(responseRaw, useAntigravitySchema, false)
						request = consolidateAntigravitySchemaAliases(request, declPath+".response", responsePaths, cleaned)
					}
				}
			}
		}
	}

	generationSchemaPaths := make([]string, 0, len(generationConfigKeys)*len(generationSchemaKeys))
	for _, configKey := range generationConfigKeys {
		for _, schemaKey := range generationSchemaKeys {
			generationSchemaPaths = append(generationSchemaPaths, configKey+"."+schemaKey)
		}
	}
	if _, schemaRaw, found := firstAntigravityJSONObject(request, generationSchemaPaths); found {
		cleaned := cleanAntigravitySchemaRaw(schemaRaw, true, true)
		request = consolidateAntigravitySchemaAliases(request, "generationConfig.responseSchema", generationSchemaPaths, cleaned)
	}
	return request
}

func cleanAntigravitySchemaRaw(raw string, antigravity, response bool) string {
	input := raw
	if !response {
		input = `{"schema":` + raw + `}`
	}
	cleaned := ""
	switch {
	case response:
		cleaned = cliproxyutil.CleanJSONSchemaForAntigravityResponse(input)
	case antigravity:
		cleaned = cliproxyutil.CleanJSONSchemaForAntigravity(input)
	default:
		cleaned = cliproxyutil.CleanJSONSchemaForGemini(input)
	}
	if !response {
		if nested := gjson.Get(cleaned, "schema"); nested.IsObject() {
			return nested.Raw
		}
		return raw
	}
	if !gjson.Valid(cleaned) {
		return raw
	}
	return cleaned
}

func ensureAntigravityValidatedToolMode(request []byte) []byte {
	return setJSONValue(request, "toolConfig.functionCallingConfig.mode", "VALIDATED")
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
	if matcher == nil || !gjson.ParseBytes(body).IsObject() {
		return body
	}
	instruction := gjson.GetBytes(body, "request.systemInstruction")
	if !instruction.Exists() {
		return body
	}
	updated := body
	var err error
	switch {
	case instruction.Type == gjson.String:
		updated, err = sjson.SetBytes(updated, "request.systemInstruction", obfuscateAntigravityText(instruction.String(), matcher))
	case instruction.IsObject():
		for index, part := range instruction.Get("parts").Array() {
			if part.Get("text").Type != gjson.String {
				continue
			}
			updated, err = sjson.SetBytes(updated, fmt.Sprintf("request.systemInstruction.parts.%d.text", index), obfuscateAntigravityText(part.Get("text").String(), matcher))
			if err != nil {
				return body
			}
		}
	}
	if err != nil {
		return body
	}
	return updated
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
