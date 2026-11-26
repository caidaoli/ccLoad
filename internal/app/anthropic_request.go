package app

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxysignature "ccLoad/internal/protocol/cliproxy/signature"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
)

/**
 * @brief 将 Responses 系统项按原顺序前置，使不支持中途 system 的 Anthropic wire 保持顶层指令契约。
 * @param cfg 当前渠道配置。
 * @param target 本次实际选择的上游地址。
 * @param client 客户端协议。
 * @param upstream 上游协议。
 * @param body 尚未转换的 Responses 请求正文。
 * @return 仅调整 input 系统项位置后的请求正文。
 */
func preserveResponsesTopLevelSystemItems(cfg *model.Config, target string, client, upstream protocol.Protocol, body []byte) []byte {
	if client != protocol.Codex || upstream != protocol.Anthropic ||
		(!isOpenCodeAPIKeyEndpoint(cfg, target) && !cfg.UsesZAIOAuth()) {
		return body
	}
	return prependResponsesSystemItems(body)
}

/**
 * @brief 前置 Responses 系统项，保持系统指令和其余会话项各自的原始顺序。
 * @param body 尚未转换的 Responses 请求正文。
 * @return 前置系统项后的正文；没有需要移动的系统项时返回原正文。
 */
func prependResponsesSystemItems(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	var systemItems, otherItems []string
	moved := false
	for _, item := range input.Array() {
		switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
		case "system", "developer":
			systemItems = append(systemItems, item.Raw)
			moved = moved || len(otherItems) > 0
		default:
			otherItems = append(otherItems, item.Raw)
		}
	}
	if !moved {
		return body
	}
	return setJSONRaw(body, "input", joinJSONRaw(append(systemItems, otherItems...)))
}

// Anthropic 线协议改写全程以 []byte 为唯一表示：gjson 读、sjson 就地写。
//
// 不用 map[string]any 中转不是风格偏好——map 丢键序。上游按 body 形态做 Claude Code
// 指纹识别，一旦把请求解成 map 再整体重编码，所有网关新增的键都会按字母序落位，与
// 原生客户端形态对不上；为了绕开这一点还要额外维护一层「原始字节 vs 目标 map」的
// 差分渲染。就地改写让未触及的字节原样保留，键序问题从根上不存在。

// normalizeAnthropicMessagesBody is the single native Claude body boundary.
// Protocol conversion produces the shape; this function only enforces Anthropic
// wire invariants shared by API-key and OAuth attempts.
func normalizeAnthropicMessagesBody(body []byte) ([]byte, error) {
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("normalize Anthropic request: invalid JSON body")
	}
	return encodeNormalizedAnthropicRequest(body), nil
}

// isAnthropicJSONObject 是所有 sjson 就地改写的准入守卫。守卫必须与消费者同源：
// sjson 对截断输入静默返回损坏结果且 err == nil，所以入口这一次校验是唯一防线。
func isAnthropicJSONObject(body []byte) bool {
	return gjson.ValidBytes(body) && gjson.ParseBytes(body).IsObject()
}

/**
 * @brief 去掉 advisor 服务端工具自身模型的已知思考后缀，保留未知后缀和其他工具。
 * @param body Anthropic 请求正文。
 * @return 仅改写 advisor 工具模型后的正文；没有已知后缀时返回原正文。
 * @note 在渠道 BodyRules 之前调用，用户显式覆盖的模型不再归一。
 */
func normalizeAnthropicAdvisorToolModels(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	for i, tool := range tools.Array() {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Get("type").String())), "advisor_") {
			continue
		}
		toolModel := tool.Get("model")
		if toolModel.Type != gjson.String {
			continue
		}
		if base, _, ok := model.ParseThinkingSuffix(strings.TrimSpace(toolModel.String())); ok {
			body = setJSONValue(body, fmt.Sprintf("tools.%d.model", i), base)
		}
	}
	return body
}

