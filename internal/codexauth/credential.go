package codexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"ccLoad/internal/oauthcost"
)

const (
	// ChannelType is the CLIProxyAPI provider type stored in Codex credentials.
	ChannelType = "codex"
	// AuthModePersonalAccessToken identifies a static Codex access token.
	AuthModePersonalAccessToken = "personalAccessToken"
	personalAccessTokenPrefix   = "at-"
	maxCredentialSize           = 1 << 20
)

// ErrPersonalAccessTokenCannotRefresh marks a rejected static PAT as terminal.
var ErrPersonalAccessTokenCannotRefresh = errors.New("codex personal access token cannot be refreshed")

// Credential is the CLIProxyAPI-compatible Codex OAuth payload persisted as a
// private channel field. General channel responses omit it; the authenticated
// single-channel editor response may expose it for read-only inspection.
type Credential struct {
	IDToken        string           `json:"id_token,omitempty"`
	AccessToken    string           `json:"access_token"`
	RefreshToken   string           `json:"refresh_token,omitempty"`
	AuthMode       string           `json:"auth_mode,omitempty"`
	ChatGPTUserID  string           `json:"chatgpt_user_id,omitempty"`
	AccountID      string           `json:"account_id,omitempty"`
	LastRefresh    string           `json:"last_refresh,omitempty"`
	Email          string           `json:"email,omitempty"`
	Type           string           `json:"type"`
	Expired        string           `json:"expired,omitempty"`
	PlanType       string           `json:"plan_type,omitempty"`
	AccountFedRAMP bool             `json:"chatgpt_account_is_fedramp,omitempty"`
	PassiveUsage   *PassiveUsage    `json:"passive_usage,omitempty"`
	OAuthUsage     json.RawMessage  `json:"oauth_usage,omitempty"`
	QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage,omitempty"`
	QuotaOverdraft *QuotaOverdraft  `json:"quota_overdraft,omitempty"`
}

// QuotaOverdraft controls the one-shot usage_limit_reached replay and keeps
// its active quota window plus cumulative successful usage in the private
// credential payload.
type QuotaOverdraft struct {
	Enabled            bool  `json:"enabled"`
	ActiveUntil        int64 `json:"active_until,omitempty"`
	SuccessfulRequests int64 `json:"successful_requests,omitempty"`
	CostMicroUSD       int64 `json:"cost_microusd,omitempty"`
}

// PassiveUsage is the latest quota snapshot sampled from Codex upstream
// responses. It contains no OAuth secrets and is safe to project into admin APIs.
type PassiveUsage struct {
	Windows   []PassiveUsageWindow `json:"windows"`
	SampledAt string               `json:"sampled_at"`
}

// PassiveUsageWindow contains one account or product quota window.
type PassiveUsageWindow struct {
	Scope              string  `json:"scope"`
	LimitName          string  `json:"limit_name"`
	Kind               string  `json:"kind"`
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
	SampledAt          string  `json:"sampled_at"`
}

// IDTokenInfo is the readable Codex subscription metadata embedded in an ID
// token. The persisted credential keeps the original JWT string intact.
type IDTokenInfo struct {
	ChatGPTUserID                  string `json:"chatgpt_user_id,omitempty"`
	ChatGPTAccountID               string `json:"chatgpt_account_id,omitempty"`
	ChatGPTSubscriptionActiveStart any    `json:"chatgpt_subscription_active_start,omitempty"`
	ChatGPTSubscriptionActiveUntil any    `json:"chatgpt_subscription_active_until,omitempty"`
	PlanType                       string `json:"plan_type,omitempty"`
}

