package app

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestSub2APIBalanceUsesAuthMeAndSummaryWithoutCheckin(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	steps := []sub2APIStep{
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
			status: http.StatusOK, body: `{"code":0,"message":"","metadata":{},"data":{"balance":12.5}}`,
		},
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/summary",
			status: http.StatusOK,
			body: `{"code":0,"message":"","metadata":{},"data":{"active_count":2,"total_used_usd":38,"subscriptions":[` +
				`{"id":9,"group_name":"Pro","daily_used_usd":2,"daily_limit_usd":10,"weekly_limit_usd":70,"monthly_used_usd":30,"monthly_limit_usd":0,"expires_at":"2026-12-31T00:00:00Z"},` +
				`{"id":2,"group_name":"Basic","daily_used_usd":1,"weekly_used_usd":5,"weekly_limit_usd":20,"monthly_used_usd":6,"monthly_limit_usd":24,"expires_at":"2026-09-30T00:00:00Z"}` +
				`]}}`,
		},
	}
	script := &sub2APIScript{t: t, steps: steps}
	service := newChannelManagementService(nil, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	service.now = func() time.Time { return fixedNow }

	snapshot, status, err := service.refreshSub2APIBalance(
		context.Background(),
		&model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
		sub2APITestEnvelope(model.ChannelManagementProfileSub2API),
	)
	if err != nil {
		t.Fatalf("refreshSub2APIBalance: %v", err)
	}
	if status != http.StatusOK || snapshot == nil || snapshot.BalanceUSD == nil || *snapshot.BalanceUSD != 12.5 {
		t.Fatalf("balance result = (%#v, %d)", snapshot, status)
	}
	if snapshot.RemainingRaw != nil || snapshot.UsedRaw != nil || snapshot.TotalRaw != nil || snapshot.Divisor != 0 || snapshot.SampledAt != fixedNow {
		t.Fatalf("Sub2API balance representation = %#v", snapshot)
	}
	want := []model.ChannelManagementSubscriptionSnapshot{
		{ID: 2, Name: "Basic", Window: "daily", UsedUSD: float64Ptr(1), ExpiresAt: "2026-09-30T00:00:00Z"},
		{ID: 2, Name: "Basic", Window: "weekly", UsedUSD: float64Ptr(5), LimitUSD: float64Ptr(20), AvailablePercent: float64Ptr(75), ExpiresAt: "2026-09-30T00:00:00Z"},
		{ID: 2, Name: "Basic", Window: "monthly", UsedUSD: float64Ptr(6), LimitUSD: float64Ptr(24), AvailablePercent: float64Ptr(75), ExpiresAt: "2026-09-30T00:00:00Z"},
		{ID: 9, Name: "Pro", Window: "daily", UsedUSD: float64Ptr(2), LimitUSD: float64Ptr(10), AvailablePercent: float64Ptr(80), ExpiresAt: "2026-12-31T00:00:00Z"},
		{ID: 9, Name: "Pro", Window: "weekly", LimitUSD: float64Ptr(70), ExpiresAt: "2026-12-31T00:00:00Z"},
		{ID: 9, Name: "Pro", Window: "monthly", UsedUSD: float64Ptr(30), LimitUSD: float64Ptr(0), ExpiresAt: "2026-12-31T00:00:00Z"},
	}
	assertSub2APISubscriptions(t, snapshot.Subscriptions, want)
	requests := script.finishedRequests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.authorization != "Bearer private-token" || request.userID != "" {
			t.Fatalf("Sub2API headers = %#v", request)
		}
		if strings.Contains(request.target, "/redeem/checkin") {
			t.Fatalf("standard Sub2API requested checkin endpoint: %s", request.target)
		}
	}
}

