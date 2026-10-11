package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/gin-gonic/gin"
)

type routePreviewCandidate struct {
	ID                int64   `json:"id"`
	Name              string  `json:"name"`
	Priority          int     `json:"priority"`
	EffectivePriority float64 `json:"effective_priority"`
	PriorityGroup     float64 `json:"priority_group"`
	AvailableKeyCount int     `json:"available_key_count"`
	CooldownFallback  bool    `json:"cooldown_fallback"`
	SameTierRotation  bool    `json:"same_tier_rotation"`
}

type routePreviewExcluded struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Priority int        `json:"priority"`
	Reason   string     `json:"reason"`
	Until    *time.Time `json:"until,omitempty"`
}

type routePreviewResult struct {
	Model          string                  `json:"model"`
	ClientProtocol string                  `json:"client_protocol"`
	ModelDenied    bool                    `json:"model_denied"`
	Candidates     []routePreviewCandidate `json:"candidates"`
	Excluded       []routePreviewExcluded  `json:"excluded"`
	Limitations    []string                `json:"limitations"`
}

type routeRejectContextKey struct{}

// This request-local sink is deliberately absent on the real proxy hot path.
// One record per channel survives repeated exact/fuzzy/strict passes; final
// candidates remove their records, including the all-cooldown fallback.
type routeRejectSink struct {
	now              time.Time
	rejected         map[int64]routePreviewExcluded
	keys             map[int64][]*model.APIKey
	rowReasons       map[int64]string
	channelCooldowns map[int64]time.Time
	keyCooldowns     map[int64]map[int]time.Time
	modelCooldowns   map[int64]map[string]time.Time
	snapshotsLoaded  bool
}

func routeRejectSinkFromContext(ctx context.Context) *routeRejectSink {
	sink, _ := ctx.Value(routeRejectContextKey{}).(*routeRejectSink)
	return sink
}

func (sink *routeRejectSink) reject(cfg *model.Config, reason string, until time.Time) {
	if sink == nil || cfg == nil {
		return
	}
	entry := routePreviewExcluded{ID: cfg.ID, Name: cfg.Name, Priority: cfg.Priority, Reason: reason}
	if until.After(sink.now) {
		entry.Until = &until
	}
	sink.rejected[cfg.ID] = entry
}

func (sink *routeRejectSink) rejectRemoved(before, after []*model.Config, reason string) {
	if sink == nil {
		return
	}
	for _, cfg := range before {
		if !slices.ContainsFunc(after, func(candidate *model.Config) bool { return cfg.ID == candidate.ID }) {
			sink.reject(cfg, reason, time.Time{})
		}
	}
}

