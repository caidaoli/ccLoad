package zaiauth

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Credential is the private Z.ai Coding Plan payload persisted on a channel.
//
// APIKey is the only field required to forward traffic: Z.ai Coding Plan keys
// are long-lived, so there is no rotating access token on the request path.
// AccessToken is the ZCode OAuth token that minted APIKey; it is kept so a
// rejected key can be re-resolved without a new browser authorization.
type Credential struct {
	Type        string `json:"type"`
	APIKey      string `json:"api_key"`
	AccessToken string `json:"access_token,omitempty"`
	JWTToken    string `json:"jwt_token,omitempty"`
	// DeviceID is the stable per-credential fingerprint ZCode reports in
	// metadata.user_id. It is derived from the account identity when absent.
	DeviceID    string          `json:"device_id,omitempty"`
	UserID      string          `json:"user_id,omitempty"`
	Email       string          `json:"email,omitempty"`
	Name        string          `json:"name,omitempty"`
	LastRefresh string          `json:"last_refresh,omitempty"`
	OAuthUsage  json.RawMessage `json:"oauth_usage,omitempty"`
}

// Identity is the account this credential belongs to.
type Identity struct {
	UserID string
	Email  string
}

// ParseCredential decodes and normalizes one persisted Z.ai credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("z.ai credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("z.ai credential exceeds %d bytes", maxCredentialSize)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var credential Credential
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode z.ai credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("z.ai credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("z.ai credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.Type == "" || c.Type == "zcode" || c.Type == "z.ai" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported z.ai credential type %q", c.Type)
	}
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.JWTToken = strings.TrimSpace(c.JWTToken)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.UserID = strings.TrimSpace(c.UserID)
	c.Email = strings.TrimSpace(c.Email)
	c.Name = strings.TrimSpace(c.Name)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	if c.APIKey == "" {
		return errors.New("z.ai credential is missing api_key")
	}
	if strings.ContainsAny(c.APIKey, " \r\n\t") {
		return errors.New("z.ai credential has an invalid api_key")
	}
	if c.LastRefresh != "" {
		parsed, err := time.Parse(time.RFC3339, c.LastRefresh)
		if err != nil {
			return errors.New("z.ai credential has invalid last_refresh")
		}
		c.LastRefresh = parsed.UTC().Format(time.RFC3339)
	}
	if c.DeviceID == "" {
		c.DeviceID = DeriveDeviceID(c.Identity(), c.APIKey)
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

// Matches reports whether two identities describe the same Z.ai account.
func (i Identity) Matches(other Identity) bool {
	if i.UserID != "" && other.UserID != "" {
		return i.UserID == other.UserID
	}
	return i.Email != "" && other.Email != "" && strings.EqualFold(i.Email, other.Email)
}

// DeriveDeviceID returns a stable UUID-shaped device fingerprint. ZCode stores
// a random per-installation UUID; ccLoad derives one per credential so the same
// account keeps reporting the same device across restarts.
func DeriveDeviceID(identity Identity, fallback string) string {
	seed := strings.TrimSpace(identity.UserID)
	if seed == "" {
		seed = strings.ToLower(strings.TrimSpace(identity.Email))
	}
	if seed == "" {
		seed = strings.TrimSpace(fallback)
	}
	if seed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("ccLoad:zai-device:" + seed))
	// RFC 4122 variant/version bits keep the value shaped like ZCode's UUID.
	digest[6] = (digest[6] & 0x0f) | 0x40
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

// MergeRefresh keeps identity metadata that a re-resolved credential omits.
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("z.ai refresh credential is nil")
	}
	merged := *refreshed
	preserve := func(target *string, previous string) {
		if strings.TrimSpace(*target) == "" {
			*target = previous
		}
	}
	preserve(&merged.AccessToken, c.AccessToken)
	preserve(&merged.JWTToken, c.JWTToken)
	preserve(&merged.DeviceID, c.DeviceID)
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
		return "", fmt.Errorf("encode z.ai credential: %w", err)
	}
	return string(raw), nil
}
