package cursorauth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// PeriodUsage is one billing-cycle snapshot from GetCurrentPeriodUsage.
type PeriodUsage struct {
	PlanType       string
	DisplayMessage string
	Windows        []QuotaWindow
}

// QuotaWindow is one allowance window projected onto ccLoad's usage summary.
type QuotaWindow struct {
	Name               string
	Kind               string
	UsedPercent        float64
	RemainingPercent   float64
	LimitWindowSeconds int64
	ResetAt            int64
}

type periodUsagePayload struct {
	BillingCycleStart json.RawMessage `json:"billingCycleStart"`
	BillingCycleEnd   json.RawMessage `json:"billingCycleEnd"`
	DisplayMessage    string          `json:"displayMessage"`
	PlanUsage         *planUsage      `json:"planUsage"`
	SpendLimitUsage   *struct {
		LimitType string `json:"limitType"`
	} `json:"spendLimitUsage"`
}

type planUsage struct {
	TotalSpend      *float64 `json:"totalSpend"`
	Limit           *float64 `json:"limit"`
	Remaining       *float64 `json:"remaining"`
	APIPercentUsed  *float64 `json:"apiPercentUsed"`
	AutoPercentUsed *float64 `json:"autoPercentUsed"`
}

// FetchPeriodUsage reads the current Cursor billing-cycle allowance.
func (s *Service) FetchPeriodUsage(ctx context.Context, accessToken string) (*PeriodUsage, error) {
	var payload periodUsagePayload
	if err := s.connectJSON(ctx, UsageRPC, accessToken, map[string]any{}, &payload, "usage"); err != nil {
		return nil, err
	}
	return normalizePeriodUsage(&payload)
}

func normalizePeriodUsage(payload *periodUsagePayload) (*PeriodUsage, error) {
	if payload == nil || payload.PlanUsage == nil {
		return nil, errors.New("cursor usage response has no plan usage")
	}
	startMS := jsonInt64(payload.BillingCycleStart)
	endMS := jsonInt64(payload.BillingCycleEnd)
	windowSeconds := int64(0)
	if startMS > 0 && endMS > startMS {
		windowSeconds = (endMS - startMS) / 1000
	}
	resetAt := int64(0)
	if endMS > 0 {
		resetAt = endMS / 1000
	}
	usage := &PeriodUsage{
		PlanType:       strings.TrimSpace(payload.SpendLimitUsageLimitType()),
		DisplayMessage: strings.TrimSpace(payload.DisplayMessage),
		Windows:        make([]QuotaWindow, 0, 3),
	}
	if used := percentOf(payload.PlanUsage.TotalSpend, payload.PlanUsage.Limit); used != nil {
		usage.Windows = append(usage.Windows, quotaWindow("included", *used, windowSeconds, resetAt))
	}
	if payload.PlanUsage.APIPercentUsed != nil {
		usage.Windows = append(usage.Windows, quotaWindow("api", *payload.PlanUsage.APIPercentUsed, windowSeconds, resetAt))
	}
	if payload.PlanUsage.AutoPercentUsed != nil {
		usage.Windows = append(usage.Windows, quotaWindow("auto", *payload.PlanUsage.AutoPercentUsed, windowSeconds, resetAt))
	}
	if len(usage.Windows) == 0 {
		return nil, errors.New("cursor usage response has no windows")
	}
	return usage, nil
}

func (p *periodUsagePayload) SpendLimitUsageLimitType() string {
	if p == nil || p.SpendLimitUsage == nil {
		return ""
	}
	return p.SpendLimitUsage.LimitType
}

func quotaWindow(name string, usedPercent float64, windowSeconds, resetAt int64) QuotaWindow {
	used := math.Min(math.Max(usedPercent, 0), 100)
	return QuotaWindow{
		Name:               name,
		Kind:               "spend",
		UsedPercent:        used,
		RemainingPercent:   math.Round((100-used)*100) / 100,
		LimitWindowSeconds: windowSeconds,
		ResetAt:            resetAt,
	}
}

func percentOf(used, total *float64) *float64 {
	if used == nil || total == nil || *total <= 0 {
		return nil
	}
	value := math.Round((*used / *total)*10000) / 100
	return &value
}

func jsonInt64(raw json.RawMessage) int64 {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	trimmed = strings.Trim(trimmed, `"`)
	var value float64
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return 0
	}
	return int64(value)
}
