package accioauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Usage expresses the quota in its original percentage unit. Subscription
// quantities are independent and retain their declared unit, if any.
type Usage struct {
	UsagePercent            float64       `json:"usage_percent"`
	RemainingPercent        float64       `json:"remaining_percent"`
	RefreshCountdownSeconds *float64      `json:"refresh_countdown_seconds,omitempty"`
	Unit                    string        `json:"unit"`
	Subscription            *Subscription `json:"subscription,omitempty"`
}

// Subscription contains only public entitlement fields, never raw account data.
// ExpiresAt retains the upstream timestamp encoding rather than guessing its unit.
type Subscription struct {
	PlanName       string          `json:"plan_name,omitempty"`
	Status         string          `json:"status,omitempty"`
	ExpiresAt      json.RawMessage `json:"expires_at,omitempty"`
	RemainingQuota *float64        `json:"remaining_quota,omitempty"`
	TotalQuota     *float64        `json:"total_quota,omitempty"`
	Unit           string          `json:"unit,omitempty"`
}

// FetchUsage queries quota and optional subscription. Subscription failures leave
// the quota available, except authentication failures requiring a token refresh.
func (s *Service) FetchUsage(ctx context.Context, credential *Credential) (*Usage, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	data, err := s.authDo(ctx, c, http.MethodGet, "/api/entitlement/quota", nil)
	if err != nil {
		return nil, err
	}
	data = authData(data)
	used := authNumber(data, "usagePercent", "usage_percent")
	if used == nil || *used < 0 || *used > 100 {
		return nil, errors.New("invalid Accio usage percentage")
	}
	usage := &Usage{UsagePercent: *used, RemainingPercent: 100 - *used, Unit: "%", RefreshCountdownSeconds: authNumber(data, "refreshCountdownSeconds", "refresh_countdown_seconds")}
	if usage.RefreshCountdownSeconds != nil && *usage.RefreshCountdownSeconds < 0 {
		return nil, errors.New("invalid Accio quota reset countdown")
	}
	data, err = s.authDo(ctx, c, http.MethodGet, "/api/entitlement/currentSubscription", nil)
	if err != nil {
		var upstream *HTTPError
		if errors.As(err, &upstream) && upstream.StatusCode == http.StatusUnauthorized || ctx.Err() != nil {
			return nil, err
		}
		return usage, nil
	}
	if string(data["data"]) == "null" {
		return usage, nil
	}
	data = authData(data)
	subscription := &Subscription{PlanName: authText(data, "planName", "plan_name", "productName", "name"), Status: authText(data, "status", "state"), RemainingQuota: authNumber(data, "remainQuota", "remain", "remaining", "leftQuota"), TotalQuota: authNumber(data, "totalQuota", "total", "quota"), Unit: authText(data, "unit")}
	for _, key := range []string{"expireAt", "expiresAt", "endTime", "end_time"} {
		if value := data[key]; len(value) != 0 && string(value) != "null" {
			// Whitelist scalar timestamps; never echo arbitrary nested response data.
			var text string
			var number float64
			if json.Unmarshal(value, &text) == nil || json.Unmarshal(value, &number) == nil {
				subscription.ExpiresAt = value
			}
			break
		}
	}
	usage.Subscription = subscription
	return usage, nil
}

func authNumber(data map[string]json.RawMessage, keys ...string) *float64 {
	for _, key := range keys {
		var number float64
		if raw := data[key]; len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &number) == nil {
			return &number
		}
	}
	return nil
}
