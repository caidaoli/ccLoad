package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"

	"github.com/gin-gonic/gin"
)

var anthropicResetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

type anthropicResetCredit struct {
	Label            string             `json:"label"`
	ResetsLeft       int                `json:"resets_left"`
	StartsAt         *time.Time         `json:"starts_at,omitempty"`
	ExpiresAt        *time.Time         `json:"expires_at,omitempty"`
	Clears           []string           `json:"clears"`
	PercentUsed      map[string]float64 `json:"percent_used"`
	Blocking         []string           `json:"blocking"`
	UseRequiresLimit bool               `json:"use_requires_limit"`
	Redeemable       bool               `json:"redeemable"`
}

type anthropicResetCredits struct {
	Eligible       bool                   `json:"eligible"`
	AvailableCount int                    `json:"available_count"`
	Credits        []anthropicResetCredit `json:"credits"`
	CooldownUntil  *time.Time             `json:"cooldown_until,omitempty"`
	WeeklyResetsAt *time.Time             `json:"weekly_resets_at,omitempty"`
	FetchedAt      time.Time              `json:"fetched_at"`
}

type anthropicResetGrant struct {
	ID               string             `json:"id"`
	Label            string             `json:"label"`
	ResetsLeft       int                `json:"resets_left"`
	StartsAt         *time.Time         `json:"starts_at"`
	EndsAt           *time.Time         `json:"ends_at"`
	Clears           []string           `json:"clears"`
	Paused           bool               `json:"paused"`
	UsableNow        bool               `json:"usable_now"`
	UseRequiresLimit *bool              `json:"use_requires_limit"`
	PercentUsed      map[string]float64 `json:"percent_used"`
	Blocking         []string           `json:"blocking"`
}

type anthropicResetBlock struct {
	Eligible       bool                  `json:"eligible"`
	AtLimit        bool                  `json:"at_limit"`
	Grants         []anthropicResetGrant `json:"grants"`
	NextGrantID    string                `json:"next_grant_id"`
	CooldownUntil  *time.Time            `json:"cooldown_until"`
	WeeklyResetsAt *time.Time            `json:"weekly_resets_at"`
}

func parseAnthropicResetCredits(body []byte, now time.Time) (*anthropicResetCredits, error) {
	invalid := errors.New("invalid Anthropic reset status")
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, invalid
	}
	if _, hasError := envelope["error"]; hasError {
		return nil, invalid
	}
	result := &anthropicResetCredits{Credits: []anthropicResetCredit{}, FetchedAt: now.UTC()}
	raw, present := envelope["cedar_ember"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return result, nil
	}
	var block anthropicResetBlock
	if err := json.Unmarshal(raw, &block); err != nil || block.Grants == nil {
		return nil, errors.New("invalid Anthropic reset grants")
	}
	result.Eligible = block.Eligible
	if block.CooldownUntil != nil && now.Before(*block.CooldownUntil) {
		result.CooldownUntil = block.CooldownUntil
	}
	result.WeeklyResetsAt = block.WeeklyResetsAt
	for _, grant := range block.Grants {
		if !anthropicResetGrantIDPattern.MatchString(grant.ID) || len(grant.Clears) == 0 || grant.ResetsLeft <= 0 || grant.Paused ||
			(grant.StartsAt != nil && now.Before(*grant.StartsAt)) || (grant.EndsAt != nil && !now.Before(*grant.EndsAt)) {
			continue
		}
		requiresLimit := grant.UseRequiresLimit == nil || *grant.UseRequiresLimit
		redeemable := block.Eligible && grant.UsableNow && grant.ID == block.NextGrantID &&
			(!requiresLimit || block.AtLimit) && len(grant.Blocking) == 0 &&
			(block.CooldownUntil == nil || !now.Before(*block.CooldownUntil))
		percentUsed := map[string]float64{}
		for name, value := range grant.PercentUsed {
			if validOAuthUsedPercent(value) {
				percentUsed[name] = value
			}
		}
		result.Credits = append(result.Credits, anthropicResetCredit{
			Label: grant.Label, ResetsLeft: grant.ResetsLeft, StartsAt: grant.StartsAt, ExpiresAt: grant.EndsAt,
			Clears: grant.Clears, PercentUsed: percentUsed, Blocking: grant.Blocking,
			UseRequiresLimit: requiresLimit, Redeemable: redeemable,
		})
		if redeemable {
			result.AvailableCount += grant.ResetsLeft
		}
	}
	return result, nil
}

