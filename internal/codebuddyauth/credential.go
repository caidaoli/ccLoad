// Package codebuddyauth implements the CodeBuddy CLI authentication contract.
package codebuddyauth

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CodeBuddy provider endpoints and authentication lifetimes.
const (
	ChannelType    = "codebuddy"
	BaseURL        = "https://copilot.tencent.com"
	CompletionsURL = BaseURL + "/v2/chat/completions"
	RefreshLead    = time.Minute
	LoginTTL       = 5 * time.Minute
)

// DefaultModels is the reference provider's catalog, not an account entitlement list.
var DefaultModels = []string{"glm-5.2", "glm-5.1", "glm-5v-turbo", "kimi-k2.7", "minimax-m3-pay", "hy3", "hy3-preview", "hy3-preview-agent", "deepseek-v4-pro", "deepseek-v4-flash"}

// Credential is the canonical storage format. Import also accepts workbuddy.json.
type Credential struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Domain       string `json:"domain,omitempty"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

// ParseCredential accepts canonical credentials and workbuddy.json exports.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("invalid CodeBuddy credential size")
	}
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("invalid CodeBuddy credential JSON")
	}
	if c.AccessToken == "" {
		var legacy struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, errors.New("invalid workbuddy credential")
		}
		c.AccessToken, c.RefreshToken, c.ExpiresAt, c.Domain = legacy.Auth.AccessToken, legacy.Auth.RefreshToken, legacy.Auth.ExpiresAt, legacy.Auth.Domain
		c.UID, c.EnterpriseID, c.Nickname = legacy.Account.UID, legacy.Account.EnterpriseID, legacy.Account.Nickname
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Normalize validates credentials before storage or use in request headers.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("CodeBuddy credential is nil")
	}
	if c.Type != "" && c.Type != ChannelType && c.Type != "workbuddy" {
		return errors.New("invalid CodeBuddy credential type")
	}
	c.Type = ChannelType
	for _, value := range []*string{&c.AccessToken, &c.RefreshToken, &c.Domain, &c.UID, &c.EnterpriseID} {
		*value = strings.TrimSpace(*value)
		if strings.ContainsFunc(*value, func(r rune) bool { return r < 32 || r == 127 }) {
			return errors.New("CodeBuddy credential contains invalid header characters")
		}
	}
	if c.AccessToken == "" {
		return errors.New("CodeBuddy credential is missing access_token")
	}
	if c.ExpiresAt < 0 {
		return errors.New("CodeBuddy credential has invalid expires_at")
	}
	return nil
}

// JSON encodes a validated copy in canonical storage format.
func (c *Credential) JSON() (string, error) {
	copy := *c
	if err := copy.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(copy)
	return string(raw), err
}

// NeedsRefresh reports whether a known expiry is within the refresh lead time.
func (c *Credential) NeedsRefresh(now time.Time) bool {
	return c.ExpiresAt > 0 && c.ExpiresAt <= now.Add(RefreshLead).Unix()
}
