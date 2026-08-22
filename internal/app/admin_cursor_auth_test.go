package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
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
}

func TestFetchCursorOAuthModelsUsesExactSDKCatalog(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	runner := &fakeCursorRunner{models: []string{"default", "grok-4.6", "composer-2.5"}}
	server.cursorRunner = runner
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "access-token", APIKey: "user-api-key",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg := newCursorOAuthChannel("Cursor-test", raw, []string{"stale-thinking-high"})

	response, err := server.fetchCursorOAuthModels(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("fetchCursorOAuthModels() error = %v", err)
	}
	want := []string{"default", "grok-4.6", "composer-2.5"}
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
}

func TestCreateOrUpdateCursorChannelReauthorizesSameAccount(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	first := &cursorauth.Credential{AccessToken: "tok-1", RefreshToken: "ref-1", Email: "user@example.com", UserID: "auth-1"}
	created, isNew, err := createOrUpdateCursorChannel(ctx, store, first, nil)
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateCursorChannel() created=%v err=%v", isNew, err)
	}
	if created.Name != "Cursor-user@example.com" {
		t.Fatalf("name = %q", created.Name)
	}

	second := &cursorauth.Credential{AccessToken: "tok-2", Email: "USER@example.com"}
	updated, isNew, err := createOrUpdateCursorChannel(ctx, store, second, nil)
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

func TestHandleRefreshCursorCredentialRemintsFromAPIKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	raw, err := (&cursorauth.Credential{
		Type: cursorauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		APIKey: "user-api-key", Email: "refresh@example.com", UserID: "auth-1",
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
