package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
)

func TestHandleImportCursorCredentialRejectsSessionToken(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	c, w := newTestContext(t, newRequest(
		http.MethodPost,
		"/admin/cursor/credentials/import",
		bytes.NewBufferString(`{"access_token":"unsupported-session"}`),
	))
	c.Request.Header.Set("Content-Type", "application/json")

	server.HandleImportCursorCredential(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "api_key is required") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestHandleImportCursorCredentialRejectsCLIAuthJSON(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	for _, body := range []string{
		`{"api_key":"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.e30.sig"}`,
		`{"api_key":"{\"accessToken\":\"eyJhbGciOiJSUzI1NiJ9.e30.sig\",\"refreshToken\":\"rt\"}"}`,
	} {
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/cursor/credentials/import", bytes.NewBufferString(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		server.HandleImportCursorCredential(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "User API Key") && !strings.Contains(w.Body.String(), "auth.json") {
			t.Fatalf("body=%s", w.Body.String())
		}
	}
}

/**
 * @brief 验证导入保存发现的窗口和档位，按 API Key 变更清除过期目录。
 * @param t 测试上下文。
 */
func TestHandleImportCursorCredentialBindsModelCatalogToAPIKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.cursorService = cursorauth.NewService(&http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		var payload string
		switch request.URL.Path {
		case cursorauth.ExchangeAPIKeyPath:
			payload = `{"accessToken":"cursor-session","refreshToken":"cursor-refresh"}`
		case cursorauth.GetMeRPC:
			payload = `{"authId":"account-1","email":"cursor@example.invalid"}`
		default:
			return nil, fmt.Errorf("unexpected Cursor RPC %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})})
	windows := map[string]int{"composer-2.5@300k": 300000}
	levels := map[string][]string{"composer-2.5@300k": {}}
	runner := &fakeCursorRunner{catalog: cursorauth.ModelCatalog{
		Models: []string{"composer-2.5@300k"}, ContextWindows: windows, EffortLevels: levels,
	}}
	server.cursorRunner = runner
	var channelID int64
	for _, tc := range []struct {
		name, apiKey string
		created      bool
		catalogErr   error
		wantWindows  map[string]int
		wantLevels   map[string][]string
	}{
		{name: "new account", apiKey: "original-key", created: true, wantWindows: windows, wantLevels: levels},
		{name: "restore same key without catalog", apiKey: "original-key", catalogErr: errors.New("bridge unavailable"), wantWindows: windows, wantLevels: levels},
		{name: "rotate key without catalog", apiKey: "rotated-key", catalogErr: errors.New("bridge unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner.err = tc.catalogErr
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/cursor/credentials/import", map[string]string{"api_key": tc.apiKey}))
			server.HandleImportCursorCredential(c)
			response := mustParseAPIResponse[cursorCredentialImportResponse](t, w.Body.Bytes())
			if w.Code != http.StatusOK || !response.Success || response.Data.Created != tc.created {
				t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.created {
				channelID = response.Data.ChannelID
			} else if response.Data.ChannelID != channelID {
				t.Fatalf("reauthorization replaced channel %d with %d", channelID, response.Data.ChannelID)
			}
			cfg, err := store.GetConfig(t.Context(), channelID)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if credential.APIKey != tc.apiKey || !maps.Equal(credential.ContextWindows, tc.wantWindows) {
				t.Fatalf("imported context windows = %v, want %v for the imports' key", credential.ContextWindows, tc.wantWindows)
			}
			if !maps.EqualFunc(credential.EffortLevels, tc.wantLevels, slices.Equal) {
				t.Fatalf("imported effort levels = %v, want %v for the imported key", credential.EffortLevels, tc.wantLevels)
			}
		})
	}
}

func TestNewCursorOAuthChannelUsesCLIOrigin(t *testing.T) {
	t.Parallel()
	channel := newCursorOAuthChannel("Cursor-user@example.com", `{"type":"cursor","access_token":"tok"}`, nil)
	if channel.AuthType != model.AuthTypeCursorOAuth || !channel.Enabled || channel.CostMultiplier != 1 {
		t.Fatalf("channel = %+v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != cursorauth.APIBaseURL {
		t.Fatalf("urls = %+v", channel.URLs)
	}
	if len(channel.URLs[0].Protocols) != 2 {
		t.Fatalf("protocols = %+v", channel.URLs[0].Protocols)
	}
	if channel.ProtocolTransformMode != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol transform mode = %q", channel.ProtocolTransformMode)
	}
	if len(channel.ModelEntries) != len(cursorauth.DefaultModels) {
		t.Fatalf("models = %+v", channel.ModelEntries)
	}
	if !channel.SupportsModel("grok-4.7") || !channel.SupportsModel("grok-4.7-fast") {
		t.Fatalf("fallback models = %+v, want grok-4.7 and grok-4.7-fast", channel.ModelEntries)
	}
}

/**
 * @brief 验证管理端实时发现保留原始模型 ID，并给历史渠道补充账号窗口和档位。
 * @param t 测试上下文。
 */
func TestFetchCursorOAuthModelsUsesSDKCatalog(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)
	windows := map[string]int{"composer-2.5": 200000, "composer-2.5-fast": 200000}
	levels := map[string][]string{"grok-4.6": {"low", "high", "xhigh"}, "composer-2.5": {}}
	runner := &fakeCursorRunner{catalog: cursorauth.ModelCatalog{
		Models: []string{"default", "grok-4.6", "composer-2.5", "composer-2.5-fast"}, ContextWindows: windows, EffortLevels: levels,
	}}
	server.cursorRunner = runner
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "access-token", APIKey: "user-api-key",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(t.Context(), newCursorOAuthChannel("Cursor-catalog", raw, []string{"stale-thinking-high"}))
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID)
	c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
	server.HandleFetchModels(c)
	result := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
	if w.Code != http.StatusOK || !result.Success || strings.Contains(w.Body.String(), "user-api-key") {
		t.Fatalf("discovery status=%d body=%s", w.Code, w.Body.String())
	}
	response := result.Data
	want := []string{"composer-2.5", "composer-2.5-fast", "default", "grok-4.6"}
	if len(response.Models) != len(want) {
		t.Fatalf("models = %+v, want %v", response.Models, want)
	}
	for index, entry := range response.Models {
		if entry.Model != want[index] || entry.RedirectModel != "" {
			t.Fatalf("models[%d] = %+v, want exact ID %q", index, entry, want[index])
		}
	}
	if response.Source != "api" || response.Debug == nil || response.Debug.Fetcher != "cursor_sdk_catalog" {
		t.Fatalf("response metadata = %+v", response)
	}
	if runner.apiKey != "user-api-key" {
		t.Fatalf("SDK catalog API key = %q", runner.apiKey)
	}
	stored, err := store.GetConfig(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cursorauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(credential.ContextWindows, windows) || !maps.EqualFunc(credential.EffortLevels, levels, slices.Equal) {
		t.Fatalf("stored windows = %v, levels = %v, want %v and %v", credential.ContextWindows, credential.EffortLevels, windows, levels)
	}
	if !reflect.DeepEqual(stored.ModelEntries, cfg.ModelEntries) {
		t.Errorf("discovery changed saved models to %+v", stored.ModelEntries)
	}
}

/**
 * @brief 验证 Cursor 批量覆盖仅采用实时模型目录，读取目录仍可展示预置模型。
 * @param t 测试上下文。
 */
func TestBatchReplaceCursorModelsRequiresLiveCatalog(t *testing.T) {
	for _, tc := range []struct {
		name              string
		catalog           cursorauth.ModelCatalog
		catalogErr        error
		concurrentKey     string
		concurrentWindows map[string]int
		concurrentLevels  map[string][]string
		wantError         string
	}{
		{name: "upstream failure", catalogErr: errors.New("account catalog unavailable"), wantError: "account catalog unavailable"},
		{name: "empty catalog", wantError: "模型列表为空"},
		{name: "live catalog", catalog: cursorauth.ModelCatalog{
			Models: []string{"account-model"}, ContextWindows: map[string]int{},
		}},
		{
			name: "API key changed after discovery", concurrentKey: "replacement-key", wantError: "Cursor 凭证已变更",
			catalog: cursorauth.ModelCatalog{
				Models: []string{"account-model"}, ContextWindows: map[string]int{"account-model": 300000},
			},
		},
		{
			name: "context windows changed after discovery", wantError: "Cursor 模型目录已并发更新",
			concurrentWindows: map[string]int{"account-model": 200000},
			catalog: cursorauth.ModelCatalog{
				Models: []string{"account-model"}, ContextWindows: map[string]int{"account-model": 300000},
			},
		},
		{
			name: "effort levels changed after discovery", wantError: "Cursor 模型目录已并发更新",
			concurrentWindows: map[string]int{"account-model": 300000},
			concurrentLevels:  map[string][]string{"account-model": {"low"}},
			catalog: cursorauth.ModelCatalog{
				Models: []string{"account-model"}, ContextWindows: map[string]int{"account-model": 300000},
				EffortLevels: map[string][]string{"account-model": {"high"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store, cleanup := setupAdminTestServer(t)
			t.Cleanup(cleanup)
			server.channelCache = storage.NewChannelCache(store, time.Minute)
			server.cursorRunner = &fakeCursorRunner{catalog: tc.catalog, err: tc.catalogErr}
			priorWindows := map[string]int{"manual-target": 300000}
			priorLevels := map[string][]string{"manual-target": {"high"}}
			raw, err := (&cursorauth.Credential{
				Type: cursorauth.ChannelType, AccessToken: "access-token", APIKey: "user-api-key", ContextWindows: priorWindows,
				EffortLevels: priorLevels,
			}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			cfg := newCursorOAuthChannel("Cursor-test", raw, []string{"manual-alias"})
			cfg.ModelEntries[0].RedirectModel = "manual-target"
			cfg.ScheduledCheckModel = "manual-alias"
			cfg, err = store.CreateConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}

			if len(tc.catalog.Models) == 0 {
				path := fmt.Sprintf("/admin/channels/%d/models/fetch", cfg.ID)
				c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
				server.HandleFetchModels(c)
				resp := mustParseAPIResponse[FetchModelsResponse](t, w.Body.Bytes())
				if w.Code != http.StatusOK || !resp.Success || resp.Data.Source != "predefined" || len(resp.Data.Models) != len(cursorauth.DefaultModels) {
					t.Fatalf("discovery status=%d body=%s", w.Code, w.Body.String())
				}
			}
			wantWindows := priorWindows
			wantLevels := priorLevels
			if tc.concurrentKey != "" || tc.concurrentWindows != nil {
				wantWindows = tc.concurrentWindows
				wantLevels = tc.concurrentLevels
				server.store = &modelScopeEditStore{Store: store, edit: func(ctx context.Context) error {
					current, err := store.GetConfig(ctx, cfg.ID)
					if err != nil {
						return err
					}
					credential, err := cursorauth.ParseCredential([]byte(current.OAuthCredential))
					if err != nil {
						return err
					}
					if tc.concurrentKey != "" {
						credential.APIKey = tc.concurrentKey
					}
					credential.ContextWindows = tc.concurrentWindows
					credential.EffortLevels = tc.concurrentLevels
					payload, err := credential.JSON()
					if err != nil {
						return err
					}
					updated, err := store.CompareAndSwapOAuthCredential(ctx, cfg.ID, model.AuthTypeCursorOAuth, current.OAuthCredential, payload)
					if err == nil && !updated {
						return errors.New("expected concurrent credential change to succeed")
					}
					return err
				}}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", BatchRefreshModelsRequest{
				ChannelIDs: []int64{cfg.ID}, Mode: "replace",
			}))
			server.HandleBatchRefreshModels(c)
			resp := mustParseAPIResponse[struct {
				Results []BatchRefreshModelsItem `json:"results"`
			}](t, w.Body.Bytes())
			if w.Code != http.StatusOK || !resp.Success || len(resp.Data.Results) != 1 {
				t.Fatalf("replace status=%d body=%s", w.Code, w.Body.String())
			}
			item := resp.Data.Results[0]
			stored, err := store.GetConfig(t.Context(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := cursorauth.ParseCredential([]byte(stored.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" {
				if item.Status != "failed" || !strings.Contains(item.Error, tc.wantError) {
					t.Errorf("replace result = %+v, want failed with %q", item, tc.wantError)
				}
				if !reflect.DeepEqual(stored.ModelEntries, cfg.ModelEntries) || stored.ScheduledCheckModel != cfg.ScheduledCheckModel {
					t.Errorf("replace changed saved models to %+v and scheduled model to %q", stored.ModelEntries, stored.ScheduledCheckModel)
				}
				if !maps.Equal(credential.ContextWindows, wantWindows) {
					t.Errorf("failed refresh left context windows %v, want %v", credential.ContextWindows, wantWindows)
				}
				if !maps.EqualFunc(credential.EffortLevels, wantLevels, slices.Equal) {
					t.Errorf("failed refresh left effort levels %v, want %v", credential.EffortLevels, wantLevels)
				}
				return
			}
			if item.Status != "updated" || len(stored.ModelEntries) != 1 || stored.ModelEntries[0].Model != "account-model" {
				t.Fatalf("replace result = %+v, saved models = %+v", item, stored.ModelEntries)
			}
			if !maps.Equal(credential.ContextWindows, tc.catalog.ContextWindows) {
				t.Errorf("replace context windows = %v, want %v", credential.ContextWindows, tc.catalog.ContextWindows)
			}
			if !maps.EqualFunc(credential.EffortLevels, tc.catalog.EffortLevels, slices.Equal) {
				t.Errorf("replace effort levels = %v, want %v", credential.EffortLevels, tc.catalog.EffortLevels)
			}
		})
	}
}

func TestCreateOrUpdateCursorChannelReauthorizesSameAccount(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	first := &cursorauth.Credential{AccessToken: "tok-1", RefreshToken: "ref-1", Email: "user@example.com", UserID: "auth-1"}
	created, isNew, err := createOrUpdateCursorChannel(ctx, store, first, nil, "")
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateCursorChannel() created=%v err=%v", isNew, err)
	}
	if created.Name != "Cursor-user@example.com" {
		t.Fatalf("name = %q", created.Name)
	}

	second := &cursorauth.Credential{AccessToken: "tok-2", Email: "USER@example.com"}
	updated, isNew, err := createOrUpdateCursorChannel(ctx, store, second, nil, "")
	if err != nil || isNew {
		t.Fatalf("reauthorization created=%v err=%v", isNew, err)
	}
	if updated.ID != created.ID {
		t.Fatalf("id changed: %d -> %d", created.ID, updated.ID)
	}
	credential, err := cursorauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || credential.AccessToken != "tok-2" || credential.RefreshToken != "ref-1" {
		t.Fatalf("credential = %+v err = %v", credential, err)
	}
}

func TestHandleOAuthUsageReturnsCursorQuotaWithoutFakeUnavailableWarning(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "at-cursor-quota", Email: "usage@example.com",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newCursorOAuthChannel(
		"Cursor-usage@example.com", raw, []string{"claude-sonnet-5"},
	))
	if err != nil {
		t.Fatal(err)
	}
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != cursorauth.UsageRPC {
			t.Fatalf("usage request path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer at-cursor-quota" {
			t.Errorf("Authorization = %q", got)
		}
		body := `{
			"billingCycleStart":"1755772740000",
			"billingCycleEnd":"1758451140000",
			"displayMessage":"You've hit your usage limit",
			"planUsage":{"totalSpend":40000,"limit":40000,"remaining":0,"apiPercentUsed":100,"autoPercentUsed":90.5},
			"spendLimitUsage":{"limitType":"user"}
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	server.cursorCredentials = newCursorCredentialManager(store, server.getClientForChannel, func(int64) {})

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "at-cursor-quota") {
		t.Fatalf("usage response leaked credential: %s", w.Body.String())
	}
	response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
	summary := response.Data
	if summary.Provider != cursorauth.ChannelType || summary.PlanType != "user" ||
		summary.DisplayMessage != "You've hit your usage limit" || len(summary.Warnings) != 0 {
		t.Fatalf("usage summary = %#v", summary)
	}
	if summary.QuotaCostUsage != nil {
		t.Fatalf("cursor must not attach quota cost windows: %#v", summary.QuotaCostUsage)
	}
	if len(summary.Windows) != 3 {
		t.Fatalf("windows = %#v", summary.Windows)
	}
	if summary.Windows[0].LimitName != "included" || summary.Windows[0].RemainingPercent != 0 ||
		summary.Windows[0].StandardCostMicroUSD != nil {
		t.Fatalf("included window = %#v", summary.Windows[0])
	}
	if summary.Windows[1].LimitName != "api" || summary.Windows[1].RemainingPercent != 0 {
		t.Fatalf("api window = %#v", summary.Windows[1])
	}
	if summary.Windows[2].LimitName != "auto" || summary.Windows[2].RemainingPercent != 9.5 {
		t.Fatalf("auto window = %#v", summary.Windows[2])
	}

	stored, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cursorauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	persisted, _, _ := persistedOAuthUsage(credential.OAuthUsage, cursorauth.ChannelType)
	if persisted == nil || persisted.DisplayMessage != "You've hit your usage limit" || len(persisted.Warnings) != 0 {
		t.Fatalf("persisted usage = %#v", persisted)
	}
}

func TestHandleOAuthUsageRemintsCursorSessionWhenQuotaRejectsToken(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		APIKey: "user-api-key", Email: "usage@example.com", UserID: "auth-1",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newCursorOAuthChannel(
		"Cursor-usage@example.com", raw, []string{"default"},
	))
	if err != nil {
		t.Fatal(err)
	}
	usageOld, exchange, usageNew := 0, 0, 0
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		auth := request.Header.Get("Authorization")
		switch {
		case request.URL.Path == cursorauth.UsageRPC && auth == "Bearer at-old":
			usageOld++
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"expired-at-old"}`)),
				Request:    request,
			}, nil
		case request.URL.Path == cursorauth.ExchangeAPIKeyPath:
			exchange++
			if auth != "Bearer user-api-key" {
				t.Errorf("exchange Authorization = %q", auth)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"accessToken":"at-new","refreshToken":"rt-new"}`)),
				Request:    request,
			}, nil
		case request.URL.Path == cursorauth.UsageRPC && auth == "Bearer at-new":
			usageNew++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"billingCycleStart":"1755772740000",
					"billingCycleEnd":"1758451140000",
					"planUsage":{"totalSpend":10,"limit":100,"remaining":90,"apiPercentUsed":20,"autoPercentUsed":30},
					"spendLimitUsage":{"limitType":"user"}
				}`)),
				Request: request,
			}, nil
		default:
			t.Fatalf("unexpected request path=%s auth=%q", request.URL.Path, auth)
			return nil, nil
		}
	})}
	server.cursorCredentials = newCursorCredentialManager(store, server.getClientForChannel, func(int64) {})

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"at-old", "at-new", "rt-old", "rt-new", "user-api-key"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("usage response leaked %q: %s", secret, w.Body.String())
		}
	}
	if usageOld != 1 || exchange != 1 || usageNew != 1 {
		t.Fatalf("requests usageOld=%d exchange=%d usageNew=%d", usageOld, exchange, usageNew)
	}
	summary := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes()).Data
	if summary.Provider != cursorauth.ChannelType || len(summary.Windows) != 3 {
		t.Fatalf("usage summary = %#v", summary)
	}

	stored, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cursorauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil || credential.AccessToken != "at-new" || credential.RefreshToken != "rt-new" {
		t.Fatalf("persisted credential = (%#v, %v)", credential, err)
	}
}

