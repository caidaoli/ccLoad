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
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"
	"ccLoad/internal/zedauth"
)

type oauthUsageCredentialState struct {
	provider        string
	authType        string
	oauthUsage      json.RawMessage
	quotaCostUsage  *oauthcost.Usage
	tracksQuotaCost bool
	encode          func(json.RawMessage, *oauthcost.Usage) (string, error)
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
			tracksQuotaCost: true,
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
			tracksQuotaCost: true,
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
			tracksQuotaCost: true,
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
			tracksQuotaCost: true,
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
	case cfg.UsesCursorOAuth():
		credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		// Cursor meters included spend itself; ccLoad stores the snapshot but
		// tracks no standard-cost windows for it.
		return &oauthUsageCredentialState{
			provider: cursorauth.ChannelType, authType: model.AuthTypeCursorOAuth,
			oauthUsage: credential.OAuthUsage,
			encode: func(usage json.RawMessage, _ *oauthcost.Usage) (string, error) {
				credential.OAuthUsage = append(json.RawMessage(nil), usage...)
				return credential.JSON()
			},
		}, nil
	case cfg.UsesZedOAuth():
		credential, err := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, err
		}
		return &oauthUsageCredentialState{
			provider: zedauth.ChannelType, authType: model.AuthTypeZedOAuth,
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
	if summary != nil && summary.Partial {
		return oauthcost.ReconcilePartial(current, oauthQuotaSamples(summary), observedAt)
	}
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

// pruneCodexPassiveQuotaCostUsage mirrors ReplaceScopes for cost windows.
// ReconcilePartial intentionally retains omitted siblings; a marked passive
// scope is the one exception where omitted keys in that scope are stale and
// must be retired. Scope and limit-name matching cover both current records
// and legacy records whose scope metadata is incomplete.
func pruneCodexPassiveQuotaCostUsage(
	usage *oauthcost.Usage,
	current *codexauth.PassiveUsage,
	update codexPassiveUsageUpdate,
) *oauthcost.Usage {
	if usage == nil || len(update.ReplaceScopes) == 0 {
		return usage
	}
	scopes := make(map[string]struct{}, len(update.ReplaceScopes))
	for _, scope := range update.ReplaceScopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			scopes[scope] = struct{}{}
		}
	}
	if len(scopes) == 0 {
		return usage
	}
	incomingKeys := make(map[string]struct{}, len(update.Windows))
	incomingLimitNames := make(map[string]struct{}, len(update.Windows))
	for _, window := range update.Windows {
		key := oauthcost.Key(window.LimitName, window.Kind)
		if key == "" {
			continue
		}
		incomingKeys[key] = struct{}{}
		if scope := codexPassiveWindowScope(window); scope != "" {
			if _, replace := scopes[scope]; replace {
				if limitName := strings.ToLower(strings.TrimSpace(window.LimitName)); limitName != "" {
					incomingLimitNames[limitName] = struct{}{}
				}
			}
		}
	}
	currentScopes := make(map[string]string)
	if current != nil {
		for _, window := range current.Windows {
			key := oauthcost.Key(window.LimitName, window.Kind)
			if key != "" {
				currentScopes[key] = codexPassiveWindowScope(window)
			}
		}
	}
	// ReconcilePartial can reject an old layout using a newer active sample.
	// Its scope must then survive this pruning step as well.
	if sampledAt, err := time.Parse(time.RFC3339Nano, update.SampledAt); err == nil {
		for _, window := range usage.Windows {
			if window != nil && window.SampledUpstreamAtUnixNano > sampledAt.UnixNano() {
				delete(scopes, currentScopes[window.Key])
				limitName := strings.ToLower(strings.TrimSpace(strings.SplitN(window.Key, "|", 2)[0]))
				delete(incomingLimitNames, limitName)
			}
		}
	}
	retained := usage.Windows[:0]
	for _, window := range usage.Windows {
		if window == nil {
			retained = append(retained, window)
			continue
		}
		limitName := strings.ToLower(strings.TrimSpace(strings.SplitN(window.Key, "|", 2)[0]))
		_, replaceByScope := scopes[currentScopes[window.Key]]
		_, replaceByLimit := incomingLimitNames[limitName]
		if replaceByScope || replaceByLimit {
			if _, incoming := incomingKeys[window.Key]; !incoming {
				continue
			}
		}
		retained = append(retained, window)
	}
	usage.Windows = retained
	return usage
}

func codexPassiveWindowScope(window codexauth.PassiveUsageWindow) string {
	scope := strings.ToLower(strings.TrimSpace(window.Scope))
	if scope == "" {
		scope = strings.ToLower(strings.TrimSpace(window.LimitName))
	}
	return scope
}

// oauthQuotaSnapshotSummary 把内存里的采样摘要投影成 oauthcost 的快照形状，
// 只保留重建窗口和识别上游提前重置所需的字段。
func oauthQuotaSnapshotSummary(summary *oauthUsageSummary) oauthcost.SnapshotSummary {
	snapshot := oauthcost.SnapshotSummary{Provider: summary.Provider}
	if len(summary.Windows) > 0 {
		snapshot.Windows = make([]oauthcost.SnapshotWindow, 0, len(summary.Windows))
		for _, window := range summary.Windows {
			snapshotWindow := oauthcost.SnapshotWindow{
				LimitName:          window.LimitName,
				Kind:               window.Kind,
				LimitWindowSeconds: window.LimitWindowSeconds,
				ResetAt:            window.ResetAt,
			}
			if !window.SampledAt.IsZero() {
				usedPercent := window.UsedPercent
				snapshotWindow.UsedPercent = &usedPercent
				snapshotWindow.SampledAt = window.SampledAt
			}
			snapshot.Windows = append(snapshot.Windows, snapshotWindow)
		}
	}
	if summary.XAIBilling != nil {
		snapshot.XAIBilling = &oauthcost.SnapshotBilling{
			WeeklyPresent:     summary.XAIBilling.WeeklyPresent,
			WeeklyUsedPercent: summary.XAIBilling.WeeklyUsagePercent,
			WeeklyResetAt:     summary.XAIBilling.WeeklyResetAt,
			MonthlyPresent:    summary.XAIBilling.MonthlyPresent,
			MonthlyLimitCents: summary.XAIBilling.MonthlyLimitCents,
			MonthlyUsedCents:  summary.XAIBilling.IncludedUsedCents,
			MonthlyResetAt:    summary.XAIBilling.MonthlyResetAt,
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
			if window := oauthcost.Find(usage, oauthcost.Key(windows[i].LimitName, windows[i].Kind)); window != nil &&
				oauthQuotaCostMatchesSampledWindow(windows[i], window) {
				cost := window.StandardCostMicroUSD
				windows[i].StandardCostMicroUSD = &cost
			}
		}
		clone.Windows = windows
	}
	return &clone
}

func oauthQuotaCostMatchesSampledWindow(sample oauthUsageWindow, cost *oauthcost.Window) bool {
	if cost == nil || cost.WindowSeconds <= 0 || cost.ResetAt <= 0 || sample.ResetAt <= 0 {
		return false
	}
	var delta uint64
	if cost.ResetAt >= sample.ResetAt {
		delta = uint64(cost.ResetAt) - uint64(sample.ResetAt)
	} else {
		delta = uint64(sample.ResetAt) - uint64(cost.ResetAt)
	}
	return delta <= uint64(cost.WindowSeconds-1)/2
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
