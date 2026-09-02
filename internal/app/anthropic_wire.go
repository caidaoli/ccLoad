package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	anthropicCLIVersion  = "2.1.220"
	anthropicBillingSalt = "59cf53e54c78"

	// anthropicClaudeCodeIdentityPrompt 是 Claude Code CLI system 三段式的第二段。
	anthropicClaudeCodeIdentityPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
)

// anthropicClaudeCLIUserAgentPattern 对齐上游 claudeCodeNativeUserAgentPattern，
// 捕获组 1 是版本、2 是 entrypoint。
var anthropicClaudeCLIUserAgentPattern = regexp.MustCompile(
	`(?i)^claude-cli/([0-9]+\.[0-9]+\.[0-9]+)\s+\(external,\s*([^,)]+?)\s*(?:,\s*agent-sdk/[0-9]+\.[0-9]+\.[0-9]+\s*)?\)$`)

// nativeAnthropicClaudeEntrypoints 对齐上游 nativeClaudeEntrypoints：只有 wire 形态
// 被实测确认过的第一方入口才允许直通。
var nativeAnthropicClaudeEntrypoints = map[string]bool{
	"cli":           true,
	"sdk-cli":       true,
	"claude-vscode": true,
}

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
// 共用 CLI wire 形状；CCH 另由 Claude OAuth 凭证边界决定。唯一例外是 Z.ai Coding Plan：它也走 anthropic
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
	cfg *model.Config,
	apiKey string,
	headers http.Header,
	target *url.URL,
) error {
	if !isOfficialAnthropicURL(target) {
		return nil
	}
	if !isAnthropicJSONObject(body) {
		return nil
	}
	if nativeAnthropicHaikuHelperShape(body, headers) != anthropicHaikuHelperNone ||
		isNativeAnthropicClaudeCodeRequest(headers) {
		return nil
	}
	return validateAnthropicLegacySystemMessages(body)
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

// anthropicCCHSigningEnabled 是 CCH 签名策略的唯一判据，对齐上游 CLIProxyAPI 的
// claudeCCHSigningEnabled（internal/runtime/executor/claude_signing.go）。
//
// 原生 gate（Claude Code 2.1.220–2.1.234）：
//
//	s = (provider === "firstParty" && isFirstPartyBaseURL()) || provider === "vertex"
//	      ? " cch=00000;" : ""
//
// 映射到两条判据：
//
//   - Claude OAuth 凭证：任何上游都签。ccLoad 是恢复第一方形态的那一跳——下游
//     Claude Code 指向 ccLoad 时看到的是非第一方 base URL，因此自己省略了 cch，
//     这个值必须在这里重新生成，而不是继承。理由是指纹保真，不是缓存。
//   - 其余凭证：ccLoad 无条件为 Anthropic 渠道套 CLI 指纹（等价上游
//     cliFingerprint=true），因此只在第一方 origin 签。第三方网关把 billing block
//     当普通 prompt 文本，每请求变化的 cch 会打散它的 prompt cache。
//
// 上游的 Vertex 分支不适用：ccLoad 没有 Anthropic Vertex 上游。
func anthropicCCHSigningEnabled(cfg *model.Config, target *url.URL) bool {
	if cfg != nil && cfg.UsesAnthropicOAuth() {
		return true
	}
	return isOfficialAnthropicURL(target)
}

// finalizeAnthropicClaudeCodeMessagesBody 是 Anthropic Messages 上游 body 的唯一
// 最终化入口。OAuth 与 API Key 共用同一套 CLI wire 形状与 anthropic-beta 集合；
// 唯一按凭证/origin 条件化的字段是 billing block 里的 CCH，判据见
// anthropicCCHSigningEnabled。
func finalizeAnthropicClaudeCodeMessagesBody(
	body []byte,
	cfg *model.Config,
	apiKey string,
	headers http.Header,
	target *url.URL,
) ([]byte, error) {
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("finalize Anthropic Claude Code request: invalid JSON body")
	}
	cchSigning := anthropicCCHSigningEnabled(cfg, target)
	helperShape := nativeAnthropicHaikuHelperShape(body, headers)
	if helperShape != anthropicHaikuHelperNone {
		if helperShape == anthropicHaikuHelperStructured && cchSigning {
			return finalizeAnthropicCCH(body)
		}
		return body, nil
	}
	if isNativeAnthropicClaudeCodeRequest(headers) {
		// Native Claude Code owns sampling, prompt-cache placement and JSON member
		// order. Where the policy signs, only the CCH digits are refreshed in place;
		// otherwise the caller's body goes out byte-for-byte, including its own CCH.
		if cchSigning {
			return finalizeAnthropicCCH(body)
		}
		return body, nil
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
	// 新增的顶层键按 sjson 的插入顺序落在对象尾部，所以这里的写入次序就是线上键序。
	// 顺序取自原生 Claude Code 请求：system → tools → metadata。采样参数不在其中：
	// normalizeAnthropicSampling 会无条件删掉 temperature/top_p，在这里补默认值是
	// 写完即被抹掉的死操作。
	originalSystem := anthropicSystemText(gjson.GetBytes(body, "system"))
	firstUserText := anthropicFirstUserText(gjson.GetBytes(body, "messages"))
	body = setJSONRaw(body, "system", "["+strings.Join([]string{
		anthropicTextBlockRaw(anthropicBillingHeader(firstUserText), ""),
		anthropicTextBlockRaw(anthropicClaudeCodeIdentityPrompt, ""),
		anthropicTextBlockRaw(anthropicClaudeCodePrompt, anthropicCloakCacheControl(cloakCacheTTL)),
	}, ",")+"]")

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
	body = ensureAnthropicCloakedCacheBreakpoints(body, messagePrefixCount, cloakCacheTTL)
	// Forced tool choice strips thinking during normalization. Only withdraw
	// the object ccLoad injected; caller-owned context_management keeps its
	// ownership and is left untouched.
	body = normalizeAnthropicToolChoice(body)
	body = normalizeAnthropicThinking(body)
	if autoContextManagement && !anthropicThinkingAcceptsContextManagement(body) {
		body = deleteJSONPath(body, "context_management")
	}

	encoded := encodeNormalizedAnthropicRequest(body)
	if cchSigning {
		return finalizeAnthropicCCH(encoded)
	}
	return encoded, nil
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
		"X-Stainless-Package-Version": "0.94.0", "X-Stainless-Runtime-Version": "v26.3.0",
		"Anthropic-Version": "2023-06-01", "Anthropic-Dangerous-Direct-Browser-Access": "true",
	}
	for name, want := range expected {
		if anthropicHeaderValue(headers, name) != want {
			return false
		}
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

// isNativeAnthropicClaudeCodeRequest 判断这组请求头是否就是原生 Claude Code 的线协议
// 形态。入站命中即整体直通、绝不重写；出站分派（anthropicRequestOwnsItsWire、重试重放）
// 同样用它判断「这份 wire 已经是对的，网关只补认证头」。
//
// 判据只有三个请求头信号：
//
//	X-App=cli && CLI UA 形态 && anthropic-beta 含 claude-code-20250219
//
// 刻意不加条件。每加严一条就多一条静默假阴性通道，而假阴性不报错——它把真实 Claude
// Code 请求降级进重写路径，system 被重建成 CLI 三段式、客户端 system block 上的
// cache_control 随 anthropicSystemText 降级整段丢弃、剩余断点再被
// enforceAnthropicCacheControlLimit 裁剪，客户端自管的 prompt cache 就此失效。这类
// 故障只能靠命中率异常反推，排查成本极高。已经逐条踩过并移除的加严条件：
//   - ` cch=` 存在性——签名不是身份。下游指向 ccLoad 时 base URL 非第一方，native
//     gate 直接省略 cch。上游也只在 measuredClaudeCodeHelperSystemMatches 那个窄的
//     Haiku helper profile 里校验它（实测命中率掉到 7.6%）；
//   - metadata.user_id 的 account_uuid 非空——API Key + 非第一方 base URL 没有
//     Anthropic 账号，这一格本就是空串（掉到 16%）；
//   - metadata.user_id 整体存在性——即本条，见下；
//   - CLI 版本号相等——上游基线随观测流量自升级，ccLoad 是常量，客户端一升级就复发；
//   - UA 后缀写死 " (external, cli)"——带 Agent SDK 的 CLI 与 VSCode 扩展被整条拒掉；
//   - system[0] 的 billing 前缀、header 与 body 的 session id 等式；
//   - OAuth 凭证身份逐字段比对——网关侧身份本就是合成的，下游带的才可信。
//
// 代价是明确的：刻意复制这三个头的第三方客户端也会被原样直通，网关不再为它补 CLI
// 指纹、billing header 与合成身份。这是有意的取舍——真实 Claude Code 的缓存保真优先。
//
// 同一个判据同时服务入站与出站：网关自己产出的 body 一样通过检测，不存在「自己不认
// 自己」的自指。
func isNativeAnthropicClaudeCodeRequest(headers http.Header) bool {
	return validAnthropicClaudeCLIUserAgent(anthropicHeaderValue(headers, "User-Agent")) &&
		anthropicHeaderValue(headers, "X-App") == "cli" &&
		slices.Contains(strings.Split(normalizedAnthropicBetaHeader(headers), ","), "claude-code-20250219")
}

func validAnthropicClaudeCLIUserAgent(userAgent string) bool {
	matches := anthropicClaudeCLIUserAgentPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	return matches != nil && nativeAnthropicClaudeEntrypoints[strings.ToLower(matches[2])]
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
	credential := anthropicCredentialForWire(cfg, apiKey)
	if credential == nil {
		return nil, errors.New("finalize Anthropic Claude Code request: credential identity is incomplete")
	}
	identitySeed := credential.AccountUUID
	if identitySeed == "" {
		identitySeed = strings.ToLower(credential.EmailAddress)
	}
	if credential.DeviceID == "" || identitySeed == "" {
		return nil, errors.New("finalize Anthropic Claude Code request: credential identity is incomplete")
	}
	sessionID := anthropicSessionIDFromHeaders(headers)
	if sessionID == "" {
		sessionID = anthropicStableSessionID(identitySeed, anthropicFirstUserText(gjson.GetBytes(body, "messages")))
	}
	identity := "{}"
	var err error
	for _, field := range []struct{ key, value string }{
		{"device_id", credential.DeviceID},
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
// OAuth 渠道用凭证里真实的账号与设备；API Key 渠道（含第三方网关）没有这两个字段，
// 按 Key 稳定派生一份。身份必须随 Key 稳定：每次请求换设备，上游看到的就是一台
// 反复重装的机器。合成身份复用 anthropicauth.Credential，下游所有身份逻辑因此只有
// 一份实现。
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
	var patches []anthropicRawPatch
	var deletions []string
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for index, message := range messages.Array() {
			if !message.IsObject() {
				continue
			}
			if cleaned, changed := stripEmptyAnthropicTextBlocks(message.Get("content")); changed {
				patches = append(patches, anthropicRawPatch{
					path: "messages." + strconv.Itoa(index) + ".content", raw: cleaned,
				})
			}
		}
	}
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
	for _, patch := range patches {
		body = setJSONRaw(body, patch.path, patch.raw)
	}
	for _, path := range deletions {
		body = deleteJSONPath(body, path)
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
// system and rolling-message selectors. Tools remain unstamped because cloaking
// always installs a usable system prompt that already covers the shared prefix.
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

func anthropicBillingHeader(firstUserText string) string {
	padded := []byte(firstUserText + strings.Repeat("0", 21))
	selected := []byte{padded[4], padded[7], padded[20]}
	digest := sha256.Sum256(append([]byte(anthropicBillingSalt), append(selected, []byte(anthropicCLIVersion)...)...))
	fingerprint := hex.EncodeToString(digest[:])[:3]
	return "x-anthropic-billing-header: cc_version=" + anthropicCLIVersion + "." + fingerprint + "; cc_entrypoint=cli;"
}

func injectAnthropicOAuthHeaders(
	req *http.Request,
	cfg *model.Config,
	accessToken string,
	body []byte,
	incomingHeaders ...http.Header,
) {
	if req == nil {
		return
	}
	incoming := anthropicIncomingHeaders(req, incomingHeaders)
	if anthropicRequestOwnsItsWire(body, incoming, cfg, "") {
		applyAnthropicNativeHeaders(req, incoming)
		setRawHeader(req.Header, "Authorization", "Bearer "+strings.TrimSpace(accessToken))
		return
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	setRawHeader(req.Header, "Authorization", "Bearer "+strings.TrimSpace(accessToken))
	applyAnthropicClaudeCodeHeaders(req, anthropicClaudeCodeBetas(body), resolveAnthropicSessionID(body, cfg, "", incoming))
}

// injectAnthropicAPIKeyHeaders 为 API Key 渠道重建 Claude Code CLI 请求头。
// CLI 能力头与 OAuth 共用，认证头走 applyAnthropicAPIKeyAuth；body 的 CCH 已在最终化边界排除。
func injectAnthropicAPIKeyHeaders(
	req *http.Request,
	cfg *model.Config,
	apiKey string,
	body []byte,
	incomingHeaders ...http.Header,
) {
	if req == nil {
		return
	}
	incoming := anthropicIncomingHeaders(req, incomingHeaders)
	if anthropicRequestOwnsItsWire(body, incoming, cfg, apiKey) {
		applyAnthropicNativeHeaders(req, incoming)
		applyAnthropicAPIKeyAuth(req, apiKey)
		return
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	applyAnthropicAPIKeyAuth(req, apiKey)
	applyAnthropicClaudeCodeHeaders(
		req, anthropicClaudeCodeBetas(body), resolveAnthropicSessionID(body, cfg, apiKey, incoming),
	)
}

func anthropicIncomingHeaders(req *http.Request, override []http.Header) http.Header {
	if len(override) > 0 && override[0] != nil {
		return override[0]
	}
	return req.Header.Clone()
}

// anthropicRequestOwnsItsWire 判断这个 body 配套的 header 就是正确的指纹，网关只做
// 透传。它跑在**出站** body 上（已经过最终化），所以用不含 CCH 的身份判据：签名与否
// 是策略决定的，掺进来会让「网关自己产出的 body 通不过自己的检测器」。
func anthropicRequestOwnsItsWire(body []byte, incoming http.Header, cfg *model.Config, apiKey string) bool {
	if !isAnthropicJSONObject(body) {
		return false
	}
	return nativeAnthropicHaikuHelperShape(body, incoming) != anthropicHaikuHelperNone ||
		isNativeAnthropicClaudeCodeRequest(incoming)
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
		"Accept", "Accept-Encoding", "Content-Type", "User-Agent", "X-App", "Anthropic-Beta", "Anthropic-Version",
		"Anthropic-Dangerous-Direct-Browser-Access", "X-Claude-Code-Session-Id", "X-Client-Request-Id",
		"X-Stainless-Async", "X-Stainless-Lang", "X-Stainless-Runtime", "X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Retry-Count", "X-Stainless-Timeout",
	} {
		if value := anthropicHeaderValue(incoming, name); value != "" {
			setRawHeader(req.Header, name, value)
		}
	}
}

// anthropicClaudeCodeBetas 组装 Claude Code CLI 的 Anthropic-Beta 集合。
//
// 这里没有「OAuth 版」和「API Key 版」两套集合：betas 必须与
// finalizeAnthropicClaudeCodeMessagesBody 产出的 body 形态严格对应，拆成两套就会
// 出现 body 用了某能力、header 没声明对应 beta 的 400。CCH 是独立的凭证签名边界，
// 不参与 beta 集合分支。同源是双向的：extended-cache-ttl-2025-04-11 跟随 body 里
// 实际存在的 cache_control.ttl——缓存窗口由调用方的原始请求决定，网关不主动升级
// 到 1h，也就不替它声明这个 beta。
func anthropicClaudeCodeBetas(body []byte) string {
	betas := make([]string, 0, 14)
	betas = append(betas, "claude-code-20250219", "oauth-2025-04-20", "interleaved-thinking-2025-05-14")
	if strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "thinking.display"))) == "" {
		betas = append(betas, "redact-thinking-2026-02-12")
	}
	betas = append(betas,
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
	)
	if !anthropicUsesLegacySystemReminder(jsonStringValue(gjson.GetBytes(body, "model"))) {
		betas = append(betas, "mid-conversation-system-2026-04-07")
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && jsonMemberCount(tools) > 0 {
		betas = append(betas, "advanced-tool-use-2025-11-20")
	}
	betas = append(betas, "effort-2025-11-24", "fallback-credit-2026-06-01")
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

func applyAnthropicClaudeCodeHeaders(req *http.Request, betas, sessionID string) {
	setRawHeader(req.Header, "Accept", "application/json")
	setRawHeader(req.Header, "Content-Type", "application/json")
	setRawHeader(req.Header, "User-Agent", "claude-cli/2.1.220 (external, cli)")
	setRawHeader(req.Header, "X-Claude-Code-Session-Id", sessionID)
	setRawHeader(req.Header, "X-Stainless-Arch", anthropicStainlessArch())
	setRawHeader(req.Header, "X-Stainless-Lang", "js")
	setRawHeader(req.Header, "X-Stainless-OS", anthropicStainlessOS())
	setRawHeader(req.Header, "X-Stainless-Package-Version", "0.94.0")
	setRawHeader(req.Header, "X-Stainless-Retry-Count", "0")
	setRawHeader(req.Header, "X-Stainless-Runtime", "node")
	setRawHeader(req.Header, "X-Stainless-Runtime-Version", "v26.3.0")
	setRawHeader(req.Header, "X-Stainless-Timeout", "600")
	setRawHeader(req.Header, "anthropic-beta", betas)
	setRawHeader(req.Header, "anthropic-dangerous-direct-browser-access", "true")
	setRawHeader(req.Header, "anthropic-version", "2023-06-01")
	setRawHeader(req.Header, "x-app", "cli")
	setRawHeader(req.Header, "x-client-request-id", uuid.NewString())
	setRawHeader(req.Header, "Connection", "keep-alive")
	setRawHeader(req.Header, "Accept-Encoding", "gzip, deflate, br, zstd")
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
// Body 先把 session_id 写进 metadata.user_id，这里读回来才能保证 header 与 body 同值，
// 而 isNativeAnthropicClaudeCodeRequest 正是按这个等式识别原生 Claude Code 请求的。
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

func anthropicStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func anthropicStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	default:
		return runtime.GOARCH
	}
}
