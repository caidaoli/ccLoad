package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	codexUsageURL              = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent        = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	anthropicUsageUserAgent    = "claude-code/" + anthropicCLIVersion
	antigravityUsageURL        = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	antigravityUsageUserAgent  = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	oauthUsageTimeout          = 30 * time.Second
	oauthUsageBatchWorkers     = 8
	maxOAuthUsageBatchChannels = 1000
	xaiUsageRequestTimeout     = 15 * time.Second
	maxOAuthUsageResponseBytes = 1 << 20
	maxOAuthCredentialBytes    = 1 << 20
	weeklyUsageWindowSeconds   = 7 * 24 * 60 * 60
)

var (
	errOAuthUsageUnsupported         = errors.New("usage: channel does not use a supported OAuth provider")
	errCodexUsageManagerUnavailable  = errors.New("usage: Codex credential manager is unavailable")
	errAnthropicManagerUnavailable   = errors.New("usage: Anthropic credential manager is unavailable")
	errAntigravityManagerUnavailable = errors.New("usage: Antigravity credential manager is unavailable")
	errXAIUsageManagerUnavailable    = errors.New("usage: xAI credential manager is unavailable")
	errXAIBillingBadCredential       = errors.New("usage: xAI credential was rejected")
	errOAuthUsageChannelNotFound     = errors.New("channel not found")
	errOAuthUsagePersistFailed       = errors.New("usage: persist OAuth quota failed")
)

type oauthUsageBatchRequest struct {
	ChannelIDs []int64 `json:"channel_ids"`
}

type oauthUsageBatchResult struct {
	ChannelID int64              `json:"channel_id"`
	Status    string             `json:"status"`
	Usage     *oauthUsageSummary `json:"usage,omitempty"`
	Error     string             `json:"error,omitempty"`
}

type oauthUsageBatchEvent struct {
	Event     string                 `json:"event"`
	Processed int                    `json:"processed"`
	Total     int                    `json:"total"`
	Succeeded int                    `json:"succeeded"`
	Failed    int                    `json:"failed"`
	Result    *oauthUsageBatchResult `json:"result,omitempty"`
}

type oauthUsageRequestError struct {
	provider string
}

func (e *oauthUsageRequestError) Error() string {
	return fmt.Sprintf("usage: %s request failed", e.provider)
}

type oauthUsageUpstreamResponseError interface {
	error
	UpstreamResponseBody() string
}

func oauthUsageCredentialRefreshError(err error, fallback string) error {
	var upstreamErr oauthUsageUpstreamResponseError
	if errors.As(err, &upstreamErr) {
		if body := upstreamErr.UpstreamResponseBody(); body != "" {
			return errors.New(body)
		}
	}
	return errors.New(fallback)
}

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

type anthropicUsageRawWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type anthropicUsagePayload struct {
	FiveHour                *anthropicUsageRawWindow `json:"five_hour"`
	SevenDay                *anthropicUsageRawWindow `json:"seven_day"`
	SevenDaySonnet          *anthropicUsageRawWindow `json:"seven_day_sonnet"`
	SevenDayOverageIncluded *anthropicUsageRawWindow `json:"seven_day_overage_included"`
}

type anthropicProfileSubject struct {
	SubscriptionType string `json:"subscription_type"`
	RateLimitTier    string `json:"rate_limit_tier"`
	HasClaudePro     *bool  `json:"has_claude_pro"`
	HasClaudeMax     *bool  `json:"has_claude_max"`
}

type anthropicProfileOrganization struct {
	OrganizationType      string          `json:"organization_type"`
	RateLimitTier         string          `json:"rate_limit_tier"`
	ClaudeCodeTrialEndsAt json.RawMessage `json:"claude_code_trial_ends_at"`
}

type anthropicProfilePayload struct {
	SubscriptionType          string                        `json:"subscription_type"`
	OrganizationType          string                        `json:"organization_type"`
	OrganizationRateLimitTier string                        `json:"organization_rate_limit_tier"`
	UserRateLimitTier         string                        `json:"user_rate_limit_tier"`
	Account                   *anthropicProfileSubject      `json:"account"`
	User                      *anthropicProfileSubject      `json:"user"`
	Organization              *anthropicProfileOrganization `json:"organization"`
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
	XAIBilling        *xaiBillingSummary `json:"xai_billing,omitempty"`
}

type persistedOAuthUsageSnapshot struct {
	RequestedAt string            `json:"requested_at"`
	SampledAt   string            `json:"sampled_at"`
	Summary     oauthUsageSummary `json:"summary"`
}

type xaiUsageCent struct {
	Val *float64 `json:"val"`
}

type xaiProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usagePercent"`
}

type xaiBillingProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usage_percent"`
}

