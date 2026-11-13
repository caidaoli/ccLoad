package app

import (
	"fmt"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 模型名思考后缀是"客户端在请求体里显式写思考参数"的语法糖。网关只在客户端协议的
// 原始请求体上改写一次，位置在协议转换之前：跨协议映射与模型能力裁剪归
// protocol/registry 的转换器，Antigravity 的 request 信封也在这之后才包上。
// 不要把这段逻辑挪到 prepareTranslatedUpstreamBody——那里的 body 形态取决于上游，
// 改写必须同时预判信封结构和后续所有归一化步骤。

const (
	geminiThinkingLevelPath  = "generationConfig.thinkingConfig.thinkingLevel"
	geminiThinkingBudgetPath = "generationConfig.thinkingConfig.thinkingBudget"
)

// applyThinkingSuffix 把 requestedModel 尾部的思考后缀写成 clientProtocol 的请求字段。
// body 必须是客户端协议的原始请求体，写出的形态要和客户端自己发这个字段完全一致。
func applyThinkingSuffix(body []byte, clientProtocol protocol.Protocol, requestedModel string) []byte {
	return applyThinkingSuffixForModel(
		body,
		clientProtocol,
		requestedModel,
		model.RoutingModelName(requestedModel),
	)
}

// applyThinkingSuffixForModel 保留 requestedModel 的后缀语义，但按 resolvedModel 的能力
// 收敛等级。模型重定向和模糊匹配发生在选路之后，因此代理链路必须在每次渠道尝试时调用它。
func applyThinkingSuffixForModel(
	body []byte,
	clientProtocol protocol.Protocol,
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
		return applyAnthropicThinking(body, cfg, resolvedModel)
	case protocol.Gemini:
		return applyGeminiThinking(body, cfg, resolvedModel)
	default:
		return body
	}
}

// thinkingEffortLabel 把后缀归一成日志用的等级名。
func thinkingEffortLabel(cfg thinking.ThinkingConfig) string {
	switch cfg.Mode {
	case thinking.ModeNone:
		return string(thinking.LevelNone)
	case thinking.ModeAuto:
		return string(thinking.LevelAuto)
	case thinking.ModeLevel:
		return strings.ToLower(strings.TrimSpace(string(cfg.Level)))
	case thinking.ModeBudget:
		level, _ := thinking.ConvertBudgetToLevel(cfg.Budget)
		return level
	default:
		return ""
	}
}

