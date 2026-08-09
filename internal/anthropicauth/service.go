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
	// DefaultUpstreamURL is Anthropic's public Messages API origin.
	DefaultUpstreamURL   = "https://api.anthropic.com"
	defaultTokenTimeout  = 60 * time.Second
	maxTokenResponseSize = 1 << 20
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
	TokenURL         string
	ClientID         string
	RedirectURI      string
	Scope            string
	Now              func() time.Time
}

// NewService returns the production Anthropic OAuth service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, AuthorizationURL: AuthorizationURL, TokenURL: TokenURL,
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
		return nil, fmt.Errorf("anthropic token endpoint returned HTTP %d", response.StatusCode)
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
