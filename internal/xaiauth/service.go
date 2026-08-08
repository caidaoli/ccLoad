package xaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrAccessDenied and ErrDeviceExpired identify terminal RFC 8628 polling outcomes.
	ErrAccessDenied = errors.New("xAI device authorization denied")
	// ErrDeviceExpired reports that a device code expired before authorization completed.
	ErrDeviceExpired = errors.New("xAI device code expired")
)

// Service performs xAI OAuth operations against fixed trusted endpoints.
type Service struct{ client *http.Client }

// NewService constructs a Service using client or http.DefaultClient when nil.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{client: client}
}

// Discovery contains the trusted OAuth endpoints returned by xAI discovery.
type Discovery struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// DeviceCode contains the server-issued state for an RFC 8628 device flow.
type DeviceCode struct {
	DeviceCode              string        `json:"-"`
	UserCode                string        `json:"user_code"`
	VerificationURI         string        `json:"verification_uri"`
	VerificationURIComplete string        `json:"verification_uri_complete"`
	ExpiresIn               int           `json:"expires_in"`
	ExpiresAt               time.Time     `json:"expires_at"`
	Interval                time.Duration `json:"-"`
	TokenEndpoint           string        `json:"-"`
}

// Redacted returns a diagnostic representation without the device secret.
func (d *DeviceCode) Redacted() string {
	if d == nil {
		return "<nil>"
	}
	return fmt.Sprintf("xAI device code{user_code=%q,verification_uri=%q,expires_in=%d}", d.UserCode, d.VerificationURI, d.ExpiresIn)
}

func (d *DeviceCode) String() string { return d.Redacted() }

// Discover loads xAI discovery and enforces the fixed trusted origin.
func (s *Service) Discover(ctx context.Context) (*Discovery, error) {
	var discovery Discovery
	if err := s.doJSON(ctx, http.MethodGet, DiscoveryURL, nil, maxOAuthResponseBytes, &discovery); err != nil {
		return nil, fmt.Errorf("xAI discovery: %w", err)
	}
	device, err := validateAuthURL(discovery.DeviceAuthorizationEndpoint)
	if err != nil {
		return nil, fmt.Errorf("xAI discovery device endpoint origin: %w", err)
	}
	token, err := validateAuthURL(discovery.TokenEndpoint)
	if err != nil {
		return nil, fmt.Errorf("xAI discovery token endpoint origin: %w", err)
	}
	discovery.DeviceAuthorizationEndpoint, discovery.TokenEndpoint = device, token
	return &discovery, nil
}

// StartDevice starts an xAI RFC 8628 device authorization flow.
func (s *Service) StartDevice(ctx context.Context) (*DeviceCode, error) {
	discovery, err := s.Discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{"client_id": {ClientID}, "scope": {DeviceScope}}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := s.doJSON(ctx, http.MethodPost, discovery.DeviceAuthorizationEndpoint, form, maxOAuthResponseBytes, &payload); err != nil {
		return nil, fmt.Errorf("xAI device code: %w", err)
	}
	receivedAt := time.Now().UTC()
	if strings.TrimSpace(payload.DeviceCode) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return nil, errors.New("xAI device code response is incomplete")
	}
	if payload.ExpiresIn <= 0 {
		return nil, errors.New("xAI device code response has invalid expires_in")
	}
	verification, complete := "", ""
	if strings.TrimSpace(payload.VerificationURI) != "" {
		verification, err = validateAuthURL(payload.VerificationURI)
		if err != nil {
			return nil, fmt.Errorf("xAI verification URL origin: %w", err)
		}
	}
	if strings.TrimSpace(payload.VerificationURIComplete) != "" {
		complete, err = validateAuthURL(payload.VerificationURIComplete)
		if err != nil {
			return nil, fmt.Errorf("xAI complete verification URL origin: %w", err)
		}
	}
	if verification == "" && complete == "" {
		return nil, errors.New("xAI device code response is missing verification URL")
	}
	interval := time.Duration(payload.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return &DeviceCode{
		DeviceCode: payload.DeviceCode, UserCode: payload.UserCode,
		VerificationURI: verification, VerificationURIComplete: complete,
		ExpiresIn: payload.ExpiresIn, ExpiresAt: receivedAt.Add(time.Duration(payload.ExpiresIn) * time.Second),
		Interval: interval, TokenEndpoint: discovery.TokenEndpoint,
	}, nil
}

