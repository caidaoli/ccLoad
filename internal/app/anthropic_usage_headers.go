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

const anthropicPassiveUsageQueueSize = 256

type anthropicPassiveUsageTask struct {
	channelID int64
	update    anthropicPassiveUsageUpdate
}

// 额度响应头是旁路元数据，持久化要经过凭证 CAS 与成本对账，绝不能占用
// 代理转发路径——CAS 冲突退避和日志对账都会直接计入客户端 TTFB。
func (s *Server) persistAnthropicPassiveUsage(cfg *model.Config, resp *http.Response) {
	s.observeAnthropicPassiveUsage(cfg, resp, func(update anthropicPassiveUsageUpdate) {
		s.enqueueAnthropicPassiveUsage(cfg.ID, update)
	})
}

// 检测同步写日志，必须先保存响应头中的额度窗口，避免首次检测成本丢失。
func (s *Server) persistDetectionAnthropicPassiveUsage(ctx context.Context, cfg *model.Config, resp *http.Response) {
	s.observeAnthropicPassiveUsage(cfg, resp, func(update anthropicPassiveUsageUpdate) {
		s.persistAnthropicPassiveUsageUpdate(ctx, cfg, update)
	})
}

func (s *Server) observeAnthropicPassiveUsage(
	cfg *model.Config, resp *http.Response, onUpdate func(anthropicPassiveUsageUpdate),
) {
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
	onUpdate(update)
}

func (s *Server) enqueueAnthropicPassiveUsage(channelID int64, update anthropicPassiveUsageUpdate) {
	if s == nil || s.anthropicPassiveUsageCh == nil || channelID <= 0 || s.isShuttingDown.Load() {
		return
	}
	select {
	case s.anthropicPassiveUsageCh <- anthropicPassiveUsageTask{channelID: channelID, update: update}:
	default:
		if dropped := s.anthropicPassiveUsageDropCount.Add(1); dropped%100 == 1 {
			log.Printf("[WARN] Anthropic passive usage queue full; dropped updates=%d", dropped)
		}
	}
}

func (s *Server) anthropicPassiveUsageWorker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.shutdownCh:
			return
		case task := <-s.anthropicPassiveUsageCh:
			s.persistAnthropicPassiveUsageUpdate(s.baseCtx, &model.Config{
				ID: task.channelID, AuthType: model.AuthTypeAnthropicOAuth,
			}, task.update)
		}
	}
}

func (s *Server) persistAnthropicPassiveUsageUpdate(
	ctx context.Context, cfg *model.Config, update anthropicPassiveUsageUpdate,
) {
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
