package qoderauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// LoginTTL bounds the lifetime of a pending device login.
const LoginTTL = 5 * time.Minute

// ErrLoginPending indicates that the user has not yet authorized the device.
var ErrLoginPending = errors.New("qoder login is pending")

// LoginSession keeps the PKCE verifier on the server. Only AuthURL is public.
type LoginSession struct {
	AuthURL   string `json:"auth_url"`
	region    string
	nonce     string
	verifier  string
	machineID string
	expiresAt time.Time
}

// StartLogin prepares a PKCE device login without contacting the upstream.
func StartLogin(region string) (*LoginSession, error) {
	if err := authRegion(region); err != nil {
		return nil, err
	}
	verifier, err := authRandom(32)
	if err != nil {
		return nil, err
	}
	nonce, err := authRandom(16)
	if err != nil {
		return nil, err
	}
	machineID, err := authRandom(32)
	if err != nil {
		return nil, err
	}
	origin := "https://qoder.com"
	if region == "cn" {
		origin = "https://qoder.com.cn"
	}
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"challenge":        {base64.RawURLEncoding.EncodeToString(digest[:])},
		"challenge_method": {"S256"},
		"machine_id":       {machineID},
		"nonce":            {nonce},
	}
	return &LoginSession{AuthURL: origin + "/device/selectAccounts?" + query.Encode(), region: region, nonce: nonce, verifier: verifier, machineID: machineID, expiresAt: time.Now().Add(LoginTTL)}, nil
}

// StartLogin prepares a device login for callers that already own a Service.
func (s *Service) StartLogin(region string) (*LoginSession, error) { return StartLogin(region) }

// PollLogin performs one poll. The caller owns the polling schedule and session.
func (s *Service) PollLogin(ctx context.Context, session *LoginSession) (*Credential, error) {
	if session == nil || session.verifier == "" || session.nonce == "" {
		return nil, errors.New("invalid Qoder login session")
	}
	if !time.Now().Before(session.expiresAt) {
		return nil, errors.New("qoder login session expired")
	}
	if err := authRegion(session.region); err != nil {
		return nil, err
	}
	c := &Credential{Region: session.region, MachineID: session.machineID}
	query := url.Values{"nonce": {session.nonce}, "verifier": {session.verifier}, "challenge_method": {"S256"}}
	data, status, err := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, "/api/v1/deviceToken/poll?")+query.Encode(), nil, authHeaders(nil))
	if status == http.StatusAccepted || status == http.StatusNotFound {
		return nil, ErrLoginPending
	}
	if err != nil {
		return nil, err
	}
	c.AccessToken = authText(data, "token")
	if c.AccessToken == "" {
		return nil, errors.New("qoder login response is missing token")
	}
	c.RefreshToken = authText(data, "refresh_token")
	c.UserID = authText(data, "user_id")
	c.ExpiresAt = authTimestamp(data)
	return s.CompleteIdentity(ctx, c)
}
