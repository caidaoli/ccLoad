package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

func TestAPITokenLoginDisabledRevokesRestoredSessions(t *testing.T) {
	svc, store := setupAPITokenLoginService(t, "sk-login-switch")
	login := runLoginHandler(t, svc, `{"mode":"api_token","token":"sk-login-switch"}`, "1.2.3.4:1234")
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var data struct {
		Token string `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, login.Body.Bytes(), &data)
	adminLogin := runLoginHandler(t, svc, `{"mode":"admin","password":"admin-pass"}`, "1.2.3.4:1234")
	var adminData struct {
		Token string `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, adminLogin.Body.Bytes(), &adminData)
	svc.Close()

	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	disabled := NewAuthService("admin-pass", limiter, store)
	t.Cleanup(disabled.Close)
	for _, token := range []string{"sk-login-switch", "invalid-token"} {
		w := runLoginHandler(t, disabled, `{"mode":"api_token","token":"`+token+`"}`, "1.2.3.4:1234")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "未开启API Token登陆，请联系管理员") {
			t.Fatalf("disabled login: %d %s", w.Code, w.Body.String())
		}
	}
	if w := runWebAuthMiddleware(t, disabled.RequireWebAuth(), data.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("old Token session status=%d", w.Code)
	}
	if w := runWebAuthMiddleware(t, disabled.RequireAdminAuth(), adminData.Token); w.Code != http.StatusOK {
		t.Fatalf("admin session status=%d", w.Code)
	}
	req := newRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-login-switch")
	if w := runMiddleware(t, disabled.RequireAPIAuth(), req); w.Code != http.StatusOK {
		t.Fatalf("API call status=%d", w.Code)
	}
	disabled.Close()
	enabled := newEnabledTokenAuthService(limiter, store)
	t.Cleanup(enabled.Close)
	if w := runWebAuthMiddleware(t, enabled.RequireWebAuth(), data.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("re-enabling resurrected revoked session: %d", w.Code)
	}
}

func TestTokenChannelVisibilityKeepsStatisticsRows(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	ctx := context.Background()
	for i, name := range []string{"private-channel-one", "private-channel-two"} {
		channel, err := store.CreateConfig(ctx, &model.Config{
			Name: name, URLs: model.ChannelURLs{{URL: "https://private-upstream.example"}},
			Enabled: true, ModelEntries: []model.ModelEntry{{Model: "shared-model"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AddLog(ctx, &model.LogEntry{
			Time: model.JSONTime{Time: time.Now()}, ChannelID: channel.ID, ChannelName: name,
			Model: "shared-model", AuthTokenID: 42, LogSource: model.LogSourceProxy,
			StatusCode: 200, Duration: float64(i + 1), Cost: float64(i + 1), CostMultiplier: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, hidden := range []bool{true, false} {
		for _, endpoint := range []struct {
			path    string
			handler gin.HandlerFunc
		}{
			{"/dashboard/logs", server.HandleErrors},
			{"/dashboard/logs/bootstrap", server.HandleLogsBootstrap},
			{"/dashboard/models", server.HandleGetModels},
			{"/dashboard/metrics", server.HandleMetrics},
			{"/dashboard/stats/filter-options", server.HandleStatsFilterOptions},
			{"/dashboard/stats", server.HandleStats},
			{"/dashboard/channels", server.HandleDashboardChannels},
			{"/dashboard/channels/filter-options", server.HandleDashboardChannelFilterOptions},
		} {
			query := "?range=today"
			if hidden {
				query += "&channel_id=999999&channel_name=nonexistent&channel_name_like=nonexistent"
			}
			c, w := newTestContext(t, newRequest(http.MethodGet, endpoint.path+query, nil))
			c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAPIToken, AuthTokenID: 42, HideChannels: hidden})
			endpoint.handler(c)
			if hidden && strings.HasPrefix(endpoint.path, "/dashboard/channels") {
				if w.Code != http.StatusForbidden {
					t.Fatalf("%s status=%d", endpoint.path, w.Code)
				}
				continue
			}
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", endpoint.path, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private-channel") == hidden {
				t.Fatalf("%s hidden=%v returned wrong channel visibility: %s", endpoint.path, hidden, w.Body.String())
			}
			if endpoint.path == "/dashboard/stats" {
				var data struct {
					Stats []model.StatsEntry `json:"stats"`
				}
				mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
				if len(data.Stats) != 2 {
					t.Fatalf("statistics rows were merged or filtered: %+v", data.Stats)
				}
				cost := 0.0
				for _, row := range data.Stats {
					if row.Total != 1 || row.TotalCost == nil {
						t.Fatalf("changed row: %+v", row)
					}
					if hidden && row.ChannelID != nil {
						t.Fatal("hidden channel ID exposed")
					}
					cost += *row.TotalCost
				}
				if cost != 3 {
					t.Fatalf("cost=%v", cost)
				}
			}
		}
	}
	// The same endpoints remain unrestricted for administrators.
	c, w := newTestContext(t, newRequest(http.MethodGet, "/dashboard/stats?range=today", nil))
	c.Set(webIdentityContextKey, WebIdentity{Role: model.WebRoleAdmin, HideChannels: true})
	server.HandleStats(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "private-channel-one") {
		t.Fatalf("administrator statistics changed: %d %s", w.Code, w.Body.String())
	}
	for _, identity := range []WebIdentity{
		{Role: model.WebRoleAPIToken, AuthTokenID: 42, HideChannels: true},
		{Role: model.WebRoleAdmin, HideChannels: true},
		{}, // The unauthenticated public summary is unaffected.
	} {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/summary?range=today", nil))
		if identity.Role != "" {
			c.Set(webIdentityContextKey, identity)
		}
		server.HandlePublicSummary(c)
		wantTypes := identity.Role != model.WebRoleAPIToken
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "by_auth_type") != wantTypes {
			t.Fatalf("summary role=%s: %d %s", identity.Role, w.Code, w.Body.String())
		}
	}
}

func TestAPITokenChannelSettingReachesWebSession(t *testing.T) {
	previous, store := setupAPITokenLoginService(t, "sk-visibility-setting")
	previous.Close()
	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	for _, show := range []bool{false, true} {
		cfg := NewConfigService(store)
		cfg.cache[config.APITokenLoginEnabledSettingKey] = &model.SystemSetting{Value: "true"}
		if show {
			cfg.cache[config.APITokenShowChannelsSettingKey] = &model.SystemSetting{Value: "true"}
		}
		svc := NewAuthService("admin-pass", limiter, store, cfg)
		t.Cleanup(svc.Close)
		login := runLoginHandler(t, svc, `{"mode":"api_token","token":"sk-visibility-setting"}`, "1.2.3.4:1234")
		var loginData struct {
			Token string `json:"token"`
		}
		mustUnmarshalAPIResponseData(t, login.Body.Bytes(), &loginData)
		router := gin.New()
		router.GET("/session", svc.RequireWebAuth(), svc.HandleWebSession)
		c, w := newTestContext(t, newRequest(http.MethodGet, "/session", nil))
		c.Request.Header.Set("Authorization", "Bearer "+loginData.Token)
		router.ServeHTTP(w, c.Request)
		var session struct {
			ShowChannels bool `json:"show_channels"`
		}
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &session)
		if w.Code != http.StatusOK || session.ShowChannels != show {
			t.Fatalf("show=%v session: %d %s", show, w.Code, w.Body.String())
		}
		svc.Close()
	}
}
