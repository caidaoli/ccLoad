package antigravityauth

import (
	"context"
	"crypto/rand"
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

// Production Antigravity OAuth endpoints and public client configuration.
const (
	DefaultAuthorizationURL = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL         = "https://oauth2.googleapis.com/token"
	DefaultUserInfoURL      = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	DefaultAPIBaseURL       = "https://cloudcode-pa.googleapis.com"
	DefaultDailyAPIBaseURL  = "https://daily-cloudcode-pa.googleapis.com"
	DefaultClientID         = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	DefaultClientSecret     = "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"
	DefaultRedirectURI      = "http://localhost:51121/oauth-callback"
	DefaultUserAgent        = "antigravity/hub/2.5.0 darwin/arm64"
	defaultRequestTimeout   = 30 * time.Second
	maxResponseBytes        = 1 << 20
	apiVersion              = "v1internal"
)

var defaultScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// Service implements the Google OAuth and project-discovery contract used by CLIProxyAPI.
type Service struct {
	Client              *http.Client
	AuthorizationURL    string
	TokenURL            string
	UserInfoURL         string
	APIBaseURL          string
	DailyAPIBaseURL     string
	ClientID            string
	ClientSecret        string
	RedirectURI         string
	UserAgent           string
	Sleep               func(context.Context, time.Duration) error
	OnboardPollAttempts int
}

// NewService returns the production Antigravity OAuth service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, AuthorizationURL: DefaultAuthorizationURL, TokenURL: DefaultTokenURL,
		UserInfoURL: DefaultUserInfoURL, APIBaseURL: DefaultAPIBaseURL, DailyAPIBaseURL: DefaultDailyAPIBaseURL,
		ClientID: DefaultClientID, ClientSecret: DefaultClientSecret, RedirectURI: DefaultRedirectURI,
		UserAgent: DefaultUserAgent, Sleep: sleepContext, OnboardPollAttempts: 5,
	}
}

// GenerateState returns an unguessable OAuth state value.
func GenerateState() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// AuthorizationLink builds the Google installed-app authorization URL.
func (s *Service) AuthorizationLink(state string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return "", errors.New("oauth state is required")
	}
	parsed, err := url.Parse(s.AuthorizationURL)
	if err != nil {
		return "", fmt.Errorf("parse authorization URL: %w", err)
	}
	query := parsed.Query()
	query.Set("access_type", "offline")
	query.Set("client_id", s.ClientID)
	query.Set("prompt", "consent")
	query.Set("redirect_uri", s.RedirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(defaultScopes, " "))
	query.Set("state", state)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ExchangeCode exchanges a code and resolves the email and Cloud Code project.
func (s *Service) ExchangeCode(ctx context.Context, code string) (*Credential, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("authorization code is required")
	}
	credential, err := s.requestToken(ctx, url.Values{
		"code":          {code},
		"client_id":     {s.ClientID},
		"client_secret": {s.ClientSecret},
		"redirect_uri":  {s.RedirectURI},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return nil, err
	}
	if credential.RefreshToken == "" {
		return nil, errors.New("token: Antigravity response is missing refresh_token")
	}
	return s.CompleteCredential(ctx, credential)
}

