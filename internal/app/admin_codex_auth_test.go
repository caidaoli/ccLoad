package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const (
	codexTestSubscriptionActiveStart = "2030-01-03T04:05:06Z"
	codexTestSubscriptionActiveUntil = "2030-02-03T04:05:06Z"
)

type oauthUsageRoundTripper func(*http.Request) (*http.Response, error)

func (f oauthUsageRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

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
	if channel.Name != "Codex-user@example.com" || !channel.UsesCodexOAuth() || !channel.Websockets || channel.KeyCount != 0 || !channel.SupportsModel("gpt-5.4") {
		t.Fatalf("created channel = %#v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != codexUpstreamURL || !channel.URLs[0].Exact || strings.Contains(channel.OAuthCredential, "code-1") {
		t.Fatalf("created channel URL/credential = %#v", channel)
	}
}

func TestAntigravityOAuthCreatesDatabaseChannel(t *testing.T) {
	store := newCodexAuthTestStore(t)
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "gravity-code" || r.Form.Get("code_verifier") != "" {
				t.Errorf("token form = %v", r.Form)
			}
			_, _ = io.WriteString(w, `{"access_token":"gravity-at","refresh_token":"gravity-rt","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"email":"gravity@example.com"}`)
		case "/v1internal:loadCodeAssist":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"gravity-project"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oauthServer.Close()

	service := antigravityauth.NewService(oauthServer.Client())
	service.AuthorizationURL = "https://accounts.example.test/authorize"
	service.TokenURL = oauthServer.URL + "/token"
	service.UserInfoURL = oauthServer.URL + "/userinfo"
	service.APIBaseURL = oauthServer.URL
	manager := newAntigravityOAuthManager(service, store, nil)
	manager.listenAddr = "127.0.0.1:0"
	manager.timeout = 2 * time.Second
	defer manager.close()

	authURL, state, err := manager.start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	redirectURI := parsed.Query().Get("redirect_uri")
	if parsed.Query().Get("state") != state || !strings.HasSuffix(redirectURI, "/oauth-callback") || parsed.Query().Get("code_challenge") != "" {
		t.Fatalf("Antigravity auth URL query = %v", parsed.Query())
	}
	response, err := http.Get(redirectURI + "?code=gravity-code&state=" + url.QueryEscape(state)) //nolint:gosec // local callback listener
	if err != nil {
		t.Fatalf("callback: %v", err)
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
			t.Fatal("Antigravity OAuth channel creation timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatalf("ListConfigs = (%d, %v)", len(channels), err)
	}
	channel := channels[0]
	if channel.Name != "Antigravity-gravity@example.com" || !channel.UsesAntigravityOAuth() || channel.KeyCount != 0 || channel.Websockets || channel.GetProtocolTransformMode() != model.ProtocolTransformModeLocal {
		t.Fatalf("created Antigravity channel = %#v", channel)
	}
	if len(channel.URLs) != 2 || !channel.SupportsModel("gemini-3-flash") || !strings.Contains(channel.OAuthCredential, `"project_id":"gravity-project"`) {
		t.Fatalf("created Antigravity channel contract = %#v", channel)
	}
}

func TestAntigravityChannelEditorExposesCredentialOnlyInEditor(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "gravity-editor-at", RefreshToken: "gravity-editor-rt",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "editor@example.com", ProjectID: "editor-project",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity editor", payload))
	if err != nil {
		t.Fatal(err)
	}

	requestContext, response := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/editor", channel.ID), nil))
	requestContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleChannelEditor(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("editor status=%d body=%s", response.Code, response.Body.String())
	}
	editor := mustParseAPIResponse[struct {
		Keys            []*model.APIKey `json:"keys"`
		OAuthCredential json.RawMessage `json:"oauth_credential"`
	}](t, response.Body.Bytes())
	if len(editor.Data.Keys) != 1 || editor.Data.Keys[0].APIKey != "gravity-editor-at" || !strings.Contains(string(editor.Data.OAuthCredential), `"project_id":"editor-project"`) {
		t.Fatalf("editor data=%#v", editor.Data)
	}

	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	if strings.Contains(listResponse.Body.String(), "gravity-editor-at") || strings.Contains(listResponse.Body.String(), "gravity-editor-rt") {
		t.Fatalf("channel list leaked Antigravity credential: %s", listResponse.Body.String())
	}
}

func TestHandleImportAntigravityCredentialCreatesSkipsAndDoesNotLeakTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store}
	existingCredential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-existing", RefreshToken: "rt-existing",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "duplicate@example.com", ProjectID: "project-existing",
	}
	existingPayload, err := existingCredential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity-duplicate@example.com", existingPayload))
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	files := []struct {
		name string
		body string
	}{
		{name: "duplicate.json", body: fmt.Sprintf(`{"type":"antigravity","access_token":"at-must-not-overwrite","refresh_token":"rt-must-not-overwrite","expired":%q,"email":"duplicate@example.com","project_id":"project-other"}`, expiresAt)},
		{name: "new.json", body: fmt.Sprintf(`{"type":"antigravity","access_token":"at-import-secret","refresh_token":"rt-import-secret","expired":%q,"email":"new@example.com","project_id":"project-new"}`, expiresAt)},
		{name: "broken.json", body: `{"type":"antigravity"`},
	}
	for _, file := range files {
		part, createErr := writer.CreateFormFile("files", file.name)
		if createErr != nil {
			t.Fatalf("CreateFormFile(%q): %v", file.name, createErr)
		}
		if _, writeErr := part.Write([]byte(file.body)); writeErr != nil {
			t.Fatalf("write %q: %v", file.name, writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/antigravity/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportAntigravityCredential(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"at-import-secret", "rt-import-secret", "at-must-not-overwrite", "rt-must-not-overwrite"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("import response leaked %q: %s", secret, response.Body.String())
		}
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 1 || result.Data.Skipped != 1 || result.Data.Failed != 1 || len(result.Data.Results) != 3 {
		t.Fatalf("import summary = %#v", result.Data)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 2 {
		t.Fatalf("channels = (%#v, %v)", channels, err)
	}
	var imported *model.Config
	for _, channel := range channels {
		if channel.Name == "Antigravity-new@example.com" {
			imported = channel
			break
		}
	}
	if imported == nil || !imported.UsesAntigravityOAuth() {
		t.Fatalf("new Antigravity channel was not created with canonical name: %#v", channels)
	}
	persisted, err := store.GetConfig(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.OAuthCredential != existingPayload {
		t.Fatalf("same-name import overwrote existing credential")
	}
}

func TestHandleImportOAuthCredentialsAutoDetectsTypeAndIncrementsCreatedPriorities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("priority_increment", "20"); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name string
		body string
	}{
		{
			name: "codex-explicit.json",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-codex-explicit","refresh_token":"rt-codex-explicit","account_id":"account-explicit","email":"codex-explicit@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "antigravity-explicit.json",
			body: fmt.Sprintf(
				`{"type":"antigravity","access_token":"at-gravity-explicit","refresh_token":"rt-gravity-explicit","email":"gravity-explicit@example.com","project_id":"project-explicit","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "codex-inferred.json",
			body: fmt.Sprintf(
				`{"access_token":"at-codex-inferred","refresh_token":"rt-codex-inferred","account_id":"account-inferred","email":"codex-inferred@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "antigravity-inferred.json",
			body: fmt.Sprintf(
				`{"access_token":"at-gravity-inferred","refresh_token":"rt-gravity-inferred","email":"gravity-inferred@example.com","project_id":"project-inferred","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "ambiguous.json",
			body: fmt.Sprintf(
				`{"access_token":"at-ambiguous","refresh_token":"rt-ambiguous","email":"ambiguous@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "unsupported.json",
			body: fmt.Sprintf(
				`{"type":"other","access_token":"at-unsupported","refresh_token":"rt-unsupported","account_id":"account-unsupported","expired":%q}`,
				expiresAt,
			),
		},
	}
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatalf("CreateFormFile(%q): %v", file.name, err)
		}
		if _, err := part.Write([]byte(file.body)); err != nil {
			t.Fatalf("write %q: %v", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportOAuthCredentials(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{
		"at-codex-explicit", "rt-codex-explicit", "at-gravity-explicit", "rt-gravity-explicit",
		"at-codex-inferred", "rt-codex-inferred", "at-gravity-inferred", "rt-gravity-inferred",
		"at-ambiguous", "rt-ambiguous", "at-unsupported", "rt-unsupported",
	} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("import response leaked %q: %s", secret, response.Body.String())
		}
	}

	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 4 || result.Data.Skipped != 2 || result.Data.Failed != 0 || len(result.Data.Results) != len(files) {
		t.Fatalf("import summary = %#v", result.Data)
	}
	if result.Data.Results[4].Status != "skipped" || result.Data.Results[5].Status != "skipped" {
		t.Fatalf("unrecognized credentials were not skipped: %#v", result.Data.Results)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 4 {
		t.Fatalf("channels = (%#v, %v)", channels, err)
	}
	want := map[string]struct {
		authType string
		priority int
	}{
		"Codex-codex-explicit@example.com":         {authType: model.AuthTypeCodexOAuth, priority: 0},
		"Antigravity-gravity-explicit@example.com": {authType: model.AuthTypeAntigravityOAuth, priority: 20},
		"Codex-codex-inferred@example.com":         {authType: model.AuthTypeCodexOAuth, priority: 40},
		"Antigravity-gravity-inferred@example.com": {authType: model.AuthTypeAntigravityOAuth, priority: 60},
	}
	for _, channel := range channels {
		expected, ok := want[channel.Name]
		if !ok {
			t.Fatalf("unexpected channel %#v", channel)
		}
		if channel.GetAuthType() != expected.authType || channel.Priority != expected.priority {
			t.Fatalf("channel %q auth_type=%q priority=%d, want %q/%d", channel.Name, channel.GetAuthType(), channel.Priority, expected.authType, expected.priority)
		}
	}
}

func TestHandleImportOAuthCredentialsRejectsInvalidOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	tests := []struct {
		name              string
		provider          string
		priorityIncrement string
	}{
		{name: "provider", provider: "unknown", priorityIncrement: "0"},
		{name: "priority increment", provider: "auto", priorityIncrement: "30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("provider", tt.provider); err != nil {
				t.Fatal(err)
			}
			if err := writer.WriteField("priority_increment", tt.priorityIncrement); err != nil {
				t.Fatal(err)
			}
			part, err := writer.CreateFormFile("files", "credential.json")
			if err != nil {
				t.Fatal(err)
			}
			credential := fmt.Sprintf(
				`{"type":"codex","access_token":"at","refresh_token":"rt","account_id":"account","expired":%q}`,
				expiresAt,
			)
			if _, err := part.Write([]byte(credential)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			requestContext, response := newTestContext(t, request)
			server.HandleImportOAuthCredentials(requestContext)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 0 {
		t.Fatalf("invalid import persisted channels: (%#v, %v)", channels, err)
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

func TestImportedOAuthCredentialUpsertsSameAccount(t *testing.T) {
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
	if updated.ID != created.ID || !strings.Contains(updated.OAuthCredential, `"access_token":"at-2"`) {
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

func TestImportedOAuthCredentialRemovesModelsUnsupportedByPlan(t *testing.T) {
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

func TestImportedOAuthCredentialModelsFollowPlanType(t *testing.T) {
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
	if !strings.Contains(persistedExisting.OAuthCredential, `"access_token":"at-existing"`) ||
		strings.Contains(persistedExisting.OAuthCredential, "must-not-overwrite") {
		t.Fatalf("duplicate import overwrote existing channel")
	}
	var created *model.Config
	for _, channel := range channels {
		if channel.Name == "Codex-new@example.com" {
			created = channel
			break
		}
	}
	if created == nil || !created.UsesCodexOAuth() {
		t.Fatalf("new Codex channel was not created: %#v", channels)
	}
}

func TestHandleChannelEditorExposesOAuthCredentialOnlyInEditorData(t *testing.T) {
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
		OAuthCredential     json.RawMessage        `json:"oauth_credential"`
		OAuthCredentialInfo *codexauth.IDTokenInfo `json:"oauth_credential_info"`
		Channel             struct {
			CodexPlanType                string     `json:"codex_plan_type"`
			CodexSubscriptionActiveUntil *time.Time `json:"codex_subscription_active_until"`
		} `json:"channel"`
	}](t, w.Body.Bytes())
	if len(resp.Data.Keys) != 1 || resp.Data.Keys[0].APIKey != "at-editor-secret" {
		t.Fatalf("editor keys = %#v, want read-only AT", resp.Data.Keys)
	}
	var exposed codexauth.Credential
	if err := json.Unmarshal(resp.Data.OAuthCredential, &exposed); err != nil {
		t.Fatalf("decode editor credential: %v; raw=%s", err, resp.Data.OAuthCredential)
	}
	if exposed.AccessToken != credential.AccessToken || exposed.RefreshToken != credential.RefreshToken || exposed.AccountID != credential.AccountID {
		t.Fatalf("editor credential = %#v", exposed)
	}
	if resp.Data.OAuthCredentialInfo == nil || resp.Data.OAuthCredentialInfo.ChatGPTAccountID != "account-editor" ||
		resp.Data.OAuthCredentialInfo.ChatGPTSubscriptionActiveStart != codexTestSubscriptionActiveStart ||
		resp.Data.OAuthCredentialInfo.ChatGPTSubscriptionActiveUntil != codexTestSubscriptionActiveUntil ||
		resp.Data.OAuthCredentialInfo.PlanType != "plus" {
		t.Fatalf("editor decoded credential info = %#v", resp.Data.OAuthCredentialInfo)
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
	if persisted.Name != "codex-renamed" || persisted.OAuthCredential != channel.OAuthCredential {
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

func TestOAuthCredentialRefreshIsSingleflightAndPersistsToDatabase(t *testing.T) {
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
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
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
		OAuthCredential     codexauth.Credential   `json:"oauth_credential"`
		OAuthCredentialInfo *codexauth.IDTokenInfo `json:"oauth_credential_info"`
		CodexPlanType       string                 `json:"codex_plan_type"`
	}](t, w.Body.Bytes())
	if resp.Data.OAuthCredential.AccessToken != "at-manual-new" ||
		resp.Data.OAuthCredential.RefreshToken != "rt-manual-new" ||
		resp.Data.OAuthCredential.IDToken != idToken || resp.Data.CodexPlanType != "team" {
		t.Fatalf("refresh response credential = %#v", resp.Data)
	}
	if resp.Data.OAuthCredentialInfo == nil || resp.Data.OAuthCredentialInfo.ChatGPTAccountID != "account-manual-refresh" ||
		resp.Data.OAuthCredentialInfo.ChatGPTSubscriptionActiveStart != codexTestSubscriptionActiveStart ||
		resp.Data.OAuthCredentialInfo.ChatGPTSubscriptionActiveUntil != codexTestSubscriptionActiveUntil ||
		resp.Data.OAuthCredentialInfo.PlanType != "team" {
		t.Fatalf("refresh response decoded info = %#v", resp.Data.OAuthCredentialInfo)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.AccessToken != "at-manual-new" || persistedCredential.IDToken != idToken {
		t.Fatalf("persisted credential = (%#v, %v)", persistedCredential, err)
	}
}

func TestHandleOAuthUsageReturnsCodexQuotaWithoutLeakingCredential(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &codexauth.Credential{
		Type: "codex", AccessToken: "at-quota-secret", RefreshToken: "rt-quota-secret",
		Expired:   time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		AccountID: "account-quota", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}

	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != codexUsageURL {
			t.Errorf("usage request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer at-quota-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Chatgpt-Account-Id"); got != "account-quota" {
			t.Errorf("Chatgpt-Account-Id = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != codexUsageUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		body := `{
			"plan_type":"pro",
			"rate_limit":{"primary_window":{"used_percent":29,"limit_window_seconds":604800,"reset_at":1786163635}},
			"additional_rate_limits":[{
				"limit_name":"codex-spark",
				"rate_limit":{
					"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1786000000},
					"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1786500000}
				}
			}]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	server.codexCredentials = newCodexCredentialManager(
		codexauth.NewService(server.client), store,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)

	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "at-quota-secret") || strings.Contains(w.Body.String(), "rt-quota-secret") {
		t.Fatalf("usage response leaked credential: %s", w.Body.String())
	}
	response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
	if response.Data.Provider != codexauth.ChannelType || response.Data.PlanType != "pro" || len(response.Data.Windows) != 3 {
		t.Fatalf("usage summary = %#v", response.Data)
	}
	windows := response.Data.Windows
	if windows[0].LimitName != "codex" || windows[0].Kind != "primary" || windows[0].UsedPercent != 29 || windows[0].RemainingPercent != 71 {
		t.Fatalf("primary window = %#v", windows[0])
	}
	if windows[1].LimitName != "codex-spark" || windows[1].Kind != "primary" || windows[1].RemainingPercent != 90 {
		t.Fatalf("additional primary window = %#v", windows[1])
	}
	if windows[2].LimitName != "codex-spark" || windows[2].Kind != "secondary" || windows[2].RemainingPercent != 0 {
		t.Fatalf("additional secondary window = %#v", windows[2])
	}
}

func TestHandleOAuthUsageReturnsAntigravityQuotaWithoutLeakingCredential(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-gravity-quota-secret", RefreshToken: "rt-gravity-quota-secret",
		Expired: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), ProjectID: "forward-bonus-fjkxm",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Antigravity credential JSON: %v", err)
	}
	channel, err := store.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity quota", payload))
	if err != nil {
		t.Fatalf("create Antigravity channel: %v", err)
	}

	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != antigravityUsageURL {
			t.Errorf("usage request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer at-gravity-quota-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != antigravityUsageUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		var body struct {
			Project string `json:"project"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode Antigravity usage request: %v", err)
		}
		if body.Project != "forward-bonus-fjkxm" {
			t.Errorf("project = %q", body.Project)
		}
		responseBody := `{
			"groups":[
				{"displayName":"Gemini Models","buckets":[
					{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-08-13T08:24:21Z","remainingFraction":1},
					{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-08-06T17:07:55Z","remainingFraction":0.75}
				]},
				{"displayName":"Claude and GPT models","buckets":[
					{"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-08-13T08:28:21Z","remainingFraction":0.9}
				]}
			]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    request,
		}, nil
	})}
	server.antigravityService = antigravityauth.NewService(server.client)
	server.antigravityCredentials = newAntigravityCredentialManager(
		server.antigravityService, store,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)

	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "at-gravity-quota-secret") || strings.Contains(w.Body.String(), "rt-gravity-quota-secret") {
		t.Fatalf("usage response leaked credential: %s", w.Body.String())
	}
	response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
	if response.Data.Provider != antigravityauth.ChannelType || response.Data.PlanType != "" || len(response.Data.Windows) != 3 {
		t.Fatalf("usage summary = %#v", response.Data)
	}
	windows := response.Data.Windows
	if windows[0].LimitName != "Gemini Models" || windows[0].Kind != "gemini-weekly" || windows[0].RemainingPercent != 100 || windows[0].UsedPercent != 0 || windows[0].LimitWindowSeconds != weeklyUsageWindowSeconds || windows[0].ResetAt != 1786609461 {
		t.Fatalf("Gemini weekly window = %#v", windows[0])
	}
	if windows[1].Kind != "gemini-5h" || windows[1].RemainingPercent != 75 || windows[1].UsedPercent != 25 || windows[1].LimitWindowSeconds != 5*60*60 || windows[1].ResetAt != 1786036075 {
		t.Fatalf("Gemini five-hour window = %#v", windows[1])
	}
	if windows[2].LimitName != "Claude and GPT models" || windows[2].Kind != "3p-weekly" || windows[2].RemainingPercent != 90 {
		t.Fatalf("third-party weekly window = %#v", windows[2])
	}
}

func TestHandleOAuthUsageHidesUpstreamErrorBody(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, &codexauth.Credential{
		Type: "codex", AccessToken: "at-safe", RefreshToken: "rt-safe",
		Expired: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), AccountID: "account-safe",
	})
	if err != nil {
		t.Fatalf("createOrUpdateCodexChannel() error = %v", err)
	}
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"upstream-secret","error":"expired"}`)),
			Request:    request,
		}, nil
	})}
	server.codexCredentials = newCodexCredentialManager(
		codexauth.NewService(server.client), store,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)

	c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/1/oauth-usage", nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "upstream-secret") || strings.Contains(w.Body.String(), "at-safe") {
		t.Fatalf("usage error leaked sensitive content: %s", w.Body.String())
	}
}

func TestHandleOAuthUsageRejectsUnsupportedChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	channel, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "API key channel", AuthType: model.AuthTypeAPIKey, Enabled: true,
		URLs: model.ChannelURLs{{URL: "https://api.example.test"}},
	})
	if err != nil {
		t.Fatalf("create API key channel: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/1/oauth-usage", nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
}
