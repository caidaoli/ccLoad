package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

const (
	reasoningStateLimit         = 1024
	reasoningStateSweepInterval = time.Minute
	// Replays of real traffic put reliable answers at 0.39 and above, while
	// answers at 0.25 and below flipped between distant levels on identical
	// input; such an answer is recorded but treated as no decision.
	reasoningMinConfidence = 0.3
)

type reasoningState struct {
	mu           sync.Mutex
	version      uint64
	effort       string
	pending      string
	pendingCount int
	toolTurns    int
	force        bool
	lastAccess   time.Time
}

type reasoningStateStore struct {
	mu        sync.Mutex
	entries   map[string]*reasoningState
	lastSweep time.Time
}

func (s *reasoningStateStore) get(key string, now time.Time, ttl time.Duration) *reasoningState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*reasoningState)
	}
	if state := s.entries[key]; state != nil {
		state.mu.Lock()
		expired := now.Sub(state.lastAccess) > ttl
		state.mu.Unlock()
		if !expired {
			return state
		}
		delete(s.entries, key)
	}
	// Sweep lazily: at most once per interval, or when a new entry needs room.
	if len(s.entries) >= reasoningStateLimit || now.Sub(s.lastSweep) >= reasoningStateSweepInterval {
		s.lastSweep = now
		for key, state := range s.entries {
			state.mu.Lock()
			expired := now.Sub(state.lastAccess) > ttl
			state.mu.Unlock()
			if expired {
				delete(s.entries, key)
			}
		}
	}
	if len(s.entries) >= reasoningStateLimit {
		return &reasoningState{}
	}
	state := &reasoningState{lastAccess: now}
	s.entries[key] = state
	return state
}

type reasoningTurn struct {
	metadata     model.ReasoningDecision
	state        *reasoningState
	version      uint64
	effort       string
	pending      string
	pendingCount int
	toolTurns    int
	force        bool
}

func reasoningRank(effort string) int {
	switch effort {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "xhigh":
		return 4
	case "max":
		return 5
	}
	return 0
}

func (t *reasoningTurn) acceptSuggestion(effort string) {
	t.metadata.SuggestedEffort = effort
	t.metadata.Status = "decided"
	t.force = false
	t.toolTurns = 0
	switch {
	case t.effort == "" || reasoningRank(effort) > reasoningRank(t.effort):
		t.effort, t.pending, t.pendingCount = effort, "", 0
	case effort == t.effort:
		t.pending, t.pendingCount = "", 0
	default:
		if t.pending == "" || reasoningRank(effort) > reasoningRank(t.pending) {
			t.pending = effort
		}
		t.pendingCount++
		if t.pendingCount >= 2 {
			t.effort, t.pending, t.pendingCount = t.pending, "", 0
		} else {
			t.metadata.Reason = "downgrade_confirmation"
		}
	}
}

func (t *reasoningTurn) fallback(reason string) {
	t.metadata.Status, t.metadata.Reason = "fallback", reason
	t.pending, t.pendingCount, t.force = "", 0, true
}

func (s *Server) reasoningStateFor(r *proxyRequestContext) *reasoningState {
	identity := responsesExecutionSessionID(r.header)
	if r.clientProtocol == protocol.Anthropic {
		// Claude Code names its session in a header or metadata.user_id.
		if identity = anthropicSessionIDFromHeaders(r.header); identity == "" {
			identity = anthropicSessionIDFromBody(r.body)
		}
	}
	if identity == "" || r.tokenHash == "" {
		return &reasoningState{}
	}
	key := responsesExecutionSessionKey(r.tokenHash, string(r.clientProtocol)+"\x00"+identity+"\x00"+r.originalModel)
	ttl := 15 * time.Minute
	if s.responsesExecutionSessions != nil {
		ttl = s.responsesExecutionSessions.sessionTTL()
	}
	return s.reasoningStates.get(key, time.Now(), ttl)
}

type reasoningRequestContextKey struct{}

// The decision is immutable across routing attempts. Only the attempt's applied
// value and log metadata change when a different model or channel rule wins.
func withReasoningRequest(ctx context.Context, r *proxyRequestContext) context.Context {
	if r.reasoning == nil {
		return ctx
	}
	return context.WithValue(ctx, reasoningRequestContextKey{}, r)
}