// thinkingEffortFromRequest 优先采用后缀声明的等级：(none)/(auto) 在部分协议上以
// "删除字段"表达，请求体里读不回来。
func thinkingEffortFromRequest(requestedModel string, body []byte) string {
	if _, cfg, ok := model.ParseThinkingSuffix(requestedModel); ok {
		if label := thinkingEffortLabel(cfg); label != "" {
			return label
		}
	}
	return extractThinkingEffortFromJSON(body)
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

// openAIStyleEffort 按 catalog 里该模型的 thinking.levels 收敛后缀，对齐
// CLIProxyAPI ApplyThinking：模型支持 max 就保留 max，不支持则夹到最近档。
func openAIStyleEffort(cfg thinking.ThinkingConfig, baseModel string) string {
	return clampOpenAIStyleEffort(thinkingEffortLabel(cfg), baseModel)
}

var openAIStyleLevelOrder = []string{
	string(thinking.LevelMinimal),
	string(thinking.LevelLow),
	string(thinking.LevelMedium),
	string(thinking.LevelHigh),
	string(thinking.LevelXHigh),
	string(thinking.LevelMax),
}

func clampOpenAIStyleEffort(effort, baseModel string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	info := modelregistry.LookupModelInfo(baseModel, "openai")
	if info == nil || info.Thinking == nil {
		return effort
	}
	return clampEffortToLevels(effort, info.Thinking.Levels)
}

// clampEffortToLevels picks the nearest supported level, preferring the lower
// one on a tie; unknown inputs and empty level lists pass through.
func clampEffortToLevels(effort string, levels []string) string {
	if len(levels) == 0 || thinking.HasLevel(levels, effort) {
		return effort
	}
	pos := indexOfOpenAIStyleLevel(effort)
	if pos < 0 {
		return effort
	}
	bestIdx, bestDist := -1, len(openAIStyleLevelOrder)+1
	for _, supported := range levels {
		idx := indexOfOpenAIStyleLevel(supported)
		if idx < 0 {
			continue
		}
		dist := idx - pos
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist || (dist == bestDist && idx < bestIdx) {
			bestIdx, bestDist = idx, dist
		}
	}
	if bestIdx >= 0 {
		return openAIStyleLevelOrder[bestIdx]
	}
	return effort
}

func indexOfOpenAIStyleLevel(level string) int {
	level = strings.ToLower(strings.TrimSpace(level))
	for i, name := range openAIStyleLevelOrder {
		if name == level {
			return i
		}
	}
	return -1
}

/**
 * @brief 将模型后缀写入 Anthropic 字段，并按实际模型表达最低思考模式。
 * @param body 客户端协议正文。
 * @param cfg 已解析的思考后缀。
 * @param baseModel 本次选路实际使用的模型名。
 * @return 应用后缀后的正文；显式后缀替换历史消息中的等级指令。
 */
func applyAnthropicThinking(body []byte, cfg thinking.ThinkingConfig, baseModel string) []byte {
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
		out, err := sjson.SetBytes(body, "thinking.type", "enabled")
		if err != nil {
			return body
		}
		out, _ = sjson.SetBytes(out, "thinking.budget_tokens", cfg.Budget)
		return deleteAnthropicThinkingEffort(out)
	}

	out, err := sjson.SetBytes(body, "thinking.type", "adaptive")
	if err != nil {
		return body
	}
	out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
	if cfg.Mode == thinking.ModeAuto {
		return deleteAnthropicThinkingEffort(out)
	}
	effort, ok := thinking.MapToClaudeEffort(string(cfg.Level), true)
	if !ok {
		return deleteAnthropicThinkingEffort(out)
	}
	if cfg.Level == thinking.LevelXHigh && anthropicClaude55Family(anthropicCanonicalModelName(baseModel)) != "" {
		effort = "xhigh"
	}
	out, _ = sjson.SetBytes(out, "output_config.effort", effort)
	return out
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
 * @brief 应用 Gemini 思考后缀，按实际模型保留受支持的 xhigh 等级。
 * @param body 客户端 Gemini 请求正文。
 * @param cfg 已解析的模型思考后缀。
 * @param baseModel 实际路由后的模型名。
 * @return 后缀应用后的请求正文，其他生成参数不变。
 */
func applyGeminiThinking(body []byte, cfg thinking.ThinkingConfig, baseModel string) []byte {
	switch cfg.Mode {
	case thinking.ModeNone:
		// Gemini 的 ThinkingLevel 枚举没有 none，关闭思考靠 thinkingBudget=0。
		return setGeminiThinkingBudget(body, 0)
	case thinking.ModeAuto:
		return setGeminiThinkingBudget(body, -1)
	case thinking.ModeBudget:
		return setGeminiThinkingBudget(body, cfg.Budget)
	case thinking.ModeLevel:
		level := normalizeGeminiThinkingLevel(string(cfg.Level), baseModel)
		if level == "" {
			return body
		}
		out, _ := sjson.DeleteBytes(body, geminiThinkingBudgetPath)
		out, err := sjson.SetBytes(out, geminiThinkingLevelPath, level)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}

func setGeminiThinkingBudget(body []byte, budget int) []byte {
	out, _ := sjson.DeleteBytes(body, geminiThinkingLevelPath)
	out, err := sjson.SetBytes(out, geminiThinkingBudgetPath, budget)
	if err != nil {
		return body
	}
	return out
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
