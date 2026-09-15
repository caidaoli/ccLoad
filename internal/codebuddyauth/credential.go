// Package codebuddyauth implements the CodeBuddy CLI authentication contract.
package codebuddyauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CodeBuddy provider endpoints and authentication lifetimes.
const (
	ChannelType                 = "codebuddy"
	BaseURL                     = "https://copilot.tencent.com"
	InternationalBaseURL        = "https://www.codebuddy.ai"
	WorkBuddyBaseURL            = "https://www.workbuddy.ai"
	CompletionsURL              = BaseURL + "/v2/chat/completions"
	InternationalCompletionsURL = InternationalBaseURL + "/v2/chat/completions"
	WorkBuddyCompletionsURL     = WorkBuddyBaseURL + "/v2/chat/completions"
	// CLIVersion is the official @tencent-ai/codebuddy-code version used on
	// chat and catalog requests. FetchModels historically pinned 2.148.0;
	// keep one value so UA and X-IDE-Version cannot drift.
	CLIVersion  = "2.151.0"
	RefreshLead = time.Minute
	LoginTTL    = 5 * time.Minute
)

// ErrDailyCheckinUnsupported indicates that the selected public edition does
// not expose the domestic daily check-in operation.
var ErrDailyCheckinUnsupported = errors.New("CodeBuddy international edition does not support daily check-in")

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
	// BaseURL identifies the CodeBuddy public edition that issued the token.
	// Empty values retain the historical China endpoint for backwards compatibility.
	BaseURL string `json:"base_url,omitempty"`
	// OAuthUsage stores the last billing snapshot. It is intentionally kept in
	// the private credential envelope and exposed only through safe metadata.
	OAuthUsage string `json:"oauth_usage,omitempty"`
}

// CompletionsURLForBaseURL returns the OpenAI-compatible endpoint for an edition.
func CompletionsURLForBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = BaseURL
	}
	return baseURL + "/v2/chat/completions"
}

// Endpoint returns the control-plane endpoint associated with the credential.
func (c *Credential) Endpoint() string {
	if c != nil && strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return BaseURL
}

// IsInternational reports whether the credential belongs to CodeBuddy's
// international public edition (codebuddy.ai or workbuddy.ai).
func (c *Credential) IsInternational() bool {
	host := ChatHost(c)
	return host == "www.codebuddy.ai" || host == "www.workbuddy.ai"
}

// ChatHost is the host the official CLI uses for chat. International OAuth
// tokens often store base_url=www.codebuddy.ai while JWT iss is workbuddy.ai;
// sending that token to the wrong site is upstream 403 code 11140.
func ChatHost(c *Credential) string {
	if host := canonicalProductHost(issuerHost(c)); isKnownProductHost(host) {
		return host
	}
	if c != nil {
		if u, err := url.Parse(c.Endpoint()); err == nil {
			if host := canonicalProductHost(u.Hostname()); host != "" {
				return host
			}
		}
	}
	return "copilot.tencent.com"
}

// ChatBaseURL is the https origin for ChatHost.
func ChatBaseURL(c *Credential) string {
	return "https://" + ChatHost(c)
}

// RewriteChatRequest retargets a production CodeBuddy/WorkBuddy URL to the
// token's chat host. Test and custom upstreams are left unchanged.
func RewriteChatRequest(req *http.Request, c *Credential) {
	if req == nil || req.URL == nil || !isKnownProductHost(canonicalProductHost(req.URL.Hostname())) {
		return
	}
	host := ChatHost(c)
	req.URL.Scheme = "https"
	req.URL.Host = host
	req.Host = host
}

// SupportsDailyCheckin reports whether the credential's edition supports the
// CodeBuddy daily check-in operation.
func (c *Credential) SupportsDailyCheckin() bool {
	return c != nil && !c.IsInternational()
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
	for _, value := range []*string{&c.AccessToken, &c.RefreshToken, &c.Domain, &c.UID, &c.EnterpriseID, &c.BaseURL} {
		*value = strings.TrimSpace(*value)
		if strings.ContainsFunc(*value, func(r rune) bool { return r < 32 || r == 127 }) {
			return errors.New("CodeBuddy credential contains invalid header characters")
		}
	}
	if c.BaseURL != "" {
		normalized, err := normalizeBaseURL(c.BaseURL)
		if err != nil {
			return err
		}
		c.BaseURL = normalized
	}
	if c.AccessToken == "" {
		return errors.New("CodeBuddy credential is missing access_token")
	}
	if c.ExpiresAt < 0 {
		return errors.New("CodeBuddy credential has invalid expires_at")
	}
	return nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid CodeBuddy credential base_url")
	}
	switch strings.ToLower(u.Hostname()) {
	case "copilot.tencent.com", "www.codebuddy.ai", "www.workbuddy.ai":
	default:
		return "", errors.New("unsupported CodeBuddy credential base_url")
	}
	return strings.TrimRight(raw, "/"), nil
}

func issuerHost(c *Credential) string {
	if c == nil {
		return ""
	}
	parts := strings.Split(c.AccessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := decodeJWTPayload(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	u, err := url.Parse(claims.Iss)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return u.Hostname()
}

func decodeJWTPayload(seg string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(seg)
}

func canonicalProductHost(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "workbuddy.ai", "www.workbuddy.ai":
		return "www.workbuddy.ai"
	case "codebuddy.ai", "www.codebuddy.ai":
		return "www.codebuddy.ai"
	case "codebuddy.cn", "www.codebuddy.cn":
		return "www.codebuddy.cn"
	case "copilot.tencent.com":
		return "copilot.tencent.com"
	default:
		return ""
	}
}

func isKnownProductHost(host string) bool {
	switch host {
	case "www.workbuddy.ai", "www.codebuddy.ai", "www.codebuddy.cn", "copilot.tencent.com":
		return true
	default:
		return false
	}
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
