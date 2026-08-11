// Package anthropicauth implements Anthropic's public Claude Code OAuth flow.
package anthropicauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClientID is Anthropic's public Claude Code OAuth client identifier.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// AuthorizationURL starts Anthropic's hosted authorization flow.
	AuthorizationURL = "https://claude.com/cai/oauth/authorize"
	// TokenURL exchanges authorization codes and refresh tokens.
	TokenURL = "https://platform.claude.com/v1/oauth/token"
	// RedirectURI is Anthropic's hosted callback page where users copy the code.
	RedirectURI = "https://platform.claude.com/oauth/code/callback"
	// Scope is the full Claude Code OAuth permission set.
	Scope = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// CookieScope is the permission set accepted by claude.ai's internal OAuth authorization endpoint.
	CookieScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// ClaudeWebURL is the origin used to exchange a claude.ai sessionKey for an authorization code.
	ClaudeWebURL = "https://claude.ai"
	// DefaultUpstreamURL is Anthropic's public Messages API origin.
	DefaultUpstreamURL   = "https://api.anthropic.com"
	defaultTokenTimeout  = 60 * time.Second
	maxTokenResponseSize = 1 << 20
	cookieBrowserUA      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

// PKCE is one S256 verifier/challenge pair.
type PKCE struct {
	Verifier  string
	Challenge string
}

// Service exchanges Anthropic authorization codes and refresh tokens.
type Service struct {
	Client           *http.Client
	AuthorizationURL string
	ClaudeWebURL     string
	TokenURL         string
	ClientID         string
	RedirectURI      string
	Scope            string
	Now              func() time.Time
}

type upstreamResponseError struct {
	operation    string
	statusCode   int
	responseBody string
}

func (e *upstreamResponseError) Error() string {
	if e == nil {
		return "anthropic upstream request failed"
	}
	return fmt.Sprintf("anthropic %s returned HTTP %d", e.operation, e.statusCode)
}

// UpstreamResponseBody returns the bounded response body for an explicitly authorized caller.
func (e *upstreamResponseError) UpstreamResponseBody() string {
	if e == nil {
		return ""
	}
	return e.responseBody
}

// StatusCode exposes the upstream status without exposing the private error type.
func (e *upstreamResponseError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

// NewService returns the production Anthropic OAuth service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, AuthorizationURL: AuthorizationURL, ClaudeWebURL: ClaudeWebURL, TokenURL: TokenURL,
		ClientID: ClientID, RedirectURI: RedirectURI, Scope: Scope, Now: time.Now,
	}
}

