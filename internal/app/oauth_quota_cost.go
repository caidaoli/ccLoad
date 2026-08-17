package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/xaiauth"
)

// xAI 的额度不在 windows 数组里，按固定标识合成窗口槽位。
const (
	xaiQuotaWindowLimitName = "xai"
	xaiMonthlyWindowSeconds = 30 * 24 * 60 * 60
)

type oauthUsageCredentialState struct {
	provider       string
	authType       string
	oauthUsage     json.RawMessage
	quotaCostUsage *oauthcost.Usage
	encode         func(json.RawMessage, *oauthcost.Usage) (string, error)
}

func parseOAuthUsageCredentialState(cfg *model.Config) (*oauthUsageCredentialState, error) {
	if cfg == nil || strings.TrimSpace(cfg.OAuthCredential) == "" {
		return nil, errors.New("OAuth credential is unavailable")
	}
	switch {
	case cfg.UsesCodexOAuth():
		credential, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		return &oauthUsageCredentialState{
			provider: codexauth.ChannelType, authType: model.AuthTypeCodexOAuth,
			oauthUsage: credential.OAuthUsage, quotaCostUsage: credential.QuotaCostUsage,
			encode: func(usage json.RawMessage, costUsage *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
				credential.QuotaCostUsage = oauthcost.Clone(costUsage)
				return credential.JSON()
			},
		}, nil
	case cfg.UsesAnthropicOAuth():
		credential, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		return &oauthUsageCredentialState{
			provider: anthropicauth.ChannelType, authType: model.AuthTypeAnthropicOAuth,
			oauthUsage: credential.OAuthUsage, quotaCostUsage: credential.QuotaCostUsage,
			encode: func(usage json.RawMessage, costUsage *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
				credential.QuotaCostUsage = oauthcost.Clone(costUsage)
				return credential.JSON()
			},
		}, nil
	case cfg.UsesAntigravityOAuth():
		credential, err := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		return &oauthUsageCredentialState{
			provider: antigravityauth.ChannelType, authType: model.AuthTypeAntigravityOAuth,
			oauthUsage: credential.OAuthUsage, quotaCostUsage: credential.QuotaCostUsage,
			encode: func(usage json.RawMessage, costUsage *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
				credential.QuotaCostUsage = oauthcost.Clone(costUsage)
				return credential.JSON()
			},
		}, nil
	case cfg.UsesXAIOAuth():
		credential, err := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		return &oauthUsageCredentialState{
			provider: xaiauth.ChannelType, authType: model.AuthTypeXAIOAuth,
			oauthUsage: credential.OAuthUsage, quotaCostUsage: credential.QuotaCostUsage,
			encode: func(usage json.RawMessage, costUsage *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
				credential.QuotaCostUsage = oauthcost.Clone(costUsage)
				return credential.JSON()
			},
		}, nil
	default:
		return nil, errOAuthUsageUnsupported
	}
}

func reconcileOAuthQuotaCostUsage(
	current *oauthcost.Usage,
	summary *oauthUsageSummary,
	observedAt time.Time,
) *oauthcost.Usage {
	return oauthcost.Reconcile(current, oauthQuotaSamples(summary), observedAt)
}

