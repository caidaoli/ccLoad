package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
)

const (
	opencodeGoUsageProvider  = "opencode-go"
	opencodeGoUsagePath      = "/zen/go/v1/usage"
	opencodeGoUsageUserAgent = "opencode/1.0"
	opencodeGoRollingSeconds = 5 * 60 * 60
	opencodeGoWeeklySeconds  = 7 * 24 * 60 * 60
)

type opencodeGoUsageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type opencodeGoUsagePayload struct {
	Usage struct {
		Rolling *opencodeGoUsageWindow `json:"rolling"`
		Weekly  *opencodeGoUsageWindow `json:"weekly"`
		Monthly *opencodeGoUsageWindow `json:"monthly"`
	} `json:"usage"`
}

func (s *Server) requestOpenCodeGoUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("usage: OpenCode Go store is unavailable")
	}
	keys, err := s.store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("usage: load OpenCode Go API keys: %w", err)
	}
	apiKey := firstOpenCodeGoAPIKey(keys)
	if apiKey == "" {
		return nil, errors.New("usage: OpenCode Go channel has no API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+opencodeHost+opencodeGoUsagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("usage: build OpenCode Go request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", opencodeGoUsageUserAgent)

	resp, err := s.getClientForChannel(cfg).Do(req)
	if err != nil {
		return nil, &oauthUsageRequestError{provider: opencodeGoUsageProvider}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthUsageResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("usage: read OpenCode Go response: %w", err)
	}
	if len(body) > maxOAuthUsageResponseBytes {
		return nil, errors.New("usage: OpenCode Go response exceeds size limit")
	}
	if resp.StatusCode != http.StatusOK {
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			return nil, errors.New(trimmed)
		}
		return nil, fmt.Errorf("usage: OpenCode Go endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload opencodeGoUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("usage: decode OpenCode Go response: %w", err)
	}
	return normalizeOpenCodeGoUsage(&payload)
}

func firstOpenCodeGoAPIKey(keys []*model.APIKey) string {
	for _, key := range keys {
		if key == nil || key.Disabled {
			continue
		}
		if apiKey := strings.TrimSpace(key.APIKey); apiKey != "" {
			return apiKey
		}
	}
	return ""
}

func normalizeOpenCodeGoUsage(payload *opencodeGoUsagePayload) (*oauthUsageSummary, error) {
	if payload == nil {
		return nil, errors.New("usage: OpenCode Go response is empty")
	}
	summary := &oauthUsageSummary{
		Provider: opencodeGoUsageProvider,
		PlanType: "go",
		Windows:  make([]oauthUsageWindow, 0, 3),
	}
	type mappedWindow struct {
		name          string
		kind          string
		windowSeconds int64
		raw           *opencodeGoUsageWindow
	}
	for _, item := range []mappedWindow{
		{name: "five_hour", kind: "rolling", windowSeconds: opencodeGoRollingSeconds, raw: payload.Usage.Rolling},
		{name: "weekly", kind: "weekly", windowSeconds: opencodeGoWeeklySeconds, raw: payload.Usage.Weekly},
		{name: "monthly", kind: "monthly", raw: payload.Usage.Monthly},
	} {
		if item.raw == nil {
			continue
		}
		usedPercent := min(max(item.raw.Percent, 0), 100)
		resetAt := parseOpenCodeGoResetAt(item.raw.ResetsAt)
		windowSeconds := item.windowSeconds
		if item.kind == "monthly" && resetAt > 0 {
			if remaining := resetAt - time.Now().UTC().Unix(); remaining > 0 {
				windowSeconds = remaining
			}
		}
		summary.Windows = append(summary.Windows, oauthUsageWindow{
			LimitName:          item.name,
			Kind:               item.kind,
			UsedPercent:        usedPercent,
			RemainingPercent:   100 - usedPercent,
			LimitWindowSeconds: windowSeconds,
			ResetAt:            resetAt,
		})
		if status := strings.TrimSpace(item.raw.Status); status != "" && !strings.EqualFold(status, "ok") {
			summary.EntitlementStatus = status
		}
	}
	if len(summary.Windows) == 0 {
		return nil, errors.New("usage: OpenCode Go response has no quota windows")
	}
	return summary, nil
}

func parseOpenCodeGoResetAt(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil {
		return 0
	}
	return parsed.UTC().Unix()
}
