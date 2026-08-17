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

const monthlyUsageWindowMinimumSeconds = 28 * 24 * 60 * 60
const monthlyUsageWindowMaximumSeconds = 31 * 24 * 60 * 60

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
	weeklyReset, monthlyReset := oauthQuotaResetTimes(summary)
	return oauthcost.Reconcile(current, weeklyReset, monthlyReset, observedAt)
}

func oauthQuotaResetTimes(summary *oauthUsageSummary) (*time.Time, *time.Time) {
	if summary == nil {
		return nil, nil
	}
	var weeklyReset, monthlyReset *time.Time
	for _, window := range summary.Windows {
		if window.ResetAt <= 0 {
			continue
		}
		resetAt := time.Unix(window.ResetAt, 0).UTC()
		switch {
		case weeklyReset == nil && window.LimitWindowSeconds == weeklyUsageWindowSeconds:
			weeklyReset = &resetAt
		case monthlyReset == nil && window.LimitWindowSeconds >= monthlyUsageWindowMinimumSeconds &&
			window.LimitWindowSeconds <= monthlyUsageWindowMaximumSeconds:
			monthlyReset = &resetAt
		}
	}
	if summary.XAIBilling != nil {
		if weeklyReset == nil && summary.XAIBilling.WeeklyPresent {
			weeklyReset = parseOAuthQuotaResetTime(summary.XAIBilling.WeeklyResetAt)
		}
		if monthlyReset == nil && summary.XAIBilling.MonthlyPresent {
			monthlyReset = parseOAuthQuotaResetTime(summary.XAIBilling.MonthlyResetAt)
		}
	}
	return weeklyReset, monthlyReset
}

func parseOAuthQuotaResetTime(raw string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func attachOAuthQuotaCostUsage(summary *oauthUsageSummary, usage *oauthcost.Usage) *oauthUsageSummary {
	if summary == nil {
		return nil
	}
	clone := *summary
	clone.QuotaCostUsage = oauthcost.Clone(usage)
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