// PollDevice waits for a device flow and accepts a nil context as Background.
func (s *Service) PollDevice(ctx context.Context, device *DeviceCode) (*Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if device == nil || strings.TrimSpace(device.DeviceCode) == "" {
		return nil, errors.New("xAI device code is required")
	}
	endpoint, err := validateAuthURL(device.TokenEndpoint)
	if err != nil {
		return nil, fmt.Errorf("xAI token endpoint origin: %w", err)
	}
	interval := device.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if device.ExpiresAt.IsZero() {
		return nil, errors.New("xAI device code is missing expires_at")
	}
	deadline := device.ExpiresAt
	hardDeadline := time.Now().Add(MaxDevicePollDuration)
	if hardDeadline.Before(deadline) {
		deadline = hardDeadline
	}
	parentDeadline, parentHasDeadline := ctx.Deadline()
	parentOwnsDeadline := parentHasDeadline && parentDeadline.Before(deadline)
	if parentOwnsDeadline {
		deadline = parentDeadline
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	first := true
	for {
		if !first {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, pollDeadlineError(pollCtx, parentOwnsDeadline)
			}
			wait := interval
			if wait > remaining {
				wait = remaining
			}
			if err := sleepContext(pollCtx, wait); err != nil {
				return nil, pollDeadlineError(pollCtx, parentOwnsDeadline)
			}
		}
		first = false
		if !time.Now().Before(deadline) {
			return nil, pollDeadlineError(pollCtx, parentOwnsDeadline)
		}
		credential, code, err := s.requestToken(pollCtx, endpoint, url.Values{"grant_type": {DeviceCodeGrantType}, "device_code": {strings.TrimSpace(device.DeviceCode)}, "client_id": {ClientID}})
		if err == nil {
			credential.TokenEndpoint = endpoint
			if err := credential.Normalize(); err != nil {
				return nil, err
			}
			return credential, nil
		}
		if pollCtx.Err() != nil {
			return nil, pollDeadlineError(pollCtx, parentOwnsDeadline)
		}
		switch code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrDeviceExpired
		default:
			return nil, err
		}
	}
}

func pollDeadlineError(ctx context.Context, parentOwnsDeadline bool) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if parentOwnsDeadline {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	return ErrDeviceExpired
}

// Refresh exchanges and merges the refresh token from an existing credential.
func (s *Service) Refresh(ctx context.Context, old *Credential) (*Credential, error) {
	if old == nil || strings.TrimSpace(old.RefreshToken) == "" {
		return nil, errors.New("xAI refresh token is required")
	}
	clientID := strings.TrimSpace(old.ClientID)
	if clientID == "" {
		clientID = ClientID
	}
	endpoint := TokenURL
	if strings.TrimSpace(old.TokenEndpoint) != "" {
		var err error
		endpoint, err = validateAuthURL(old.TokenEndpoint)
		if err != nil {
			return nil, fmt.Errorf("xAI token endpoint origin: %w", err)
		}
	}
	credential, _, err := s.requestToken(ctx, endpoint, url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {strings.TrimSpace(old.RefreshToken)}})
	if err != nil {
		return nil, err
	}
	credential.ClientID = clientID
	credential.TokenEndpoint = endpoint
	return old.MergeRefresh(credential)
}

// RefreshToken exchanges an imported refresh token at xAI's fixed token
// endpoint. It is intentionally separate from Refresh because an import has no
// complete previous credential to merge yet.
func (s *Service) RefreshToken(ctx context.Context, refreshToken, clientID string) (*Credential, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("xAI refresh token is required")
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = ClientID
	}
	credential, _, err := s.requestToken(ctx, TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		credential.RefreshToken = refreshToken
	}
	credential.ClientID = clientID
	credential.TokenEndpoint = TokenURL
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

func (s *Service) requestToken(ctx context.Context, endpoint string, form url.Values) (*Credential, string, error) {
	body, status, err := s.request(ctx, http.MethodPost, endpoint, form, maxOAuthResponseBytes)
	if err != nil {
		return nil, "", err
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	if json.Unmarshal(body, &token) != nil {
		return nil, "", errors.New("decode xAI token response")
	}
	code := safeOAuthErrorCode(token.Error)
	if status < 200 || status >= 300 || code != "" {
		if code == "" {
			code = "http_error"
		}
		return nil, code, fmt.Errorf("xAI token endpoint returned HTTP %d (%s)", status, code)
	}
	if strings.TrimSpace(token.AccessToken) == "" || token.ExpiresIn <= 0 {
		return nil, "", errors.New("xAI token response is incomplete")
	}
	now := time.Now().UTC()
	credential := &Credential{Type: ChannelType, AuthKind: "oauth", AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, TokenType: token.TokenType, ExpiresIn: token.ExpiresIn, LastRefresh: now.Format(time.RFC3339), Expired: now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339), ClientID: ClientID, Scope: token.Scope}
	identity := credential.Identity()
	credential.Email, credential.Subject = identity.Email, identity.Subject
	return credential, "", nil
}

func safeOAuthErrorCode(code string) string {
	switch strings.TrimSpace(code) {
	case "authorization_pending", "slow_down", "access_denied", "expired_token", "invalid_grant", "invalid_request", "unauthorized_client":
		return strings.TrimSpace(code)
	case "":
		return ""
	default:
		return "oauth_error"
	}
}

func (s *Service) doJSON(ctx context.Context, method, endpoint string, form url.Values, limit int64, target any) error {
	body, status, err := s.request(ctx, method, endpoint, form, limit)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP %d", status)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

func (s *Service) request(ctx context.Context, method, endpoint string, form url.Values, limit int64) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return nil, 0, errors.New("build xAI request")
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("xAI request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("read xAI response")
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, errors.New("xAI response exceeds size limit")
	}
	return data, resp.StatusCode, nil
}

func validateAuthURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.x.ai" || parsed.User != nil {
		return "", errors.New("URL must use the https://auth.x.ai origin")
	}
	if parsed.Fragment != "" {
		return "", errors.New("URL fragment is not allowed")
	}
	return parsed.String(), nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
