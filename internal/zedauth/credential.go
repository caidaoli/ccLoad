package zedauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Credential keeps the native long-lived Zed credential separate from the
// short-lived LLM JWT used by /completions.
type Credential struct {
	Type   string `json:"type"`
	UserID string `json:"user_id"`
	// SystemID is optional. When empty, Zed requests omit x-zed-system-id;
	// the upstream service decides whether the account can use trial access.
	SystemID         string          `json:"system_id"`
	NativeCredential json.RawMessage `json:"native_credential"`
	AccessToken      string          `json:"access_token,omitempty"`
	ExpiresAt        int64           `json:"expires_at,omitempty"`
	Username         string          `json:"username,omitempty"`
	GitHubUserID     string          `json:"github_user_id,omitempty"`
	GitHubUserLogin  string          `json:"github_user_login,omitempty"`
	LastRefresh      string          `json:"last_refresh,omitempty"`
	OAuthUsage       json.RawMessage `json:"oauth_usage,omitempty"`
}

// ParseCredential decodes and validates a persisted Zed credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("zed credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("zed credential exceeds %d bytes", maxCredentialSize)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var credential Credential
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode zed credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("zed credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// NewCredential builds a validated credential from a native OAuth callback.
func NewCredential(userID, systemID string, native json.RawMessage) (*Credential, error) {
	credential := &Credential{
		Type: ChannelType, UserID: userID, SystemID: systemID,
		NativeCredential: append(json.RawMessage(nil), native...),
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

// Normalize validates and canonicalizes the credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("zed credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.Type == "" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported zed credential type %q", c.Type)
	}
	c.UserID = strings.TrimSpace(c.UserID)
	c.SystemID = strings.TrimSpace(c.SystemID)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.Username = strings.TrimSpace(c.Username)
	c.GitHubUserID = strings.TrimSpace(c.GitHubUserID)
	c.GitHubUserLogin = strings.TrimSpace(c.GitHubUserLogin)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	if c.UserID == "" {
		return errors.New("zed credential is missing user_id")
	}
	if len(c.NativeCredential) == 0 || !json.Valid(c.NativeCredential) {
		return errors.New("zed credential has an invalid native_credential")
	}
	var native map[string]json.RawMessage
	if err := json.Unmarshal(c.NativeCredential, &native); err != nil {
		return errors.New("zed native credential must be a JSON object")
	}
	if c.GitHubUserLogin == "" {
		_ = json.Unmarshal(native["github_user_login"], &c.GitHubUserLogin)
		c.GitHubUserLogin = strings.TrimSpace(c.GitHubUserLogin)
	}
	if c.GitHubUserID == "" {
		c.GitHubUserID = nativeJSONScalar(native["github_user_id"])
	}
	if c.AccessToken != "" && strings.ContainsAny(c.AccessToken, " \r\n\t") {
		return errors.New("zed credential has an invalid access_token")
	}
	if c.LastRefresh != "" {
		parsed, err := time.Parse(time.RFC3339, c.LastRefresh)
		if err != nil {
			return errors.New("zed credential has invalid last_refresh")
		}
		c.LastRefresh = parsed.UTC().Format(time.RFC3339)
	}
	return nil
}

func nativeJSONScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

// NativeAuthorization returns Zed's native user-and-credential authorization value.
func (c *Credential) NativeAuthorization() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.UserID) + " " + strings.TrimSpace(string(c.NativeCredential))
}

// NeedsRefresh reports whether the short-lived LLM token needs replacement.
func (c *Credential) NeedsRefresh(now time.Time, lead time.Duration) bool {
	return c == nil || c.AccessToken == "" || c.ExpiresAt <= now.Add(lead).Unix()
}

// MergeRefresh preserves native account state while replacing the LLM token fields.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("zed refresh credential is nil")
	}
	merged := *c
	merged.AccessToken = refreshed.AccessToken
	merged.ExpiresAt = refreshed.ExpiresAt
	merged.LastRefresh = refreshed.LastRefresh
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	return &merged, nil
}

// JSON validates and encodes the credential for persistence.
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode zed credential: %w", err)
	}
	return string(raw), nil
}

// CloneCredential returns a deep copy of a credential.
func CloneCredential(c *Credential) *Credential {
	if c == nil {
		return nil
	}
	clone := *c
	clone.NativeCredential = append(json.RawMessage(nil), c.NativeCredential...)
	clone.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
	return &clone
}

// JWTExpiry returns the exp claim from a Zed LLM token.
func JWTExpiry(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, errors.New("zed LLM token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, errors.New("zed LLM token has an invalid payload")
	}
	var claims struct {
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return 0, errors.New("zed LLM token has invalid claims")
	}
	expiresAt, err := claims.ExpiresAt.Int64()
	if err != nil || expiresAt <= 0 {
		return 0, errors.New("zed LLM token is missing exp")
	}
	return expiresAt, nil
}
