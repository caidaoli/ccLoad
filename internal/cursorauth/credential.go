package cursorauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Credential is the private Cursor payload persisted on a channel.
//
// AccessToken is the session JWT the CLI stores in auth.json and the only
// secret required for control-plane RPCs. RefreshToken is kept alongside it
// because that is how the CLI persists the pair; Cursor does not currently
// expose a documented refresh endpoint for this flow, so a rejected session
// is re-minted from APIKey when one was imported, otherwise the administrator
// must sign in again.
type Credential struct {
	Type         string          `json:"type"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	APIKey       string          `json:"api_key,omitempty"`
	UserID       string          `json:"user_id,omitempty"`
	Email        string          `json:"email,omitempty"`
	Name         string          `json:"name,omitempty"`
	LastRefresh  string          `json:"last_refresh,omitempty"`
	OAuthUsage   json.RawMessage `json:"oauth_usage,omitempty"`
}

// Identity is the account this credential belongs to.
type Identity struct {
	UserID string
	Email  string
}

// ParseCredential decodes and normalizes one persisted Cursor credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("cursor credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("cursor credential exceeds %d bytes", maxCredentialSize)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var credential Credential
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode cursor credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("cursor credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("cursor credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.Type == "" || c.Type == "cursor-cli" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported cursor credential type %q", c.Type)
	}
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.UserID = strings.TrimSpace(c.UserID)
	c.Email = strings.TrimSpace(c.Email)
	c.Name = strings.TrimSpace(c.Name)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	if c.AccessToken == "" {
		return errors.New("cursor credential is missing access_token")
	}
	if strings.ContainsAny(c.AccessToken, " \r\n\t") {
		return errors.New("cursor credential has an invalid access_token")
	}
	if c.LastRefresh != "" {
		parsed, err := time.Parse(time.RFC3339, c.LastRefresh)
		if err != nil {
			return errors.New("cursor credential has invalid last_refresh")
		}
		c.LastRefresh = parsed.UTC().Format(time.RFC3339)
	}
	return nil
}

// Identity returns the account identity used to deduplicate channels.
func (c *Credential) Identity() Identity {
	if c == nil {
		return Identity{}
	}
	return Identity{UserID: strings.TrimSpace(c.UserID), Email: strings.TrimSpace(c.Email)}
}

// IsZero reports whether an identity can match another credential at all.
func (i Identity) IsZero() bool {
	return strings.TrimSpace(i.UserID) == "" && strings.TrimSpace(i.Email) == ""
}

// Matches reports whether two identities describe the same Cursor account.
func (i Identity) Matches(other Identity) bool {
	if i.UserID != "" && other.UserID != "" {
		return i.UserID == other.UserID
	}
	return i.Email != "" && other.Email != "" && strings.EqualFold(i.Email, other.Email)
}

// MergeRefresh keeps identity metadata that a re-resolved credential omits.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("cursor refresh credential is nil")
	}
	merged := *refreshed
	preserve := func(target *string, previous string) {
		if strings.TrimSpace(*target) == "" {
			*target = previous
		}
	}
	preserve(&merged.RefreshToken, c.RefreshToken)
	preserve(&merged.APIKey, c.APIKey)
	preserve(&merged.UserID, c.UserID)
	preserve(&merged.Email, c.Email)
	preserve(&merged.Name, c.Name)
	merged.OAuthUsage = append(json.RawMessage(nil), c.OAuthUsage...)
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
		return "", fmt.Errorf("encode cursor credential: %w", err)
	}
	return string(raw), nil
}
