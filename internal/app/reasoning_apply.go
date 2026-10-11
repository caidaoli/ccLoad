package app

import (
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"

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
	levels := reasoningModelLevels(actualModel, upstream)
	if len(levels) == 0 {
		r.reasoningApplyReason = "unknown_model_capability"
		return body
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

// The upstream's own catalog is authoritative; relays may serve a model under a
// different protocol, so the fullest entry in any catalog is the fallback.
func reasoningModelLevels(model string, upstream protocol.Protocol) []string {
	provider := "openai"
	switch upstream {
	case protocol.Anthropic:
		provider = "claude"
	case protocol.Gemini:
		provider = "gemini"
	}
	if info := modelregistry.LookupModelInfo(model, provider); info != nil && info.Thinking != nil && len(info.Thinking.Levels) > 0 {
		return info.Thinking.Levels
	}
	return modelregistry.LookupThinkingLevels(model)
}

// Levels the decision may choose for a business model, ascending. A redirected
// model is still clamped to its own levels at send time.
func reasoningDecisionLevels(model string) []string {
	var levels []string
	catalog := modelregistry.LookupThinkingLevels(model)
	for _, level := range openAIStyleLevelOrder {
		if reasoningLevelCriteria[level] != "" && thinking.HasLevel(catalog, level) {
			levels = append(levels, level)
		}
	}
	if len(levels) < 2 {
		return reasoningDefaultLevels
	}
	return levels
}

func writeReasoningEffort(body []byte, client protocol.Protocol, effort string) ([]byte, error) {
	switch client {
	case protocol.Codex:
		return sjson.SetBytes(body, "reasoning.effort", effort)
	case protocol.Anthropic:
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
