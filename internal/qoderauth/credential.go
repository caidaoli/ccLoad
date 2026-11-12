// Package qoderauth implements Qoder authentication and control-plane requests.
package qoderauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ChannelType identifies the Qoder provider.
const ChannelType = "qoder"

// ErrCannotRefresh marks a credential without a refresh token or PAT.
var ErrCannotRefresh = errors.New("qoder credential cannot be refreshed")

// Credential is the private, canonical Qoder credential envelope. ExpiresAt is
// Unix seconds; region is mandatory because the editions issue distinct tokens.
type Credential struct {
	Type                string `json:"type"`
	Region              string `json:"region"`
	AccessToken         string `json:"access_token,omitempty"`
	RefreshToken        string `json:"refresh_token,omitempty"`
	PAT                 string `json:"pat,omitempty"`
	UserID              string `json:"user_id,omitempty"`
	MachineID           string `json:"machine_id,omitempty"`
	Name                string `json:"name,omitempty"`
	Email               string `json:"email,omitempty"`
	ExpiresAt           int64  `json:"expires_at,omitempty"`
	OAuthUsage          string `json:"oauth_usage,omitempty"`
	LastCheckinAt       int64  `json:"last_checkin_at,omitempty"`
	LastCheckinCampaign string `json:"last_checkin_campaign,omitempty"`
}

// ParseCredential imports the canonical credential JSON.
func ParseCredential(raw []byte) (*Credential, error) {
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("invalid Qoder credential JSON")
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Normalize validates the edition and secrets without guessing an account's region.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("missing Qoder credential")
	}
	c.Type = strings.TrimSpace(c.Type)
	if c.Type != "" && c.Type != ChannelType {
		return errors.New("invalid Qoder credential type")
	}
	c.Type = ChannelType
	c.Region = strings.TrimSpace(c.Region)
	if err := authRegion(c.Region); err != nil {
		return err
	}
	for _, secret := range []*string{&c.AccessToken, &c.RefreshToken, &c.PAT} {
		*secret = strings.TrimSpace(*secret)
		if strings.ContainsFunc(*secret, unicode.IsSpace) || strings.ContainsAny(*secret, "|\x00") {
			return errors.New("invalid Qoder credential token")
		}
	}
	for _, id := range []*string{&c.UserID, &c.MachineID} {
		*id = strings.TrimSpace(*id)
		if strings.ContainsFunc(*id, unicode.IsControl) || strings.Contains(*id, "|") {
			return errors.New("invalid Qoder account identity")
		}
	}
	if c.AccessToken == "" && c.PAT == "" {
		return errors.New("qoder access_token or pat is required")
	}
	if c.ExpiresAt < 0 {
		return errors.New("invalid Qoder expiry")
	}
	if c.ExpiresAt >= 1_000_000_000_000 {
		c.ExpiresAt /= 1000
	}
	c.Name, c.Email = strings.TrimSpace(c.Name), strings.TrimSpace(c.Email)
	return nil
}

// JSON returns a validated canonical credential without mutating the receiver.
func (c *Credential) JSON() (string, error) {
	if c == nil {
		return "", errors.New("missing Qoder credential")
	}
	normalized := *c
	if err := normalized.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(&normalized)
	return string(raw), err
}

// NeedsRefresh checks the token's known expiry with a one-minute safety margin.
func (c *Credential) NeedsRefresh(now time.Time) bool {
	return c != nil && (c.AccessToken == "" || (c.ExpiresAt > 0 && c.ExpiresAt <= now.Add(time.Minute).Unix()))
}

// GatewayURL selects the inference host for the credential's region.
/**
 * @return The edition's inference origin, or empty for an invalid credential.
 */
func (c *Credential) GatewayURL() string {
	if c == nil {
		return ""
	}
	switch c.Region {
	case "cn":
		return "https://gateway.qoder.com.cn"
	case "global":
		return "https://api3.qoder.sh"
	default:
		return ""
	}
}

// OpenAPIURL selects the account/control-plane host for the credential's region.
func (c *Credential) OpenAPIURL() string {
	if c == nil {
		return ""
	}
	switch c.Region {
	case "cn":
		return "https://openapi.qoder.com.cn"
	case "global":
		return "https://openapi.qoder.sh"
	default:
		return ""
	}
}

func authRegion(region string) error {
	if region != "cn" && region != "global" {
		return errors.New("qoder region must be cn or global")
	}
	return nil
}

func authRandom(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Qoder identity: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
