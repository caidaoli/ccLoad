package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const (
	codexTestSubscriptionActiveStart = "2030-01-03T04:05:06Z"
	codexTestSubscriptionActiveUntil = "2030-02-03T04:05:06Z"
)

func codexTestIDToken(t *testing.T, email, accountID string) string {
	return codexTestIDTokenForPlan(t, email, accountID, "plus")
}

func codexTestIDTokenForPlan(t *testing.T, email, accountID, planType string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                accountID,
			"chatgpt_plan_type":                 planType,
			"chatgpt_subscription_active_start": codexTestSubscriptionActiveStart,
			"chatgpt_subscription_active_until": codexTestSubscriptionActiveUntil,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"
}

func newCodexAuthTestStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCodexOAuthCreatesDatabaseChannel(t *testing.T) {
	store := newCodexAuthTestStore(t)
	idToken := codexTestIDToken(t, "user@example.com", "account-1")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") == "" {
			t.Errorf("token form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"at-1","refresh_token":"rt-1","id_token":%q,"expires_in":3600}`, idToken)
	}))
	defer tokenServer.Close()

	service := codexauth.NewService(tokenServer.Client())
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = tokenServer.URL
	manager := newCodexOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	authURL, state, err := manager.start()
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	redirectURI := parsed.Query().Get("redirect_uri")
	if parsed.Query().Get("state") != state || redirectURI == "" {
		t.Fatalf("auth URL query = %v", parsed.Query())
	}
	callbackURL := redirectURI + "?code=code-1&state=" + url.QueryEscape(state)
	response, err := http.Get(callbackURL) //nolint:gosec // local test callback listener
	if err != nil {
		t.Fatalf("OAuth callback error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", response.StatusCode)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		status, ok := manager.status(state)
		if ok && status.Status == "complete" {
			break
		}
		if ok && status.Status == "error" {
			t.Fatalf("OAuth status error = %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("OAuth channel creation timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v), want one channel", len(channels), err)
	}
	channel := channels[0]
	if !channel.UsesCodexOAuth() || !channel.Websockets || channel.KeyCount != 0 || !channel.SupportsModel("gpt-5.4") {
		t.Fatalf("created channel = %#v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != codexUpstreamURL || !channel.URLs[0].Exact || strings.Contains(channel.CodexCredential, "code-1") {
		t.Fatalf("created channel URL/credential = %#v", channel)
	}
}

func TestCodexOAuthManualCallbackCreatesDatabaseChannel(t *testing.T) {
	store := newCodexAuthTestStore(t)
	idToken := codexTestIDToken(t, "manual@example.com", "account-manual")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "manual-code" || r.Form.Get("code_verifier") == "" {
			t.Errorf("token form = %v", r.Form)
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"at-manual","refresh_token":"rt-manual","id_token":%q,"expires_in":3600}`, idToken)
	}))
	defer tokenServer.Close()

	service := codexauth.NewService(tokenServer.Client())
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = tokenServer.URL
	manager := newCodexOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	authURL, state, err := manager.start()
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	redirectURI := parsed.Query().Get("redirect_uri")
	server := &Server{codexOAuth: manager}

	invalidRequest := newJSONRequest(t, http.MethodPost, "/admin/codex/oauth/callback", map[string]any{
		"callback_url": "https://attacker.example/auth/callback?code=stolen&state=" + url.QueryEscape(state),
	})
	invalidContext, invalidResponse := newTestContext(t, invalidRequest)
	server.HandleSubmitCodexOAuthCallback(invalidContext)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid callback status = %d, body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
	if status, ok := manager.status(state); !ok || status.Status != "pending" {
		t.Fatalf("invalid callback changed OAuth status = (%#v, %v)", status, ok)
	}

	callbackURL := redirectURI + "?code=manual-code&state=" + url.QueryEscape(state)
	request := newJSONRequest(t, http.MethodPost, "/admin/codex/oauth/callback", map[string]any{
		"callback_url": callbackURL,
	})
	callbackContext, response := newTestContext(t, request)
	server.HandleSubmitCodexOAuthCallback(callbackContext)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"accepted"`) {
		t.Fatalf("manual callback response = %d, body=%s", response.Code, response.Body.String())
	}

	duplicateRequest := newJSONRequest(t, http.MethodPost, "/admin/codex/oauth/callback", map[string]any{
		"callback_url": callbackURL,
	})
	duplicateContext, duplicateResponse := newTestContext(t, duplicateRequest)
	server.HandleSubmitCodexOAuthCallback(duplicateContext)
	if duplicateResponse.Code == http.StatusOK {
		t.Fatalf("duplicate callback unexpectedly accepted: %s", duplicateResponse.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		status, ok := manager.status(state)
		if ok && status.Status == "complete" {
			break
		}
		if ok && status.Status == "error" {
			t.Fatalf("OAuth status error = %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("manual OAuth channel creation timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 || !channels[0].UsesCodexOAuth() {
		t.Fatalf("manual callback channels = (%#v, %v)", channels, err)
	}
}

func TestCodexOAuthCancelStopsPendingSessionAndAllowsRestart(t *testing.T) {
	store := newCodexAuthTestStore(t)
	service := codexauth.NewService(http.DefaultClient)
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = "https://auth.example.test/token"
	manager := newCodexOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	authURL, state, err := manager.start()
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	callbackURL := parsed.Query().Get("redirect_uri") + "?code=cancelled-code&state=" + url.QueryEscape(state)
	server := &Server{codexOAuth: manager}

	request := newJSONRequest(t, http.MethodPost, "/admin/codex/oauth/cancel", map[string]any{"state": state})
	cancelContext, response := newTestContext(t, request)
	server.HandleCancelCodexOAuth(cancelContext)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel response = %d, body=%s", response.Code, response.Body.String())
	}
	status, ok := manager.status(state)
	if !ok || status.Status != "cancelled" {
		t.Fatalf("cancelled OAuth status = (%#v, %v)", status, ok)
	}
	if _, err := manager.submitCallbackURL(callbackURL); err == nil {
		t.Fatal("cancelled OAuth callback unexpectedly accepted")
	}

	_, restartedState, err := manager.start()
	if err != nil {
		t.Fatalf("restart after cancel error = %v", err)
	}
	if restartedState == state {
		t.Fatalf("restarted OAuth state = %q, want a new state", restartedState)
	}
}

func TestCodexOAuthStartReplacesExistingPendingSession(t *testing.T) {
	store := newCodexAuthTestStore(t)
	service := codexauth.NewService(http.DefaultClient)
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = "https://auth.example.test/token"
	manager := newCodexOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	_, firstState, err := manager.start()
	if err != nil {
		t.Fatalf("first start() error = %v", err)
	}
	_, secondState, err := manager.start()
	if err != nil {
		t.Fatalf("second start() error = %v", err)
	}
	if secondState == firstState {
		t.Fatalf("replacement state = %q, want a new state", secondState)
	}
	firstStatus, ok := manager.status(firstState)
	if !ok || firstStatus.Status != "cancelled" {
		t.Fatalf("replaced OAuth status = (%#v, %v)", firstStatus, ok)
	}
	secondStatus, ok := manager.status(secondState)
	if !ok || secondStatus.Status != "pending" {
		t.Fatalf("replacement OAuth status = (%#v, %v)", secondStatus, ok)
	}
}

func TestCodexOAuthCancelInterruptsTokenExchangeWithoutCreatingChannel(t *testing.T) {
	store := newCodexAuthTestStore(t)
	tokenStarted := make(chan struct{})
	tokenCancelled := make(chan struct{})
	releaseTokenServer := make(chan struct{})
	defer close(releaseTokenServer)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		close(tokenStarted)
		select {
		case <-r.Context().Done():
			close(tokenCancelled)
		case <-releaseTokenServer:
		}
	}))
	defer tokenServer.Close()

	service := codexauth.NewService(tokenServer.Client())
	service.AuthorizationURL = "https://auth.example.test/authorize"
	service.TokenURL = tokenServer.URL
	manager := newCodexOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	authURL, state, err := manager.start()
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	callbackURL := parsed.Query().Get("redirect_uri") + "?code=in-flight-code&state=" + url.QueryEscape(state)
	if _, err := manager.submitCallbackURL(callbackURL); err != nil {
		t.Fatalf("submitCallbackURL() error = %v", err)
	}

	select {
	case <-tokenStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("token exchange did not start")
	}
	if err := manager.cancel(state); err != nil {
		t.Fatalf("cancel() error = %v", err)
	}
	select {
	case <-tokenCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("token exchange context was not cancelled")
	}

	status, ok := manager.status(state)
	if !ok || status.Status != "cancelled" {
		t.Fatalf("cancelled OAuth status = (%#v, %v)", status, ok)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 0 {
		t.Fatalf("channels after cancellation = (%#v, %v), want none", channels, err)
	}
}

func TestImportedCodexCredentialUpsertsSameAccount(t *testing.T) {
	store := newCodexAuthTestStore(t)
	now := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	first := &codexauth.Credential{
		Type: "codex", AccessToken: "at-1", RefreshToken: "rt-1", Expired: now,
		AccountID: "account-1", Email: "user@example.com",
	}
	created, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, first)
	if err != nil || !wasCreated {
		t.Fatalf("first import = (%#v, %v, %v)", created, wasCreated, err)
	}
	wantModels := []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex-spark",
		"codex-auto-review",
	}
	if got := created.GetModels(); !slices.Equal(got, wantModels) {
		t.Fatalf("imported channel models = %v, want %v", got, wantModels)
	}
	legacy := created.Clone()
	legacy.ModelEntries = []model.ModelEntry{{Model: "*"}}
	if _, err := store.UpdateConfig(context.Background(), created.ID, legacy); err != nil {
		t.Fatalf("prepare legacy wildcard channel: %v", err)
	}
	second := &codexauth.Credential{
		Type: "codex", AccessToken: "at-2", RefreshToken: "rt-2", Expired: now,
		AccountID: "account-1", Email: "renamed@example.com",
	}
	updated, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, second)
	if err != nil || wasCreated {
		t.Fatalf("second import = (%#v, %v, %v)", updated, wasCreated, err)
	}
	if updated.ID != created.ID || !strings.Contains(updated.CodexCredential, `"access_token":"at-2"`) {
		t.Fatalf("updated channel = %#v", updated)
	}
	if got := updated.GetModels(); !slices.Equal(got, wantModels) {
		t.Fatalf("reimported legacy channel models = %v, want %v", got, wantModels)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v), want one channel", len(channels), err)
	}
}