func TestSub2APIBalanceSummaryFallbacksOnceToActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		summaryStatus int
		summaryBody   string
	}{
		{name: "summary not found", summaryStatus: http.StatusNotFound, summaryBody: `{"code":404,"message":"secret","data":null}`},
		{name: "summary method not allowed", summaryStatus: http.StatusMethodNotAllowed, summaryBody: `{"code":405,"message":"secret","data":null}`},
		{name: "summary business failure", summaryStatus: http.StatusOK, summaryBody: `{"code":17,"message":"secret","reason":"NO_SUMMARY","data":null}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			steps := []sub2APIStep{
				{
					method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
					status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":9.25}}`,
				},
				{
					method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/summary",
					status: tt.summaryStatus, body: tt.summaryBody,
				},
				{
					method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/active",
					status: http.StatusOK,
					body: `{"code":0,"message":"","data":[` +
						`{"id":8,"daily_usage_usd":3,"weekly_usage_usd":4,"monthly_usage_usd":5,"expires_at":"2026-10-01T00:00:00Z","group":null},` +
						`{"id":3,"daily_usage_usd":1,"weekly_usage_usd":2,"expires_at":"2026-11-01T00:00:00Z","group":{"name":"Team","daily_limit_usd":4,"weekly_limit_usd":0,"monthly_limit_usd":12}}` +
						`]}`,
				},
			}
			script := &sub2APIScript{t: t, steps: steps}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})

			snapshot, status, err := service.refreshSub2APIBalance(
				context.Background(),
				&model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
				sub2APITestEnvelope(model.ChannelManagementProfileSub2APIPro),
			)
			if err != nil || status != http.StatusOK || snapshot == nil {
				t.Fatalf("fallback balance = (%#v, %d, %v)", snapshot, status, err)
			}
			want := []model.ChannelManagementSubscriptionSnapshot{
				{ID: 3, Name: "Team", Window: "daily", UsedUSD: float64Ptr(1), LimitUSD: float64Ptr(4), AvailablePercent: float64Ptr(75), ExpiresAt: "2026-11-01T00:00:00Z"},
				{ID: 3, Name: "Team", Window: "weekly", UsedUSD: float64Ptr(2), LimitUSD: float64Ptr(0), ExpiresAt: "2026-11-01T00:00:00Z"},
				{ID: 3, Name: "Team", Window: "monthly", LimitUSD: float64Ptr(12), ExpiresAt: "2026-11-01T00:00:00Z"},
				{ID: 8, Window: "daily", UsedUSD: float64Ptr(3), ExpiresAt: "2026-10-01T00:00:00Z"},
				{ID: 8, Window: "weekly", UsedUSD: float64Ptr(4), ExpiresAt: "2026-10-01T00:00:00Z"},
				{ID: 8, Window: "monthly", UsedUSD: float64Ptr(5), ExpiresAt: "2026-10-01T00:00:00Z"},
			}
			assertSub2APISubscriptions(t, snapshot.Subscriptions, want)
			requests := script.finishedRequests()
			active := 0
			for _, request := range requests {
				if strings.HasSuffix(request.target, "/subscriptions/active") {
					active++
				}
				if request.userID != "" {
					t.Fatalf("Sub2API sent New-API-User: %#v", request)
				}
			}
			if len(requests) != 3 || active != 1 {
				t.Fatalf("fallback requests = %#v", requests)
			}
		})
	}
}

func TestSub2APIBalanceKeepsAuthBalanceWhenSubscriptionsFail(t *testing.T) {
	t.Parallel()
	summaryTarget := "https://sub2.example.com/api/v1/subscriptions/summary"
	activeTarget := "https://sub2.example.com/api/v1/subscriptions/active"
	tests := []struct {
		name string
		tail []sub2APIStep
	}{
		{
			name: "summary transport error",
			tail: []sub2APIStep{{
				method: http.MethodGet, target: summaryTarget, err: errors.New("raw-upstream-secret"),
			}},
		},
		{
			name: "summary HTTP error does not probe active",
			tail: []sub2APIStep{{
				method: http.MethodGet, target: summaryTarget, status: http.StatusBadGateway,
				body: `{"code":0,"message":"raw-upstream-secret","data":{"subscriptions":[]}}`,
			}},
		},
		{
			name: "summary malformed does not probe active",
			tail: []sub2APIStep{{
				method: http.MethodGet, target: summaryTarget, status: http.StatusOK, body: `raw-upstream-secret`,
			}},
		},
		{
			name: "summary structure invalid does not probe active",
			tail: []sub2APIStep{{
				method: http.MethodGet, target: summaryTarget, status: http.StatusOK,
				body: `{"code":0,"message":"raw-upstream-secret","data":{"active_count":1}}`,
			}},
		},
		{
			name: "summary and active HTTP unavailable",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusNotFound,
					body: `{"code":404,"message":"raw-upstream-secret","data":null}`,
				},
				{
					method: http.MethodGet, target: activeTarget, status: http.StatusBadGateway,
					body: `{"code":502,"message":"raw-upstream-secret","data":null}`,
				},
			},
		},
		{
			name: "active transport error",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusMethodNotAllowed,
					body: `{"code":405,"message":"raw-upstream-secret","data":null}`,
				},
				{method: http.MethodGet, target: activeTarget, err: errors.New("raw-upstream-secret")},
			},
		},
		{
			name: "active business failure",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusOK,
					body: `{"code":17,"message":"raw-upstream-secret","data":null}`,
				},
				{
					method: http.MethodGet, target: activeTarget, status: http.StatusOK,
					body: `{"code":18,"message":"raw-upstream-secret","data":null}`,
				},
			},
		},
		{
			name: "active malformed",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusNotFound,
					body: `{"code":404,"message":"raw-upstream-secret","data":null}`,
				},
				{method: http.MethodGet, target: activeTarget, status: http.StatusOK, body: `raw-upstream-secret`},
			},
		},
		{
			name: "active structure invalid",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusNotFound,
					body: `{"code":404,"message":"raw-upstream-secret","data":null}`,
				},
				{
					method: http.MethodGet, target: activeTarget, status: http.StatusOK,
					body: `{"code":0,"message":"raw-upstream-secret","data":null}`,
				},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			steps := append([]sub2APIStep{{
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
				status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":6.25}}`,
			}}, tt.tail...)
			script := &sub2APIScript{t: t, steps: steps}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			snapshot, status, err := service.refreshSub2APIBalance(
				context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
				sub2APITestEnvelope(model.ChannelManagementProfileSub2API),
			)
			if err != nil || status != http.StatusOK || snapshot == nil || snapshot.BalanceUSD == nil ||
				*snapshot.BalanceUSD != 6.25 || len(snapshot.Subscriptions) != 0 {
				t.Fatalf("degraded balance = (%#v, %d, %v)", snapshot, status, err)
			}
			requests := script.finishedRequests()
			if len(requests) != len(steps) {
				t.Fatalf("subscription failure request count = %d, want %d", len(requests), len(steps))
			}
			active := 0
			for _, request := range requests {
				if request.target == activeTarget {
					active++
				}
			}
			if active > 1 {
				t.Fatalf("active fallback repeated: %#v", requests)
			}
		})
	}
}

