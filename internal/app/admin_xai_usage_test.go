package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

type xaiUsageRoundTripper func(*http.Request) (*http.Response, error)

func (f xaiUsageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func xaiUsageResponse(req *http.Request, status int, body string, headers ...http.Header) *http.Response {
	header := http.Header{"Content-Type": []string{"application/json"}}
	if len(headers) != 0 {
		header = headers[0]
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func newXAIUsageChannel(t *testing.T, server *Server, accessToken, refreshToken, baseURL string) *model.Config {
	t.Helper()
	credential := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: accessToken, RefreshToken: refreshToken,
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "usage@example.com",
	}
	raw, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: "xAI usage", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw,
		URLs: model.ChannelURLs{{URL: baseURL, Protocols: []string{"codex"}}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func callXAIUsage(t *testing.T, server *Server, channelID int64) *testHTTPResponse {
	t.Helper()
	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channelID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channelID)}}
	server.HandleOAuthUsage(c)
	return &testHTTPResponse{code: w.Code, body: w.Body.String(), raw: w.Body.Bytes()}
}

type testHTTPResponse struct {
	code int
	body string
	raw  []byte
}

func TestXAIUsage_DualBillingSuccess(t *testing.T) {
	server := newInMemoryServer(t)
	const baseURL = "https://gateway.example/xai/v1"
	cfg := newXAIUsageChannel(t, server, "access-secret", "refresh-secret", baseURL)

	var requests atomic.Int32
	server.client = &http.Client{Transport: xaiUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Method != http.MethodGet || req.URL.Host != "gateway.example" || !strings.HasPrefix(req.URL.Path, "/xai/v1/billing") {
			t.Fatalf("request=%s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer access-secret" ||
			req.Header.Get(xaiauth.CLITokenAuthHeader) != xaiauth.CLITokenAuthValue ||
			req.Header.Get(xaiauth.CLIClientVersionHeader) != xaiauth.CLIClientVersion ||
			req.Header.Get("User-Agent") != xaiauth.CLIUserAgent {
			t.Fatalf("billing headers=%v", req.Header)
		}
		if req.URL.Query().Get("format") == "credits" {
			return xaiUsageResponse(req, http.StatusOK, `{
				"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z"}},
				"plan":"grok-build","subscriptionTier":"SuperGrok"
			}`), nil
		}
		return xaiUsageResponse(req, http.StatusOK, `{
			"config":{"monthlyLimit":{"val":10000},"used":{"val":4000},"billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z"},
			"entitlement":{"status":"active"}
		}`), nil
	})}
	server.xaiCredentials = newXAICredentialManager(server.store, func(*model.Config) *http.Client { return server.client }, nil)

	response := callXAIUsage(t, server, cfg.ID)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	for _, secret := range []string{"access-secret", "refresh-secret"} {
		if strings.Contains(response.body, secret) {
			t.Fatalf("usage response leaked %q: %s", secret, response.body)
		}
	}
	summary := mustParseAPIResponse[oauthUsageSummary](t, response.raw).Data
	if summary.Provider != xaiauth.ChannelType || summary.PlanType != "grok-build" || summary.SubscriptionTier != "SuperGrok" ||
		summary.EntitlementStatus != "active" || len(summary.Warnings) != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	if len(summary.Windows) != 2 || summary.Windows[0].Kind != "weekly" || summary.Windows[0].UsedPercent != 25 ||
		summary.Windows[0].RemainingPercent != 75 || summary.Windows[0].LimitWindowSeconds != 7*24*60*60 ||
		summary.Windows[1].Kind != "monthly" || summary.Windows[1].UsedPercent != 40 || summary.Windows[1].RemainingPercent != 60 {
		t.Fatalf("windows=%+v", summary.Windows)
	}
	if requests.Load() != 2 {
		t.Fatalf("billing requests=%d, want 2", requests.Load())
	}
}

func TestXAIUsage_UnifiedBillingOnDemandCapCalculatesWindow(t *testing.T) {
	server := newInMemoryServer(t)
	cfg := newXAIUsageChannel(t, server, "unified-access", "unified-refresh", xaiauth.CLIBaseURL)
	server.client = &http.Client{Transport: xaiUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("format") == "credits" {
			return xaiUsageResponse(req, http.StatusOK, `{"config":{
				"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z"},
				"isUnifiedBillingUser":true,"onDemandCap":{"val":400},"onDemandUsed":{"val":100},"prepaidBalance":{"val":0}
			}}`), nil
		}
		return xaiUsageResponse(req, http.StatusInternalServerError, `{}`), nil
	})}
	server.xaiCredentials = newXAICredentialManager(server.store, func(*model.Config) *http.Client { return server.client }, nil)

	response := callXAIUsage(t, server, cfg.ID)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	summary := mustParseAPIResponse[oauthUsageSummary](t, response.raw).Data
	if len(summary.Windows) != 1 || summary.Windows[0].Kind != "weekly" ||
		summary.Windows[0].UsedPercent != 25 || summary.Windows[0].RemainingPercent != 75 ||
		len(summary.Warnings) != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	for _, secret := range []string{"unified-access", "unified-refresh"} {
		if strings.Contains(response.body, secret) {
			t.Fatalf("usage response leaked %q: %s", secret, response.body)
		}
	}
}

