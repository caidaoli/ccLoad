package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/sjson"
)

var errAnthropicResetIdentityChanged = errors.New("OAuth identity changed after reset")

var errAnthropicResetProfileScope = anthropicResetFailure(http.StatusBadRequest, "profile_scope_required", "user:profile scope required")

var anthropicResetKnownReasons = map[string]bool{
	"no_grant": true, "unknown_grant": true, "not_next_grant": true, "grant_id_required": true,
	"tenure": true, "other_experiment": true, "stamp_indeterminate": true, "reset_unconfirmed": true,
	"authorization_rejected": true,
}

// Only these upstream windows have a known local quota identity.
var anthropicResetWindowKeys = map[string]string{
	"five_hour":                  oauthcost.Key("", "five_hour"),
	"seven_day":                  oauthcost.Key("", "seven_day"),
	"seven_day_overage_included": oauthcost.Key("Claude Fable", "seven_day_fable"),
}

type anthropicResetOutcome struct {
	Outcome       string                 `json:"outcome"`
	Reason        string                 `json:"reason,omitempty"`
	Cleared       []string               `json:"cleared"`
	CooldownUntil *time.Time             `json:"cooldown_until,omitempty"`
	Credits       *anthropicResetCredits `json:"credits,omitempty"`
	Warnings      []string               `json:"warnings"`
	Usage         *oauthUsageSummary     `json:"usage,omitempty"`
}

type anthropicResetError struct {
	status  int
	code    string
	message string
}

func (e *anthropicResetError) Error() string { return e.message }

func anthropicResetFailure(status int, code, message string) error {
	return &anthropicResetError{status: status, code: code, message: message}
}

func anthropicResetAccount(credential *anthropicauth.Credential, organization string) string {
	return firstNonEmpty(credential.AccountUUID, strings.ToLower(credential.EmailAddress), organization)
}

// HandleRedeemAnthropicResetCredits consumes one explicitly confirmed native
// reset. Transport failures are results, never permission to resend a claim.
func (s *Server) HandleRedeemAnthropicResetCredits(c *gin.Context) {
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
	credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil || s.anthropicCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential unavailable")
		return
	}
	var result *anthropicResetOutcome
	if !anthropicResetHasProfileScope(credential.Scope) {
		err = errAnthropicResetProfileScope
	} else {
		ctx, cancel := context.WithTimeout(c.Request.Context(), oauthUsageTimeout)
		defer cancel()
		result, err = s.redeemAnthropicResetCredits(ctx, s.withOAuthBaseURLOverride(cfg))
	}
	if err != nil {
		// Every error response precedes the claim POST: no credit was consumed.
		var resetErr *anthropicResetError
		if !errors.As(err, &resetErr) {
			resetErr = &anthropicResetError{status: http.StatusBadGateway, code: "reset_prepare_failed", message: "Anthropic reset preparation failed"}
		}
		RespondErrorWithData(c, resetErr.status, resetErr.message, gin.H{"code": resetErr.code})
		return
	}
	RespondJSON(c, http.StatusOK, result)
}

