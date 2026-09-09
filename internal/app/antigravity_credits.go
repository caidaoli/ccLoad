package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

var errAntigravityCreditsUnavailable = errors.New("antigravity credits unavailable")

func antigravityCredentialAttempted(tried *map[string]bool, cfg *model.Config, credential *antigravityauth.Credential) bool {
	channel := fmt.Sprintf("channel:%d", cfg.ID)
	identity := credential.RefreshToken
	if credential.Email != "" {
		identity = credential.Email + "\x00" + credential.ProjectID
	}
	if (*tried)[channel] || (*tried)[identity] {
		return true
	}
	if *tried == nil {
		*tried = make(map[string]bool)
	}
	(*tried)[channel], (*tried)[identity] = true, true
	return false
}

func antigravityClaudeModel(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "claude-")
}

// Keep paid candidates after all ordinary candidates, including when the latter
// are empty. General cooldowns stay strict; standard quota has its own state.
func (s *Server) appendAntigravityCreditsCandidates(ctx context.Context, ordinary []*model.Config, modelName, clientProtocol string, body []byte) []*model.Config {
	if s.antigravityCredentials == nil || hasAntigravityWebSearchTool(body) {
		return ordinary
	}
	source, err := s.getEnabledChannelsSnapshotByModel(ctx, "*")
	if err != nil {
		log.Printf("[WARN] Antigravity credits candidates unavailable: %v", err)
		return ordinary
	}
	paid := make([]*model.Config, 0)
	for _, cfg := range source {
		if cfg == nil || !cfg.Enabled || !cfg.UsesAntigravityOAuth() || !s.configSupportsModelWithFuzzyMatch(cfg, modelName) {
			continue
		}
		if !antigravityClaudeModel(s.resolveFinalUpstreamModel(cfg, modelName, string(protocol.Gemini))) {
			continue
		}
		clone := cfg.Clone()
		clone.AntigravityCredits = true
		clone.CooldownFallback = false
		paid = append(paid, clone)
	}
	paid, err = s.filterCooldownChannelsStrict(ctx, paid, modelName, clientProtocol)
	if err != nil {
		log.Printf("[WARN] Antigravity credits cooldown check failed: %v", err)
		return ordinary
	}
	return append(ordinary, paid...)
}

func (m *antigravityCredentialManager) standardQuotaUntil(cfg *model.Config, modelName string) time.Time {
	if m == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return time.Time{}
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return time.Time{}
	}
	return credential.StandardQuota[modelName]
}

