package app

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

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	codexUsageURL              = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent        = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	antigravityUsageURL        = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	antigravityUsageUserAgent  = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	oauthUsageTimeout          = 30 * time.Second
	xaiUsageRequestTimeout     = 15 * time.Second
	maxOAuthUsageResponseBytes = 1 << 20
	weeklyUsageWindowSeconds   = 7 * 24 * 60 * 60
)

var (
	errOAuthUsageUnsupported         = errors.New("usage: channel does not use a supported OAuth provider")
	errCodexUsageManagerUnavailable  = errors.New("usage: Codex credential manager is unavailable")
	errAntigravityManagerUnavailable = errors.New("usage: Antigravity credential manager is unavailable")
	errXAIUsageManagerUnavailable    = errors.New("usage: xAI credential manager is unavailable")
	errXAIBillingBadCredential       = errors.New("usage: xAI credential was rejected")
)

type codexUsageRawWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

type codexUsageRateLimit struct {
	PrimaryWindow   *codexUsageRawWindow `json:"primary_window"`
	SecondaryWindow *codexUsageRawWindow `json:"secondary_window"`
}

type codexAdditionalRateLimit struct {
	LimitName       string               `json:"limit_name"`
	MeteredFeature  string               `json:"metered_feature"`
	RateLimit       *codexUsageRateLimit `json:"rate_limit"`
	PrimaryWindow   *codexUsageRawWindow `json:"primary_window"`
	SecondaryWindow *codexUsageRawWindow `json:"secondary_window"`
}

type codexUsagePayload struct {
	PlanType             string                     `json:"plan_type"`
	RateLimit            *codexUsageRateLimit       `json:"rate_limit"`
	AdditionalRateLimits []codexAdditionalRateLimit `json:"additional_rate_limits"`
}

type antigravityUsageBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

type antigravityUsageGroup struct {
	Buckets     []antigravityUsageBucket `json:"buckets"`
	DisplayName string                   `json:"displayName"`
}

type antigravityUsagePayload struct {
	Groups []antigravityUsageGroup `json:"groups"`
}

