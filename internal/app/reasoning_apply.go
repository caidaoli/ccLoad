package app

import (
	"ccLoad/internal/protocol"
	modelregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"

	"github.com/tidwall/sjson"
)

func applyAutomaticReasoning(body []byte, r *proxyRequestContext, upstream protocol.Protocol, actualModel string) []byte {
	if r.reasoning == nil {
		return body
	}
	r.reasoningExpected, r.reasoningApplied, r.reasoningApplyReason = "", "", ""
	turn := r.reasoning
	if turn.metadata.Status == "skipped" || turn.effort == "" {
		return body
	}
	// OpenAI and Responses expose the same ordered effort semantics. Other
	// provider budgets/adaptive modes have no verified equivalence in this phase.
	if upstream != protocol.OpenAI && upstream != protocol.Codex {
		r.reasoningApplyReason = "unsupported_upstream_protocol"
		return body
	}
	info := modelregistry.LookupModelInfo(actualModel, "openai")
	if info == nil || info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		r.reasoningApplyReason = "unknown_model_capability"
		return body
	}
	effort := clampOpenAIStyleEffort(turn.effort, actualModel)
	if !thinking.HasLevel(info.Thinking.Levels, effort) || effort == "none" || effort == "auto" {
		r.reasoningApplyReason = "unsupported_model_effort"
		return body
	}
	path := "reasoning_effort"
	if r.clientProtocol == protocol.Codex {
		path = "reasoning.effort"
	}
	out, err := sjson.SetBytes(body, path, effort)
	if err != nil {
		r.reasoningApplyReason = "invalid_request_body"
		return body
	}
	r.reasoningExpected = effort
	return out
}
