package app

import (
	"context"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
)

const (
	codexQuotaHeaderPrefix = "x-codex-"
)

func (s *Server) persistCodexPassiveUsage(ctx context.Context, cfg *model.Config, resp *http.Response) {
	if s == nil || s.codexCredentials == nil || cfg == nil || !cfg.UsesCodexOAuth() || resp == nil {
		return
	}
	statusOK := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if !statusOK && resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	update, ok := sampleCodexPassiveUsage(resp.Header, time.Now().UTC())
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	updated, err := s.codexCredentials.updatePassiveUsage(persistCtx, cfg, update)
	if err != nil {
		log.Printf("[WARN] persist Codex passive usage: channel_id=%d err=%v", cfg.ID, err)
		return
	}
	if updated {
		// Quota metadata does not affect routing. Only expire the channel data
		// snapshot; resetting balancers and protocol capability learning here would
		// turn every quota change into unrelated routing churn.
		if cache := s.getChannelCache(); cache != nil {
			cache.InvalidateCache()
		}
	}
}

func sampleCodexPassiveUsage(headers http.Header, sampledAt time.Time) (codexPassiveUsageUpdate, bool) {
	update := codexPassiveUsageUpdate{
		Windows:   make([]codexauth.PassiveUsageWindow, 0, 4),
		SampledAt: sampledAt.UTC().Format(time.RFC3339Nano),
	}
	update.Windows = appendCodexPassiveHeaderWindow(update.Windows, headers, "x-codex", "codex", "codex", "primary", sampledAt)
	update.Windows = appendCodexPassiveHeaderWindow(update.Windows, headers, "x-codex", "codex", "codex", "secondary", sampledAt)

	for _, group := range codexAdditionalQuotaGroups(headers) {
		base := codexQuotaHeaderPrefix + group
		limitName := strings.TrimSpace(headers.Get(base + "-limit-name"))
		if limitName == "" {
			limitName = group
		}
		update.Windows = appendCodexPassiveHeaderWindow(update.Windows, headers, base, group, limitName, "primary", sampledAt)
		update.Windows = appendCodexPassiveHeaderWindow(update.Windows, headers, base, group, limitName, "secondary", sampledAt)
	}
	if len(update.Windows) == 0 {
		return codexPassiveUsageUpdate{}, false
	}
	return update, true
}

func codexAdditionalQuotaGroups(headers http.Header) []string {
	groups := make(map[string]struct{})
	suffixes := [...]string{
		"-limit-name",
		"-primary-used-percent",
		"-secondary-used-percent",
	}
	for name := range headers {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(lowerName, codexQuotaHeaderPrefix) {
			continue
		}
		rest := strings.TrimPrefix(lowerName, codexQuotaHeaderPrefix)
		if strings.HasPrefix(rest, "primary-") || strings.HasPrefix(rest, "secondary-") {
			continue
		}
		for _, suffix := range suffixes {
			if group := strings.TrimSuffix(rest, suffix); group != rest && group != "" {
				groups[group] = struct{}{}
				break
			}
		}
	}
	result := make([]string, 0, len(groups))
	for group := range groups {
		result = append(result, group)
	}
	sort.Strings(result)
	return result
}

func appendCodexPassiveHeaderWindow(
	windows []codexauth.PassiveUsageWindow,
	headers http.Header,
	base, scope, limitName, kind string,
	sampledAt time.Time,
) []codexauth.PassiveUsageWindow {
	prefix := base + "-" + kind + "-"
	usedPercent, ok := parseCodexHeaderFloat(headers.Get(prefix + "used-percent"))
	if !ok {
		return windows
	}
	windowMinutes, _ := parseCodexHeaderInt(headers.Get(prefix + "window-minutes"))
	if windowMinutes <= 0 {
		return windows
	}
	resetAt, _ := parseCodexHeaderInt(headers.Get(prefix + "reset-at"))
	if resetAt > 1e11 {
		resetAt /= 1000
	}
	if resetAt <= 0 {
		if resetAfter, ok := parseCodexHeaderInt(headers.Get(prefix + "reset-after-seconds")); ok && resetAfter > 0 {
			resetAt = sampledAt.Unix() + resetAfter
		}
	}
	return append(windows, codexauth.PassiveUsageWindow{
		Scope:              strings.ToLower(strings.TrimSpace(scope)),
		LimitName:          strings.TrimSpace(limitName),
		Kind:               kind,
		UsedPercent:        min(max(usedPercent, 0), 100),
		LimitWindowSeconds: windowMinutes * 60,
		ResetAt:            max(resetAt, 0),
		SampledAt:          sampledAt.UTC().Format(time.RFC3339Nano),
	})
}

func cloneCodexQuotaHeaders(headers http.Header) http.Header {
	cloned := make(http.Header)
	for name, values := range headers {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), codexQuotaHeaderPrefix) {
			continue
		}
		cloned[name] = append([]string(nil), values...)
	}
	return cloned
}

func parseCodexHeaderFloat(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func parseCodexHeaderInt(raw string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func codexPassiveUsageSummary(credential *codexauth.Credential) *oauthUsageSummary {
	if credential == nil || credential.PassiveUsage == nil || len(credential.PassiveUsage.Windows) == 0 {
		return nil
	}
	summary := &oauthUsageSummary{
		Provider: codexauth.ChannelType,
		PlanType: strings.TrimSpace(credential.PlanType),
		Windows:  make([]oauthUsageWindow, 0, len(credential.PassiveUsage.Windows)),
	}
	for _, window := range credential.PassiveUsage.Windows {
		usedPercent := min(max(window.UsedPercent, 0), 100)
		summary.Windows = append(summary.Windows, oauthUsageWindow{
			LimitName:          window.LimitName,
			Kind:               window.Kind,
			UsedPercent:        usedPercent,
			RemainingPercent:   100 - usedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAt:            window.ResetAt,
		})
	}
	return summary
}
