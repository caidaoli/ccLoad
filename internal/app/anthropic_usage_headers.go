package app

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/model"
)

const (
	anthropicRateLimit5hStatus      = "anthropic-ratelimit-unified-5h-status"
	anthropicRateLimit5hUtilization = "anthropic-ratelimit-unified-5h-utilization"
	anthropicRateLimit5hReset       = "anthropic-ratelimit-unified-5h-reset"
	anthropicRateLimit7dUtilization = "anthropic-ratelimit-unified-7d-utilization"
	anthropicRateLimit7dReset       = "anthropic-ratelimit-unified-7d-reset"
	anthropicRateLimit7dOIUsage     = "anthropic-ratelimit-unified-7d_oi-utilization"
	anthropicRateLimit7dOIReset     = "anthropic-ratelimit-unified-7d_oi-reset"
)

func (s *Server) persistAnthropicPassiveUsage(ctx context.Context, cfg *model.Config, resp *http.Response) {
	if s == nil || s.anthropicCredentials == nil || cfg == nil || !cfg.UsesAnthropicOAuth() || resp == nil {
		return
	}
	statusOK := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if (!statusOK || strings.TrimSpace(resp.Header.Get(anthropicRateLimit5hStatus)) == "") &&
		resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	update, ok := sampleAnthropicPassiveUsage(resp.Header, time.Now().UTC())
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, err := s.anthropicCredentials.updatePassiveUsage(persistCtx, cfg, update); err != nil {
		log.Printf("[WARN] persist Anthropic passive usage: channel_id=%d err=%v", cfg.ID, err)
	}
}

func sampleAnthropicPassiveUsage(headers http.Header, sampledAt time.Time) (anthropicPassiveUsageUpdate, bool) {
	update := anthropicPassiveUsageUpdate{}
	update.FiveHour = sampleAnthropicPassiveWindow(headers, anthropicRateLimit5hUtilization, anthropicRateLimit5hReset)
	update.SevenDay = sampleAnthropicPassiveWindow(headers, anthropicRateLimit7dUtilization, anthropicRateLimit7dReset)
	update.SevenDayOverageIncluded = sampleAnthropicPassiveWindow(headers, anthropicRateLimit7dOIUsage, anthropicRateLimit7dOIReset)
	if update.FiveHour == nil && update.SevenDay == nil && update.SevenDayOverageIncluded == nil {
		return anthropicPassiveUsageUpdate{}, false
	}
	update.SampledAt = sampledAt.UTC().Format(time.RFC3339Nano)
	stampAnthropicPassiveWindow(update.FiveHour, update.SampledAt)
	stampAnthropicPassiveWindow(update.SevenDay, update.SampledAt)
	stampAnthropicPassiveWindow(update.SevenDayOverageIncluded, update.SampledAt)
	return update, true
}

func stampAnthropicPassiveWindow(window *anthropicauth.PassiveUsageWindow, sampledAt string) {
	if window != nil {
		window.SampledAt = strings.TrimSpace(sampledAt)
	}
}

func sampleAnthropicPassiveWindow(headers http.Header, utilizationHeader, resetHeader string) *anthropicauth.PassiveUsageWindow {
	window := &anthropicauth.PassiveUsageWindow{}
	if raw := strings.TrimSpace(headers.Get(utilizationHeader)); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) &&
			value >= 0 && value <= 1 {
			window.Utilization = &value
		}
	}
	if raw := strings.TrimSpace(headers.Get(resetHeader)); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			if value > 1e11 {
				value /= 1000
			}
			window.ResetAt = &value
		}
	}
	if window.Utilization == nil && window.ResetAt == nil {
		return nil
	}
	return window
}

func anthropicPassiveUsageSummary(credential *anthropicauth.Credential) *oauthUsageSummary {
	if credential == nil || credential.PassiveUsage == nil {
		return nil
	}
	summary := &oauthUsageSummary{
		Provider: anthropicauth.ChannelType,
		PlanType: strings.TrimSpace(credential.PlanType),
		Windows:  make([]oauthUsageWindow, 0, 3),
	}
	summary.Windows = appendAnthropicPassiveWindow(summary.Windows, "", "five_hour", 5*60*60, credential.PassiveUsage.FiveHour)
	summary.Windows = appendAnthropicPassiveWindow(summary.Windows, "", "seven_day", weeklyUsageWindowSeconds, credential.PassiveUsage.SevenDay)
	summary.Windows = appendAnthropicPassiveWindow(
		summary.Windows, "Claude Fable", "seven_day_fable", weeklyUsageWindowSeconds,
		credential.PassiveUsage.SevenDayOverageIncluded,
	)
	if len(summary.Windows) == 0 {
		return nil
	}
	return summary
}

func appendAnthropicPassiveWindow(
	windows []oauthUsageWindow,
	limitName, kind string,
	windowSeconds int64,
	window *anthropicauth.PassiveUsageWindow,
) []oauthUsageWindow {
	if window == nil || window.Utilization == nil {
		return windows
	}
	usedPercent := *window.Utilization * 100
	if !validOAuthUsedPercent(usedPercent) {
		return windows
	}
	resetAt := int64(0)
	if window.ResetAt != nil {
		resetAt = *window.ResetAt
	}
	sampledAt := time.Time{}
	if !window.UtilizationStale {
		sampledAt, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(window.SampledAt))
	}
	return append(windows, oauthUsageWindow{
		LimitName: limitName, Kind: kind, UsedPercent: usedPercent, RemainingPercent: 100 - usedPercent,
		LimitWindowSeconds: windowSeconds, ResetAt: resetAt, SampledAt: sampledAt,
	})
}