func (m *antigravityCredentialManager) updateQuotaState(ctx context.Context, cfg *model.Config, modelName string, until time.Time, insufficient bool) {
	if m == nil {
		return
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil || credential.AccessToken != cfg.AntigravityAccessToken {
		return
	}
	writeCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()
	now := time.Now()
	_, err = m.updateStoredMetadata(writeCtx, cfg.ID, credential, func(current *antigravityauth.Credential) {
		if insufficient {
			if current.Credits == nil {
				current.Credits = &antigravityauth.Credits{}
			}
			current.Credits.UnavailableAt = now.UnixMilli()
			return
		}
		for name, expiry := range current.StandardQuota {
			if !expiry.After(now) {
				delete(current.StandardQuota, name)
			}
		}
		if until.IsZero() {
			delete(current.StandardQuota, modelName)
			return
		}
		if current.StandardQuota == nil {
			current.StandardQuota = make(map[string]time.Time)
		}
		current.StandardQuota[modelName] = until
	})
	if err != nil {
		log.Printf("[WARN] Antigravity quota state persistence failed: channel_id=%d err=%v", cfg.ID, err)
	}
}

func (s *Server) prepareAntigravityCredits(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, credential *antigravityauth.Credential) (*antigravityauth.Credential, error) {
	if credential == nil {
		return nil, errAntigravityCreditsUnavailable
	}
	actualModel := s.resolveFinalUpstreamModel(cfg, reqCtx.originalModel, string(protocol.Gemini))
	if !antigravityClaudeModel(actualModel) || hasAntigravityWebSearchTool(reqCtx.body) || !credential.StandardQuota[actualModel].After(time.Now()) {
		return credential, errAntigravityCreditsUnavailable
	}
	if !credential.Credits.Fresh(time.Now()) {
		var err error
		credential, err = s.antigravityCredentials.completeStoredMetadata(ctx, cfg, credential, true)
		if err != nil {
			if oauthRefreshTokenRejected(err) {
				return credential, err
			}
			return credential, errAntigravityCreditsUnavailable
		}
	}
	if credential == nil || !credential.Credits.Fresh(time.Now()) || !credential.Credits.Available() || !credential.StandardQuota[actualModel].After(time.Now()) {
		return credential, errAntigravityCreditsUnavailable
	}
	eligible, err := s.filterCooldownChannelsStrict(ctx, []*model.Config{cfg}, reqCtx.originalModel, string(reqCtx.clientProtocol))
	if err != nil || len(eligible) == 0 {
		return credential, errAntigravityCreditsUnavailable
	}
	return credential, nil
}

// Only typed Google details authorize paid fallback. RetryInfo is not evidence
// of exhausted quota, regardless of how long its delay is.
func antigravityLimitDetails(body []byte) (reason string, delay time.Duration) {
	if gjson.GetBytes(body, "error.status").String() != "RESOURCE_EXHAUSTED" {
		return "", 0
	}
	for _, detail := range gjson.GetBytes(body, "error.details").Array() {
		switch detail.Get("@type").String() {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			value := detail.Get("reason").String()
			order := []string{"RATE_LIMIT_EXCEEDED", "QUOTA_EXHAUSTED", "INSUFFICIENT_G1_CREDITS_BALANCE"}
			if slices.Index(order, value) > slices.Index(order, reason) {
				reason = value
			}
		case "type.googleapis.com/google.rpc.RetryInfo":
			parsed, err := time.ParseDuration(detail.Get("retryDelay").String())
			if err == nil && parsed > 0 {
				delay = parsed
			}
		}
	}
	return reason, delay
}

func antigravityQuotaReset(res *fwResult, delay time.Duration) time.Time {
	if delay > 0 {
		return time.Now().Add(delay)
	}
	classification := util.ClassifyHTTPResponseWithMeta(res.Status, res.Header, res.Body)
	for _, until := range []time.Time{classification.ModelCooldownUntil, classification.KeyCooldownUntil, classification.ChannelCooldownUntil} {
		if until.After(time.Now()) {
			return until
		}
	}
	return time.Now().Add(time.Minute)
}

// Standard quota and credits balance errors never become generic channel/model
// cooldowns; those must remain independent blockers for both execution paths.
func (s *Server) handleAntigravityQuotaFailure(ctx context.Context, cfg *model.Config, modelName, selectedKey string, res *fwResult, duration float64, reqCtx *proxyRequestContext) (*proxyResult, bool) {
	if !cfg.UsesAntigravityOAuth() || res.Status != http.StatusTooManyRequests {
		return nil, false
	}
	reason, delay := antigravityLimitDetails(res.Body)
	if reason == "RATE_LIMIT_EXCEEDED" && delay > 0 {
		writeCtx, cancel := cooldownWriteContext(ctx)
		defer cancel()
		// The existing cooldown store has second precision. Round up, never release early.
		until := time.Now().Add(delay).Truncate(time.Second).Add(time.Second)
		if err := s.store.SetModelCooldown(writeCtx, cfg.ID, modelName, until); err != nil {
			log.Printf("[WARN] Antigravity rate cooldown persistence failed: channel_id=%d err=%v", cfg.ID, err)
		}
		s.invalidateChannelRelatedCache(cfg.ID)
	} else {
		if reason != "QUOTA_EXHAUSTED" && (!cfg.AntigravityCredits || reason != "INSUFFICIENT_G1_CREDITS_BALANCE") {
			return nil, false
		}
		if reason == "QUOTA_EXHAUSTED" && cfg.AntigravityCredits {
			return nil, false
		}
		s.antigravityCredentials.updateQuotaState(ctx, cfg, modelName, antigravityQuotaReset(res, delay), cfg.AntigravityCredits)
	}
	entry := buildProxyLogEntry(reqCtx, cfg, modelName, selectedKey, res.Status, time.Since(reqCtx.channelStartTime).Seconds(), res, "")
	s.AddLogAsync(entry)
	s.updateTokenStatsForProxy(reqCtx, cfg, false, duration, res, modelName)
	return &proxyResult{status: res.Status, body: res.Body, header: res.Header, channelID: &cfg.ID, duration: duration, nextAction: cooldown.ActionRetryChannel, proxyLogWritten: true}, true
}
