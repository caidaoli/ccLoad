package xaiauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const maxCredentialSize = 1 << 20

// Credential is the canonical persisted xAI OAuth credential.
type Credential struct {
	Type              string          `json:"type"`
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token"`
	IDToken           string          `json:"id_token,omitempty"`
	TokenType         string          `json:"token_type,omitempty"`
	ExpiresIn         int64           `json:"expires_in,omitempty"`
	ExpiresAt         any             `json:"expires_at,omitempty"`
	Expired           string          `json:"expired"`
	LastRefresh       string          `json:"last_refresh,omitempty"`
	Email             string          `json:"email,omitempty"`
	Subject           string          `json:"sub,omitempty"`
	BaseURL           string          `json:"base_url,omitempty"`
	TokenEndpoint     string          `json:"token_endpoint,omitempty"`
	AuthKind          string          `json:"auth_kind"`
	ClientID          string          `json:"client_id,omitempty"`
	Scope             string          `json:"scope,omitempty"`
	TeamID            string          `json:"team_id,omitempty"`
	SubscriptionTier  string          `json:"subscription_tier,omitempty"`
	EntitlementStatus string          `json:"entitlement_status,omitempty"`
	OAuthUsage        json.RawMessage `json:"oauth_usage,omitempty"`
}

// Identity contains the non-secret account identity derived from a credential.
type Identity struct {
	Email   string `json:"email,omitempty"`
	Subject string `json:"sub,omitempty"`
}

// ParseCredential decodes, validates, and normalizes one credential JSON object.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("xAI credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("xAI credential exceeds %d bytes", maxCredentialSize)
	}
	var credential Credential
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode xAI credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("xAI credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize canonicalizes and validates the credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("xAI credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.IDToken = strings.TrimSpace(c.IDToken)
	c.TokenType = strings.TrimSpace(c.TokenType)
	c.Expired = strings.TrimSpace(c.Expired)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	c.Email = strings.TrimSpace(c.Email)
	c.Subject = strings.TrimSpace(c.Subject)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.TokenEndpoint = strings.TrimSpace(c.TokenEndpoint)
	c.AuthKind = strings.ToLower(strings.TrimSpace(c.AuthKind))
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.Scope = strings.TrimSpace(c.Scope)
	c.TeamID = strings.TrimSpace(c.TeamID)
	c.SubscriptionTier = strings.TrimSpace(c.SubscriptionTier)
	c.EntitlementStatus = strings.TrimSpace(c.EntitlementStatus)
	if c.Type == "" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return errors.New("unsupported xAI credential type")
	}
	if c.AuthKind == "" {
		c.AuthKind = "oauth"
	}
	if c.AuthKind != "oauth" {
		return errors.New("unsupported xAI auth_kind")
	}
	if c.TokenEndpoint != "" {
		endpoint, err := validateAuthURL(c.TokenEndpoint)
		if err != nil {
			return fmt.Errorf("xAI token_endpoint: %w", err)
		}
		c.TokenEndpoint = endpoint
	}
	if c.AccessToken == "" {
		return errors.New("xAI credential is missing access_token")
	}
	if c.RefreshToken == "" {
		return errors.New("xAI credential is missing refresh_token")
	}
	expires, err := c.resolveExpiry()
	if err != nil {
		return err
	}
	c.Expired = expires.UTC().Format(time.RFC3339)
	identity := c.Identity()
	c.Email = identity.Email
	c.Subject = identity.Subject
	return nil
}

func (c *Credential) resolveExpiry() (time.Time, error) {
	if c.Expired != "" {
		if parsed, err := parseTimeValue(c.Expired); err == nil {
			return parsed, nil
		}
		return time.Time{}, errors.New("xAI credential has invalid expired")
	}
	if c.ExpiresAt != nil {
		if parsed, err := parseTimeValue(c.ExpiresAt); err == nil {
			return parsed, nil
		}
		return time.Time{}, errors.New("xAI credential has invalid expires_at")
	}
	if c.LastRefresh != "" && c.ExpiresIn > 0 {
		last, err := parseTimeValue(c.LastRefresh)
		if err != nil {
			return time.Time{}, errors.New("xAI credential has invalid last_refresh")
		}
		return last.Add(time.Duration(c.ExpiresIn) * time.Second), nil
	}
	return time.Time{}, errors.New("xAI credential is missing a valid expiry")
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
	numeric, err := strconv.ParseFloat(raw, 64)
	if err != nil || numeric <= 0 {
		return time.Time{}, errors.New("invalid time value")
	}
	seconds, fraction := mathModf(numeric)
	return time.Unix(seconds, int64(fraction*float64(time.Second))).UTC(), nil
}

