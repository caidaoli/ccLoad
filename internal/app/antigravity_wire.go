package app

import (
	"bytes"
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

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"
	"ccLoad/internal/util"
)

const zeroWidthSpace = "\u200B"

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

func prepareAntigravityRequestBody(
	cfg *model.Config,
	modelName string,
	body []byte,
	headers http.Header,
	matcher *regexp.Regexp,
) ([]byte, error) {
	if cfg == nil || !cfg.UsesAntigravityOAuth() {
		return body, nil
	}
	if strings.TrimSpace(cfg.AntigravityProjectID) == "" {
		return nil, errors.New("request: Antigravity credential is missing project_id")
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode Antigravity Gemini request: %w", err)
	}
	delete(request, "model")
	if instruction, exists := request["system_instruction"]; exists {
		if _, camelExists := request["systemInstruction"]; !camelExists {
			request["systemInstruction"] = instruction
		}
		delete(request, "system_instruction")
	}
	delete(request, "safetySettings")
	normalizeAntigravityContentsRoles(request)
	normalizeAntigravitySchemas(request, modelName)
	if strings.Contains(strings.ToLower(modelName), "claude") {
		ensureAntigravityValidatedToolMode(request)
	} else if generationConfig, ok := request["generationConfig"].(map[string]any); ok {
		delete(generationConfig, "maxOutputTokens")
	}

	requestType := "agent"
	requestID := "agent-" + util.NewUUIDv4()
	if strings.Contains(strings.ToLower(modelName), "image") {
		requestType = "image_gen"
		requestID = fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), util.NewUUIDv4())
	}
	if requestType == "agent" {
		if _, exists := request["sessionId"]; !exists {
			request["sessionId"] = antigravitySessionID(headers, body)
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

func antigravitySessionID(headers http.Header, body []byte) string {
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	var request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if json.Unmarshal(body, &request) == nil {
		for _, content := range request.Contents {
			if content.Role != "user" || len(content.Parts) == 0 || content.Parts[0].Text == "" {
				continue
			}
			digest := sha256.Sum256([]byte(content.Parts[0].Text))
			value := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff)
			return "-" + strconv.FormatInt(value, 10)
		}
	}
	digest := sha256.Sum256([]byte(util.NewUUIDv4()))
	return "-" + strconv.FormatUint(binary.BigEndian.Uint64(digest[:8])&0x7fffffffffffffff, 10)
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

func injectAntigravityOAuthHeaders(req *http.Request, cfg *model.Config) {
	if req == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return
	}
	req.Header = make(http.Header, 3)
	req.Header.Set("Authorization", "Bearer "+cfg.AntigravityAccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityauth.DefaultUserAgent)
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