// ParseCredential validates imported CLIProxyAPI JSON and returns its canonical form.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("codex credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("codex credential exceeds %d bytes", maxCredentialSize)
	}
	var credential Credential
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode Codex credential: %w", err)
	}
	if decoder.Decode(&struct{}{}) == nil {
		return nil, errors.New("codex credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("codex credential is nil")
	}
	c.IDToken = strings.TrimSpace(c.IDToken)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.AuthMode = strings.TrimSpace(c.AuthMode)
	c.ChatGPTUserID = strings.TrimSpace(c.ChatGPTUserID)
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	c.Email = strings.TrimSpace(c.Email)
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.Expired = strings.TrimSpace(c.Expired)
	c.PlanType = strings.TrimSpace(c.PlanType)
	if c.PassiveUsage != nil {
		c.PassiveUsage.SampledAt = strings.TrimSpace(c.PassiveUsage.SampledAt)
		if _, err := time.Parse(time.RFC3339, c.PassiveUsage.SampledAt); err != nil {
			return errors.New("codex credential has invalid passive_usage.sampled_at")
		}
		for i := range c.PassiveUsage.Windows {
			window := &c.PassiveUsage.Windows[i]
			window.Scope = strings.ToLower(strings.TrimSpace(window.Scope))
			window.LimitName = strings.TrimSpace(window.LimitName)
			window.Kind = strings.TrimSpace(window.Kind)
			window.SampledAt = strings.TrimSpace(window.SampledAt)
			_, sampledAtErr := time.Parse(time.RFC3339, window.SampledAt)
			if window.Scope == "" || window.Kind == "" || sampledAtErr != nil ||
				math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) ||
				window.UsedPercent < 0 || window.UsedPercent > 100 || window.LimitWindowSeconds < 0 || window.ResetAt < 0 {
				return errors.New("codex credential has invalid passive_usage window")
			}
		}
	}
	if c.QuotaOverdraft != nil &&
		(c.QuotaOverdraft.ActiveUntil < 0 || c.QuotaOverdraft.SuccessfulRequests < 0 ||
			c.QuotaOverdraft.CostMicroUSD < 0) {
		return errors.New("codex credential has invalid quota_overdraft state")
	}
	if err := oauthcost.Validate(c.QuotaCostUsage); err != nil {
		return fmt.Errorf("codex credential has invalid quota_cost_usage: %w", err)
	}

	if c.Type == "" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported credential type %q", c.Type)
	}
	if c.AccessToken == "" {
		return errors.New("codex credential is missing access_token")
	}
	if c.AuthMode != "" && c.AuthMode != AuthModePersonalAccessToken {
		return fmt.Errorf("unsupported Codex auth_mode %q", c.AuthMode)
	}
	if c.IsPersonalAccessToken() {
		if !strings.HasPrefix(c.AccessToken, personalAccessTokenPrefix) {
			return errors.New("codex personal access token must start with at-")
		}
		c.IDToken = ""
		c.RefreshToken = ""
		c.LastRefresh = ""
		c.Expired = ""
		return nil
	}
	if c.RefreshToken == "" {
		return errors.New("codex credential is missing refresh_token")
	}
	if _, err := c.Expiry(); err != nil {
		return err
	}
	if c.IDToken != "" {
		if claims, err := parseIDToken(c.IDToken); err == nil {
			if c.ChatGPTUserID == "" {
				c.ChatGPTUserID = strings.TrimSpace(claims.Auth.ChatGPTUserID)
			}
			if c.AccountID == "" {
				c.AccountID = strings.TrimSpace(claims.Auth.ChatGPTAccountID)
			}
			if c.Email == "" {
				c.Email = strings.TrimSpace(claims.Email)
			}
			if c.PlanType == "" {
				c.PlanType = strings.TrimSpace(claims.Auth.ChatGPTPlanType)
			}
		}
	}
	return nil
}

// IsPersonalAccessToken reports whether this credential uses a static Codex
// access token instead of the refreshable browser OAuth lifecycle.
func (c *Credential) IsPersonalAccessToken() bool {
	return c != nil && strings.TrimSpace(c.AuthMode) == AuthModePersonalAccessToken
}

// Expiry returns the absolute credential expiration time.
func (c *Credential) Expiry() (time.Time, error) {
	if c == nil || strings.TrimSpace(c.Expired) == "" {
		return time.Time{}, errors.New("codex credential is missing expired")
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(c.Expired))
	if err != nil {
		return time.Time{}, fmt.Errorf("codex credential has invalid expired: %w", err)
	}
	return expiresAt, nil
}