func (s *Server) redeemAnthropicResetCredits(ctx context.Context, cfg *model.Config) (*anthropicResetOutcome, error) {
	credential, err := s.anthropicCredentials.credential(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	baseURL := anthropicauth.DefaultUpstreamURL
	if urls := cfg.GetURLs(); len(urls) > 0 {
		baseURL = urls[0]
	}
	client := s.getClientForChannel(cfg)
	organization, credential, err := s.anthropicResetOrganization(ctx, cfg, client, credential, baseURL)
	if err != nil {
		return nil, err
	}
	if _, loaded := s.anthropicQuotaResetInFlight.LoadOrStore(organization, struct{}{}); loaded {
		return nil, anthropicResetFailure(http.StatusConflict, "reset_busy", "another reset is in progress")
	}
	defer s.anthropicQuotaResetInFlight.Delete(organization)
	account := anthropicResetAccount(credential, organization)
	block, err := requestAnthropicResetBlock(ctx, client, credential, baseURL)
	if anthropicResetUnauthorized(err) {
		credential, err = s.anthropicCredentials.credentialAfterUnauthorized(ctx, cfg, credential.AccessToken)
		if err != nil {
			return nil, err
		}
		freshOrganization, queryErr := requestAnthropicResetOrganization(ctx, client, credential, baseURL)
		if queryErr != nil {
			return nil, queryErr
		}
		if freshOrganization != organization || anthropicResetAccount(credential, organization) != account {
			return nil, anthropicResetFailure(http.StatusConflict, "reset_identity_changed", "OAuth identity changed during reset preparation")
		}
		block, err = requestAnthropicResetBlock(ctx, client, credential, baseURL)
	}
	if err != nil {
		return nil, err
	}
	if !anthropicResetHasProfileScope(credential.Scope) {
		return nil, errAnthropicResetProfileScope
	}
	grantID := ""
	if block != nil {
		for _, grant := range block.Grants {
			if anthropicResetGrantRedeemable(block, grant, time.Now()) {
				grantID = grant.ID
				break
			}
		}
	}
	if grantID == "" {
		return nil, anthropicResetFailure(http.StatusConflict, "reset_not_available", "no reset is redeemable right now")
	}
	// Preparation may consume the whole request budget; a claim sent with the
	// leftover would almost surely end as "unknown" after spending the credit.
	if ctx.Err() != nil {
		return nil, anthropicResetFailure(http.StatusGatewayTimeout, "reset_prepare_timeout", "Anthropic reset preparation timed out")
	}
	// Taken before the POST: usage recorded during the round-trip still reflects
	// the old window but lands in the new one, over-counting rather than under.
	resetAt := time.Now().UTC()
	claimCtx, claimCancel := context.WithTimeout(context.WithoutCancel(ctx), oauthUsageTimeout)
	result := claimAnthropicReset(claimCtx, client, credential, baseURL, organization, grantID, uuid.NewString())
	claimCancel()
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthUsageTimeout)
	defer cancel()
	if result.Outcome == "reset" {
		// Persist confirmed window resets before refreshing upstream usage.
		identityChanged := false
		if err := s.resetAnthropicClearedWindows(postCtx, cfg.ID, account, organization, credential.OrgUUID, result.Cleared, resetAt); err != nil {
			identityChanged = errors.Is(err, errAnthropicResetIdentityChanged)
			result.Warnings = append(result.Warnings, "Claude quota was reset, but local cost usage synchronization failed")
		}
		if !identityChanged {
			if err := s.resetAllChannelCooldowns(postCtx, cfg.ID); err != nil {
				result.Warnings = append(result.Warnings, "Claude quota was reset, but local cooldown cleanup failed")
			}
			result.Usage, err = s.refreshOAuthUsage(postCtx, cfg.ID)
			if err != nil {
				result.Warnings = append(result.Warnings, "Claude quota was reset, but refreshed usage is unavailable")
			}
		}
	}
	if result.Outcome != "unknown" {
		if credits, queryErr := requestAnthropicResetCredits(postCtx, client, credential, baseURL); queryErr == nil {
			result.Credits = credits
		}
	}
	return result, nil
}

func anthropicResetUnauthorized(err error) bool {
	var statusErr *oauthUsageHTTPStatusError
	return errors.As(err, &statusErr) && statusErr.statusCode == http.StatusUnauthorized
}

func (s *Server) anthropicResetOrganization(ctx context.Context, cfg *model.Config, client *http.Client, credential *anthropicauth.Credential, baseURL string) (string, *anthropicauth.Credential, error) {
	organization, err := requestAnthropicResetOrganization(ctx, client, credential, baseURL)
	if anthropicResetUnauthorized(err) {
		credential, err = s.anthropicCredentials.credentialAfterUnauthorized(ctx, cfg, credential.AccessToken)
		if err != nil {
			return "", nil, err
		}
		organization, err = requestAnthropicResetOrganization(ctx, client, credential, baseURL)
	}
	return organization, credential, err
}

func requestAnthropicResetOrganization(ctx context.Context, client *http.Client, credential *anthropicauth.Credential, baseURL string) (string, error) {
	req, err := newAnthropicOAuthMetadataRequest(ctx, buildUpstreamURL(baseURL, "/api/oauth/profile", ""), credential.AccessToken, true)
	if err != nil {
		return "", err
	}
	setAnthropicResetHeaders(req)
	body, err := executeOAuthUsageRequest(client, req, "Anthropic profile")
	if err != nil {
		return "", err
	}
	var profile struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return "", errors.New("OAuth organization unavailable")
	}
	organization, err := uuid.Parse(profile.Organization.UUID)
	if err != nil || organization == uuid.Nil {
		return "", errors.New("OAuth organization unavailable")
	}
	return organization.String(), nil
}

func unknownAnthropicReset(reason string) *anthropicResetOutcome {
	return &anthropicResetOutcome{Outcome: "unknown", Reason: reason, Cleared: []string{}, Warnings: []string{}}
}