func TestOAuthUsageLateCursorRejectionReusesConcurrentWinner(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		APIKey: "user-api-key", Email: "usage-race@example.com", UserID: "auth-race",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newCursorOAuthChannel(
		"Cursor-usage-race@example.com", raw, []string{"default"},
	))
	if err != nil {
		t.Fatal(err)
	}

	var oldRequests atomic.Int32
	var exchanges atomic.Int32
	var newRequests atomic.Int32
	bothOldRequestsStarted := make(chan struct{})
	winnerRequestStarted := make(chan struct{})
	var winnerRequestOnce sync.Once
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		auth := request.Header.Get("Authorization")
		switch {
		case request.URL.Path == cursorauth.UsageRPC && auth == "Bearer at-old":
			switch oldRequests.Add(1) {
			case 1:
				select {
				case <-bothOldRequestsStarted:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
			case 2:
				close(bothOldRequestsStarted)
				select {
				case <-winnerRequestStarted:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
			default:
				return nil, errors.New("unexpected extra request with old Cursor token")
			}
			return cursorTestHTTPResponse(request, http.StatusUnauthorized, `{"error":"expired"}`), nil
		case request.URL.Path == cursorauth.ExchangeAPIKeyPath:
			exchange := exchanges.Add(1)
			if auth != "Bearer user-api-key" {
				return nil, fmt.Errorf("unexpected exchange authorization %q", auth)
			}
			body := fmt.Sprintf(`{"accessToken":"at-new-%d","refreshToken":"rt-new-%d"}`, exchange, exchange)
			return cursorTestHTTPResponse(request, http.StatusOK, body), nil
		case request.URL.Path == cursorauth.UsageRPC && strings.HasPrefix(auth, "Bearer at-new-"):
			newRequests.Add(1)
			if auth == "Bearer at-new-1" {
				winnerRequestOnce.Do(func() { close(winnerRequestStarted) })
			}
			return cursorTestHTTPResponse(request, http.StatusOK, `{
				"billingCycleStart":"1755772740000",
				"billingCycleEnd":"1758451140000",
				"planUsage":{"totalSpend":10,"limit":100,"remaining":90,"apiPercentUsed":20,"autoPercentUsed":30},
				"spendLimitUsage":{"limitType":"user"}
			}`), nil
		default:
			return nil, fmt.Errorf("unexpected Cursor request path=%s auth=%q", request.URL.Path, auth)
		}
	})}
	server.cursorCredentials = newCursorCredentialManager(store, server.getClientForChannel, func(int64) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			summary, summaryErr := server.oauthUsageSummary(ctx, channel)
			if summaryErr == nil && (summary == nil || len(summary.Windows) != 3) {
				summaryErr = fmt.Errorf("unexpected Cursor usage summary: %#v", summary)
			}
			results <- summaryErr
		}()
	}
	close(start)
	for range 2 {
		if resultErr := <-results; resultErr != nil {
			t.Fatalf("oauthUsageSummary() error = %v", resultErr)
		}
	}
	if oldRequests.Load() != 2 || exchanges.Load() != 1 || newRequests.Load() != 2 {
		t.Fatalf("requests old=%d exchanges=%d new=%d", oldRequests.Load(), exchanges.Load(), newRequests.Load())
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cursorauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || credential.AccessToken != "at-new-1" || credential.RefreshToken != "rt-new-1" {
		t.Fatalf("persisted credential = (%#v, %v)", credential, err)
	}
}

func cursorTestHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

/**
 * @brief 验证重新签发 Cursor 会话仍保留同一 API Key 的上下文上限。
 * @param t 测试上下文。
 */
func TestHandleRefreshCursorCredentialRemintsFromAPIKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	windows := map[string]int{"composer-2.5": 200000}
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		APIKey: "user-api-key", Email: "refresh@example.com", UserID: "auth-1",
		ContextWindows: windows,
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newCursorOAuthChannel(
		"Cursor-refresh@example.com", raw, []string{"default"},
	))
	if err != nil {
		t.Fatal(err)
	}
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != cursorauth.ExchangeAPIKeyPath {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer user-api-key" {
			t.Errorf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"accessToken":"at-new","refreshToken":"rt-new"}`)),
			Request:    request,
		}, nil
	})}
	server.cursorCredentials = newCursorCredentialManager(store, server.getClientForChannel, func(int64) {})

	path := fmt.Sprintf("/admin/channels/%d/cursor-credential/refresh", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleRefreshCursorCredential(c)

	if w.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[struct {
		OAuthCredential cursorauth.Credential `json:"oauth_credential"`
	}](t, w.Body.Bytes())
	if resp.Data.OAuthCredential.AccessToken != "at-new" ||
		resp.Data.OAuthCredential.RefreshToken != "rt-new" ||
		resp.Data.OAuthCredential.APIKey != "user-api-key" ||
		resp.Data.OAuthCredential.Email != "refresh@example.com" {
		t.Fatalf("refresh response credential = %#v", resp.Data.OAuthCredential)
	}

	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := cursorauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.AccessToken != "at-new" || persistedCredential.RefreshToken != "rt-new" {
		t.Fatalf("persisted credential = (%#v, %v)", persistedCredential, err)
	}
	if !maps.Equal(persistedCredential.ContextWindows, windows) {
		t.Errorf("refreshed context windows = %v, want %v", persistedCredential.ContextWindows, windows)
	}
}

func TestHandleRefreshCursorCredentialRejectsNonCursorChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	channel, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "api-key", AuthType: model.AuthTypeAPIKey, Enabled: true,
		URLs: model.ChannelURLs{{URL: "https://example.invalid"}}, ModelEntries: []model.ModelEntry{{Model: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/admin/channels/%d/cursor-credential/refresh", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleRefreshCursorCredential(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "channel does not use Cursor OAuth") {
		t.Fatalf("body=%s", w.Body.String())
	}
}
