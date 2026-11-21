package app

import (
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/tidwall/sjson"
)

// The decision is written into the client-protocol body before translation, like
// a thinking suffix; translators carry it to whichever upstream serves the model.
func applyAutomaticReasoning(body []byte, r *proxyRequestContext, upstream protocol.Protocol, actualModel string) []byte {
	if r.reasoning == nil {
		return body
	}
	r.reasoningExpected, r.reasoningApplied, r.reasoningApplyReason = "", "", ""
	turn := r.reasoning
	if turn.metadata.Status == "skipped" || turn.effort == "" {
		return body
	}
	levels, known := reasoningModelLevels(actualModel, upstream)
	if len(levels) == 0 {
		if known {
			// Budget-only or non-reasoning models reject effort levels.
			r.reasoningApplyReason = "unknown_model_capability"
			return body
		}
		// The decision chose among the logical model's levels; a redirect name
		// the catalogs cannot resolve still serves that model.
		levels = turn.levels
	}
	effort := clampEffortToLevels(turn.effort, levels)
	if !thinking.HasLevel(levels, effort) || effort == "none" || effort == "auto" {
		r.reasoningApplyReason = "unsupported_model_effort"
		return body
	}
	out, err := writeReasoningEffort(body, r.clientProtocol, effort)
	if err != nil {
		r.reasoningApplyReason = "invalid_request_body"
		return body
	}
	r.reasoningExpected = effort
	return out
}

/**
 * @brief 读取实际上游模型允许的思考等级
 * @param model 实际路由后的模型名，可带供应商前缀，大小写无关
 * @param upstream 本次使用的上游协议，决定优先查询的目录
 * @return 模型允许的思考等级；目录未收录或收录但无档位时返回 nil
 * @return 任一目录是否收录该模型；收录但无档位（只有预算或不支持思考）时为 true
 */
func reasoningModelLevels(model string, upstream protocol.Protocol) ([]string, bool) {
	provider := "openai"
	switch upstream {
	case protocol.Anthropic:
		provider = "claude"
	case protocol.Gemini:
		provider = "gemini"
	}
	if info := modelregistry.LookupModelInfo(anthropicCanonicalModelName(model), provider); info != nil && info.Thinking != nil && len(info.Thinking.Levels) > 0 {
		return info.Thinking.Levels, true
	}
	return reasoningCatalogLevels(model)
}

/**
 * @brief 按模型名查询已知思考等级，不区分上游协议
 * @param model 模型名，可带供应商前缀（如 `Qwen/Qwen3.8-Max`），大小写无关
 * @return 依次取 CLIProxy 内置目录、models.dev 目录、尚未进入快照的 Anthropic 5.2 官方等级；都无档位时返回 nil
 * @return 任一目录是否收录该模型，收录但无档位时为 true
 */
func reasoningCatalogLevels(model string) ([]string, bool) {
	name := anthropicCanonicalModelName(model)
	if levels := modelregistry.LookupThinkingLevels(name); len(levels) > 0 {
		return levels, true
	}
	levels, known := util.ModelReasoningEfforts(name)
	if len(levels) > 0 {
		return levels, true
	}
	if anthropicClaude52Family(name) != "" {
		return []string{string(thinking.LevelLow), string(thinking.LevelMedium), string(thinking.LevelHigh),
			string(thinking.LevelXHigh), string(thinking.LevelMax)}, true
	}
	return nil, known || modelregistry.LookupModelInfo(name) != nil
}

/**
 * @brief 业务模型可交给判断模型选择的等级
 * @param model 逻辑业务模型名
 * @return 按强度升序的可判断等级；可选等级不足两档（含未收录）时返回 nil，表示无需判断
 * @note 重定向后的实际模型在发送时仍按自身等级裁剪
 */
func reasoningDecisionLevels(model string) []string {
	var levels []string
	catalog, _ := reasoningCatalogLevels(model)
	for _, level := range openAIStyleLevelOrder {
		if reasoningLevelCriteria[level] != "" && thinking.HasLevel(catalog, level) {
			levels = append(levels, level)
		}
	}
	if len(levels) < 2 {
		return nil
	}
	return levels
}

func writeReasoningEffort(body []byte, client protocol.Protocol, effort string) ([]byte, error) {
	switch client {
	case protocol.Codex:
		return sjson.SetBytes(body, "reasoning.effort", effort)
	case protocol.Anthropic:
		body = clearAnthropicEffortDirectives(body)
		// Same wire as a level suffix: translators read output_config.effort only
		// under adaptive thinking, and a fixed budget would override the level.
		out, err := sjson.SetBytes(body, "thinking.type", "adaptive")
		if err != nil {
			return nil, err
		}
		if out, err = sjson.DeleteBytes(out, "thinking.budget_tokens"); err != nil {
			return nil, err
		}
		return sjson.SetBytes(out, "output_config.effort", effort)
	case protocol.Gemini:
		out, err := sjson.DeleteBytes(body, geminiThinkingBudgetPath)
		if err != nil {
			return nil, err
		}
		return sjson.SetBytes(out, geminiThinkingLevelPath, effort)
	default:
		return sjson.SetBytes(body, "reasoning_effort", effort)
	}
}