// Refresh exchanges a refresh token for a new access token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Credential, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("refresh token is required")
	}
	return s.requestToken(ctx, url.Values{
		"client_id":     {s.ClientID},
		"client_secret": {s.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// CompleteCredential resolves missing public identity metadata without changing tokens.
func (s *Service) CompleteCredential(ctx context.Context, credential *Credential) (*Credential, error) {
	if credential == nil {
		return nil, errors.New("credential: Antigravity data is nil")
	}
	completed := *credential
	if completed.Email == "" {
		email, err := s.FetchUserInfo(ctx, completed.AccessToken)
		if err != nil {
			return nil, err
		}
		completed.Email = email
	}
	if completed.ProjectID == "" {
		projectID, err := s.FetchProjectID(ctx, completed.AccessToken)
		if err != nil {
			return nil, err
		}
		completed.ProjectID = projectID
	}
	if completed.ProjectID == "" {
		return nil, errors.New("project discovery: Antigravity returned an empty project_id")
	}
	paidTier, err := s.FetchPaidTier(ctx, completed.AccessToken)
	if err != nil {
		return nil, err
	}
	completed.PaidTier = paidTier
	if err := completed.Normalize(); err != nil {
		return nil, err
	}
	return &completed, nil
}

// FetchPaidTier returns the current paid subscription tier from the daily
// loadCodeAssist endpoint. A missing paidTier means the account has no paid tier.
func (s *Service) FetchPaidTier(ctx context.Context, accessToken string) (*PaidTier, error) {
	request := map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}}
	body, err := s.doJSON(ctx, http.MethodPost, strings.TrimRight(s.DailyAPIBaseURL, "/")+"/"+apiVersion+":loadCodeAssist", accessToken, request, false)
	if err != nil {
		return nil, fmt.Errorf("load Antigravity paid tier: %w", err)
	}
	var response struct {
		PaidTier *PaidTier `json:"paidTier"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Antigravity paid tier: %w", err)
	}
	if response.PaidTier == nil {
		return nil, nil
	}
	response.PaidTier.ID = strings.TrimSpace(response.PaidTier.ID)
	response.PaidTier.Name = strings.TrimSpace(response.PaidTier.Name)
	if response.PaidTier.ID == "" && response.PaidTier.Name == "" {
		return nil, nil
	}
	return response.PaidTier, nil
}

// FetchUserInfo returns the Google account email.
func (s *Service) FetchUserInfo(ctx context.Context, accessToken string) (string, error) {
	body, err := s.doJSON(ctx, http.MethodGet, s.UserInfoURL, accessToken, nil, false)
	if err != nil {
		return "", fmt.Errorf("fetch Antigravity user info: %w", err)
	}
	var response struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Antigravity user info: %w", err)
	}
	response.Email = strings.TrimSpace(response.Email)
	if response.Email == "" {
		return "", errors.New("user info: Antigravity response is missing email")
	}
	return response.Email, nil
}

// FetchProjectID resolves the Cloud Code companion project, onboarding when required.
func (s *Service) FetchProjectID(ctx context.Context, accessToken string) (string, error) {
	request := map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}}
	body, err := s.doJSON(ctx, http.MethodPost, strings.TrimRight(s.APIBaseURL, "/")+"/"+apiVersion+":loadCodeAssist", accessToken, request, false)
	if err != nil {
		return "", fmt.Errorf("load Antigravity Code Assist: %w", err)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Antigravity Code Assist: %w", err)
	}
	if projectID := extractProjectID(response); projectID != "" {
		return projectID, nil
	}
	return s.onboardUser(ctx, accessToken, defaultTierID(response))
}

func (s *Service) onboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	attempts := s.OnboardPollAttempts
	if attempts <= 0 {
		attempts = 5
	}
	request := map[string]any{
		"tier_id": tierID,
		"metadata": map[string]string{
			"ide_type": "ANTIGRAVITY", "ide_version": "2.2.1", "ide_name": "antigravity",
		},
	}
	endpoint := strings.TrimRight(s.DailyAPIBaseURL, "/") + "/" + apiVersion + ":onboardUser"
	for attempt := 0; attempt < attempts; attempt++ {
		body, err := s.doJSON(ctx, http.MethodPost, endpoint, accessToken, request, true)
		if err != nil {
			return "", fmt.Errorf("onboard Antigravity user: %w", err)
		}
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			return "", fmt.Errorf("decode Antigravity onboarding: %w", err)
		}
		if done, _ := response["done"].(bool); done {
			if nested, ok := response["response"].(map[string]any); ok {
				if projectID := extractProjectID(nested); projectID != "" {
					return projectID, nil
				}
			}
			return "", errors.New("onboarding: Antigravity response is missing project_id")
		}
		if attempt+1 < attempts {
			if err := s.Sleep(ctx, 2*time.Second); err != nil {
				return "", err
			}
		}
	}
	return "", errors.New("onboarding: Antigravity did not complete")
}

func (s *Service) requestToken(ctx context.Context, values url.Values) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build Antigravity token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token: Antigravity request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read Antigravity token response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("token: Antigravity endpoint returned HTTP %d", resp.StatusCode)
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Antigravity token response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("token: Antigravity response is missing access_token")
	}
	if token.ExpiresIn <= 0 {
		return nil, errors.New("token: Antigravity response has invalid expires_in")
	}
	now := time.Now().UTC()
	return &Credential{
		Type: ChannelType, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		ExpiresIn: token.ExpiresIn, Timestamp: now.UnixMilli(),
		Expired: now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

func (s *Service) doJSON(ctx context.Context, method, endpoint, accessToken string, payload any, onboard bool) ([]byte, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	userAgent := s.UserAgent
	if onboard {
		userAgent += " google-api-nodejs-client/10.3.0"
		req.Header.Set("X-Goog-Api-Client", "gl-node/22.21.1")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil {
		return errors.New("oauth: Antigravity service is unavailable")
	}
	if strings.TrimSpace(s.AuthorizationURL) == "" || strings.TrimSpace(s.TokenURL) == "" ||
		strings.TrimSpace(s.UserInfoURL) == "" || strings.TrimSpace(s.APIBaseURL) == "" ||
		strings.TrimSpace(s.DailyAPIBaseURL) == "" || strings.TrimSpace(s.ClientID) == "" ||
		strings.TrimSpace(s.ClientSecret) == "" || strings.TrimSpace(s.RedirectURI) == "" {
		return errors.New("oauth: Antigravity service configuration is incomplete")
	}
	if s.Sleep == nil {
		return errors.New("oauth: Antigravity sleep function is unavailable")
	}
	return nil
}

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, defaultRequestTimeout)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func extractProjectID(data map[string]any) string {
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := data[key].(type) {
		case string:
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		case map[string]any:
			if id, _ := value["id"].(string); strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	return ""
}

func defaultTierID(data map[string]any) string {
	if tiers, ok := data["allowedTiers"].([]any); ok {
		for _, raw := range tiers {
			tier, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			isDefault, _ := tier["isDefault"].(bool)
			id, _ := tier["id"].(string)
			if isDefault && strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	if currentTier, ok := data["currentTier"].(map[string]any); ok {
		if id, _ := currentTier["id"].(string); strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return "free-tier"
}