func requestAnthropicResetCredits(ctx context.Context, client *http.Client, credential *anthropicauth.Credential, baseURL string) (*anthropicResetCredits, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("anthropic reset request is unavailable")
	}
	if baseURL == "" {
		baseURL = anthropicauth.DefaultUpstreamURL
	}
	targetURL := buildUpstreamURL(baseURL, "/api/oauth/usage", "") + "?cedar_ember=1&skip_spend=1"
	req, err := newAnthropicOAuthMetadataRequest(ctx, targetURL, credential.AccessToken, true)
	if err != nil {
		return nil, errors.New("anthropic reset request is unavailable")
	}
	req.Header.Set("x-app", "cli")
	req.Header.Set("User-Agent", "claude-cli/"+anthropicEffectiveCLIVersion()+" (external, cli)")
	body, err := executeOAuthUsageRequest(client, req, "Anthropic reset")
	if err != nil {
		return nil, err
	}
	return parseAnthropicResetCredits(body, time.Now())
}

// HandleAnthropicResetCredits queries native reset credits without redeeming them.
func (s *Server) HandleAnthropicResetCredits(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := ParseInt64Param(c, "id")
	if err != nil || id <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesAnthropicOAuth() {
		RespondErrorMsg(c, http.StatusBadRequest, "Claude OAuth channel required")
		return
	}
	stored, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential unavailable")
		return
	}
	if !anthropicResetHasProfileScope(stored.Scope) {
		RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
		return
	}
	if s.anthropicCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential manager unavailable")
		return
	}
	requestCtx, cancel := context.WithTimeout(c.Request.Context(), oauthUsageTimeout)
	defer cancel()
	cfg = s.withOAuthBaseURLOverride(cfg)
	credential, err := s.anthropicCredentials.credential(requestCtx, cfg, false)
	if err != nil {
		RespondError(c, http.StatusServiceUnavailable, oauthUsageCredentialRefreshError(err, "Anthropic credential refresh failed"))
		return
	}
	if !anthropicResetHasProfileScope(credential.Scope) {
		RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
		return
	}
	client := s.getClientForChannel(cfg)
	baseURL := anthropicauth.DefaultUpstreamURL
	if urls := cfg.GetURLs(); len(urls) > 0 {
		baseURL = urls[0]
	}
	for attempt := 0; attempt < 2; attempt++ {
		credits, queryErr := requestAnthropicResetCredits(requestCtx, client, credential, baseURL)
		if queryErr == nil {
			RespondJSON(c, http.StatusOK, credits)
			return
		}
		var statusErr *oauthUsageHTTPStatusError
		if attempt == 0 && errors.As(queryErr, &statusErr) && statusErr.statusCode == http.StatusUnauthorized {
			credential, err = s.anthropicCredentials.credentialAfterUnauthorized(requestCtx, cfg, credential.AccessToken)
			if err != nil {
				RespondError(c, http.StatusServiceUnavailable, oauthUsageCredentialRefreshError(err, "Anthropic credential refresh failed"))
				return
			}
			if !anthropicResetHasProfileScope(credential.Scope) {
				RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
				return
			}
			continue
		}
		RespondError(c, http.StatusBadGateway, queryErr)
		return
	}
}

func anthropicResetHasProfileScope(scope string) bool {
	for _, item := range strings.Fields(scope) {
		if item == "user:profile" {
			return true
		}
	}
	return false
}