func TestImportedCodexCredentialRemovesModelsUnsupportedByPlan(t *testing.T) {
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	plus := &codexauth.Credential{
		Type: "codex", AccessToken: "at-plus", RefreshToken: "rt-plus", Expired: expiresAt,
		AccountID: "account-plan", Email: "plan@example.com", PlanType: "plus",
	}
	created, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, plus)
	if err != nil || !wasCreated {
		t.Fatalf("plus import = (%#v, %v, %v)", created, wasCreated, err)
	}
	if !created.SupportsModel("gpt-5.6-sol") || !created.SupportsModel("gpt-5.4") || !created.SupportsModel("gpt-5.3-codex-spark") {
		t.Fatalf("plus channel models = %v", created.GetModels())
	}

	free := &codexauth.Credential{
		Type: "codex", AccessToken: "at-free", RefreshToken: "rt-free", Expired: expiresAt,
		AccountID: "account-plan", Email: "plan@example.com", PlanType: "free",
	}
	updated, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, free)
	if err != nil || wasCreated {
		t.Fatalf("free reimport = (%#v, %v, %v)", updated, wasCreated, err)
	}
	want := []string{
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4-mini",
		"codex-auto-review",
	}
	if got := updated.GetModels(); !slices.Equal(got, want) {
		t.Fatalf("free channel models = %v, want %v", got, want)
	}
}