func TestSub2APISubscriptionPercentageRequiresFinitePair(t *testing.T) {
	t.Parallel()
	items := []sub2APISummaryItem{
		{ID: 1, DailyUsedUSD: float64Ptr(math.Inf(1)), DailyLimitUSD: float64Ptr(10)},
		{ID: 2, DailyUsedUSD: float64Ptr(1), DailyLimitUSD: float64Ptr(math.NaN())},
		{ID: 3, DailyUsedUSD: float64Ptr(1), DailyLimitUSD: float64Ptr(-1)},
	}
	got := sub2APISummarySubscriptions(items)
	if len(got) != 9 {
		t.Fatalf("subscription windows = %d, want 9", len(got))
	}
	for _, window := range got {
		if window.AvailablePercent != nil {
			t.Fatalf("invalid pair produced percentage: %#v", window)
		}
	}
}

func TestSub2APIBalanceRejectsUnsafeEnvelopeWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()
	const token = "private-token"
	const secret = "raw-upstream-secret"
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "missing code", status: http.StatusOK, body: `{"message":"","data":{"balance":1}}`},
		{name: "nonzero code", status: http.StatusOK, body: `{"code":9,"message":"` + secret + ` ` + token + `","data":{"balance":1}}`},
		{name: "missing data", status: http.StatusOK, body: `{"code":0,"message":""}`},
		{name: "null data", status: http.StatusOK, body: `{"code":0,"message":"","data":null}`},
		{name: "negative balance", status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":-1}}`},
		{name: "overflow balance", status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":1e309}}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"code":401,"message":"` + secret + `"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script := &sub2APIScript{t: t, steps: []sub2APIStep{{
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me", status: tt.status, body: tt.body,
			}}}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			snapshot, status, err := service.refreshSub2APIBalance(
				context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}, sub2APITestEnvelope(model.ChannelManagementProfileSub2API),
			)
			if err == nil || snapshot != nil || status != tt.status {
				t.Fatalf("unsafe balance = (%#v, %d, %v)", snapshot, status, err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), tt.body) {
				t.Fatalf("balance error leaked secret: %v", err)
			}
			script.finishedRequests()
		})
	}
}

