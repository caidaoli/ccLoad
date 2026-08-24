package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	codexQuotaHeaderPrefix            = "x-codex-"
	maxCodexPassiveUsageSSEEventBytes = 64 << 10
	codexPassiveUsageSSEEventType     = "codex.rate_limits"
	codexPassiveUsageQueueSize        = 256
)

type codexPassiveUsageTask struct {
	channelID int64
	update    codexPassiveUsageUpdate
}

func (s *Server) persistCodexPassiveUsage(ctx context.Context, cfg *model.Config, resp *http.Response) {
	if s == nil || s.codexCredentials == nil || cfg == nil || !cfg.UsesCodexOAuth() || resp == nil {
		return
	}
	statusOK := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if !statusOK && resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	if update, ok := sampleCodexPassiveUsage(resp.Header, time.Now().UTC()); ok {
		s.persistCodexPassiveUsageUpdate(ctx, cfg, update)
	}
	if resp.Body == nil {
		return
	}
	resp.Body = &codexPassiveUsageReadCloser{
		ReadCloser: resp.Body,
		onUpdate: func(update codexPassiveUsageUpdate) {
			s.enqueueCodexPassiveUsage(cfg.ID, update)
		},
	}
}

func (s *Server) enqueueCodexPassiveUsage(channelID int64, update codexPassiveUsageUpdate) {
	if s == nil || s.codexPassiveUsageCh == nil || channelID <= 0 || len(update.Windows) == 0 || s.isShuttingDown.Load() {
		return
	}
	select {
	case s.codexPassiveUsageCh <- codexPassiveUsageTask{channelID: channelID, update: update}:
	default:
		if dropped := s.codexPassiveUsageDropCount.Add(1); dropped%100 == 1 {
			log.Printf("[WARN] Codex passive usage queue full; dropped updates=%d", dropped)
		}
	}
}

func (s *Server) codexPassiveUsageWorker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.shutdownCh:
			return
		case task := <-s.codexPassiveUsageCh:
			s.persistCodexPassiveUsageUpdate(s.baseCtx, &model.Config{
				ID: task.channelID, AuthType: model.AuthTypeCodexOAuth,
			}, task.update)
		}
	}
}