func TestImportedCodexCredentialModelsFollowPlanType(t *testing.T) {
	allModels := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark", "codex-auto-review",
	}
	teamModels := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "codex-auto-review",
	}
	freeModels := []string{
		"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4-mini", "codex-auto-review",
	}
	tests := []struct {
		plan string
		want []string
	}{
		{plan: "free", want: freeModels},
		{plan: "team", want: teamModels},
		{plan: "business", want: teamModels},
		{plan: "go", want: teamModels},
		{plan: "plus", want: allModels},
		{plan: "pro", want: allModels},
		{plan: "enterprise", want: allModels},
		{plan: "", want: allModels},
	}
	for _, tt := range tests {
		t.Run(tt.plan, func(t *testing.T) {
			store := newCodexAuthTestStore(t)
			credential := &codexauth.Credential{
				Type: "codex", AccessToken: "at", RefreshToken: "rt", PlanType: tt.plan,
				Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-" + tt.plan,
			}
			channel, created, err := createOrUpdateCodexChannel(context.Background(), store, credential)
			if err != nil || !created {
				t.Fatalf("create channel = (%#v, %v, %v)", channel, created, err)
			}
			if got := channel.GetModels(); !slices.Equal(got, tt.want) {
				t.Fatalf("plan %q models = %v, want %v", tt.plan, got, tt.want)
			}
		})
	}
}

