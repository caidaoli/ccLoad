package qoderauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// LoginTTL bounds the lifetime of a pending device login.
const LoginTTL = 5 * time.Minute

/**
 * @brief Indicates the device poll can continue on the caller's existing schedule.
 */
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

// StartLogin prepares a PKCE device login for the edition's CLI client.
/**
 * @param region The explicit cn or global edition.
 */
func StartLogin(region string) (*LoginSession, error) {
	if err := authRegion(region); err != nil {
		return nil, err
	}
	verifier, err := authRandom(32)
	if err != nil {
		return nil, err
	}
	nonce := uuid.NewString()
	machineID, err := authRandom(32)
	if err != nil {
		return nil, err
	}
	origin := "https://qoder.com"
	clientID := "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	if region == "cn" {
		origin = "https://qoder.cn"
		clientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	}
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"challenge":        {base64.RawURLEncoding.EncodeToString(digest[:])},
		"challenge_method": {"S256"},
		"client_id":        {clientID},
		"machine_id":       {machineID},
		"nonce":            {nonce},
	}
	return &LoginSession{AuthURL: origin + "/device/selectAccounts?" + query.Encode(), region: region, nonce: nonce, verifier: verifier, machineID: machineID, expiresAt: time.Now().Add(LoginTTL)}, nil
}

// StartLogin prepares a device login for callers that already own a Service.
func (s *Service) StartLogin(region string) (*LoginSession, error) { return StartLogin(region) }

/**
 * @brief Polls for device credentials and verifies their owner before returning them.
 * @param ctx Request context, including the caller's deadline and cancellation.
 * @param session The pending login containing the server-held PKCE verifier.
 * @return Verified credentials, or nil on pending and failure outcomes.
 * @return ErrLoginPending when waiting for authorization or recovering from a transport failure
 * before receiving a poll response. Invalid responses and identity verification failures are terminal.
 */
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if status == http.StatusAccepted || status == http.StatusNotFound || errors.Is(err, errAuthRetryableTransport) {
		return nil, ErrLoginPending
	}
	if err != nil {
		return nil, err
	}
	c.AccessToken = authText(data, "token", "device_token", "access_token", "accessToken", "deviceToken")
	if c.AccessToken == "" {
		return nil, errors.New("qoder login response is missing token")
	}
	c.RefreshToken = authText(data, "refresh_token", "refreshToken")
	c.UserID = authIdentity(data, "user_id", "uid", "userId")
	c.ExpiresAt = authTimestamp(data, lifetimeSeconds)
	return s.CompleteIdentity(ctx, c)
}