type xaiBillingSummary struct {
	WeeklyPresent      bool                     `json:"weekly_present"`
	WeeklyUsagePercent *float64                 `json:"weekly_usage_percent"`
	WeeklyResetAt      string                   `json:"weekly_reset_at,omitempty"`
	ProductUsage       []xaiBillingProductUsage `json:"product_usage,omitempty"`
	OnDemandCapCents   *float64                 `json:"on_demand_cap_cents"`
	OnDemandUsedCents  *float64                 `json:"on_demand_used_cents"`
	MonthlyLimitCents  *float64                 `json:"monthly_limit_cents"`
	UsedCents          *float64                 `json:"used_cents"`
	IncludedUsedCents  *float64                 `json:"included_used_cents"`
	MonthlyResetAt     string                   `json:"monthly_reset_at,omitempty"`
	MonthlyPresent     bool                     `json:"monthly_present"`
}

type xaiUsagePeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type xaiUsageConfig struct {
	CreditUsagePercent   *float64          `json:"creditUsagePercent"`
	ProductUsage         []xaiProductUsage `json:"productUsage"`
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

func (config *xaiUsageConfig) UnmarshalJSON(data []byte) error {
	type rawProduct struct {
		Product            string          `json:"product"`
		UsagePercent       json.RawMessage `json:"usagePercent"`
		UsagePercentLegacy json.RawMessage `json:"usage_percent"`
	}
	type rawConfig struct {
		CreditUsagePercent       json.RawMessage   `json:"creditUsagePercent"`
		CreditUsagePercentLegacy json.RawMessage   `json:"credit_usage_percent"`
		CurrentPeriod            *xaiUsagePeriod   `json:"currentPeriod"`
		CurrentPeriodLegacy      *xaiUsagePeriod   `json:"current_period"`
		ProductUsage             []rawProduct      `json:"productUsage"`
		ProductUsageLegacy       []rawProduct      `json:"product_usage"`
		MonthlyLimit             json.RawMessage   `json:"monthlyLimit"`
		MonthlyLimitLegacy       json.RawMessage   `json:"monthly_limit"`
		Used                     json.RawMessage   `json:"used"`
		OnDemandCap              json.RawMessage   `json:"onDemandCap"`
		OnDemandCapLegacy        json.RawMessage   `json:"on_demand_cap"`
		OnDemandUsed             json.RawMessage   `json:"onDemandUsed"`
		OnDemandUsedLegacy       json.RawMessage   `json:"on_demand_used"`
		PrepaidBalance           json.RawMessage   `json:"prepaidBalance"`
		PrepaidBalanceLegacy     json.RawMessage   `json:"prepaid_balance"`
		IsUnifiedBillingUser     *bool             `json:"isUnifiedBillingUser"`
		IsUnifiedLegacy          *bool             `json:"is_unified_billing_user"`
		History                  []json.RawMessage `json:"history"`
		BillingPeriodStart       string            `json:"billingPeriodStart"`
		BillingPeriodStartLegacy string            `json:"billing_period_start"`
		BillingPeriodEnd         string            `json:"billingPeriodEnd"`
		BillingPeriodEndLegacy   string            `json:"billing_period_end"`
	}
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	config.CreditUsagePercent = parseXAINumber(firstXAIRaw(raw.CreditUsagePercent, raw.CreditUsagePercentLegacy))
	config.CurrentPeriod = raw.CurrentPeriod
	if config.CurrentPeriod == nil {
		config.CurrentPeriod = raw.CurrentPeriodLegacy
	}
	products := raw.ProductUsage
	if products == nil {
		products = raw.ProductUsageLegacy
	}
	config.ProductUsage = make([]xaiProductUsage, 0, len(products))
	for _, product := range products {
		config.ProductUsage = append(config.ProductUsage, xaiProductUsage{
			Product:      product.Product,
			UsagePercent: parseXAINumber(firstXAIRaw(product.UsagePercent, product.UsagePercentLegacy)),
		})
	}
	config.MonthlyLimit = parseXAICent(firstXAIRaw(raw.MonthlyLimit, raw.MonthlyLimitLegacy))
	config.Used = parseXAICent(raw.Used)
	config.OnDemandCap = parseXAICent(firstXAIRaw(raw.OnDemandCap, raw.OnDemandCapLegacy))
	config.OnDemandUsed = parseXAICent(firstXAIRaw(raw.OnDemandUsed, raw.OnDemandUsedLegacy))
	config.PrepaidBalance = parseXAICent(firstXAIRaw(raw.PrepaidBalance, raw.PrepaidBalanceLegacy))
	config.IsUnifiedBillingUser = raw.IsUnifiedBillingUser
	if config.IsUnifiedBillingUser == nil {
		config.IsUnifiedBillingUser = raw.IsUnifiedLegacy
	}
	config.History = raw.History
	config.BillingPeriodStart = firstNonEmpty(raw.BillingPeriodStart, raw.BillingPeriodStartLegacy)
	config.BillingPeriodEnd = firstNonEmpty(raw.BillingPeriodEnd, raw.BillingPeriodEndLegacy)
	return nil
}

func firstXAIRaw(primary, fallback json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(primary)) > 0 && string(bytes.TrimSpace(primary)) != "null" {
		return primary
	}
	return fallback
}