func TestChannelManagementServiceSub2APIRefreshBalancePersistsSnapshot(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "sub2-balance")
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, sub2APITestEnvelope(model.ChannelManagementProfileSub2API))
	script := &sub2APIScript{t: t, steps: []sub2APIStep{
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":7.5}}`,
		},
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/summary",
			status: http.StatusOK,
			body: `{"code":0,"message":"","data":{"active_count":1,"total_used_usd":2,"subscriptions":[` +
				`{"id":4,"group_name":"Basic","daily_used_usd":2,"daily_limit_usd":8}` +
				`]}}`,
		},
	}}
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	view, err := service.RefreshBalance(context.Background(), cfg.ID)
	if err != nil || view == nil || view.Balance == nil || view.Balance.Remaining != 7.5 ||
		len(view.Balance.Subscriptions) != 3 || view.Balance.Subscriptions[0].AvailablePercent == nil ||
		*view.Balance.Subscriptions[0].AvailablePercent != 75 {
		t.Fatalf("service RefreshBalance = (%#v, %v)", view, err)
	}
	requests := script.finishedRequests()
	if len(requests) != 2 {
		t.Fatalf("refresh request count = %d, want 2", len(requests))
	}
	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse stored envelope: %v", err)
	}
	if envelope.Profile != model.ChannelManagementProfileSub2API ||
		envelope.State.LastBalance == nil || envelope.State.LastBalance.BalanceUSD == nil ||
		*envelope.State.LastBalance.BalanceUSD != 7.5 || envelope.State.LastBalance.SampledAt != fixedNow {
		t.Fatalf("stored balance = %#v", envelope)
	}
}

func TestChannelManagementServiceSub2APIBalanceDegradationPersistsAuthBalance(t *testing.T) {
	t.Parallel()
	summaryTarget := "https://sub2.example.com/api/v1/subscriptions/summary"
	activeTarget := "https://sub2.example.com/api/v1/subscriptions/active"
	tests := []struct {
		name string
		tail []sub2APIStep
	}{
		{
			name: "summary and active unavailable",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusNotFound,
					body: `{"code":404,"message":"raw-upstream-secret","data":null}`,
				},
				{
					method: http.MethodGet, target: activeTarget, status: http.StatusBadGateway,
					body: `{"code":502,"message":"raw-upstream-secret","data":null}`,
				},
			},
		},
		{
			name: "summary malformed",
			tail: []sub2APIStep{{
				method: http.MethodGet, target: summaryTarget, status: http.StatusOK, body: `raw-upstream-secret`,
			}},
		},
		{
			name: "active malformed",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusMethodNotAllowed,
					body: `{"code":405,"message":"raw-upstream-secret","data":null}`,
				},
				{method: http.MethodGet, target: activeTarget, status: http.StatusOK, body: `raw-upstream-secret`},
			},
		},
		{
			name: "active transport error",
			tail: []sub2APIStep{
				{
					method: http.MethodGet, target: summaryTarget, status: http.StatusOK,
					body: `{"code":17,"message":"raw-upstream-secret","data":null}`,
				},
				{method: http.MethodGet, target: activeTarget, err: errors.New("raw-upstream-secret")},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newInMemoryServer(t)
			cfg := createChannelManagementTestConfig(t, server.store, "sub2-degraded-"+strings.ReplaceAll(tt.name, " ", "-"))
			cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, sub2APITestEnvelope(model.ChannelManagementProfileSub2API))
			steps := append([]sub2APIStep{{
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
				status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":8.75}}`,
			}}, tt.tail...)
			script := &sub2APIScript{t: t, steps: steps}
			service := newChannelManagementService(server.store, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			fixedNow := time.Date(2026, time.August, 25, 10, 0, 0, 0, time.UTC)
			service.now = func() time.Time { return fixedNow }

			view, err := service.RefreshBalance(context.Background(), cfg.ID)
			if err != nil || view == nil || view.Balance == nil || view.Balance.Remaining != 8.75 ||
				len(view.Balance.Subscriptions) != 0 || view.Balance.SampledAt != fixedNow {
				t.Fatalf("degraded RefreshBalance = (%#v, %v)", view, err)
			}
			script.finishedRequests()
			stored, err := server.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
			if err != nil {
				t.Fatalf("parse stored envelope: %v", err)
			}
			if envelope.State.LastBalance == nil || envelope.State.LastBalance.BalanceUSD == nil ||
				*envelope.State.LastBalance.BalanceUSD != 8.75 ||
				len(envelope.State.LastBalance.Subscriptions) != 0 || envelope.State.LastBalance.SampledAt != fixedNow {
				t.Fatalf("degraded balance was not persisted: %#v", envelope.State.LastBalance)
			}
		})
	}
}

func TestSub2APINoCheckinStandardIsUnsupportedWithoutUpstreamRequest(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "sub2-no-checkin")
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, sub2APITestEnvelope(model.ChannelManagementProfileSub2API))
	script := &sub2APIScript{t: t}
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	result, err := service.CheckIn(context.Background(), cfg.ID)
	if err != nil || result == nil || result.Status != newAPICheckinUnsupported || result.StatusCode != 0 {
		t.Fatalf("standard Sub2API CheckIn = (%#v, %v)", result, err)
	}
	if result.CheckedInAt == nil || *result.CheckedInAt != fixedNow {
		t.Fatalf("unsupported checkin time = %v", result.CheckedInAt)
	}
	requests := script.finishedRequests()
	if len(requests) != 0 {
		t.Fatalf("standard Sub2API made upstream requests: %#v", requests)
	}
	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse stored envelope: %v", err)
	}
	if envelope.State.LastCheckinStatus != newAPICheckinUnsupported ||
		envelope.State.LastCheckinAt == nil || *envelope.State.LastCheckinAt != fixedNow ||
		envelope.State.LastBalance != nil {
		t.Fatalf("standard unsupported state = %#v", envelope.State)
	}
}

