package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// anthropicCLIVersion 是 Claude Code wire 的内置最低版本/离线回退值。
	// 运行中的服务会由 anthropic_cli_version_sync.go 向前同步官方稳定版。
	anthropicCLIVersion  = "2.1.280"
	anthropicBillingSalt = "59cf53e54c78"

	// anthropicClaudeCodeIdentityPrompt 是 Claude Code CLI system 三段式的第二段。
	anthropicClaudeCodeIdentityPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
)

// anthropicClaudeCLIUserAgentPattern 对齐 sub2api 的 Claude Code UA 判定：只要求
// claude-cli/X.Y.Z 前缀，入口后缀由客户端版本和宿主环境决定，不绑定固定枚举。
var anthropicClaudeCLIUserAgentPattern = regexp.MustCompile(
	`(?i)^claude-cli/([0-9]+\.[0-9]+\.[0-9]+)`)

// anthropicFingerprintUserAgentPattern is the stricter form used when a UA is
// allowed to become an account-level fingerprint. The version must be followed
// by whitespace or the end of the value, matching sub2api's cache guard.
var anthropicFingerprintUserAgentPattern = regexp.MustCompile(
	`(?i)^(claude-cli/)([0-9]+\.[0-9]+\.[0-9]+)(\s.*)?$`)

var anthropicBillingVersionPattern = regexp.MustCompile(`cc_version=([0-9]+\.[0-9]+\.[0-9]+)`)
var anthropicBillingFingerprintPattern = regexp.MustCompile(`cc_version=[0-9]+\.[0-9]+\.[0-9]+\.[0-9a-fA-F]{3}\b`)

// anthropicClaudeCodeLegacyMetadataUserIDPattern 对齐 sub2api 的 legacy
// metadata.user_id 格式。新版 Claude Code 使用 JSON 字符串格式，见
// validAnthropicClaudeCodeMetadataUserID。
var anthropicClaudeCodeLegacyMetadataUserIDPattern = regexp.MustCompile(
	`^user_[a-fA-F0-9]{64}_account_[a-fA-F0-9-]*_session_[a-fA-F0-9-]{36}$`)

const anthropicClaudeCodePrompt = `You are an interactive agent that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.

IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.
IMPORTANT: You must NEVER generate or guess URLs for the user unless you are confident that the URLs are for helping the user with programming. You may use URLs provided by the user in their messages or local files.

# Tone and style
 - Only use emojis if the user explicitly requests it. Avoid using emojis in all communication unless asked.
 - Your responses should be short and concise.
 - When referencing specific functions or pieces of code include the pattern file_path:line_number to allow the user to easily navigate to the source code location.
 - When referencing GitHub issues or pull requests, use the owner/repo#123 format (e.g. anthropics/claude-code#100) so they render as clickable links.
 - Do not use a colon before tool calls. Your tool calls may not be shown directly in the output, so text like "Let me read the file:" followed by a read tool call should just be "Let me read the file." with a period.`

func isAnthropicOAuthMessagesRequest(cfg *model.Config, upstream protocol.Protocol, requestPath string) bool {
	return cfg != nil && cfg.UsesAnthropicOAuth() && isAnthropicMessagesRequest(upstream, requestPath)
}

// isAnthropicClaudeCodeMessagesRequest 判断本次请求要不要套 Claude Code CLI 指纹。
//
// 判据只有「是不是 Anthropic Messages 上游」——OAuth、第一方 API Key、第三方网关
// 都生成 CLI body；OAuth 的 beta/Header 使用 sub2api 模拟 profile。唯一例外是 Z.ai Coding Plan：它也走 anthropic
// 协议，却有自己的 ZCode 设备指纹契约，两套指纹叠加会互相破坏（ZCode 覆盖
// metadata.user_id，而 Claude Code 的 1h cache TTL 配不上 ZCode 的 beta 头）。
func isAnthropicClaudeCodeMessagesRequest(cfg *model.Config, upstream protocol.Protocol, requestPath string) bool {
	return isAnthropicMessagesRequest(upstream, requestPath) && !isZAICodingPlanRequest(cfg, upstream, requestPath)
}

func isAnthropicMessagesRequest(upstream protocol.Protocol, requestPath string) bool {
	if upstream != protocol.Anthropic {
		return false
	}
	path := strings.TrimSuffix(strings.TrimSpace(requestPath), "/")
	return path == "/v1/messages" || path == "/messages"
}

func isAnthropicCountTokensRequest(upstream protocol.Protocol, requestPath string) bool {
	if upstream != protocol.Anthropic {
		return false
	}
	path := strings.TrimSuffix(strings.TrimSpace(requestPath), "/")
	return path == "/v1/messages/count_tokens" || path == "/messages/count_tokens"
}

// finalizeAnthropicCountTokensBody keeps the caller's wire where the target
// accepts it. Anthropic's first-party count_tokens endpoint rejects metadata,
// context_management and diagnostics for both OAuth and API keys.
func finalizeAnthropicCountTokensBody(body []byte, cfg *model.Config, target *url.URL, nativeCaller bool) ([]byte, error) {
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("finalize Anthropic count_tokens request: invalid JSON body")
	}
	body = sanitizeAnthropicEmptyTextBlocks(body)
	if cfg != nil && cfg.UsesAnthropicOAuth() && !nativeCaller {
		body = normalizeAnthropicOAuthModel(body)
		body = encodeNormalizedAnthropicRequest(body)
	}
	fields := []string{
		"temperature", "top_p", "top_k", "stream", "stop_sequences", "stop", "max_tokens",
	}
	if isOfficialAnthropicURL(target) {
		fields = append(fields, "metadata", "context_management", "diagnostics")
	}
	for _, field := range fields {
		body = deleteJSONPath(body, field)
	}
	return body, nil
}

func isOfficialAnthropicURL(target *url.URL) bool {
	if target == nil || target.User != nil || !strings.EqualFold(target.Scheme, "https") ||
		!strings.EqualFold(strings.TrimSpace(target.Hostname()), "api.anthropic.com") {
		return false
	}
	port := target.Port()
	return port == "" || port == "443"
}

// validateAnthropicLegacySystemRequestForUpstream runs on the finished wire
// body. The incompatibility was measured only on Anthropic's first-party API;
// compatible gateways and confirmed native Claude Code callers own their wire.
func validateAnthropicLegacySystemRequestForUpstream(
	body []byte,
	callerOwnsWire bool,
	target *url.URL,
) error {
	if !isOfficialAnthropicURL(target) {
		return nil
	}
	if !isAnthropicJSONObject(body) {
		return nil
	}
	if callerOwnsWire {
		return nil
	}
	return validateAnthropicLegacySystemMessages(body)
}

// validateAnthropicOpus55Request mirrors Claude Code's request guard for the
// Opus 5.5 contract. It must run before the CLI finalizer normalizes thinking
// or tool_choice, otherwise an invalid caller request would be silently turned
// into a different request instead of receiving the same 400 as the native API.
func validateAnthropicOpus55Request(body []byte, requestModel string) error {
	modelName := strings.ToLower(strings.TrimSpace(requestModel))
	if modelName == "" && isAnthropicJSONObject(body) {
		modelName = strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "model"))))
	}
	if slash := strings.LastIndexByte(modelName, '/'); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	if !strings.HasPrefix(modelName, "claude-opus-5-5") {
		return nil
	}

	thinkingType := strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "thinking.type"))))
	if thinkingType == "disabled" || thinkingType == "enabled" {
		return &anthropicRequestValidationError{message: "claude-opus-5-5 requires adaptive thinking; omit thinking or use thinking.type=adaptive and output_config.effort"}
	}
	toolChoice := gjson.GetBytes(body, "tool_choice")
	if strings.EqualFold(strings.TrimSpace(jsonStringValue(toolChoice)), "required") {
		return &anthropicRequestValidationError{message: "claude-opus-5-5 does not support forced tool_choice; use auto or none"}
	}
	choiceType := strings.ToLower(strings.TrimSpace(jsonStringValue(toolChoice.Get("type"))))
	switch choiceType {
	case "any", "tool", "function", "custom", "namespace":
		return &anthropicRequestValidationError{message: "claude-opus-5-5 does not support forced tool_choice; use auto or none"}
	default:
		return nil
	}
}

func buildAnthropicOAuthURL(baseURL, requestPath, rawQuery string) string {
	upstreamURL := buildUpstreamURL(baseURL, requestPath, rawQuery)
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		return upstreamURL
	}
	query := parsed.Query()
	query.Set("beta", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// anthropicCCHSigningEnabled 仅保留旧版 Haiku helper 的签名策略。新生成的
// Claude Code billing block 跟随 sub2api 当前模拟路径，不再注入 CCH。
func anthropicCCHSigningEnabled(cfg *model.Config, target *url.URL) bool {
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		return true
	}
	return isOfficialAnthropicURL(target)
}

// finalizeAnthropicClaudeCodeMessagesBody 是 Anthropic Messages 上游 body 的唯一
// 最终化入口。原生 Claude Code 请求保留调用方 body/CCH；模拟请求生成无 CCH
// 的 billing block。
func finalizeAnthropicClaudeCodeMessagesBody(
	body []byte,
	cfg *model.Config,
	apiKey string,
	headers http.Header,
	target *url.URL,
) ([]byte, error) {
	return finalizeAnthropicClaudeCodeMessagesBodyForCaller(
		body, cfg, apiKey, headers, target, classifyAnthropicCallerWire(body, headers))
}

