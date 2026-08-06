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

	"github.com/gin-gonic/gin"
)

const (
	codexUsageURL              = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent        = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	antigravityUsageURL        = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	antigravityUsageUserAgent  = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	oauthUsageTimeout          = 30 * time.Second
	maxOAuthUsageResponseBytes = 1 << 20
	weeklyUsageWindowSeconds   = 7 * 24 * 60 * 60
)

var (
	errOAuthUsageUnsupported         = errors.New("usage: channel does not use a supported OAuth provider")
	errCodexUsageManagerUnavailable  = errors.New("usage: Codex credential manager is unavailable")
	errAntigravityManagerUnavailable = errors.New("usage: Antigravity credential manager is unavailable")
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
	Provider string             `json:"provider"`
	PlanType string             `json:"plan_type,omitempty"`
	Windows  []oauthUsageWindow `json:"windows"`
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, errors.New("usage: Codex request is unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUsageUserAgent)
	if credential.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
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
		return nil, fmt.Errorf("usage: %s request returned HTTP %d", provider, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthUsageResponseBytes+1))
	if err != nil || len(body) > maxOAuthUsageResponseBytes {
		return nil, fmt.Errorf("usage: %s response is invalid", provider)
	}
	return body, nil
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
		case errors.Is(err, errCodexUsageManagerUnavailable), errors.Is(err, errAntigravityManagerUnavailable):
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
		credential, err := s.antigravityCredentials.credential(ctx, cfg, false)
		if err != nil {
			return nil, errors.New("usage: Antigravity credential refresh failed")
		}
		return requestAntigravityUsage(ctx, s.getClientForChannel(cfg), credential)
	default:
		return nil, errOAuthUsageUnsupported
	}
}
