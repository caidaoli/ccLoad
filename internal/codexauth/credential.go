package codexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// ChannelType is the CLIProxyAPI provider type stored in Codex credentials.
	ChannelType       = "codex"
	maxCredentialSize = 1 << 20
)

// Credential is the CLIProxyAPI-compatible Codex OAuth payload persisted as a
// private channel field. General channel responses omit it; the authenticated
// single-channel editor response may expose it for read-only inspection.
type Credential struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
	Email        string `json:"email,omitempty"`
	Type         string `json:"type"`
	Expired      string `json:"expired"`
	PlanType     string `json:"plan_type,omitempty"`
}

// IDTokenInfo is the readable Codex subscription metadata embedded in an ID
// token. The persisted credential keeps the original JWT string intact.
type IDTokenInfo struct {
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
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	c.Email = strings.TrimSpace(c.Email)
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.Expired = strings.TrimSpace(c.Expired)
	c.PlanType = strings.TrimSpace(c.PlanType)

	if c.Type == "" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported credential type %q", c.Type)
	}
	if c.AccessToken == "" {
		return errors.New("codex credential is missing access_token")
	}
	if c.RefreshToken == "" {
		return errors.New("codex credential is missing refresh_token")
	}
	if _, err := c.Expiry(); err != nil {
		return err
	}
	if c.IDToken != "" {
		if claims, err := parseIDToken(c.IDToken); err == nil {
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
		ChatGPTAccountID:               strings.TrimSpace(claims.Auth.ChatGPTAccountID),
		ChatGPTSubscriptionActiveStart: claims.Auth.ChatGPTSubscriptionActiveStart,
		ChatGPTSubscriptionActiveUntil: claims.Auth.ChatGPTSubscriptionActiveUntil,
		PlanType:                       strings.TrimSpace(claims.Auth.ChatGPTPlanType),
	}
	if info.ChatGPTAccountID == "" && info.ChatGPTSubscriptionActiveStart == nil &&
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
	merged := *refreshed
	if merged.RefreshToken == "" {
		merged.RefreshToken = c.RefreshToken
	}
	if merged.IDToken == "" {
		merged.IDToken = c.IDToken
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
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	return &merged, nil
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