type oauthUsageWindow struct {
	LimitName          string  `json:"limit_name"`
	Kind               string  `json:"kind"`
	UsedPercent        float64 `json:"used_percent"`
	RemainingPercent   float64 `json:"remaining_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type oauthUsageSummary struct {
	Provider          string             `json:"provider"`
	PlanType          string             `json:"plan_type,omitempty"`
	SubscriptionTier  string             `json:"subscription_tier,omitempty"`
	EntitlementStatus string             `json:"entitlement_status,omitempty"`
	Windows           []oauthUsageWindow `json:"windows"`
	Warnings          []string           `json:"warnings,omitempty"`
}

type xaiUsageCent struct {
	Val float64 `json:"val"`
}

type xaiUsagePeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type xaiUsageConfig struct {
	CreditUsagePercent   *float64          `json:"creditUsagePercent"`
	CurrentPeriod        *xaiUsagePeriod   `json:"currentPeriod"`
	MonthlyLimit         *xaiUsageCent     `json:"monthlyLimit"`
	Used                 *xaiUsageCent     `json:"used"`
	OnDemandCap          *xaiUsageCent     `json:"onDemandCap"`
	OnDemandUsed         *xaiUsageCent     `json:"onDemandUsed"`
	PrepaidBalance       *xaiUsageCent     `json:"prepaidBalance"`
	IsUnifiedBillingUser *bool             `json:"isUnifiedBillingUser"`
	History              []json.RawMessage `json:"history"`
	BillingPeriodStart   string            `json:"billingPeriodStart"`
	BillingPeriodEnd     string            `json:"billingPeriodEnd"`
}

type xaiUsagePayload struct {
	Config                  *xaiUsageConfig `json:"config"`
	Plan                    string          `json:"plan"`
	PlanType                string          `json:"planType"`
	PlanTypeLegacy          string          `json:"plan_type"`
	SubscriptionTier        string          `json:"subscriptionTier"`
	SubscriptionTierLegacy  string          `json:"subscription_tier"`
	EntitlementStatus       string          `json:"entitlementStatus"`
	EntitlementStatusLegacy string          `json:"entitlement_status"`
	Subscription            struct {
		Tier string `json:"tier"`
		Plan string `json:"plan"`
	} `json:"subscription"`
	Entitlement struct {
		Status string `json:"status"`
	} `json:"entitlement"`
}

type xaiUsageEndpointResult struct {
	recognized        bool
	windows           []oauthUsageWindow
	planType          string
	subscriptionTier  string
	entitlementStatus string
	warning           string
}

type oauthUsageHTTPStatusError struct {
	provider   string
	statusCode int
}

func (e *oauthUsageHTTPStatusError) Error() string {
	return fmt.Sprintf("usage: %s request returned HTTP %d", e.provider, e.statusCode)
}

func appendCodexUsageWindow(windows []oauthUsageWindow, limitName, kind string, raw *codexUsageRawWindow) []oauthUsageWindow {
	if raw == nil || raw.UsedPercent == nil {
		return windows
	}
	usedPercent := min(max(*raw.UsedPercent, 0), 100)
	return append(windows, oauthUsageWindow{
		LimitName:          limitName,
		Kind:               kind,
		UsedPercent:        usedPercent,
		RemainingPercent:   100 - usedPercent,
		LimitWindowSeconds: max(raw.LimitWindowSeconds, 0),
		ResetAt:            max(raw.ResetAt, 0),
	})
}

func normalizeCodexUsage(payload *codexUsagePayload, fallbackPlanType string) (*oauthUsageSummary, error) {
	if payload == nil {
		return nil, errors.New("usage: Codex response is invalid")
	}
	summary := &oauthUsageSummary{
		Provider: codexauth.ChannelType,
		PlanType: strings.TrimSpace(payload.PlanType),
		Windows:  make([]oauthUsageWindow, 0, 2+2*len(payload.AdditionalRateLimits)),
	}
	if summary.PlanType == "" {
		summary.PlanType = strings.TrimSpace(fallbackPlanType)
	}
	if payload.RateLimit != nil {
		summary.Windows = appendCodexUsageWindow(summary.Windows, "codex", "primary", payload.RateLimit.PrimaryWindow)
		summary.Windows = appendCodexUsageWindow(summary.Windows, "codex", "secondary", payload.RateLimit.SecondaryWindow)
	}
	for _, additional := range payload.AdditionalRateLimits {
		limitName := strings.TrimSpace(additional.LimitName)
		if limitName == "" {
			limitName = strings.TrimSpace(additional.MeteredFeature)
		}
		if limitName == "" {
			limitName = "additional"
		}
		primary, secondary := additional.PrimaryWindow, additional.SecondaryWindow
		if additional.RateLimit != nil {
			if additional.RateLimit.PrimaryWindow != nil {
				primary = additional.RateLimit.PrimaryWindow
			}
			if additional.RateLimit.SecondaryWindow != nil {
				secondary = additional.RateLimit.SecondaryWindow
			}
		}
		summary.Windows = appendCodexUsageWindow(summary.Windows, limitName, "primary", primary)
		summary.Windows = appendCodexUsageWindow(summary.Windows, limitName, "secondary", secondary)
	}
	if len(summary.Windows) == 0 {
		return nil, errors.New("usage: Codex response has no rate limit windows")
	}
	return summary, nil
}

func normalizeAntigravityUsage(payload *antigravityUsagePayload) (*oauthUsageSummary, error) {
	if payload == nil {
		return nil, errors.New("usage: Antigravity response is invalid")
	}
	summary := &oauthUsageSummary{
		Provider: antigravityauth.ChannelType,
		Windows:  make([]oauthUsageWindow, 0),
	}
	for _, group := range payload.Groups {
		limitName := strings.TrimSpace(group.DisplayName)
		if limitName == "" {
			limitName = "Antigravity"
		}
		for _, bucket := range group.Buckets {
			if bucket.RemainingFraction == nil {
				continue
			}
			remainingPercent := min(max(*bucket.RemainingFraction*100, 0), 100)
			summary.Windows = append(summary.Windows, oauthUsageWindow{
				LimitName:          limitName,
				Kind:               antigravityUsageBucketKind(bucket),
				UsedPercent:        100 - remainingPercent,
				RemainingPercent:   remainingPercent,
				LimitWindowSeconds: antigravityUsageWindowSeconds(bucket.Window),
				ResetAt:            antigravityUsageResetAt(bucket.ResetTime),
			})
		}
	}
	if len(summary.Windows) == 0 {
		return nil, errors.New("usage: Antigravity response has no quota buckets")
	}
	return summary, nil
}

func antigravityUsageBucketKind(bucket antigravityUsageBucket) string {
	if kind := strings.TrimSpace(bucket.BucketID); kind != "" {
		return kind
	}
	return strings.TrimSpace(bucket.DisplayName)
}

func antigravityUsageWindowSeconds(window string) int64 {
	window = strings.ToLower(strings.TrimSpace(window))
	if window == "weekly" {
		return weeklyUsageWindowSeconds
	}
	duration, err := time.ParseDuration(window)
	if err != nil || duration <= 0 {
		return 0
	}
	return int64(duration / time.Second)
}

func antigravityUsageResetAt(resetTime string) int64 {
	resetAt, err := time.Parse(time.RFC3339, strings.TrimSpace(resetTime))
	if err != nil {
		return 0
	}
	return resetAt.Unix()
}

func requestCodexUsage(ctx context.Context, client *http.Client, credential *codexauth.Credential) (*oauthUsageSummary, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("usage: Codex request is unavailable")
	}
	req, err := newCodexUsageRequest(ctx, credential)
	if err != nil {
		return nil, errors.New("usage: Codex request is unavailable")
	}

	body, err := executeOAuthUsageRequest(client, req, "Codex")
	if err != nil {
		return nil, err
	}
	var payload codexUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("usage: Codex response is invalid")
	}
	return normalizeCodexUsage(&payload, credential.PlanType)
}

func newCodexUsageRequest(ctx context.Context, credential *codexauth.Credential) (*http.Request, error) {
	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("usage: Codex request is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUsageUserAgent)
	if credential.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	return req, nil
}

func requestAntigravityUsage(ctx context.Context, client *http.Client, credential *antigravityauth.Credential) (*oauthUsageSummary, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.ProjectID) == "" {
		return nil, errors.New("usage: Antigravity request is unavailable")
	}
	requestBody, err := json.Marshal(struct {
		Project string `json:"project"`
	}{Project: credential.ProjectID})
	if err != nil {
		return nil, errors.New("usage: Antigravity request is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityUsageURL, bytes.NewReader(requestBody))
	if err != nil {
		return nil, errors.New("usage: Antigravity request is unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityUsageUserAgent)

	body, err := executeOAuthUsageRequest(client, req, "Antigravity")
	if err != nil {
		return nil, err
	}
	var payload antigravityUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("usage: Antigravity response is invalid")
	}
	return normalizeAntigravityUsage(&payload)
}

func executeOAuthUsageRequest(client *http.Client, req *http.Request, provider string) ([]byte, error) {
	usageClient := &http.Client{
		Transport: client.Transport,
		Timeout:   client.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := usageClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage: %s request failed", provider)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthUsageResponseBytes))
		return nil, &oauthUsageHTTPStatusError{provider: provider, statusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthUsageResponseBytes+1))
	if err != nil || len(body) > maxOAuthUsageResponseBytes {
		return nil, fmt.Errorf("usage: %s response is invalid", provider)
	}
	return body, nil
}

func requestXAIUsage(
	ctx context.Context,
	client *http.Client,
	credential *xaiauth.Credential,
	baseURL string,
) (*oauthUsageSummary, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("usage: xAI request is unavailable")
	}
	creditsURL, err := xaiauth.BillingURL(baseURL, true)
	if err != nil {
		return nil, errors.New("usage: xAI model base URL is invalid")
	}
	monthlyURL, err := xaiauth.BillingURL(baseURL, false)
	if err != nil {
		return nil, errors.New("usage: xAI model base URL is invalid")
	}

	credits, err := requestXAIUsageEndpoint(ctx, client, credential.AccessToken, creditsURL, "weekly credits")
	if err != nil {
		return nil, err
	}
	monthly, err := requestXAIUsageEndpoint(ctx, client, credential.AccessToken, monthlyURL, "monthly billing")
	if err != nil {
		return nil, err
	}
	if !credits.recognized && !monthly.recognized {
		return nil, errors.New("usage: xAI billing data is unavailable")
	}

	summary := &oauthUsageSummary{
		Provider:          xaiauth.ChannelType,
		SubscriptionTier:  strings.TrimSpace(credential.SubscriptionTier),
		EntitlementStatus: strings.TrimSpace(credential.EntitlementStatus),
		Windows:           make([]oauthUsageWindow, 0, len(credits.windows)+len(monthly.windows)),
		Warnings:          make([]string, 0, 2),
	}
	for _, result := range []xaiUsageEndpointResult{credits, monthly} {
		summary.Windows = append(summary.Windows, result.windows...)
		if result.subscriptionTier != "" {
			summary.SubscriptionTier = result.subscriptionTier
		}
		if result.planType != "" {
			summary.PlanType = result.planType
		}
		if result.entitlementStatus != "" {
			summary.EntitlementStatus = result.entitlementStatus
		}
		if result.warning != "" {
			summary.Warnings = append(summary.Warnings, result.warning)
		}
	}
	if summary.PlanType == "" {
		summary.PlanType = summary.SubscriptionTier
	}
	return summary, nil
}

func requestXAIUsageEndpoint(
	ctx context.Context,
	client *http.Client,
	accessToken, endpoint, label string,
) (xaiUsageEndpointResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return xaiUsageEndpointResult{}, fmt.Errorf("usage: xAI %s request is unavailable", label)
	}
	xaiauth.ApplyBillingHeaders(req, accessToken)
	body, status, headers, err := executeXAIBillingUsageRequest(client, req)
	if err != nil {
		return xaiUsageEndpointResult{warning: fmt.Sprintf("xAI %s is unavailable", label)}, nil
	}
	classification := xaiauth.ClassifyBillingResponse(status, headers, body)
	switch classification {
	case xaiauth.BillingBadCredential:
		return xaiUsageEndpointResult{}, errXAIBillingBadCredential
	case xaiauth.BillingEntitlement, xaiauth.BillingQuota:
		result := xaiUsageResultFromPayload(body, label)
		result.recognized = true
		result.entitlementStatus = string(classification)
		return result, nil
	case xaiauth.BillingOK:
		result := xaiUsageResultFromPayload(body, label)
		if !result.recognized {
			result.warning = fmt.Sprintf("xAI %s response is unavailable", label)
		}
		return result, nil
	default:
		return xaiUsageEndpointResult{warning: fmt.Sprintf("xAI %s is unavailable", label)}, nil
	}
}

func executeXAIBillingUsageRequest(client *http.Client, req *http.Request) ([]byte, int, http.Header, error) {
	if client == nil || req == nil {
		return nil, 0, nil, errors.New("usage: xAI request is unavailable")
	}
	usageClient := &http.Client{
		Transport: client.Transport,
		Timeout:   xaiUsageRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := usageClient.Do(req)
	if err != nil {
		return nil, 0, nil, errors.New("usage: xAI request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOAuthUsageResponseBytes+1))
	if readErr != nil || len(body) > maxOAuthUsageResponseBytes {
		return nil, resp.StatusCode, resp.Header, errors.New("usage: xAI response is invalid")
	}
	return body, resp.StatusCode, resp.Header, nil
}

func xaiUsageResultFromPayload(body []byte, label string) xaiUsageEndpointResult {
	var payload xaiUsagePayload
	if json.Unmarshal(body, &payload) != nil {
		return xaiUsageEndpointResult{}
	}
	result := xaiUsageEndpointResult{
		planType:          firstNonEmpty(payload.Plan, payload.PlanType, payload.PlanTypeLegacy, payload.Subscription.Plan),
		subscriptionTier:  firstNonEmpty(payload.SubscriptionTier, payload.SubscriptionTierLegacy, payload.Subscription.Tier),
		entitlementStatus: firstNonEmpty(payload.EntitlementStatus, payload.EntitlementStatusLegacy, payload.Entitlement.Status),
	}
	if payload.Config == nil {
		result.recognized = result.planType != "" || result.subscriptionTier != "" || result.entitlementStatus != ""
		return result
	}
	result.recognized = xaiUsageConfigRecognized(payload.Config) ||
		result.planType != "" || result.subscriptionTier != "" || result.entitlementStatus != ""
	if window, ok := xaiUsageWindowFromConfig(payload.Config, label); ok {
		result.windows = []oauthUsageWindow{window}
		result.recognized = true
	}
	return result
}

func xaiUsageConfigRecognized(config *xaiUsageConfig) bool {
	if config == nil {
		return false
	}
	if config.CreditUsagePercent != nil || config.MonthlyLimit != nil && config.Used != nil {
		return true
	}
	if config.CurrentPeriod != nil && (config.IsUnifiedBillingUser != nil ||
		config.OnDemandCap != nil && config.OnDemandUsed != nil || config.PrepaidBalance != nil) {
		return true
	}
	return len(config.History) > 0 && (config.Used != nil || config.OnDemandCap != nil)
}

func xaiUsageWindowFromConfig(config *xaiUsageConfig, label string) (oauthUsageWindow, bool) {
	if config == nil {
		return oauthUsageWindow{}, false
	}
	if config.CreditUsagePercent != nil {
		used := min(max(*config.CreditUsagePercent, 0), 100)
		periodType := ""
		periodStart := config.BillingPeriodStart
		periodEnd := config.BillingPeriodEnd
		if config.CurrentPeriod != nil {
			periodType = config.CurrentPeriod.Type
			periodStart = firstNonEmpty(config.CurrentPeriod.Start, periodStart)
			periodEnd = firstNonEmpty(config.CurrentPeriod.End, periodEnd)
		}
		kind := normalizedXAIUsagePeriodKind(periodType, label)
		windowSeconds, resetAt, ok := xaiUsagePeriodBounds(periodStart, periodEnd)
		if !ok {
			return oauthUsageWindow{}, false
		}
		return oauthUsageWindow{
			LimitName: label, Kind: kind, UsedPercent: used, RemainingPercent: 100 - used,
			LimitWindowSeconds: windowSeconds, ResetAt: resetAt,
		}, true
	}
	if config.OnDemandCap != nil && config.OnDemandUsed != nil && config.OnDemandCap.Val > 0 {
		periodType := ""
		periodStart := config.BillingPeriodStart
		periodEnd := config.BillingPeriodEnd
		if config.CurrentPeriod != nil {
			periodType = config.CurrentPeriod.Type
			periodStart = firstNonEmpty(config.CurrentPeriod.Start, periodStart)
			periodEnd = firstNonEmpty(config.CurrentPeriod.End, periodEnd)
		}
		windowSeconds, resetAt, ok := xaiUsagePeriodBounds(periodStart, periodEnd)
		if !ok {
			return oauthUsageWindow{}, false
		}
		used := min(max(config.OnDemandUsed.Val*100/config.OnDemandCap.Val, 0), 100)
		return oauthUsageWindow{
			LimitName: label, Kind: normalizedXAIUsagePeriodKind(periodType, label),
			UsedPercent: used, RemainingPercent: 100 - used,
			LimitWindowSeconds: windowSeconds, ResetAt: resetAt,
		}, true
	}
	if config.MonthlyLimit != nil && config.Used != nil && config.MonthlyLimit.Val > 0 {
		windowSeconds, resetAt, ok := xaiUsagePeriodBounds(config.BillingPeriodStart, config.BillingPeriodEnd)
		if !ok {
			return oauthUsageWindow{}, false
		}
		used := min(max(config.Used.Val*100/config.MonthlyLimit.Val, 0), 100)
		return oauthUsageWindow{
			LimitName: label, Kind: "monthly", UsedPercent: used, RemainingPercent: 100 - used,
			LimitWindowSeconds: windowSeconds, ResetAt: resetAt,
		}, true
	}
	return oauthUsageWindow{}, false
}

func xaiUsagePeriodBounds(startRaw, endRaw string) (int64, int64, bool) {
	end, endErr := time.Parse(time.RFC3339, strings.TrimSpace(endRaw))
	if endErr != nil {
		return 0, 0, false
	}
	start, startErr := time.Parse(time.RFC3339, strings.TrimSpace(startRaw))
	if startErr != nil || !end.After(start) {
		return 0, end.Unix(), true
	}
	return int64(end.Sub(start) / time.Second), end.Unix(), true
}

func normalizedXAIUsagePeriodKind(periodType, fallback string) string {
	periodType = strings.ToLower(strings.TrimSpace(periodType))
	switch {
	case strings.Contains(periodType, "weekly"):
		return "weekly"
	case strings.Contains(periodType, "monthly"):
		return "monthly"
	default:
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// HandleOAuthUsage fetches one OAuth channel's current quota without exposing
// the database-backed credential to the browser.
func (s *Server) HandleOAuthUsage(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), oauthUsageTimeout)
	defer cancel()
	summary, err := s.oauthUsageSummary(ctx, cfg)
	if err != nil {
		switch {
		case errors.Is(err, errOAuthUsageUnsupported):
			RespondError(c, http.StatusConflict, err)
		case errors.Is(err, errCodexUsageManagerUnavailable),
			errors.Is(err, errAntigravityManagerUnavailable),
			errors.Is(err, errXAIUsageManagerUnavailable):
			RespondError(c, http.StatusServiceUnavailable, err)
		default:
			RespondError(c, http.StatusBadGateway, err)
		}
		return
	}
	RespondJSON(c, http.StatusOK, summary)
}

func (s *Server) oauthUsageSummary(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	switch {
	case cfg.UsesCodexOAuth():
		if s.codexCredentials == nil {
			return nil, errCodexUsageManagerUnavailable
		}
		credential, err := s.codexCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, errors.New("usage: Codex credential refresh failed")
		}
		return requestCodexUsage(ctx, s.getClientForChannel(cfg), credential)
	case cfg.UsesAntigravityOAuth():
		if s.antigravityCredentials == nil {
			return nil, errAntigravityManagerUnavailable
		}
		credential, err := s.antigravityCredentials.credentialWithMetadata(ctx, cfg)
		if err != nil {
			return nil, errors.New("usage: Antigravity credential refresh failed")
		}
		return requestAntigravityUsage(ctx, s.getClientForChannel(cfg), credential)
	case cfg.UsesXAIOAuth():
		if s.xaiCredentials == nil {
			return nil, errXAIUsageManagerUnavailable
		}
		credential, err := s.xaiCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, errors.New("usage: xAI credential refresh failed")
		}
		baseURL := xaiauth.CLIBaseURL
		if len(cfg.URLs) > 0 && strings.TrimSpace(cfg.URLs[0].URL) != "" {
			baseURL = cfg.URLs[0].URL
		}
		client := s.getClientForChannel(cfg)
		for attempt := 0; attempt < 2; attempt++ {
			summary, usageErr := requestXAIUsage(ctx, client, credential, baseURL)
			if !errors.Is(usageErr, errXAIBillingBadCredential) {
				return summary, usageErr
			}
			if attempt == 1 {
				return nil, errors.New("usage: xAI credential was rejected")
			}
			credential, err = s.xaiCredentials.credential(ctx, cfg, true)
			if err != nil {
				return nil, errors.New("usage: xAI credential refresh failed")
			}
		}
		return nil, errors.New("usage: xAI credential was rejected")
	default:
		return nil, errOAuthUsageUnsupported
	}
}