func claimAnthropicReset(ctx context.Context, client *http.Client, credential *anthropicauth.Credential, baseURL, organization, grantID, operation string) *anthropicResetOutcome {
	unknown := unknownAnthropicReset("claim_unconfirmed")
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": grantID, "request_id": operation})
	target := buildUpstreamURL(baseURL, "/api/organizations/"+organization+"/reset_rate_limits", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return unknown
	}
	// POST is never replayable: neither Go's transport nor uTLS fallback can
	// rewind it after an uncertain transport failure.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	setAnthropicResetHeaders(req)
	isolated := &http.Client{Transport: client.Transport, Timeout: client.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := isolated.Do(req)
	if err != nil {
		return unknown
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &anthropicResetOutcome{Outcome: "ineligible", Reason: "authorization_rejected", Cleared: []string{}, Warnings: []string{}}
	}
	if resp.StatusCode != http.StatusOK {
		return unknown
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthUsageResponseBytes+1))
	if err != nil || len(raw) > maxOAuthUsageResponseBytes {
		return unknown
	}
	var payload struct {
		Result        string     `json:"result"`
		Reason        string     `json:"reason"`
		Cleared       []string   `json:"cleared"`
		CooldownUntil *time.Time `json:"cooldown_until"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return unknown
	}
	if payload.Reason == "stamp_indeterminate" || payload.Reason == "reset_unconfirmed" {
		return unknown
	}
	result := &anthropicResetOutcome{Outcome: payload.Result, Cleared: []string{}, Warnings: []string{}, CooldownUntil: payload.CooldownUntil}
	if anthropicResetKnownReasons[payload.Reason] {
		result.Reason = payload.Reason
	}
	switch payload.Result {
	case "reset", "already_used", "not_limited", "cooldown", "ineligible":
		seen := make(map[string]bool)
		for _, window := range payload.Cleared {
			if _, known := anthropicResetWindowKeys[window]; known && !seen[window] {
				result.Cleared = append(result.Cleared, window)
				seen[window] = true
			}
		}
		return result
	case "unavailable":
		return unknownAnthropicReset("upstream_unavailable")
	default:
		return unknown
	}
}

// resetAnthropicClearedWindows moves only confirmed window counters. The global
// quota epoch and other windows must retain their earlier ledger interval.
func (s *Server) resetAnthropicClearedWindows(ctx context.Context, channelID int64, account, organization, credentialOrganization string, cleared []string, resetAt time.Time) error {
	keys := make([]string, 0, len(cleared))
	for _, window := range cleared {
		keys = append(keys, anthropicResetWindowKeys[window])
	}
	for attempt := 0; ; attempt++ {
		cfg, err := s.store.GetConfig(ctx, channelID)
		if err != nil {
			return err
		}
		if !cfg.UsesAnthropicOAuth() {
			return errAnthropicResetIdentityChanged
		}
		credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return err
		}
		if anthropicResetAccount(credential, organization) != account || credential.OrgUUID != credentialOrganization {
			return errAnthropicResetIdentityChanged
		}
		if len(cleared) == 0 {
			return nil
		}
		nextUsage := oauthcost.ResetWindows(oauthcost.EffectiveUsage(credential.QuotaCostUsage, credential.OAuthUsage), keys, resetAt)
		if err := oauthcost.Validate(nextUsage); err != nil {
			return err
		}
		costJSON, err := json.Marshal(nextUsage)
		if err != nil {
			return err
		}
		nextCredential, err := sjson.SetRawBytes([]byte(cfg.OAuthCredential), "quota_cost_usage", costJSON)
		if err != nil {
			return err
		}
		// Old passive headers would otherwise reintroduce the pre-reset reading.
		for _, window := range cleared {
			var passive *anthropicauth.PassiveUsageWindow
			if credential.PassiveUsage != nil {
				switch window {
				case "five_hour":
					passive = credential.PassiveUsage.FiveHour
				case "seven_day":
					passive = credential.PassiveUsage.SevenDay
				case "seven_day_overage_included":
					passive = credential.PassiveUsage.SevenDayOverageIncluded
				}
			}
			if passive != nil {
				sampledAt, _ := time.Parse(time.RFC3339Nano, passive.SampledAt)
				if !sampledAt.After(resetAt) {
					nextCredential, err = sjson.DeleteBytes(nextCredential, "passive_usage."+window)
					if err != nil {
						return err
					}
				}
			}
		}
		changed, err := s.store.CompareAndSwapOAuthCredential(ctx, channelID, model.AuthTypeAnthropicOAuth, cfg.OAuthCredential, string(nextCredential))
		if err != nil {
			return err
		}
		if changed {
			s.invalidateOAuthCredential(channelID, anthropicauth.ChannelType)
			return nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return err
		}
	}
}