func TestXAIUsage_PartialAndStrictStatusClassification(t *testing.T) {
	tests := []struct {
		name          string
		creditsStatus int
		creditsBody   string
		monthlyStatus int
		monthlyBody   string
		wantStatus    int
		wantWindows   int
		wantWarning   bool
		wantEntitled  string
	}{
		{
			name: "partial on server failure", creditsStatus: 200,
			creditsBody:   `{"config":{"creditUsagePercent":10,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z"}}}`,
			monthlyStatus: 500, monthlyBody: `{"error":"upstream-body-secret"}`,
			wantStatus: 200, wantWindows: 1, wantWarning: true,
		},
		{
			name: "credits reset survives omitted period start", creditsStatus: 200,
			creditsBody:   `{"config":{"creditUsagePercent":0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-08T00:00:00Z"}}}`,
			monthlyStatus: 500, monthlyBody: `{}`,
			wantStatus: 200, wantWindows: 1, wantWarning: true,
		},
		{
			name:          "ordinary forbidden and rate limit stay unknown",
			creditsStatus: 403, creditsBody: `{"error":"ordinary-forbidden-secret"}`,
			monthlyStatus: 429, monthlyBody: `not-json-secret`,
			wantStatus: 502,
		},
		{
			name:          "structured entitlement is authenticated",
			creditsStatus: 403, creditsBody: `{"error":{"type":"permission_error","code":"subscription_required"}}`,
			monthlyStatus: 200,
			monthlyBody:   `{"config":{"monthlyLimit":{"val":100},"used":{"val":100},"billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z"}}`,
			wantStatus:    200, wantWindows: 1, wantEntitled: "entitlement",
		},
		{
			name:          "successful responses with missing quota fields stay unknown",
			creditsStatus: 200, creditsBody: `{"config":{"currentPeriod":{"end":"2026-08-08T00:00:00Z"}}}`,
			monthlyStatus: 200, monthlyBody: `{"config":{"monthlyLimit":{"val":100}}}`,
			wantStatus: 502,
		},
		{
			name:          "unified billing with zero caps returns unknown windows without failing",
			creditsStatus: 200,
			creditsBody: `{"config":{
				"billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z",
				"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z"},
				"isUnifiedBillingUser":true,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},
				"prepaidBalance":{"val":0},"topUpMethod":"unknown"
			}}`,
			monthlyStatus: 200,
			monthlyBody: `{"config":{
				"billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z",
				"monthlyLimit":{"val":0},"used":{"val":25},"onDemandCap":{"val":0},
				"history":[{"billingCycle":{},"includedUsed":{"val":25},"onDemandUsed":{"val":0},"totalUsed":{"val":25}}]
			}}`,
			wantStatus:  http.StatusOK,
			wantWindows: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newInMemoryServer(t)
			cfg := newXAIUsageChannel(t, server, "status-access-secret", "status-refresh-secret", xaiauth.CLIBaseURL)
			var calls atomic.Int32
			server.client = &http.Client{Transport: xaiUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.URL.Query().Get("format") == "credits" {
					return xaiUsageResponse(req, tc.creditsStatus, tc.creditsBody), nil
				}
				return xaiUsageResponse(req, tc.monthlyStatus, tc.monthlyBody), nil
			})}
			server.xaiCredentials = newXAICredentialManager(server.store, func(*model.Config) *http.Client { return server.client }, nil)

			response := callXAIUsage(t, server, cfg.ID)
			if response.code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.code, tc.wantStatus, response.body)
			}
			for _, secret := range []string{"status-access-secret", "status-refresh-secret", "upstream-body-secret", "ordinary-forbidden-secret", "not-json-secret"} {
				if strings.Contains(response.body, secret) {
					t.Fatalf("response leaked %q: %s", secret, response.body)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("requests=%d, want 2 without refresh", calls.Load())
			}
			if tc.wantStatus == http.StatusOK {
				summary := mustParseAPIResponse[oauthUsageSummary](t, response.raw).Data
				if len(summary.Windows) != tc.wantWindows || (len(summary.Warnings) != 0) != tc.wantWarning || summary.EntitlementStatus != tc.wantEntitled {
					t.Fatalf("summary=%+v", summary)
				}
			}
		})
	}
}

