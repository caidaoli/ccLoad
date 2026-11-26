package antigravityauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/util"

	"github.com/goccy/go-yaml"
)

// Production Antigravity OAuth endpoints and public client configuration.
const (
	DefaultAuthorizationURL = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL         = "https://oauth2.googleapis.com/token"
	DefaultUserInfoURL      = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	DefaultDailyAPIBaseURL  = "https://daily-cloudcode-pa.googleapis.com"
	DefaultClientID         = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	DefaultClientSecret     = "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"
	DefaultRedirectURI      = "http://localhost:51121/oauth-callback"
	DefaultManifestURL      = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"
	defaultRequestTimeout   = 30 * time.Second
	maxResponseBytes        = 1 << 20
	maxManifestBytes        = 4096
	apiVersion              = "v1internal"
	antigravityHubPlatform  = "os_type=darwin; arch=arm64"
)

/**
 * @brief Verified Hub versions and their changelists, newest first.
 * @details The first entry serves as both the startup and unmapped manifest fallback.
 */
var hubReleases = [...]struct {
	version    string ///< Version published in the Hub manifest.
	changelist string ///< Changelist reported by this release's build metadata.
}{
	{"2.35.1", "1029194470"},
	{"2.35.0", "1028447422"},
	{"2.34.0", "1026637980"},
	{"2.33.0", "1023160026"},
	{"2.32.2", "1022991777"},
	{"2.32.1", "1021296516"},
	{"2.29.0", "1013060670"},
	{"2.24.0", "1000335682"},
	{"2.23.0", "998057262"},
}

/**
 * @brief Hub User-Agent used before a manifest is fetched.
 */
var DefaultUserAgent = hubUserAgent(hubReleases[0].version)

var defaultScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/aicode",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

var (
	errAccessTokenRejected = errors.New("antigravity access token was rejected")
	// ErrCredentialUnusable means neither the imported AT nor one RT refresh produced an accepted access token.
	ErrCredentialUnusable = errors.New("antigravity credential could not obtain a usable access token")
	// ErrProjectUnavailable prevents inference without a discovered project.
	ErrProjectUnavailable = errors.New("antigravity project is unavailable")
)

// MetadataError preserves diagnostics without implementing the token endpoint
// failure interface: a metadata 401 must never classify as a revoked refresh token.
type MetadataError struct {
	HTTPStatus int
	Code       string
	body       string
	retryAfter time.Time
}

func (e *MetadataError) Error() string {
	return fmt.Sprintf("Antigravity metadata upstream returned HTTP %d: %s", e.HTTPStatus, e.body)
}

// UpstreamResponseBody preserves the original response for internal diagnostics.
func (e *MetadataError) UpstreamResponseBody() string { return e.body }

/**
 * @brief 返回项目或订阅元数据端点给出的恢复时间。
 * @return Retry-After 截止时间；缺失或无法解析时为零。
 */
func (e *MetadataError) RetryAfter() time.Time { return e.retryAfter }

// Is identifies access-token rejection without classifying permissions as authentication.
func (e *MetadataError) Is(target error) bool {
	return target == errAccessTokenRejected && e.HTTPStatus == http.StatusUnauthorized
}

// AccessTokenRejected reports a metadata authentication failure, excluding 403.
func AccessTokenRejected(err error) bool { return errors.Is(err, errAccessTokenRejected) }

var unavailableModelIDs = map[string]struct{}{
	"chat_20706":                  {},
	"chat_23310":                  {},
	"gemini-2.5-flash-thinking":   {},
	"gemini-2.5-pro":              {},
	"tab_flash_lite_preview":      {},
	"tab_jump_flash_lite_preview": {},
}

// Service implements the Google OAuth and project-discovery contract used by CLIProxyAPI.
type Service struct {
	Client              *http.Client
	AuthorizationURL    string
	TokenURL            string
	UserInfoURL         string
	DailyAPIBaseURL     string
	ClientID            string
	ClientSecret        string
	RedirectURI         string
	UserAgent           string
	ManifestURL         string
	Sleep               func(context.Context, time.Duration) error
	OnboardPollAttempts int
}

type updaterManifest struct {
	Version string `yaml:"version"`
}

type tokenEndpointError struct {
	statusCode   int
	responseBody string
	retryAfter   time.Time
}