func reasoningBusinessSending(ctx context.Context, body []byte) {
	r, _ := ctx.Value(reasoningRequestContextKey{}).(*proxyRequestContext)
	if r == nil || r.reasoning == nil || ctx.Err() != nil {
		return
	}
	t := r.reasoning
	applied := false
	r.reasoningApplied = ""
	if r.reasoningExpected != "" {
		actual := extractThinkingEffortFromJSON(body)
		r.thinkingEffort, r.reasoningApplied = actual, actual
		applied = actual == r.reasoningExpected
		if r.observer != nil && r.observer.OnThinkingEffort != nil {
			r.observer.OnThinkingEffort(actual, t.metadata.ClientEffort)
		}
		if !applied {
			r.reasoningApplyReason = "channel_rules_override"
		}
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	if t.state.version != t.version {
		return
	}
	if t.metadata.Status != "skipped" && t.effort != "" && !applied {
		// A target incapable of applying the automatic result cannot establish a
		// new effective level, and forces a fresh decision next turn. A later
		// capable attempt of the same turn may still commit.
		t.state.pending, t.state.pendingCount, t.state.force = "", 0, true
		return
	}
	// Committing bumps the version so a later incapable retry of the same turn
	// cannot undo the established state. Explicit/fallback rounds still break
	// downgrade runs.
	t.state.version++
	t.state.effort, t.state.pending, t.state.pendingCount = t.effort, t.pending, t.pendingCount
	t.state.toolTurns, t.state.force = t.toolTurns, t.force
}

// Only an applied automatic value needs the wire body to detect channel rule
// overrides; other turns commit without copying the request.
func reasoningHTTPRequestBody(req *http.Request) []byte {
	if r, _ := req.Context().Value(reasoningRequestContextKey{}).(*proxyRequestContext); r == nil || r.reasoningExpected == "" {
		return nil
	}
	if req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err == nil {
		return content
	}
	return nil
}

func (r *proxyRequestContext) reasoningLogMetadata() *model.ReasoningDecision {
	if r.reasoning == nil {
		return nil
	}
	m := r.reasoning.metadata
	m.AppliedEffort = r.reasoningApplied
	// A decision reason (fallback cause, downgrade confirmation) wins; the
	// override remains visible as applied_effort differing from the decision.
	if m.Reason == "" && r.reasoningApplyReason != "" {
		m.Reason = r.reasoningApplyReason
	}
	return &m
}

// A concrete model suffix opts out of automatic decisions; auto explicitly
// delegates effort selection to the gateway. Body effort fields may be replaced.
func hasExplicitThinkingSuffix(requestedModel string) bool {
	_, cfg, ok := model.ParseThinkingSuffix(requestedModel)
	return ok && cfg.Mode != thinking.ModeAuto
}

// Only the generation endpoint itself is a business turn. Request families also
// match sub-resources such as /responses/compact and /responses/input_tokens,
// which must neither call the decision model nor advance session state.
func reasoningTurnEndpoint(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/responses") ||
		strings.HasSuffix(path, "/codex/responses") || strings.HasSuffix(path, "/v1/messages") ||
		strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
}

func (s *Server) reasoningTarget(r *proxyRequestContext) bool {
	return s.reasoningAuto.Enabled && containsExact(s.reasoningAuto.BusinessModels, r.originalModel)
}

func (s *Server) prepareReasoningTurn(ctx context.Context, r *proxyRequestContext, transcript, original []byte, explicit bool) {
	if !s.reasoningTarget(r) {
		return
	}
	// Codex prewarms with generate:false and no user input: there is nothing to
	// judge, and it must not touch the session's level or pending downgrade.
	if gjson.GetBytes(original, "generate").Type == gjson.False {
		r.reasoning = &reasoningTurn{state: &reasoningState{}, metadata: model.ReasoningDecision{Status: "skipped", Reason: "prewarm"}}
		return
	}
	// Codex's guardian sub-agent reviews another agent's planned action at the
	// level Codex picked; the judge would rate the reviewed task instead.
	if strings.EqualFold(r.header.Get("X-OpenAI-Subagent"), "guardian") {
		r.reasoning = &reasoningTurn{state: &reasoningState{}, metadata: model.ReasoningDecision{Status: "skipped", Reason: "guardian"}}
		return
	}
	state := s.reasoningStateFor(r)
	state.mu.Lock()
	state.version++
	t := &reasoningTurn{state: state, version: state.version, effort: state.effort,
		pending: state.pending, pendingCount: state.pendingCount, toolTurns: state.toolTurns, force: state.force}
	state.lastAccess = time.Now()
	state.mu.Unlock()
	t.metadata = model.ReasoningDecision{Status: "skipped", ClientEffort: r.thinkingEffort, PreviousEffort: t.effort}
	r.reasoning = t
	if explicit {
		t.pending, t.pendingCount, t.force = "", 0, true
		t.metadata.Reason = "client_explicit"
		return
	}
	user, failed := reasoningTurnInput(original, r.clientProtocol)
	trigger := "first_turn"
	reuse := false
	if t.effort != "" && !t.force {
		switch {
		case user:
			trigger = "user_input"
		case failed:
			trigger = "tool_failure"
		default:
			t.toolTurns++
			if t.toolTurns < 3 {
				trigger, reuse = "tool_continuation", true
			} else {
				trigger = "tool_periodic"
			}
		}
	}
	t.metadata.Trigger = trigger
	// Reused turns only need completeness; skip building the decision text.
	projection := projectReasoningContext(transcript, r.clientProtocol, !reuse)
	t.metadata.ContextIncomplete = projection.incomplete
	if projection.incomplete {
		t.fallback("incomplete_context")
		return
	}
	if reuse {
		t.metadata.Status = "reused"
		return
	}
	result, err := s.executeReasoningDecision(ctx, projection.text, t.effort, reasoningDecisionLevels(r.originalModel))
	// Decision cost belongs to the System One log row, not the business turn.
	t.metadata.DurationMS = result.duration.Milliseconds()
	if err != nil {
		t.fallback(result.reason)
		return
	}
	t.metadata.Confidence = result.confidence
	if result.confidence != nil && *result.confidence < reasoningMinConfidence {
		t.metadata.SuggestedEffort = result.effort
		t.fallback("low_confidence")
		return
	}
	// Truncation keeps the original task, latest request and recent tail, so it
	// remains a usable decision and may count toward a downgrade.
	t.acceptSuggestion(result.effort)
}

func containsExact(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func reasoningDecisionContext(ctx context.Context) context.Context {
	// Keep cancellation/deadlines, but the administrator's decision channel has
	// no client token channel restrictions and cannot commit the business turn.
	ctx = context.WithValue(ctx, channelRestrictionTokenContextKey{}, (*channelRestrictionState)(nil))
	return context.WithValue(ctx, reasoningRequestContextKey{}, (*proxyRequestContext)(nil))
}

// Usage and cost use the same result keys as other channel tests, so the
// manual-test log records them instead of a free call.
func (s *Server) testSystemOneChannel(ctx context.Context, cfg *model.Config, apiKey, modelName, state string) map[string]any {
	start := time.Now()
	result, err := s.callSystemOne(ctx, cfg, apiKey, modelName, state, "", reasoningDefaultLevels)
	if errors.Is(err, ErrChannelRPMExceeded) {
		return channelRPMExceededTestResult(start, channelRPMRetryAfter(err))
	}
	if errors.Is(err, ErrChannelConcurrencyExceeded) {
		return channelConcurrencyExceededTestResult(start, err)
	}
	result.costUnknown = s.reasoningCostUnknown(cfg, modelName, result)
	out := map[string]any{"success": err == nil, "status_code": result.status, "duration_ms": result.duration.Milliseconds(),
		"effort": result.effort, "cost_unknown": result.costUnknown, "client_protocol": util.ProtocolSystemOne, "upstream_protocol": util.ProtocolSystemOne,
		"base_url": result.baseURL, "actual_model": result.actualModel}
	if result.responseBody != nil {
		out["raw_response"] = string(result.responseBody)
	}
	if result.responseHeader != nil {
		out["response_headers"] = flattenHeader(result.responseHeader)
	}
	if result.debug != nil {
		out["debug_data"] = result.debug
	}
	if result.usage != nil {
		out["usage"] = map[string]any{"input_tokens": result.usage.InputTokens, "output_tokens": result.usage.OutputTokens}
		if !result.costUnknown {
			row, _ := s.firstModelRow(cfg, modelName)
			out["cost_usd"] = computeRequestCostWithPrice(util.ResolveBillingModel(result.actualModel, modelName), "", row.entry.Pricing, result.usage)
		}
	}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["message"] = "API测试成功"
	}
	return out
}