// HandleRoutePreview reports the ordinary generation candidate groups without
// forwarding or advancing any channel/model-row/Key rotation state.
func (s *Server) HandleRoutePreview(c *gin.Context) {
	var req struct {
		AuthTokenID    *int64 `json:"auth_token_id"`
		ClientProtocol string `json:"client_protocol"`
		Model          string `json:"model"`
		Multimodal     bool   `json:"multimodal"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	req.ClientProtocol = normalizeOptionalProtocol(req.ClientProtocol)
	req.Model = strings.TrimSpace(req.Model)
	if !protocol.IsValid(protocol.Protocol(req.ClientProtocol)) || req.Model == "" || model.RoutingModelName(req.Model) == "*" || (req.AuthTokenID != nil && *req.AuthTokenID <= 0) {
		RespondErrorMsg(c, http.StatusBadRequest, "valid client_protocol, model and auth_token_id are required")
		return
	}
	input := generationRouteInput{Model: req.Model, ClientProtocol: req.ClientProtocol, Multimodal: req.Multimodal}
	ctx := c.Request.Context()
	if req.AuthTokenID != nil {
		token, err := s.store.GetAuthToken(ctx, *req.AuthTokenID)
		if errors.Is(err, model.ErrAuthTokenNotFound) {
			RespondError(c, http.StatusNotFound, err)
			return
		}
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		if !token.IsActive || (token.ExpiresAt != nil && *token.ExpiresAt > 0 && time.Now().UnixMilli() > *token.ExpiresAt) {
			RespondErrorMsg(c, http.StatusBadRequest, "auth token is inactive or expired")
			return
		}
		input.TokenHash = token.Token
	}
	result := routePreviewResult{
		Model: model.RoutingModelName(s.generationRoutingModel(input)), ClientProtocol: req.ClientProtocol,
		Candidates: make([]routePreviewCandidate, 0), Excluded: make([]routePreviewExcluded, 0),
		Limitations: []string{"session_affinity", "execution_session_pinning", "channel_header", "antigravity_credits", "key_rotation", "model_row_rotation", "live_rpm_concurrency", "protocol_capabilities", "token_budget"},
	}
	sink := &routeRejectSink{now: time.Now(), rejected: make(map[int64]routePreviewExcluded), keys: make(map[int64][]*model.APIKey), rowReasons: make(map[int64]string)}
	ctx = context.WithValue(ctx, routeRejectContextKey{}, sink)
	candidates, err := s.resolveGenerationCandidates(ctx, input)
	if errors.Is(err, errGenerationModelDenied) {
		result.ModelDenied = true
		RespondJSON(c, http.StatusOK, result)
		return
	}
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	scores := s.scoreRoutePreviewCandidates(candidates)
	groups := make(map[int64]int, len(scores))
	healthEnabled := s.healthCache != nil && s.healthCache.Config().Enabled
	for _, item := range scores {
		bucket := int64(item.config.Priority)
		if healthEnabled {
			bucket = effPriorityBucket(item.effPriority)
		}
		groups[bucket]++
	}
	for _, item := range scores {
		cfg := item.config
		bucket := int64(cfg.Priority)
		if healthEnabled {
			bucket = effPriorityBucket(item.effPriority)
		}
		priorityGroup := float64(bucket)
		if healthEnabled {
			priorityGroup /= float64(effPriorityPrecision)
		}
		result.Candidates = append(result.Candidates, routePreviewCandidate{
			ID: cfg.ID, Name: cfg.Name, Priority: cfg.Priority, EffectivePriority: item.effPriority, PriorityGroup: priorityGroup,
			AvailableKeyCount: s.routePreviewAvailableKeys(cfg, sink.keys[cfg.ID], result.Model, req.ClientProtocol, sink.now),
			CooldownFallback:  cfg.CooldownFallback, SameTierRotation: groups[bucket] > 1,
		})
		if cfg.UsesOAuth() && sink.channelCooldowns[cfg.ID].After(sink.now) {
			result.Candidates[len(result.Candidates)-1].AvailableKeyCount = 0
		}
		delete(sink.rejected, cfg.ID)
	}
	for _, rejected := range sink.rejected {
		result.Excluded = append(result.Excluded, rejected)
	}
	slices.SortFunc(result.Excluded, func(a, b routePreviewExcluded) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	RespondJSON(c, http.StatusOK, result)
}

func (s *Server) scoreRoutePreviewCandidates(candidates []*model.Config) []channelWithScore {
	if s.healthCache != nil && s.healthCache.Config().Enabled {
		return s.scoreChannelsByHealth(candidates)
	}
	scored := make([]channelWithScore, len(candidates))
	for i, cfg := range candidates {
		scored[i] = channelWithScore{config: cfg, effPriority: float64(cfg.Priority)}
	}
	return scored
}

func (s *Server) routePreviewAvailableKeys(cfg *model.Config, keys []*model.APIKey, requested, client string, now time.Time) int {
	if cfg.UsesOAuth() {
		return 1
	}
	count := 0
	rows := s.enumerateModelRows(cfg, requested)
	for _, key := range keys {
		if key == nil || key.Disabled || key.IsCoolingDown(now) {
			continue
		}
		for _, row := range rows {
			if s.keyAllowsModelRow(cfg, key, row, client) {
				count++
				break
			}
		}
	}
	return count
}

// routePreviewRowRejectReason explains why configured rows produced no
// authorized combination, mirroring the gate order above: the first enabled,
// applicable row is blocked only by its Keys, so Key-level reasons win.
func (s *Server) routePreviewRowRejectReason(cfg *model.Config, rows []modelRoutingSelection, keys []*model.APIKey, now time.Time) string {
	reason := "model_disabled"
	for _, row := range rows {
		if row.entry.Disabled {
			continue
		}
		if cfg.AntigravityCredits && !s.modelRowApplicable(cfg, row, now) {
			reason = "credits_not_applicable"
			continue
		}
		if slices.ContainsFunc(keys, func(key *model.APIKey) bool { return key != nil && !key.Disabled }) {
			return "key_model_scope"
		}
		return "keys_disabled"
	}
	return reason
}

func sinkKeys(sink *routeRejectSink, channelID int64) ([]*model.APIKey, bool) {
	if sink == nil {
		return nil, false
	}
	keys, ok := sink.keys[channelID]
	return keys, ok
}