func finalizeAnthropicClaudeCodeMessagesBodyForCaller(
	body []byte,
	cfg *model.Config,
	apiKey string,
	headers http.Header,
	target *url.URL,
	callerWire anthropicCallerWire,
) ([]byte, error) {
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("finalize Anthropic Claude Code request: invalid JSON body")
	}
	helperShape := callerWire.haikuHelper
	if helperShape != anthropicHaikuHelperNone {
		return finishAnthropicPassthrough(body,
			helperShape == anthropicHaikuHelperStructured && anthropicCCHSigningEnabled(cfg, target))
	}
	if callerWire.nativeClaudeCode {
		return finishAnthropicPassthrough(body, false)
	}
	body = normalizeAnthropicOAuthModel(body)
	// 缓存窗口归调用方：调用方自己声明了 1h，网关注入的 breakpoint 就跟到 1h，否则
	// 保持默认 5m。Anthropic 按 tools → system → messages 顺序评估，网关注入的
	// system breakpoint 排在调用方 block 前面，不跟随就会被
	// normalizeAnthropicCacheControlTTL 连带把调用方的 1h 降级。跟随是对齐调用方
	// 已经做出的选择，不是替它升窗口——调用方没要 1h 时这里一律是 5m。
	cloakCacheTTL := ""
	if anthropicRequestHasCacheControl(body, anthropicCacheControlIsLongTTL) {
		cloakCacheTTL = "1h"
	}
	// 新增的顶层键按 sjson 的插入顺序落在对象尾部。
	originalSystem := anthropicSystemText(gjson.GetBytes(body, "system"))
	firstUserText := anthropicFirstUserText(gjson.GetBytes(body, "messages"))
	clientVersion := anthropicClientVersion(headers)
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		// sub2api 的模拟 UA 与 billing 同取本次运行期版本，不继承下游 UA。
		clientVersion = anthropicEffectiveCLIVersion()
	}
	systemBlocks := []string{
		anthropicTextBlockRaw(anthropicBillingHeader(firstUserText, clientVersion), ""),
		anthropicTextBlockRaw(anthropicClaudeCodeIdentityPrompt, ""),
	}
	// sub2api keeps only billing + identity for Fable: its generic CLI expansion
	// prompt can cause an otherwise valid request to be refused.
	if cfg == nil || !cfg.UsesAnthropicOAuth() ||
		!strings.Contains(strings.ToLower(jsonStringValue(gjson.GetBytes(body, "model"))), "fable") {
		systemBlocks = append(systemBlocks,
			anthropicTextBlockRaw(anthropicClaudeCodePrompt, anthropicCloakCacheControl(cloakCacheTTL)))
	}
	body = setJSONRaw(body, "system", "["+strings.Join(systemBlocks, ",")+"]")

	messagePrefixCount := 0
	if originalSystem != "" {
		prefix := []string{
			anthropicTextMessageRaw("user", "[System Instructions]\n"+originalSystem),
			anthropicTextMessageRaw("assistant", "Understood. I will follow these instructions."),
		}
		messages := append(prefix, anthropicRawArrayItems(gjson.GetBytes(body, "messages"))...)
		body = setJSONRaw(body, "messages", "["+strings.Join(messages, ",")+"]")
		messagePrefixCount = len(prefix)
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		body = setJSONRaw(body, "tools", "[]")
		tools = gjson.GetBytes(body, "tools")
	}
	if jsonMemberCount(tools) == 0 {
		body = deleteJSONPath(body, "tool_choice")
	}
	body, err := injectAnthropicClaudeCodeMetadata(body, cfg, apiKey, headers)
	if err != nil {
		return nil, err
	}
	autoContextManagement := false
	if !gjson.GetBytes(body, "context_management").Exists() && anthropicThinkingAcceptsContextManagement(body) {
		body = setJSONRaw(body, "context_management",
			`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
		autoContextManagement = true
	}
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		body = ensureAnthropicMimicToolCacheBreakpoint(body, cloakCacheTTL)
	}
	body = ensureAnthropicCloakedCacheBreakpoints(body, messagePrefixCount, cloakCacheTTL)
	// Forced tool choice strips thinking during normalization. Only withdraw
	// the object ccLoad injected; caller-owned context_management keeps its
	// ownership and is left untouched.
	body = normalizeAnthropicToolChoice(body)
	body = normalizeAnthropicThinking(body)
	if autoContextManagement && !anthropicThinkingAcceptsContextManagement(body) {
		body = deleteJSONPath(body, "context_management")
	}

	// Shared normalization removes temperature; OAuth mimic restores the caller's
	// value (or Claude Code's default) immediately below.
	callerTemperature := gjson.GetBytes(body, "temperature")
	body = encodeNormalizedAnthropicRequest(body)
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		if !gjson.GetBytes(body, "max_tokens").Exists() {
			body = setJSONRaw(body, "max_tokens", "128000")
		}
		if !strings.HasPrefix(strings.ToLower(jsonStringValue(gjson.GetBytes(body, "model"))), "claude-opus-5-5") {
			temperature := "1"
			if callerTemperature.Exists() {
				temperature = callerTemperature.Raw
			}
			body = setJSONRaw(body, "temperature", temperature)
		}
	}
	return finishAnthropicPassthrough(body, false)
}

// finishAnthropicPassthrough is the single Anthropic Messages outbound exit.
// Fingerprint rewrite (system cloak, sampling, cache stamps) happens before
// this, or is skipped for native Claude Code / Haiku helper. This layer only
// applies API-contract invariants and optional CCH, so new Anthropic 400 guards
// extend applyAnthropicMessagesAPIInvariants instead of growing every early-
// return branch.
func finishAnthropicPassthrough(body []byte, signCCH bool) ([]byte, error) {
	body = applyAnthropicMessagesAPIInvariants(body)
	if !signCCH {
		return body, nil
	}
	return finalizeAnthropicCCH(body)
}

// applyAnthropicMessagesAPIInvariants enforces Anthropic Messages request
// constraints that are independent of CLI fingerprint and session identity.
// Native passthrough skips fingerprint rewrite but still runs this layer.
func applyAnthropicMessagesAPIInvariants(body []byte) []byte {
	return sanitizeAnthropicEmptyTextBlocks(body)
}

// anthropicRawArrayItems 取出数组每个元素的原始字节。重建数组时逐个拼回，元素自身
// 的键序与格式因此原样保留。
func anthropicRawArrayItems(array gjson.Result) []string {
	if !array.IsArray() {
		return nil
	}
	items := array.Array()
	raw := make([]string, 0, len(items))
	for _, item := range items {
		raw = append(raw, item.Raw)
	}
	return raw
}

func anthropicThinkingAcceptsContextManagement(body []byte) bool {
	thinking := gjson.GetBytes(body, "thinking")
	if !thinking.IsObject() {
		return false
	}
	typ := strings.ToLower(strings.TrimSpace(jsonStringValue(thinking.Get("type"))))
	return typ == "enabled" || typ == "adaptive"
}

type anthropicHaikuHelperShape uint8

const (
	anthropicHaikuHelperNone anthropicHaikuHelperShape = iota
	anthropicHaikuHelperMinimal
	anthropicHaikuHelperStructured
	anthropicHaikuHelperModel = "claude-haiku-4-5-20251001"
)

var anthropicHaikuHelperBetaProfiles = map[string]anthropicHaikuHelperShape{
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05":                                                                                  anthropicHaikuHelperMinimal,
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05":                                                                                                             anthropicHaikuHelperMinimal,
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,structured-outputs-2025-12-15,cache-diagnosis-2026-04-07": anthropicHaikuHelperStructured,
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15,fallback-credit-2026-06-01":                         anthropicHaikuHelperStructured,
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15":                                                    anthropicHaikuHelperStructured,
	"oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15":                                                                               anthropicHaikuHelperStructured,
}

// nativeAnthropicHaikuHelperShape 识别 Claude Code 的内部 Haiku 辅助请求。
// 判定只看下游请求形态（UA、x-app、beta 组合指纹、JSON 键序、身份形态），与本渠道
// 用什么凭证无关——辅助请求经 OAuth 还是 API Key 渠道转发，形态都是同一份。
func nativeAnthropicHaikuHelperShape(body []byte, headers http.Header) anthropicHaikuHelperShape {
	if !validAnthropicClaudeCLIUserAgent(anthropicHeaderValue(headers, "User-Agent")) ||
		anthropicHeaderValue(headers, "X-App") != "cli" {
		return anthropicHaikuHelperNone
	}
	shape := anthropicHaikuHelperBetaProfiles[normalizedAnthropicBetaHeader(headers)]
	if shape == anthropicHaikuHelperNone || !matchesAnthropicHaikuHelperHeaders(headers, body, shape) {
		return anthropicHaikuHelperNone
	}
	if !matchesAnthropicHaikuHelperIdentityShape(body) {
		return anthropicHaikuHelperNone
	}
	if shape == anthropicHaikuHelperMinimal && matchesAnthropicMinimalHaikuHelper(body) {
		return shape
	}
	if shape == anthropicHaikuHelperStructured && matchesAnthropicStructuredHaikuHelper(body) {
		return shape
	}
	return anthropicHaikuHelperNone
}

func matchesAnthropicMinimalHaikuHelper(body []byte) bool {
	if !anthropicJSONObjectHasOrderedKeys(body, []string{"model", "max_tokens", "messages", "metadata"}) ||
		jsonStringValue(gjson.GetBytes(body, "model")) != anthropicHaikuHelperModel {
		return false
	}
	maxTokens, ok := jsonIntegerValue(gjson.GetBytes(body, "max_tokens"))
	messages := gjson.GetBytes(body, "messages")
	if !ok || maxTokens != 1 || !messages.IsArray() || jsonMemberCount(messages) != 1 {
		return false
	}
	message := messages.Array()[0]
	return message.IsObject() && jsonMemberCount(message) == 2 &&
		jsonStringValue(message.Get("role")) == "user" && message.Get("content").Type == gjson.String &&
		anthropicJSONArrayObjectHasOrderedKeys(body, "messages", 0, []string{"role", "content"})
}

func matchesAnthropicStructuredHaikuHelper(body []byte) bool {
	if !anthropicJSONObjectHasOrderedKeys(body, []string{
		"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking", "temperature", "output_config", "stream",
	}) || jsonStringValue(gjson.GetBytes(body, "model")) != anthropicHaikuHelperModel {
		return false
	}
	maxTokens, maxOK := jsonIntegerValue(gjson.GetBytes(body, "max_tokens"))
	temperature := gjson.GetBytes(body, "temperature")
	if !maxOK || maxTokens != 32000 || temperature.Type != gjson.Number || temperature.Float() != 1 ||
		gjson.GetBytes(body, "stream").Type != gjson.True {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() || jsonMemberCount(messages) != 1 {
		return false
	}
	message := messages.Array()[0]
	content := message.Get("content")
	if !message.IsObject() || jsonMemberCount(message) != 2 || jsonStringValue(message.Get("role")) != "user" ||
		!content.IsArray() || jsonMemberCount(content) != 1 ||
		!anthropicJSONArrayObjectHasOrderedKeys(body, "messages", 0, []string{"role", "content"}) {
		return false
	}
	text := content.Array()[0]
	if !text.IsObject() || jsonMemberCount(text) != 2 || jsonStringValue(text.Get("type")) != "text" ||
		!anthropicNestedArrayObjectHasOrderedKeys(body, []string{"messages", "0", "content"}, 0, []string{"type", "text"}) {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	thinking := gjson.GetBytes(body, "thinking")
	if !tools.IsArray() || jsonMemberCount(tools) != 0 || !thinking.IsObject() || jsonMemberCount(thinking) != 1 ||
		jsonStringValue(thinking.Get("type")) != "disabled" {
		return false
	}
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() || jsonMemberCount(system) != 3 ||
		!strings.HasPrefix(anthropicFirstSystemBlockText(system), "x-anthropic-billing-header:") {
		return false
	}
	if _, ok := anthropicCCHDigitsOffset(body); !ok {
		return false
	}
	if !strings.HasPrefix(anthropicTextBlock(system.Array()[1]), "You are Claude Code") {
		return false
	}
	format := gjson.GetBytes(body, "output_config.format")
	schema := format.Get("schema")
	required := schema.Get("required")
	return format.IsObject() && schema.IsObject() && schema.Get("properties").IsObject() &&
		schema.Get("properties.title").IsObject() && required.IsArray() && jsonMemberCount(required) == 1 &&
		jsonStringValue(required.Array()[0]) == "title" && jsonStringValue(format.Get("type")) == "json_schema" &&
		jsonStringValue(schema.Get("type")) == "object" &&
		jsonStringValue(schema.Get("properties.title.type")) == "string" &&
		schema.Get("additionalProperties").Type == gjson.False &&
		matchesAnthropicStructuredHaikuHelperObjectOrder(body)
}

func matchesAnthropicHaikuHelperIdentityShape(body []byte) bool {
	metadata, ok := anthropicJSONRawAtPath(body, "metadata")
	if !ok || !anthropicJSONObjectHasOrderedKeys(metadata, []string{"user_id"}) {
		return false
	}
	userID := jsonStringValue(gjson.GetBytes(metadata, "user_id"))
	if userID == "" || !gjson.Valid(userID) {
		return false
	}
	identity := []byte(userID)
	ordered := anthropicJSONObjectHasOrderedKeys(identity, []string{"device_id", "account_uuid", "session_id"}) ||
		anthropicJSONObjectHasOrderedKeys(identity, []string{"device_id", "account_uuid", "session_id", "parent_session_id"})
	if !ordered {
		return false
	}
	parsed := gjson.Parse(userID)
	deviceID := jsonStringValue(parsed.Get("device_id"))
	if len(deviceID) != 64 || strings.Trim(deviceID, "0123456789abcdef") != "" {
		return false
	}
	if _, err := uuid.Parse(jsonStringValue(parsed.Get("session_id"))); err != nil {
		return false
	}
	if accountUUID := jsonStringValue(parsed.Get("account_uuid")); accountUUID != "" {
		if _, err := uuid.Parse(accountUUID); err != nil {
			return false
		}
	}
	return true
}

func matchesAnthropicStructuredHaikuHelperObjectOrder(body []byte) bool {
	if raw, ok := anthropicJSONRawAtPath(body, "max_tokens"); !ok || string(raw) != "32000" {
		return false
	}
	if raw, ok := anthropicJSONRawAtPath(body, "temperature"); !ok || string(raw) != "1" {
		return false
	}
	for index := 0; index < 3; index++ {
		block, ok := anthropicJSONRawAtPath(body, "system", strconv.Itoa(index))
		if !ok || !anthropicJSONObjectHasOrderedKeys(block, []string{"type", "text"}) {
			return false
		}
	}
	checks := []struct {
		path []string
		keys []string
	}{
		{path: []string{"thinking"}, keys: []string{"type"}},
		{path: []string{"output_config"}, keys: []string{"format"}},
		{path: []string{"output_config", "format"}, keys: []string{"type", "schema"}},
		{path: []string{"output_config", "format", "schema"}, keys: []string{"type", "properties", "required", "additionalProperties"}},
		{path: []string{"output_config", "format", "schema", "properties"}, keys: []string{"title"}},
		{path: []string{"output_config", "format", "schema", "properties", "title"}, keys: []string{"type"}},
	}
	for _, check := range checks {
		raw, ok := anthropicJSONRawAtPath(body, check.path...)
		if !ok || !anthropicJSONObjectHasOrderedKeys(raw, check.keys) {
			return false
		}
	}
	return true
}

func anthropicJSONArrayObjectHasOrderedKeys(body []byte, field string, index int, keys []string) bool {
	raw, ok := anthropicJSONRawAtPath(body, field, strconv.Itoa(index))
	return ok && anthropicJSONObjectHasOrderedKeys(raw, keys)
}

func anthropicNestedArrayObjectHasOrderedKeys(body []byte, path []string, index int, keys []string) bool {
	path = append(append([]string(nil), path...), strconv.Itoa(index))
	raw, ok := anthropicJSONRawAtPath(body, path...)
	return ok && anthropicJSONObjectHasOrderedKeys(raw, keys)
}

func anthropicJSONRawAtPath(body []byte, path ...string) ([]byte, bool) {
	value := gjson.GetBytes(body, strings.Join(path, "."))
	if !value.Exists() {
		return nil, false
	}
	return []byte(value.Raw), true
}

// anthropicJSONObjectHasOrderedKeys 判定 raw 是否恰好是按 want 顺序排列的对象键。
// 用 gjson 保序遍历而非 json.Decoder 逐 token：后者为每个成员分配一份 RawMessage
// 并复制字节，而这里只需要键名。校验器取 gjson.ValidBytes 与遍历同源——它与
// encoding/json 的判定在本项目全部用例上一致，唯一的异类是更宽松的 sonic.Valid。
func anthropicJSONObjectHasOrderedKeys(raw []byte, want []string) bool {
	if !gjson.ValidBytes(raw) {
		return false
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return false
	}
	keyIndex := 0
	ordered := true
	root.ForEach(func(key, _ gjson.Result) bool {
		if keyIndex >= len(want) || key.String() != want[keyIndex] {
			ordered = false
			return false
		}
		keyIndex++
		return true
	})
	return ordered && keyIndex == len(want)
}

func anthropicTextBlock(value gjson.Result) string {
	return jsonStringValue(value.Get("text"))
}

func matchesAnthropicHaikuHelperHeaders(headers http.Header, body []byte, shape anthropicHaikuHelperShape) bool {
	expected := map[string]string{
		"Accept": "application/json", "Content-Type": "application/json", "X-Stainless-Lang": "js",
		"X-Stainless-Runtime": "node", "X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "600",
		"Anthropic-Version": "2023-06-01", "Anthropic-Dangerous-Direct-Browser-Access": "true",
	}
	for name, want := range expected {
		if anthropicHeaderValue(headers, name) != want {
			return false
		}
	}
	// SDK/运行时版本只校验形态：锁死某个版本会让每次客户端升级都静默丢失 helper 识别。
	if _, ok := parseAnthropicCLIVersion(anthropicHeaderValue(headers, "X-Stainless-Package-Version")); !ok {
		return false
	}
	if runtimeVersion, ok := strings.CutPrefix(anthropicHeaderValue(headers, "X-Stainless-Runtime-Version"), "v"); !ok {
		return false
	} else if _, ok := parseAnthropicCLIVersion(runtimeVersion); !ok {
		return false
	}
	for _, name := range []string{"X-Stainless-OS", "X-Stainless-Arch"} {
		if anthropicHeaderValue(headers, name) == "" {
			return false
		}
	}
	async := anthropicHeaderValue(headers, "X-Stainless-Async")
	compression := anthropicHeaderValue(headers, "Accept-Encoding")
	if (shape == anthropicHaikuHelperStructured && (async != "async" || compression != "gzip, deflate, br, zstd")) ||
		(shape == anthropicHaikuHelperMinimal && (async != "" || compression != "gzip")) {
		return false
	}
	if _, err := uuid.Parse(anthropicHeaderValue(headers, "X-Client-Request-Id")); err != nil {
		return false
	}
	return anthropicHeaderValue(headers, "X-Claude-Code-Session-Id") == anthropicSessionIDFromRequest(body)
}

func anthropicSessionIDFromRequest(body []byte) string {
	userID := jsonStringValue(gjson.GetBytes(body, "metadata.user_id"))
	if !gjson.Valid(userID) {
		return ""
	}
	return jsonStringValue(gjson.Get(userID, "session_id"))
}

func normalizedAnthropicBetaHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	rawValues := headers.Values("Anthropic-Beta")
	if len(rawValues) == 0 {
		keys := make([]string, 0, 2)
		for key := range headers {
			if strings.EqualFold(key, "Anthropic-Beta") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			rawValues = append(rawValues, headers[key]...)
		}
	}
	values := make([]string, 0, 12)
	for _, rawValue := range rawValues {
		for _, raw := range strings.Split(rawValue, ",") {
			if value := strings.TrimSpace(raw); value != "" {
				values = append(values, value)
			}
		}
	}
	return strings.Join(values, ",")
}

func anthropicHeaderValue(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func normalizeAnthropicOAuthModel(body []byte) []byte {
	switch strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "model"))) {
	case "claude-sonnet-4-5":
		return setJSONRaw(body, "model", `"claude-sonnet-4-5-20250929"`)
	case "claude-opus-4-5":
		return setJSONRaw(body, "model", `"claude-opus-4-5-20251101"`)
	case "claude-haiku-4-5":
		return setJSONRaw(body, "model", `"claude-haiku-4-5-20251001"`)
	default:
		return body
	}
}

// isNativeAnthropicClaudeCodeRequest 判断请求是否应按原生 Claude Code wire 直通。
//
// 直接请求对齐 sub2api 的最终转发判定：合法 Claude Code UA + 可解析的
// metadata.user_id。X-App 与 anthropic-beta 不参与这条判定；sub2api 仅在单独的
// Claude Code 请求验证器中要求它们非空，最终 OAuth 透传还有此简化分支。
//
// 如果请求经过其他网关，UA 可能被替换成 Go-http-client。此时沿用 sub2api 的恢复规则：
// body 仍带非空 metadata.user_id，且 system 中保留 billing attribution block，就认为
// 它仍是调用方拥有 wire 的 Claude Code 请求。该分支只用于恢复 UA 丢失场景，不会单凭
// billing block 识别没有 metadata 的请求。
func isNativeAnthropicClaudeCodeRequest(body []byte, headers http.Header) bool {
	userID := anthropicMetadataUserID(body)
	if validAnthropicClaudeCLIUserAgent(anthropicHeaderValue(headers, "User-Agent")) &&
		validAnthropicClaudeCodeMetadataUserID(userID) {
		return true
	}
	return strings.TrimSpace(userID) != "" && anthropicSystemHasBillingAttributionBlock(body)
}

func validAnthropicClaudeCLIUserAgent(userAgent string) bool {
	return anthropicClaudeCLIUserAgentPattern.MatchString(strings.TrimSpace(userAgent))
}

func anthropicMetadataUserID(body []byte) string {
	if !isAnthropicJSONObject(body) {
		return ""
	}
	userID := gjson.GetBytes(body, "metadata.user_id")
	if userID.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(userID.String())
}

func validAnthropicClaudeCodeMetadataUserID(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if strings.HasPrefix(raw, "{") && gjson.Valid(raw) {
		identity := gjson.Parse(raw)
		deviceID := identity.Get("device_id")
		sessionID := identity.Get("session_id")
		return deviceID.Type == gjson.String && deviceID.String() != "" &&
			sessionID.Type == gjson.String && sessionID.String() != ""
	}
	return anthropicClaudeCodeLegacyMetadataUserIDPattern.MatchString(raw)
}

func anthropicSystemHasBillingAttributionBlock(body []byte) bool {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		return false
	}
	for _, item := range system.Array() {
		text := jsonStringValue(item.Get("text"))
		if strings.HasPrefix(text, "x-anthropic-billing-header") && strings.Contains(text, "cc_entrypoint=") {
			return true
		}
	}
	return false
}

func anthropicFirstSystemBlockText(system gjson.Result) string {
	if !system.IsArray() {
		return ""
	}
	blocks := system.Array()
	if len(blocks) == 0 {
		return ""
	}
	return anthropicTextBlock(blocks[0])
}

// injectAnthropicClaudeCodeMetadata 写入 metadata.user_id。身份 JSON 的键序
// device_id → account_uuid → session_id 是契约的一部分：
// matchesAnthropicHaikuHelperIdentityShape 正是按这个顺序识别原生请求，用 map 编码
// 会按字母序排成 account_uuid → device_id → session_id，与原生形态对不上。
func injectAnthropicClaudeCodeMetadata(
	body []byte,
	cfg *model.Config,
	apiKey string,
	headers http.Header,
) ([]byte, error) {
	// sub2api 的 OAuth 模拟路径只在缺失时注入 metadata.user_id。
	if cfg != nil && cfg.UsesAnthropicOAuth() && anthropicMetadataUserID(body) != "" {
		return body, nil
	}
	credential := anthropicCredentialForWire(cfg, apiKey)
	if credential == nil {
		return nil, errors.New("finalize Anthropic Claude Code request: credential identity is incomplete")
	}
	identitySeed := anthropicOAuthIdentitySeed(credential)
	if credential.DeviceID == "" || identitySeed == "" {
		return nil, errors.New("finalize Anthropic Claude Code request: credential identity is incomplete")
	}
	deviceID := credential.DeviceID
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		// 原生与模拟请求共用同一个账号级 ClientID。
		deviceID = anthropicSub2APIClientID(cfg.ID, identitySeed)
	}
	sessionID := anthropicSessionIDFromHeaders(headers)
	if sessionID == "" {
		seed := identitySeed
		if cfg != nil && cfg.UsesAnthropicOAuth() {
			seed = strconv.FormatInt(cfg.ID, 10) + "\x00" + deviceID
		}
		sessionID = anthropicStableSessionID(seed, anthropicFirstUserText(gjson.GetBytes(body, "messages")))
	}
	identity := "{}"
	var err error
	for _, field := range []struct{ key, value string }{
		{"device_id", deviceID},
		{"account_uuid", credential.AccountUUID},
		{"session_id", sessionID},
	} {
		if identity, err = sjson.Set(identity, field.key, field.value); err != nil {
			return nil, errors.New("finalize Anthropic Claude Code request: encode credential identity")
		}
	}
	updated, err := sjson.SetBytes(body, "metadata.user_id", identity)
	if err != nil {
		return nil, errors.New("finalize Anthropic Claude Code request: encode credential identity")
	}
	return updated, nil
}

// anthropicCredentialForWire 解析 Claude Code 指纹使用的凭证身份。
//
// OAuth 渠道从凭证取账号身份，再按渠道稳定派生模拟客户端 ID；API Key 渠道
// （含第三方网关）按 Key 稳定派生身份。合成身份复用 anthropicauth.Credential。
func anthropicCredentialForWire(cfg *model.Config, apiKey string) *anthropicauth.Credential {
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		if strings.TrimSpace(cfg.OAuthCredential) == "" {
			return nil
		}
		credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil
		}
		return credential
	}
	return synthesizeAnthropicAPIKeyCredential(apiKey)
}

// rebaseAnthropicNativeOAuthIdentity applies the account identity policy used by
// sub2api to a confirmed native Claude Code request. The selected OAuth account
// owns the device/client id, account UUID and session namespace; the caller's
// session tail is retained so retries and subsequent turns stay in one logical
// conversation while account switching cannot leak the source account.
func rebaseAnthropicNativeOAuthIdentity(
	body, callerBody []byte,
	cfg *model.Config,
	callerWire anthropicCallerWire,
	incoming http.Header,
) ([]byte, string, error) {
	if cfg == nil || !cfg.UsesAnthropicOAuth() || !callerWire.nativeClaudeCode {
		return body, "", nil
	}
	credential := anthropicCredentialForWire(cfg, "")
	if credential == nil {
		return body, "", nil
	}
	// setup-token 等凭证没有账号 UUID 时也要重写：account_uuid 发空（与模拟路径及
	// 真实 CLI 一致），device 用账号级 ClientID，调用方自己的身份不能原样透传。
	targetAccount := credential.AccountUUID
	current, currentValid := parseAnthropicSub2APIUserIdentity(anthropicMetadataUserID(body))
	if !currentValid {
		// An explicit body rule can remove metadata.user_id. sub2api only
		// rewrites an identity that is present in the final body, so do not
		// recreate a field the caller deliberately removed.
		return body, "", nil
	}

	// sub2api keeps a random 64-hex ClientID in an account fingerprint cache.
	// ccLoad has no equivalent external cache, so derive the same account-scoped
	// value deterministically from the stable channel/account identity.
	clientID := anthropicSub2APIClientID(cfg.ID, anthropicOAuthIdentitySeed(credential))
	parsed := current
	if source, sourceValid := parseAnthropicSub2APIUserIdentity(anthropicMetadataUserID(callerBody)); sourceValid {
		// A retry may start from an already rewritten upstream body. Reuse the
		// original session only when the current identity is exactly the mapping
		// we produced; otherwise honor a caller/body-rule change in the final body.
		if current.deviceID == clientID && current.accountUUID == targetAccount &&
			current.sessionID == anthropicSub2APISessionID(cfg.ID, source.sessionID) {
			parsed = source
		}
	}

	// sub2api derives UUID(v4-shaped) sessions from accountID::originalSession.
	mappedSession := anthropicSub2APISessionID(cfg.ID, parsed.sessionID)

	version := anthropicUserAgentVersion(incoming)
	if version == "" {
		version = anthropicBillingVersion(body)
	}
	if version == "" {
		version = anthropicEffectiveCLIVersion()
	}
	identity := anthropicFormatSub2APIUserIdentity(clientID, targetAccount, mappedSession, version)
	updated, err := sjson.SetBytes(body, "metadata.user_id", identity)
	if err != nil {
		return nil, "", errors.New("rebase Anthropic native identity: encode user_id")
	}
	return updated, mappedSession, nil
}

// anthropicOAuthIdentitySeed 是账号级身份：优先账号 UUID，setup-token 等只有邮箱的凭证用邮箱。
func anthropicOAuthIdentitySeed(credential *anthropicauth.Credential) string {
	if credential.AccountUUID != "" {
		return credential.AccountUUID
	}
	return strings.ToLower(credential.EmailAddress)
}

func anthropicSub2APIClientID(channelID int64, accountUUID string) string {
	seed := "ccload:anthropic:sub2api-client-id\x00" + strconv.FormatInt(channelID, 10) + "\x00" + accountUUID
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:])
}

func anthropicSub2APISessionID(channelID int64, originalSession string) string {
	seed := strconv.FormatInt(channelID, 10) + "::" + originalSession
	digest := sha256.Sum256([]byte(seed))
	var mapped uuid.UUID
	copy(mapped[:], digest[:len(mapped)])
	mapped[6] = (mapped[6] & 0x0f) | 0x40
	mapped[8] = (mapped[8] & 0x3f) | 0x80
	return mapped.String()
}

type anthropicSub2APIUserIdentity struct {
	deviceID    string
	accountUUID string
	sessionID   string
}

// parseAnthropicSub2APIUserIdentity mirrors sub2api's parser: JSON identities
// only require non-empty device/session values, while the legacy form keeps its
// 64-hex device and 36-character session contract.
func parseAnthropicSub2APIUserIdentity(raw string) (anthropicSub2APIUserIdentity, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return anthropicSub2APIUserIdentity{}, false
	}
	if strings.HasPrefix(raw, "{") && gjson.Valid(raw) {
		identity := gjson.Parse(raw)
		deviceID := jsonStringValue(identity.Get("device_id"))
		sessionID := jsonStringValue(identity.Get("session_id"))
		if deviceID == "" || sessionID == "" {
			return anthropicSub2APIUserIdentity{}, false
		}
		return anthropicSub2APIUserIdentity{
			deviceID:    deviceID,
			accountUUID: jsonStringValue(identity.Get("account_uuid")),
			sessionID:   sessionID,
		}, true
	}
	if !anthropicClaudeCodeLegacyMetadataUserIDPattern.MatchString(raw) {
		return anthropicSub2APIUserIdentity{}, false
	}
	accountAt := strings.Index(raw, "_account_")
	sessionAt := strings.LastIndex(raw, "_session_")
	return anthropicSub2APIUserIdentity{
		deviceID:    raw[len("user_"):accountAt],
		accountUUID: raw[accountAt+len("_account_") : sessionAt],
		sessionID:   raw[sessionAt+len("_session_"):],
	}, true
}

func anthropicFormatSub2APIUserIdentity(deviceID, accountUUID, sessionID, version string) string {
	if anthropicCLIVersionGTE(version, "2.1.78") {
		identity := "{}"
		identity, _ = sjson.Set(identity, "device_id", deviceID)
		identity, _ = sjson.Set(identity, "account_uuid", accountUUID)
		identity, _ = sjson.Set(identity, "session_id", sessionID)
		return identity
	}
	return "user_" + deviceID + "_account_" + accountUUID + "_session_" + sessionID
}

func synthesizeAnthropicAPIKeyCredential(apiKey string) *anthropicauth.Credential {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	device := sha256.Sum256([]byte("ccload:anthropic:device\x00" + apiKey))
	return &anthropicauth.Credential{
		AccountUUID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ccload:anthropic:account\x00"+apiKey)).String(),
		DeviceID:    hex.EncodeToString(device[:]),
	}
}

func anthropicStableSessionID(accountUUID, firstUserText string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(accountUUID+"\x00"+firstUserText)).String()
}

func anthropicSessionIDFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if nativeSessionID := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id")); nativeSessionID != "" {
		if parsed, err := uuid.Parse(nativeSessionID); err == nil {
			return parsed.String()
		}
	}
	seed := responsesExecutionSessionID(headers)
	if seed == "" {
		seed = strings.TrimSpace(headers.Get("Session_id"))
		if seed != "" {
			if threadID := strings.TrimSpace(headers.Get("Thread-Id")); threadID != "" {
				seed += "\x00thread\x00" + threadID
			}
		}
	}
	if seed == "" {
		return ""
	}
	if parsed, err := uuid.Parse(seed); err == nil {
		return parsed.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("ccload:anthropic:session\x00"+seed)).String()
}

func sanitizeAnthropicOAuthMessages(body []byte) []byte {
	body = applyAnthropicMessagesAPIInvariants(body)
	var deletions []string
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		for index, tool := range tools.Array() {
			if !tool.IsObject() || !strings.HasPrefix(jsonStringValue(tool.Get("type")), "web_search_") {
				continue
			}
			for _, field := range []string{"allowed_domains", "blocked_domains"} {
				if domains := tool.Get(field); domains.IsArray() && jsonMemberCount(domains) == 0 {
					deletions = append(deletions, "tools."+strconv.Itoa(index)+"."+field)
				}
			}
		}
	}
	for _, path := range deletions {
		body = deleteJSONPath(body, path)
	}
	return body
}

// sanitizeAnthropicEmptyTextBlocks drops empty Anthropic `text` blocks that
// the Messages API rejects with 400 "text content blocks must be non-empty".
// Native Claude Code often serializes tool_use-only assistant turns as
// `{"type":"text","text":""}` plus `tool_use`; only those empty text blocks
// (and empty nested tool_result text) are removed. Sampling, tools,
// cache_control on remaining blocks, and key order of untouched members are
// left as-is. Messages that would become an empty content array are not
// rewritten, so role alternation is preserved.
func sanitizeAnthropicEmptyTextBlocks(body []byte) []byte {
	if !isAnthropicJSONObject(body) {
		return body
	}
	var patches []anthropicRawPatch
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for index, message := range messages.Array() {
			if !message.IsObject() {
				continue
			}
			cleaned, changed := stripEmptyAnthropicTextBlocks(message.Get("content"))
			if !changed || cleaned == "" || cleaned == "[]" {
				continue
			}
			patches = append(patches, anthropicRawPatch{
				path: "messages." + strconv.Itoa(index) + ".content", raw: cleaned,
			})
		}
	}
	if system := gjson.GetBytes(body, "system"); system.IsArray() {
		cleaned, changed := stripEmptyAnthropicTextBlocks(system)
		if changed && cleaned != "" && cleaned != "[]" {
			patches = append(patches, anthropicRawPatch{path: "system", raw: cleaned})
		}
	}
	for _, patch := range patches {
		body = setJSONRaw(body, patch.path, patch.raw)
	}
	return body
}

// anthropicRawPatch 是「先遍历收集、后统一改写」的一条待写记录。遍历读的是入参
// 快照，边遍历边改写会让后续路径指向旧字节。
type anthropicRawPatch struct{ path, raw string }

// stripEmptyAnthropicTextBlocks 删除空 text 块并递归清理 tool_result。返回值是新的
// 数组原始 JSON；第二个返回值为 false 表示没有任何块被删除，调用方不必改写字节——
// 保留原字节才能让未触及的成员键序原样过关。
func stripEmptyAnthropicTextBlocks(blocks gjson.Result) (string, bool) {
	if !blocks.IsArray() {
		return "", false
	}
	items := blocks.Array()
	kept := make([]string, 0, len(items))
	changed := false
	for _, block := range items {
		if !block.IsObject() {
			kept = append(kept, block.Raw)
			continue
		}
		switch jsonStringValue(block.Get("type")) {
		case "text":
			if strings.TrimSpace(jsonStringValue(block.Get("text"))) == "" {
				changed = true
				continue
			}
		case "tool_result":
			nested, nestedChanged := stripEmptyAnthropicTextBlocks(block.Get("content"))
			if nestedChanged {
				if updated, err := sjson.SetRaw(block.Raw, "content", nested); err == nil {
					kept = append(kept, updated)
					changed = true
					continue
				}
			}
		}
		kept = append(kept, block.Raw)
	}
	if !changed {
		return "", false
	}
	return "[" + strings.Join(kept, ",") + "]", true
}

// ensureAnthropicCloakedCacheBreakpoints mirrors Claude Code's independent
// system and rolling-message selectors. OAuth tool breakpoints are handled
// separately to cover stable tool declarations as sub2api does.
// cacheTTL 跟随调用方声明的缓存窗口（空即默认 5m），见 anthropicCloakCacheControl。
func ensureAnthropicCloakedCacheBreakpoints(body []byte, skipMessagePrefix int, cacheTTL string) []byte {
	cacheControl := anthropicCloakCacheControl(cacheTTL)
	if system := gjson.GetBytes(body, "system"); system.IsArray() {
		hasBreakpoint := false
		lastObject := -1
		for index, block := range system.Array() {
			if !block.IsObject() {
				continue
			}
			if block.Get("cache_control").Exists() {
				hasBreakpoint = true
				break
			}
			lastObject = index
		}
		if !hasBreakpoint && lastObject >= 0 {
			body = setJSONRaw(body, "system."+strconv.Itoa(lastObject)+".cache_control", cacheControl)
		}
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	items := messages.Array()
	lastEligible := -1
	for index := len(items) - 1; index >= skipMessagePrefix; index-- {
		message := items[index]
		if !message.IsObject() {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(jsonStringValue(message.Get("role"))))
		if role != "user" && role != "assistant" {
			continue
		}
		if anthropicMessageEligibleForRollingCache(message, role) {
			lastEligible = index
			break
		}
	}
	if lastEligible < 0 {
		return body
	}
	if lastIndex := len(items) - 1; lastIndex >= skipMessagePrefix {
		final := items[lastIndex]
		if final.IsObject() && strings.EqualFold(jsonStringValue(final.Get("role")), "system") {
			content := final.Get("content")
			if content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
				return setJSONRaw(body, "messages."+strconv.Itoa(lastIndex)+".content",
					"["+anthropicTextBlockRaw(content.String(), cacheControl)+"]")
			}
		}
	}
	target := "messages." + strconv.Itoa(lastEligible) + ".content"
	content := items[lastEligible].Get("content")
	switch {
	case content.Type == gjson.String:
		return setJSONRaw(body, target, "["+anthropicTextBlockRaw(content.String(), cacheControl)+"]")
	case content.IsArray():
		blocks := content.Array()
		for _, block := range blocks {
			if block.IsObject() && block.Get("cache_control").Exists() {
				return body
			}
		}
		for index := len(blocks) - 1; index >= 0; index-- {
			if blocks[index].IsObject() {
				return setJSONRaw(body, target+"."+strconv.Itoa(index)+".cache_control", cacheControl)
			}
		}
	}
	return body
}

func ensureAnthropicMimicToolCacheBreakpoint(body []byte, cacheTTL string) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	lastEligible := -1
	for index, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		if tool.Get("defer_loading").Bool() || tool.Get("custom.defer_loading").Bool() {
			body = deleteJSONPath(body, "tools."+strconv.Itoa(index)+".cache_control")
			continue
		}
		lastEligible = index
	}
	if lastEligible < 0 {
		return body
	}
	path := "tools." + strconv.Itoa(lastEligible) + ".cache_control"
	if gjson.GetBytes(body, path).Exists() {
		return body
	}
	return setJSONRaw(body, path, anthropicCloakCacheControl(cacheTTL))
}

func anthropicMessageEligibleForRollingCache(message gjson.Result, role string) bool {
	content := message.Get("content")
	switch {
	case content.Type == gjson.String:
		return true
	case content.IsArray():
		blocks := content.Array()
		if len(blocks) == 0 {
			return false
		}
		if role != "assistant" {
			return true
		}
		typ := strings.ToLower(strings.TrimSpace(jsonStringValue(blocks[len(blocks)-1].Get("type"))))
		return typ != "thinking" && typ != "redacted_thinking"
	default:
		return false
	}
}

// orderAnthropicCacheControlWireShape 把每个 cache_control 的成员归一成原生键序
// type → ttl → scope → 其余（字母序）。调用方送来的顺序是任意的，而上游按 body
// 形态识别 Claude Code；只重排 cache_control 本身，其余字节一律不动。
func orderAnthropicCacheControlWireShape(body []byte) []byte {
	var patches []anthropicRawPatch
	forEachAnthropicCacheBlock(body, func(path string, block gjson.Result) bool {
		cache := block.Get("cache_control")
		if !cache.IsObject() {
			return true
		}
		if ordered, changed := orderedAnthropicCacheControlRaw(cache); changed {
			patches = append(patches, anthropicRawPatch{path: path + ".cache_control", raw: ordered})
		}
		return true
	})
	for _, patch := range patches {
		body = setJSONRaw(body, patch.path, patch.raw)
	}
	return body
}

// orderedAnthropicCacheControlRaw 按原生键序重拼 cache_control。成员用 gjson 的
// key.Raw / value.Raw 原样搬运，所以转义与数字字面量都不会在重排中漂移；第二个
// 返回值为 false 表示顺序已经正确，调用方不必改写字节。
func orderedAnthropicCacheControlRaw(cache gjson.Result) (string, bool) {
	type member struct{ key, rawKey, rawValue string }
	members := make([]member, 0, 3)
	cache.ForEach(func(key, value gjson.Result) bool {
		members = append(members, member{key.String(), key.Raw, value.Raw})
		return true
	})
	rank := func(key string) int {
		switch key {
		case "type":
			return 0
		case "ttl":
			return 1
		case "scope":
			return 2
		default:
			return 3
		}
	}
	ordered := make([]member, len(members))
	copy(ordered, members)
	sort.SliceStable(ordered, func(left, right int) bool {
		if rank(ordered[left].key) != rank(ordered[right].key) {
			return rank(ordered[left].key) < rank(ordered[right].key)
		}
		return ordered[left].key < ordered[right].key
	})
	changed := false
	for index := range ordered {
		if ordered[index].key != members[index].key {
			changed = true
			break
		}
	}
	if !changed {
		return "", false
	}
	var out strings.Builder
	out.WriteByte('{')
	for index, entry := range ordered {
		if index > 0 {
			out.WriteByte(',')
		}
		out.WriteString(entry.rawKey)
		out.WriteByte(':')
		out.WriteString(entry.rawValue)
	}
	out.WriteByte('}')
	return out.String(), true
}

// anthropicCloakCacheControl 生成网关注入 breakpoint 用的 cache_control 原始 JSON。
// ttl 为空即 Anthropic 默认的 5m 窗口；只有调用方自己声明了 1h 才会传 "1h"。
func anthropicCloakCacheControl(ttl string) string {
	if ttl == "" {
		return anthropicEphemeralCacheControl()
	}
	cache, err := sjson.Set(anthropicEphemeralCacheControl(), "ttl", ttl)
	if err != nil {
		return anthropicEphemeralCacheControl()
	}
	return cache
}

// anthropicRequestHasCacheControl 判断 body 里是否存在满足 match 的 cache_control。
// 缓存窗口归调用方所有：网关不主动改写 5m/1h，所以既要按 body 实际用到的 ttl 决定
// beta，也要按调用方声明的 1h 决定自己注入的 breakpoint 跟到哪个窗口。
func anthropicRequestHasCacheControl(body []byte, match func(cache gjson.Result) bool) bool {
	found := false
	forEachAnthropicCacheBlock(body, func(_ string, block gjson.Result) bool {
		cache := block.Get("cache_control")
		if cache.IsObject() && match(cache) {
			found = true
			return false
		}
		return true
	})
	return found
}

// anthropicCacheControlHasTTL 命中任何显式 ttl 字段。
func anthropicCacheControlHasTTL(cache gjson.Result) bool { return cache.Get("ttl").Exists() }

// anthropicCacheControlIsLongTTL 只命中 1h 窗口。
func anthropicCacheControlIsLongTTL(cache gjson.Result) bool {
	return jsonStringValue(cache.Get("ttl")) == "1h"
}

func enforceAnthropicCacheControlLimit(body []byte, limit int) []byte {
	if limit < 0 {
		limit = 0
	}
	var tools, system, messages []string
	forEachAnthropicCacheBlock(body, func(path string, block gjson.Result) bool {
		if !block.Get("cache_control").Exists() {
			return true
		}
		switch {
		case strings.HasPrefix(path, "tools."):
			tools = append(tools, path)
		case strings.HasPrefix(path, "system."):
			system = append(system, path)
		default:
			messages = append(messages, path)
		}
		return true
	})
	excess := len(tools) + len(system) + len(messages) - limit
	if excess <= 0 {
		return body
	}
	stripped := make(map[string]bool, excess)
	remove := func(paths []string) {
		for _, path := range paths {
			if excess <= 0 {
				return
			}
			if stripped[path] {
				continue
			}
			stripped[path] = true
			body = deleteJSONPath(body, path+".cache_control")
			excess--
		}
	}
	// Preserve the last tool and last system breakpoint as long as possible;
	// each one covers the complete prefix of its section.
	if len(system) > 1 {
		remove(system[:len(system)-1])
	}
	if len(tools) > 1 {
		remove(tools[:len(tools)-1])
	}
	remove(messages)
	remove(system)
	remove(tools)
	return body
}

func anthropicSystemText(system gjson.Result) string {
	switch {
	case system.Type == gjson.String:
		return strings.TrimSpace(system.String())
	case system.IsArray():
		blocks := system.Array()
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if !block.IsObject() {
				continue
			}
			if text := jsonStringValue(block.Get("text")); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func anthropicFirstUserText(messages gjson.Result) string {
	if !messages.IsArray() {
		return ""
	}
	for _, message := range messages.Array() {
		if !message.IsObject() || jsonStringValue(message.Get("role")) != "user" {
			continue
		}
		content := message.Get("content")
		switch {
		case content.Type == gjson.String:
			return content.String()
		case content.IsArray():
			for _, block := range content.Array() {
				if !block.IsObject() || jsonStringValue(block.Get("type")) != "text" {
					continue
				}
				if text := block.Get("text"); text.Type == gjson.String {
					return text.String()
				}
			}
		}
	}
	return ""
}

func anthropicBillingHeader(firstUserText, clientVersion string) string {
	return "x-anthropic-billing-header: cc_version=" + clientVersion + "." + anthropicBillingFingerprint(firstUserText, clientVersion) + "; cc_entrypoint=cli;"
}

func anthropicBillingFingerprint(firstUserText, clientVersion string) string {
	padded := []byte(firstUserText + strings.Repeat("0", 21))
	selected := []byte{padded[4], padded[7], padded[20]}
	digest := sha256.Sum256(append([]byte(anthropicBillingSalt), append(selected, []byte(clientVersion)...)...))
	return hex.EncodeToString(digest[:])[:3]
}

func anthropicBillingVersion(body []byte) string {
	billing := jsonStringValue(gjson.GetBytes(body, "system.0.text"))
	if !strings.HasPrefix(billing, "x-anthropic-billing-header:") {
		return ""
	}
	match := anthropicBillingVersionPattern.FindStringSubmatch(billing)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

// syncAnthropicOAuthBillingVersion keeps native billing attribution in step
// with the account fingerprint applied to the outgoing User-Agent header.
func syncAnthropicOAuthBillingVersion(body []byte, userAgent string) []byte {
	version := anthropicUserAgentVersion(http.Header{"User-Agent": {userAgent}})
	if version == "" {
		return body
	}
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		return body
	}
	fingerprint := anthropicBillingFingerprint(anthropicFirstUserText(gjson.GetBytes(body, "messages")), version)
	for index, block := range system.Array() {
		field := block.Get("text")
		if field.Type != gjson.String || !strings.HasPrefix(field.String(), "x-anthropic-billing-header") {
			continue
		}
		updated := anthropicBillingFingerprintPattern.ReplaceAllString(field.String(), "cc_version="+version+"."+fingerprint)
		updated = anthropicBillingVersionPattern.ReplaceAllString(updated, "cc_version="+version)
		if updated != field.String() {
			if rewritten, err := sjson.SetBytes(body, "system."+strconv.Itoa(index)+".text", updated); err == nil {
				body = rewritten
			}
		}
	}
	return body
}

func anthropicUserAgentVersion(headers http.Header) string {
	matches := anthropicClaudeCLIUserAgentPattern.FindStringSubmatch(
		strings.TrimSpace(anthropicHeaderValue(headers, "User-Agent")),
	)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func anthropicCLIVersionGTE(version, min string) bool {
	v, vok := parseAnthropicCLIVersion(version)
	m, mok := parseAnthropicCLIVersion(min)
	if !vok || !mok {
		return false
	}
	for i := 0; i < 3; i++ {
		if v[i] != m[i] {
			return v[i] > m[i]
		}
	}
	return true
}

func parseAnthropicCLIVersion(version string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// anthropicClientVersion keeps a current Claude Code version on the wire.
// Callers older than anthropicCLIVersion are raised to the pin so Anthropic
// does not 400 new models; newer official CLIs keep their real version.
func anthropicClientVersion(headers http.Header) string {
	floor := anthropicEffectiveCLIVersion()
	if version := anthropicUserAgentVersion(headers); version != "" &&
		anthropicCLIVersionGTE(version, floor) {
		return version
	}
	return floor
}

// OAuth fingerprints are persisted with the private credential and cached per server.
type anthropicOAuthFingerprint = anthropicauth.Fingerprint

// Claude Code 2.1.280（内置 CLI 版本下限）实测随附的 @anthropic-ai/sdk 与运行时版本（对齐 CPA）。
// CLI 版本只升不降，SDK 版本随之单调，低于这一组的 Stainless 版本不可能与当前 UA 配套。
const (
	anthropicStainlessPackageVersion = "0.112.1"
	anthropicStainlessRuntimeVersion = "v26.3.0"
)

func defaultAnthropicOAuthFingerprint() anthropicOAuthFingerprint {
	return anthropicOAuthFingerprint{
		UserAgent:               "claude-cli/" + anthropicEffectiveCLIVersion() + " (external, cli)",
		StainlessLang:           "js",
		StainlessPackageVersion: anthropicStainlessPackageVersion,
		StainlessOS:             "Linux",
		StainlessArch:           "arm64",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: anthropicStainlessRuntimeVersion,
	}
}

// resetStaleAnthropicStainlessVersions 把低于内置配对的 SDK/运行时版本换成内置配对。
// 旧默认值（0.94.0）和旧客户端学到的版本都会随 UA 抬升到下限而失配。
func resetStaleAnthropicStainlessVersions(fp *anthropicOAuthFingerprint) {
	if fp.StainlessPackageVersion != "" && !anthropicCLIVersionGTE(fp.StainlessPackageVersion, anthropicStainlessPackageVersion) {
		fp.StainlessPackageVersion = anthropicStainlessPackageVersion
		fp.StainlessRuntimeVersion = anthropicStainlessRuntimeVersion
	}
}

func anthropicOAuthFingerprintKey(cfg *model.Config) string {
	if cfg == nil {
		return ""
	}
	identity := ""
	if credential := anthropicCredentialForWire(cfg, ""); credential != nil {
		identity = anthropicOAuthIdentitySeed(credential)
	}
	if identity == "" {
		identity = strings.TrimSpace(cfg.Name)
	}
	return strconv.FormatInt(cfg.ID, 10) + "\x00" + identity
}

func anthropicAcceptableFingerprintUserAgent(userAgent string) bool {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" || len(userAgent) > 256 {
		return false
	}
	matches := anthropicFingerprintUserAgentPattern.FindStringSubmatch(userAgent)
	if len(matches) != 4 {
		return false
	}
	version, ok := parseAnthropicCLIVersion(matches[2])
	if !ok {
		return false
	}
	current, currentOK := parseAnthropicCLIVersion(anthropicEffectiveCLIVersion())
	if !currentOK {
		return true
	}
	return anthropicFingerprintVersionPlausible(version, current)
}

// anthropicFingerprintMaxPatchLead bounds how far a client may lead the runtime
// version. A genuine client leads it only by releases the hourly sync has not
// seen yet; an adopted sentinel such as 2.999.0 would otherwise pin the account
// forever, because the floor only ever raises a stored version.
const anthropicFingerprintMaxPatchLead = 100

func anthropicFingerprintVersionPlausible(version, current [3]int) bool {
	switch {
	case version[0] != current[0]:
		return false
	case version[1] < current[1]:
		return true
	case version[1] == current[1]:
		return version[2] <= current[2]+anthropicFingerprintMaxPatchLead
	case version[1] == current[1]+1:
		return version[2] <= anthropicFingerprintMaxPatchLead
	default:
		return false
	}
}

func normalizeAnthropicFingerprintUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	matches := anthropicFingerprintUserAgentPattern.FindStringSubmatch(userAgent)
	if len(matches) != 4 {
		return defaultAnthropicOAuthFingerprint().UserAgent
	}
	effective := anthropicEffectiveCLIVersion()
	if anthropicCLIVersionGTE(matches[2], effective) {
		return userAgent
	}
	return matches[1] + effective + matches[3]
}

func anthropicFingerprintVersionIsNewer(candidate, cached string) bool {
	candidateVersion := anthropicUserAgentVersion(http.Header{"User-Agent": []string{candidate}})
	cachedVersion := anthropicUserAgentVersion(http.Header{"User-Agent": []string{cached}})
	return candidateVersion != "" && cachedVersion != "" &&
		candidateVersion != cachedVersion && anthropicCLIVersionGTE(candidateVersion, cachedVersion)
}

func anthropicFingerprintHeadersFromIncoming(fp *anthropicOAuthFingerprint, incoming http.Header) bool {
	if fp == nil {
		return false
	}
	ua := anthropicHeaderValue(incoming, "User-Agent")
	if !anthropicAcceptableFingerprintUserAgent(ua) {
		return false
	}
	fp.UserAgent = normalizeAnthropicFingerprintUserAgent(ua)
	mergeAnthropicOAuthFingerprintHeaders(fp, incoming)
	return true
}

func mergeAnthropicOAuthFingerprintHeaders(fp *anthropicOAuthFingerprint, incoming http.Header) {
	if fp == nil {
		return
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"X-Stainless-Lang", &fp.StainlessLang},
		{"X-Stainless-Package-Version", &fp.StainlessPackageVersion},
		{"X-Stainless-OS", &fp.StainlessOS},
		{"X-Stainless-Arch", &fp.StainlessArch},
		{"X-Stainless-Runtime", &fp.StainlessRuntime},
		{"X-Stainless-Runtime-Version", &fp.StainlessRuntimeVersion},
	} {
		if value := strings.TrimSpace(anthropicHeaderValue(incoming, field.name)); value != "" {
			*field.value = value
		}
	}
}

// getAnthropicOAuthFingerprint keeps one account identity across requests and
// restarts. Only a newer, well-formed Claude CLI UA may replace it.
func (s *Server) getAnthropicOAuthFingerprint(ctx context.Context, cfg *model.Config, incoming http.Header) *anthropicOAuthFingerprint {
	fallback := defaultAnthropicOAuthFingerprint()
	if s == nil || cfg == nil || !cfg.UsesAnthropicOAuth() {
		return &fallback
	}
	key := anthropicOAuthFingerprintKey(cfg)
	s.anthropicOAuthFingerprintMu.Lock()
	if s.anthropicOAuthFingerprints == nil {
		s.anthropicOAuthFingerprints = make(map[string]anthropicOAuthFingerprint)
	}
	if s.anthropicOAuthFingerprintDirty == nil {
		s.anthropicOAuthFingerprintDirty = make(map[string]bool)
	}
	if s.anthropicOAuthFingerprintSaving == nil {
		s.anthropicOAuthFingerprintSaving = make(map[string]bool)
	}
	if s.anthropicOAuthFingerprintRetryAt == nil {
		s.anthropicOAuthFingerprintRetryAt = make(map[string]time.Time)
	}
	cached, exists := s.anthropicOAuthFingerprints[key]
	if !exists {
		if credential := anthropicCredentialForWire(cfg, ""); credential != nil && credential.Fingerprint != nil {
			cached = *credential.Fingerprint
			exists = true
		}
	}
	previous := cached
	if !exists || !anthropicAcceptableFingerprintUserAgent(cached.UserAgent) {
		cached = fallback
		anthropicFingerprintHeadersFromIncoming(&cached, incoming)
	} else {
		// Raise an older stored version to the runtime floor without changing its
		// suffix. The floor is re-applied on every read, so raising it is not an
		// account change: persisting it would rewrite every OAuth credential and
		// flush the channel cache once per account after each version sync.
		cached.UserAgent = normalizeAnthropicFingerprintUserAgent(cached.UserAgent)
		resetStaleAnthropicStainlessVersions(&cached)
		previous.UserAgent = cached.UserAgent
		previous.StainlessPackageVersion = cached.StainlessPackageVersion
		previous.StainlessRuntimeVersion = cached.StainlessRuntimeVersion
		if candidate := strings.TrimSpace(anthropicHeaderValue(incoming, "User-Agent")); anthropicAcceptableFingerprintUserAgent(candidate) {
			candidate = normalizeAnthropicFingerprintUserAgent(candidate)
			if anthropicFingerprintVersionIsNewer(candidate, cached.UserAgent) || cached == fallback {
				cached.UserAgent = candidate
				mergeAnthropicOAuthFingerprintHeaders(&cached, incoming)
			} else if candidate == cached.UserAgent {
				// A native client at the same version can correct stale header metadata.
				mergeAnthropicOAuthFingerprintHeaders(&cached, incoming)
			}
		}
	}
	cached = completeAnthropicOAuthFingerprint(cached, fallback)
	s.anthropicOAuthFingerprints[key] = cached
	changed := !exists || cached != previous
	save := (changed || s.anthropicOAuthFingerprintDirty[key]) &&
		!s.anthropicOAuthFingerprintSaving[key] &&
		(changed || !time.Now().Before(s.anthropicOAuthFingerprintRetryAt[key]))
	if save {
		s.anthropicOAuthFingerprintSaving[key] = true
	}
	s.anthropicOAuthFingerprintMu.Unlock()

	if save {
		persisted, err := s.persistAnthropicOAuthFingerprint(ctx, cfg, cached)
		s.anthropicOAuthFingerprintMu.Lock()
		delete(s.anthropicOAuthFingerprintSaving, key)
		latest := s.anthropicOAuthFingerprints[key]
		if err != nil {
			s.anthropicOAuthFingerprintDirty[key] = true
			s.anthropicOAuthFingerprintRetryAt[key] = time.Now().Add(time.Minute)
		} else {
			delete(s.anthropicOAuthFingerprintRetryAt, key)
			if anthropicFingerprintVersionIsNewer(persisted.UserAgent, latest.UserAgent) || latest == cached {
				latest = persisted
				s.anthropicOAuthFingerprints[key] = latest
			}
			if latest == persisted {
				delete(s.anthropicOAuthFingerprintDirty, key)
			} else {
				s.anthropicOAuthFingerprintDirty[key] = true
			}
		}
		s.anthropicOAuthFingerprintMu.Unlock()
		if err != nil {
			log.Printf("[WARN] persist Anthropic OAuth fingerprint for channel %d: %v", cfg.ID, err)
		}
		cached = latest
	}
	copy := cached
	return &copy
}

func completeAnthropicOAuthFingerprint(fp, fallback anthropicOAuthFingerprint) anthropicOAuthFingerprint {
	resetStaleAnthropicStainlessVersions(&fp)
	if fp.StainlessLang == "" {
		fp.StainlessLang = fallback.StainlessLang
	}
	if fp.StainlessPackageVersion == "" {
		fp.StainlessPackageVersion = fallback.StainlessPackageVersion
	}
	if fp.StainlessOS == "" {
		fp.StainlessOS = fallback.StainlessOS
	}
	if fp.StainlessArch == "" {
		fp.StainlessArch = fallback.StainlessArch
	}
	if fp.StainlessRuntime == "" {
		fp.StainlessRuntime = fallback.StainlessRuntime
	}
	if fp.StainlessRuntimeVersion == "" {
		fp.StainlessRuntimeVersion = fallback.StainlessRuntimeVersion
	}
	return fp
}

func (s *Server) persistAnthropicOAuthFingerprint(ctx context.Context, cfg *model.Config, proposed anthropicOAuthFingerprint) (anthropicOAuthFingerprint, error) {
	if s.store == nil || cfg.ID <= 0 || cfg.OAuthCredential == "" {
		return proposed, nil
	}
	currentCfg := cfg
	for attempts := 0; attempts < 3; attempts++ {
		if err := ctx.Err(); err != nil {
			return proposed, err
		}
		credential, err := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
		if err != nil {
			return proposed, err
		}
		chosen := proposed
		if credential.Fingerprint != nil && anthropicAcceptableFingerprintUserAgent(credential.Fingerprint.UserAgent) {
			stored := completeAnthropicOAuthFingerprint(*credential.Fingerprint, defaultAnthropicOAuthFingerprint())
			stored.UserAgent = normalizeAnthropicFingerprintUserAgent(stored.UserAgent)
			if anthropicFingerprintVersionIsNewer(stored.UserAgent, proposed.UserAgent) {
				chosen = stored
			}
		}
		if credential.Fingerprint != nil && *credential.Fingerprint == chosen {
			return chosen, nil
		}
		credential.Fingerprint = &chosen
		payload, err := credential.JSON()
		if err != nil {
			return proposed, err
		}
		updated, err := s.store.CompareAndSwapOAuthCredential(ctx, currentCfg.ID, model.AuthTypeAnthropicOAuth, currentCfg.OAuthCredential, payload)
		if err != nil {
			return proposed, err
		}
		if updated {
			s.InvalidateChannelListCache()
			return chosen, nil
		}
		currentCfg, err = s.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return proposed, err
		}
		if !currentCfg.UsesAnthropicOAuth() {
			return proposed, errors.New("anthropic OAuth channel changed while saving fingerprint")
		}
		if anthropicOAuthFingerprintKey(currentCfg) != anthropicOAuthFingerprintKey(cfg) {
			return proposed, errors.New("anthropic OAuth account changed while saving fingerprint")
		}
	}
	return proposed, errors.New("anthropic OAuth fingerprint changed during save")
}

func applyAnthropicOAuthFingerprint(req *http.Request, fingerprint *anthropicOAuthFingerprint) {
	if req == nil {
		return
	}
	if fingerprint == nil {
		fallback := defaultAnthropicOAuthFingerprint()
		fingerprint = &fallback
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"User-Agent", fingerprint.UserAgent},
		{"X-Stainless-Lang", fingerprint.StainlessLang},
		{"X-Stainless-Package-Version", fingerprint.StainlessPackageVersion},
		{"X-Stainless-OS", fingerprint.StainlessOS},
		{"X-Stainless-Arch", fingerprint.StainlessArch},
		{"X-Stainless-Runtime", fingerprint.StainlessRuntime},
		{"X-Stainless-Runtime-Version", fingerprint.StainlessRuntimeVersion},
	} {
		if strings.TrimSpace(field.value) != "" {
			setRawHeader(req.Header, field.name, field.value)
		}
	}
}

func injectAnthropicOAuthHeadersWithFingerprint(
	req *http.Request,
	cfg *model.Config,
	accessToken string,
	body []byte,
	callerOwnsWire bool,
	fingerprint *anthropicOAuthFingerprint,
	incomingHeaders ...http.Header,
) {
	if req == nil {
		return
	}
	incoming := anthropicIncomingHeaders(req, incomingHeaders)
	if callerOwnsWire {
		applyAnthropicNativeHeaders(req, incoming)
		applyAnthropicNativeOAuthDefaults(req, body, fingerprint)
		setRawHeader(req.Header, "Anthropic-Beta", anthropicNativeOAuthCredentialBetas(
			normalizedAnthropicBetaHeader(req.Header), normalizedAnthropicBetaHeader(incoming), incoming, body,
		))
		applyAnthropicFirstPartyTransportHeaders(req)
		setRawHeader(req.Header, "Authorization", "Bearer "+strings.TrimSpace(accessToken))
		return
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	setRawHeader(req.Header, "Authorization", "Bearer "+strings.TrimSpace(accessToken))
	clientVersion := anthropicBillingVersion(body)
	if clientVersion == "" {
		clientVersion = anthropicEffectiveCLIVersion()
	}
	applyAnthropicClaudeCodeHeaders(
		req, anthropicOAuthMimicBetas(), "", clientVersion,
		anthropicRequestIsStreaming(body), true,
	)
}

// Header rules run after the wire headers are rebuilt. Restore only the
// identity fields so custom rules can still control unrelated headers.
func applyAnthropicOAuthMimicFingerprint(req *http.Request, body []byte) {
	fingerprint := defaultAnthropicOAuthFingerprint()
	if version := anthropicBillingVersion(body); version != "" {
		fingerprint.UserAgent = "claude-cli/" + version + " (external, cli)"
	}
	applyAnthropicOAuthFingerprint(req, &fingerprint)
}

// injectAnthropicAPIKeyHeaders 为 API Key 渠道重建 Claude Code CLI 请求头。
// CLI 能力头与 OAuth 共用，认证头走 applyAnthropicAPIKeyAuth；body 的 CCH 已在最终化边界排除。
func injectAnthropicAPIKeyHeaders(
	req *http.Request,
	cfg *model.Config,
	apiKey string,
	body []byte,
	callerOwnsWire bool,
	incomingHeaders ...http.Header,
) {
	if req == nil {
		return
	}
	incoming := anthropicIncomingHeaders(req, incomingHeaders)
	if callerOwnsWire {
		applyAnthropicNativeHeaders(req, incoming)
		applyAnthropicFirstPartyTransportHeaders(req)
		applyAnthropicAPIKeyAuth(req, apiKey)
		return
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	applyAnthropicAPIKeyAuth(req, apiKey)
	applyAnthropicClaudeCodeHeaders(
		req, anthropicClaudeCodeBetas(body), resolveAnthropicSessionID(body, cfg, apiKey, incoming),
		anthropicClientVersion(incoming), anthropicRequestIsStreaming(body), false,
	)
}

func injectAnthropicCountTokensHeadersWithFingerprint(req *http.Request, cfg *model.Config, apiKey string, body []byte, callerOwnsWire bool, fingerprint *anthropicOAuthFingerprint, incoming http.Header) {
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		if callerOwnsWire {
			applyAnthropicNativeHeaders(req, incoming)
			if normalizedAnthropicBetaHeader(incoming) == "" {
				setRawHeader(req.Header, "Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
			}
			applyAnthropicNativeOAuthDefaults(req, body, fingerprint)
			applyAnthropicFirstPartyTransportHeaders(req)
			betas := normalizedAnthropicBetaHeader(req.Header)
			setRawHeader(req.Header, "Anthropic-Beta", appendAnthropicBeta(betas, "token-counting-2024-11-01"))
		} else {
			for name := range req.Header {
				delete(req.Header, name)
			}
			betas := anthropicOAuthMimicBetas()
			for _, beta := range strings.Split(normalizedAnthropicBetaHeader(incoming), ",") {
				betas = appendAnthropicBeta(betas, beta)
			}
			applyAnthropicClaudeCodeHeaders(req, appendAnthropicBeta(betas, "token-counting-2024-11-01"),
				"", anthropicEffectiveCLIVersion(), false, true)
		}
		setRawHeader(req.Header, "Authorization", "Bearer "+strings.TrimSpace(apiKey))
		return
	}
	applyAnthropicNativeHeaders(req, incoming)
	if anthropicHeaderValue(req.Header, "Content-Type") == "" {
		setRawHeader(req.Header, "Content-Type", "application/json")
	}
	if anthropicHeaderValue(req.Header, "Anthropic-Version") == "" {
		setRawHeader(req.Header, "Anthropic-Version", "2023-06-01")
	}
	applyAnthropicAPIKeyAuth(req, apiKey)
}

func appendAnthropicBeta(betas, token string) string {
	token = strings.TrimSpace(token)
	if token == "" || slices.Contains(strings.Split(betas, ","), token) {
		return betas
	}
	if betas == "" {
		return token
	}
	return betas + "," + token
}

func anthropicIncomingHeaders(req *http.Request, override []http.Header) http.Header {
	if len(override) > 0 && override[0] != nil {
		return override[0]
	}
	return req.Header.Clone()
}

type anthropicCallerWire struct {
	haikuHelper      anthropicHaikuHelperShape
	nativeClaudeCode bool
}

func (wire anthropicCallerWire) ownsWire() bool {
	return wire.haikuHelper != anthropicHaikuHelperNone || wire.nativeClaudeCode
}

// classifyAnthropicCallerWire 只读取改写前的调用方 body，避免把网关注入的
// metadata/billing 或 BodyRules 的修改重新解释为另一种调用方身份。
func classifyAnthropicCallerWire(body []byte, incoming http.Header) anthropicCallerWire {
	if !isAnthropicJSONObject(body) {
		return anthropicCallerWire{}
	}
	return anthropicCallerWire{
		haikuHelper:      nativeAnthropicHaikuHelperShape(body, incoming),
		nativeClaudeCode: isNativeAnthropicClaudeCodeRequest(body, incoming),
	}
}

// classifyAnthropicRequestCallerWire is the single caller-wire classification
// shared by the proxy and admin-test paths. Claude Code sends count_tokens
// without a billing block, so there an untransformed Anthropic body with a
// Claude CLI UA is the native signal.
func classifyAnthropicRequestCallerWire(
	body []byte,
	incoming http.Header,
	upstreamProtocol protocol.Protocol,
	requestPath string,
	callerBodyIsAnthropic bool,
) anthropicCallerWire {
	wire := classifyAnthropicCallerWire(body, incoming)
	if callerBodyIsAnthropic && isAnthropicCountTokensRequest(upstreamProtocol, requestPath) &&
		validAnthropicClaudeCLIUserAgent(anthropicHeaderValue(incoming, "User-Agent")) {
		wire.nativeClaudeCode = true
	}
	return wire
}

// anthropicAPIKeyAuthorizationAllowed 判断 x-api-key 之外能否再带 Bearer。第一方
// API 只认 x-api-key，多带一个 Authorization 会被拒；第三方网关两种形态都可能认，
// 都给才不挑上游。策略与写法分离：通用转发路径用 canonical 头，Claude Code 指纹
// 路径用 raw 头，两边共用这一条判定。
func anthropicAPIKeyAuthorizationAllowed(target *url.URL) bool {
	return !isOfficialAnthropicURL(target)
}

// applyAnthropicAPIKeyAuth 以 Claude Code CLI 的 raw 头形态重建 API Key 认证头。
func applyAnthropicAPIKeyAuth(req *http.Request, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	setRawHeader(req.Header, "x-api-key", apiKey)
	if !anthropicAPIKeyAuthorizationAllowed(req.URL) {
		deleteRawHeader(req.Header, "Authorization")
		return
	}
	setRawHeader(req.Header, "Authorization", "Bearer "+apiKey)
}

func applyAnthropicNativeHeaders(req *http.Request, incoming http.Header) {
	for name := range req.Header {
		delete(req.Header, name)
	}
	for _, name := range []string{
		"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Sec-Fetch-Mode", "User-Agent", "X-App", "Anthropic-Beta", "Anthropic-Version",
		"Anthropic-Dangerous-Direct-Browser-Access", "X-Claude-Code-Session-Id", "X-Client-Request-Id",
		"X-Stainless-Async", "X-Stainless-Lang", "X-Stainless-Runtime", "X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Retry-Count", "X-Stainless-Timeout",
		"X-Stainless-Helper-Method",
	} {
		if value := anthropicHeaderValue(incoming, name); value != "" {
			setRawHeader(req.Header, name, value)
		}
	}
	// Claude Code 的 agent/subagent、remote、请求类别与压缩标记都在这些前缀下；
	// 丢掉它们，上游就分不清 subagent 与压缩请求。
	for name, values := range incoming {
		lower := strings.ToLower(name)
		passthrough := strings.HasPrefix(lower, "x-claude-code-") || strings.HasPrefix(lower, "x-claude-remote-") ||
			lower == "x-client-app" || lower == "x-anthropic-additional-protection"
		if !passthrough || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
			continue
		}
		if anthropicHeaderValue(req.Header, name) == "" {
			setRawHeader(req.Header, name, strings.TrimSpace(values[0]))
		}
	}
}

// applyAnthropicFirstPartyTransportHeaders 补齐原生客户端直连第一方时才会发的传输头：
// Claude Code 只对第一方 base URL 生成 x-client-request-id，经网关转发时自然缺失。
func applyAnthropicFirstPartyTransportHeaders(req *http.Request) {
	if !isOfficialAnthropicURL(req.URL) {
		return
	}
	if anthropicHeaderValue(req.Header, "X-Client-Request-Id") == "" {
		setRawHeader(req.Header, "x-client-request-id", uuid.NewString())
	}
	setRawHeader(req.Header, "Connection", "keep-alive")
}

// anthropicNativeOAuthCredentialBetas 补回 OAuth 凭证本身的 beta（对齐 CPA
// withClaudeOAuthCredentialBetas）。调用方以为连的是 API Key 上游，不会声明
// oauth/extended-cache-ttl；真实 OAuth 客户端从不以「Bearer + 无这两项」形态出现。
// 调用方已声明 oauth 的（OAuth 登录的客户端、实测 Haiku helper）原样保留；
// subagent（未自带 1h ttl）与 max_tokens=1 探针、标题 helper 本来就不带 extended-cache-ttl。
func anthropicNativeOAuthCredentialBetas(betas, callerBetas string, incoming http.Header, body []byte) string {
	if callerBetas == "" || slices.Contains(strings.Split(callerBetas, ","), "oauth-2025-04-20") {
		return betas
	}
	if gjson.GetBytes(body, "max_tokens").Int() == 1 || anthropicIsTitleHelperRequest(body) {
		return betas
	}
	subagent := anthropicHeaderValue(incoming, "X-Claude-Code-Agent-Id") != "" ||
		anthropicHeaderValue(incoming, "X-Claude-Code-Parent-Agent-Id") != "" ||
		strings.Contains(anthropicMetadataUserID(body), "parent_session_id") ||
		strings.Contains(anthropicFirstSystemBlockText(gjson.GetBytes(body, "system")), "cc_is_subagent=true")
	if subagent && !anthropicRequestHasCacheControl(body, anthropicCacheControlIsLongTTL) {
		return betas
	}
	return appendAnthropicBeta(betas, "extended-cache-ttl-2025-04-11")
}

// anthropicIsTitleHelperRequest 识别 Claude Code 生成会话标题的结构化输出 helper。
func anthropicIsTitleHelperRequest(body []byte) bool {
	properties := gjson.GetBytes(body, "output_config.format.schema.properties")
	return properties.IsObject() && len(properties.Map()) == 1 && properties.Get("title").Exists()
}

// applyAnthropicNativeOAuthDefaults 与 sub2api 的 OAuth 透传路径一样，只补调用方
// 缺失的必需头。UA 被中间网关替换时恢复 CLI UA；已有的原生值保持不动。
func applyAnthropicNativeOAuthDefaults(req *http.Request, body []byte, fingerprint *anthropicOAuthFingerprint) {
	// sub2api applies the cached account fingerprint before filling generic
	// Claude OAuth defaults. This deliberately overrides a native client's
	// per-request UA/Stainless values after the account fingerprint is known.
	applyAnthropicOAuthFingerprint(req, fingerprint)
	for _, header := range [][2]string{
		{"Accept", "application/json"}, {"Content-Type", "application/json"},
		{"Anthropic-Version", "2023-06-01"}, {"X-App", "cli"},
		{"X-Stainless-Lang", "js"}, {"X-Stainless-Package-Version", anthropicStainlessPackageVersion},
		{"X-Stainless-OS", "Linux"}, {"X-Stainless-Arch", "arm64"},
		{"X-Stainless-Runtime", "node"}, {"X-Stainless-Runtime-Version", anthropicStainlessRuntimeVersion},
		{"X-Stainless-Retry-Count", "0"}, {"X-Stainless-Timeout", "600"},
		{"Anthropic-Dangerous-Direct-Browser-Access", "true"},
	} {
		if anthropicHeaderValue(req.Header, header[0]) == "" {
			setRawHeader(req.Header, header[0], header[1])
		}
	}
	betas := normalizedAnthropicBetaHeader(req.Header)
	if betas == "" {
		if strings.Contains(strings.ToLower(jsonStringValue(gjson.GetBytes(body, "model"))), "haiku") {
			betas = "oauth-2025-04-20,interleaved-thinking-2025-05-14"
		} else {
			betas = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
		}
	} else if !slices.Contains(strings.Split(betas, ","), "oauth-2025-04-20") {
		parts := strings.Split(betas, ",")
		if index := slices.Index(parts, "claude-code-20250219"); index >= 0 {
			parts = slices.Insert(parts, index+1, "oauth-2025-04-20")
		} else {
			parts = append([]string{"oauth-2025-04-20"}, parts...)
		}
		betas = strings.Join(parts, ",")
	}
	setRawHeader(req.Header, "Anthropic-Beta", betas)
}

// anthropicOAuthMimicBetas 与 sub2api 的 FullClaudeCodeMimicryBetas 保持
// 相同集合和顺序。OAuth 模拟请求不继承第三方客户端的 beta。
func anthropicOAuthMimicBetas() string {
	return "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14," +
		"prompt-caching-scope-2026-01-05,effort-2025-11-24,context-management-2025-06-27," +
		"thinking-binding-controls-2026-08-01,mid-conversation-output-config-2026-07-01," +
		"extended-cache-ttl-2025-04-11"
}

// anthropicClaudeCodeBetas 为 API Key 模拟请求按实际使用的能力声明 beta。
// oauth-2025-04-20 只属于 OAuth 凭证，真实 CLI 用 API Key 时不发。
func anthropicClaudeCodeBetas(body []byte) string {
	betas := []string{
		"claude-code-20250219", "interleaved-thinking-2025-05-14",
		"prompt-caching-scope-2026-01-05", "effort-2025-11-24", "context-management-2025-06-27",
	}
	if gjson.GetBytes(body, "thinking.block_binding").Exists() {
		betas = append(betas, "thinking-binding-controls-2026-08-01")
	}
	if anthropicHasMessageOutputConfig(body) {
		betas = append(betas, "mid-conversation-output-config-2026-07-01")
	}
	if strings.EqualFold(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "speed"))), "fast") {
		betas = append(betas, "fast-mode-2026-02-01")
	}
	if anthropicRequestHasCacheControl(body, anthropicCacheControlHasTTL) {
		betas = append(betas, "extended-cache-ttl-2025-04-11")
	}
	if gjson.GetBytes(body, "diagnostics").IsObject() {
		betas = append(betas, "cache-diagnosis-2026-04-07")
	}
	return strings.Join(betas, ",")
}

func anthropicHasMessageOutputConfig(body []byte) bool {
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if strings.EqualFold(strings.TrimSpace(jsonStringValue(message.Get("role"))), "system") &&
			message.Get("output_config").IsObject() {
			return true
		}
	}
	return false
}

func anthropicUsesLegacySystemReminder(modelName string) bool {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if slash := strings.LastIndexByte(modelName, '/'); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	switch modelName {
	case "claude-3-5-haiku-20241022", "claude-3-5-haiku-latest",
		"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest",
		"claude-haiku-4-5", "claude-haiku-4-5-20251001",
		"claude-opus-4", "claude-opus-4-20250514", "claude-opus-4-1",
		"claude-opus-4-1-20250805", "claude-opus-4-5", "claude-opus-4-5-20251101",
		"claude-opus-4-6", "claude-opus-4-7", "claude-sonnet-4",
		"claude-sonnet-4-20250514", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929",
		"claude-sonnet-4-6":
		return true
	default:
		return false
	}
}

func applyAnthropicClaudeCodeHeaders(req *http.Request, betas, sessionID, clientVersion string, isStreaming, oauthMimic bool) {
	setRawHeader(req.Header, "Accept", "application/json")
	if isStreaming {
		setRawHeader(req.Header, "x-stainless-helper-method", "stream")
	}
	setRawHeader(req.Header, "Content-Type", "application/json")
	setRawHeader(req.Header, "User-Agent", "claude-cli/"+clientVersion+" (external, cli)")
	if !oauthMimic {
		setRawHeader(req.Header, "X-Claude-Code-Session-Id", sessionID)
	}
	// 平台与 OAuth 默认指纹一致固定；取网关宿主机会让同一渠道的指纹随部署机器漂移。
	setRawHeader(req.Header, "X-Stainless-Arch", "arm64")
	setRawHeader(req.Header, "X-Stainless-Lang", "js")
	setRawHeader(req.Header, "X-Stainless-OS", "Linux")
	setRawHeader(req.Header, "X-Stainless-Package-Version", anthropicStainlessPackageVersion)
	setRawHeader(req.Header, "X-Stainless-Retry-Count", "0")
	setRawHeader(req.Header, "X-Stainless-Runtime", "node")
	setRawHeader(req.Header, "X-Stainless-Runtime-Version", anthropicStainlessRuntimeVersion)
	setRawHeader(req.Header, "X-Stainless-Timeout", "600")
	setRawHeader(req.Header, "anthropic-beta", betas)
	setRawHeader(req.Header, "anthropic-dangerous-direct-browser-access", "true")
	setRawHeader(req.Header, "anthropic-version", "2023-06-01")
	setRawHeader(req.Header, "x-app", "cli")
	setRawHeader(req.Header, "x-client-request-id", uuid.NewString())
	if !oauthMimic {
		setRawHeader(req.Header, "Connection", "keep-alive")
		setRawHeader(req.Header, "Accept-Encoding", "gzip, deflate, br, zstd")
	}
}

func anthropicRequestIsStreaming(body []byte) bool {
	return gjson.GetBytes(body, "stream").Bool()
}

// setRawHeader 以给定大小写写入请求头。Claude Code CLI 的线上头名全部小写，Go 的
// http.Header.Set 会做 canonical 化，所以指纹路径必须直接操作 map。
func setRawHeader(headers http.Header, name, value string) {
	deleteRawHeader(headers, name)
	headers[name] = []string{value}
}

// deleteRawHeader 按大小写不敏感删除请求头（http.Header.Del 只认 canonical 键）。
func deleteRawHeader(headers http.Header, name string) {
	for existing := range headers {
		if strings.EqualFold(existing, name) {
			delete(headers, existing)
		}
	}
}

// resolveAnthropicSessionID 解析写入 X-Claude-Code-Session-Id 的会话 ID。
//
// 优先级：下游显式声明的 header → body 的 metadata.user_id.session_id → 凭证身份
// 与首条用户消息稳定派生 → 随机。body 这一级不能省：finalizeAnthropicOAuthMessages
// Body 先把 session_id 写进 metadata.user_id，这里读回来才能保证 header 与 body 同值。
func resolveAnthropicSessionID(body []byte, cfg *model.Config, apiKey string, headers http.Header) string {
	if sessionID := anthropicSessionIDFromHeaders(headers); sessionID != "" {
		return sessionID
	}
	if sessionID := anthropicSessionIDFromBody(body); sessionID != "" {
		return sessionID
	}
	if credential := anthropicCredentialForWire(cfg, apiKey); credential != nil && credential.AccountUUID != "" {
		return anthropicStableSessionID(
			credential.AccountUUID, anthropicFirstUserText(gjson.GetBytes(body, "messages")),
		)
	}
	return uuid.NewString()
}

func anthropicSessionIDFromBody(body []byte) string {
	userID := jsonStringValue(gjson.GetBytes(body, "metadata.user_id"))
	if gjson.Valid(userID) {
		sessionID := jsonStringValue(gjson.Get(userID, "session_id"))
		if parsed, err := uuid.Parse(strings.TrimSpace(sessionID)); err == nil {
			return parsed.String()
		}
	}
	if marker := strings.LastIndex(userID, "_session_"); marker >= 0 {
		if parsed, err := uuid.Parse(strings.TrimSpace(userID[marker+len("_session_"):])); err == nil {
			return parsed.String()
		}
	}
	return ""
}
