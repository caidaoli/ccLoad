package qoderauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Service performs Qoder requests. BaseURL overrides the control-plane origin
// for tests. RiskHeaders supplies official runtime-generated machine identity.
type Service struct {
	Client      *http.Client
	BaseURL     string
	RiskHeaders func(context.Context, *Credential) (http.Header, error)
}

// NewService creates a control-plane client with a bounded default timeout.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{Client: client}
}

// HTTPError reports upstream HTTP status without exposing response credentials.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Qoder request failed (HTTP %d)", e.StatusCode)
}

func (s *Service) authEndpoint(c *Credential, path string) string {
	origin := c.OpenAPIURL()
	if s.BaseURL != "" {
		origin = strings.TrimRight(s.BaseURL, "/")
	}
	return origin + path
}

func authHeaders(c *Credential) http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "Qoder")
	headers.Set("Cosy-Version", "1.0.1")
	headers.Set("Cosy-ClientType", "5")
	if c != nil && c.AccessToken != "" {
		headers.Set("Authorization", "Bearer "+c.AccessToken)
	}
	return headers
}

func (s *Service) authDo(ctx context.Context, method, endpoint string, body any, headers http.Header) (map[string]json.RawMessage, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode Qoder request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, errors.New("invalid Qoder request")
	}
	req.Header = headers.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, errors.New("qoder transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("read Qoder response failed")
	}
	if len(raw) > 2<<20 {
		return nil, resp.StatusCode, errors.New("qoder response exceeds size limit")
	}
	var data map[string]json.RawMessage
	decodeErr := json.Unmarshal(raw, &data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode}
	}
	if decodeErr != nil || data == nil {
		return nil, resp.StatusCode, errors.New("invalid Qoder JSON response")
	}
	return data, resp.StatusCode, nil
}

func authText(data map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(data[key], &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func authTimestamp(data map[string]json.RawMessage) int64 {
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		var value int64
		if json.Unmarshal(data[key], &value) == nil && value > 0 {
			if value >= 1_000_000_000_000 {
				value /= 1000
			}
			return value
		}
	}
	return 0
}

func authCopy(c *Credential) (*Credential, error) {
	if c == nil {
		return nil, errors.New("missing Qoder credential")
	}
	copy := *c
	if err := copy.Normalize(); err != nil {
		return nil, err
	}
	return &copy, nil
}

// ExchangePAT exchanges a personal access token and validates the account identity.
func (s *Service) ExchangePAT(ctx context.Context, region, pat string) (*Credential, error) {
	c := &Credential{Region: region, PAT: pat}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	data, _, err := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, "/api/v1/jobToken/exchange"), map[string]string{"personal_token": c.PAT}, authHeaders(nil))
	if err != nil {
		return nil, err
	}
	c.AccessToken = authText(data, "token")
	if c.AccessToken == "" {
		return nil, errors.New("qoder exchange response is missing token")
	}
	c.RefreshToken = authText(data, "refresh_token")
	c.ExpiresAt = authTimestamp(data)
	return s.CompleteIdentity(ctx, c)
}

// CompleteIdentity fetches the token owner and rejects mismatched imported IDs.
func (s *Service) CompleteIdentity(ctx context.Context, credential *Credential) (*Credential, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.AccessToken == "" {
		return nil, errors.New("qoder access_token is required")
	}
	data, _, err := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, "/api/v1/userinfo"), nil, authHeaders(c))
	if err != nil {
		return nil, err
	}
	userID := authText(data, "id", "user_id", "userId")
	if userID == "" {
		return nil, errors.New("qoder profile is missing user identity")
	}
	if c.UserID != "" && c.UserID != userID {
		return nil, errors.New("qoder token owner does not match credential user_id")
	}
	c.UserID = userID
	if name := authText(data, "name", "username"); name != "" {
		c.Name = name
	}
	if email := authText(data, "email"); email != "" {
		c.Email = email
	}
	if c.MachineID == "" {
		c.MachineID, err = authRandom(32)
		if err != nil {
			return nil, err
		}
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Refresh rotates a device or job token. The input remains unchanged on failure.
func (s *Service) Refresh(ctx context.Context, credential *Credential) (*Credential, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		if c.PAT == "" {
			return nil, ErrCannotRefresh
		}
		fresh, exchangeErr := s.ExchangePAT(ctx, c.Region, c.PAT)
		if exchangeErr != nil {
			return nil, exchangeErr
		}
		if c.UserID != "" && c.UserID != fresh.UserID {
			return nil, errors.New("qoder refreshed token owner does not match credential")
		}
		fresh.MachineID, fresh.OAuthUsage = c.MachineID, c.OAuthUsage
		fresh.LastCheckinCampaign = c.LastCheckinCampaign
		fresh.LastCheckinAt = c.LastCheckinAt
		if fresh.MachineID == "" {
			fresh.MachineID, err = authRandom(32)
		}
		return fresh, err
	}
	path := "/api/v1/deviceToken/refresh"
	if strings.HasPrefix(c.RefreshToken, "jrt-") {
		path = "/api/v1/jobToken/refresh"
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "Qoder")
	data, _, err := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, path), map[string]string{"refresh_token": c.RefreshToken}, headers)
	if err != nil {
		return nil, err
	}
	token := authText(data, "device_token", "deviceToken", "token", "access_token")
	if token == "" {
		return nil, errors.New("qoder refresh response is missing token")
	}
	if userID := authText(data, "user_id", "userId", "uid"); userID != "" {
		if c.UserID != "" && userID != c.UserID {
			return nil, errors.New("qoder refreshed token owner does not match credential")
		}
		c.UserID = userID
	}
	c.AccessToken = token
	if refresh := authText(data, "refresh_token", "refreshToken"); refresh != "" {
		c.RefreshToken = refresh
	}
	// An omitted expiry must not leave the old, already-expired deadline behind.
	c.ExpiresAt = authTimestamp(data)
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	if c.UserID == "" {
		return s.CompleteIdentity(ctx, c)
	}
	return c, nil
}
