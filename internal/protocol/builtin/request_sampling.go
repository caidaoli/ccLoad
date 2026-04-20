package builtin

import (
	"strings"

	"github.com/bytedance/sonic"
)

// buildOpenAISampling 从 OpenAI chat.completions 请求中抽取采样/上限参数。
// max_completion_tokens 优先于 max_tokens（OpenAI o-系列模型的新字段）。
// 全部字段为空时返回 nil，避免空结构污染 conversation。
func buildOpenAISampling(req openAIChatRequest) *samplingParams {
	sp := &samplingParams{
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		TopK:             req.TopK,
		Seed:             req.Seed,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		User:             strings.TrimSpace(req.User),
		ReasoningEffort:  strings.TrimSpace(strings.ToLower(req.ReasoningEffort)),
	}
	switch {
	case req.MaxCompletionTokens != nil:
		sp.MaxTokens = req.MaxCompletionTokens
	case req.MaxTokens != nil:
		sp.MaxTokens = req.MaxTokens
	}
	sp.Stop = parseStopSequences(req.Stop)
	if samplingParamsIsZero(sp) {
		return nil
	}
	return sp
}

// buildCodexSampling 从 Codex /v1/responses 请求中抽取采样/上限/推理参数。
// reasoning.effort → ReasoningEffort；max_output_tokens → MaxTokens；stop 同 OpenAI 接受 string/[]string。
func buildCodexSampling(req codexRequest) *samplingParams {
	sp := &samplingParams{
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		TopK:             req.TopK,
		MaxTokens:        req.MaxOutputTokens,
		Seed:             req.Seed,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		User:             strings.TrimSpace(req.User),
	}
	if req.Reasoning != nil {
		sp.ReasoningEffort = strings.ToLower(strings.TrimSpace(req.Reasoning.Effort))
	}
	sp.Stop = parseStopSequences(req.Stop)
	if samplingParamsIsZero(sp) {
		return nil
	}
	return sp
}

// parseStopSequences 接受 OpenAI stop 字段的两种形态：字符串或字符串数组。
// 其它类型静默丢弃，与 OpenAI 官方行为一致。
func parseStopSequences(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var asSlice []string
	if err := sonic.Unmarshal(raw, &asSlice); err == nil {
		out := asSlice[:0]
		for _, s := range asSlice {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	var asString string
	if err := sonic.Unmarshal(raw, &asString); err == nil {
		if s := strings.TrimSpace(asString); s != "" {
			return []string{s}
		}
	}
	return nil
}

// openAIReasoningEffortToThinking 把 OpenAI reasoning_effort 枚举映射成
// Anthropic 风格 thinking 结构，供 Anthropic/Codex/Gemini 编码器复用。
// 未指定或未识别值返回 nil，保留现有行为（不启用思考）。
func openAIReasoningEffortToThinking(effort string) *anthropicThinkingConfig {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return nil
	case "none":
		return &anthropicThinkingConfig{Type: "disabled"}
	case "minimal", "low":
		return &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 1024}
	case "medium", "auto":
		return &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 4096}
	case "high":
		return &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 16384}
	default:
		return &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 4096}
	}
}

func samplingParamsIsZero(sp *samplingParams) bool {
	if sp == nil {
		return true
	}
	return sp.Temperature == nil && sp.TopP == nil && sp.TopK == nil &&
		sp.MaxTokens == nil && len(sp.Stop) == 0 && sp.ReasoningEffort == "" &&
		sp.Seed == nil && sp.FrequencyPenalty == nil && sp.PresencePenalty == nil &&
		sp.User == ""
}
