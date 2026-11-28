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

/**
 * @brief 仅查询账号的百分比额度及可选的重置倒计时。
 * @param ctx 请求上下文；取消时中止请求。
 * @param credential 当前 Accio 账号凭证。
 * @return 不含订阅信息的额度；失败时为 nil。
 * @return 凭证、额度或传输无效时返回错误；上游拒绝为 *HTTPError；成功时为 nil。
 */
func (s *Service) FetchQuota(ctx context.Context, credential *Credential) (*Usage, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	return s.fetchQuota(ctx, c)
}

/**
 * @brief 解码账号的百分比额度及以秒计的重置倒计时。
 * @param ctx 请求上下文；取消时中止请求。
 * @param c 已归一化的 Accio 账号凭证。
 * @return 额度记录；失败时为 nil。
 * @return 额度或传输无效时返回错误；上游拒绝为 *HTTPError；成功时为 nil。
 */
func (s *Service) fetchQuota(ctx context.Context, c *Credential) (*Usage, error) {
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
	return usage, nil
}

/**
 * @brief 查询百分比额度并附加可用的订阅信息。
 * @param ctx 请求上下文；取消时中止请求。
 * @param credential 当前 Accio 账号凭证。
 * @return 额度与可选订阅；失败时为 nil。
 * @return 额度查询失败、订阅认证失败或取消时返回错误；上游拒绝为 *HTTPError；成功时为 nil。
 */
func (s *Service) FetchUsage(ctx context.Context, credential *Credential) (*Usage, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	usage, err := s.fetchQuota(ctx, c)
	if err != nil {
		return nil, err
	}
	data, err := s.authDo(ctx, c, http.MethodGet, "/api/entitlement/currentSubscription", nil)
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