/**
 * @brief Claude 工具 input_schema 属性名回退参数（参考 CLIProxyAPI f0c9c2d01）
 */
const (
	anthropicToolPropertyKeyMaxLength  = 64                  ///< Claude 接受的属性名最大长度
	anthropicToolSchemaMaxInspectDepth = 256                 ///< schema 递归检查深度上限，超过按需要回退处理
	anthropicToolSchemaFallbackSchema  = `{"type":"object"}` ///< 回退后的开放对象 schema
)

/**
 * @brief 回退时追加到工具描述的固定前缀，与上游逐字一致；模型据此按原属性名调用工具
 */
const anthropicToolSchemaFallbackPrefix = "The input object must conform to the following JSON Schema. Use its property names exactly as written, including any characters such as brackets:\n"

/**
 * @brief 判断 Claude 是否接受该 input_schema 属性名。
 * @param name 属性名字面量。
 * @return 名称匹配 ^[a-zA-Z0-9_.-]{1,64}$ 时返回 true。
 */
func anthropicPropertyKeyAccepted(name string) bool {
	if len(name) == 0 || len(name) > anthropicToolPropertyKeyMaxLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

/**
 * @brief 按 JSON Schema 关键字的模式位置递归检查属性名。
 * @param schema 待检查的 schema 节点。
 * @param depth 当前递归深度。
 * @return schema 或其可达子 schema 声明了 Claude 拒收的属性名时返回 true；深度超过
 * 上限时同样返回 true（按回退处理，正文仍完整保留在 description 中）。
 * @details 关键字名只在 schema 位置解释：以关键字命名的 definition/property 按其角色
 * 处理；const/default/enum/examples 等数据值与未知注解不检查。
 */
func anthropicSchemaHasRejectedPropertyKey(schema gjson.Result, depth int) bool {
	if !schema.IsObject() {
		return false
	}
	if depth > anthropicToolSchemaMaxInspectDepth {
		return true
	}
	found := false
	subschema := func(value gjson.Result) bool {
		found = anthropicSchemaHasRejectedPropertyKey(value, depth+1)
		return !found
	}
	subschemas := func(value gjson.Result) {
		value.ForEach(func(_, item gjson.Result) bool { return subschema(item) })
	}
	schema.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "properties":
			if !value.IsObject() {
				return true
			}
			value.ForEach(func(name, item gjson.Result) bool {
				if !anthropicPropertyKeyAccepted(name.String()) {
					found = true
					return false
				}
				return subschema(item)
			})
		case "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies":
			// 命名子 schema 的映射；dependencies 也可能是名称数组，非对象由 subschema 跳过。
			if value.IsObject() {
				subschemas(value)
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			if value.IsArray() {
				subschemas(value)
			}
		case "items":
			if value.IsArray() {
				subschemas(value)
			} else {
				subschema(value)
			}
		case "additionalProperties", "unevaluatedProperties", "additionalItems", "unevaluatedItems",
			"contains", "propertyNames", "not", "if", "then", "else", "contentSchema":
			subschema(value)
		}
		return !found
	})
	return found
}

/**
 * @brief 判断一个客户端工具声明的 input_schema 是否会被 Claude 拒收。
 * @param tool 工具声明节点。
 * @return 客户端工具的 schema 含拒收属性名时返回 true；服务端工具返回 false。
 */
func anthropicToolNeedsSchemaFallback(tool gjson.Result) bool {
	if !isAnthropicClientToolDeclaration(tool) {
		return false
	}
	schema := tool.Get("input_schema")
	return schema.IsObject() && anthropicSchemaHasRejectedPropertyKey(schema, 0)
}

/**
 * @brief 把 input_schema 含 Claude 拒收属性名的工具改写为开放对象描述形态。
 * @param body 转换后的 Anthropic 请求正文。
 * @return 需要回退的工具被保留并改写后的正文；无可回退工具时返回原正文。
 * @details 遍历 tools[] 与 messages[].content[] 中 type=tool_addition、
 * tool.type=tool_definition 的 tool.definition，跳过服务端工具。命中时 description
 * 追加固定前缀与原 schema 原文，input_schema 置为 {"type":"object"}，并删除 strict：
 * 模型仍能看到全部原始属性名与约束，按原名调用工具，历史 tool_use 与响应均无需改写。
 * @note 在渠道 BodyRules 之前调用，用户规则可以覆盖 description。
 */
