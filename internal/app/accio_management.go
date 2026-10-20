package app

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"
)

func (s *Server) requestAccioUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	c, err := s.accioCredentials.credential(ctx, cfg, false, "")
	if err != nil {
		return nil, err
	}
	service := *s.accioService
	service.Client = s.getClientForChannel(cfg)
	usage, err := service.FetchUsage(ctx, c)
	var endpointErr *accioauth.HTTPError
	if errors.As(err, &endpointErr) && endpointErr.StatusCode == http.StatusUnauthorized {
		c, err = s.accioCredentials.credential(ctx, cfg, true, c.AccessToken)
		if err == nil {
			usage, err = service.FetchUsage(ctx, c)
		}
	}
	if err != nil {
		return nil, err
	}
	window := oauthUsageWindow{Kind: "accio", LimitName: "Accio", UsedPercent: usage.UsagePercent, RemainingPercent: usage.RemainingPercent}
	if countdown := usage.RefreshCountdownSeconds; countdown != nil && *countdown <= float64(math.MaxInt64-time.Now().Unix()) {
		window.ResetAt = time.Now().Unix() + int64(math.Ceil(*countdown))
	}
	result := &oauthUsageSummary{Provider: accioauth.ChannelType, AccioUsage: usage, Windows: []oauthUsageWindow{window}}
	if usage.Subscription != nil {
		result.PlanType = usage.Subscription.PlanName
		result.EntitlementStatus = usage.Subscription.Status
	}
	return result, nil
}
