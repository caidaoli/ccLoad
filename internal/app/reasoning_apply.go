package app

import (
	"slices"
	"strings"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/tidwall/sjson"
)

/**
 * @brief 在协议转换前将自动思考等级写入客户端请求体。
 * @param body 转换前的客户端请求体。
 * @param r 当前业务请求及其判断状态。
 * @param upstream 本次尝试的上游协议。
 * @param actualModel 实际路由目标。
 * @return 已写入等级的请求体；没有可应用的等级时沿用原文。
 */
func applyAutomaticReasoning(body []byte, r *proxyRequestContext, upstream protocol.Protocol, actualModel string) []byte {
	effort := automaticReasoningEffort(r, upstream, actualModel, nil, false)
	if effort == "" {
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
 * @brief 依据本次尝试的实际模型选择可发送的自动等级。
 * @param r 当前业务请求及其判断状态。
 * @param upstream 内置能力目录的查找协议。
 * @param actualModel 实际路由目标。
 * @param levels 账号确认的等级；确认不支持时为空。
 * @param known 账号目录是否确认该模型的能力。
 * @return 可发送的等级；本次无需应用或无法应用时为空。
 * @note 重置上一次尝试的应用状态；成功构造出站请求后由调用方记录预期等级。
 */
func automaticReasoningEffort(r *proxyRequestContext, upstream protocol.Protocol, actualModel string, levels []string, known bool) string {
	if r.reasoning == nil {
		return ""
	}
	r.reasoningExpected, r.reasoningApplied, r.reasoningApplyReason = "", "", ""
	turn := r.reasoning
	if turn.metadata.Status == "skipped" || turn.effort == "" {
		return ""
	}
	if !known {
		levels, known = reasoningModelLevels(actualModel, upstream)
	}
	if len(levels) == 0 {
		if known {
			// Budget-only or non-reasoning models reject effort levels.
			r.reasoningApplyReason = "unknown_model_capability"
			return ""
		}
		// The decision chose among the logical model's levels; a redirect name
		// the catalogs cannot resolve still serves that model.
		levels = turn.levels
	}
	// 会话复用的等级可能来自另一组路由，先对齐本轮候选再解析实际目标。
	effort := thinking.ClampEffortToLevels(turn.effort, turn.levels)
	effort = thinking.ClampEffortToLevels(effort, levels)
	if !thinking.HasLevel(levels, effort) || effort == "none" || effort == "auto" {
		r.reasoningApplyReason = "unsupported_model_effort"
		return ""
	}
	return effort
}

/**
 * @brief 读取账号发现的 Cursor 等级，优先匹配 SDK 模型 ID 的原始拼写。
 * @param credential Cursor 渠道凭证，nil 表示没有有效账号目录。
 * @param modelID SDK 实际模型 ID 或目录已登记的变体名。
 * @return 目录中的等级列表；条目存在但不支持时可为空。
 * @return 是否有明确条目；仅大小写不同且匹配多个条目时为 false。
 */
func cursorCatalogEffortLevels(credential *cursorauth.Credential, modelID string) ([]string, bool) {
	if credential == nil {
		return nil, false
	}
	return cursorCatalogValue(credential.EffortLevels, modelID)
}

/**
 * @brief 精确或唯一忽略大小写匹配同一账号的模型能力。
 * @tparam T 能力字段的类型。
 * @param values 按实际模型 ID 索引的目录。
 * @param modelID 客户端选定的实际模型。
 * @return 匹配值，缺失或歧义时为零值。
 * @return 是否有唯一的对应条目。
 */
func cursorCatalogValue[T any](values map[string]T, modelID string) (T, bool) {
	if levels, ok := values[modelID]; ok {
		return levels, true
	}
	var matched T
	found := false
	for id, levels := range values {
		if !strings.EqualFold(id, modelID) {
			continue
		}
		if found {
			var zero T
			return zero, false
		}
		matched, found = levels, true
	}
	return matched, found
}

/**
 * @brief 累积候选路由的共同等级，保留初始目录的顺序。
 * @param common 已确认的交集，nil 表示尚未累积；空切片表示没有共同等级。
 * @param levels 当前候选支持的等级。
 * @return 独立于两个输入的交集；没有共同等级时为空切片。
 */
func intersectReasoningLevels(common, levels []string) []string {
	if common == nil {
		return append([]string{}, levels...)
	}
	result := make([]string, 0, len(common))
	for _, level := range common {
		if thinking.HasLevel(levels, level) {
			result = append(result, level)
		}
	}
	return result
}

/**
 * @brief 读取实际上游模型允许的思考等级
 * @param model 实际路由后的模型名，可带供应商前缀，大小写无关
 * @param upstream 本次使用的上游协议，决定优先查询的目录
 * @return 模型允许的思考等级；未知或无档位时返回 nil
 * @return 是否已知目标模型的思考能力，包括只有预算或不支持思考
 */
func reasoningModelLevels(model string, upstream protocol.Protocol) ([]string, bool) {
	provider := "openai"
	switch upstream {
	case protocol.Anthropic:
		if info := claudeThinkingModel(model); info != nil {
			if info.Thinking == nil {
				return nil, true
			}
			return info.Thinking.Levels, true
		}
		return reasoningCatalogLevels(model)
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
 * @return 是否有明确的模型思考能力信息
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
 * @brief 取得业务模型可交给判断模型选择的等级。
 * @param logical 逻辑业务模型名。
 * @param channels 当前允许尝试的渠道；管理场景为该模型全部启用渠道。
 * @param client 当前客户端协议；管理场景为空，核对可用客户端协议。
 * @return 按强度升序的可判断等级；可选等级不足两档时为 nil。
 * @details 有 Cursor 账号能力时取候选目标的交集；无账号能力时沿用逻辑模型的内置目录。
 */
func (s *Server) reasoningDecisionLevels(logical string, channels []*model.Config, client protocol.Protocol) []string {
	catalog, _ := reasoningCatalogLevels(logical)
	if slices.ContainsFunc(channels, (*model.Config).UsesCursorOAuth) {
		if common, known := s.cursorDecisionLevels(logical, channels, client, catalog); known {
			catalog = common
		}
	}
	var levels []string
	for _, level := range thinking.OrderedLevels() {
		if reasoningLevelCriteria[level] != "" && thinking.HasLevel(catalog, level) {
			levels = append(levels, level)
		}
	}
	if len(levels) < 2 {
		return nil
	}
	return levels
}

/**
 * @brief 汇总 Cursor 实际模型与可回退路由共同支持的自动等级。
 * @param logical 逻辑业务模型名。
 * @param channels 允许尝试的候选渠道。
 * @param client 当前客户端协议；为空时汇总所有聊天协议。
 * @param fallback 旧逻辑目录可用于目标名未知的中继，未知时为 nil。
 * @return 各目标支持等级的交集。
 * @return 是否至少有一个实际 Cursor 目标具备明确的账号目录能力。
 */
func (s *Server) cursorDecisionLevels(logical string, channels []*model.Config, client protocol.Protocol, fallback []string) ([]string, bool) {
	clients := []protocol.Protocol{client}
	if client == "" {
		clients = automaticFallbackProtocolOrder[:]
	}
	var common []string
	var knownCursor bool
	for _, cfg := range channels {
		cfg = s.withOAuthBaseURLOverride(cfg)
		cursor := cursorCatalogCredential(cfg)
		rows := s.enumerateModelRows(cfg, logical)
		for _, client := range clients {
			upstreams := possibleUpstreamProtocols(cfg, client)
			if cfg.UsesCursorOAuth() && len(upstreams) > 0 {
				// Cursor 内部转换到 Chat，实际模型按客户端协议解析。
				upstreams = []protocol.Protocol{client}
			}
			for _, row := range rows {
				for _, upstream := range upstreams {
					actual := s.resolveFinalUpstreamModel(cfg, row, string(upstream))
					levels, known := cursorCatalogEffortLevels(cursor, actual)
					knownCursor = knownCursor || known
					if !known {
						lookupProtocol := upstream
						if cfg.UsesCursorOAuth() {
							lookupProtocol = protocol.OpenAI
						}
						levels, known = reasoningModelLevels(actual, lookupProtocol)
						if !known {
							levels = fallback
						}
					}
					common = intersectReasoningLevels(common, levels)
				}
			}
		}
	}
	return common, knownCursor
}

/**
 * @brief 在客户端协议正文中写入选定的思考等级供后续转换。
 * @param body 应用思考决策前的请求 JSON。
 * @param client 决定思考字段位置的客户端协议。
 * @param effort 已选定的思考等级。
 * @return 写入等级的正文；Claude 请求同时清除与等级冲突的预算和指令。
 * @return 修改 JSON 失败时返回对应错误。
 */
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
		return writeGeminiThinking(body, effort, 0)
	default:
		return sjson.SetBytes(body, "reasoning_effort", effort)
	}
}