func normalizeAnthropicToolPropertyNames(body []byte) []byte {
	var paths []string
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		for i, tool := range tools.Array() {
			if anthropicToolNeedsSchemaFallback(tool) {
				paths = append(paths, fmt.Sprintf("tools.%d", i))
			}
		}
	}
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for messageIndex, message := range messages.Array() {
			content := message.Get("content")
			if !content.IsArray() {
				continue
			}
			for partIndex, part := range content.Array() {
				if jsonStringValue(part.Get("type")) != "tool_addition" || jsonStringValue(part.Get("tool.type")) != "tool_definition" {
					continue
				}
				if anthropicToolNeedsSchemaFallback(part.Get("tool.definition")) {
					paths = append(paths, fmt.Sprintf("messages.%d.content.%d.tool.definition", messageIndex, partIndex))
				}
			}
		}
	}
	for _, path := range paths {
		tool := gjson.GetBytes(body, path)
		var description strings.Builder
		if existing := jsonStringValue(tool.Get("description")); strings.TrimSpace(existing) != "" {
			description.WriteString(existing)
			description.WriteString("\n\n")
		}
		description.WriteString(anthropicToolSchemaFallbackPrefix)
		description.WriteString(tool.Get("input_schema").Raw)
		body = setJSONValue(body, path+".description", description.String())
		body = setJSONRaw(body, path+".input_schema", anthropicToolSchemaFallbackSchema)
		if tool.Get("strict").Exists() {
			body = deleteJSONPath(body, path+".strict")
		}
	}
	return body
}

// encodeNormalizedAnthropicRequest 只收尾 Anthropic Messages 的通用 wire 形状。
// 入口守卫由调用方（normalizeAnthropicMessagesBody / 最终化边界）负责，这里三步
// 都不会失败，所以不返回 error。
//
// CCH 不在这里签：模拟请求省略它；旧版结构化 Haiku helper 在专用分支处理。
func encodeNormalizedAnthropicRequest(body []byte) []byte {
	body = normalizeAnthropicMessagesRequest(body)
	body = orderAnthropicCacheControlWireShape(body)
	body, _ = cliproxysignature.SanitizeClaudeMessagesForClaudeUpstream(
		body, jsonStringValue(gjson.GetBytes(body, "model")),
	)
	return body
}

func normalizeAnthropicMessagesRequest(body []byte) []byte {
	body = normalizeAnthropicToolChoice(body)
	body = normalizeAnthropicThinking(body)
	body = normalizeAnthropicSampling(body)
	body = sanitizeAnthropicOAuthMessages(body)
	if countAnthropicCacheControls(body) == 0 {
		body = ensureAnthropicCacheControls(body)
	}
	body = normalizeAnthropicCacheControlTTL(body)
	return enforceAnthropicCacheControlLimit(body, anthropicMaxCacheControls)
}

func validateAnthropicLegacySystemMessages(body []byte) error {
	modelName := jsonStringValue(gjson.GetBytes(body, "model"))
	if !cliproxyutil.IsClaudeLegacySystemReminderModel(modelName) {
		return nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	var failure error
	index := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		current := index
		index++
		if !message.IsObject() {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(jsonStringValue(message.Get("role"))), "system") {
			failure = &requestValidationError{message: fmt.Sprintf(
				"Anthropic model %q does not support system messages in messages[%d]", modelName, current,
			)}
			return false
		}
		return true
	})
	return failure
}

func countAnthropicCacheControls(body []byte) int {
	count := 0
	forEachAnthropicCacheBlock(body, func(_ string, block gjson.Result) bool {
		if block.Get("cache_control").Exists() {
			count++
		}
		return true
	})
	return count
}