func parseXAICent(raw json.RawMessage) *xaiUsageCent {
	value := parseXAINumber(raw)
	if value == nil {
		return nil
	}
	return &xaiUsageCent{Val: value}
}

func parseXAINumber(raw json.RawMessage) *float64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '{' {
		var object struct {
			Val json.RawMessage `json:"val"`
		}
		if json.Unmarshal(trimmed, &object) != nil {
			return nil
		}
		return parseXAINumber(object.Val)
	}
	var text string
	if trimmed[0] == '"' {
		if json.Unmarshal(trimmed, &text) != nil {
			return nil
		}
	} else {
		text = string(trimmed)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
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
	billing           *xaiBillingSummary
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

func appendAnthropicUsageWindow(
	windows []oauthUsageWindow,
	limitName string,
	kind string,
	windowSeconds int64,
	raw *anthropicUsageRawWindow,
) []oauthUsageWindow {
	if raw == nil || raw.Utilization == nil {
		return windows
	}
	usedPercent := min(max(*raw.Utilization, 0), 100)
	resetAt := int64(0)
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw.ResetsAt)); err == nil {
		resetAt = parsed.Unix()
	}
	return append(windows, oauthUsageWindow{
		LimitName:          limitName,
		Kind:               kind,
		UsedPercent:        usedPercent,
		RemainingPercent:   100 - usedPercent,
		LimitWindowSeconds: windowSeconds,
		ResetAt:            resetAt,
	})
}

func normalizeAnthropicUsage(payload *anthropicUsagePayload) (*oauthUsageSummary, error) {
	if payload == nil {
		return nil, errors.New("usage: Anthropic response is invalid")
	}
	summary := &oauthUsageSummary{
		Provider: anthropicauth.ChannelType,
		Windows:  make([]oauthUsageWindow, 0, 4),
	}
	summary.Windows = appendAnthropicUsageWindow(summary.Windows, "", "five_hour", 5*60*60, payload.FiveHour)
	summary.Windows = appendAnthropicUsageWindow(summary.Windows, "", "seven_day", weeklyUsageWindowSeconds, payload.SevenDay)
	summary.Windows = appendAnthropicUsageWindow(summary.Windows, "Claude Sonnet", "seven_day_sonnet", weeklyUsageWindowSeconds, payload.SevenDaySonnet)
	summary.Windows = appendAnthropicUsageWindow(summary.Windows, "Claude Fable", "seven_day_fable", weeklyUsageWindowSeconds, payload.SevenDayOverageIncluded)
	if len(summary.Windows) == 0 {
		return nil, errors.New("usage: Anthropic response has no rate limit windows")
	}
	return summary, nil
}

func anthropicProfileMetadata(payload *anthropicProfilePayload) (string, string, string, string, bool, bool, bool) {
	if payload == nil {
		return "", "", "", "", false, false, false
	}
	var accountSubscription, accountTier, userSubscription, userTier string
	var hasClaudeMax, hasClaudePro bool
	if payload.Account != nil {
		accountSubscription = payload.Account.SubscriptionType
		accountTier = payload.Account.RateLimitTier
		hasClaudeMax = payload.Account.HasClaudeMax != nil && *payload.Account.HasClaudeMax
		hasClaudePro = payload.Account.HasClaudePro != nil && *payload.Account.HasClaudePro
	}
	if payload.User != nil {
		userSubscription = payload.User.SubscriptionType
		userTier = payload.User.RateLimitTier
	}
	organizationType, organizationTier, trialEndsAt, trialEndsAtSet := payload.OrganizationType, payload.OrganizationRateLimitTier, "", false
	if payload.Organization != nil {
		organizationType = firstNonEmpty(payload.Organization.OrganizationType, organizationType)
		organizationTier = firstNonEmpty(payload.Organization.RateLimitTier, organizationTier)
		if raw := payload.Organization.ClaudeCodeTrialEndsAt; len(raw) > 0 {
			trialEndsAtSet = true
			if string(raw) != "null" {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					trialEndsAtSet = false
				} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
					trialEndsAt = parsed.UTC().Format(time.RFC3339)
				} else {
					trialEndsAtSet = false
				}
			}
		}
	}
	return firstNonEmpty(payload.SubscriptionType, userSubscription, accountSubscription),
		firstNonEmpty(organizationTier, payload.UserRateLimitTier, userTier, accountTier),
		strings.TrimSpace(organizationType), trialEndsAt, trialEndsAtSet, hasClaudeMax, hasClaudePro
}

