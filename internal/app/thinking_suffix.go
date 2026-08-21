package app

import (
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
		return applyAnthropicThinking(body, cfg)
	case protocol.Gemini:
		return applyGeminiThinking(body, cfg)
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
	if info == nil || info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		return effort
	}
	if thinking.HasLevel(info.Thinking.Levels, effort) {
		return effort
	}
	pos := indexOfOpenAIStyleLevel(effort)
	if pos < 0 {
		return effort
	}
	bestIdx, bestDist := -1, len(openAIStyleLevelOrder)+1
	for _, supported := range info.Thinking.Levels {
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

// applyAnthropicThinking 只写客户端协议字段。等级走 adaptive + MapToClaudeEffort，
// 数字预算走 enabled；能力裁剪归转换器/后续归一化，这里不查 catalog——选路和
// 重定向前拿不到实际上游模型。
func applyAnthropicThinking(body []byte, cfg thinking.ThinkingConfig) []byte {
	if cfg.Mode == thinking.ModeNone {
		// ccLoad 全链路以"没有 thinking 字段"表示关闭思考（见 normalizeAnthropicThinking），
		// 直接写成终态，避免归一化路径与原生 Claude Code 直通路径给出两种线协议。
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
	out, _ = sjson.SetBytes(out, "output_config.effort", effort)
	return out
}

func deleteAnthropicThinkingEffort(body []byte) []byte {
	out, _ := sjson.DeleteBytes(body, "output_config.effort")
	if outputConfig := gjson.GetBytes(out, "output_config"); outputConfig.IsObject() && len(outputConfig.Map()) == 0 {
		out, _ = sjson.DeleteBytes(out, "output_config")
	}
	return out
}

func applyGeminiThinking(body []byte, cfg thinking.ThinkingConfig) []byte {
	switch cfg.Mode {
	case thinking.ModeNone:
		// Gemini 的 ThinkingLevel 枚举没有 none，关闭思考靠 thinkingBudget=0。
		return setGeminiThinkingBudget(body, 0)
	case thinking.ModeAuto:
		return setGeminiThinkingBudget(body, -1)
	case thinking.ModeBudget:
		return setGeminiThinkingBudget(body, cfg.Budget)
	case thinking.ModeLevel:
		level := normalizeGeminiThinkingLevel(string(cfg.Level))
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

// normalizeGeminiThinkingLevel 收敛到 Gemini ThinkingLevel 枚举，
// 与 normalizeAntigravityThinkingLevel 的契约保持一致。
func normalizeGeminiThinkingLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	default:
		return ""
	}
}