// DecodedIDToken returns readable metadata without changing the raw credential.
func (c *Credential) DecodedIDToken() *IDTokenInfo {
	if c == nil || strings.TrimSpace(c.IDToken) == "" {
		return nil
	}
	claims, err := parseIDToken(c.IDToken)
	if err != nil {
		return nil
	}
	info := &IDTokenInfo{
		ChatGPTUserID:                  strings.TrimSpace(claims.Auth.ChatGPTUserID),
		ChatGPTAccountID:               strings.TrimSpace(claims.Auth.ChatGPTAccountID),
		ChatGPTSubscriptionActiveStart: claims.Auth.ChatGPTSubscriptionActiveStart,
		ChatGPTSubscriptionActiveUntil: claims.Auth.ChatGPTSubscriptionActiveUntil,
		PlanType:                       strings.TrimSpace(claims.Auth.ChatGPTPlanType),
	}
	if info.ChatGPTUserID == "" && info.ChatGPTAccountID == "" && info.ChatGPTSubscriptionActiveStart == nil &&
		info.ChatGPTSubscriptionActiveUntil == nil && info.PlanType == "" {
		return nil
	}
	return info
}

// SubscriptionActiveUntil returns the Codex subscription end time embedded in
// the ID token. It is intentionally derived from the persisted token instead of
// duplicating OAuth identity metadata in the channel record.
func (c *Credential) SubscriptionActiveUntil() (time.Time, bool) {
	info := c.DecodedIDToken()
	if info == nil {
		return time.Time{}, false
	}
	raw, ok := info.ChatGPTSubscriptionActiveUntil.(string)
	if !ok {
		return time.Time{}, false
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return until.UTC(), true
}

// NeedsRefresh reports whether the access token is inside the refresh window.
func (c *Credential) NeedsRefresh(now time.Time, lead time.Duration) (bool, error) {
	if c.IsPersonalAccessToken() {
		return false, nil
	}
	expiresAt, err := c.Expiry()
	if err != nil {
		return false, err
	}
	return !expiresAt.After(now.Add(lead)), nil
}

// MergeRefresh preserves identity and a rotated refresh token when OpenAI omits
// those fields from a refresh response.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("codex refresh credential is nil")
	}
	if c.IsPersonalAccessToken() {
		return nil, ErrPersonalAccessTokenCannotRefresh
	}
	merged := *refreshed
	if merged.RefreshToken == "" {
		merged.RefreshToken = c.RefreshToken
	}
	if merged.IDToken == "" {
		merged.IDToken = c.IDToken
	}
	if merged.ChatGPTUserID == "" {
		merged.ChatGPTUserID = c.ChatGPTUserID
	}
	if merged.AccountID == "" {
		merged.AccountID = c.AccountID
	}
	if merged.Email == "" {
		merged.Email = c.Email
	}
	if merged.PlanType == "" {
		merged.PlanType = c.PlanType
	}
	if !merged.AccountFedRAMP {
		merged.AccountFedRAMP = c.AccountFedRAMP
	}
	if merged.PassiveUsage == nil {
		merged.PassiveUsage = ClonePassiveUsage(c.PassiveUsage)
	}
	merged.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
	merged.QuotaCostUsage = oauthcost.Clone(c.QuotaCostUsage)
	merged.QuotaOverdraft = CloneQuotaOverdraft(c.QuotaOverdraft)
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	return &merged, nil
}

// CloneQuotaOverdraft returns an independent settings and statistics snapshot.
func CloneQuotaOverdraft(overdraft *QuotaOverdraft) *QuotaOverdraft {
	if overdraft == nil {
		return nil
	}
	clone := *overdraft
	return &clone
}

// ClonePassiveUsage returns an independent quota snapshot.
func ClonePassiveUsage(usage *PassiveUsage) *PassiveUsage {
	if usage == nil {
		return nil
	}
	clone := *usage
	clone.Windows = append([]PassiveUsageWindow(nil), usage.Windows...)
	return &clone
}

// JSON returns the canonical private database payload.
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode Codex credential: %w", err)
	}
	return string(raw), nil
}
