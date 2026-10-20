package accioauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const accioAppVersion = "0.32.6"

// Service performs control-plane requests; BaseURL overrides the gateway for tests.
type Service struct {
	Client  *http.Client
	BaseURL string
}

// NewService creates a client with a bounded default timeout.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{Client: client}
}

// HTTPError reports an upstream rejection without returning its sensitive body.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return "Accio request failed: " + e.Message
}

func authCopy(c *Credential) (*Credential, error) {
	if c == nil {
		return nil, errors.New("missing Accio credential")
	}
	copy := *c
	if err := copy.Normalize(); err != nil {
		return nil, err
	}
	return &copy, nil
}

func requestHeaders(c *Credential) http.Header {
	h := make(http.Header)
	// Match the reference client's Node fetch UA; Go's default receives code 423.
	h.Set("User-Agent", "undici")
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Accept-Language", "en")
	h.Set("x-language", "en")
	h.Set("x-platform", "desktop")
	os := runtime.GOOS
	if os == "darwin" {
		os = "macos"
	}
	h.Set("x-os", os)
	h.Set("x-app-version", accioAppVersion)
	h.Set("x-package-region", strings.ToUpper(c.Region))
	return h
}

func (s *Service) authDo(ctx context.Context, c *Credential, method, path string, body any) (map[string]json.RawMessage, error) {
	base := s.BaseURL
	if base == "" {
		base = c.GatewayURL()
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid Accio request URL")
	}
	if method == http.MethodGet {
		query := u.Query()
		query.Set("accessToken", c.AccessToken)
		u.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, encodeErr := json.Marshal(body)
		if encodeErr != nil {
			return nil, errors.New("encode Accio request failed")
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errors.New("invalid Accio request")
	}
	req.Header = requestHeaders(c)
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Query credentials must never be forwarded to a redirect destination.
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("accio transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := resp.StatusCode
		// Accio uses both 401 and 403 for invalid account credentials.
		if status == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, &HTTPError{status, fmt.Sprintf("%s %s (HTTP %d): upstream rejected request", method, path, resp.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, errors.New("read Accio response failed")
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("accio response exceeds size limit")
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil || data == nil {
		return nil, errors.New("invalid Accio JSON response")
	}
	var success *bool
	_ = json.Unmarshal(data["success"], &success)
	code := authText(data, "code")
	if success != nil && !*success || code != "" && code != "0" && code != "200" {
		status := http.StatusBadGateway
		if code == "401" {
			status = http.StatusUnauthorized
		}
		if code == "403" {
			status = http.StatusUnauthorized
		}
		if code == "429" {
			status = http.StatusTooManyRequests
		}
		// Only numeric business codes are diagnostic; never expose arbitrary
		// response strings, which can contain credentials or account details.
		diagnosticCode := "unrecognized"
		if n, parseErr := strconv.Atoi(code); parseErr == nil && n >= 0 && n <= 999999 {
			diagnosticCode = strconv.Itoa(n)
		}
		return nil, &HTTPError{status, fmt.Sprintf("%s %s (HTTP %d, business code %s): upstream rejected request", method, path, resp.StatusCode, diagnosticCode)}
	}
	return data, nil
}

func authText(data map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(data[key], &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		var number json.Number
		if json.Unmarshal(data[key], &number) == nil && number.String() != "" {
			return number.String()
		}
	}
	return ""
}

func authData(data map[string]json.RawMessage) map[string]json.RawMessage {
	var inner map[string]json.RawMessage
	if json.Unmarshal(data["data"], &inner) == nil && inner != nil {
		return inner
	}
	return data
}

// CompleteIdentity validates the token owner and returns an enriched copy.
func (s *Service) CompleteIdentity(ctx context.Context, credential *Credential) (*Credential, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	data, err := s.authDo(ctx, c, http.MethodPost, "/api/auth/userinfo", map[string]string{"accessToken": c.AccessToken})
	if err != nil {
		return nil, err
	}
	data = authData(data)
	var user map[string]json.RawMessage
	if json.Unmarshal(data["user"], &user) == nil && user != nil {
		data = user
	}
	id := authText(data, "id", "userId", "user_id", "uid")
	if id == "" {
		return nil, errors.New("accio profile is missing user identity")
	}
	if c.UserID != "" && c.UserID != id {
		return nil, errors.New("accio token owner does not match credential user_id")
	}
	c.UserID = id
	if name := authText(data, "nickname", "name", "displayName", "userName"); name != "" {
		c.Name = name
	}
	if email := authText(data, "email", "emailAddress"); email != "" {
		c.Email = email
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Refresh rotates tokens without mutating the imported credential on failure.
func (s *Service) Refresh(ctx context.Context, credential *Credential) (*Credential, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		return nil, ErrCannotRefresh
	}
	data, err := s.authDo(ctx, c, http.MethodPost, "/api/auth/refresh_token", map[string]string{"accessToken": c.AccessToken, "refreshToken": c.RefreshToken})
	if err != nil {
		return nil, err
	}
	data = authData(data)
	token := authText(data, "accessToken", "access_token")
	if token == "" {
		return nil, errors.New("accio refresh response is missing accessToken")
	}
	if id := authText(data, "userId", "user_id"); id != "" {
		if c.UserID != "" && c.UserID != id {
			return nil, errors.New("accio refreshed token owner does not match credential")
		}
		c.UserID = id
	}
	c.AccessToken = token
	if refresh := authText(data, "refreshToken", "refresh_token"); refresh != "" {
		c.RefreshToken = refresh
	}
	expiry := data["expiresAt"]
	if len(expiry) == 0 {
		expiry = data["expires_at"]
	}
	c.ExpiresAt, err = authExpiry(expiry)
	if err != nil {
		return nil, err
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}