func (e *tokenEndpointError) Error() string {
	if e == nil {
		return "token: Antigravity endpoint request failed"
	}
	return fmt.Sprintf("token: Antigravity endpoint returned HTTP %d", e.statusCode)
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

// NewService returns the production Antigravity OAuth service.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, AuthorizationURL: DefaultAuthorizationURL, TokenURL: DefaultTokenURL,
		UserInfoURL: DefaultUserInfoURL, DailyAPIBaseURL: DefaultDailyAPIBaseURL,
		ClientID: DefaultClientID, ClientSecret: DefaultClientSecret, RedirectURI: DefaultRedirectURI,
		UserAgent: DefaultUserAgent, ManifestURL: DefaultManifestURL, Sleep: sleepContext, OnboardPollAttempts: 5,
	}
}

/**
 * @brief Reads the Hub manifest and selects a verified User-Agent.
 * @param ctx Request context for the manifest fetch.
 * @return Error if the manifest cannot be fetched or contains an invalid version.
 * @note Call before concurrent use. On error, the existing User-Agent is retained.
 */
func (s *Service) RefreshUserAgent(ctx context.Context) error {
	if s == nil || s.Client == nil {
		return errors.New("antigravity manifest client is unavailable")
	}
	manifestURL := strings.TrimSpace(s.ManifestURL)
	if manifestURL == "" {
		return errors.New("antigravity manifest URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return fmt.Errorf("build Antigravity manifest request: %w", err)
	}
	req.Header.Set("User-Agent", "electron-builder")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch Antigravity manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("antigravity manifest returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return fmt.Errorf("read Antigravity manifest: %w", err)
	}
	var manifest updaterManifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode Antigravity manifest: %w", err)
	}
	version := strings.TrimSpace(manifest.Version)
	if !validVersion(version) {
		return fmt.Errorf("antigravity manifest returned invalid version %q", version)
	}
	s.UserAgent = hubUserAgent(version)
	return nil
}

/**
 * @brief Builds a Hub User-Agent from a verified release pair.
 * @param version Manifest version used to select the release.
 * @return User-Agent for the matching release or the newest registered release.
 */
func hubUserAgent(version string) string {
	release := hubReleases[0]
	for _, candidate := range hubReleases {
		if candidate.version == version {
			release = candidate
			break
		}
	}
	return fmt.Sprintf("antigravity/hub/%s (aidev_client; %s; cl=%s)", release.version, antigravityHubPlatform, release.changelist)
}

// RequestUserAgent returns the discovered User-Agent or the production fallback.
func (s *Service) RequestUserAgent() string {
	if s == nil || strings.TrimSpace(s.UserAgent) == "" {
		return DefaultUserAgent
	}
	return strings.TrimSpace(s.UserAgent)
}

/**
 * @brief Reads the version sent in the Hub User-Agent for onboarding metadata.
 * @return Version from the request User-Agent or the newest registered release if absent or invalid.
 */
func (s *Service) hubVersion() string {
	rest, ok := strings.CutPrefix(s.RequestUserAgent(), "antigravity/hub/")
	version, _, _ := strings.Cut(rest, " ")
	if ok && validVersion(version) {
		return version
	}
	return hubReleases[0].version
}

func validVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
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

// CompleteCredential refreshes a stale access token and resolves public identity metadata.
func (s *Service) CompleteCredential(ctx context.Context, credential *Credential) (*Credential, error) {
	if credential == nil {
		return nil, errors.New("credential: Antigravity data is nil")
	}
	completed := *credential
	needsRefresh, err := completed.NeedsRefresh(time.Now(), CredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	refreshed := false
	if needsRefresh {
		merged, err := s.refreshCredential(ctx, &completed)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCredentialUnusable, err)
		}
		completed = *merged
		refreshed = true
	}

	err = s.completeCredentialMetadata(ctx, &completed)
	if errors.Is(err, errAccessTokenRejected) && !refreshed {
		merged, refreshErr := s.refreshCredential(ctx, &completed)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrCredentialUnusable, refreshErr)
		}
		completed = *merged
		err = s.completeCredentialMetadata(ctx, &completed)
	}
	if err != nil {
		if errors.Is(err, errAccessTokenRejected) {
			return nil, fmt.Errorf("%w: refreshed access token was rejected", ErrCredentialUnusable)
		}
		return nil, err
	}
	if err := completed.Normalize(); err != nil {
		return nil, err
	}
	return &completed, nil
}