func TestHandleImportCodexCredentialCreatesSkipsAndReportsFilesWithoutLeakingTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store}
	engine := gin.New()
	engine.POST("/codex/credentials/import", server.HandleImportCodexCredential)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	existing, _, err := createOrUpdateCodexChannel(context.Background(), store, &codexauth.Credential{
		Type: "codex", AccessToken: "at-existing", RefreshToken: "rt-existing", Expired: expiresAt,
		AccountID: "account-existing", Email: "duplicate@example.com",
	})
	if err != nil {
		t.Fatalf("create existing Codex channel: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	files := []struct {
		name string
		body string
	}{
		{
			name: "duplicate.json",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-must-not-overwrite","refresh_token":"rt-must-not-overwrite","account_id":"account-existing","email":"duplicate@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "new.json",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-import-secret","refresh_token":"rt-import-secret","account_id":"account-import","email":"new@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{name: "broken.json", body: `{"type":"codex"`},
	}
	for _, file := range files {
		part, partErr := writer.CreateFormFile("files", file.name)
		if partErr != nil {
			t.Fatalf("CreateFormFile(%q) error = %v", file.name, partErr)
		}
		if _, writeErr := part.Write([]byte(file.body)); writeErr != nil {
			t.Fatalf("write multipart credential %q: %v", file.name, writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/codex/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "at-import-secret") || strings.Contains(response.Body.String(), "rt-import-secret") ||
		strings.Contains(response.Body.String(), "at-must-not-overwrite") || strings.Contains(response.Body.String(), "rt-must-not-overwrite") {
		t.Fatalf("import response leaked credential: %s", response.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Created int `json:"created"`
			Skipped int `json:"skipped"`
			Failed  int `json:"failed"`
			Results []struct {
				FileName    string `json:"file_name"`
				ChannelName string `json:"channel_name,omitempty"`
				Status      string `json:"status"`
				Error       string `json:"error,omitempty"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if !payload.Success || payload.Data.Created != 1 || payload.Data.Skipped != 1 || payload.Data.Failed != 1 || len(payload.Data.Results) != 3 {
		t.Fatalf("import response = %#v", payload)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 2 {
		t.Fatalf("persisted channels = (%#v, %v)", channels, err)
	}
	persistedExisting, err := store.GetConfig(context.Background(), existing.ID)
	if err != nil {
		t.Fatalf("get existing channel: %v", err)
	}
	if !strings.Contains(persistedExisting.CodexCredential, `"access_token":"at-existing"`) ||
		strings.Contains(persistedExisting.CodexCredential, "must-not-overwrite") {
		t.Fatalf("duplicate import overwrote existing channel")
	}
	var created *model.Config
	for _, channel := range channels {
		if channel.Name == "Codex - new@example.com" {
			created = channel
			break
		}
	}
	if created == nil || !created.UsesCodexOAuth() {
		t.Fatalf("new Codex channel was not created: %#v", channels)
	}
}

func TestHandleChannelEditorExposesCodexCredentialOnlyInEditorData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &codexauth.Credential{
		Type:         "codex",
		IDToken:      codexTestIDTokenForPlan(t, "editor@example.com", "account-editor", "plus"),
		AccessToken:  "at-editor-secret",
		RefreshToken: "rt-editor-secret",
		Expired:      time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		AccountID:    "account-editor",
		Email:        "editor@example.com",
		PlanType:     "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}
	path := fmt.Sprintf("/admin/channels/%d/editor", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}

	server.HandleChannelEditor(c)

	if w.Code != http.StatusOK {
		t.Fatalf("editor status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[struct {
		Keys                []*model.APIKey        `json:"keys"`
		CodexCredential     json.RawMessage        `json:"codex_credential"`
		CodexCredentialInfo *codexauth.IDTokenInfo `json:"codex_credential_info"`
		Channel             struct {
			CodexPlanType                string     `json:"codex_plan_type"`
			CodexSubscriptionActiveUntil *time.Time `json:"codex_subscription_active_until"`
		} `json:"channel"`
	}](t, w.Body.Bytes())
	if len(resp.Data.Keys) != 1 || resp.Data.Keys[0].APIKey != "at-editor-secret" {
		t.Fatalf("editor keys = %#v, want read-only AT", resp.Data.Keys)
	}
	var exposed codexauth.Credential
	if err := json.Unmarshal(resp.Data.CodexCredential, &exposed); err != nil {
		t.Fatalf("decode editor credential: %v; raw=%s", err, resp.Data.CodexCredential)
	}
	if exposed.AccessToken != credential.AccessToken || exposed.RefreshToken != credential.RefreshToken || exposed.AccountID != credential.AccountID {
		t.Fatalf("editor credential = %#v", exposed)
	}
	if resp.Data.CodexCredentialInfo == nil || resp.Data.CodexCredentialInfo.ChatGPTAccountID != "account-editor" ||
		resp.Data.CodexCredentialInfo.ChatGPTSubscriptionActiveStart != codexTestSubscriptionActiveStart ||
		resp.Data.CodexCredentialInfo.ChatGPTSubscriptionActiveUntil != codexTestSubscriptionActiveUntil ||
		resp.Data.CodexCredentialInfo.PlanType != "plus" {
		t.Fatalf("editor decoded credential info = %#v", resp.Data.CodexCredentialInfo)
	}
	if resp.Data.Channel.CodexPlanType != "plus" {
		t.Fatalf("editor channel plan type = %q, want plus", resp.Data.Channel.CodexPlanType)
	}
	wantUntil, err := time.Parse(time.RFC3339, codexTestSubscriptionActiveUntil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Data.Channel.CodexSubscriptionActiveUntil == nil ||
		!resp.Data.Channel.CodexSubscriptionActiveUntil.Equal(wantUntil) {
		t.Fatalf("editor subscription until = %v, want %v", resp.Data.Channel.CodexSubscriptionActiveUntil, wantUntil)
	}

	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].CodexPlanType != "plus" {
		t.Fatalf("channel list plan type = %#v, want plus", list.Data)
	}
	if list.Data[0].CodexSubscriptionActiveUntil == nil ||
		!list.Data[0].CodexSubscriptionActiveUntil.Equal(wantUntil) {
		t.Fatalf("channel list subscription until = %v, want %v", list.Data[0].CodexSubscriptionActiveUntil, wantUntil)
	}
	if strings.Contains(listResponse.Body.String(), "at-editor-secret") || strings.Contains(listResponse.Body.String(), "rt-editor-secret") {
		t.Fatalf("channel list leaked Codex credential: %s", listResponse.Body.String())
	}

	detailPath := fmt.Sprintf("/admin/channels/%d", channel.ID)
	detailContext, detailResponse := newTestContext(t, newRequest(http.MethodGet, detailPath, nil))
	detailContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleChannelByID(detailContext)
	if strings.Contains(detailResponse.Body.String(), "at-editor-secret") || strings.Contains(detailResponse.Body.String(), "rt-editor-secret") {
		t.Fatalf("ordinary channel response leaked Codex credential: %s", detailResponse.Body.String())
	}
}

func TestCodexChannelKeyMutationEndpointsAreReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: "codex", AccessToken: "at", RefreshToken: "rt",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-read-only", PlanType: "free",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}
	server := &Server{store: store}
	engine := gin.New()
	engine.PUT("/channels/:id", server.HandleChannelByID)
	engine.DELETE("/channels/:id/keys/:keyIndex", server.HandleDeleteAPIKey)

	update := fmt.Sprintf(`{"name":%q,"auth_type":"codex_oauth","urls":[{"url":%q,"exact":true,"protocols":["codex"]}],"api_key":"forbidden","models":[{"model":"*"}],"enabled":true,"websockets":true}`, channel.Name, codexUpstreamURL)
	updateRequest := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/channels/%d", channel.ID), strings.NewReader(update))
	updateRequest.Header.Set("Content-Type", "application/json")
	updateResponse := httptest.NewRecorder()
	engine.ServeHTTP(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusConflict {
		t.Fatalf("key update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}

	deleteResponse := httptest.NewRecorder()
	engine.ServeHTTP(deleteResponse, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/channels/%d/keys/0", channel.ID), nil))
	if deleteResponse.Code != http.StatusConflict {
		t.Fatalf("key delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}

	submittedModels := append([]model.ModelEntry(nil), channel.ModelEntries...)
	submittedModels = append(submittedModels, model.ModelEntry{Model: "gpt-5.4"})
	allowedUpdate, err := json.Marshal(map[string]any{
		"name":                    "codex-renamed",
		"auth_type":               model.AuthTypeCodexOAuth,
		"urls":                    channel.URLs,
		"api_key":                 "",
		"api_keys":                []ChannelAPIKeyRequest{},
		"models":                  submittedModels,
		"enabled":                 true,
		"websockets":              true,
		"protocol_transform_mode": model.ProtocolTransformModeAuto,
	})
	if err != nil {
		t.Fatalf("marshal allowed update: %v", err)
	}
	allowedRequest := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/channels/%d", channel.ID), bytes.NewReader(allowedUpdate))
	allowedRequest.Header.Set("Content-Type", "application/json")
	allowedResponse := httptest.NewRecorder()
	engine.ServeHTTP(allowedResponse, allowedRequest)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("allowed update status=%d body=%s", allowedResponse.Code, allowedResponse.Body.String())
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetConfig() after allowed update error = %v", err)
	}
	if persisted.Name != "codex-renamed" || persisted.CodexCredential != channel.CodexCredential {
		t.Fatalf("allowed update changed credential or missed name: %#v", persisted)
	}
	if persisted.SupportsModel("gpt-5.4") {
		t.Fatalf("free Codex channel kept unsupported model: %v", persisted.GetModels())
	}
	keys, err := store.GetAPIKeys(context.Background(), channel.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("Codex API keys after allowed update = (%#v, %v)", keys, err)
	}
}

func TestCodexCredentialRefreshIsSingleflightAndPersistsToDatabase(t *testing.T) {
	store := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: "codex", AccessToken: "at-old", RefreshToken: "rt-old",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-refresh", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}

	var refreshCount atomic.Int32
	freeIDToken := codexTestIDTokenForPlan(t, "refresh@example.com", "account-refresh", "free")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
			t.Errorf("refresh form = %v", r.Form)
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"at-new","refresh_token":"rt-new","id_token":%q,"expires_in":604800}`, freeIDToken)
	}))
	defer tokenServer.Close()

	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	manager := newCodexCredentialManager(service, store, nil, nil)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *codexauth.Credential, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, getErr := manager.credential(context.Background(), channel, false)
			results <- got
			errs <- getErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for getErr := range errs {
		if getErr != nil {
			t.Fatalf("credential() error = %v", getErr)
		}
	}
	for got := range results {
		if got == nil || got.AccessToken != "at-new" || got.RefreshToken != "rt-new" {
			t.Fatalf("credential() = %#v", got)
		}
	}
	if got := refreshCount.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.CodexCredential))
	if err != nil {
		t.Fatalf("ParseCredential() persisted refresh error = %v", err)
	}
	if persistedCredential.AccessToken != "at-new" || persistedCredential.RefreshToken != "rt-new" ||
		persistedCredential.IDToken != freeIDToken {
		t.Fatalf("persisted refreshed credential = %#v", persistedCredential)
	}
	if persisted.SupportsModel("gpt-5.6-sol") || persisted.SupportsModel("gpt-5.4") || persisted.SupportsModel("gpt-5.3-codex-spark") {
		t.Fatalf("refreshed free channel kept unsupported models: %v", persisted.GetModels())
	}
}