// GenerateState returns an unguessable OAuth state value.
func GenerateState() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate Anthropic OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// GeneratePKCE returns a high-entropy S256 verifier/challenge pair.
func GeneratePKCE() (PKCE, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return PKCE{}, fmt.Errorf("generate Anthropic PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(digest[:])}, nil
}

// AuthorizationLink builds the hosted Anthropic authorization URL.
func (s *Service) AuthorizationLink(state string, pkce PKCE) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(state) == "" {
		return "", errors.New("anthropic OAuth state is required")
	}
	if pkce.Verifier == "" || pkce.Challenge == "" {
		return "", errors.New("anthropic PKCE verifier and challenge are required")
	}
	parsed, err := url.Parse(s.AuthorizationURL)
	if err != nil {
		return "", fmt.Errorf("parse Anthropic authorization URL: %w", err)
	}
	query := parsed.Query()
	query.Set("code", "true")
	query.Set("client_id", s.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", s.RedirectURI)
	query.Set("scope", s.Scope)
	query.Set("code_challenge", pkce.Challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ExchangeCode exchanges one hosted authorization code for a credential.
func (s *Service) ExchangeCode(ctx context.Context, code, state string, pkce PKCE) (*Credential, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("anthropic authorization code is required")
	}
	if strings.TrimSpace(state) == "" || pkce.Verifier == "" {
		return nil, errors.New("anthropic OAuth state and PKCE verifier are required")
	}
	credential, err := s.requestToken(ctx, map[string]string{
		"code": code, "grant_type": "authorization_code", "client_id": s.ClientID,
		"redirect_uri": s.RedirectURI, "code_verifier": pkce.Verifier, "state": state,
	})
	if err != nil {
		return nil, err
	}
	if credential.RefreshToken == "" {
		return nil, errors.New("anthropic token response is missing refresh_token")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

// CookieAuth exchanges a claude.ai sessionKey for a regular rotating OAuth credential.
// The sessionKey is used only for the two claude.ai requests and is never returned or persisted.
func (s *Service) CookieAuth(ctx context.Context, sessionKey string) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.ClaudeWebURL) == "" {
		return nil, errors.New("anthropic Cookie authorization is unavailable")
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil, errors.New("anthropic sessionKey is required")
	}
	if strings.ContainsAny(sessionKey, "\r\n") {
		return nil, errors.New("anthropic sessionKey is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultTokenTimeout)
	defer cancel()

	client := *s.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	orgUUID, err := s.cookieOrganizationUUID(requestCtx, &client, sessionKey)
	if err != nil {
		return nil, annotateCookieAuthError(err, sessionKey)
	}
	state, err := GenerateState()
	if err != nil {
		return nil, err
	}
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	code, err := s.cookieAuthorizationCode(requestCtx, &client, sessionKey, orgUUID, state, pkce.Challenge)
	if err != nil {
		return nil, annotateCookieAuthError(err, sessionKey)
	}
	credential, err := s.ExchangeCode(requestCtx, code, state, pkce)
	if err != nil {
		return nil, annotateCookieAuthError(err, sessionKey)
	}
	if credential.OrgUUID == "" {
		credential.OrgUUID = orgUUID
	}
	return credential, nil
}

// Refresh exchanges a refresh token for the next rotating credential.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Credential, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("anthropic refresh token is required")
	}
	return s.requestToken(ctx, map[string]string{
		"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": s.ClientID,
	})
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil || strings.TrimSpace(s.AuthorizationURL) == "" ||
		strings.TrimSpace(s.TokenURL) == "" || strings.TrimSpace(s.ClientID) == "" ||
		strings.TrimSpace(s.RedirectURI) == "" || strings.TrimSpace(s.Scope) == "" {
		return errors.New("anthropic OAuth service is unavailable")
	}
	return nil
}

func (s *Service) cookieOrganizationUUID(ctx context.Context, client *http.Client, sessionKey string) (string, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(s.ClaudeWebURL, "/")+"/api/organizations", nil,
	)
	if err != nil {
		return "", fmt.Errorf("build Anthropic organization request: %w", err)
	}
	request.AddCookie(&http.Cookie{Name: "sessionKey", Value: sessionKey})
	setCookieBrowserHeaders(request)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("anthropic organization request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", newUpstreamResponseError(response, "organization endpoint")
	}
	var organizations []struct {
		UUID      string  `json:"uuid"`
		RavenType *string `json:"raven_type"`
	}
	if err := decodeLimitedJSON(response.Body, &organizations); err != nil {
		return "", fmt.Errorf("decode Anthropic organization response: %w", err)
	}
	if len(organizations) == 0 {
		return "", errors.New("anthropic account has no organization")
	}
	selected := strings.TrimSpace(organizations[0].UUID)
	for _, organization := range organizations {
		if organization.RavenType != nil && *organization.RavenType == "team" {
			selected = strings.TrimSpace(organization.UUID)
			break
		}
	}
	if selected == "" {
		return "", errors.New("anthropic organization response is missing uuid")
	}
	return selected, nil
}

func (s *Service) cookieAuthorizationCode(
	ctx context.Context,
	client *http.Client,
	sessionKey, orgUUID, state, challenge string,
) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"response_type": "code", "client_id": s.ClientID, "organization_uuid": orgUUID,
		"redirect_uri": s.RedirectURI, "scope": CookieScope, "state": state,
		"code_challenge": challenge, "code_challenge_method": "S256",
	})
	if err != nil {
		return "", fmt.Errorf("encode Anthropic Cookie authorization request: %w", err)
	}
	targetURL := strings.TrimRight(s.ClaudeWebURL, "/") + "/v1/oauth/" + url.PathEscape(orgUUID) + "/authorize"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build Anthropic Cookie authorization request: %w", err)
	}
	request.AddCookie(&http.Cookie{Name: "sessionKey", Value: sessionKey})
	setCookieBrowserHeaders(request)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Origin", "https://claude.ai")
	request.Header.Set("Referer", "https://claude.ai/new")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("anthropic Cookie authorization request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", newUpstreamResponseError(response, "Cookie authorization endpoint")
	}
	var result struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := decodeLimitedJSON(response.Body, &result); err != nil {
		return "", fmt.Errorf("decode Anthropic Cookie authorization response: %w", err)
	}
	redirect, err := url.Parse(strings.TrimSpace(result.RedirectURI))
	if err != nil || redirect == nil {
		return "", errors.New("anthropic Cookie authorization response has an invalid redirect_uri")
	}
	code := strings.TrimSpace(redirect.Query().Get("code"))
	if code == "" {
		return "", errors.New("anthropic Cookie authorization response is missing code")
	}
	if responseState := strings.TrimSpace(redirect.Query().Get("state")); responseState != "" && responseState != state {
		return "", errors.New("anthropic Cookie authorization state mismatch")
	}
	return code, nil
}