func anthropicPlanLabel(subscriptionType, rateLimitTier, organizationType string, hasClaudeMax, hasClaudePro bool) (string, bool) {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		return strings.NewReplacer("-", "_", " ", "_").Replace(value)
	}
	subscription := normalize(subscriptionType)
	tier := normalize(rateLimitTier)
	organization := normalize(organizationType)
	if organization != "" {
		switch organization {
		case "claude_enterprise", "enterprise":
			return "Enterprise", true
		case "claude_team", "team":
			return "Team", true
		case "claude_max", "max":
			if strings.Contains(tier, "max_20x") {
				return "Max 20x", true
			}
			if strings.Contains(tier, "max_5x") {
				return "Max 5x", true
			}
			return "Max", true
		case "claude_pro", "pro":
			return "Pro", true
		default:
			return "", true
		}
	}
	switch {
	case strings.Contains(tier, "max_20x"):
		return "Max 20x", true
	case strings.Contains(tier, "max_5x"):
		return "Max 5x", true
	case hasClaudeMax, subscription == "max", subscription == "claude_max":
		return "Max", true
	case hasClaudePro, subscription == "pro", subscription == "claude_pro",
		subscription == "stripe_subscription", subscription == "stripe_subscription_contracted",
		subscription == "apple_subscription", subscription == "google_play_subscription":
		return "Pro", true
	case subscription == "team":
		return "Team", true
	case subscription == "enterprise":
		return "Enterprise", true
	default:
		return "", false
	}
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

func requestAnthropicUsage(
	ctx context.Context,
	client *http.Client,
	credential *anthropicauth.Credential,
	baseURL string,
) (*oauthUsageSummary, anthropicCredentialMetadata, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, anthropicCredentialMetadata{}, errors.New("usage: Anthropic request is unavailable")
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = anthropicauth.DefaultUpstreamURL
	}
	usageURL := buildUpstreamURL(baseURL, "/api/oauth/usage", "")
	profileURL := buildUpstreamURL(baseURL, "/api/oauth/profile", "")
	usageRequest, err := newAnthropicOAuthMetadataRequest(ctx, usageURL, credential.AccessToken, true)
	if err != nil {
		return nil, anthropicCredentialMetadata{}, errors.New("usage: Anthropic request is unavailable")
	}
	usageBody, err := executeOAuthUsageRequest(client, usageRequest, "Anthropic")
	if err != nil {
		return nil, anthropicCredentialMetadata{}, err
	}
	var usagePayload anthropicUsagePayload
	if err := json.Unmarshal(usageBody, &usagePayload); err != nil {
		return nil, anthropicCredentialMetadata{}, errors.New("usage: Anthropic response is invalid")
	}
	summary, err := normalizeAnthropicUsage(&usagePayload)
	if err != nil {
		return nil, anthropicCredentialMetadata{}, err
	}
	summary.PlanType = strings.TrimSpace(credential.PlanType)

	profileRequest, err := newAnthropicOAuthMetadataRequest(ctx, profileURL, credential.AccessToken, false)
	if err != nil {
		summary.Warnings = append(summary.Warnings, "Anthropic subscription metadata unavailable")
		return summary, anthropicCredentialMetadata{}, nil
	}
	profileBody, err := executeOAuthUsageRequest(client, profileRequest, "Anthropic profile")
	if err != nil {
		summary.Warnings = append(summary.Warnings, "Anthropic subscription metadata unavailable")
		return summary, anthropicCredentialMetadata{}, nil
	}
	var profilePayload anthropicProfilePayload
	if err := json.Unmarshal(profileBody, &profilePayload); err != nil {
		summary.Warnings = append(summary.Warnings, "Anthropic subscription metadata unavailable")
		return summary, anthropicCredentialMetadata{}, nil
	}
	subscriptionType, rateLimitTier, organizationType, trialEndsAt, trialEndsAtSet, hasClaudeMax, hasClaudePro := anthropicProfileMetadata(&profilePayload)
	profilePlanType, planTypeSet := anthropicPlanLabel(subscriptionType, rateLimitTier, organizationType, hasClaudeMax, hasClaudePro)
	summary.SubscriptionTier = rateLimitTier
	if planTypeSet {
		summary.PlanType = profilePlanType
	}
	if !planTypeSet || profilePlanType == "" {
		summary.Warnings = append(summary.Warnings, "Anthropic subscription metadata unavailable")
	}
	return summary, anthropicCredentialMetadata{
		PlanType: profilePlanType, PlanTypeSet: planTypeSet,
		ClaudeCodeTrialEndsAt: trialEndsAt, TrialEndsAtSet: trialEndsAtSet,
	}, nil
}

