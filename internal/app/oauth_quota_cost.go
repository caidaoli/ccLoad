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
	"ccLoad/internal/zaiauth"
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
	case cfg.UsesZAIOAuth():
		credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		// The Coding Plan meters its own quota, so ccLoad stores the usage
		// snapshot but tracks no standard-cost windows for it.
		return &oauthUsageCredentialState{
			provider: zaiauth.ChannelType, authType: model.AuthTypeZAIOAuth,
			oauthUsage: credential.OAuthUsage,
			encode: func(usage json.RawMessage, _ *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
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

// oauthQuotaSamples 把一次上游额度采样转成持久化槽位。转换规则与凭证里
// oauth_usage 快照的重建路径共用 oauthcost 的实现，避免两处漂移。
func oauthQuotaSamples(summary *oauthUsageSummary) []oauthcost.Sample {
	if summary == nil {
		return nil
	}
	snapshot := oauthQuotaSnapshotSummary(summary)
	return snapshot.Samples()
}

// oauthQuotaSnapshotSummary 把内存里的采样摘要投影成 oauthcost 的快照形状，
// 只保留重建窗口边界所需的字段。
func oauthQuotaSnapshotSummary(summary *oauthUsageSummary) oauthcost.SnapshotSummary {
	snapshot := oauthcost.SnapshotSummary{Provider: summary.Provider}
	if len(summary.Windows) > 0 {
		snapshot.Windows = make([]oauthcost.SnapshotWindow, 0, len(summary.Windows))
		for _, window := range summary.Windows {
			snapshot.Windows = append(snapshot.Windows, oauthcost.SnapshotWindow{
				LimitName:          window.LimitName,
				Kind:               window.Kind,
				LimitWindowSeconds: window.LimitWindowSeconds,
				ResetAt:            window.ResetAt,
			})
		}
	}
	if summary.XAIBilling != nil {
		snapshot.XAIBilling = &oauthcost.SnapshotBilling{
			WeeklyPresent:  summary.XAIBilling.WeeklyPresent,
			WeeklyResetAt:  summary.XAIBilling.WeeklyResetAt,
			MonthlyPresent: summary.XAIBilling.MonthlyPresent,
			MonthlyResetAt: summary.XAIBilling.MonthlyResetAt,
		}
	}
	return snapshot
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