/**
 * @brief 归一工具选择；Haiku 5.5 的强制工具选择允许保留思考模式和输出等级。
 * @param body 待最终化的 Anthropic 正文。
 * @return 移除无工具选择及旧模型强制工具思考冲突后的正文。
 */
func normalizeAnthropicToolChoice(body []byte) []byte {
	if tools := gjson.GetBytes(body, "tools"); !tools.IsArray() || jsonMemberCount(tools) == 0 {
		body = deleteJSONPath(body, "tool_choice")
	}
	choiceType := strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "tool_choice.type"))))
	if choiceType != "any" && choiceType != "tool" {
		return body
	}
	if anthropicClaude55Family(anthropicRequestModelName(body, "")) == "claude-haiku-5-5" {
		return body
	}
	body = deleteJSONPath(body, "thinking")
	return deleteAnthropicOutputEffort(body)
}

/**
 * @brief 将旧思考别名和自适应预算归一，保留默认开启模型的显式关闭语义。
 * @param body 待最终化的 Anthropic 正文。
 * @return 归一后的正文；Haiku 5.5 关闭思考时仍保留输出等级。
 */
func normalizeAnthropicThinking(body []byte) []byte {
	thinking := gjson.GetBytes(body, "thinking")
	if !thinking.IsObject() {
		return body
	}
	typ := strings.ToLower(strings.TrimSpace(jsonStringValue(thinking.Get("type"))))
	switch typ {
	case "auto":
		body = setJSONRaw(body, "thinking.type", `"adaptive"`)
		typ = "adaptive"
	case "disabled", "off", "none":
		if anthropicClaude55Family(anthropicRequestModelName(body, "")) == "claude-haiku-5-5" {
			return setJSONValue(body, "thinking.type", "disabled")
		}
		body = deleteJSONPath(body, "thinking")
		return deleteAnthropicOutputEffort(body)
	}
	if typ != "adaptive" {
		return body
	}
	if budget, ok := jsonIntegerValue(gjson.GetBytes(body, "thinking.budget_tokens")); ok && budget > 0 {
		body = setAnthropicOutputEffort(body, anthropicBudgetToEffort(int(budget)))
	}
	return deleteJSONPath(body, "thinking.budget_tokens")
}

func deleteAnthropicOutputEffort(body []byte) []byte {
	if !gjson.GetBytes(body, "output_config").IsObject() {
		return body
	}
	body = deleteJSONPath(body, "output_config.effort")
	if jsonMemberCount(gjson.GetBytes(body, "output_config")) == 0 {
		body = deleteJSONPath(body, "output_config")
	}
	return body
}

func normalizeAnthropicSampling(body []byte) []byte {
	// Claude Code does not forward caller sampling knobs. Keeping both temperature
	// and top_p is invalid, and thinking requests reject top_k as well.
	body = deleteJSONPath(body, "temperature")
	body = deleteJSONPath(body, "top_p")
	switch strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "thinking.type")))) {
	case "enabled", "adaptive", "auto":
		body = deleteJSONPath(body, "top_k")
	}
	return body
}

/**
 * @brief 按最终上游 beta（含渠道规则）移除未获授权的请求字段。
 * @param body 已最终化的 Anthropic 请求正文。
 * @param betaHeader 实际发送的 Anthropic-Beta。
 * @return 保留已授权能力后的正文，不改变当前轮思考模式。
 */