func setCookieBrowserHeaders(request *http.Request) {
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Sec-CH-UA", `"Not_A Brand";v="8", "Chromium";v="133", "Google Chrome";v="133"`)
	request.Header.Set("Sec-CH-UA-Mobile", "?0")
	request.Header.Set("Sec-CH-UA-Platform", `"macOS"`)
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	request.Header.Set("User-Agent", cookieBrowserUA)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml,application/json;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	request.Header.Set("Sec-Fetch-Site", "none")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-User", "?1")
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
}

func decodeLimitedJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxTokenResponseSize))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

type upstreamResponseBodyError interface {
	error
	UpstreamResponseBody() string
}

func newUpstreamResponseError(response *http.Response, operation string) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseSize))
	if err != nil {
		return fmt.Errorf("read Anthropic %s response: %w", operation, err)
	}
	return &upstreamResponseError{
		operation: operation, statusCode: response.StatusCode, responseBody: string(body),
	}
}

func annotateCookieAuthError(err error, sessionKey string) error {
	var upstreamErr upstreamResponseBodyError
	if !errors.As(err, &upstreamErr) {
		return err
	}
	body := strings.TrimSpace(upstreamErr.UpstreamResponseBody())
	if body == "" {
		return err
	}
	if sessionKey != "" {
		body = sanitizeCookieAuthUpstreamBody(body, sessionKey)
	}
	return fmt.Errorf("%w: %s", err, body)
}

func sanitizeCookieAuthUpstreamBody(body, sessionKey string) string {
	candidates := []string{body}
	decoder := json.NewDecoder(strings.NewReader(body))
	sawJSONToken := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if sawJSONToken || strings.Contains(body, `\u`) || strings.Contains(body, `\/`) {
				return "[REDACTED]"
			}
			break
		}
		sawJSONToken = true
		if value, ok := token.(string); ok {
			candidates = append(candidates, value)
		}
	}
	for _, candidate := range candidates {
		if containsCookieAuthSecret(candidate, sessionKey, 3) {
			return "[REDACTED]"
		}
	}
	return body
}

func containsCookieAuthSecret(value, sessionKey string, decodeDepth int) bool {
	if strings.Contains(value, sessionKey) {
		return true
	}
	if decodeDepth == 0 {
		return false
	}
	if decoded := html.UnescapeString(value); decoded != value &&
		containsCookieAuthSecret(decoded, sessionKey, decodeDepth-1) {
		return true
	}
	if decoded, err := url.PathUnescape(value); err == nil && decoded != value &&
		containsCookieAuthSecret(decoded, sessionKey, decodeDepth-1) {
		return true
	}
	if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
		return containsCookieAuthSecret(decoded, sessionKey, decodeDepth-1)
	}
	return false
}

func (s *Service) requestToken(ctx context.Context, payload map[string]string) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic token request: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultTokenTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.TokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Anthropic token request: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "axios/1.13.6")
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic token request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read Anthropic token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &upstreamResponseError{
			operation:    "token endpoint",
			statusCode:   response.StatusCode,
			responseBody: string(responseBody),
		}
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
		Account struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if err := json.Unmarshal(responseBody, &token); err != nil {
		return nil, fmt.Errorf("decode Anthropic token response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("anthropic token response is missing access_token")
	}
	if token.ExpiresIn <= 0 {
		return nil, errors.New("anthropic token response has invalid expires_in")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	return &Credential{
		Type: ChannelType, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		TokenType: token.TokenType, ExpiresIn: token.ExpiresIn,
		Expired:     now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339),
		LastRefresh: now.Format(time.RFC3339), Scope: token.Scope,
		OrgUUID: token.Organization.UUID, AccountUUID: token.Account.UUID,
		EmailAddress: token.Account.EmailAddress,
	}, nil
}
