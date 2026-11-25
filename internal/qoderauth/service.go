package qoderauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"ccLoad/internal/util"
)

/**
 * @brief Identifies a temporary control-plane transport failure before an HTTP response.
 */
var errAuthRetryableTransport = errors.New("qoder transport failed")

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
type HTTPError struct {
	StatusCode int
	retryAfter time.Time
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Qoder request failed (HTTP %d)", e.StatusCode)
}

/**
 * @brief 返回真实 HTTP 限流或服务端错误给出的恢复时间。
 * @return 有效的未来 Retry-After 截止时间，其他错误返回零值。
 */
func (e *HTTPError) RetryAfter() time.Time { return e.retryAfter }

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

/**
 * @brief Sends a control-plane request and decodes its bounded JSON response.
 * @param ctx Request context, including cancellation.
 * @param method HTTP method.
 * @param endpoint Control-plane URL.
 * @param body Optional JSON payload.
 * @param headers Request headers, copied before sending.
 * @return Decoded JSON object, or nil when unavailable.
 * @return Status of the response, or zero when no response was accepted.
 * @return Request, response, or context error. Temporary failures before a response use errAuthRetryableTransport.
 */
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
		if errors.Is(err, context.Canceled) {
			return nil, 0, context.Canceled
		}
		if resp == nil && isRetryableAuthTransport(err) {
			return nil, 0, errAuthRetryableTransport
		}
		return nil, 0, errors.New("qoder transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, resp.StatusCode, ctx.Err()
		}
		return nil, resp.StatusCode, errors.New("read Qoder response failed")
	}
	if len(raw) > 2<<20 {
		return nil, resp.StatusCode, errors.New("qoder response exceeds size limit")
	}
	var data map[string]json.RawMessage
	decodeErr := json.Unmarshal(raw, &data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &HTTPError{StatusCode: resp.StatusCode}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode < 600 {
			if until, ok := util.ParseRetryAfter(resp.Header, time.Now()); ok {
				failure.retryAfter = until
			}
		}
		return data, resp.StatusCode, failure
	}
	if decodeErr != nil || data == nil {
		return nil, resp.StatusCode, errors.New("invalid Qoder JSON response")
	}
	return data, resp.StatusCode, nil
}

/**
 * @brief Recognizes temporary failures from the HTTP client before it receives a response.
 * @param err HTTP client error after the caller's context and redirect response have been checked.
 * @return Whether the failure indicates a timeout, temporary name resolution error, or connection failure.
 */
func isRetryableAuthTransport(err error) bool {
	if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
		return true
	}
	if dns, ok := errors.AsType[*net.DNSError](err); ok {
		return dns.IsTemporary
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if network, ok := errors.AsType[*net.OpError](err); ok {
		if call, ok := errors.AsType[*os.SyscallError](network.Err); ok && !errors.Is(call, os.ErrPermission) {
			switch call.Syscall {
			case "connect", "read", "write", "connectex", "wsarecv", "wsasend":
				return true
			}
		}
	}
	return false
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

/**
 * @brief Preserves string and numeric account IDs without rounding large numbers.
 * @param data Device poll, refresh, or user profile response fields.
 * @param keys Account ID fields in precedence order.
 * @return First usable account ID, or empty if the response provides none.
 */
func authIdentity(data map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := authText(data, key); value != "" {
			return value
		}
		var number json.Number
		if json.Unmarshal(data[key], &number) == nil && number != "" {
			return number.String()
		}
	}
	return ""
}

/**
 * @brief Relative expiry formats used by Qoder authentication endpoints.
 */
type lifetimeFormat uint8

/**
 * @brief Selects the interpretation of an endpoint's expires_in field.
 */
const (
	lifetimeSeconds       lifetimeFormat = iota ///< Device login uses seconds.
	lifetimeMilliseconds                        ///< Job exchange and refresh use milliseconds.
	lifetimeDeviceRefresh                       ///< Device refresh also reports milliseconds.
)

/**
 * @brief Reads upstream absolute expiry first, then the endpoint's relative expiry.
 * @param data Qoder authentication response fields.
 * @param format Interpretation of expires_in for the responding endpoint.
 * @return Expiry in Unix seconds, or zero when the response provides no usable expiry.
 */