func mathModf(value float64) (int64, float64) {
	seconds := int64(value)
	return seconds, value - float64(seconds)
}

// Expiry resolves the credential's absolute expiration time.
func (c *Credential) Expiry() (time.Time, error) {
	if c == nil {
		return time.Time{}, errors.New("xAI credential is nil")
	}
	return c.resolveExpiry()
}

// NeedsRefresh reports whether the credential expires within lead of now.
func (c *Credential) NeedsRefresh(now time.Time, lead time.Duration) (bool, error) {
	expires, err := c.Expiry()
	if err != nil {
		return false, err
	}
	return !expires.After(now.Add(lead)), nil
}

// MergeRefresh combines a token response with fields that the response legitimately omits.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("xAI refresh credential is nil")
	}
	merged := *refreshed
	freshIdentity, hasFreshIdentity := credentialIdentity(refreshed)
	identity := freshIdentity
	oldIdentity := c.Identity()
	if identity.Email == "" {
		identity.Email = oldIdentity.Email
	}
	if identity.Subject == "" {
		identity.Subject = oldIdentity.Subject
	}
	preserve := func(target *string, old string) {
		if strings.TrimSpace(*target) == "" {
			*target = old
		}
	}
	preserve(&merged.RefreshToken, c.RefreshToken)
	if !hasFreshIdentity {
		preserve(&merged.IDToken, c.IDToken)
	}
	preserve(&merged.TokenType, c.TokenType)
	preserve(&merged.BaseURL, c.BaseURL)
	preserve(&merged.TokenEndpoint, c.TokenEndpoint)
	preserve(&merged.ClientID, c.ClientID)
	preserve(&merged.Scope, c.Scope)
	preserve(&merged.TeamID, c.TeamID)
	preserve(&merged.SubscriptionTier, c.SubscriptionTier)
	preserve(&merged.EntitlementStatus, c.EntitlementStatus)
	merged.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
	merged.Email = identity.Email
	merged.Subject = identity.Subject
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	return &merged, nil
}

// Identity derives email and subject using the canonical token precedence.
func (c *Credential) Identity() Identity {
	if c == nil {
		return Identity{}
	}
	identity, _ := credentialIdentity(c)
	return identity
}

func credentialIdentity(c *Credential) (Identity, bool) {
	if c == nil {
		return Identity{}, false
	}
	identity := Identity{}
	idTokenIdentity := identityFromJWT(c.IDToken)
	accessIdentity := identityFromJWT(c.AccessToken)
	identity.Email = idTokenIdentity.Email
	identity.Subject = idTokenIdentity.Subject
	if identity.Email == "" {
		identity.Email = accessIdentity.Email
	}
	if identity.Subject == "" {
		identity.Subject = accessIdentity.Subject
	}
	if identity.Email == "" {
		identity.Email = strings.TrimSpace(c.Email)
	}
	if identity.Subject == "" {
		identity.Subject = strings.TrimSpace(c.Subject)
	}
	hasIdentity := idTokenIdentity.Email != "" || idTokenIdentity.Subject != "" ||
		accessIdentity.Email != "" || accessIdentity.Subject != "" ||
		strings.TrimSpace(c.Email) != "" || strings.TrimSpace(c.Subject) != ""
	return identity, hasIdentity
}

func identityFromJWT(token string) Identity {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Identity{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return Identity{}
	}
	stringClaim := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	return Identity{Email: stringClaim("email", "preferred_username"), Subject: stringClaim("sub")}
}

// JSON returns normalized credential JSON suitable for persistence.
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode xAI credential: %w", err)
	}
	return string(raw), nil
}

// Redacted returns a secret-free diagnostic representation of the credential.
func (c *Credential) Redacted() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("xAI credential{type=%q,email=%q,sub=%q,expires=%q,tier=%q,entitlement=%q}", c.Type, c.Email, c.Subject, c.Expired, c.SubscriptionTier, c.EntitlementStatus)
}

func (c *Credential) String() string { return c.Redacted() }
