package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
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
	reasoningMinConfidence      = 0.3
	reasoningConfidentDowngrade = 0.5 ///< 报置信度的判断模型降级免确认的下限：离线 D3 低档建议在它以上判低 12%、以下 44%
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
	levels       []string ///< 本轮候选可判断等级；实际模型未收录时也按它裁剪
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

/**
 * @brief 按会话当前等级接受一次有效建议
 * @param effort 判断模型建议的等级
 * @param confidence 判断模型对 effort 的置信度；不报置信度的模型为 nil
 * @note 升级立即生效；降级在不报置信度或置信度达到 reasoningConfidentDowngrade 时立即生效，
 *       否则需连续两次低于当前等级的建议，取两次中较高的一档
 * @details 2026-11-19 生产盲标：被确认压住的降级 21 条中 8 条因此偏高、仅 2 条被救回
 */
func (t *reasoningTurn) acceptSuggestion(effort string, confidence *float64) {
	t.metadata.SuggestedEffort = effort
	t.metadata.Status = "decided"
	t.force = false
	t.toolTurns = 0
	switch {
	case t.effort == "" || reasoningRank(effort) > reasoningRank(t.effort):
		t.effort, t.pending, t.pendingCount = effort, "", 0
	case effort == t.effort:
		t.pending, t.pendingCount = "", 0
	case confidence == nil || *confidence >= reasoningConfidentDowngrade:
		t.effort, t.pending, t.pendingCount = effort, "", 0
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

/**
 * @brief 在 HTTP 或 WebSocket 业务请求发送时按实际正文提交思考等级。
 * @param ctx 携带本次代理请求的上下文，已取消时跳过。
 * @param body 上游请求正文；无需比对自动等级时可为空。
 */
func reasoningBusinessSending(ctx context.Context, body []byte) {
	r, _ := ctx.Value(reasoningRequestContextKey{}).(*proxyRequestContext)
	if r == nil || r.reasoning == nil || ctx.Err() != nil {
		return
	}
	actual := ""
	if r.reasoningExpected != "" {
		actual = extractThinkingEffortFromJSON(body)
	}
	r.commitSentReasoning(actual, "channel_rules_override")
}

/**
 * @brief 依据本次业务请求实际发送的等级更新日志和后续轮次的判断状态。
 * @param actual 实际发送的等级；空字符串表示未发送显式等级。
 * @param mismatchReason 实际等级与预期不一致时记录的原因。
 * @note 仅在已有判断且业务请求已发送时调用。
 */
func (r *proxyRequestContext) commitSentReasoning(actual, mismatchReason string) {
	t := r.reasoning
	applied := false
	r.reasoningApplied = ""
	if r.reasoningExpected != "" {
		r.thinkingEffort, r.reasoningApplied = actual, actual
		applied = actual == r.reasoningExpected
		if r.observer != nil && r.observer.OnThinkingEffort != nil {
			r.observer.OnThinkingEffort(actual, t.metadata.ClientEffort)
		}
		if !applied {
			r.reasoningApplyReason = mismatchReason
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

// reasoningTarget reads the live policy: saving the setting takes effect on the
// next request. A Server built without one (zero value) has the feature off.
func (s *Server) reasoningTarget(r *proxyRequestContext) bool {
	cfg := s.reasoningAuto.Load()
	return cfg != nil && cfg.Enabled && containsExact(cfg.BusinessModels, r.originalModel)
}

/**
 * @brief 为一轮业务请求复用上次等级或执行新的动态判断。
 * @param ctx 请求上下文，取消时停止等待判断。
 * @param r 当前请求及思考状态。
 * @param channels 已完成请求协议、权限与客户端指定渠道过滤的候选。
 * @param transcript 用于判断的完整会话正文。
 * @param original 仅含本轮输入的原请求正文。
 * @param explicit 客户端是否通过明确的思考后缀选择等级或预算。
 */
func (s *Server) prepareReasoningTurn(ctx context.Context, r *proxyRequestContext, channels []*model.Config, transcript, original []byte, explicit bool) {
	if !s.reasoningTarget(r) {
		return
	}
	// A model without two known levels has nothing to choose between; the
	// result could not be written, so the judge is not called at all.
	levels := s.reasoningDecisionLevels(r.originalModel, channels, r.clientProtocol)
	if levels == nil {
		r.reasoning = &reasoningTurn{state: &reasoningState{}, metadata: model.ReasoningDecision{Status: "skipped", Reason: "unknown_model_capability"}}
		return
	}
	if r.clientProtocol == protocol.Codex && responsesCompactionRequested(r.requestPath, original) {
		r.reasoning = &reasoningTurn{state: &reasoningState{}, metadata: model.ReasoningDecision{Status: "skipped", Reason: "compaction"}}
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
	// Claude Code's background forks replay the session with its parameters to
	// share the prompt cache. They read the session's level without judging and
	// neither move it nor count toward its downgrade.
	if r.clientProtocol == protocol.Anthropic && claudeCodeAuxiliaryRequest(original) {
		t := &reasoningTurn{state: &reasoningState{}, levels: levels, metadata: model.ReasoningDecision{
			Status: "skipped", Trigger: "auxiliary", ClientEffort: r.thinkingEffort}}
		if explicit {
			t.metadata.Reason = "client_explicit"
		} else {
			state := s.reasoningStateFor(r)
			state.mu.Lock()
			t.effort = state.effort
			state.mu.Unlock()
			t.metadata.PreviousEffort = t.effort
			if t.effort != "" {
				t.metadata.Status = "reused"
			}
		}
		r.reasoning = t
		return
	}
	state := s.reasoningStateFor(r)
	state.mu.Lock()
	state.version++
	t := &reasoningTurn{state: state, version: state.version, effort: state.effort,
		pending: state.pending, pendingCount: state.pendingCount, toolTurns: state.toolTurns, force: state.force, levels: levels}
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
	result, err := s.executeReasoningDecision(ctx, projection.text, levels)
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
	t.acceptSuggestion(result.effort, result.confidence)
}

func containsExact(values []string, value string) bool {
	return slices.Contains(values, value)
}

func reasoningDecisionContext(ctx context.Context) context.Context {
	// Keep cancellation/deadlines, but the administrator's decision channel has
	// no client token channel restrictions and cannot commit the business turn.
	ctx = context.WithValue(ctx, channelRestrictionTokenContextKey{}, (*channelRestrictionState)(nil))
	return context.WithValue(ctx, reasoningRequestContextKey{}, (*proxyRequestContext)(nil))
}

/**
 * @brief 管理测试调用 System One 并返回与普通渠道测试一致的用量和费用字段
 * @param ctx 管理测试上下文
 * @param cfg 固定了待测模型行的渠道配置
 * @param apiKey 待测凭据
 * @param modelName 待测逻辑模型名
 * @param state 判断文本
 * @return 测试结果；失败原因包含在结果字段中
 */
func (s *Server) testSystemOneChannel(ctx context.Context, cfg *model.Config, apiKey, modelName, state string) (out map[string]any) {
	start := time.Now()
	urls := orderChannelAttemptURLs(s.urlSelector, cfg, cfg.GetURLs())
	if len(urls) == 0 {
		return map[string]any{"success": false, "error": "reasoning decision URLs unavailable"}
	}
	row, _ := s.firstModelRow(cfg, modelName)
	decision := s.reasoningAuto.Load()
	result, err := s.callSystemOne(ctx, cfg, apiKey, urls[0].url, row, state, reasoningDefaultLevels, decision.PromptTemplate, decision.CustomPrompt)
	defer func() {
		if result.upstreamStatus != 0 && result.upstreamStatus != out["status_code"] {
			out["upstream_status_code"] = result.upstreamStatus
		}
		if result.responseBody != nil {
			out["raw_response"] = string(result.responseBody)
		}
		if result.responseHeader != nil {
			out["response_headers"] = flattenHeader(result.responseHeader)
		}
		if result.debug != nil {
			out["debug_data"] = result.debug
		}
	}()
	if errors.Is(err, ErrChannelRPMExceeded) {
		return channelRPMExceededTestResult(start, channelRPMRetryAfter(err))
	}
	if errors.Is(err, ErrChannelConcurrencyExceeded) {
		return channelConcurrencyExceededTestResult(start, err)
	}
	if result.networkFailure {
		result.status, _, _ = util.ClassifyError(err)
	}
	result.costUnknown = s.reasoningCostUnknown(row, result)
	out = map[string]any{"success": err == nil, "status_code": result.status, "duration_ms": result.duration.Milliseconds(),
		"effort": result.effort, "cost_unknown": result.costUnknown, "client_protocol": util.ProtocolSystemOne, "upstream_protocol": util.ProtocolSystemOne,
		"base_url": result.baseURL, "actual_model": result.actualModel}
	if result.configuredDetection != nil {
		out["api_error"] = map[string]any{"message": result.configuredDetection.Message}
	}
	if result.reason == "invalid_response" {
		out["cooldown_action"] = "invalid_response_no_cooldown"
	} else if result.networkFailure {
		out["cooldown_action"] = "network_error_no_cooldown"
	}
	if result.usage != nil {
		out["usage"] = map[string]any{
			"input_tokens": result.usage.InputTokens, "output_tokens": result.usage.OutputTokens,
			"cache_read_input_tokens":     result.usage.CacheReadInputTokens,
			"cache_creation_input_tokens": result.usage.CacheCreationInputTokens,
			"cache_5m_input_tokens":       result.usage.Cache5mInputTokens,
			"cache_1h_input_tokens":       result.usage.Cache1hInputTokens,
			"cache_30m_input_tokens":      result.usage.Cache30mInputTokens,
			"cache_12h_input_tokens":      result.usage.Cache12hInputTokens,
		}
		if !result.costUnknown {
			billingModel := util.ResolveBillingModel(result.actualModel, modelName)
			out["cost_usd"] = computeRequestCostWithPrice(billingModel, "", util.DecisionModelPrice(billingModel, row.entry.Pricing), result.usage)
		}
	}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["message"] = "API测试成功"
	}
	return out
}