func TestSub2APIProCheckinStateMachine(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	successTime := time.Date(2026, time.August, 25, 9, 31, 2, 0, time.UTC)
	successFractionTime := time.Date(2026, time.August, 25, 9, 31, 2, 123000000, time.UTC)
	statusUnchecked := sub2APIStep{
		method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
		status: http.StatusOK, body: `{"code":0,"message":"","data":{"enabled":true,"checked_in_today":false}}`,
	}
	statusChecked := sub2APIStep{
		method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
		status: http.StatusOK, body: `{"code":0,"message":"","data":{"enabled":true,"checked_in_today":true}}`,
	}
	balance := []sub2APIStep{
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":11}}`,
		},
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/summary",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"active_count":0,"total_used_usd":0,"subscriptions":[]}}`,
		},
	}
	tests := []struct {
		name           string
		steps          []sub2APIStep
		wantStatus     string
		wantStatusCode int
		wantReward     *float64
		wantCheckedAt  time.Time
		wantBalance    bool
		wantError      bool
		wantPosts      int
		wantReadbacks  int
	}{
		{
			name: "disabled by status",
			steps: []sub2APIStep{{
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
				status: http.StatusOK, body: `{"code":0,"message":"","data":{"enabled":false,"checked_in_today":false}}`,
			}},
			wantStatus: newAPICheckinSkippedDisabled, wantStatusCode: http.StatusOK, wantCheckedAt: fixedNow,
		},
		{
			name:       "already checked by status",
			steps:      append([]sub2APIStep{statusChecked}, balance...),
			wantStatus: newAPICheckinAlreadyChecked, wantStatusCode: http.StatusOK, wantCheckedAt: fixedNow, wantBalance: true,
		},
		{
			name: "success validates fork payload",
			steps: append([]sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"code":0,"message":"","data":{"reward_amount":2.5,"new_balance":11,"checked_in_at":"2026-08-25T09:31:02Z"}}`,
			}}, balance...),
			wantStatus: newAPICheckinSuccess, wantStatusCode: http.StatusOK, wantReward: float64Ptr(2.5),
			wantCheckedAt: successTime, wantBalance: true, wantPosts: 1,
		},
		{
			name: "success accepts RFC3339 fractional seconds",
			steps: append([]sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"code":0,"message":"","data":{"reward_amount":2.5,"new_balance":11,"checked_in_at":"2026-08-25T09:31:02.123Z"}}`,
			}}, balance...),
			wantStatus: newAPICheckinSuccess, wantStatusCode: http.StatusOK, wantReward: float64Ptr(2.5),
			wantCheckedAt: successFractionTime, wantBalance: true, wantPosts: 1,
		},
		{
			name: "structured already done",
			steps: append([]sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusConflict, wrote: true,
				body: `{"code":409,"message":"translated secret","reason":"DAILY_CHECKIN_ALREADY_DONE","data":null}`,
			}}, balance...),
			wantStatus: newAPICheckinAlreadyChecked, wantStatusCode: http.StatusConflict,
			wantCheckedAt: fixedNow, wantBalance: true, wantPosts: 1,
		},
		{
			name: "structured disabled",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusForbidden, wrote: true,
				body: `{"code":403,"message":"translated secret","reason":"DAILY_CHECKIN_DISABLED","data":null}`,
			}},
			wantStatus: newAPICheckinSkippedDisabled, wantStatusCode: http.StatusForbidden,
			wantCheckedAt: fixedNow, wantPosts: 1,
		},
		{
			name: "role forbidden differs from credential failure",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusForbidden, wrote: true,
				body: `{"code":403,"message":"translated secret","reason":"DAILY_CHECKIN_ROLE_FORBIDDEN","data":null}`,
			}},
			wantStatus: "credential_forbidden", wantStatusCode: http.StatusForbidden,
			wantCheckedAt: fixedNow, wantPosts: 1,
		},
		{
			name: "forbidden message never guesses role reason",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusForbidden, wrote: true,
				body: `{"code":403,"message":"DAILY_CHECKIN_ROLE_FORBIDDEN private-token","data":null}`,
			}},
			wantStatus: newAPICheckinCredentialError, wantStatusCode: http.StatusForbidden,
			wantCheckedAt: fixedNow, wantPosts: 1,
		},
		{
			name: "unauthorized is credential failure",
			steps: []sub2APIStep{{
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
				status: http.StatusUnauthorized,
				body:   `{"code":401,"message":"private-token","reason":"DAILY_CHECKIN_ROLE_FORBIDDEN","data":null}`,
			}},
			wantStatus: newAPICheckinCredentialError, wantStatusCode: http.StatusUnauthorized, wantCheckedAt: fixedNow,
		},
		{
			name: "written bad gateway with unknown reason checked readback wins",
			steps: append([]sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusBadGateway, wrote: true,
				body: `{"code":502,"message":"raw-upstream-secret","reason":"UPSTREAM_UNAVAILABLE","data":null}`,
			}, statusChecked}, balance...),
			wantStatus: newAPICheckinAlreadyChecked, wantStatusCode: http.StatusBadGateway,
			wantCheckedAt: fixedNow, wantBalance: true, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "written rate limit with unknown reason unchecked is uncertain",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				status: http.StatusTooManyRequests, wrote: true,
				body: `{"code":429,"message":"raw-upstream-secret","reason":"RATE_LIMITED","data":null}`,
			}, statusUnchecked},
			wantStatus: newAPICheckinUncertain, wantStatusCode: http.StatusTooManyRequests,
			wantCheckedAt: fixedNow, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "written transport failure checked readback wins",
			steps: append([]sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				wrote: true, err: errors.New("raw-upstream-secret private-token"),
			}, statusChecked}, balance...),
			wantStatus: newAPICheckinAlreadyChecked, wantCheckedAt: fixedNow, wantBalance: true,
			wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "written transport failure unchecked is uncertain",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				wrote: true, err: errors.New("raw-upstream-secret private-token"),
			}, statusUnchecked},
			wantStatus: newAPICheckinUncertain, wantCheckedAt: fixedNow, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "written transport failure unreadable is uncertain",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				wrote: true, err: errors.New("raw-upstream-secret private-token"),
			}, {
				method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
				err: errors.New("raw-upstream-secret"),
			}},
			wantStatus: newAPICheckinUncertain, wantCheckedAt: fixedNow, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "unwritten transport failure is safe error",
			steps: []sub2APIStep{statusUnchecked, {
				method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
				err: errors.New("raw-upstream-secret private-token"),
			}},
			wantError: true, wantPosts: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script := &sub2APIScript{t: t, steps: tt.steps}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			service.now = func() time.Time { return fixedNow }
			result, snapshot, err := service.checkInSub2APIPro(
				context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
				sub2APITestEnvelope(model.ChannelManagementProfileSub2APIPro),
			)
			requests := script.finishedRequests()
			if tt.wantError {
				if err == nil || result != nil || snapshot != nil {
					t.Fatalf("ordinary POST failure = (%#v, %#v, %v)", result, snapshot, err)
				}
				if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "raw-upstream-secret") {
					t.Fatalf("checkin error leaked secret: %v", err)
				}
			} else {
				if err != nil || result == nil || result.Status != tt.wantStatus || result.StatusCode != tt.wantStatusCode {
					t.Fatalf("checkin result = (%#v, %#v, %v)", result, snapshot, err)
				}
				if result.CheckedInAt == nil || *result.CheckedInAt != tt.wantCheckedAt {
					t.Fatalf("checked_at = %v, want %v", result.CheckedInAt, tt.wantCheckedAt)
				}
				if !equalOptionalFloat64(result.Reward, tt.wantReward) {
					t.Fatalf("reward = %v, want %v", result.Reward, tt.wantReward)
				}
				if tt.wantBalance != (snapshot != nil) || tt.wantBalance != (result.Balance != nil) {
					t.Fatalf("balance result = (%#v, %#v), want present %t", result.Balance, snapshot, tt.wantBalance)
				}
			}
			posts := 0
			readbacks := 0
			sawPost := false
			for _, request := range requests {
				if request.authorization != "Bearer private-token" || request.userID != "" {
					t.Fatalf("Sub2API Pro headers = %#v", request)
				}
				if request.method == http.MethodPost {
					posts++
					sawPost = true
					if request.target != "https://sub2.example.com/api/v1/redeem/checkin" ||
						request.body != `{}` || request.contentType != "application/json" {
						t.Fatalf("Sub2API Pro POST contract = %#v", request)
					}
				} else if sawPost && request.target == "https://sub2.example.com/api/v1/redeem/checkin/status" {
					readbacks++
				}
			}
			if posts != tt.wantPosts || readbacks != tt.wantReadbacks || posts > 1 || readbacks > 1 {
				t.Fatalf("request counts = (POST %d, readback %d), want (%d, %d): %#v", posts, readbacks, tt.wantPosts, tt.wantReadbacks, requests)
			}
		})
	}
}