// oauthQuotaSamples 把一次上游额度采样转成持久化槽位。槽位身份是上游的
// (limit_name, kind)，同一时长可以对应多个互不相干的窗口（Antigravity 的
// gemini/3p 周额度、Anthropic 的三个 7 天窗口），只按时长归并必然错位。
func oauthQuotaSamples(summary *oauthUsageSummary) []oauthcost.Sample {
	if summary == nil {
		return nil
	}
	samples := make([]oauthcost.Sample, 0, len(summary.Windows)+2)
	for _, window := range summary.Windows {
		key := oauthcost.Key(window.LimitName, window.Kind)
		if key == "" || window.ResetAt <= 0 || window.LimitWindowSeconds <= 0 {
			continue
		}
		samples = append(samples, oauthcost.Sample{
			Key:           key,
			Family:        oauthQuotaWindowFamily(summary.Provider, window.LimitName, window.Kind),
			WindowSeconds: window.LimitWindowSeconds,
			ResetAt:       time.Unix(window.ResetAt, 0).UTC(),
		})
	}
	if summary.XAIBilling != nil {
		if summary.XAIBilling.WeeklyPresent {
			if resetAt := parseOAuthQuotaResetTime(summary.XAIBilling.WeeklyResetAt); resetAt != nil {
				samples = append(samples, oauthcost.Sample{
					Key:           oauthcost.Key(xaiQuotaWindowLimitName, "weekly"),
					WindowSeconds: weeklyUsageWindowSeconds,
					ResetAt:       *resetAt,
				})
			}
		}
		if summary.XAIBilling.MonthlyPresent {
			if resetAt := parseOAuthQuotaResetTime(summary.XAIBilling.MonthlyResetAt); resetAt != nil {
				samples = append(samples, oauthcost.Sample{
					Key:           oauthcost.Key(xaiQuotaWindowLimitName, "monthly"),
					WindowSeconds: xaiMonthlyWindowSeconds,
					ResetAt:       *resetAt,
				})
			}
		}
	}
	return samples
}

// oauthQuotaWindowFamily 把 provider 的窗口标识映射到模型族：只覆盖部分模型的
// 窗口不能累加其他模型的消耗。
func oauthQuotaWindowFamily(provider, limitName, kind string) string {
	limitName = strings.ToLower(strings.TrimSpace(limitName))
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch provider {
	case antigravityauth.ChannelType:
		// Antigravity 的 bucketId 带族前缀（gemini-* / 3p-*）。
		switch {
		case strings.HasPrefix(kind, "gemini"):
			return oauthcost.FamilyGemini
		case strings.HasPrefix(kind, "3p"):
			return oauthcost.FamilyNonGemini
		}
	case anthropicauth.ChannelType:
		switch kind {
		case "seven_day_sonnet":
			return oauthcost.FamilySonnet
		case "seven_day_fable":
			return oauthcost.FamilyFable
		}
	case codexauth.ChannelType:
		// codex-spark 是附加额度窗口，只覆盖 Spark 模型；主 codex 窗口覆盖全部。
		if strings.Contains(limitName, "spark") {
			return oauthcost.FamilySpark
		}
	}
	return oauthcost.FamilyAll
}

func parseOAuthQuotaResetTime(raw string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

// attachOAuthQuotaCostUsage 把每个上游窗口的累计成本内联到窗口本身，
// 前端按窗口渲染即可，不必再从时长反查槽位。
func attachOAuthQuotaCostUsage(summary *oauthUsageSummary, usage *oauthcost.Usage) *oauthUsageSummary {
	if summary == nil {
		return nil
	}
	clone := *summary
	clone.QuotaCostUsage = oauthcost.Clone(usage)
	if len(summary.Windows) > 0 {
		windows := make([]oauthUsageWindow, len(summary.Windows))
		copy(windows, summary.Windows)
		for i := range windows {
			windows[i].StandardCostMicroUSD = nil
			if window := oauthcost.Find(usage, oauthcost.Key(windows[i].LimitName, windows[i].Kind)); window != nil {
				cost := window.StandardCostMicroUSD
				windows[i].StandardCostMicroUSD = &cost
			}
		}
		clone.Windows = windows
	}
	return &clone
}

func (s *Server) resetOAuthQuotaCostUsage(ctx context.Context, channelID int64, resetAt time.Time) error {
	if s == nil || s.store == nil {
		return errors.New("OAuth quota cost persistence is unavailable")
	}
	if err := s.store.ResetOAuthQuotaCostUsage(ctx, channelID, resetAt); err != nil {
		return err
	}
	s.invalidateOAuthCredential(channelID, codexauth.ChannelType)
	return nil
}