func authTimestamp(data map[string]json.RawMessage, format lifetimeFormat) int64 {
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		if value, ok := authNumber(data[key]); ok && value > 0 {
			if value >= 1_000_000_000_000 {
				value /= 1000
			}
			if value < math.MaxInt64 {
				return int64(value)
			}
		}
		if value, err := time.Parse(time.RFC3339Nano, authText(data, key)); err == nil && value.Unix() > 0 {
			return value.Unix()
		}
	}
	if value, ok := authNumber(data["expires_in"]); ok && value > 0 {
		switch format {
		case lifetimeDeviceRefresh:
			// A measured device refresh reported nearly 30 days in milliseconds.
			// Retain seconds within that lifetime and treat values above both ranges as unknown.
			const deviceLifetimeSeconds = 30 * 24 * 60 * 60
			if value > deviceLifetimeSeconds*1000 {
				return 0
			}
			if value > deviceLifetimeSeconds {
				value *= time.Millisecond.Seconds()
			}
		case lifetimeMilliseconds:
			value *= time.Millisecond.Seconds()
		}
		expiry := float64(time.Now().Unix()) + value
		if expiry < math.MaxInt64 {
			return int64(expiry)
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

// ExchangePAT exchanges a personal access token and verifies its owner.
/**
 * @param region The explicit cn or global edition.
 * @param pat The personal token retained for subsequent job-token recovery.
 */
func (s *Service) ExchangePAT(ctx context.Context, region, pat string) (*Credential, error) {
	c := &Credential{Region: region, PAT: pat}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return s.exchangePAT(ctx, c)
}

/**
 * @brief exchangePAT replaces a normalized credential's token pair and verifies its owner.
 */
func (s *Service) exchangePAT(ctx context.Context, c *Credential) (*Credential, error) {
	data, _, err := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, "/api/v1/jobToken/exchange"), map[string]string{"personal_token": c.PAT}, authHeaders(nil))
	if err != nil {
		return nil, err
	}
	c.AccessToken = authText(data, "token", "device_token", "access_token")
	if c.AccessToken == "" {
		return nil, errors.New("qoder exchange response is missing token")
	}
	c.RefreshToken = authText(data, "refresh_token", "refreshToken")
	c.ExpiresAt = authTimestamp(data, lifetimeMilliseconds)
	return s.CompleteIdentity(ctx, c)
}

/**
 * @brief Fetches the token owner and rejects a mismatch with the credential identity.
 * @param ctx Request context, including cancellation.
 * @param credential Credential to verify without changing the caller's copy.
 * @return Credential with the verified identity, or nil on failure.
 * @return Validation, profile request, or identity mismatch error.
 */
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
	userID := authIdentity(data, "id", "user_id", "userId", "uid")
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

/**
 * @brief Rotates tokens and checks any returned account ID against the credential owner.
 * @param ctx Request context, including cancellation.
 * @param credential Original credential, which is unchanged on all outcomes.
 * @return Credential with updated tokens, or nil on failure.
 * @return ErrCannotRefresh if no refresh token or PAT is available, or an error from validation, refresh, PAT exchange, or identity verification.
 * @details A job refresh rejected with HTTP 401 or 403 uses the stored PAT when available.
 */
func (s *Service) Refresh(ctx context.Context, credential *Credential) (*Credential, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		if c.PAT == "" {
			return nil, ErrCannotRefresh
		}
		return s.exchangePAT(ctx, c)
	}
	path := "/api/v1/deviceToken/refresh"
	format := lifetimeDeviceRefresh
	if strings.HasPrefix(c.RefreshToken, "jrt-") {
		path = "/api/v1/jobToken/refresh"
		format = lifetimeMilliseconds
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "Qoder")
	data, status, err := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, path), map[string]string{"refresh_token": c.RefreshToken}, headers)
	if err != nil {
		if path == "/api/v1/jobToken/refresh" && c.PAT != "" && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			return s.exchangePAT(ctx, c)
		}
		return nil, err
	}
	token := authText(data, "device_token", "deviceToken", "token", "access_token", "accessToken")
	if token == "" {
		return nil, errors.New("qoder refresh response is missing token")
	}
	if userID := authIdentity(data, "user_id", "userId", "uid"); userID != "" {
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
	c.ExpiresAt = authTimestamp(data, format)
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	if c.UserID == "" {
		return s.CompleteIdentity(ctx, c)
	}
	return c, nil
}
