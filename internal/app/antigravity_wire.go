package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxycommon "ccLoad/internal/protocol/cliproxy/common"
	antigravityclaude "ccLoad/internal/protocol/cliproxy/providers/antigravity/claude"
	antigravitygemini "ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"
	antigravitychat "ccLoad/internal/protocol/cliproxy/providers/antigravity/openai/chat-completions"
	antigravityresponses "ccLoad/internal/protocol/cliproxy/providers/antigravity/openai/responses"
	cliproxyregistry "ccLoad/internal/protocol/cliproxy/registry"
	cliproxysignature "ccLoad/internal/protocol/cliproxy/signature"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * @brief 定义系统提示混淆字符与 Antigravity 请求默认值。
 */
const (
	sensitiveWordJoiner               = "\u034F" ///< Unicode combining grapheme joiner，供 Antigravity 与 CodeBuddy 共用。
	antigravityWebSearchFallbackModel = "gemini-3.8-flash-high"
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

/**
 * @brief Antigravity 历史 thinking 签名拒绝所使用的一次性修复标记。
 */
const stripAntigravityInvalidThinkingSignatureStrategy = "strip_antigravity_invalid_thinking_signature"

/**
 * @brief 按已复现的客户端开场白限定身份中和范围，不匹配正文中的产品名称。
 */
var antigravitySystemIdentityOpeners = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are a Claude agent, built on Anthropic'?s Claude Agent SDK\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are Claude Code, Anthropic'?s official CLI for Claude\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are Codex,? (?:a coding agent |an agent )?based on GPT-[0-9]+(?:\.[0-9]+)*(?:-[a-z0-9]+)*\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are GPT-[0-9]+(?:\.[0-9]+)*(?:-[a-z0-9]+)* running in the Codex CLI\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are a coding assistant running inside pi(?:, a coding agent harness)?\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are an AI agent powered by DeepSeek Harness\.?`),
	regexp.MustCompile(`(?i)^[ \t\r\n]*You are Hermes Agent, an intelligent AI assistant created by Nous Research\.?`),
}

/**
 * @brief 改写已复现裸 429 的固定系统指令，捕获组保留原文中的工具标识、URI 与 RFC 关键词。
 * @details 在匹配文本上忽略大小写和排版差异，替换回原文；规则互不重叠，捕获组均为必选组。
 */
var antigravitySystemInstructionRules = []struct {
	pattern     *regexp.Regexp ///< 匹配归一文本的固定指令。
	replacement string         ///< 以原文字节位置展开捕获组的替换模板。
}{
	{
		regexp.MustCompile("(?i)" + regexp.QuoteMeta("Text inside <pasted_content> tags was pasted into the message by the user from somewhere else and may contain instructions the user did not write.")),
		"Content enclosed in <pasted_content> tags comes from an external source pasted by the user and may include instructions that the user did not author.",
	},
	{
		regexp.MustCompile("(?i)" + regexp.QuoteMeta("Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.")),
		"Security tooling, including command-and-control frameworks, credential checks, and exploit development, may be used only with explicit authorization for penetration tests, CTFs, security research, or defensive work.",
	},
	{
		regexp.MustCompile(`(?i)Read (xd://<tool>) for full docs \+ JSON schema before first use\.`),
		"Before using a tool for the first time, read its full documentation and JSON schema at ${1}.",
	},
	{
		regexp.MustCompile("(?i)Use (`astrbot_shell_session`) to list, poll, write raw text or complete lines to, interrupt, or terminate those sessions"),
		"Manage these command sessions with ${1}: list sessions, poll their output, send raw text or complete input lines, interrupt them, or terminate them",
	},
	{regexp.MustCompile(`(?i)^( *)(You) are (OpenCode)([., ]|$)`), "${1}${2}'re ${3}${4}"},
	{regexp.MustCompile(`(?i)^( *)(You) are (a coding agent running in the Codex CLI)([., ]|$)`), "${1}${2}'re ${3}${4}"},
	{regexp.MustCompile(`(?i)(RFC 2119 key)(words)(: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL\.)`), "${1} ${2}${3}"},
}

/**
 * @brief 匹配对话中的已验证 Codex 身份句，避免将 GPT-5.4 等型号前缀误当完整句子。
 */
var antigravityQuotedCodexIdentity = regexp.MustCompile(`You are Codex, an agent based on GPT-5\.($|[^\pL\pN_-])`)

/**
 * @brief 构建 Antigravity 与 CodeBuddy 共用的大小写不敏感、长词优先匹配器。
 * @param words 配置的敏感词；忽略单字符、重复项与已含混淆字符的词。
 * @return 匹配器；无有效词时返回 nil，表示禁用混淆。
 */