func (s *Service) refreshCredential(ctx context.Context, credential *Credential) (*Credential, error) {
	refreshed, err := s.Refresh(ctx, credential.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("refresh Antigravity credential: %w", err)
	}
	merged, err := credential.MergeRefresh(refreshed)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

func (s *Service) completeCredentialMetadata(ctx context.Context, credential *Credential) error {
	if credential == nil {
		return errors.New("credential: Antigravity data is nil")
	}
	{
		email, err := s.FetchUserInfo(ctx, credential.AccessToken)
		if err != nil {
			return err
		}
		credential.Email = email
	}
	sampledAt := time.Now().UTC()
	body, err := s.loadCodeAssist(ctx, credential.AccessToken)
	if err != nil {
		return err
	}
	projectID, err := s.projectIDFromCodeAssist(ctx, credential.AccessToken, body)
	if err != nil {
		return err
	}
	if projectID == "" {
		return errors.New("project discovery: Antigravity returned an empty project_id")
	}
	credential.ProjectID = projectID
	paidTier, credits, err := parseSubscription(body, sampledAt)
	if err != nil {
		return nil // Subscription metadata is optional; preserve the previous value.
	}
	credential.PaidTier = paidTier
	credential.Credits = credits
	return nil
}

// loadCodeAssist targets the daily endpoint for both project discovery and
// subscription metadata, matching the Hub client.
func (s *Service) loadCodeAssist(ctx context.Context, accessToken string) ([]byte, error) {
	request := map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}}
	body, err := s.doJSON(ctx, http.MethodPost, strings.TrimRight(s.DailyAPIBaseURL, "/")+"/"+apiVersion+":loadCodeAssist", accessToken, request, false)
	if err != nil {
		return nil, fmt.Errorf("load Antigravity Code Assist: %w", err)
	}
	return body, nil
}

// FetchPaidTier returns the current paid subscription tier from the daily
// loadCodeAssist endpoint. A missing paidTier means the account has no paid tier.
func (s *Service) FetchPaidTier(ctx context.Context, accessToken string) (*PaidTier, error) {
	tier, _, err := s.FetchSubscription(ctx, accessToken)
	return tier, err
}

/**
 * @brief Fetches the paid tier and Google One AI credit snapshot from loadCodeAssist.
 * @param ctx Request context.
 * @param accessToken Antigravity access token.
 * @return Paid tier, or nil when the account has no tier metadata.
 * @return Credit snapshot with unknown amounts when no valid Google One AI entry exists.
 * @return Error when loadCodeAssist fails or returns an invalid response.
 */
func (s *Service) FetchSubscription(ctx context.Context, accessToken string) (*PaidTier, *Credits, error) {
	sampledAt := time.Now().UTC()
	body, err := s.loadCodeAssist(ctx, accessToken)
	if err != nil {
		return nil, nil, err
	}
	return parseSubscription(body, sampledAt)
}

/**
 * @brief Decodes subscription metadata and the first valid Google One AI credit entry.
 * @param body JSON returned by loadCodeAssist.
 * @param sampledAt Time at which the sample was requested.
 * @return Paid tier, or nil when it is absent.
 * @return Credit snapshot, with a zero minimum when the source leaves it unset.
 * @return Error when the subscription response cannot be decoded.
 */
func parseSubscription(body []byte, sampledAt time.Time) (*PaidTier, *Credits, error) {
	var response struct {
		PaidTier *PaidTier `json:"paidTier"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, nil, fmt.Errorf("decode Antigravity paid tier: %w", err)
	}
	var raw struct {
		PaidTier struct {
			AvailableCredits []struct {
				Type    string          `json:"creditType"`
				Balance json.RawMessage `json:"creditAmount"`
				Minimum json.RawMessage `json:"minimumCreditAmountForUsage"`
			} `json:"availableCredits"`
		} `json:"paidTier"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("decode Antigravity credits: %w", err)
	}
	credits := &Credits{SampledAt: sampledAt}
	for _, entry := range raw.PaidTier.AvailableCredits {
		if !strings.EqualFold(entry.Type, "GOOGLE_ONE_AI") {
			continue
		}
		balance := parseCreditAmount(entry.Balance, nil)
		minimum := parseCreditAmount(entry.Minimum, new(float64))
		if balance == nil || minimum == nil {
			continue
		}
		credits.Balance = balance
		credits.Minimum = minimum
		break
	}
	if response.PaidTier == nil {
		return nil, credits, nil
	}
	response.PaidTier.ID = strings.TrimSpace(response.PaidTier.ID)
	response.PaidTier.Name = strings.TrimSpace(response.PaidTier.Name)
	if response.PaidTier.ID == "" && response.PaidTier.Name == "" {
		return nil, credits, nil
	}
	return response.PaidTier, credits, nil
}

/**
 * @brief Reads a finite non-negative credit amount from a protobuf JSON field.
 * @param raw JSON number, decimal string, null, or an omitted field.
 * @param defaultAmount Value to use when the field is omitted, null, or blank.
 * @return Parsed amount or the default for an unset field, or nil for an invalid value.
 */
