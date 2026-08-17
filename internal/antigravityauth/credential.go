// Package antigravityauth implements the Antigravity OAuth credential lifecycle.
package antigravityauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/oauthcost"
)

const (
	// ChannelType is the CLIProxyAPI provider type stored in Antigravity credentials.
	ChannelType = "antigravity"
	// CredentialRefreshLead is the window in which an access token is treated as stale.
	CredentialRefreshLead = 5 * time.Minute
	maxCredentialSize     = 1 << 20
)

// PaidTier is the minimal non-secret subscription metadata persisted with an
// Antigravity credential.
type PaidTier struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// DisplayName returns the human-readable tier name, falling back to its ID.
func (t *PaidTier) DisplayName() string {
	if t == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(t.ID), "free-tier") {
		return "Antigravity Free"
	}
	if name := strings.TrimSpace(t.Name); name != "" {
		return name
	}
	return strings.TrimSpace(t.ID)
}

// Credential is the CLIProxyAPI-compatible Antigravity OAuth payload stored in
// the private OAuth channel column.
type Credential struct {
	Type           string           `json:"type"`
	AccessToken    string           `json:"access_token"`
	RefreshToken   string           `json:"refresh_token"`
	ExpiresIn      int64            `json:"expires_in,omitempty"`
	Timestamp      int64            `json:"timestamp,omitempty"`
	Expired        string           `json:"expired"`
	Email          string           `json:"email,omitempty"`
	ProjectID      string           `json:"project_id,omitempty"`
	PaidTier       *PaidTier        `json:"paid_tier,omitempty"`
	OAuthUsage     json.RawMessage  `json:"oauth_usage,omitempty"`
	QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage,omitempty"`
}

// ParseCredential validates imported CLIProxyAPI JSON and returns its canonical form.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("credential: Antigravity data is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("credential: Antigravity data exceeds %d bytes", maxCredentialSize)
	}
	var credential Credential
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode Antigravity credential: %w", err)
	}
	if decoder.Decode(&struct{}{}) == nil {
		return nil, errors.New("credential: Antigravity data contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("credential: Antigravity data is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.Expired = strings.TrimSpace(c.Expired)
	c.Email = strings.TrimSpace(c.Email)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	if c.PaidTier != nil {
		c.PaidTier.ID = strings.TrimSpace(c.PaidTier.ID)
		c.PaidTier.Name = strings.TrimSpace(c.PaidTier.Name)
		if c.PaidTier.ID == "" && c.PaidTier.Name == "" {
			c.PaidTier = nil
		}
	}
	if err := oauthcost.Validate(c.QuotaCostUsage); err != nil {
		return fmt.Errorf("credential: Antigravity data has invalid quota_cost_usage: %w", err)
	}
	if c.Type == "" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported credential type %q", c.Type)
	}
	if c.AccessToken == "" {
		return errors.New("credential: Antigravity data is missing access_token")
	}
	if c.RefreshToken == "" {
		return errors.New("credential: Antigravity data is missing refresh_token")
	}
	if c.Expired == "" && c.Timestamp > 0 && c.ExpiresIn > 0 {
		c.Expired = time.UnixMilli(c.Timestamp).UTC().Add(time.Duration(c.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if _, err := c.Expiry(); err != nil {
		return err
	}
	return nil
}

// Expiry returns the absolute access-token expiration time.
func (c *Credential) Expiry() (time.Time, error) {
	if c == nil || strings.TrimSpace(c.Expired) == "" {
		return time.Time{}, errors.New("credential: Antigravity data is missing expired")
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(c.Expired))
	if err != nil {
		return time.Time{}, fmt.Errorf("credential: Antigravity data has invalid expired: %w", err)
	}
	return expiresAt, nil
}

// NeedsRefresh reports whether the access token is inside the refresh window.
func (c *Credential) NeedsRefresh(now time.Time, lead time.Duration) (bool, error) {
	expiresAt, err := c.Expiry()
	if err != nil {
		return false, err
	}
	return !expiresAt.After(now.Add(lead)), nil
}

// MergeRefresh keeps stable identity fields and a rotated refresh token.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("refresh: Antigravity credential is nil")
	}
	merged := *refreshed
	if merged.RefreshToken == "" {
		merged.RefreshToken = c.RefreshToken
	}
	if merged.Email == "" {
		merged.Email = c.Email
	}
	if merged.ProjectID == "" {
		merged.ProjectID = c.ProjectID
	}
	if merged.PaidTier == nil && c.PaidTier != nil {
		paidTier := *c.PaidTier
		merged.PaidTier = &paidTier
	}
	merged.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
	merged.QuotaCostUsage = oauthcost.Clone(c.QuotaCostUsage)
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
		return "", fmt.Errorf("encode Antigravity credential: %w", err)
	}
	return string(raw), nil
}