func TestSub2APIProCheckinRejectsIncompleteSuccessData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "missing reward", body: `{"code":0,"message":"","data":{"new_balance":11,"checked_in_at":"2026-08-25T09:31:02Z"}}`},
		{name: "missing new balance", body: `{"code":0,"message":"","data":{"reward_amount":1,"checked_in_at":"2026-08-25T09:31:02Z"}}`},
		{name: "missing checked at", body: `{"code":0,"message":"","data":{"reward_amount":1,"new_balance":11}}`},
		{name: "negative reward", body: `{"code":0,"message":"","data":{"reward_amount":-1,"new_balance":11,"checked_in_at":"2026-08-25T09:31:02Z"}}`},
		{name: "negative new balance", body: `{"code":0,"message":"","data":{"reward_amount":1,"new_balance":-1,"checked_in_at":"2026-08-25T09:31:02Z"}}`},
		{name: "invalid checked at", body: `{"code":0,"message":"","data":{"reward_amount":1,"new_balance":11,"checked_in_at":"2026-08-25"}}`},
		{name: "overflow reward", body: `{"code":0,"message":"","data":{"reward_amount":1e309,"new_balance":11,"checked_in_at":"2026-08-25T09:31:02Z"}}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script := &sub2APIScript{t: t, steps: []sub2APIStep{
				{
					method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
					status: http.StatusOK, body: `{"code":0,"message":"","data":{"enabled":true,"checked_in_today":false}}`,
				},
				{
					method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
					status: http.StatusOK, wrote: true, body: tt.body,
				},
			}}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			result, snapshot, err := service.checkInSub2APIPro(
				context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
				sub2APITestEnvelope(model.ChannelManagementProfileSub2APIPro),
			)
			if !errors.Is(err, errInvalidManagementResponse) || result != nil || snapshot != nil {
				t.Fatalf("invalid success = (%#v, %#v, %v)", result, snapshot, err)
			}
			script.finishedRequests()
		})
	}
}

func TestChannelManagementServiceSub2APIProCheckinCASConflictMergesWithoutReplaying(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "sub2-pro-checkin-cas")
	seed := sub2APITestEnvelope(model.ChannelManagementProfileSub2APIPro)
	seed.Settings.DailyCheckinEnabled = true
	seed.Settings.DailyCheckinTime = "09:30"
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, seed)

	steps := []sub2APIStep{
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/redeem/checkin/status",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"enabled":true,"checked_in_today":false}}`,
		},
		{
			method: http.MethodPost, target: "https://sub2.example.com/api/v1/redeem/checkin",
			status: http.StatusOK, wrote: true,
			body: `{"code":0,"message":"","data":{"reward_amount":2.5,"new_balance":11,"checked_in_at":"2026-08-25T09:31:02Z"}}`,
		},
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/auth/me",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"balance":11}}`,
		},
		{
			method: http.MethodGet, target: "https://sub2.example.com/api/v1/subscriptions/summary",
			status: http.StatusOK, body: `{"code":0,"message":"","data":{"active_count":0,"total_used_usd":0,"subscriptions":[]}}`,
		},
	}
	script := &sub2APIScript{t: t, steps: steps}
	conflicts := &newAPIManagementConflictStore{Store: server.store}
	service := newChannelManagementService(conflicts, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})

	result, err := service.CheckIn(context.Background(), cfg.ID)
	if err != nil || result == nil || result.Status != newAPICheckinSuccess ||
		result.CheckedInAt == nil || result.CheckedInAt.Format(time.RFC3339) != "2026-08-25T09:31:02Z" ||
		result.Balance == nil || result.Balance.Remaining != 11 {
		t.Fatalf("service CheckIn = (%#v, %v)", result, err)
	}
	requests := script.finishedRequests()
	posts := 0
	for _, request := range requests {
		if request.method == http.MethodPost {
			posts++
		}
	}
	if len(requests) != 4 || posts != 1 {
		t.Fatalf("CAS replayed upstream requests: %#v", requests)
	}

	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse stored envelope: %v", err)
	}
	if envelope.State.LastScheduledDay != "concurrent-day" ||
		envelope.State.LastCheckinStatus != newAPICheckinSuccess ||
		envelope.State.LastCheckinAt == nil || envelope.State.LastCheckinAt.Format(time.RFC3339) != "2026-08-25T09:31:02Z" ||
		envelope.State.LastBalance == nil || envelope.State.LastBalance.BalanceUSD == nil || *envelope.State.LastBalance.BalanceUSD != 11 {
		t.Fatalf("CAS merge lost state or split checkin and balance: %#v", envelope.State)
	}
	conflicts.mu.Lock()
	defer conflicts.mu.Unlock()
	if len(conflicts.candidates) != 2 {
		t.Fatalf("CAS candidates = %d, want 2", len(conflicts.candidates))
	}
	for index, candidate := range conflicts.candidates {
		if candidate.State.LastCheckinStatus != newAPICheckinSuccess || candidate.State.LastCheckinAt == nil ||
			candidate.State.LastBalance == nil || candidate.State.LastBalance.BalanceUSD == nil {
			t.Fatalf("CAS candidate %d was not one complete state update: %#v", index, candidate.State)
		}
	}
}