func newAnthropicOAuthMetadataRequest(ctx context.Context, targetURL, accessToken string, usage bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", anthropicUsageUserAgent)
	if usage {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else {
		req.Header.Set("Cache-Control", "no-cache")
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
		if requestErr := req.Context().Err(); requestErr != nil {
			return nil, requestErr
		}
		return nil, &oauthUsageRequestError{provider: provider}
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
		summary.XAIBilling = mergeXAIBillingSummary(summary.XAIBilling, result.billing)
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
	result.billing = xaiBillingSummaryFromConfig(payload.Config)
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
	if config.CreditUsagePercent != nil || len(config.ProductUsage) > 0 || config.MonthlyLimit != nil && config.Used != nil {
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
		periodStart, periodEnd := "", ""
		if config.MonthlyLimit == nil && config.Used == nil {
			periodStart = config.BillingPeriodStart
			periodEnd = config.BillingPeriodEnd
		}
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
	if (config.MonthlyLimit == nil || config.Used == nil) && config.OnDemandCap != nil && config.OnDemandCap.Val != nil && config.OnDemandUsed != nil && config.OnDemandUsed.Val != nil && *config.OnDemandCap.Val > 0 {
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
		used := min(max(*config.OnDemandUsed.Val*100 / *config.OnDemandCap.Val, 0), 100)
		return oauthUsageWindow{
			LimitName: label, Kind: normalizedXAIUsagePeriodKind(periodType, label),
			UsedPercent: used, RemainingPercent: 100 - used,
			LimitWindowSeconds: windowSeconds, ResetAt: resetAt,
		}, true
	}
	if config.MonthlyLimit != nil && config.MonthlyLimit.Val != nil && config.Used != nil && config.Used.Val != nil && *config.MonthlyLimit.Val > 0 {
		windowSeconds, resetAt, ok := xaiUsagePeriodBounds(config.BillingPeriodStart, config.BillingPeriodEnd)
		if !ok {
			return oauthUsageWindow{}, false
		}
		used := min(max(*config.Used.Val*100 / *config.MonthlyLimit.Val, 0), 100)
		return oauthUsageWindow{
			LimitName: label, Kind: "monthly", UsedPercent: used, RemainingPercent: 100 - used,
			LimitWindowSeconds: windowSeconds, ResetAt: resetAt,
		}, true
	}
	return oauthUsageWindow{}, false
}

func xaiBillingSummaryFromConfig(config *xaiUsageConfig) *xaiBillingSummary {
	if config == nil {
		return nil
	}
	billing := &xaiBillingSummary{
		WeeklyUsagePercent: config.CreditUsagePercent,
		OnDemandCapCents:   xaiCentValue(config.OnDemandCap),
		OnDemandUsedCents:  xaiCentValue(config.OnDemandUsed),
		MonthlyLimitCents:  xaiCentValue(config.MonthlyLimit),
		UsedCents:          xaiCentValue(config.Used),
		ProductUsage:       make([]xaiBillingProductUsage, 0, len(config.ProductUsage)),
	}
	periodKind := ""
	if config.CurrentPeriod != nil {
		periodKind = normalizedXAIUsagePeriodKind(config.CurrentPeriod.Type, "")
	}
	billing.WeeklyPresent = billing.WeeklyUsagePercent != nil || len(config.ProductUsage) > 0 || periodKind == "weekly"
	billing.MonthlyPresent = billing.MonthlyLimitCents != nil || billing.UsedCents != nil ||
		(!billing.WeeklyPresent && (billing.OnDemandCapCents != nil || strings.TrimSpace(config.BillingPeriodEnd) != ""))
	if config.CurrentPeriod != nil {
		periodEnd := strings.TrimSpace(config.CurrentPeriod.End)
		switch periodKind {
		case "monthly":
			if billing.MonthlyPresent {
				billing.MonthlyResetAt = periodEnd
			}
		case "weekly":
			billing.WeeklyResetAt = periodEnd
		default:
			if billing.WeeklyUsagePercent != nil || len(config.ProductUsage) > 0 {
				billing.WeeklyResetAt = periodEnd
			}
		}
	}
	billingPeriodEnd := strings.TrimSpace(config.BillingPeriodEnd)
	if billing.MonthlyPresent {
		billing.MonthlyResetAt = firstNonEmpty(billing.MonthlyResetAt, billingPeriodEnd)
	} else if billing.WeeklyPresent {
		billing.WeeklyResetAt = firstNonEmpty(billing.WeeklyResetAt, billingPeriodEnd)
	}
	for _, product := range config.ProductUsage {
		billing.ProductUsage = append(billing.ProductUsage, xaiBillingProductUsage{
			Product: strings.TrimSpace(product.Product), UsagePercent: product.UsagePercent,
		})
	}
	if billing.UsedCents != nil {
		included := *billing.UsedCents
		if billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0 {
			included = min(included, *billing.MonthlyLimitCents)
		}
		billing.IncludedUsedCents = &included
		if billing.OnDemandUsedCents == nil && billing.MonthlyLimitCents != nil {
			onDemandUsed := max(0, *billing.UsedCents-*billing.MonthlyLimitCents)
			billing.OnDemandUsedCents = &onDemandUsed
		}
	}
	if !billing.WeeklyPresent && !billing.MonthlyPresent && billing.WeeklyUsagePercent == nil && billing.WeeklyResetAt == "" && len(billing.ProductUsage) == 0 &&
		billing.OnDemandCapCents == nil && billing.OnDemandUsedCents == nil &&
		billing.MonthlyLimitCents == nil && billing.UsedCents == nil && billing.MonthlyResetAt == "" {
		return nil
	}
	return billing
}

func xaiCentValue(cent *xaiUsageCent) *float64 {
	if cent == nil {
		return nil
	}
	return cent.Val
}

func mergeXAIBillingSummary(current, next *xaiBillingSummary) *xaiBillingSummary {
	if current == nil {
		return next
	}
	if next == nil {
		return current
	}
	current.WeeklyPresent = current.WeeklyPresent || next.WeeklyPresent
	current.MonthlyPresent = current.MonthlyPresent || next.MonthlyPresent
	if current.WeeklyUsagePercent == nil && next.WeeklyUsagePercent != nil {
		current.WeeklyUsagePercent = next.WeeklyUsagePercent
	}
	if current.WeeklyResetAt == "" && next.WeeklyResetAt != "" {
		current.WeeklyResetAt = next.WeeklyResetAt
	}
	if len(current.ProductUsage) == 0 && len(next.ProductUsage) > 0 {
		current.ProductUsage = next.ProductUsage
	}
	if current.OnDemandCapCents == nil && next.OnDemandCapCents != nil {
		current.OnDemandCapCents = next.OnDemandCapCents
	}
	if current.OnDemandUsedCents == nil && next.OnDemandUsedCents != nil {
		current.OnDemandUsedCents = next.OnDemandUsedCents
	}
	if current.MonthlyLimitCents == nil && next.MonthlyLimitCents != nil {
		current.MonthlyLimitCents = next.MonthlyLimitCents
	}
	if current.UsedCents == nil && next.UsedCents != nil {
		current.UsedCents = next.UsedCents
		current.IncludedUsedCents = next.IncludedUsedCents
	}
	if current.MonthlyResetAt == "" && next.MonthlyResetAt != "" {
		current.MonthlyResetAt = next.MonthlyResetAt
	}
	return current
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
	summary, err := s.refreshOAuthUsage(c.Request.Context(), id)
	if err != nil {
		RespondError(c, oauthUsageHTTPStatus(err), err)
		return
	}
	RespondJSON(c, http.StatusOK, summary)
}

// HandleOAuthUsageBatchStream refreshes OAuth quota with bounded server-side
// concurrency and streams per-channel results as SSE events.
func (s *Server) HandleOAuthUsageBatchStream(c *gin.Context) {
	var request oauthUsageBatchRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	channelIDs := normalizeBatchChannelIDs(request.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "channel_ids must not be empty")
		return
	}
	if len(channelIDs) > maxOAuthUsageBatchChannels {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel_ids must contain at most %d channels", maxOAuthUsageBatchChannels))
		return
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")
	disableResponseWriteTimeout(c.Writer, "OAuth usage batch stream")
	c.Status(http.StatusOK)

	total := len(channelIDs)
	if err := writeSSEEvent(c, "start", oauthUsageBatchEvent{Event: "start", Total: total}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	processed, succeeded, failed := 0, 0, 0
	for result := range s.runOAuthUsageBatch(ctx, channelIDs) {
		processed++
		if result.Status == "succeeded" {
			succeeded++
		} else {
			failed++
		}
		event := oauthUsageBatchEvent{
			Event:     "progress",
			Processed: processed,
			Total:     total,
			Succeeded: succeeded,
			Failed:    failed,
			Result:    &result,
		}
		if err := writeSSEEvent(c, "progress", event); err != nil {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	_ = writeSSEEvent(c, "complete", oauthUsageBatchEvent{
		Event:     "complete",
		Processed: processed,
		Total:     total,
		Succeeded: succeeded,
		Failed:    failed,
	})
}

func (s *Server) refreshOAuthUsage(ctx context.Context, id int64) (*oauthUsageSummary, error) {
	cfg, err := s.store.GetConfig(ctx, id)
	if err != nil {
		return nil, errOAuthUsageChannelNotFound
	}
	requestedAt := time.Now().UTC()
	requestCtx, cancel := context.WithTimeout(ctx, oauthUsageTimeout)
	defer cancel()
	summary, err := s.oauthUsageSummary(requestCtx, cfg)
	if err != nil {
		return nil, err
	}
	sampledAt := time.Now().UTC()
	summary, err = s.persistOAuthUsage(ctx, cfg, summary, requestedAt, sampledAt)
	if err != nil {
		return nil, errOAuthUsagePersistFailed
	}
	return summary, nil
}

func oauthUsageHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errOAuthUsageChannelNotFound):
		return http.StatusNotFound
	case errors.Is(err, errOAuthUsageUnsupported):
		return http.StatusConflict
	case errors.Is(err, errCodexUsageManagerUnavailable),
		errors.Is(err, errAnthropicManagerUnavailable),
		errors.Is(err, errAntigravityManagerUnavailable),
		errors.Is(err, errXAIUsageManagerUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, errOAuthUsagePersistFailed):
		return http.StatusInternalServerError
	default:
		return http.StatusBadGateway
	}
}

func (s *Server) runOAuthUsageBatch(ctx context.Context, channelIDs []int64) <-chan oauthUsageBatchResult {
	results := make(chan oauthUsageBatchResult)
	go func() {
		defer close(results)
		jobs := make(chan int64)
		workerCount := min(oauthUsageBatchWorkers, len(channelIDs))
		var workers sync.WaitGroup
		workers.Add(workerCount)
		for range workerCount {
			go func() {
				defer workers.Done()
				for channelID := range jobs {
					usage, err := s.refreshOAuthUsage(ctx, channelID)
					result := oauthUsageBatchResult{ChannelID: channelID, Status: "succeeded", Usage: usage}
					if err != nil {
						result.Status = "failed"
						result.Usage = nil
						result.Error = err.Error()
					}
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}()
		}

		for _, channelID := range channelIDs {
			select {
			case jobs <- channelID:
			case <-ctx.Done():
				close(jobs)
				workers.Wait()
				return
			}
		}
		close(jobs)
		workers.Wait()
	}()
	return results
}

func (s *Server) persistOAuthUsage(
	ctx context.Context,
	cfg *model.Config,
	summary *oauthUsageSummary,
	requestedAt time.Time,
	sampledAt time.Time,
) (*oauthUsageSummary, error) {
	if s == nil || s.store == nil || cfg == nil || summary == nil {
		return nil, errors.New("OAuth usage persistence is unavailable")
	}
	snapshot, err := json.Marshal(persistedOAuthUsageSnapshot{
		RequestedAt: requestedAt.UTC().Format(time.RFC3339Nano),
		SampledAt:   sampledAt.UTC().Format(time.RFC3339Nano),
		Summary:     *summary,
	})
	if err != nil {
		return nil, fmt.Errorf("encode OAuth usage snapshot: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		currentCfg, err := s.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			return nil, fmt.Errorf("reload OAuth credential: %w", err)
		}

		var authType string
		var payload string
		switch {
		case currentCfg.UsesCodexOAuth() && summary.Provider == codexauth.ChannelType:
			authType = model.AuthTypeCodexOAuth
			credential, parseErr := codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			if persisted, ok := s.currentOAuthUsageAtOrAfter(
				currentCfg.ID, credential.OAuthUsage, summary.Provider, requestedAt,
			); ok {
				return persisted, nil
			}
			credential.OAuthUsage = append(json.RawMessage(nil), snapshot...)
			payload, err = credential.JSON()
		case currentCfg.UsesAnthropicOAuth() && summary.Provider == anthropicauth.ChannelType:
			authType = model.AuthTypeAnthropicOAuth
			credential, parseErr := anthropicauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			if persisted, ok := s.currentOAuthUsageAtOrAfter(
				currentCfg.ID, credential.OAuthUsage, summary.Provider, requestedAt,
			); ok {
				return persisted, nil
			}
			credential.OAuthUsage = append(json.RawMessage(nil), snapshot...)
			payload, err = credential.JSON()
		case currentCfg.UsesAntigravityOAuth() && summary.Provider == antigravityauth.ChannelType:
			authType = model.AuthTypeAntigravityOAuth
			credential, parseErr := antigravityauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			if persisted, ok := s.currentOAuthUsageAtOrAfter(
				currentCfg.ID, credential.OAuthUsage, summary.Provider, requestedAt,
			); ok {
				return persisted, nil
			}
			credential.OAuthUsage = append(json.RawMessage(nil), snapshot...)
			payload, err = credential.JSON()
		case currentCfg.UsesXAIOAuth() && summary.Provider == xaiauth.ChannelType:
			authType = model.AuthTypeXAIOAuth
			credential, parseErr := xaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, parseErr
			}
			if persisted, ok := s.currentOAuthUsageAtOrAfter(
				currentCfg.ID, credential.OAuthUsage, summary.Provider, requestedAt,
			); ok {
				return persisted, nil
			}
			credential.OAuthUsage = append(json.RawMessage(nil), snapshot...)
			payload, err = credential.JSON()
		default:
			return nil, errors.New("OAuth credential changed provider while persisting usage")
		}
		if err != nil {
			return nil, err
		}
		if len(payload) > maxOAuthCredentialBytes {
			return nil, errors.New("OAuth credential exceeds persistence limit")
		}
		updated, err := s.store.CompareAndSwapOAuthCredential(
			ctx, currentCfg.ID, authType, currentCfg.OAuthCredential, payload,
		)
		if err != nil {
			return nil, err
		}
		if !updated {
			continue
		}

		s.invalidateOAuthCredential(currentCfg.ID, summary.Provider)
		s.InvalidateChannelListCache()
		return summary, nil
	}
}

func (s *Server) invalidateOAuthCredential(channelID int64, provider string) {
	switch provider {
	case codexauth.ChannelType:
		s.codexCredentials.invalidateCredentialCache(channelID)
	case anthropicauth.ChannelType:
		s.anthropicCredentials.invalidate(channelID)
	case antigravityauth.ChannelType:
		s.antigravityCredentials.invalidate(channelID)
	case xaiauth.ChannelType:
		s.xaiCredentials.invalidate(channelID)
	}
}

func persistedOAuthUsage(raw json.RawMessage, provider string) (*oauthUsageSummary, time.Time, time.Time) {
	if len(raw) == 0 {
		return nil, time.Time{}, time.Time{}
	}
	var snapshot persistedOAuthUsageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Summary.Provider != provider {
		return nil, time.Time{}, time.Time{}
	}
	sampledAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(snapshot.SampledAt))
	if err != nil {
		return nil, time.Time{}, time.Time{}
	}
	requestedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(snapshot.RequestedAt))
	if err != nil {
		return nil, time.Time{}, time.Time{}
	}
	return &snapshot.Summary, sampledAt, requestedAt
}

func (s *Server) currentOAuthUsageAtOrAfter(
	channelID int64,
	raw json.RawMessage,
	provider string,
	requestedAt time.Time,
) (*oauthUsageSummary, bool) {
	persisted, _, persistedRequestAt := persistedOAuthUsage(raw, provider)
	if persisted == nil || persistedRequestAt.Before(requestedAt) {
		return nil, false
	}
	s.invalidateOAuthCredential(channelID, provider)
	s.InvalidateChannelListCache()
	return persisted, true
}

func latestOAuthUsage(
	active *oauthUsageSummary,
	activeSampledAt time.Time,
	passive *oauthUsageSummary,
	passiveSampledAt string,
) *oauthUsageSummary {
	if active == nil {
		return passive
	}
	if passive == nil {
		return active
	}
	passiveTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(passiveSampledAt))
	if err == nil && passiveTime.After(activeSampledAt) {
		return passive
	}
	return active
}