func (s *Server) persistCodexPassiveUsageUpdate(ctx context.Context, cfg *model.Config, update codexPassiveUsageUpdate) {
	if len(update.Windows) == 0 {
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

type codexPassiveUsageReadCloser struct {
	io.ReadCloser
	pending  bytes.Buffer
	done     bool
	onUpdate func(codexPassiveUsageUpdate)
}

func (r *codexPassiveUsageReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.observe(p[:n])
	}
	return n, err
}

func (r *codexPassiveUsageReadCloser) observe(chunk []byte) {
	if r == nil || r.done || len(chunk) == 0 {
		return
	}
	remaining := maxCodexPassiveUsageSSEEventBytes - r.pending.Len()
	if remaining <= 0 {
		r.done = true
		r.pending.Reset()
		return
	}
	truncated := len(chunk) > remaining
	if truncated {
		chunk = chunk[:remaining]
	}
	_, _ = r.pending.Write(chunk)
	for {
		rawEvent, ok := nextSSEEvent(&r.pending)
		if !ok {
			break
		}
		update, sampled := sampleCodexPassiveUsageEvent(sseEventData(rawEvent), time.Now().UTC())
		if !sampled {
			continue
		}
		r.done = true
		r.pending.Reset()
		if r.onUpdate != nil {
			r.onUpdate(update)
		}
		return
	}
	if truncated {
		r.done = true
		r.pending.Reset()
	}
}

type codexPassiveUsageSSEWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	WindowMinutes      int64    `json:"window_minutes"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

type codexPassiveUsageSSERateLimit struct {
	Primary         *codexPassiveUsageSSEWindow `json:"primary"`
	Secondary       *codexPassiveUsageSSEWindow `json:"secondary"`
	PrimaryWindow   *codexPassiveUsageSSEWindow `json:"primary_window"`
	SecondaryWindow *codexPassiveUsageSSEWindow `json:"secondary_window"`
}

type codexPassiveUsageSSEEvent struct {
	Type                 string                                    `json:"type"`
	RateLimits           *codexPassiveUsageSSERateLimit            `json:"rate_limits"`
	CodeReviewRateLimits *codexPassiveUsageSSERateLimit            `json:"code_review_rate_limits"`
	AdditionalRateLimits map[string]*codexPassiveUsageSSERateLimit `json:"additional_rate_limits"`
}

func sampleCodexPassiveUsageEvent(payload []byte, sampledAt time.Time) (codexPassiveUsageUpdate, bool) {
	var event codexPassiveUsageSSEEvent
	if json.Unmarshal(payload, &event) != nil || strings.TrimSpace(event.Type) != codexPassiveUsageSSEEventType {
		return codexPassiveUsageUpdate{}, false
	}
	update := codexPassiveUsageUpdate{
		Windows:   make([]codexauth.PassiveUsageWindow, 0, 4),
		SampledAt: sampledAt.UTC().Format(time.RFC3339Nano),
	}
	update.Windows = appendCodexPassiveEventRateLimit(update.Windows, event.RateLimits, "codex", "codex", sampledAt)
	update.Windows = appendCodexPassiveEventRateLimit(update.Windows, event.CodeReviewRateLimits, "code_review", "code_review", sampledAt)
	additionalNames := make([]string, 0, len(event.AdditionalRateLimits))
	for name := range event.AdditionalRateLimits {
		additionalNames = append(additionalNames, name)
	}
	sort.Strings(additionalNames)
	for _, name := range additionalNames {
		limitName := strings.TrimSpace(name)
		if limitName == "" {
			continue
		}
		update.Windows = appendCodexPassiveEventRateLimit(
			update.Windows, event.AdditionalRateLimits[name], limitName, limitName, sampledAt,
		)
	}
	if len(update.Windows) == 0 {
		return codexPassiveUsageUpdate{}, false
	}
	return update, true
}

func appendCodexPassiveEventRateLimit(
	windows []codexauth.PassiveUsageWindow,
	rateLimit *codexPassiveUsageSSERateLimit,
	scope, limitName string,
	sampledAt time.Time,
) []codexauth.PassiveUsageWindow {
	if rateLimit == nil {
		return windows
	}
	primary := rateLimit.Primary
	if primary == nil {
		primary = rateLimit.PrimaryWindow
	}
	secondary := rateLimit.Secondary
	if secondary == nil {
		secondary = rateLimit.SecondaryWindow
	}
	windows = appendCodexPassiveEventWindow(windows, primary, scope, limitName, "primary", sampledAt)
	return appendCodexPassiveEventWindow(windows, secondary, scope, limitName, "secondary", sampledAt)
}

func appendCodexPassiveEventWindow(
	windows []codexauth.PassiveUsageWindow,
	window *codexPassiveUsageSSEWindow,
	scope, limitName, kind string,
	sampledAt time.Time,
) []codexauth.PassiveUsageWindow {
	if window == nil || window.UsedPercent == nil || !validOAuthUsedPercent(*window.UsedPercent) {
		return windows
	}
	windowSeconds := window.WindowMinutes * 60
	if windowSeconds <= 0 {
		windowSeconds = window.LimitWindowSeconds
	}
	if windowSeconds <= 0 {
		return windows
	}
	resetAt := window.ResetAt
	if resetAt > 1e11 {
		resetAt /= 1000
	}
	if resetAt <= 0 && window.ResetAfterSeconds > 0 {
		resetAt = sampledAt.Unix() + window.ResetAfterSeconds
	}
	return append(windows, codexauth.PassiveUsageWindow{
		Scope:              strings.ToLower(strings.TrimSpace(scope)),
		LimitName:          strings.TrimSpace(limitName),
		Kind:               kind,
		UsedPercent:        *window.UsedPercent,
		LimitWindowSeconds: windowSeconds,
		ResetAt:            max(resetAt, 0),
		SampledAt:          sampledAt.UTC().Format(time.RFC3339Nano),
	})
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
	if !ok || !validOAuthUsedPercent(usedPercent) {
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
		UsedPercent:        usedPercent,
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
		if !validOAuthUsedPercent(window.UsedPercent) {
			continue
		}
		sampledAt, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(window.SampledAt))
		usedPercent := window.UsedPercent
		summary.Windows = append(summary.Windows, oauthUsageWindow{
			LimitName:          window.LimitName,
			Kind:               window.Kind,
			UsedPercent:        usedPercent,
			RemainingPercent:   100 - usedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAt:            window.ResetAt,
			SampledAt:          sampledAt,
		})
	}
	if len(summary.Windows) == 0 {
		return nil
	}
	return summary
}