func TestXAIUsage_RefreshesOnceOnlyForBadCredential(t *testing.T) {
	server := newInMemoryServer(t)
	cfg := newXAIUsageChannel(t, server, "old-access-secret", "old-refresh-secret", xaiauth.CLIBaseURL)
	var tokenRequests atomic.Int32
	var oldBillingRequests atomic.Int32
	var newBillingRequests atomic.Int32
	server.client = &http.Client{Transport: xaiUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == xaiauth.TokenURL {
			tokenRequests.Add(1)
			return xaiUsageResponse(req, http.StatusOK, `{"access_token":"new-access-secret","refresh_token":"rotated-refresh-secret","expires_in":3600,"token_type":"Bearer"}`), nil
		}
		switch req.Header.Get("Authorization") {
		case "Bearer old-access-secret":
			oldBillingRequests.Add(1)
			return xaiUsageResponse(req, http.StatusUnauthorized, `{"error":"expired-old-access-secret"}`), nil
		case "Bearer new-access-secret":
			newBillingRequests.Add(1)
			if req.URL.Query().Get("format") == "credits" {
				return xaiUsageResponse(req, http.StatusOK, `{"config":{"creditUsagePercent":0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z"}}}`), nil
			}
			return xaiUsageResponse(req, http.StatusOK, `{"config":{"monthlyLimit":{"val":100},"used":{"val":0},"billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z"}}`), nil
		default:
			t.Fatalf("unexpected Authorization=%q", req.Header.Get("Authorization"))
			return nil, nil
		}
	})}
	server.xaiCredentials = newXAICredentialManager(server.store, func(*model.Config) *http.Client { return server.client }, nil)

	response := callXAIUsage(t, server, cfg.ID)
	if response.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.code, response.body)
	}
	if tokenRequests.Load() != 1 || oldBillingRequests.Load() != 1 || newBillingRequests.Load() != 2 {
		t.Fatalf("requests token=%d old=%d new=%d", tokenRequests.Load(), oldBillingRequests.Load(), newBillingRequests.Load())
	}
	persisted, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := xaiauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || credential.AccessToken != "new-access-secret" || credential.RefreshToken != "rotated-refresh-secret" {
		t.Fatalf("persisted credential=%v err=%v", credential, err)
	}
}
