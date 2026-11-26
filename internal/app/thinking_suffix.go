package app

import (
	"fmt"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	cliproxyutil "ccLoad/internal/protocol/cliproxy/util"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 后缀写入客户端协议的思考字段。选路后按实际目标收敛，再交给协议转换和提供商封装。
// prepareTranslatedUpstreamBody 已在转换之后，Antigravity 等上游的封装会改变字段路径。

/**
 * @brief Gemini 客户端未使用蛇形字段时的默认思考配置路径。
 */
const (
	geminiThinkingLevelKey   = "thinkingLevel"                                              ///< 与 thinking_level 对应的枚举字段。
	geminiThinkingBudgetKey  = "thinkingBudget"                                             ///< 与 thinking_budget 对应的整数字段。
	geminiThinkingLevelPath  = "generationConfig.thinkingConfig." + geminiThinkingLevelKey  ///< 默认等级路径。
	geminiThinkingBudgetPath = "generationConfig.thinkingConfig." + geminiThinkingBudgetKey ///< 默认预算路径。
)

/**
 * @brief 在选路之前将模型后缀写入客户端正文。
 * @param body 客户端原始请求正文。
 * @param clientProtocol 客户端协议。
 * @param requestedModel 客户端原始模型名。
 * @return 用于初始请求处理的正文。
 */
func applyThinkingSuffix(body []byte, clientProtocol protocol.Protocol, requestedModel string) []byte {
	return applyThinkingSuffixForModel(
		body,
		clientProtocol,
		clientProtocol,
		requestedModel,
		model.RoutingModelName(requestedModel),
	)
}

/**
 * @brief 将后缀写成客户端协议字段，按本次实际上游收敛。
 * @param body 客户端请求正文。
 * @param clientProtocol 客户端协议，决定思考字段的写法。
 * @param upstreamProtocol 选中的上游协议，决定是否使用原生档位。
 * @param requestedModel 客户端原始模型名，保留显式后缀。
 * @param resolvedModel 重定向或模糊匹配后的模型名。
 * @return 应用后缀后的正文。
 */
func applyThinkingSuffixForModel(
	body []byte,
	clientProtocol, upstreamProtocol protocol.Protocol,
	requestedModel, resolvedModel string,
) []byte {
	_, cfg, ok := model.ParseThinkingSuffix(requestedModel)
	if !ok || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	switch protocol.Protocol(util.NormalizeProtocol(string(clientProtocol))) {
	case protocol.OpenAI:
		return applyOpenAIStyleThinking(body, cfg, "reasoning_effort", resolvedModel)
	case protocol.Codex:
		return applyOpenAIStyleThinking(body, cfg, "reasoning.effort", resolvedModel)
	case protocol.Anthropic:
		return applyAnthropicThinking(body, cfg, resolvedModel, upstreamProtocol)
	case protocol.Gemini:
		return applyGeminiThinking(body, cfg, resolvedModel, upstreamProtocol)
	default:
		return body
	}
}

/**
 * @brief 解释显式思考后缀的等级，数字预算按模型支持的档位收敛。
 * @param cfg 已解析的后缀。
 * @param baseModel 请求模型或实际出站模型的基名。
 * @return 离散等级或特殊值；未知模式返回空字符串。
 */
func thinkingEffortLabel(cfg thinking.ThinkingConfig, baseModel string) string {
	switch cfg.Mode {
	case thinking.ModeNone:
		return string(thinking.LevelNone)
	case thinking.ModeAuto:
		return string(thinking.LevelAuto)
	case thinking.ModeLevel:
		return strings.ToLower(strings.TrimSpace(string(cfg.Level)))
	case thinking.ModeBudget:
		level, _ := thinking.ConvertBudgetToModelLevel(cfg.Budget, baseModel, "openai")
		return level
	default:
		return ""
	}
}

// thinkingEffortFromRequest 优先采用后缀声明的等级：(none)/(auto) 在部分协议上以
// "删除字段"表达，请求体里读不回来。
func thinkingEffortFromRequest(requestedModel string, body []byte) string {
	if base, cfg, ok := model.ParseThinkingSuffix(requestedModel); ok {
		if label := thinkingEffortLabel(cfg, base); label != "" {
			return label
		}
	}
	return extractThinkingEffortFromJSON(body)
}

/**
 * @brief 从 OpenAI Chat 的 Google 扩展获取日志标签，标签独立于请求内存。
 * @param body 已解析协议的 OpenAI Chat 请求体。
 * @return 显式等级或按预算换算的标签，缺少有效配置时为空字符串。
 */
func openAIGoogleThinkingEffort(body []byte) string {
	level, budget, found := thinking.ParseOpenAIGoogleThinking(body)
	if !found {
		return ""
	}
	if level != "" {
		return strings.Clone(level)
	}
	level, _ = thinking.ConvertBudgetToLevel(budget)
	return level
}

/**
 * @brief Chat 转 Gemini 时，显式 auto 后缀使用模型默认思考，保留扩展里的摘要开关。
 * @param body 协议转换前的 Chat 请求体。
 * @param requestedModel 客户端提交的模型名，可能含思考后缀。
 * @return 显式 auto 时去掉原生生成配置与扩展中的思考等级和预算，否则返回原始请求体。
 * @details 路径与 thinking.ParseOpenAIGoogleThinking 读取的容器一致，否则日志记 auto 而上游仍按残留配置执行。
 */
func omitOpenAIGoogleThinkingForAuto(body []byte, requestedModel string) []byte {
	if _, cfg, ok := model.ParseThinkingSuffix(requestedModel); !ok || cfg.Mode != thinking.ModeAuto {
		return body
	}
	for _, googlePath := range [...]string{"generationConfig", "generation_config", "extra_body.google", "extra_body.extra_body.google", "google"} {
		google := gjson.GetBytes(body, googlePath)
		for _, configKey := range [...]string{"thinking_config", "thinkingConfig"} {
			config := google.Get(configKey)
			for _, key := range [...]string{"thinking_level", "thinkingLevel", "thinking_budget", "thinkingBudget"} {
				if config.Get(key).Exists() {
					body, _ = sjson.DeleteBytes(body, googlePath+"."+configKey+"."+key)
				}
			}
		}
	}
	return body
}

func applyOpenAIStyleThinking(body []byte, cfg thinking.ThinkingConfig, effortPath, baseModel string) []byte {
	if cfg.Mode == thinking.ModeAuto {
		// auto 不是 OpenAI/Codex 的合法档位，删掉字段把决定权交回模型默认值。
		out, err := sjson.DeleteBytes(body, effortPath)
		if err != nil {
			return body
		}
		return out
	}
	effort := openAIStyleEffort(cfg, baseModel)
	if effort == "" {
		return body
	}
	out, err := sjson.SetBytes(body, effortPath, effort)
	if err != nil {
		return body
	}
	return out
}

/**
 * @brief 按实际模型支持的档位将后缀写成 OpenAI 或 Codex effort。
 * @param cfg 已解析的思考后缀。
 * @param baseModel 实际转发的模型名。
 * @return 可用于出站请求的等级。
 */
func openAIStyleEffort(cfg thinking.ThinkingConfig, baseModel string) string {
	effort := thinkingEffortLabel(cfg, baseModel)
	if cfg.Mode != thinking.ModeLevel {
		return effort
	}
	info := modelregistry.LookupModelInfo(baseModel, "openai")
	if info == nil || info.Thinking == nil {
		return effort
	}
	return thinking.ClampEffortToLevels(effort, info.Thinking.Levels)
}

/**
 * @brief 将模型后缀写入 Anthropic 字段，并按实际模型表达最低思考模式。
 * @param body 客户端协议正文。
 * @param cfg 已解析的思考后缀。
 * @param baseModel 本次选路实际使用的模型名。
 * @param upstream 选中的上游协议。
 * @return 应用后缀后的正文；显式后缀替换历史消息中的等级指令。
 */
func applyAnthropicThinking(body []byte, cfg thinking.ThinkingConfig, baseModel string, upstream protocol.Protocol) []byte {
	body = clearAnthropicEffortDirectives(body)
	if cfg.Mode == thinking.ModeNone {
		switch anthropicClaude55Family(anthropicCanonicalModelName(baseModel)) {
		case "claude-sonnet-5-5":
			return deleteAnthropicThinkingEffort(setJSONRaw(body, "thinking", `{"type":"between_tools"}`))
		case "claude-haiku-5-5":
			return deleteAnthropicThinkingEffort(setJSONRaw(body, "thinking", `{"type":"disabled"}`))
		}
		out, _ := sjson.DeleteBytes(body, "thinking")
		return deleteAnthropicThinkingEffort(out)
	}
	if cfg.Mode == thinking.ModeBudget {
		return setAnthropicThinkingBudget(body, cfg.Budget)
	}

	var levels []string
	if upstream == protocol.Anthropic {
		if info := claudeThinkingModel(baseModel); info != nil {
			if info.Thinking == nil || (len(info.Thinking.Levels) == 0 && cfg.Mode == thinking.ModeAuto) {
				return deleteAnthropicThinkingEffort(deleteJSONPath(body, "thinking"))
			}
			if len(info.Thinking.Levels) == 0 {
				budget, _ := thinking.ConvertLevelToBudget(string(cfg.Level))
				budget = max(budget, info.Thinking.Min)
				if info.Thinking.Max > 0 {
					budget = min(budget, info.Thinking.Max)
				}
				return setAnthropicThinkingBudget(body, budget)
			}
			levels = info.Thinking.Levels
		} else {
			levels, _ = reasoningCatalogLevels(baseModel)
		}
	} else {
		levels, _ = reasoningModelLevels(baseModel, upstream)
	}

	out, err := sjson.SetBytes(body, "thinking.type", "adaptive")
	if err != nil {
		return body
	}
	out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
	if cfg.Mode == thinking.ModeAuto {
		return deleteAnthropicThinkingEffort(out)
	}
	effort := string(cfg.Level)
	if upstream != protocol.Anthropic {
		effort = thinking.ClampEffortToLevels(effort, levels)
	} else if !thinking.HasLevel(levels, effort) {
		effort, _ = thinking.MapToClaudeEffort(effort, len(levels) == 0 || thinking.HasLevel(levels, "max"))
	}
	out, _ = sjson.SetBytes(out, "output_config.effort", effort)
	return out
}

/**
 * @brief 通过官方别名查询 Claude 模型的思考能力。
 * @param modelName 路由后的模型名，可带供应商前缀和上下文选择器。
 * @return 已登记的模型信息，未知时为空。
 */
func claudeThinkingModel(modelName string) *modelregistry.ModelInfo {
	name := strings.TrimSuffix(anthropicCanonicalModelName(modelName), "[1m]")
	if family := anthropicClaude55Family(name); family != "" {
		name = family
	}
	name = strings.ReplaceAll(name, ".", "-")
	return modelregistry.LookupModelInfo(cliproxyutil.ResolveClaudeThinkingModelAlias(name), "claude")
}

/**
 * @brief 在渠道规则生效后将合成的手动预算收敛到输出上限。
 * @param body 已最终化的 Claude 请求。
 * @param generatedBudget 渠道规则生效前的手动预算。
 * @param level 生成预算的等级；非等级请求为空。
 * @param modelName 实际路由的 Claude 模型。
 * @return 调整后的正文；渠道显式修改的预算保持原值。
 * @return 最终手动思考模式与预算冲突时的请求校验错误。
 */
func finalizeAnthropicManualThinkingLevel(body []byte, generatedBudget int64, level thinking.ThinkingLevel, modelName string) ([]byte, error) {
	thinkingConfig := gjson.GetBytes(body, "thinking")
	if level == "" || generatedBudget == 0 || thinkingConfig.Get("type").Str != "enabled" {
		return body, nil
	}
	info := claudeThinkingModel(modelName)
	minBudget := int64(info.Thinking.Min)
	budget := thinkingConfig.Get("budget_tokens").Int()
	if budget < minBudget {
		return nil, &requestValidationError{message: fmt.Sprintf("thinking.budget_tokens must be at least %d for thinking level %q", minBudget, level)}
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if maxTokens.Type != gjson.Number {
		maxTokens = gjson.Parse(anthropicClaudeCodeDefaultMaxTokens(modelName))
	}
	limit := maxTokens.Int()
	if limit > budget {
		return body, nil
	}
	if budget != generatedBudget || limit <= minBudget {
		if budget == generatedBudget {
			budget = minBudget
		}
		return nil, &requestValidationError{message: fmt.Sprintf("max_tokens must exceed %d for thinking level %q", budget, level)}
	}
	return setJSONValue(body, "thinking.budget_tokens", limit-1), nil
}

/**
 * @brief 把手动思考预算写入 Claude 请求并清除与其冲突的等级。
 * @param body Claude 请求正文。
 * @param budget 手动思考预算。
 * @return 更新后的正文。
 */
func setAnthropicThinkingBudget(body []byte, budget int) []byte {
	out, err := sjson.SetBytes(body, "thinking.type", "enabled")
	if err != nil {
		return body
	}
	out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
	return deleteAnthropicThinkingEffort(out)
}

// Explicit model suffixes and automatic decisions replace the client's current
// effort, including directives retained in conversation history.
func clearAnthropicEffortDirectives(body []byte) []byte {
	for index, message := range gjson.GetBytes(body, "messages").Array() {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") && message.Get("output_config.effort").Exists() {
			body = deleteJSONPath(body, fmt.Sprintf("messages.%d.output_config.effort", index))
		}
	}
	return body
}

func deleteAnthropicThinkingEffort(body []byte) []byte {
	out, _ := sjson.DeleteBytes(body, "output_config.effort")
	if outputConfig := gjson.GetBytes(out, "output_config"); outputConfig.IsObject() && len(outputConfig.Map()) == 0 {
		out, _ = sjson.DeleteBytes(out, "output_config")
	}
	return out
}

/**
 * @brief 应用 Gemini 思考后缀，在原生与跨协议请求中保留实际支持的等级。
 * @param body 客户端 Gemini 请求正文。
 * @param cfg 已解析的模型思考后缀。
 * @param baseModel 实际路由后的模型名。
 * @param upstream 选中的上游协议。
 * @return 后缀应用后的请求正文，其他生成参数不变。
 */
func applyGeminiThinking(body []byte, cfg thinking.ThinkingConfig, baseModel string, upstream protocol.Protocol) []byte {
	var level string
	switch cfg.Mode {
	case thinking.ModeNone, thinking.ModeAuto, thinking.ModeBudget:
		// 解析得到的预算 0 关闭思考，-1 使用模型默认值。
	case thinking.ModeLevel:
		level = string(cfg.Level)
		switch upstream {
		case protocol.Gemini:
			level = normalizeGeminiThinkingLevel(level, baseModel)
		case protocol.OpenAI, protocol.Codex:
			levels, _ := reasoningModelLevels(baseModel, upstream)
			level = thinking.ClampEffortToLevels(level, levels)
		}
	default:
		return body
	}
	out, err := writeGeminiThinking(body, level, cfg.Budget)
	if err != nil {
		return body
	}
	return out
}

/**
 * @brief 在现有 Gemini 生成配置中写入等级或预算，清除各别名容器中的冲突设置。
 * @param body 客户端 Gemini 请求正文。
 * @param level 要写入的等级，为空时改写预算。
 * @param budget level 为空时使用的预算。
 * @return 写入思考设置后的正文，其他生成参数保留。
 * @return sjson 无法写入时的错误。
 */
func writeGeminiThinking(body []byte, level string, budget int) ([]byte, error) {
	root := gjson.ParseBytes(body)
	generationKey := "generationConfig"
	generation := root.Get(generationKey)
	if snake := root.Get("generation_config"); !generation.IsObject() && snake.IsObject() {
		generationKey, generation = "generation_config", snake
	}

	thinkingKey := "thinkingConfig"
	levelKey, budgetKey := geminiThinkingLevelKey, geminiThinkingBudgetKey
	if !generation.Get(thinkingKey).IsObject() && (generation.Get("thinking_config").IsObject() || generationKey == "generation_config") {
		thinkingKey, levelKey, budgetKey = "thinking_config", "thinking_level", "thinking_budget"
	}
	key, value := levelKey, any(level)
	if level == "" {
		key, value = budgetKey, budget
	}

	path := generationKey + "." + thinkingKey + "."
	for _, generationAlias := range [...]string{"generationConfig", "generation_config"} {
		gen := root.Get(generationAlias)
		// Codex also reads a legacy level directly from the generation config.
		for _, alias := range [...]string{geminiThinkingLevelKey, "thinking_level"} {
			if gen.Get(alias).Exists() {
				body = deleteJSONPath(body, generationAlias+"."+alias)
			}
		}
		for _, thinkingAlias := range [...]string{"thinkingConfig", "thinking_config"} {
			config := gen.Get(thinkingAlias)
			prefix := generationAlias + "." + thinkingAlias + "."
			for _, alias := range [...]string{geminiThinkingLevelKey, "thinking_level", geminiThinkingBudgetKey, "thinking_budget"} {
				if (prefix != path || alias != key) && config.Get(alias).Exists() {
					body = deleteJSONPath(body, prefix+alias)
				}
			}
		}
	}
	return sjson.SetBytes(body, path+key, value)
}

/**
 * @brief 按静态模型能力保留 xhigh，其他后缀沿用 Gemini 等级映射。
 * @param level 请求的思考等级。
 * @param baseModel 实际路由后的模型名。
 * @return 可发送的等级；未知等级返回空字符串。
 */
func normalizeGeminiThinkingLevel(level, baseModel string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "xhigh":
		if info := modelregistry.LookupModelInfo(baseModel, "gemini"); info != nil && info.Thinking != nil && thinking.HasLevel(info.Thinking.Levels, "xhigh") {
			return "xhigh"
		}
		return "high"
	case "high", "max":
		return "high"
	default:
		return ""
	}
}