func (s *Server) oauthUsageSummary(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	cfg = s.withOAuthBaseURLOverride(cfg)
	switch {
	case cfg.UsesCodexOAuth():
		if s.codexCredentials == nil {
			return nil, errCodexUsageManagerUnavailable
		}
		credential, err := s.codexCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, oauthUsageCredentialRefreshError(err, "usage: Codex credential refresh failed")
		}
		return requestCodexUsage(ctx, s.getClientForChannel(cfg), credential)
	case cfg.UsesAnthropicOAuth():
		if s.anthropicCredentials == nil {
			return nil, errAnthropicManagerUnavailable
		}
		credential, err := s.anthropicCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, oauthUsageCredentialRefreshError(err, "usage: Anthropic credential refresh failed")
		}
		baseURL := anthropicauth.DefaultUpstreamURL
		if urls := cfg.GetURLs(); len(urls) > 0 {
			baseURL = urls[0]
		}
		summary, metadata, err := requestAnthropicUsage(ctx, s.getClientForChannel(cfg), credential, baseURL)
		if err != nil {
			return nil, err
		}
		updated, err := s.anthropicCredentials.updateMetadata(ctx, cfg, metadata)
		if err != nil {
			return nil, errors.New("usage: persist Anthropic subscription metadata failed")
		}
		if updated {
			s.InvalidateChannelListCache()
		}
		return summary, nil
	case cfg.UsesAntigravityOAuth():
		if s.antigravityCredentials == nil {
			return nil, errAntigravityManagerUnavailable
		}
		credential, err := s.antigravityCredentials.credentialWithMetadata(ctx, cfg)
		if err != nil {
			return nil, oauthUsageCredentialRefreshError(err, "usage: Antigravity credential refresh failed")
		}
		return requestAntigravityUsage(ctx, s.getClientForChannel(cfg), credential)
	case cfg.UsesXAIOAuth():
		if s.xaiCredentials == nil {
			return nil, errXAIUsageManagerUnavailable
		}
		credential, err := s.xaiCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, oauthUsageCredentialRefreshError(err, "usage: xAI credential refresh failed")
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
				return nil, oauthUsageCredentialRefreshError(err, "usage: xAI credential refresh failed")
			}
		}
		return nil, errors.New("usage: xAI credential was rejected")
	default:
		return nil, errOAuthUsageUnsupported
	}
}