func sanitizeAnthropicBodyForBetaTokens(body []byte, betaHeader string) []byte {
	activeBetas := make(map[string]bool)
	for part := range strings.SplitSeq(betaHeader, ",") {
		if token := strings.TrimSpace(part); token != "" {
			activeBetas[token] = true
		}
	}
	hasBeta := func(tokens ...string) bool {
		for _, token := range tokens {
			if activeBetas[token] {
				return true
			}
		}
		return false
	}
	for _, field := range []struct {
		path  string
		betas []string
	}{
		{"context_management", []string{"context-management-2025-06-27"}},
		{"thinking.block_binding", []string{"thinking-binding-controls-2026-08-01"}},
		{"output_config.timing", []string{"timing-2026-09-09"}},
		{"thread", []string{"message-threads-2026-08-12"}},
		{"fallbacks", []string{"server-side-fallback-2026-07-01"}},
		{"fallback_credit_token", []string{"server-side-fallback-2026-07-01", "fallback-credit-2026-07-01", "fallback-credit-2026-06-01"}},
	} {
		if !hasBeta(field.betas...) && gjson.GetBytes(body, field.path).Exists() {
			body = deleteJSONPath(body, field.path)
		}
	}
	// 2.1.283 起消息级 output_config 由 per-turn-control 承载；旧 beta 名保留给旧客户端。
	for index, message := range gjson.GetBytes(body, "messages").Array() {
		if !message.Get("output_config").Exists() {
			continue
		}
		path := "messages." + strconv.Itoa(index) + ".output_config"
		if !hasBeta("per-turn-control-2026-07-01", "mid-conversation-output-config-2026-07-01") {
			body = deleteJSONPath(body, path)
		} else if !hasBeta("timing-2026-09-09") {
			body = deleteJSONPath(body, path+".timing")
		}
	}
	// display:"updates" needs thinking-display-updates-2026-08-18. Drop the
	// field (model default omitted) instead of killing adaptive thinking.
	if !hasBeta("thinking-display-updates-2026-08-18") {
		display := strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "thinking.display")))
		if strings.EqualFold(display, "updates") {
			body = deleteJSONPath(body, "thinking.display")
		}
	}
	return body
}

func ensureAnthropicCacheControls(body []byte) []byte {
	body = ensureAnthropicToolCacheControl(body)
	body = ensureAnthropicSystemCacheControl(body)
	return ensureAnthropicMessageCacheControl(body)
}

func ensureAnthropicToolCacheControl(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	lastEligible := -1
	for index, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		if tool.Get("cache_control").Exists() {
			return body
		}
		if !isAnthropicDeferredTool(tool) {
			lastEligible = index
		}
	}
	if lastEligible < 0 {
		return body
	}
	return setJSONRaw(body, "tools."+strconv.Itoa(lastEligible)+".cache_control", anthropicEphemeralCacheControl())
}

func ensureAnthropicSystemCacheControl(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	switch {
	case !system.Exists():
		return body
	case system.Type == gjson.String:
		if strings.TrimSpace(system.String()) == "" {
			return body
		}
		block := anthropicTextBlockRaw(system.String(), anthropicEphemeralCacheControl())
		return setJSONRaw(body, "system", "["+block+"]")
	case system.IsArray():
		last := -1
		for index, block := range system.Array() {
			if !block.IsObject() {
				continue
			}
			if block.Get("cache_control").Exists() {
				return body
			}
			last = index
		}
		if last < 0 {
			return body
		}
		return setJSONRaw(body, "system."+strconv.Itoa(last)+".cache_control", anthropicEphemeralCacheControl())
	default:
		return body
	}
}

func ensureAnthropicMessageCacheControl(body []byte) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	userIndexes := make([]int, 0, 2)
	for index, message := range messages.Array() {
		if !message.IsObject() {
			continue
		}
		if content := message.Get("content"); content.IsArray() {
			for _, block := range content.Array() {
				if block.IsObject() && block.Get("cache_control").Exists() {
					return body
				}
			}
		}
		if jsonStringValue(message.Get("role")) == "user" {
			userIndexes = append(userIndexes, index)
		}
	}
	if len(userIndexes) < 2 {
		return body
	}
	target := "messages." + strconv.Itoa(userIndexes[len(userIndexes)-2])
	content := gjson.GetBytes(body, target+".content")
	switch {
	case content.Type == gjson.String:
		block := anthropicTextBlockRaw(content.String(), anthropicEphemeralCacheControl())
		return setJSONRaw(body, target+".content", "["+block+"]")
	case content.IsArray():
		blocks := content.Array()
		for index, block := range slices.Backward(blocks) {
			if block.IsObject() {
				return setJSONRaw(body, target+".content."+strconv.Itoa(index)+".cache_control",
					anthropicEphemeralCacheControl())
			}
		}
	}
	return body
}