func TestSub2APIProfileIsolationRejectsCrossProfileCallsWithoutRequests(t *testing.T) {
	t.Parallel()
	script := &sub2APIScript{t: t}
	service := newChannelManagementService(nil, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	cfg := &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}
	if snapshot, status, err := service.refreshSub2APIBalance(
		context.Background(), cfg, newAPITestEnvelope(nil),
	); !errors.Is(err, errInvalidManagementRequest) || snapshot != nil || status != 0 {
		t.Fatalf("New API through Sub2API balance parser = (%#v, %d, %v)", snapshot, status, err)
	}
	if result, snapshot, err := service.checkInSub2APIPro(
		context.Background(), cfg, sub2APITestEnvelope(model.ChannelManagementProfileSub2API),
	); !errors.Is(err, errInvalidManagementRequest) || result != nil || snapshot != nil {
		t.Fatalf("standard Sub2API through Pro checkin = (%#v, %#v, %v)", result, snapshot, err)
	}
	if requests := script.finishedRequests(); len(requests) != 0 {
		t.Fatalf("cross-profile calls made requests: %#v", requests)
	}
}

type sub2APIStep struct {
	method string
	target string
	status int
	body   string
	err    error
	wrote  bool
}

type sub2APIRecordedRequest struct {
	method        string
	target        string
	body          string
	authorization string
	userID        string
	contentType   string
}