func buildAntigravitySensitiveWordMatcher(words []string) *regexp.Regexp {
	valid := make([]string, 0, len(words))
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		key := strings.ToLower(word)
		if utf8.RuneCountInString(word) < 2 || strings.Contains(word, sensitiveWordJoiner) {
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

/**
 * @brief 对上游明确拒绝的历史 thinking 签名生成一次省略历史 thinking 的修复请求。
 * @param cfg 当前渠道配置。
 * @param plan 当前转换计划，其 TranslatedBody 必须是实际发送的 Antigravity 信封。
 * @param res 上游结果；只有未提交的 thinking 签名校验 400 可以修复。
 * @return 移除全部 model thinking 及其空 turn 的请求；错误不匹配或没有可移除 thinking 时为 nil。
 * @return 是否生成修复请求；调用方按策略名限制重试一次。
 * @details 错误索引属于 Cloud Code 内部转换后的 Anthropic 消息，不能用于索引 Gemini contents。
 */
func antigravityThinkingSignatureRetryBody(cfg *model.Config, plan protocol.TransformPlan, res *fwResult) ([]byte, bool) {
	if !cfg.UsesAntigravityOAuth() || !strings.Contains(strings.ToLower(plan.RequestModel()), "claude") || res == nil || res.ResponseCommitted {
		return nil, false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest {
		return nil, false
	}
	message := gjson.GetBytes(errorBody, "error.message").String()
	if gjson.Valid(message) {
		if gjson.Get(message, "error.type").String() != "invalid_request_error" {
			return nil, false
		}
		message = gjson.Get(message, "error.message").String()
	}
	path, reason, located := strings.Cut(message, ":")
	if !located || !strings.HasPrefix(path, "messages.") || !anthropicToolErrorPath.MatchString(path) ||
		strings.ToLower(strings.ReplaceAll(strings.TrimSpace(reason), "`", "")) != "invalid signature in thinking block" {
		return nil, false
	}

	body := plan.TranslatedBody
	var removePaths []string
	for i, content := range gjson.GetBytes(body, "request.contents").Array() {
		if content.Get("role").String() != "model" {
			continue
		}
		parts := content.Get("parts").Array()
		contentPath := fmt.Sprintf("request.contents.%d", i)
		start := len(removePaths)
		for j, part := range parts {
			if part.Get("thought").Type != gjson.True {
				continue
			}
			if part.Get("functionCall").Exists() || part.Get("functionResponse").Exists() {
				return nil, false
			}
			removePaths = append(removePaths, fmt.Sprintf("%s.parts.%d", contentPath, j))
		}
		if removed := len(removePaths) - start; removed > 0 && removed == len(parts) {
			removePaths = append(removePaths[:start], contentPath)
		}
	}
	if len(removePaths) == 0 {
		return nil, false
	}
	for _, removePath := range slices.Backward(removePaths) {
		var err error
		body, err = sjson.DeleteBytes(body, removePath)
		if err != nil {
			return nil, false
		}
	}
	return body, true
}

func translateAntigravityRequest(clientProtocol protocol.Protocol, modelName string, body []byte, stream bool) ([]byte, error) {
	var translated []byte
	var err error
	switch clientProtocol {
	case protocol.Anthropic:
		body, err = protocol.NormalizeAnthropicEffortForTranslation(body)
		if err == nil {
			translated, err = antigravityclaude.ConvertClaudeRequestToAntigravity(modelName, body, stream)
		}
	case protocol.Codex:
		translated, err = antigravityresponses.ConvertOpenAIResponsesRequestToAntigravity(modelName, body, stream)
	case protocol.OpenAI:
		translated, err = antigravitychat.ConvertOpenAIRequestToAntigravity(modelName, body, stream)
	case protocol.Gemini:
		translated, err = antigravitygemini.ConvertGeminiRequestToAntigravity(modelName, body, stream)
	default:
		return nil, &protocol.RequestTranslationError{From: clientProtocol, To: protocol.Gemini, Err: fmt.Errorf("unsupported Antigravity client protocol")}
	}
	if err != nil {
		return nil, &protocol.RequestTranslationError{From: clientProtocol, To: protocol.Gemini, Err: err}
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
	if err := protocol.ResponseToolInputError(state); err != nil {
		return translated, err
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
	if state == nil {
		var local any
		state = &local
	}
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
	return frameAntigravityStreamChunks(chunks), protocol.ResponseToolInputError(*state)
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

// antigravityEventPayload returns the JSON payload of one Antigravity stream
// event, or nil for events without one. Data lines join per SSE; the backend
// may also append a bare, pretty-printed JSON error object after the frames.
func antigravityEventPayload(event []byte) []byte {
	normalized := bytes.ReplaceAll(event, []byte("\r\n"), []byte("\n"))
	var dataLines, bareLines [][]byte
	for line := range bytes.SplitSeq(normalized, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if data, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
			dataLines = append(dataLines, bytes.TrimSpace(data))
			continue
		}
		if len(trimmed) == 0 || trimmed[0] == ':' || bytes.HasPrefix(trimmed, []byte("event:")) ||
			bytes.HasPrefix(trimmed, []byte("id:")) || bytes.HasPrefix(trimmed, []byte("retry:")) {
			continue
		}
		bareLines = append(bareLines, trimmed)
	}
	if len(dataLines) == 0 {
		dataLines = bareLines
	}
	payload := bytes.TrimSpace(bytes.Join(dataLines, []byte("\n")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return payload
	}
	// A frame split mid-object stays undecodable; dropping it keeps the
	// truncation visible as an incomplete stream.
	if !gjson.ValidBytes(payload) {
		return nil
	}
	if bytes.IndexByte(payload, '\n') >= 0 {
		var compact bytes.Buffer
		if json.Compact(&compact, payload) == nil {
			payload = compact.Bytes()
		}
	}
	return payload
}

// isAntigravityErrorPayload reports a backend error object sent in place of a
// response chunk.
func isAntigravityErrorPayload(payload []byte) bool {
	return gjson.GetBytes(payload, "error").IsObject() && !gjson.GetBytes(payload, "response").Exists()
}

// antigravitySSEData returns the payload handed to the stream converters; nil
// skips the event. Backend errors reach the usage parser instead.
func antigravitySSEData(event []byte) []byte {
	payload := antigravityEventPayload(event)
	if len(payload) == 0 || isAntigravityErrorPayload(payload) {
		return nil
	}
	if antigravityBareFilterNotice(payload) {
		return append(append([]byte(`{"response":`), payload...), '}')
	}
	return payload
}

/**
 * @brief 识别未包在 response 信封中的 Antigravity 过滤通知。
 * @param payload 原始 SSE JSON 载荷。
 * @return 仅当裸 Gemini 帧符合稀疏过滤通知形状时为 true。
 */
func antigravityBareFilterNotice(payload []byte) bool {
	if !gjson.GetBytes(payload, "candidates").IsArray() || gjson.GetBytes(payload, "response").Exists() {
		return false
	}
	event, err := sseJSONObjectMap(payload)
	return err == nil && geminiFilterNotice(event) != ""
}

/**
 * @brief 将转换及渠道规则处理后的请求收敛为 Antigravity 出站信封。
 * @param cfg 渠道凭据与积分模式；非 Antigravity 渠道或 nil 时原样返回。
 * @param modelName 实际上游模型，用于工具、思考和输出限制的模型分支。
 * @param body Gemini 请求对象，可包含 request 信封。
 * @param sourceBody 客户端原始请求，用于搜索意图、工具 ID 与会话恢复。
 * @param headers 客户端会话头，用于稳定会话标识。
 * @param matcher 可选系统敏感词匹配器；nil 不影响已知客户端身份中和。
 * @return 最终出站 JSON；失败时为 nil。
 * @return 项目缺失、请求格式无效、积分模式不适用或 JSON 写入失败时返回错误。
 */
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

	// Antigravity 只在专用 web_search 请求里执行 googleSearch，且拒绝内置工具与
	// functionDeclarations 混用（include_server_side_tool_invocations 也无效）。
	webSearch := gjson.GetBytes(body, "requestType").String() == "web_search" || wantsAntigravityWebSearch(sourceBody)
	if webSearch {
		modelName = antigravityWebSearchModel(modelName)
		request = ensureAntigravityGoogleSearchTool(request)
	} else {
		request = stripAntigravityGoogleSearchTools(request)
	}

	// system_instruction → systemInstruction（snake_case 到 camelCase）
	if sysInst := gjson.GetBytes(request, "system_instruction"); sysInst.Exists() {
		if !gjson.GetBytes(request, "systemInstruction").Exists() {
			request = setJSONRaw(request, "systemInstruction", sysInst.Raw)
		}
		request = deleteJSONPath(request, "system_instruction")
	}

	request = injectAntigravityIdentityPrompt(request)
	request = cliproxysignature.SanitizeGeminiSystemInstruction(request, "contents")
	request = deleteJSONPath(request, "safetySettings")
	request = normalizeAntigravityContentsRoles(request)
	request = restoreAntigravityAnthropicToolIDs(request, sourceBody)
	request = rewriteAntigravityConversationText(request)
	request, structuredOutputPrompt := takeAntigravityClaude55StructuredOutput(request, modelName)
	request = normalizeAntigravityThinkingConfig(request, modelName)
	request = normalizeAntigravitySchemas(request, modelName)
	lookupID := strings.ToLower(strings.TrimSpace(modelName))
	if strings.Contains(lookupID, "claude") {
		request = normalizeAntigravityClaudeOutputLimit(request, lookupID)
		request = normalizeAntigravityClaudeToolResultOrder(request)
		request = normalizeAntigravityClaudeToolMode(request, structuredOutputPrompt != "")
	} else {
		request = cliproxysignature.SanitizeVertexSignaturesForNonVertex(request, "contents")
		if gjson.GetBytes(request, "generationConfig").IsObject() {
			request = deleteJSONPath(request, "generationConfig.maxOutputTokens")
		}
	}

	requestType := "agent"
	requestID := "agent-" + util.NewUUIDv4()
	if webSearch {
		requestType = "web_search"
	} else if strings.Contains(strings.ToLower(modelName), "image") {
		requestType = "image_gen"
		requestID = fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), util.NewUUIDv4())
	}
	if requestType != "image_gen" {
		if !gjson.GetBytes(request, "sessionId").Exists() {
			request = setJSONValue(request, "sessionId", antigravitySessionID(cfg, headers, sourceBody, body))
		}
	}

	envelope := struct {
		EnabledCreditTypes []string        `json:"enabledCreditTypes,omitempty"`
		Project            string          `json:"project"`
		Request            json.RawMessage `json:"request"`
		Model              string          `json:"model"`
		UserAgent          string          `json:"userAgent"`
		RequestType        string          `json:"requestType"`
		RequestID          string          `json:"requestId"`
	}{
		Project: cfg.AntigravityProjectID, Request: json.RawMessage(request),
		Model: strings.TrimSpace(modelName), UserAgent: "antigravity",
		RequestType: requestType, RequestID: requestID,
	}
	if cfg.AntigravityCredits {
		if requestType != "agent" || !antigravityClaudeModel(modelName) {
			return nil, errAntigravityCreditsUnavailable
		}
		envelope.EnabledCreditTypes = []string{"GOOGLE_ONE_AI"}
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode Antigravity request: %w", err)
	}
	raw, err = rewriteAntigravitySystemInstruction(raw, matcher)
	if err != nil {
		return nil, fmt.Errorf("rewrite Antigravity system instruction: %w", err)
	}
	if structuredOutputPrompt != "" {
		// 在混淆之后追加，schema 字段名里的敏感词不能被插入零宽字符。
		raw = setJSONValue(raw, "request.systemInstruction.parts.-1.text", structuredOutputPrompt)
	}
	return raw, nil
}

/**
 * @brief 归一 Claude 的输出上限别名，按有效上限限制思考预算。
 * @param request 已合并 generationConfig 别名的 Antigravity 请求体。
 * @param modelID 已归一为小写的实际上游模型 ID。
 * @return 保留缺省或零输出上限，已有正上限按模型目录收紧后的请求体。
 */
func normalizeAntigravityClaudeOutputLimit(request []byte, modelID string) []byte {
	const canonicalPath = "generationConfig.maxOutputTokens"
	const aliasPath = "generationConfig.max_output_tokens"
	canonical := gjson.GetBytes(request, canonicalPath)
	maxOut, ok := jsonIntegerValue(canonical)
	if alias := gjson.GetBytes(request, aliasPath); alias.Exists() {
		aliasMax, aliasOK := jsonIntegerValue(alias)
		if !canonical.Exists() || aliasOK && aliasMax > 0 && (!ok || maxOut < 0 || aliasMax < maxOut) {
			request = setJSONRaw(request, canonicalPath, alias.Raw)
			maxOut, ok = aliasMax, aliasOK
		}
		request = deleteJSONPath(request, aliasPath)
	}
	if !ok || maxOut <= 0 {
		return request
	}
	if info := cliproxyregistry.LookupModelInfo(modelID, "antigravity"); info != nil &&
		info.MaxCompletionTokens > 0 && maxOut > int64(info.MaxCompletionTokens) {
		maxOut = int64(info.MaxCompletionTokens)
		request = setJSONValue(request, canonicalPath, maxOut)
	}
	for _, path := range []string{
		"generationConfig.thinkingConfig.thinkingBudget",
		"generationConfig.thinkingConfig.thinking_budget",
		"generationConfig.thinking_config.thinkingBudget",
		"generationConfig.thinking_config.thinking_budget",
	} {
		if budget, ok := jsonIntegerValue(gjson.GetBytes(request, path)); ok && budget >= maxOut {
			request = setJSONValue(request, path, maxOut-1)
		}
	}
	return request
}

/**
 * @brief 将 Claude 5.5 不支持的原生结构化输出转换为提示词。
 * @param request Antigravity 信封内的请求，schema 尚未清洗。
 * @param modelName 实际请求的上游模型。
 * @return 移除了已转换 schema 和响应 MIME 类型的请求。
 * @return 将 Protobuf 类型转为 JSON Schema 类型后的提示词；无需转换时为空。
 */
func takeAntigravityClaude55StructuredOutput(request []byte, modelName string) ([]byte, string) {
	if anthropicForcedToolChoiceRejectingFamily(anthropicCanonicalModelName(modelName)) == "" {
		return request, ""
	}
	configKeys := []string{"generationConfig", "generation_config"}
	schemaKeys := []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"}
	var schema gjson.Result
	for _, configKey := range configKeys {
		for _, schemaKey := range schemaKeys {
			if candidate := gjson.GetBytes(request, configKey+"."+schemaKey); candidate.IsObject() && !schema.Exists() {
				schema = candidate
			}
		}
	}
	if !schema.Exists() {
		return request, ""
	}
	schemaJSON := cliproxyutil.NormalizeJSONSchemaNumericConstraints(schema.Raw)
	prompt := cliproxycommon.BuildClaudeStructuredOutputInstruction(gjson.Parse(`{"type":"json_schema","schema":` + schemaJSON + `}`))
	for _, configKey := range configKeys {
		for _, key := range append(schemaKeys, "responseMimeType", "response_mime_type") {
			request = deleteJSONPath(request, configKey+"."+key)
		}
	}
	return request, prompt
}

/**
 * @brief 合并思考配置别名，按实际模型能力输出一个等级或预算。
 * @param request 信封内 Gemini 请求体。
 * @param modelName 实际发送到 Antigravity 的模型名。
 * @return 清理冲突的思考字段并保留摘要开关和其他选项后的请求体。
 */
func normalizeAntigravityThinkingConfig(request []byte, modelName string) []byte {
	paths := [...]string{"generationConfig.thinkingConfig", "generationConfig.thinking_config", "generation_config.thinking_config", "generation_config.thinkingConfig"}
	root := cliproxyutil.ParseGJSONBytesNoCopy(request)

	var config []byte
	var path string
	merged := false
	for _, alias := range paths {
		value := root.Get(alias)
		if !value.IsObject() {
			continue
		}
		if path == "" {
			path, config = alias, []byte(value.Raw)
			continue
		}
		path, merged = paths[0], true
		value.ForEach(func(key, value gjson.Result) bool {
			if !gjson.GetBytes(config, sjsonPathEscape(key.String())).Exists() {
				config = setJSONRaw(config, sjsonObjectPathJoin("", key.String()), value.Raw)
			}
			return true
		})
	}
	if path == "" {
		return request
	}

	level, budget, hasBudget := cliproxycommon.GeminiThinkingConfig(root)
	includeThoughts := cliproxycommon.GeminiIncludeThoughts(root)
	data := gjson.ParseBytes(config)
	switch level {
	case "minimal":
		level = "low"
	case "max":
		level = "high"
	case "xhigh":
		info := cliproxyregistry.LookupModelInfo(strings.ToLower(strings.TrimSpace(modelName)), "antigravity")
		if info == nil || info.Thinking == nil || !slices.Contains(info.Thinking.Levels, "xhigh") {
			level = "high"
		}
	case "", "low", "medium", "high":
	default:
		for _, key := range [...]string{geminiThinkingLevelKey, "thinking_level"} {
			if original := jsonStringValue(data.Get(key)); strings.EqualFold(strings.TrimSpace(original), level) {
				level = original
				break
			}
		}
	}
	levelKey, budgetKey, summaryKey := geminiThinkingLevelKey, geminiThinkingBudgetKey, "includeThoughts"
	if !merged {
		if !data.Get(levelKey).Exists() && data.Get("thinking_level").Exists() {
			levelKey = "thinking_level"
		}
		if !data.Get(budgetKey).Exists() && data.Get("thinking_budget").Exists() {
			budgetKey = "thinking_budget"
		}
		if !data.Get(summaryKey).Exists() && data.Get("include_thoughts").Exists() {
			summaryKey = "include_thoughts"
		}
	}
	for _, key := range [...]string{geminiThinkingLevelKey, "thinking_level", geminiThinkingBudgetKey, "thinking_budget", "includeThoughts", "include_thoughts"} {
		switch {
		case key == levelKey && level != "":
			config = setJSONValue(config, key, level)
		case key == budgetKey && level == "" && hasBudget:
			config = setJSONValue(config, key, budget)
		case key == summaryKey && includeThoughts != gjson.Null:
			config = setJSONValue(config, key, includeThoughts == gjson.True)
		default:
			config = deleteJSONPath(config, key)
		}
	}
	for _, alias := range paths {
		if alias != path && root.Get(alias).Exists() {
			request = deleteJSONPath(request, alias)
		}
	}
	if !bytes.Equal(config, []byte(root.Get(path).Raw)) {
		request = setJSONRaw(request, path, string(config))
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

/**
 * @brief 在忽略敏感词混淆字符后识别系统提示中的 Antigravity 身份。
 * @param request 不含信封的 Gemini 请求 JSON。
 * @return 是否已包含身份提示，用于避免重复注入。
 */
func antigravitySystemInstructionContainsIdentity(request []byte) bool {
	containsIdentity := func(text string) bool {
		return strings.Contains(strings.ReplaceAll(text, sensitiveWordJoiner, ""), "You are Antigravity")
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

func wantsAntigravityWebSearch(body []byte) bool {
	if gjson.GetBytes(body, "input").Exists() {
		return antigravityresponses.WantsWebSearch(body)
	}
	return antigravitychat.WantsWebSearch(body)
}

// Antigravity 的 Claude/GPT 模型静默忽略 googleSearch，搜索交给 Gemini；Gemini 模型原生支持搜索。
func antigravityWebSearchModel(modelName string) string {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if strings.HasPrefix(name, "claude-") || strings.HasPrefix(name, "gpt-") {
		return antigravityWebSearchFallbackModel
	}
	return modelName
}

func ensureAntigravityGoogleSearchTool(request []byte) []byte {
	for _, tool := range gjson.GetBytes(request, "tools").Array() {
		if tool.Get("googleSearch").Exists() {
			return request
		}
	}
	return setJSONRaw(request, "tools", `[{"googleSearch":{}}]`)
}

func stripAntigravityGoogleSearchTools(request []byte) []byte {
	tools := gjson.GetBytes(request, "tools")
	if !tools.IsArray() {
		return request
	}
	kept := make([]string, 0, len(tools.Array()))
	stripped := false
	for _, tool := range tools.Array() {
		raw := tool.Raw
		for _, key := range []string{"googleSearch", "google_search"} {
			if tool.Get(key).Exists() {
				raw = string(deleteJSONPath([]byte(raw), key))
				stripped = true
			}
		}
		if gjson.Parse(raw).IsObject() && len(gjson.Parse(raw).Map()) == 0 {
			continue
		}
		kept = append(kept, raw)
	}
	if !stripped {
		return request
	}
	if len(kept) == 0 {
		return deleteJSONPath(request, "tools")
	}
	return setJSONRaw(request, "tools", "["+strings.Join(kept, ",")+"]")
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

func consolidateAntigravityGenerationConfigParents(request []byte) []byte {
	snake := gjson.GetBytes(request, "generation_config")
	if !snake.IsObject() {
		return request
	}
	canonical := gjson.GetBytes(request, "generationConfig")
	if !canonical.IsObject() {
		request = setJSONRaw(request, "generationConfig", snake.Raw)
	} else {
		snake.ForEach(func(key, value gjson.Result) bool {
			memberPath := "generationConfig." + key.String()
			if gjson.GetBytes(request, memberPath).Exists() {
				return true
			}
			request = setJSONRaw(request, memberPath, value.Raw)
			return true
		})
	}
	return deleteJSONPath(request, "generation_config")
}

var (
	antigravityParameterSchemaKeys = []string{"parameters", "parametersJsonSchema", "parameters_json_schema"}
	antigravityResponseSchemaKeys  = []string{"response", "responseJsonSchema", "response_json_schema"}
)

/**
 * @brief 按实际上游模型清理 Antigravity 工具及结构化输出 schema。
 * @param request Gemini 请求正文，不含 Antigravity 信封。
 * @param modelName 实际上游模型名。
 * @return schema 别名已归一且满足目标模型约束的请求正文。
 */
func normalizeAntigravitySchemas(request []byte, modelName string) []byte {
	isClaudeModel := strings.Contains(strings.ToLower(modelName), "claude")
	useAntigravitySchema := isClaudeModel ||
		strings.Contains(strings.ToLower(modelName), "gemini-3-pro") ||
		strings.Contains(strings.ToLower(modelName), "gemini-3.1-pro")
	generationConfigKeys := []string{"generationConfig", "generation_config"}
	generationSchemaKeys := []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"}

	// 工具 schema 只在 tools 子文档上改写、最后整体写回一次：逐声明对整份请求
	// set/delete 会把长对话 contents 复制「声明数×别名数」次。
	if tools := gjson.GetBytes(request, "tools"); tools.IsArray() {
		if normalized := normalizeAntigravityToolSchemas(tools.Raw, useAntigravitySchema, isClaudeModel); normalized != tools.Raw {
			request = setJSONRaw(request, "tools", normalized)
		}
	}

	generationSchemaPaths := make([]string, 0, len(generationConfigKeys)*len(generationSchemaKeys))
	for _, configKey := range generationConfigKeys {
		for _, schemaKey := range generationSchemaKeys {
			generationSchemaPaths = append(generationSchemaPaths, configKey+"."+schemaKey)
		}
	}
	if _, schemaRaw, found := firstAntigravityJSONObject(request, generationSchemaPaths); found {
		cleaned := cliproxyutil.CleanJSONSchemaForAntigravityResponse(schemaRaw)
		request = consolidateAntigravitySchemaAliases(request, "generationConfig.responseSchema", generationSchemaPaths, cleaned)
	}
	return consolidateAntigravityGenerationConfigParents(request)
}

/**
 * @brief 归一工具声明的 schema 别名，Claude 入参使用 JSON Schema 载体保留联合类型。
 * @param tools Gemini 工具数组 JSON。
 * @param useAntigravitySchema 是否使用 Antigravity 原生 schema 规则。
 * @param isClaudeModel 实际目标是否为 Claude；仅影响入参 schema 的载体。
 * @return 清理后的工具数组 JSON。
 */
func normalizeAntigravityToolSchemas(tools string, useAntigravitySchema, isClaudeModel bool) string {
	cleanNative := cliproxyutil.CleanJSONSchemaForGemini
	if useAntigravitySchema {
		cleanNative = cliproxyutil.CleanJSONSchemaForAntigravity
	}
	out := []byte(tools)
	for ti, tool := range gjson.Parse(tools).Array() {
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
				normalized := []byte(declaration.Raw)
				for _, aliases := range [][]string{antigravityParameterSchemaKeys, antigravityResponseSchemaKeys} {
					if _, schemaRaw, found := firstAntigravityJSONObject(normalized, aliases); found {
						canonical := aliases[0]
						var cleaned string
						if isClaudeModel && canonical == "parameters" {
							canonical = "parametersJsonSchema"
							cleaned = normalizeAntigravityJSONSchemaTypes(cleanAntigravityToolSchemaRaw(schemaRaw, cliproxyutil.CleanJSONSchemaForAntigravityJSONSchema))
						} else {
							cleaned = cleanAntigravityToolSchemaRaw(schemaRaw, cleanNative)
						}
						normalized = consolidateAntigravitySchemaAliases(normalized, canonical, aliases, cleaned)
					}
				}
				if string(normalized) != declaration.Raw {
					out = setJSONRaw(out, fmt.Sprintf("%d.%s.%d", ti, key, di), string(normalized))
				}
			}
		}
	}
	return string(out)
}

/**
 * @brief 将 Gemini 原生类型枚举转为 JSON Schema 类型名，不改 schema 内的字面值。
 * @param schema 已清理的工具入参 schema JSON。
 * @return 类型名符合 JSON Schema 的工具入参。
 */
func normalizeAntigravityJSONSchemaTypes(schema string) string {
	var paths []string
	cliproxyutil.WalkSchema(gjson.Parse(schema), "", "type", &paths)
	for _, path := range paths {
		typeName := gjson.Get(schema, path).String()
		switch typeName {
		case "OBJECT", "ARRAY", "STRING", "NUMBER", "INTEGER", "BOOLEAN", "NULL":
			schema, _ = sjson.Set(schema, path, strings.ToLower(typeName))
		}
	}
	return schema
}

/**
 * @brief 将工具 schema 作为嵌套节点清理，以保留已有的工具占位规则。
 * @param raw 工具参数或返回值的 schema 对象。
 * @param clean 实际载体对应的清理函数。
 * @return 清理后的 schema 对象，清理后不再为对象时保留原 schema。
 */
func cleanAntigravityToolSchemaRaw(raw string, clean func(string) string) string {
	cleaned := clean(`{"schema":` + raw + `}`)
	if nested := gjson.Get(cleaned, "schema"); nested.IsObject() {
		return nested.Raw
	}
	return raw
}

/**
 * @brief 保留显式禁用工具的选择，并为 Claude 设置适用的调用模式。
 * @param request Antigravity 信封内的请求。
 * @param promptStructuredOutput 是否已改用提示词实现结构化输出，此时强制工具选择降为 AUTO。
 * @return 带最终工具调用模式的请求。
 */
func normalizeAntigravityClaudeToolMode(request []byte, promptStructuredOutput bool) []byte {
	for _, toolKey := range []string{"toolConfig", "tool_config"} {
		for _, callingKey := range []string{"functionCallingConfig", "function_calling_config"} {
			path := toolKey + "." + callingKey
			switch strings.ToUpper(strings.TrimSpace(jsonStringValue(gjson.GetBytes(request, path+".mode")))) {
			case "NONE":
				return setJSONValue(request, path+".mode", "NONE")
			case "ANY", "VALIDATED":
				if promptStructuredOutput {
					request = setJSONValue(request, path+".mode", "AUTO")
					request = deleteJSONPath(request, path+".allowedFunctionNames")
					request = deleteJSONPath(request, path+".allowed_function_names")
				}
			}
		}
	}
	if promptStructuredOutput {
		return request
	}
	return setJSONValue(request, "toolConfig.functionCallingConfig.mode", "VALIDATED")
}

/**
 * @brief 按 Claude 线协议将同轮工具结果移到普通内容之前，保留各组内的顺序。
 * @param request 转换后的 Antigravity 请求体。
 * @return 按 Claude 目标调整后的请求体。
 */
func normalizeAntigravityClaudeToolResultOrder(request []byte) []byte {
	for index, content := range gjson.GetBytes(request, "contents").Array() {
		if content.Get("role").String() != "user" {
			continue
		}
		var results, other []string
		needsReorder := false
		for _, part := range content.Get("parts").Array() {
			if cliproxycommon.GeminiPartFunctionResponse(part).IsObject() {
				results = append(results, part.Raw)
				needsReorder = needsReorder || len(other) > 0
			} else {
				other = append(other, part.Raw)
			}
		}
		if needsReorder {
			request = setJSONRaw(request, fmt.Sprintf("contents.%d.parts", index), joinJSONRaw(append(results, other...)))
		}
	}
	return request
}

/**
 * @brief 生成按 Antigravity 账号隔离的会话 ID，令牌轮换不影响已知账号。
 * @param cfg 实际用于出站的 Antigravity 凭证配置。
 * @param headers 客户端会话及线程请求头。
 * @param sourceBody 客户端原始请求。
 * @param body 转换后的请求，用于在缺少会话线索时取首条用户文本。
 * @return Antigravity 接受的负十进制会话 ID。
 */
func antigravitySessionID(cfg *model.Config, headers http.Header, sourceBody, body []byte) string {
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
	account := gjson.Get(cfg.OAuthCredential, "email").String()
	if account != "" {
		account = "email:" + strings.ToLower(account)
	} else {
		account = antigravityCredentialPoolScope(cfg)
	}
	digest := sha256.Sum256([]byte("antigravity-session\x00" + account + "\x00" + strings.TrimSpace(seed)))
	value := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff)
	return "-" + strconv.FormatInt(value, 10)
}

/**
 * @brief 创建仅供触发句匹配使用的文本，折叠空白并忽略已实测无效的 U+200B/U+034F 混淆。
 * @param text 原始系统文本，不修改。
 * @return 保留大小写及其他字符的匹配文本。
 * @return 匹配文本各字节边界对应的原文字节位置，用于仅替换命中片段。
 */
func antigravityPromptMatchText(text string) (string, []int) {
	var normalized strings.Builder
	normalized.Grow(len(text))
	offsets := make([]int, 1, len(text)+1)
	wasSpace := false
	for start := 0; start < len(text); {
		r, size := utf8.DecodeRuneInString(text[start:])
		end := start + size
		switch {
		case r == '\u200b' || r == '\u034f':
			offsets[len(offsets)-1] = end
		case unicode.IsSpace(r):
			if wasSpace {
				offsets[len(offsets)-1] = end
			} else {
				normalized.WriteByte(' ')
				offsets = append(offsets, end)
			}
			wasSpace = true
		default:
			normalized.WriteString(text[start:end])
			for pos := start + 1; pos <= end; pos++ {
				offsets = append(offsets, pos)
			}
			wasSpace = false
		}
		start = end
	}
	return normalized.String(), offsets
}

/**
 * @brief 容忍已验证的排版和混淆变体，仅改写原文中的已知身份及触发句，再按配置混淆敏感词。
 * @param body 已注入 Antigravity 身份的出站信封，系统指令可为字符串或 parts 对象。
 * @param matcher 可选敏感词匹配器；nil 只关闭混淆，不关闭身份中和或触发句改写。
 * @return 改写后的信封；失败时为 nil。
 * @return 写入系统文本失败时返回 JSON 修改错误。
 */
func rewriteAntigravitySystemInstruction(body []byte, matcher *regexp.Regexp) ([]byte, error) {
	rewriteText := func(original string) string {
		/**
		 * @brief 一处互不重叠的原文替换，按字节位置保留其余文本。
		 */
		type change struct {
			start, end int    ///< 原文中的半开字节区间。
			text       string ///< 替换后的指令。
		}
		matchText, offsets := antigravityPromptMatchText(original)
		var changes []change
		for _, opener := range antigravitySystemIdentityOpeners {
			loc := opener.FindStringIndex(matchText)
			if loc == nil {
				continue
			}
			rest := matchText[loc[1]:]
			replacement := "You are an AI agent."
			if strings.HasPrefix(rest, ",") {
				replacement = "You are an AI agent"
			} else if rest != "" && rest[0] != ' ' {
				continue
			}
			changes = append(changes, change{0, offsets[loc[1]], replacement})
			break
		}
		for _, rule := range antigravitySystemInstructionRules {
			for _, loc := range rule.pattern.FindAllStringSubmatchIndex(matchText, -1) {
				for i, pos := range loc {
					loc[i] = offsets[pos]
				}
				replacement := rule.pattern.ExpandString(nil, rule.replacement, original, loc)
				changes = append(changes, change{loc[0], loc[1], string(replacement)})
			}
		}
		slices.SortFunc(changes, func(a, b change) int { return a.start - b.start })
		var result strings.Builder
		result.Grow(len(original))
		cursor := 0
		for _, edit := range changes {
			result.WriteString(original[cursor:edit.start])
			result.WriteString(edit.text)
			cursor = edit.end
		}
		result.WriteString(original[cursor:])
		rewritten := result.String()
		if matcher != nil {
			rewritten = obfuscateAntigravityText(rewritten, matcher)
		}
		return rewritten
	}
	instruction := gjson.GetBytes(body, "request.systemInstruction")
	if instruction.Type == gjson.String {
		return sjson.SetBytes(body, "request.systemInstruction", rewriteText(instruction.String()))
	}
	for index, part := range instruction.Get("parts").Array() {
		text := part.Get("text")
		if text.Type != gjson.String {
			continue
		}
		original := text.String()
		rewritten := rewriteText(original)
		if rewritten == original {
			continue
		}
		var err error
		body, err = sjson.SetBytes(body, fmt.Sprintf("request.systemInstruction.parts.%d.text", index), rewritten)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

/**
 * @brief 仅在 Antigravity 出站正文与工具结果中改写已复现裸 429 的 Codex 引用句。
 * @param request Antigravity 信封内的请求。
 * @return 改写后的请求副本；未命中时返回原字节，不修改调用方字节。
 * @note 保留 thinking、签名、工具调用参数、工具定义与标识；不扩大系统指令的身份或混淆规则。
 * @details 工具结果同时识别 Gemini 原生客户端的 camelCase 与 snake_case 字段；正文不含触发句前缀时跳过逐段重建。
 */
func rewriteAntigravityConversationText(request []byte) []byte {
	if !bytes.Contains(request, []byte("You are Codex")) {
		return request
	}
	for contentIndex, content := range gjson.GetBytes(request, "contents").Array() {
		for partIndex, part := range content.Get("parts").Array() {
			for _, field := range []string{"text", "functionResponse.response", "function_response.response"} {
				if field == "text" && part.Get("thought").Bool() {
					continue
				}
				value := part.Get(field)
				if rewritten, changed := rewriteAntigravityQuotedCodexIdentity(value); changed {
					request = setJSONRaw(request, fmt.Sprintf("contents.%d.parts.%d.%s", contentIndex, partIndex, field), rewritten)
				}
			}
		}
	}
	return request
}

/**
 * @brief 在正文或工具结果的 JSON 字符串值中精确替换已验证的 Codex 身份句，保留对象键与其他字面值。
 * @param value 正文文本或工具返回值，可含嵌套对象与数组。
 * @return 改写后的原始 JSON；未命中时返回原片段。
 * @return 是否发生替换。
 */
func rewriteAntigravityQuotedCodexIdentity(value gjson.Result) (string, bool) {
	if value.Type == gjson.String {
		original := value.String()
		rewritten := antigravityQuotedCodexIdentity.ReplaceAllString(original, "Codex is an agent based on GPT-5.${1}")
		if rewritten != original {
			return jsonEscapedString(rewritten), true
		}
		return value.Raw, false
	}
	changed := false
	if value.IsObject() {
		builder := newJSONObjectBuilder()
		value.ForEach(func(key, member gjson.Result) bool {
			rewritten, memberChanged := rewriteAntigravityQuotedCodexIdentity(member)
			changed = changed || memberChanged
			builder.SetRaw(key.Raw, rewritten)
			return true
		})
		if changed {
			return builder.String(), true
		}
	} else if value.IsArray() {
		var items []string
		value.ForEach(func(_, item gjson.Result) bool {
			rewritten, itemChanged := rewriteAntigravityQuotedCodexIdentity(item)
			changed = changed || itemChanged
			items = append(items, rewritten)
			return true
		})
		if changed {
			return joinJSONRaw(items), true
		}
	}
	return value.Raw, false
}

// obfuscateChatSystemMessages only rewrites system/developer text. User content,
// tool schemas, arguments and results remain untouched.
func obfuscateChatSystemMessages(body []byte, matcher *regexp.Regexp) ([]byte, error) {
	if matcher == nil {
		return body, nil
	}
	return rewriteChatSystemMessages(body, func(text string) string {
		return obfuscateAntigravityText(text, matcher)
	})
}

func rewriteChatSystemMessages(body []byte, rewrite func(string) string) ([]byte, error) {
	var err error
	for index, message := range gjson.GetBytes(body, "messages").Array() {
		role := message.Get("role").String()
		if role != "system" && role != "developer" {
			continue
		}
		content := message.Get("content")
		path := fmt.Sprintf("messages.%d.content", index)
		if content.Type == gjson.String {
			body, err = sjson.SetBytes(body, path, rewrite(content.String()))
		} else if content.IsArray() {
			for partIndex, part := range content.Array() {
				if part.Get("type").String() != "text" || part.Get("text").Type != gjson.String {
					continue
				}
				body, err = sjson.SetBytes(body, fmt.Sprintf("%s.%d.text", path, partIndex), rewrite(part.Get("text").String()))
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

/**
 * @brief 在命中敏感词的首 rune 后插入 U+034F，供两个提供商共用。
 * @param text 系统或开发者提示文本。
 * @param matcher 已启用的敏感词匹配器，不得为 nil。
 * @return 混淆后的文本，已含混淆字符的命中保持原样。
 */
func obfuscateAntigravityText(text string, matcher *regexp.Regexp) string {
	return matcher.ReplaceAllStringFunc(text, func(word string) string {
		if strings.Contains(word, sensitiveWordJoiner) {
			return word
		}
		_, size := utf8.DecodeRuneInString(word)
		if size <= 0 || size >= len(word) {
			return word
		}
		return word[:size] + sensitiveWordJoiner + word[size:]
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

// isAntigravityModelNotFound 判断 Antigravity 的 404 是否为模型级失败。
// 端点与 wire 协议由系统内置、模型在请求体里：base URL 回退后仍是 Google 的
// 404 NOT_FOUND（"Requested entity was not found."），即该账号不可用此模型，
// 既不是协议能力缺失，也不是渠道故障。
func isAntigravityModelNotFound(cfg *model.Config, statusCode int) bool {
	return cfg != nil && cfg.UsesAntigravityOAuth() && statusCode == http.StatusNotFound
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

// terminateAntigravitySSE dispatches a trailing event the backend closes
// without a blank line, such as an appended bare JSON error. Read errors still
// surface unchanged; only a clean EOF gets the terminator.
func terminateAntigravitySSE(body io.ReadCloser) io.ReadCloser {
	return readerWithCloser{Reader: io.MultiReader(body, strings.NewReader("\n\n")), Closer: body}
}

// unwrapAntigravitySSEEvent renders one event for the usage parser; nil means
// the event carries no payload. Backend errors stay intact so the parser
// records them as the stream error.
func unwrapAntigravitySSEEvent(event []byte) ([]byte, error) {
	payload := antigravityEventPayload(event)
	if len(payload) == 0 {
		return nil, nil
	}
	if !bytes.Equal(payload, []byte("[DONE]")) && !isAntigravityErrorPayload(payload) && !antigravityBareFilterNotice(payload) {
		inner, err := unwrapAntigravityResponse(payload)
		if err != nil {
			return nil, err
		}
		payload = bytes.TrimSpace(inner)
	}
	output := make([]byte, 0, len(payload)+8)
	output = append(output, "data: "...)
	output = append(output, payload...)
	return append(output, '\n', '\n'), nil
}