func parseCreditAmount(raw json.RawMessage, defaultAmount *float64) *float64 {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return defaultAmount
	}
	if strings.HasPrefix(text, "\"") {
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil
		}
		text = strings.TrimSpace(text)
	}
	if text == "" {
		return defaultAmount
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	return &value
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
	body, err := s.loadCodeAssist(ctx, accessToken)
	if err != nil {
		return "", err
	}
	return s.projectIDFromCodeAssist(ctx, accessToken, body)
}

func (s *Service) projectIDFromCodeAssist(ctx context.Context, accessToken string, body []byte) (string, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Antigravity Code Assist: %w", err)
	}
	if projectID := extractProjectID(response); projectID != "" {
		return projectID, nil
	}
	return s.onboardUser(ctx, accessToken, defaultTierID(response))
}

// FetchAvailableModels returns the model identifiers exposed to one
// Antigravity project by the Cloud Code internal API.
func (s *Service) FetchAvailableModels(ctx context.Context, baseURL string, credential *Credential) ([]string, error) {
	if s == nil || s.Client == nil {
		return nil, errors.New("models: Antigravity service is unavailable")
	}
	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("models: Antigravity credential is missing access_token")
	}
	projectID := strings.TrimSpace(credential.ProjectID)
	if projectID == "" {
		return nil, errors.New("models: Antigravity credential is missing project_id")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("models: Antigravity base URL is empty")
	}

	body, err := s.doJSON(ctx, http.MethodPost, baseURL+"/"+apiVersion+":fetchAvailableModels", credential.AccessToken, map[string]string{
		"project": projectID,
	}, false)
	if err != nil {
		return nil, fmt.Errorf("fetch Antigravity models: %w", err)
	}
	var response struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Antigravity models: %w", err)
	}
	models := make([]string, 0, len(response.Models))
	for rawName := range response.Models {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if _, unavailable := unavailableModelIDs[name]; unavailable {
			continue
		}
		models = append(models, name)
	}
	if len(models) == 0 {
		return nil, errors.New("models: Antigravity returned an empty model list")
	}
	sort.Strings(models)
	return models, nil
}

func (s *Service) onboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	attempts := s.OnboardPollAttempts
	if attempts <= 0 {
		attempts = 5
	}
	request := map[string]any{
		"tier_id": tierID,
		"metadata": map[string]string{
			"ide_type": "ANTIGRAVITY", "ide_version": s.hubVersion(), "ide_name": "antigravity",
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

/**
 * @brief 标记需要协商标准 OAuth 传输协议的 token 请求。
 */
type tokenRequestContextKey struct{}

/**
 * @brief 区分 OAuth token 交换和使用 Cloud Code 传输的其他请求。
 * @param req 由 HTTP 客户端交给传输层的请求。
 * @return 由 Antigravity 服务发起的 token 端点请求返回 true。
 */
func IsTokenRequest(req *http.Request) bool {
	return req.Context().Value(tokenRequestContextKey{}) == true
}

/**
 * @brief 向 OAuth token 端点提交授权或刷新表单。
 * @param ctx 调用方的取消和截止时间。
 * @param values 已构造的 OAuth 表单。
 * @return 令牌凭证，失败时为 nil。
 * @return 配置、网络或上游响应无效时返回错误，端点拒绝返回 tokenEndpointError。
 * @note User-Agent 由标准 HTTP 传输按协商结果生成。
 */
func (s *Service) requestToken(ctx context.Context, values url.Values) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	requestCtx = context.WithValue(requestCtx, tokenRequestContextKey{}, true)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build Antigravity token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		retryAfter, _ := util.ParseRetryAfter(resp.Header, time.Now())
		return nil, &tokenEndpointError{statusCode: resp.StatusCode, responseBody: string(body), retryAfter: retryAfter}
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
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	userAgent := s.RequestUserAgent()
	if onboard {
		req.Header.Set("Accept", "*/*")
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
		var payload struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &payload)
		retryAfter, _ := util.ParseRetryAfter(resp.Header, time.Now())
		return nil, &MetadataError{HTTPStatus: resp.StatusCode, Code: payload.Error.Status, body: string(raw), retryAfter: retryAfter}
	}
	return raw, nil
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil {
		return errors.New("oauth: Antigravity service is unavailable")
	}
	if strings.TrimSpace(s.AuthorizationURL) == "" || strings.TrimSpace(s.TokenURL) == "" ||
		strings.TrimSpace(s.UserInfoURL) == "" || strings.TrimSpace(s.DailyAPIBaseURL) == "" ||
		strings.TrimSpace(s.ClientID) == "" || strings.TrimSpace(s.ClientSecret) == "" ||
		strings.TrimSpace(s.RedirectURI) == "" {
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