func TestHandleRefreshCodexCredentialForcesDatabaseRefresh(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &codexauth.Credential{
		Type: "codex", AccessToken: "at-old", RefreshToken: "rt-old",
		Expired:   time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		AccountID: "account-manual-refresh", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}

	idToken := codexTestIDTokenForPlan(t, "manual-refresh@example.com", "account-manual-refresh", "team")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
			t.Errorf("refresh form = %v", r.Form)
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"at-manual-new","refresh_token":"rt-manual-new","id_token":%q,"expires_in":604800}`, idToken)
	}))
	defer tokenServer.Close()
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	server.codexCredentials = newCodexCredentialManager(
		service,
		store,
		func(*model.Config) *http.Client { return tokenServer.Client() },
		nil,
	)

	path := fmt.Sprintf("/admin/channels/%d/codex-credential/refresh", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleRefreshCodexCredential(c)

	if w.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[struct {
		CodexCredential     codexauth.Credential   `json:"codex_credential"`
		CodexCredentialInfo *codexauth.IDTokenInfo `json:"codex_credential_info"`
		CodexPlanType       string                 `json:"codex_plan_type"`
	}](t, w.Body.Bytes())
	if resp.Data.CodexCredential.AccessToken != "at-manual-new" ||
		resp.Data.CodexCredential.RefreshToken != "rt-manual-new" ||
		resp.Data.CodexCredential.IDToken != idToken || resp.Data.CodexPlanType != "team" {
		t.Fatalf("refresh response credential = %#v", resp.Data)
	}
	if resp.Data.CodexCredentialInfo == nil || resp.Data.CodexCredentialInfo.ChatGPTAccountID != "account-manual-refresh" ||
		resp.Data.CodexCredentialInfo.ChatGPTSubscriptionActiveStart != codexTestSubscriptionActiveStart ||
		resp.Data.CodexCredentialInfo.ChatGPTSubscriptionActiveUntil != codexTestSubscriptionActiveUntil ||
		resp.Data.CodexCredentialInfo.PlanType != "team" {
		t.Fatalf("refresh response decoded info = %#v", resp.Data.CodexCredentialInfo)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.CodexCredential))
	if err != nil || persistedCredential.AccessToken != "at-manual-new" || persistedCredential.IDToken != idToken {
		t.Fatalf("persisted credential = (%#v, %v)", persistedCredential, err)
	}
}
