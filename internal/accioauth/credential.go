// Package accioauth implements Accio authentication and control-plane requests.
package accioauth

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ChannelType identifies Accio credentials.
const ChannelType = "accio"

// ErrCannotRefresh marks credentials without a refresh token.
var ErrCannotRefresh = errors.New("accio credential cannot be refreshed")

// Credential is the canonical private credential envelope. ExpiresAt is Unix seconds.
type Credential struct {
	Type         string `json:"type"`
	Region       string `json:"region"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	OAuthUsage   string `json:"oauth_usage,omitempty"`
}

// ParseCredential accepts canonical JSON and the official desktop token fields.
// Region must be supplied explicitly; a token does not identify its edition.
func ParseCredential(raw []byte) (*Credential, error) {
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("invalid Accio credential JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid Accio credential JSON")
	}
	if c.AccessToken == "" {
		c.AccessToken = authText(fields, "accessToken")
	}
	if c.RefreshToken == "" {
		c.RefreshToken = authText(fields, "refreshToken")
	}
	if c.DeviceID == "" {
		c.DeviceID = authText(fields, "utdid", "deviceId")
	}
	if c.UserID == "" {
		c.UserID = authText(fields, "userId")
	}
	if c.Name == "" {
		c.Name = authText(fields, "nickname")
	}
	if _, present := fields["expires_at"]; !present {
		var err error
		c.ExpiresAt, err = authExpiry(fields["expiresAt"])
		if err != nil {
			return nil, err
		}
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Normalize validates secrets and normalizes expiry to seconds.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("missing Accio credential")
	}
	c.Type = strings.TrimSpace(c.Type)
	if c.Type != "" && c.Type != ChannelType {
		return errors.New("invalid Accio credential type")
	}
	c.Type = ChannelType
	c.Region = strings.TrimSpace(c.Region)
	if c.Region != "cn" && c.Region != "global" {
		return errors.New("accio region must be cn or global")
	}
	for _, token := range []*string{&c.AccessToken, &c.RefreshToken, &c.DeviceID} {
		*token = strings.TrimSpace(*token)
		if strings.ContainsFunc(*token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || strings.Contains(*token, "|") {
			return errors.New("invalid Accio credential token")
		}
	}
	if c.AccessToken == "" {
		return errors.New("accio access_token is required")
	}
	c.UserID = strings.TrimSpace(c.UserID)
	if strings.ContainsFunc(c.UserID, unicode.IsControl) || strings.Contains(c.UserID, "|") {
		return errors.New("invalid Accio account identity")
	}
	if c.ExpiresAt < 0 {
		return errors.New("invalid Accio expiry")
	}
	if c.ExpiresAt >= 1_000_000_000_000 {
		c.ExpiresAt /= 1000
	}
	c.Email, c.Name = strings.TrimSpace(c.Email), strings.TrimSpace(c.Name)
	return nil
}

// JSON serializes a validated copy without mutating the receiver.
func (c *Credential) JSON() (string, error) {
	normalized, err := authCopy(c)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	return string(raw), err
}

// NeedsRefresh applies a one-minute margin to a known expiry.
func (c *Credential) NeedsRefresh(now time.Time) bool {
	return c != nil && (c.AccessToken == "" || c.ExpiresAt > 0 && c.ExpiresAt <= now.Add(time.Minute).Unix())
}

// GatewayURL is shared by both Accio editions.
func (c *Credential) GatewayURL() string { return "https://phoenix-gw.alibaba.com" }

func authExpiry(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number int64
	if json.Unmarshal(raw, &number) != nil {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return 0, errors.New("invalid Accio expiry")
		}
		var err error
		number, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			parsed, parseErr := time.Parse(time.RFC3339, value)
			if parseErr != nil {
				return 0, errors.New("invalid Accio expiry")
			}
			number = parsed.Unix()
		}
	}
	if number < 0 {
		return 0, errors.New("invalid Accio expiry")
	}
	if number >= 1_000_000_000_000 {
		number /= 1000
	}
	return number, nil
}
