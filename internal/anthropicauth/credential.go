package anthropicauth

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/oauthcost"
)

const (
	// ChannelType is the provider type stored in Anthropic OAuth credentials.
	ChannelType = "anthropic"
	// CredentialRefreshLead is how early an expiring access token is refreshed.
	CredentialRefreshLead = 5 * time.Minute
	maxCredentialSize     = 1 << 20
)

// Credential is the private Anthropic OAuth payload persisted on a channel.
// Expired is canonical RFC3339; ExpiresAt is accepted only for sub2api imports.
type Credential struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	ExpiresAt    any    `json:"expires_at,omitempty"`
	Expired      string `json:"expired"`
	LastRefresh  string `json:"last_refresh,omitempty"`
	Scope        string `json:"scope,omitempty"`
	OrgUUID      string `json:"org_uuid,omitempty"`
	AccountUUID  string `json:"account_uuid,omitempty"`
	EmailAddress string `json:"email_address,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	PlanType     string `json:"plan_type,omitempty"`
	// ClaudeCodeTrialEndsAt is a trial boundary reported by /api/oauth/profile.
	// It is not a general subscription expiration time.
	ClaudeCodeTrialEndsAt string           `json:"claude_code_trial_ends_at,omitempty"`
	PassiveUsage          *PassiveUsage    `json:"passive_usage,omitempty"`
	OAuthUsage            json.RawMessage  `json:"oauth_usage,omitempty"`
	QuotaCostUsage        *oauthcost.Usage `json:"quota_cost_usage,omitempty"`
}

// PassiveUsage is the latest quota snapshot sampled from Anthropic model
// response headers. Utilization is stored as Anthropic's native 0..1 fraction.
type PassiveUsage struct {
	FiveHour                *PassiveUsageWindow `json:"five_hour,omitempty"`
	SevenDay                *PassiveUsageWindow `json:"seven_day,omitempty"`
	SevenDayOverageIncluded *PassiveUsageWindow `json:"seven_day_overage_included,omitempty"`
	SampledAt               string              `json:"sampled_at,omitempty"`
}

// PassiveUsageWindow contains one rate-limit window sampled from response headers.
type PassiveUsageWindow struct {
	Utilization      *float64 `json:"utilization,omitempty"`
	ResetAt          *int64   `json:"reset_at,omitempty"`
	SampledAt        string   `json:"sampled_at,omitempty"`
	UtilizationStale bool     `json:"utilization_stale,omitempty"`
}

// ParseCredential accepts both ccLoad's canonical shape and sub2api's mixed
// number/string expiry fields, then returns one normalized credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("anthropic credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("anthropic credential exceeds %d bytes", maxCredentialSize)
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("decode Anthropic credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("anthropic credential contains trailing JSON")
	}
	var credential Credential
	var emailAlias string
	if err := json.Unmarshal(raw, &struct {
		Type           *string           `json:"type"`
		AccessToken    *string           `json:"access_token"`
		RefreshToken   *string           `json:"refresh_token"`
		TokenType      *string           `json:"token_type"`
		Expired        *string           `json:"expired"`
		LastRefresh    *string           `json:"last_refresh"`
		Scope          *string           `json:"scope"`
		OrgUUID        *string           `json:"org_uuid"`
		AccountUUID    *string           `json:"account_uuid"`
		EmailAddress   *string           `json:"email_address"`
		Email          *string           `json:"email"`
		DeviceID       *string           `json:"device_id"`
		PlanType       *string           `json:"plan_type"`
		TrialEndsAt    *string           `json:"claude_code_trial_ends_at"`
		PassiveUsage   **PassiveUsage    `json:"passive_usage"`
		OAuthUsage     *json.RawMessage  `json:"oauth_usage"`
		QuotaCostUsage **oauthcost.Usage `json:"quota_cost_usage"`
	}{
		Type: &credential.Type, AccessToken: &credential.AccessToken,
		RefreshToken: &credential.RefreshToken, TokenType: &credential.TokenType,
		Expired: &credential.Expired, LastRefresh: &credential.LastRefresh,
		Scope: &credential.Scope, OrgUUID: &credential.OrgUUID,
		AccountUUID: &credential.AccountUUID, EmailAddress: &credential.EmailAddress,
		Email:    &emailAlias,
		DeviceID: &credential.DeviceID, PlanType: &credential.PlanType,
		TrialEndsAt: &credential.ClaudeCodeTrialEndsAt, PassiveUsage: &credential.PassiveUsage,
		OAuthUsage: &credential.OAuthUsage, QuotaCostUsage: &credential.QuotaCostUsage,
	}); err != nil {
		return nil, fmt.Errorf("decode Anthropic credential fields: %w", err)
	}
	if strings.TrimSpace(credential.EmailAddress) == "" {
		credential.EmailAddress = emailAlias
	}
	if rawValue := fields["expires_in"]; len(rawValue) > 0 && string(rawValue) != "null" {
		value, err := parseInt64(rawValue)
		if err != nil {
			return nil, errors.New("anthropic credential has invalid expires_in")
		}
		credential.ExpiresIn = value
	}
	if rawValue := fields["expires_at"]; len(rawValue) > 0 && string(rawValue) != "null" {
		var value any
		valueDecoder := json.NewDecoder(strings.NewReader(string(rawValue)))
		valueDecoder.UseNumber()
		if err := valueDecoder.Decode(&value); err != nil {
			return nil, errors.New("anthropic credential has invalid expires_at")
		}
		credential.ExpiresAt = value
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

func parseInt64(raw json.RawMessage) (int64, error) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strconv.ParseInt(value, 10, 64)
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("anthropic credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.Type == "" || c.Type == "claude" || c.Type == "oauth" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported Anthropic credential type %q", c.Type)
	}
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.TokenType = strings.TrimSpace(c.TokenType)
	c.Expired = strings.TrimSpace(c.Expired)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	c.Scope = strings.TrimSpace(c.Scope)
	c.OrgUUID = strings.TrimSpace(c.OrgUUID)
	c.AccountUUID = strings.TrimSpace(c.AccountUUID)
	c.EmailAddress = strings.TrimSpace(c.EmailAddress)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	identity := c.AccountUUID
	if identity == "" {
		identity = strings.ToLower(c.EmailAddress)
	}
	if c.DeviceID == "" && identity != "" {
		digest := sha256.Sum256([]byte("ccLoad:anthropic-device:" + identity))
		c.DeviceID = fmt.Sprintf("%x", digest[:])
	}
	c.PlanType = strings.TrimSpace(c.PlanType)
	c.ClaudeCodeTrialEndsAt = strings.TrimSpace(c.ClaudeCodeTrialEndsAt)
	if c.ClaudeCodeTrialEndsAt != "" {
		trialEndsAt, err := time.Parse(time.RFC3339, c.ClaudeCodeTrialEndsAt)
		if err != nil {
			return errors.New("anthropic credential has invalid claude_code_trial_ends_at")
		}
		c.ClaudeCodeTrialEndsAt = trialEndsAt.UTC().Format(time.RFC3339)
	}
	if c.PassiveUsage != nil {
		c.PassiveUsage.SampledAt = strings.TrimSpace(c.PassiveUsage.SampledAt)
		if c.PassiveUsage.SampledAt != "" {
			sampledAt, err := time.Parse(time.RFC3339, c.PassiveUsage.SampledAt)
			if err != nil {
				return errors.New("anthropic credential has invalid passive_usage.sampled_at")
			}
			c.PassiveUsage.SampledAt = sampledAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if err := oauthcost.Validate(c.QuotaCostUsage); err != nil {
		return fmt.Errorf("anthropic credential has invalid quota_cost_usage: %w", err)
	}
	if c.AccessToken == "" {
		return errors.New("anthropic credential is missing access_token")
	}
	if c.RefreshToken == "" {
		return errors.New("anthropic credential is missing refresh_token")
	}
	expires, err := c.resolveExpiry()
	if err != nil {
		return err
	}
	c.Expired = expires.UTC().Format(time.RFC3339)
	c.ExpiresAt = nil
	return nil
}

func (c *Credential) resolveExpiry() (time.Time, error) {
	if c.Expired != "" {
		if parsed, err := time.Parse(time.RFC3339, c.Expired); err == nil {
			return parsed, nil
		}
		return time.Time{}, errors.New("anthropic credential has invalid expired")
	}
	if c.ExpiresAt != nil {
		if parsed, err := parseTimeValue(c.ExpiresAt); err == nil {
			return parsed, nil
		}
		return time.Time{}, errors.New("anthropic credential has invalid expires_at")
	}
	if c.LastRefresh != "" && c.ExpiresIn > 0 {
		last, err := time.Parse(time.RFC3339, c.LastRefresh)
		if err != nil {
			return time.Time{}, errors.New("anthropic credential has invalid last_refresh")
		}
		return last.Add(time.Duration(c.ExpiresIn) * time.Second), nil
	}
	return time.Time{}, errors.New("anthropic credential is missing a valid expiry")
}

func parseTimeValue(value any) (time.Time, error) {
	var raw string
	switch typed := value.(type) {
	case string:
		raw = strings.TrimSpace(typed)
	case json.Number:
		raw = typed.String()
	case float64:
		raw = strconv.FormatFloat(typed, 'f', -1, 64)
	case int64:
		raw = strconv.FormatInt(typed, 10)
	default:
		return time.Time{}, errors.New("unsupported time value")
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("invalid time value")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// Expiry returns the credential's absolute expiration time.
func (c *Credential) Expiry() (time.Time, error) {
	if c == nil {
		return time.Time{}, errors.New("anthropic credential is nil")
	}
	return c.resolveExpiry()
}

// NeedsRefresh reports whether the credential expires inside lead.
func (c *Credential) NeedsRefresh(now time.Time, lead time.Duration) (bool, error) {
	expires, err := c.Expiry()
	if err != nil {
		return false, err
	}
	return !expires.After(now.Add(lead)), nil
}

// MergeRefresh preserves identity metadata and the previous refresh token only
// when Anthropic legitimately omits them. A rotated refresh token always wins.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("anthropic refresh credential is nil")
	}
	merged := *refreshed
	preserve := func(target *string, old string) {
		if strings.TrimSpace(*target) == "" {
			*target = old
		}
	}
	preserve(&merged.RefreshToken, c.RefreshToken)
	preserve(&merged.TokenType, c.TokenType)
	preserve(&merged.Scope, c.Scope)
	preserve(&merged.OrgUUID, c.OrgUUID)
	preserve(&merged.AccountUUID, c.AccountUUID)
	preserve(&merged.EmailAddress, c.EmailAddress)
	preserve(&merged.DeviceID, c.DeviceID)
	preserve(&merged.PlanType, c.PlanType)
	preserve(&merged.ClaudeCodeTrialEndsAt, c.ClaudeCodeTrialEndsAt)
	if merged.PassiveUsage == nil {
		merged.PassiveUsage = ClonePassiveUsage(c.PassiveUsage)
	}
	merged.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
	merged.QuotaCostUsage = oauthcost.Clone(c.QuotaCostUsage)
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	return &merged, nil
}

// ClonePassiveUsage returns an independent quota snapshot.
func ClonePassiveUsage(usage *PassiveUsage) *PassiveUsage {
	if usage == nil {
		return nil
	}
	clone := *usage
	clone.FiveHour = clonePassiveUsageWindow(usage.FiveHour)
	clone.SevenDay = clonePassiveUsageWindow(usage.SevenDay)
	clone.SevenDayOverageIncluded = clonePassiveUsageWindow(usage.SevenDayOverageIncluded)
	return &clone
}

func clonePassiveUsageWindow(window *PassiveUsageWindow) *PassiveUsageWindow {
	if window == nil {
		return nil
	}
	clone := *window
	if window.Utilization != nil {
		value := *window.Utilization
		clone.Utilization = &value
	}
	if window.ResetAt != nil {
		value := *window.ResetAt
		clone.ResetAt = &value
	}
	return &clone
}

// JSON returns the canonical private database payload.
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode Anthropic credential: %w", err)
	}
	return string(raw), nil
}