// anthropicEphemeralCacheControl 是 Anthropic 线上默认（5m）缓存断点的原始 JSON。
func anthropicEphemeralCacheControl() string { return `{"type":"ephemeral"}` }

// anthropicTextBlockRaw 按原生键序拼一个 text 块：type → text → cache_control。
// 文本按 JS JSON.stringify 转义（`<`、`>`、`&` 原样），与 Claude Code 线上字节一致。
func anthropicTextBlockRaw(text, cacheControlRaw string) string {
	block := `{"type":"text","text":` + jsonStringifyString(text)
	if cacheControlRaw != "" {
		block += `,"cache_control":` + cacheControlRaw
	}
	return block + "}"
}

// anthropicTextMessageRaw 按原生键序拼一条纯文本消息：role → content。
func anthropicTextMessageRaw(role, content string) string {
	return `{"role":` + jsonStringifyString(role) + `,"content":` + jsonStringifyString(content) + "}"
}

/**
 * @brief 按 Anthropic 断点评估顺序归一缓存窗口，包含末尾自动缓存标记。
 * @param body Anthropic 请求正文。
 * @return 删除排在 5m 断点之后的 1h TTL 后的正文。
 */
func normalizeAnthropicCacheControlTTL(body []byte) []byte {
	seenFiveMinutes := false
	var demoted []string
	forEachAnthropicCacheBlock(body, func(path string, block gjson.Result) bool {
		cache := block.Get("cache_control")
		if !cache.IsObject() {
			if cache.Exists() {
				seenFiveMinutes = true
			}
			return true
		}
		if jsonStringValue(cache.Get("ttl")) != "1h" {
			seenFiveMinutes = true
			return true
		}
		if seenFiveMinutes {
			demoted = append(demoted, anthropicCacheControlPath(path)+".ttl")
		}
		return true
	})
	for _, path := range demoted {
		body = deleteJSONPath(body, path)
	}
	return body
}

/**
 * @brief 按 tools、system、messages、顶层自动缓存的顺序访问缓存载体。
 * @param body Anthropic 请求正文快照。
 * @param visit 接收载体路径及对象；顶层路径为空，返回 false 停止遍历。
 * @note 回调只收集路径；修改正文须在遍历后执行，以保留其余 JSON 字节。
 */
func forEachAnthropicCacheBlock(body []byte, visit func(path string, block gjson.Result) bool) {
	root := gjson.ParseBytes(body)
	visitBlocks := func(container gjson.Result, prefix string) bool {
		if !container.IsArray() {
			return true
		}
		for index, block := range container.Array() {
			if !block.IsObject() {
				continue
			}
			if !visit(prefix+"."+strconv.Itoa(index), block) {
				return false
			}
		}
		return true
	}
	if !visitBlocks(root.Get("tools"), "tools") || !visitBlocks(root.Get("system"), "system") {
		return
	}
	messages := root.Get("messages")
	if messages.IsArray() {
		for index, message := range messages.Array() {
			if !message.IsObject() {
				continue
			}
			if !visitBlocks(message.Get("content"), "messages."+strconv.Itoa(index)+".content") {
				return
			}
		}
	}
	if root.Get("cache_control").Exists() {
		visit("", root)
	}
}

/**
 * @brief 返回块级或顶层缓存标记的 JSON 路径。
 * @param blockPath 缓存载体路径；空字符串表示请求根对象。
 * @return cache_control 字段路径。
 */
func anthropicCacheControlPath(blockPath string) string {
	if blockPath == "" {
		return "cache_control"
	}
	return blockPath + ".cache_control"
}