type sub2APIScript struct {
	t        *testing.T
	mu       sync.Mutex
	steps    []sub2APIStep
	requests []sub2APIRecordedRequest
}

func (s *sub2APIScript) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := len(s.requests)
	if index >= len(s.steps) {
		s.t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
	}
	step := s.steps[index]
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
	}
	s.requests = append(s.requests, sub2APIRecordedRequest{
		method: req.Method, target: req.URL.String(), body: string(body),
		authorization: req.Header.Get("Authorization"), userID: req.Header.Get("New-API-User"),
		contentType: req.Header.Get("Content-Type"),
	})
	if req.Method != step.method || req.URL.String() != step.target {
		s.t.Fatalf("request %d = %s %s, want %s %s", index, req.Method, req.URL.String(), step.method, step.target)
	}
	if step.wrote {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil || trace.WroteRequest == nil {
			s.t.Fatal("written POST missing WroteRequest trace")
		}
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	if step.err != nil {
		return nil, step.err
	}
	return sub2APIHTTPResponse(req, step.status, step.body), nil
}

func (s *sub2APIScript) finishedRequests() []sub2APIRecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != len(s.steps) {
		s.t.Fatalf("request count = %d, want %d", len(s.requests), len(s.steps))
	}
	return append([]sub2APIRecordedRequest(nil), s.requests...)
}

func sub2APITestEnvelope(profile string) *model.ChannelManagementEnvelope {
	return &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion, Profile: profile,
		Settings: model.ChannelManagementSettings{BaseURL: "https://sub2.example.com", AccessToken: "private-token"},
	}
}

func sub2APIHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: req,
	}
}

func assertSub2APISubscriptions(t *testing.T, got, want []model.ChannelManagementSubscriptionSnapshot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("subscriptions length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || got[i].Window != want[i].Window || got[i].ExpiresAt != want[i].ExpiresAt ||
			!equalOptionalFloat64(got[i].UsedUSD, want[i].UsedUSD) || !equalOptionalFloat64(got[i].LimitUSD, want[i].LimitUSD) ||
			!equalOptionalFloat64(got[i].AvailablePercent, want[i].AvailablePercent) {
			t.Fatalf("subscription %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func equalOptionalFloat64(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func float64Ptr(value float64) *float64 {
	return &value
}
