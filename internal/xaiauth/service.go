package xaiauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ccLoad/internal/util"
)

var (
	// ErrAccessDenied and ErrDeviceExpired identify terminal RFC 8628 polling outcomes.
	ErrAccessDenied = errors.New("xAI device authorization denied")
	// ErrDeviceExpired reports that a device code expired before authorization completed.
	ErrDeviceExpired = errors.New("xAI device code expired")
)

// Service performs xAI OAuth operations against fixed trusted endpoints.
type Service struct{ client *http.Client }

type tokenEndpointError struct {
	statusCode   int
	code         string
	responseBody string
	retryAfter   time.Time
}

func (e *tokenEndpointError) Error() string {
	if e == nil {
		return "xAI token endpoint request failed"
	}
	return fmt.Sprintf("xAI token endpoint returned HTTP %d (%s)", e.statusCode, e.code)
}

// UpstreamResponseBody returns the bounded response body for an explicitly authorized caller.
func (e *tokenEndpointError) UpstreamResponseBody() string {
	if e == nil {
		return ""
	}
	return e.responseBody
}

// StatusCode exposes the token endpoint status without exposing the private error type.
func (e *tokenEndpointError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

/**
 * @brief 返回令牌端点给出的恢复时间。
 * @return Retry-After 截止时间；缺失或无法解析时为零。
 */
func (e *tokenEndpointError) RetryAfter() time.Time { return e.retryAfter }

// NewService constructs a Service using client or http.DefaultClient when nil.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{client: client}
}

// Authorization contains one locally generated xAI PKCE flow. Secret PKCE
// material is deliberately excluded from JSON so it cannot reach the browser.
type Authorization struct {
	URL           string `json:"url"`
	State         string `json:"state"`
	Nonce         string `json:"-"`
	CodeVerifier  string `json:"-"`
	CodeChallenge string `json:"-"`
}

// AuthorizationInput is a parsed callback URL, query string, or bare code.
type AuthorizationInput struct {
	Code          string
	State         string
	RequiresState bool
}

// NewAuthorization generates the complete browser URL locally without making
// an upstream request. xAI's public CLI client uses PKCE and a fixed loopback URI.
func (s *Service) NewAuthorization() (*Authorization, error) {
	state, err := randomURLToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate xAI OAuth state: %w", err)
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate xAI OAuth nonce: %w", err)
	}
	verifier, err := randomURLToken(64)
	if err != nil {
		return nil, fmt.Errorf("generate xAI PKCE verifier: %w", err)
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"redirect_uri":          {RedirectURI},
		"scope":                 {OAuthScope},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"plan":                  {"generic"},
		"referrer":              {"ccload"},
	}
	return &Authorization{
		URL: AuthorizeURL + "?" + query.Encode(), State: state, Nonce: nonce,
		CodeVerifier: verifier, CodeChallenge: challenge,
	}, nil
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseAuthorizationInput accepts the same manual inputs as Sub2API: a full
// callback URL, a query string, or a bare authorization code.
func ParseAuthorizationInput(raw string) AuthorizationInput {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return AuthorizationInput{}
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed != nil {
		if code := strings.TrimSpace(parsed.Query().Get("code")); code != "" {
			return AuthorizationInput{Code: code, State: strings.TrimSpace(parsed.Query().Get("state")), RequiresState: true}
		}
	}
	queryCandidate := strings.TrimPrefix(trimmed, "?")
	if strings.Contains(queryCandidate, "=") {
		if values, err := url.ParseQuery(queryCandidate); err == nil {
			if code := strings.TrimSpace(values.Get("code")); code != "" {
				return AuthorizationInput{Code: code, State: strings.TrimSpace(values.Get("state")), RequiresState: true}
			}
		}
	}
	return AuthorizationInput{Code: trimmed}
}

// ExchangeCode exchanges one authorization code using the server-held PKCE verifier.
func (s *Service) ExchangeCode(ctx context.Context, code, verifier string) (*Credential, error) {
	code = strings.TrimSpace(code)
	verifier = strings.TrimSpace(verifier)
	if code == "" {
		return nil, errors.New("xAI authorization code is required")
	}
	if verifier == "" {
		return nil, errors.New("xAI PKCE verifier is required")
	}
	credential, _, err := s.requestToken(ctx, TokenURL, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {ClientID},
		"code": {code}, "redirect_uri": {RedirectURI}, "code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	credential.TokenEndpoint = TokenURL
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
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
	body, status, retryAfter, err := s.request(ctx, http.MethodPost, endpoint, form, maxOAuthResponseBytes)
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
	decodeErr := json.Unmarshal(body, &token)
	code := ""
	if decodeErr == nil {
		code = safeOAuthErrorCode(token.Error)
	}
	if status < 200 || status >= 300 || code != "" {
		if code == "" {
			code = "http_error"
		}
		return nil, code, &tokenEndpointError{statusCode: status, code: code, responseBody: string(body), retryAfter: retryAfter}
	}
	if decodeErr != nil {
		return nil, "", errors.New("decode xAI token response")
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

/**
 * @brief 执行 xAI OAuth HTTP 请求，读取有界的响应正文及恢复时间。
 * @param ctx 请求生命周期。
 * @param method HTTP 方法。
 * @param endpoint 受信任的 OAuth 端点。
 * @param form 表单内容；nil 表示无正文。
 * @param limit 响应正文大小上限，单位字节。
 * @return 响应正文；读取失败时为 nil。
 * @return HTTP 状态；未收到响应时为零。
 * @return Retry-After 截止时间；缺失或无法解析时为零。
 * @return 请求构造、发送、读取或响应体超限错误。
 */
func (s *Service) request(ctx context.Context, method, endpoint string, form url.Values, limit int64) ([]byte, int, time.Time, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return nil, 0, time.Time{}, errors.New("build xAI request")
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, time.Time{}, safeRequestError(ctx, "xAI request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, time.Time{}, errors.New("read xAI response")
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, time.Time{}, errors.New("xAI response exceeds size limit")
	}
	retryAfter, _ := util.ParseRetryAfter(resp.Header, time.Now())
	return data, resp.StatusCode, retryAfter, nil
}

// safeRequestError keeps transports from reflecting credentials, request
// bodies, or redirect URLs while preserving cancellation semantics.
func safeRequestError(ctx context.Context, message string, err error) error {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("%s: %w", message, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("%s: network timeout", message)
	}
	return errors.New(message)
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
