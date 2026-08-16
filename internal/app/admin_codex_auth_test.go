package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	sqlstore "ccLoad/internal/storage/sql"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"

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

type concurrentOAuthWinnerStore struct {
	storage.Store
	once       sync.Once
	authType   string
	winnerJSON string
	winnerErr  error
}

type transientOAuthCASStore struct {
	storage.Store
	calls atomic.Int32
}

type transientOAuthGetStore struct {
	storage.Store
	getCalls atomic.Int32
	casCalls atomic.Int32
}

type blockingCodexModelStateStore struct {
	storage.Store
	firstStarted chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

type snapshotBarrierStore struct {
	storage.Store
	calls   atomic.Int32
	ready   chan struct{}
	release chan struct{}
}

type anthropicMetadataChurnStore struct {
	storage.Store
	remaining atomic.Int32
	sequence  atomic.Int32
}

func (s *anthropicMetadataChurnStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	if s.remaining.Add(-1) >= 0 {
		winner, err := anthropicauth.ParseCredential([]byte(expectedCredential))
		if err != nil {
			return false, err
		}
		winner.PlanType = fmt.Sprintf("Concurrent %d", s.sequence.Add(1))
		winnerJSON, err := winner.JSON()
		if err != nil {
			return false, err
		}
		updated, err := s.Store.CompareAndSwapOAuthCredential(
			ctx, channelID, expectedAuthType, expectedCredential, winnerJSON,
		)
		if err != nil {
			return false, err
		}
		if !updated {
			return false, errors.New("inject Anthropic metadata winner: compare and swap missed")
		}
		return false, nil
	}
	return s.Store.CompareAndSwapOAuthCredential(
		ctx, channelID, expectedAuthType, expectedCredential, nextCredential,
	)
}

func (s *snapshotBarrierStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	configs, err := s.Store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}
	call := s.calls.Add(1)
	if call <= 2 {
		if call == 2 {
			close(s.ready)
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return configs, nil
}

func (s *blockingCodexModelStateStore) UpdateOAuthModelStateIfCredentialMatches(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential string,
	modelEntries []model.ModelEntry,
	scheduledCheckModel string,
) (bool, error) {
	if s.calls.Add(1) == 1 {
		close(s.firstStarted)
		select {
		case <-s.releaseFirst:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.Store.UpdateOAuthModelStateIfCredentialMatches(
		ctx, channelID, expectedAuthType, expectedCredential, modelEntries, scheduledCheckModel,
	)
}

func (s *concurrentOAuthWinnerStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	injected := false
	s.once.Do(func() {
		injected = true
		updated, err := s.Store.CompareAndSwapOAuthCredential(
			ctx, channelID, s.authType, expectedCredential, s.winnerJSON,
		)
		if err != nil {
			s.winnerErr = err
		} else if !updated {
			s.winnerErr = fmt.Errorf("inject concurrent OAuth winner: compare and swap missed")
		}
	})
	if s.winnerErr != nil {
		return false, s.winnerErr
	}
	if injected {
		return false, nil
	}
	return s.Store.CompareAndSwapOAuthCredential(
		ctx, channelID, expectedAuthType, expectedCredential, nextCredential,
	)
}

func (s *transientOAuthCASStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	if s.calls.Add(1) == 1 {
		return false, errors.New("database is locked")
	}
	return s.Store.CompareAndSwapOAuthCredential(
		ctx, channelID, expectedAuthType, expectedCredential, nextCredential,
	)
}

func (s *transientOAuthGetStore) GetConfig(ctx context.Context, channelID int64) (*model.Config, error) {
	if s.getCalls.Add(1) == 1 {
		return nil, errors.New("database is locked")
	}
	return s.Store.GetConfig(ctx, channelID)
}

func (s *transientOAuthGetStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	s.casCalls.Add(1)
	return s.Store.CompareAndSwapOAuthCredential(
		ctx, channelID, expectedAuthType, expectedCredential, nextCredential,
	)
}

func codexTestIDToken(t *testing.T, email, accountID string) string {
	return codexTestIDTokenForPlan(t, email, accountID, "plus")
}

func codexTestIDTokenForPlan(t *testing.T, email, accountID, planType string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":                   "user-" + email,
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

func newAntigravityPaidTierTestService(t *testing.T) *antigravityauth.Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if r.Form.Get("grant_type") != "refresh_token" {
				t.Fatalf("token grant = %q", r.Form.Get("grant_type"))
			}
			if r.Form.Get("refresh_token") == "rt-unusable-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"at-refreshed-secret","refresh_token":"rt-rotated-secret","expires_in":3600}`)
		case "/v1internal:loadCodeAssist":
			if r.Header.Get("Authorization") == "Bearer at-must-not-overwrite" {
				http.Error(w, "duplicate credentials must not be validated", http.StatusInternalServerError)
				return
			}
			if r.Header.Get("Authorization") == "Bearer at-unusable-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	service := antigravityauth.NewService(server.Client())
	service.TokenURL = server.URL + "/token"
	service.DailyAPIBaseURL = server.URL
	return service
}

func newAcceptedCodexImportClient() *http.Client {
	return &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.String() == codexUsageURL &&
			request.Header.Get("Authorization") == "Bearer at-must-not-overwrite":
			return nil, fmt.Errorf("duplicate credentials must not be validated")
		case request.Method == http.MethodGet && request.URL.String() == codexUsageURL:
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    request,
			}, nil
		case request.Method == http.MethodPost && request.URL.String() == codexauth.DefaultTokenURL:
			if err := request.ParseForm(); err != nil {
				return nil, fmt.Errorf("parse Codex refresh request: %w", err)
			}
			if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") == "" {
				return nil, fmt.Errorf("invalid Codex refresh request")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-refreshed-import-test","expires_in":604800}`)),
				Request:    request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected Codex import validation request: %s %s", request.Method, request.URL.Host)
		}
	})}
}

func xaiTestCredential(accessToken, refreshToken string, expiresAt time.Time) *xaiauth.Credential {
	return &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: accessToken, RefreshToken: refreshToken,
		Expired: expiresAt.UTC().Format(time.RFC3339), ClientID: xaiauth.ClientID, TokenEndpoint: xaiauth.TokenURL,
	}
}

func TestCompleteXAICredentialProbesBillingWithoutRefreshingFreshToken(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.String() != xaiauth.CLIBaseURL+"/billing" {
			return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer fresh-access" {
			return nil, fmt.Errorf("unexpected authorization")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"subscription_tier":"pro","entitlement_status":"active"}`)), Request: request}, nil
	})}

	got, err := completeXAICredential(context.Background(), xaiauth.NewService(client), client, xaiTestCredential("fresh-access", "refresh-secret", time.Now().Add(time.Hour)), xaiauth.CLIBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "fresh-access" || got.SubscriptionTier != "pro" || got.EntitlementStatus != "active" || requests.Load() != 1 {
		t.Fatalf("completion = %s, requests=%d", got, requests.Load())
	}
}

func TestCompleteXAICredentialRefreshesBadCredentialOnlyOnce(t *testing.T) {
	t.Parallel()
	var probes atomic.Int32
	var refreshes atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodGet:
			probe := probes.Add(1)
			status := http.StatusUnauthorized
			body := `{}`
			if probe == 2 && request.Header.Get("Authorization") == "Bearer rotated-access" {
				status = http.StatusOK
				body = `{"subscription_tier":"premium"}`
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		case http.MethodPost:
			refreshes.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)), Request: request}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})}

	got, err := completeXAICredential(context.Background(), xaiauth.NewService(client), client, xaiTestCredential("rejected-access", "refresh-secret", time.Now().Add(time.Hour)), xaiauth.CLIBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || probes.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("completion = %s, probes=%d refreshes=%d", got, probes.Load(), refreshes.Load())
	}
}

func TestCompleteXAICredentialRejectsIndeterminateBillingWithoutRefresh(t *testing.T) {
	t.Parallel()
	secret := "body-must-not-leak"
	var refreshes atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			refreshes.Add(1)
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"` + secret + `"}`)), Request: request}, nil
	})}

	_, err := completeXAICredential(context.Background(), xaiauth.NewService(client), client, xaiTestCredential("fresh-access", "refresh-secret", time.Now().Add(time.Hour)), xaiauth.CLIBaseURL)
	if err == nil || strings.Contains(err.Error(), secret) || refreshes.Load() != 0 {
		t.Fatalf("unsafe completion error=%v refreshes=%d", err, refreshes.Load())
	}
}

func TestHandleImportOAuthCredentialsDetectsXAIAndExpandsCredentialsMap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != xaiauth.CLIBaseURL+"/billing" {
			return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"entitlement_status":"active"}`)), Request: request}, nil
	})}
	server := &Server{store: store, client: client}
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	container := fmt.Sprintf(`{"credentials":{"map-key-secret-b":{"type":"xai","access_token":"access-secret-b","refresh_token":"refresh-secret-b","email":"b@example.com","expired":%q},"map-key-secret-a":{"client_id":%q,"access_token":"access-secret-a","refresh_token":"refresh-secret-a","email":"a@example.com","expired":%q}}}`, expiresAt, xaiauth.ClientID, expiresAt)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "xai.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, container); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("priority_increment", "10"); err != nil {
		t.Fatal(err)
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
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 2 || result.Data.Skipped != 0 || result.Data.Failed != 0 {
		t.Fatalf("import summary = %#v", result.Data)
	}
	for _, secret := range []string{"map-key-secret", "access-secret", "refresh-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("response leaked %q: %s", secret, response.Body.String())
		}
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 2 {
		t.Fatalf("channels=%d error=%v", len(channels), err)
	}
	wantPriority := map[string]int{"xAI-a@example.com": 10, "xAI-b@example.com": 20}
	for _, channel := range channels {
		if !channel.UsesXAIOAuth() || channel.Priority != wantPriority[channel.Name] || len(channel.ModelEntries) != len(xaiOAuthDefaultModels) {
			t.Fatalf("unexpected xAI channel: %#v", channel)
		}
	}
}

func TestXAIOAuthHandlersGenerateLocallyAndExchangeManualCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminA, adminB := "admin-a-bearer", "admin-b-bearer"
	auth := newTestAuthService(t)
	injectAdminToken(auth, adminA, time.Now().Add(time.Hour))
	injectAdminToken(auth, adminB, time.Now().Add(time.Hour))
	var requests atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.URL.String() != xaiauth.TokenURL {
			return nil, fmt.Errorf("unexpected request during xAI OAuth: %s", request.URL)
		}
		if err := request.ParseForm(); err != nil {
			return nil, err
		}
		if request.Form.Get("grant_type") != "authorization_code" ||
			request.Form.Get("code") != "manual-code" || request.Form.Get("code_verifier") == "" ||
			request.Form.Get("redirect_uri") != xaiauth.RedirectURI {
			return nil, fmt.Errorf("unexpected token form: %s", request.Form.Encode())
		}
		body := `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	manager := newXAIOAuthManager(context.Background(), xaiauth.NewService(client),
		func(_ context.Context, credential *xaiauth.Credential) (*xaiauth.Credential, error) {
			return credential, nil
		},
		func(context.Context, *xaiauth.Credential) (int64, error) { return 99, nil },
	)
	t.Cleanup(manager.close)
	server := &Server{xaiOAuth: manager}
	engine := gin.New()
	engine.POST("/admin/xai/oauth/start", auth.RequireAdminAuth(), server.HandleStartXAIOAuth)
	engine.GET("/admin/xai/oauth/status", auth.RequireAdminAuth(), server.HandleXAIOAuthStatus)
	engine.POST("/admin/xai/oauth/cancel", auth.RequireAdminAuth(), server.HandleCancelXAIOAuth)
	engine.POST("/admin/xai/oauth/callback", auth.RequireAdminAuth(), server.HandleSubmitXAIOAuthCallback)
	do := func(method, target, bearer string, body any) *httptest.ResponseRecorder {
		t.Helper()
		request := newJSONRequest(t, method, target, body)
		request.Header.Set("Authorization", "Bearer "+bearer)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}

	startResponse := do(http.MethodPost, "/admin/xai/oauth/start", adminA, nil)
	if startResponse.Code != http.StatusOK || requests.Load() != 0 {
		t.Fatalf("start status=%d requests=%d body=%s", startResponse.Code, requests.Load(), startResponse.Body.String())
	}
	started := mustParseAPIResponse[xaiOAuthStartResponse](t, startResponse.Body.Bytes()).Data
	parsed, err := url.Parse(started.URL)
	if err != nil || started.State == "" || parsed.Query().Get("state") != started.State ||
		parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("redirect_uri") != xaiauth.RedirectURI {
		t.Fatalf("invalid local authorization response: %#v", started)
	}
	if crossAdmin := do(http.MethodGet, "/admin/xai/oauth/status?state="+url.QueryEscape(started.State), adminB, nil); crossAdmin.Code != http.StatusNotFound {
		t.Fatalf("cross-admin status=%d body=%s", crossAdmin.Code, crossAdmin.Body.String())
	}
	callbackURL := xaiauth.RedirectURI + "?code=manual-code&state=" + url.QueryEscape(started.State)
	if crossAdmin := do(http.MethodPost, "/admin/xai/oauth/callback", adminB, map[string]string{"callback_url": callbackURL}); crossAdmin.Code != http.StatusBadRequest {
		t.Fatalf("cross-admin callback=%d body=%s", crossAdmin.Code, crossAdmin.Body.String())
	}
	callback := do(http.MethodPost, "/admin/xai/oauth/callback", adminA, map[string]string{"callback_url": callbackURL})
	if callback.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", callback.Code, callback.Body.String())
	}
	completed := mustParseAPIResponse[xaiOAuthStatusResponse](t, callback.Body.Bytes()).Data
	if completed.Status != "complete" || completed.ChannelID != 99 || completed.State != started.State || requests.Load() != 1 {
		t.Fatalf("callback result=%#v requests=%d", completed, requests.Load())
	}
	statusResponse := do(http.MethodGet, "/admin/xai/oauth/status?state="+url.QueryEscape(started.State), adminA, nil)
	status := mustParseAPIResponse[xaiOAuthStatusResponse](t, statusResponse.Body.Bytes()).Data
	if statusResponse.Code != http.StatusOK || status.Status != "complete" || status.ChannelID != 99 {
		t.Fatalf("status=%d %#v", statusResponse.Code, status)
	}
}

func TestXAIOAuthAcceptsBareCodeForCurrentAdminSession(t *testing.T) {
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil || request.Form.Get("code") != "bare-code" {
			return nil, fmt.Errorf("unexpected bare-code request: %v %s", err, request.Form.Encode())
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)),
			Request: request,
		}, nil
	})}
	manager := newXAIOAuthManager(context.Background(), xaiauth.NewService(client),
		func(_ context.Context, credential *xaiauth.Credential) (*xaiauth.Credential, error) {
			return credential, nil
		},
		func(context.Context, *xaiauth.Credential) (int64, error) { return 7, nil },
	)
	t.Cleanup(manager.close)
	started, err := manager.start("admin-session")
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.submitCallback("admin-session", " bare-code ")
	if err != nil || status.State != started.State || status.Status != "complete" || status.ChannelID != 7 {
		t.Fatalf("bare callback = (%#v, %v)", status, err)
	}
}

func TestXAIOAuthCancellationRespectsCommitBoundary(t *testing.T) {
	newService := func() *xaiauth.Service {
		return xaiauth.NewService(&http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body:    io.NopCloser(strings.NewReader(`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)),
				Request: request,
			}, nil
		})})
	}

	t.Run("cancel while completing prevents commit", func(t *testing.T) {
		completeStarted := make(chan struct{})
		var commits atomic.Int32
		manager := newXAIOAuthManager(context.Background(), newService(),
			func(ctx context.Context, _ *xaiauth.Credential) (*xaiauth.Credential, error) {
				close(completeStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			},
			func(context.Context, *xaiauth.Credential) (int64, error) {
				commits.Add(1)
				return 0, nil
			},
		)
		defer manager.close()
		started, err := manager.start("admin-session")
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, callbackErr := manager.submitCallback("admin-session", "code=manual-code&state="+url.QueryEscape(started.State))
			result <- callbackErr
		}()
		<-completeStarted
		if err := manager.cancel("admin-session", started.State); err != nil {
			t.Fatalf("cancel() error = %v", err)
		}
		if err := <-result; err == nil {
			t.Fatal("submitCallback() error = nil, want cancellation")
		}
		status, ok := manager.status("admin-session", started.State)
		if !ok || status.Status != "cancelled" || commits.Load() != 0 {
			t.Fatalf("status = (%#v, %v), commits = %d", status, ok, commits.Load())
		}
	})

	t.Run("cancel cannot interrupt commit", func(t *testing.T) {
		commitStarted := make(chan struct{})
		releaseCommit := make(chan struct{})
		now := time.Now()
		manager := newXAIOAuthManager(context.Background(), newService(),
			func(_ context.Context, credential *xaiauth.Credential) (*xaiauth.Credential, error) {
				return credential, nil
			},
			func(context.Context, *xaiauth.Credential) (int64, error) {
				close(commitStarted)
				<-releaseCommit
				return 42, nil
			},
		)
		defer manager.close()
		manager.now = func() time.Time { return now }
		started, err := manager.start("admin-session")
		if err != nil {
			t.Fatal(err)
		}
		type callbackResult struct {
			status xaiOAuthStatusResponse
			err    error
		}
		result := make(chan callbackResult, 1)
		go func() {
			status, callbackErr := manager.submitCallback("admin-session", "code=manual-code&state="+url.QueryEscape(started.State))
			result <- callbackResult{status: status, err: callbackErr}
		}()
		<-commitStarted
		now = now.Add(xaiOAuthSessionTTL + time.Second)
		status, ok := manager.status("admin-session", started.State)
		if !ok || status.Status != "committing" {
			t.Fatalf("expired commit status = (%#v, %v), want committing", status, ok)
		}
		if err := manager.cancel("admin-session", started.State); err == nil || !strings.Contains(err.Error(), "committing") {
			t.Fatalf("cancel() error = %v, want committing rejection", err)
		}
		close(releaseCommit)
		completed := <-result
		if completed.err != nil || completed.status.Status != "complete" || completed.status.ChannelID != 42 {
			t.Fatalf("submitCallback() = (%#v, %v)", completed.status, completed.err)
		}
	})
}

func xaiTestJWT(email, subject string) string {
	payload, _ := json.Marshal(map[string]string{"email": email, "sub": subject})
	return "x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
}

func startXAICredentialImportTestJob(
	t *testing.T,
	server *Server,
	method string,
	values []string,
	priorityIncrement int,
	requestCtx context.Context,
) (oauthCredentialImportJobStart, string) {
	t.Helper()
	request := newJSONRequest(t, http.MethodPost, "/admin/xai/credentials/import/jobs", map[string]any{
		"method": method, "values": strings.Join(values, "\n"), "priority_increment": priorityIncrement,
	})
	if requestCtx != nil {
		request = request.WithContext(requestCtx)
	}
	requestContext, response := newTestContext(t, request)
	server.HandleStartXAICredentialImportJob(requestContext)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start xAI import status=%d body=%s", response.Code, response.Body.String())
	}
	started := mustParseAPIResponse[oauthCredentialImportJobStart](t, response.Body.Bytes()).Data
	if started.JobID == "" || started.Total != len(values) {
		t.Fatalf("start xAI import=%#v", started)
	}
	return started, response.Body.String()
}

func waitXAICredentialImportTestJob(
	t *testing.T,
	server *Server,
	jobID string,
) (oauthCredentialImportJobView, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		request := httptest.NewRequest(http.MethodGet, "/admin/oauth/credentials/import/jobs/"+jobID+"?after=0", nil)
		requestContext, response := newTestContext(t, request)
		requestContext.Params = gin.Params{{Key: "id", Value: jobID}}
		server.HandleOAuthCredentialImportJob(requestContext)
		if response.Code != http.StatusOK {
			t.Fatalf("xAI import job status=%d body=%s", response.Code, response.Body.String())
		}
		view := mustParseAPIResponse[oauthCredentialImportJobView](t, response.Body.Bytes()).Data
		if view.Status == oauthCredentialImportJobSucceeded {
			return view, response.Body.String()
		}
		if view.Status != oauthCredentialImportJobRunning {
			t.Fatalf("xAI import job stopped: %#v", view)
		}
		if time.Now().After(deadline) {
			t.Fatalf("xAI import job did not complete: %#v", view)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestXAIRefreshTokenImportAcceptsMoreThanHundredWithBoundedConcurrencyAndRedactsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	var active atomic.Int32
	var maximum atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodPost:
			current := active.Add(1)
			defer active.Add(-1)
			for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
			}
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			refreshToken := request.Form.Get("refresh_token")
			time.Sleep(25 * time.Millisecond)
			index := strings.TrimPrefix(refreshToken, "refresh-secret-")
			body := fmt.Sprintf(`{"access_token":"access-%s","refresh_token":"rotated-%s","id_token":%q,"expires_in":3600}`, index, index, xaiTestJWT("user-"+index+"@example.com", "subject-"+index))
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		case http.MethodGet:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"entitlement_status":"active"}`)), Request: request}, nil
		default:
			return nil, fmt.Errorf("unexpected method: %s", request.Method)
		}
	})}
	server := &Server{store: store, client: client}
	values := make([]string, 101)
	for i := range values {
		values[i] = fmt.Sprintf("refresh-secret-%d", i+1)
	}
	started, startBody := startXAICredentialImportTestJob(t, server, "refresh_token", values, 10, nil)
	view, statusBody := waitXAICredentialImportTestJob(t, server, started.JobID)
	if view.Created != len(values) || view.Processed != len(values) || view.Failed != 0 {
		t.Fatalf("completed refresh import=%#v", view)
	}
	if maximum.Load() < 2 || maximum.Load() > 5 {
		t.Fatalf("maximum refresh concurrency = %d, want 2..5", maximum.Load())
	}
	for _, secret := range values {
		if strings.Contains(startBody, secret) || strings.Contains(statusBody, secret) {
			t.Fatalf("job response leaked refresh token %q", secret)
		}
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != len(values) {
		t.Fatalf("created channels=%d error=%v", len(channels), err)
	}
	for _, channel := range channels {
		var index int
		if _, err := fmt.Sscanf(channel.Name, "xAI-user-%d@example.com", &index); err != nil || channel.Priority != index*10 {
			t.Fatalf("channel %q priority=%d", channel.Name, channel.Priority)
		}
	}
}

func TestXAIOAuthInteractivePersistenceUpdatesStableIdentity(t *testing.T) {
	store := newCodexAuthTestStore(t)
	first := xaiTestCredential("access-first", "refresh-first", time.Now().Add(time.Hour))
	first.IDToken = xaiTestJWT("first@example.com", "stable-subject")
	if err := first.Normalize(); err != nil {
		t.Fatal(err)
	}
	created, wasCreated, err := createOrUpdateXAIChannel(context.Background(), store, first)
	if err != nil || !wasCreated {
		t.Fatalf("first persistence = (%#v, %v, %v)", created, wasCreated, err)
	}

	rotated := xaiTestCredential("access-rotated", "refresh-rotated", time.Now().Add(2*time.Hour))
	rotated.IDToken = xaiTestJWT("renamed@example.com", "stable-subject")
	if err := rotated.Normalize(); err != nil {
		t.Fatal(err)
	}
	updated, wasCreated, err := createOrUpdateXAIChannel(context.Background(), store, rotated)
	if err != nil || wasCreated || updated.ID != created.ID {
		t.Fatalf("second persistence = (%#v, %v, %v)", updated, wasCreated, err)
	}
	persisted, err := xaiauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || persisted.AccessToken != "access-rotated" || persisted.RefreshToken != "refresh-rotated" {
		t.Fatalf("persisted credential = %s, error=%v", persisted, err)
	}
}

func TestXAIOAuthConcurrentInteractivePersistenceCreatesOneStableIdentity(t *testing.T) {
	baseStore := newCodexAuthTestStore(t)
	store := &snapshotBarrierStore{Store: baseStore, ready: make(chan struct{}), release: make(chan struct{})}
	credential := xaiTestCredential("access", "refresh", time.Now().Add(time.Hour))
	credential.IDToken = xaiTestJWT("same@example.com", "same-subject")
	if err := credential.Normalize(); err != nil {
		t.Fatal(err)
	}
	type persistenceResult struct {
		channel *model.Config
		err     error
	}
	results := make(chan persistenceResult, 2)
	for range 2 {
		go func() {
			channel, _, err := createOrUpdateXAIChannel(context.Background(), store, credential)
			results <- persistenceResult{channel: channel, err: err}
		}()
	}
	select {
	case <-store.ready:
	case <-time.After(time.Second):
		t.Fatal("concurrent persistence did not reach shared snapshot")
	}
	close(store.release)
	for range 2 {
		result := <-results
		if result.err != nil || result.channel == nil {
			t.Fatalf("persistence result = %#v", result)
		}
	}
	configs, err := baseStore.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("stable identity channels=%d error=%v", len(configs), err)
	}
}

func TestXAIFilePersistenceIsCreateOnlyAndCaseInsensitive(t *testing.T) {
	store := newCodexAuthTestStore(t)
	first := xaiTestCredential("access-first", "refresh-first", time.Now().Add(time.Hour))
	first.Email = "User@Example.com"
	if err := first.Normalize(); err != nil {
		t.Fatal(err)
	}
	name, created, err := createImportedXAIChannel(context.Background(), store, first, 10)
	if err != nil || !created || name != "xAI-User@Example.com" {
		t.Fatalf("first import = (%q, %v, %v)", name, created, err)
	}
	second := xaiTestCredential("access-second", "refresh-second", time.Now().Add(time.Hour))
	second.Email = "user@example.com"
	if err := second.Normalize(); err != nil {
		t.Fatal(err)
	}
	name, created, err = createImportedXAIChannel(context.Background(), store, second, 20)
	if err != nil || created || name != "xAI-User@Example.com" {
		t.Fatalf("duplicate import = (%q, %v, %v)", name, created, err)
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || strings.Contains(configs[0].OAuthCredential, "access-second") {
		t.Fatalf("create-only configs=%#v error=%v", configs, err)
	}
}

func TestXAIRefreshTokenImportJobSurvivesStartRequestCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodPost:
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			index := strings.TrimPrefix(request.Form.Get("refresh_token"), "refresh-secret-")
			body := fmt.Sprintf(
				`{"access_token":"access-%s","refresh_token":"rotated-%s","id_token":%q,"expires_in":3600}`,
				index, index, xaiTestJWT("survivor-"+index+"@example.com", "subject-"+index),
			)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		case http.MethodGet:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"entitlement_status":"active"}`)), Request: request}, nil
		default:
			return nil, fmt.Errorf("unexpected method: %s", request.Method)
		}
	})}
	server := &Server{store: store, client: client}
	values := []string{"refresh-secret-1", "refresh-secret-2", "refresh-secret-3"}
	requestCtx, cancel := context.WithCancel(context.Background())
	request := newJSONRequest(t, http.MethodPost, "/admin/xai/credentials/import/stream", map[string]any{
		"method": "refresh_token", "values": strings.Join(values, "\n"),
	}).WithContext(requestCtx)
	requestContext, response := newTestContext(t, request)
	handlerDone := make(chan struct{})
	go func() {
		server.HandleImportXAICredentialsStream(requestContext)
		close(handlerDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh import did not start")
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SSE observer did not stop after request cancellation")
	}
	var startEvent oauthCredentialImportEvent
	for line := range strings.SplitSeq(response.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &startEvent); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if startEvent.JobID == "" || startEvent.Total != len(values) {
		t.Fatalf("SSE start event=%#v body=%s", startEvent, response.Body.String())
	}
	close(release)
	view, statusBody := waitXAICredentialImportTestJob(t, server, startEvent.JobID)
	if view.Created != len(values) || view.Processed != len(values) || view.Failed != 0 {
		t.Fatalf("completed import after request cancellation=%#v", view)
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != len(values) || strings.Contains(response.Body.String()+statusBody, "refresh-secret") {
		t.Fatalf("SSE cancellation stopped or leaked import: configs=%d error=%v stream=%s status=%s", len(configs), err, response.Body.String(), statusBody)
	}
}

func TestXAISSOImportAcceptsMoreThanTenItems(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	var upstreamCalls atomic.Int32
	server := &Server{store: store, client: &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil
	})}}
	values := make([]string, 11)
	for i := range values {
		values[i] = fmt.Sprintf("sso-secret-%d", i)
	}
	started, _ := startXAICredentialImportTestJob(t, server, "sso", values, 0, nil)
	view, _ := waitXAICredentialImportTestJob(t, server, started.JobID)
	if view.Failed != len(values) || upstreamCalls.Load() != int32(len(values)) {
		t.Fatalf("completed import=%#v upstream=%d", view, upstreamCalls.Load())
	}
}

func TestXAISSOImportReportsErrorsWithBoundedConcurrencyAndRedactsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	var active atomic.Int32
	var maximum atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != xaiauth.SSOAccountsURL {
			return nil, fmt.Errorf("unexpected SSO URL: %s", request.URL)
		}
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		time.Sleep(25 * time.Millisecond)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
	})}
	server := &Server{store: store, client: client}
	values := []string{"sso-secret-1", "sso-secret-2", "sso-secret-3", "sso-secret-4"}
	started, startBody := startXAICredentialImportTestJob(t, server, "sso", values, 0, nil)
	view, statusBody := waitXAICredentialImportTestJob(t, server, started.JobID)
	if view.Failed != len(values) || maximum.Load() < 2 || maximum.Load() > 3 {
		t.Fatalf("completed import=%#v maximum concurrency=%d", view, maximum.Load())
	}
	if !strings.Contains(statusBody, xaiauth.ErrSSOUnauthorized.Error()) {
		t.Fatalf("job response omitted SSO failure detail: %s", statusBody)
	}
	for _, secret := range values {
		if strings.Contains(startBody, secret) || strings.Contains(statusBody, secret) {
			t.Fatalf("job response leaked SSO cookie %q", secret)
		}
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("failed SSO import persisted channels=%d error=%v", len(configs), err)
	}
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
	if channel.Name != "Codex-user@example.com" || !channel.UsesCodexOAuth() || !channel.Websockets || channel.KeyCount != 0 ||
		!channel.SupportsModel("gpt-5.4") || !channel.SupportsModel("gpt-image-1.5") || !channel.SupportsModel("gpt-image-2") {
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
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"gravity-project","paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`)
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
	service.DailyAPIBaseURL = oauthServer.URL
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
	if channel.Name != "Antigravity-gravity@example.com" || !channel.UsesAntigravityOAuth() || channel.KeyCount != 0 ||
		channel.Websockets || channel.GetProtocolTransformMode() != model.ProtocolTransformModeLocal ||
		channel.MaxConcurrency != antigravityOAuthMaxConcurrency {
		t.Fatalf("created Antigravity channel = %#v", channel)
	}
	wantURLs := []string{antigravityDailyBaseURL}
	if len(channel.URLs) != len(wantURLs) || !channel.SupportsModel("gemini-3-flash") ||
		!channel.SupportsModel("gemini-3.7-flash-high") ||
		!strings.Contains(channel.OAuthCredential, `"project_id":"gravity-project"`) ||
		!strings.Contains(channel.OAuthCredential, `"paid_tier":{"id":"g1-pro-tier","name":"Google AI Pro"}`) {
		t.Fatalf("created Antigravity channel contract = %#v", channel)
	}
	for i, wantURL := range wantURLs {
		if channel.URLs[i].URL != wantURL || !channel.URLs[i].SupportsProtocol(util.ProtocolGemini) {
			t.Fatalf("Antigravity URL[%d] = %#v, want Gemini %s", i, channel.URLs[i], wantURL)
		}
	}
}

func TestCreateAntigravityChannelUpdatesExistingConcurrency(t *testing.T) {
	store := newCodexAuthTestStore(t)
	existingCredential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "old-at", RefreshToken: "old-rt",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "existing@example.com", ProjectID: "old-project",
	}
	existingPayload, err := existingCredential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	existingConfig := newAntigravityOAuthChannel("Antigravity-existing@example.com", existingPayload)
	existingConfig.MaxConcurrency = 0
	existing, err := store.CreateConfig(context.Background(), existingConfig)
	if err != nil {
		t.Fatal(err)
	}

	updatedCredential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "new-at", RefreshToken: "new-rt",
		Expired: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339), Email: "existing@example.com", ProjectID: "new-project",
	}
	updated, err := createAntigravityChannel(context.Background(), store, updatedCredential)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != existing.ID || updated.MaxConcurrency != antigravityOAuthMaxConcurrency {
		t.Fatalf("updated Antigravity channel = %#v", updated)
	}
	persisted, err := store.GetConfig(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.MaxConcurrency != antigravityOAuthMaxConcurrency ||
		!strings.Contains(persisted.OAuthCredential, `"access_token":"new-at"`) {
		t.Fatalf("persisted Antigravity channel = %#v", persisted)
	}
}

func TestAntigravityChannelEditorExposesCredentialOnlyInEditor(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "gravity-editor-at", RefreshToken: "gravity-editor-rt",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "editor@example.com", ProjectID: "editor-project",
		PaidTier: &antigravityauth.PaidTier{ID: "free-tier", Name: "Antigravity Starter Quota"},
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
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].AntigravityPaidTier != "Antigravity Free" {
		t.Fatalf("channel list paid tier = %#v", list.Data)
	}
	if strings.Contains(listResponse.Body.String(), "gravity-editor-at") || strings.Contains(listResponse.Body.String(), "gravity-editor-rt") {
		t.Fatalf("channel list leaked Antigravity credential: %s", listResponse.Body.String())
	}
}

func TestHandleImportAntigravityCredentialCreatesSkipsAndDoesNotLeakTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, antigravityService: newAntigravityPaidTierTestService(t)}
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
	expiredAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	files := []struct {
		name string
		body string
	}{
		{name: "duplicate.json", body: fmt.Sprintf(`{"type":"antigravity","access_token":"at-must-not-overwrite","refresh_token":"rt-must-not-overwrite","expired":%q,"email":"duplicate@example.com","project_id":"project-other"}`, expiresAt)},
		{name: "new.json", body: fmt.Sprintf(`{"type":"antigravity","access_token":"at-import-secret","refresh_token":"rt-import-secret","expired":%q,"email":"new@example.com","project_id":"project-new"}`, expiredAt)},
		{name: "unusable.json", body: fmt.Sprintf(`{"type":"antigravity","access_token":"at-unusable-secret","refresh_token":"rt-unusable-secret","expired":%q,"email":"unusable@example.com","project_id":"project-unusable"}`, expiresAt)},
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
	for _, secret := range []string{"at-import-secret", "rt-import-secret", "at-refreshed-secret", "rt-rotated-secret", "at-must-not-overwrite", "rt-must-not-overwrite", "at-unusable-secret", "rt-unusable-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("import response leaked %q: %s", secret, response.Body.String())
		}
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 1 || result.Data.Skipped != 1 || result.Data.Failed != 2 || len(result.Data.Results) != 4 {
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
	if imported == nil || !imported.UsesAntigravityOAuth() || imported.MaxConcurrency != antigravityOAuthMaxConcurrency ||
		len(imported.URLs) != 1 || imported.URLs[0].URL != antigravityDailyBaseURL ||
		!imported.URLs[0].SupportsProtocol(util.ProtocolGemini) {
		t.Fatalf("new Antigravity channel was not created with canonical name: %#v", channels)
	}
	importedCredential, err := antigravityauth.ParseCredential([]byte(imported.OAuthCredential))
	if err != nil || importedCredential.AccessToken != "at-refreshed-secret" || importedCredential.RefreshToken != "rt-rotated-secret" ||
		importedCredential.PaidTier == nil || importedCredential.PaidTier.DisplayName() != "Google AI Pro" {
		t.Fatalf("imported paid tier = (%#v, %v)", importedCredential, err)
	}
	persisted, err := store.GetConfig(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.OAuthCredential != existingPayload {
		t.Fatalf("same-name import overwrote existing credential")
	}
}

func TestHandleImportAnthropicClaudeCredentialUsesEmailIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "claude-user@example.com.json")
	if err != nil {
		t.Fatal(err)
	}
	credentialBody := fmt.Sprintf(
		`{"type":"claude","access_token":"anthropic-import-access","refresh_token":"anthropic-import-refresh","email":"user@example.com","expired":%q,"priority":1,"disabled":false}`,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	)
	if _, err := part.Write([]byte(credentialBody)); err != nil {
		t.Fatal(err)
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
	if strings.Contains(response.Body.String(), "anthropic-import-access") ||
		strings.Contains(response.Body.String(), "anthropic-import-refresh") {
		t.Fatalf("import response leaked credential material: %s", response.Body.String())
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 1 || result.Data.Failed != 0 {
		t.Fatalf("import summary = %#v", result.Data)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 {
		t.Fatalf("channels = (%#v, %v)", channels, err)
	}
	channel := channels[0]
	if channel.Name != "Anthropic-user@example.com" {
		t.Fatalf("channel name = %q", channel.Name)
	}
	credential, err := anthropicauth.ParseCredential([]byte(channel.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if credential.EmailAddress != "user@example.com" || credential.DeviceID == "" {
		t.Fatalf("persisted Anthropic identity = email %q, device empty %t",
			credential.EmailAddress, credential.DeviceID == "")
	}

	finalized, err := finalizeAnthropicOAuthMessagesBody([]byte(`{
		"model":"claude-haiku-4-5-20251001",
		"messages":[{"role":"user","content":"hello"}]
	}`), channel, nil)
	if err != nil {
		t.Fatalf("finalize imported Anthropic credential: %v", err)
	}
	var payload struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(finalized, &payload); err != nil {
		t.Fatal(err)
	}
	var identity struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(payload.Metadata.UserID), &identity); err != nil {
		t.Fatal(err)
	}
	if identity.DeviceID == "" || identity.SessionID == "" || identity.AccountUUID != "" {
		t.Fatalf("wire identity = %#v", identity)
	}
}

func TestHandleImportCodexCredentialUsesAcceptedAccessTokenAndFailsUnusableCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	var transientProbeAttempts atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.String() == codexUsageURL:
			status := http.StatusUnauthorized
			authorization := request.Header.Get("Authorization")
			switch authorization {
			case "Bearer at-refreshed", "Bearer at-short-lived":
				status = http.StatusOK
			case "Bearer at-transient":
				if transientProbeAttempts.Add(1) == 1 {
					status = http.StatusServiceUnavailable
				} else {
					status = http.StatusOK
				}
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
		case request.Method == http.MethodPost && request.URL.String() == codexauth.DefaultTokenURL:
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if request.Form.Get("refresh_token") == "rt-refreshable" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-refreshed","refresh_token":"rt-rotated","expires_in":3600}`)),
					Request:    request,
				}, nil
			}
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
		default:
			return nil, fmt.Errorf("unexpected OAuth import request: %s %s", request.Method, request.URL.Host)
		}
	})}
	server := &Server{store: store, client: client}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	idToken := codexTestIDToken(t, "refreshable@example.com", "account-refreshable")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	files := []struct {
		name         string
		accessToken  string
		refreshToken string
		accountID    string
		idToken      string
	}{
		{name: "refreshable.json", accessToken: "at-stale", refreshToken: "rt-refreshable", accountID: "account-refreshable", idToken: idToken},
		{name: "short-lived.json", accessToken: "at-short-lived", refreshToken: "rt-short-lived-invalid", accountID: "account-short-lived", idToken: codexTestIDToken(t, "short-lived@example.com", "account-short-lived")},
		{name: "transient.json", accessToken: "at-transient", refreshToken: "rt-transient", accountID: "account-transient", idToken: codexTestIDToken(t, "transient@example.com", "account-transient")},
		{name: "unusable.json", accessToken: "at-unusable", refreshToken: "rt-unusable", accountID: "account-unusable", idToken: codexTestIDToken(t, "unusable@example.com", "account-unusable")},
	}
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatal(err)
		}
		credential := fmt.Sprintf(
			`{"type":"codex","access_token":%q,"refresh_token":%q,"id_token":%q,"account_id":%q,"expired":%q}`,
			file.accessToken, file.refreshToken, file.idToken, file.accountID, expiresAt,
		)
		if _, err := part.Write([]byte(credential)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/codex/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportCodexCredential(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 3 || result.Data.Skipped != 0 || result.Data.Failed != 1 {
		t.Fatalf("import summary = %#v", result.Data)
	}
	if attempts := transientProbeAttempts.Load(); attempts != 2 {
		t.Fatalf("transient probe attempts = %d, want 2", attempts)
	}
	var unusableResult *oauthCredentialImportResult
	for i := range result.Data.Results {
		if result.Data.Results[i].FileName == "unusable.json" {
			unusableResult = &result.Data.Results[i]
			break
		}
	}
	if unusableResult == nil || unusableResult.Status != "failed" || !strings.Contains(unusableResult.Error, "HTTP 401") {
		t.Fatalf("unusable result = %#v, want failed result with upstream status", unusableResult)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 3 {
		t.Fatalf("persisted channel count = %d, error = %v", len(channels), err)
	}
	persisted := make(map[string]*codexauth.Credential, len(channels))
	for _, channel := range channels {
		credential, parseErr := codexauth.ParseCredential([]byte(channel.OAuthCredential))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		persisted[credential.AccountID] = credential
	}
	refreshable := persisted["account-refreshable"]
	if refreshable == nil || refreshable.AccessToken != "at-refreshed" || refreshable.RefreshToken != "rt-rotated" {
		t.Fatal("persisted Codex credential did not use the refreshed tokens")
	}
	shortLived := persisted["account-short-lived"]
	if shortLived == nil || shortLived.AccessToken != "at-short-lived" || shortLived.RefreshToken != "rt-short-lived-invalid" {
		t.Fatal("persisted Codex credential did not keep the accepted access token")
	}
	transient := persisted["account-transient"]
	if transient == nil || transient.AccessToken != "at-transient" || transient.RefreshToken != "rt-transient" {
		t.Fatal("persisted Codex credential did not survive a transient validation failure")
	}
	for _, secret := range []string{"at-stale", "rt-refreshable", "at-short-lived", "rt-short-lived-invalid", "at-transient", "rt-transient", "at-unusable", "rt-unusable", "at-refreshed", "rt-rotated"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("import response leaked credential material")
		}
	}
}

func TestHandleImportOAuthCredentialsImportsTextAndAggregateFormats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	codexClient := newAcceptedCodexImportClient()
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet && request.URL.String() == xaiauth.CLIBaseURL+"/billing" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"entitlement_status":"active"}`)),
				Request:    request,
			}, nil
		}
		return codexClient.Transport.RoundTrip(request)
	})}
	server := &Server{store: store, client: client}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	expiresMillis := expiresAt.UnixMilli()

	bundle := fmt.Sprintf(`{
		"type":"cliproxyapi-auth-bundle",
		"version":1,
		"accounts":[
			{"type":"anthropic","access_token":"at-bundle","refresh_token":"rt-bundle","expired":%q,"email_address":"bundle@example.com","account_uuid":"account-bundle"},
			{"type":"codex","access_token":"at-bundle-invalid","refresh_token":"rt-bundle-invalid","expired":%q}
		]
	}`, expiresAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339))
	sub2API := fmt.Sprintf(`{
		"exported_at":%q,
		"proxies":[],
		"accounts":[
			{"platform":"openai","type":"oauth","name":"sub2-codex@example.com","priority":20,"credentials":{"accessToken":"at-sub2-codex","refreshToken":"rt-sub2-codex","accountId":"account-sub2-codex","expires_at":"","expiresAt":%d}},
			{"platform":"anthropic","credentials":{"access_token":"at-sub2-anthropic","refresh_token":"rt-sub2-anthropic","email_address":"sub2-anthropic@example.com","account_uuid":"account-sub2-anthropic","expires_at":%d}},
			{"platform":"grok","type":"oauth","name":"sub2-xai@example.com","credentials":{"access_token":"at-sub2-xai","refresh_token":"rt-sub2-xai","id_token":"id-sub2-xai","email":"sub2-xai@example.com","sub":"subject-sub2-xai","expires_at":%d,"client_id":"client-sub2-xai","base_url":"https://api.x.ai/v1","team_id":"team-sub2-xai"}},
			{"platform":"openai","credentials":{"access_token":"at-sub2-codex","refresh_token":"rt-sub2-duplicate","account_id":"account-sub2-codex","email":"duplicate@example.com","expires_at":%d}},
			{"platform":"google","credentials":{"access_token":"at-sub2-unsupported","refresh_token":"rt-sub2-unsupported","expires_at":%d}}
		]
	}`, expiresAt.Add(-time.Hour).Format(time.RFC3339), expiresMillis, expiresAt.Unix(), expiresMillis, expiresMillis, expiresMillis)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	files := []archiveCredentialTestEntry{
		{
			name: "direct.txt",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-direct","refresh_token":"rt-direct","account_id":"account-direct","email":"direct@example.com","expired":%q}`,
				expiresAt.Format(time.RFC3339),
			),
		},
		{name: "bundle.json", body: bundle},
		{name: "sub2api.json", body: sub2API},
		{name: "bad-bundle.json", body: `{"type":"cliproxyapi-auth-bundle","version":2,"accounts":[{}]}`},
	}
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, file.body); err != nil {
			t.Fatal(err)
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
		"at-direct", "rt-direct", "at-bundle", "rt-bundle", "at-bundle-invalid", "rt-bundle-invalid",
		"at-sub2-codex", "rt-sub2-codex", "at-sub2-anthropic", "rt-sub2-anthropic",
		"at-sub2-xai", "rt-sub2-xai", "id-sub2-xai",
		"rt-sub2-duplicate", "at-sub2-unsupported", "rt-sub2-unsupported",
	} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("import response leaked %q: %s", secret, response.Body.String())
		}
	}

	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes()).Data
	if result.Created != 5 || result.Skipped != 0 || result.Failed != 4 || len(result.Results) != 9 {
		t.Fatalf("import summary = %#v", result)
	}
	results := make(map[string]oauthCredentialImportResult, len(result.Results))
	for _, item := range result.Results {
		results[item.FileName] = item
	}
	if results["direct.txt"].Status != "created" ||
		results["bundle.json#1"].Status != "created" ||
		results["sub2api.json#1"].Status != "created" ||
		results["sub2api.json#2"].Status != "created" ||
		results["sub2api.json#3"].Status != "created" {
		t.Fatalf("valid aggregate results = %#v", results)
	}
	for fileName, errorPart := range map[string]string{
		"bad-bundle.json": "unsupported CLIProxyAPI auth bundle version",
		"bundle.json#2":   "account has no usable",
		"sub2api.json#4":  "duplicate credential record",
		"sub2api.json#5":  "unsupported account platform/type",
	} {
		item := results[fileName]
		if item.Status != "failed" || !strings.Contains(item.Error, errorPart) {
			t.Fatalf("result %q = %#v", fileName, item)
		}
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantChannels := map[string]string{
		"Codex-direct@example.com":             model.AuthTypeCodexOAuth,
		"Anthropic-bundle@example.com":         model.AuthTypeAnthropicOAuth,
		"Codex-sub2-codex@example.com":         model.AuthTypeCodexOAuth,
		"Anthropic-sub2-anthropic@example.com": model.AuthTypeAnthropicOAuth,
		"xAI-sub2-xai@example.com":             model.AuthTypeXAIOAuth,
	}
	if len(channels) != len(wantChannels) {
		t.Fatalf("channels = %#v", channels)
	}
	for _, channel := range channels {
		if wantType, ok := wantChannels[channel.Name]; !ok || channel.GetAuthType() != wantType {
			t.Fatalf("unexpected imported channel %#v", channel)
		}
		if channel.Name == "Codex-sub2-codex@example.com" {
			credential, parseErr := codexauth.ParseCredential([]byte(channel.OAuthCredential))
			if parseErr != nil {
				t.Fatalf("converted Codex credential = (%#v, %v)", credential, parseErr)
			}
			expiry, expiryErr := credential.Expiry()
			if credential.AccountID != "account-sub2-codex" || expiryErr != nil || !expiry.Equal(expiresAt) {
				t.Fatalf("converted Codex credential = (%#v, %v)", credential, expiryErr)
			}
		}
		if channel.Name == "Anthropic-sub2-anthropic@example.com" {
			credential, parseErr := anthropicauth.ParseCredential([]byte(channel.OAuthCredential))
			if parseErr != nil {
				t.Fatalf("converted Anthropic credential = (%#v, %v)", credential, parseErr)
			}
			expiry, expiryErr := credential.Expiry()
			if credential.AccountUUID != "account-sub2-anthropic" || expiryErr != nil || !expiry.Equal(expiresAt) {
				t.Fatalf("converted Anthropic credential = (%#v, %v)", credential, expiryErr)
			}
		}
		if channel.Name == "xAI-sub2-xai@example.com" {
			credential, parseErr := xaiauth.ParseCredential([]byte(channel.OAuthCredential))
			if parseErr != nil {
				t.Fatalf("converted xAI credential = (%#v, %v)", credential, parseErr)
			}
			expiry, expiryErr := credential.Expiry()
			if credential.Subject != "subject-sub2-xai" || credential.ClientID != "client-sub2-xai" ||
				credential.TeamID != "team-sub2-xai" || credential.BaseURL != "https://api.x.ai/v1" ||
				expiryErr != nil || !expiry.Equal(expiresAt) {
				t.Fatalf("converted xAI credential = (%#v, %v)", credential, expiryErr)
			}
		}
	}
}

func TestHandleImportOAuthCredentialsRejectsAggregateExpansionOverEntryLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, client: newAcceptedCodexImportClient()}
	accounts := make([]map[string]any, maxOAuthCredentialImportEntries+1)
	for index := range accounts {
		accounts[index] = map[string]any{
			"type":  "codex",
			"email": fmt.Sprintf("account-%05d@example.com", index),
		}
	}
	bundle, err := json.Marshal(map[string]any{
		"type": cliProxyAPIAuthBundleType, "version": cliProxyAPIAuthBundleVersion, "accounts": accounts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle) > maxOAuthCredentialImportBytes {
		t.Fatalf("test bundle size = %d, must exercise expanded entry limit", len(bundle))
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bundle); err != nil {
		t.Fatal(err)
	}
	direct, err := writer.CreateFormFile("files", "direct.txt")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := fmt.Fprintf(direct,
		`{"type":"codex","access_token":"at-direct-limit","refresh_token":"rt-direct-limit","account_id":"account-direct-limit","email":"direct-limit@example.com","expired":%q}`,
		expiresAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportOAuthCredentials(requestContext)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "exceeds 10000 entries") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes()).Data
	if result.Created != 1 || result.Failed != 1 || result.Skipped != 0 {
		t.Fatalf("import summary = %#v", result)
	}
	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 1 || channels[0].Name != "Codex-direct-limit@example.com" {
		t.Fatalf("channels = (%#v, %v)", channels, err)
	}
}

func TestHandleImportOAuthCredentialsSortsPriorityByCredentialFileName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{
		store: store, client: newAcceptedCodexImportClient(),
		antigravityService: newAntigravityPaidTierTestService(t),
	}
	existingAntigravity := newAntigravityOAuthChannel("Antigravity-existing", `{}`)
	existingAntigravity.Priority = 40
	existingCodex := newCodexOAuthChannel("Codex-existing", `{}`, "plus")
	existingCodex.Priority = 100
	for _, channel := range []*model.Config{existingAntigravity, existingCodex} {
		if _, err := store.CreateConfig(context.Background(), channel); err != nil {
			t.Fatal(err)
		}
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("priority_increment", "10"); err != nil {
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
	resultStatusByFile := make(map[string]string, len(result.Data.Results))
	for _, importResult := range result.Data.Results {
		resultStatusByFile[importResult.FileName] = importResult.Status
	}
	if resultStatusByFile["ambiguous.json"] != "skipped" || resultStatusByFile["unsupported.json"] != "skipped" {
		t.Fatalf("unrecognized credentials were not skipped: %#v", result.Data.Results)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil || len(channels) != 6 {
		t.Fatalf("channels = (%#v, %v)", channels, err)
	}
	want := map[string]struct {
		authType string
		priority int
	}{
		"Antigravity-existing":                     {authType: model.AuthTypeAntigravityOAuth, priority: 40},
		"Antigravity-gravity-explicit@example.com": {authType: model.AuthTypeAntigravityOAuth, priority: 50},
		"Antigravity-gravity-inferred@example.com": {authType: model.AuthTypeAntigravityOAuth, priority: 60},
		"Codex-existing":                           {authType: model.AuthTypeCodexOAuth, priority: 100},
		"Codex-codex-explicit@example.com":         {authType: model.AuthTypeCodexOAuth, priority: 110},
		"Codex-codex-inferred@example.com":         {authType: model.AuthTypeCodexOAuth, priority: 120},
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

func TestHandleImportOAuthCredentialsValidatesConcurrentlyAndContinuesAfterNetworkFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	var active, maxActive atomic.Int32
	concurrent := make(chan struct{})
	var concurrentOnce sync.Once
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		authorization := request.Header.Get("Authorization")
		if authorization == "Bearer at-a-network" {
			return nil, errors.New("simulated network failure")
		}
		current := active.Add(1)
		defer active.Add(-1)
		for observed := maxActive.Load(); current > observed && !maxActive.CompareAndSwap(observed, current); observed = maxActive.Load() {
		}
		if current >= 2 {
			concurrentOnce.Do(func() { close(concurrent) })
		}
		select {
		case <-concurrent:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		switch authorization {
		case "Bearer at-b-valid":
			time.Sleep(30 * time.Millisecond)
		case "Bearer at-c-valid":
			time.Sleep(10 * time.Millisecond)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil
	})}
	server := &Server{store: store, client: client}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("provider", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("priority_increment", "10"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"d-valid", "a-network", "c-valid", "b-valid"} {
		part, err := writer.CreateFormFile("files", name+".json")
		if err != nil {
			t.Fatal(err)
		}
		credential := fmt.Sprintf(
			`{"type":"codex","access_token":"at-%s","refresh_token":"rt-%s","account_id":"account-%s","email":"%s@example.com","expired":%q}`,
			name, name, name, name, expiresAt,
		)
		if _, err := io.WriteString(part, credential); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body).WithContext(requestCtx)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportOAuthCredentials(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes()).Data
	if result.Created != 3 || result.Failed != 1 || result.Skipped != 0 || len(result.Results) != 4 {
		t.Fatalf("import summary = %#v", result)
	}
	if maxActive.Load() < 2 {
		t.Fatalf("max concurrent validations = %d, want at least 2", maxActive.Load())
	}
	wantFiles := []string{"a-network.json", "b-valid.json", "c-valid.json", "d-valid.json"}
	gotFiles := make([]string, 0, len(result.Results))
	for _, importResult := range result.Results {
		gotFiles = append(gotFiles, importResult.FileName)
	}
	if !slices.Equal(gotFiles, wantFiles) {
		t.Fatalf("result order = %v, want %v", gotFiles, wantFiles)
	}
	if failed := result.Results[0]; failed.Status != "failed" || !strings.Contains(failed.Error, "Codex request failed") {
		t.Fatalf("network failure result = %#v", failed)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPriority := map[string]int{
		"Codex-b-valid@example.com": 10,
		"Codex-c-valid@example.com": 20,
		"Codex-d-valid@example.com": 30,
	}
	if len(channels) != len(wantPriority) {
		t.Fatalf("channels = %#v", channels)
	}
	for _, channel := range channels {
		if want, ok := wantPriority[channel.Name]; !ok || channel.Priority != want {
			t.Fatalf("channel %q priority=%d, want %d", channel.Name, channel.Priority, want)
		}
	}
}

func TestHandleImportOAuthCredentialsImportsArchivesByCredentialPriorityThenFileName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, client: newAcceptedCodexImportClient()}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	credential := func(account, fileName string, priority any) archiveCredentialTestEntry {
		priorityJSON, err := json.Marshal(priority)
		if err != nil {
			t.Fatal(err)
		}
		return archiveCredentialTestEntry{
			name: fileName,
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-%s","refresh_token":"rt-%s","account_id":%q,"email":%q,"expired":%q,"priority":%s}`,
				account, account, account, account+"@example.com", expiresAt, priorityJSON,
			),
		}
	}

	zipBody := makeCredentialZIP(t, []archiveCredentialTestEntry{
		credential("high", "a-high.json", 30),
		credential("low-z", "z-low.json", 10),
		credential("middle-b", "b-middle.txt", 20),
		{name: "README.md", body: "not a credential"},
	})
	tarGzBody := makeCredentialTarGz(t, []archiveCredentialTestEntry{
		credential("low-a", "a-low.json", 10),
		credential("middle-a", "middle.txt", 20),
	})
	direct := credential("middle-z", "z-middle.json", "20")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("provider", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("priority_increment", "10"); err != nil {
		t.Fatal(err)
	}
	for _, file := range []archiveCredentialTestEntry{
		{name: "credentials.zip", body: zipBody.String()},
		{name: "credentials.tar.gz", body: tarGzBody.String()},
		direct,
	} {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, file.body); err != nil {
			t.Fatal(err)
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
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
	if result.Data.Created != 6 || result.Data.Skipped != 0 || result.Data.Failed != 0 {
		t.Fatalf("import summary = %#v", result.Data)
	}
	wantFiles := []string{
		"credentials.tar.gz/a-low.json",
		"credentials.zip/z-low.json",
		"credentials.zip/b-middle.txt",
		"credentials.tar.gz/middle.txt",
		"z-middle.json",
		"credentials.zip/a-high.json",
	}
	gotFiles := make([]string, 0, len(result.Data.Results))
	for _, importResult := range result.Data.Results {
		gotFiles = append(gotFiles, importResult.FileName)
	}
	if !slices.Equal(gotFiles, wantFiles) {
		t.Fatalf("import order = %v, want %v", gotFiles, wantFiles)
	}

	channels, err := store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPriorityByName := map[string]int{
		"Codex-low-a@example.com":    10,
		"Codex-low-z@example.com":    20,
		"Codex-middle-b@example.com": 30,
		"Codex-middle-a@example.com": 40,
		"Codex-middle-z@example.com": 50,
		"Codex-high@example.com":     60,
	}
	if len(channels) != len(wantPriorityByName) {
		t.Fatalf("channels = %#v", channels)
	}
	for _, channel := range channels {
		if want, ok := wantPriorityByName[channel.Name]; !ok || channel.Priority != want {
			t.Fatalf("channel %q priority=%d, want %d", channel.Name, channel.Priority, want)
		}
	}
}

func TestHandleImportOAuthCredentialsStreamReportsEachCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, client: newAcceptedCodexImportClient()}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range []archiveCredentialTestEntry{
		{
			name: "b.json",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-b","refresh_token":"rt-b","account_id":"account-b","email":"b@example.com","expired":%q}`,
				expiresAt,
			),
		},
		{
			name: "a.json",
			body: fmt.Sprintf(
				`{"type":"codex","access_token":"at-a","refresh_token":"rt-a","account_id":"account-a","email":"a@example.com","expired":%q}`,
				expiresAt,
			),
		},
	} {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import/stream", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportOAuthCredentialsStream(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type=%q", contentType)
	}
	if !response.Flushed {
		t.Fatal("stream events were not flushed")
	}
	if strings.Contains(response.Body.String(), "at-a") || strings.Contains(response.Body.String(), "rt-b") {
		t.Fatal("stream leaked credential material")
	}

	type streamEvent struct {
		Event     string                       `json:"event"`
		JobID     string                       `json:"job_id"`
		Processed int                          `json:"processed"`
		Total     int                          `json:"total"`
		Created   int                          `json:"created"`
		Skipped   int                          `json:"skipped"`
		Failed    int                          `json:"failed"`
		FileName  string                       `json:"file_name"`
		Result    *oauthCredentialImportResult `json:"result"`
	}
	events := make([]streamEvent, 0)
	for block := range strings.SplitSeq(strings.TrimSpace(response.Body.String()), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event streamEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatalf("decode SSE event: %v", err)
			}
			events = append(events, event)
		}
	}
	wantTypes := []string{"start", "processing", "progress", "processing", "progress", "complete"}
	gotTypes := make([]string, 0, len(events))
	for _, event := range events {
		gotTypes = append(gotTypes, event.Event)
	}
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("event types=%v, want %v; body=%s", gotTypes, wantTypes, response.Body.String())
	}
	if events[0].JobID == "" || events[0].Total != 2 || events[1].FileName != "a.json" || events[2].Processed != 1 || events[2].Result == nil || events[2].Result.FileName != "a.json" {
		t.Fatalf("first credential events=%#v", events[:3])
	}
	complete := events[len(events)-1]
	if complete.Processed != 2 || complete.Total != 2 || complete.Created != 2 || complete.Skipped != 0 || complete.Failed != 0 {
		t.Fatalf("complete event=%#v", complete)
	}
}

func TestOAuthCredentialImportJobSurvivesUploadRequestCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != codexUsageURL {
			return nil, fmt.Errorf("unexpected OAuth import request: %s %s", request.Method, request.URL.String())
		}
		select {
		case probeStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseProbe:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	})}
	manager := newOAuthCredentialImportJobManager(context.Background(), 2)
	server := &Server{store: store, client: client, oauthCredentialImportJobs: manager}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Close(closeCtx); err != nil {
			t.Fatalf("close OAuth credential import jobs: %v", err)
		}
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "one.json")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	credential := fmt.Sprintf(
		`{"type":"codex","access_token":"at-job","refresh_token":"rt-job","account_id":"account-job","email":"job@example.com","expired":%q}`,
		expiresAt,
	)
	if _, err := io.WriteString(part, credential); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import/jobs", &body).WithContext(requestCtx)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	ginContext, response := newTestContext(t, request)
	server.HandleStartOAuthCredentialImportJob(ginContext)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start status=%d body=%s", response.Code, response.Body.String())
	}
	started := mustParseAPIResponse[oauthCredentialImportJobStart](t, response.Body.Bytes())
	if started.Data.JobID == "" || started.Data.Total != 1 {
		t.Fatalf("start response = %#v", started.Data)
	}
	cancelRequest()

	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("background import did not start after upload request cancellation")
	}
	close(releaseProbe)

	deadline := time.Now().Add(2 * time.Second)
	for {
		statusRequest := httptest.NewRequest(http.MethodGet, "/admin/oauth/credentials/import/jobs/"+started.Data.JobID+"?after=0", nil)
		statusContext, statusResponse := newTestContext(t, statusRequest)
		statusContext.Params = gin.Params{{Key: "id", Value: started.Data.JobID}}
		server.HandleOAuthCredentialImportJob(statusContext)
		if statusResponse.Code != http.StatusOK {
			t.Fatalf("job status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
		}
		if cacheControl := statusResponse.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("job Cache-Control = %q, want no-store", cacheControl)
		}
		if strings.Contains(statusResponse.Body.String(), "at-job") || strings.Contains(statusResponse.Body.String(), "rt-job") {
			t.Fatalf("job response leaked credential material: %s", statusResponse.Body.String())
		}
		view := mustParseAPIResponse[oauthCredentialImportJobView](t, statusResponse.Body.Bytes()).Data
		if view.Status == oauthCredentialImportJobSucceeded {
			if view.Processed != 1 || view.Created != 1 || view.Failed != 0 || len(view.Results) != 1 || view.Next != 1 {
				t.Fatalf("completed job = %#v", view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: %#v", view)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleImportOAuthCredentialsStreamAcceptsMoreThanDefaultMultipartLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCodexAuthTestStore(t)
	server := &Server{store: store, client: newAcceptedCodexImportClient()}

	const fileCount = 1506
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := range fileCount {
		part, err := writer.CreateFormFile("files", fmt.Sprintf("credential-%04d.json", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import/stream", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	requestContext, response := newTestContext(t, request)
	server.HandleImportOAuthCredentialsStream(requestContext)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	var complete oauthCredentialImportEvent
	for block := range strings.SplitSeq(strings.TrimSpace(response.Body.String()), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event oauthCredentialImportEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatalf("decode SSE event: %v", err)
			}
			if event.Event == "complete" {
				complete = event
			}
		}
	}
	if complete.Event != "complete" || complete.Processed != fileCount || complete.Total != fileCount || complete.Skipped != fileCount {
		t.Fatalf("complete event=%#v", complete)
	}
}

func TestHandleImportOAuthCredentialsRejectsUnsafeOrOversizedArchives(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		fileName  string
		archive   func(*testing.T) bytes.Buffer
		wantError string
	}{
		{
			name:     "ZIP path escape",
			fileName: "credentials.zip",
			archive: func(t *testing.T) bytes.Buffer {
				return makeCredentialZIP(t, []archiveCredentialTestEntry{{
					name: "../credential.json",
					body: `{ "type": "codex" }`,
				}})
			},
			wantError: "entry path",
		},
		{
			name:     "expanded size",
			fileName: "credentials.zip",
			archive: func(t *testing.T) bytes.Buffer {
				return makeCredentialZIP(t, []archiveCredentialTestEntry{{
					name: "ignored.bin",
					body: strings.Repeat("0", maxOAuthCredentialExpandedBytes+1),
				}})
			},
			wantError: "expanded bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newCodexAuthTestStore(t)
			server := &Server{store: store, client: newAcceptedCodexImportClient()}
			archive := tt.archive(t)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("files", tt.fileName)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(archive.Bytes()); err != nil {
				t.Fatal(err)
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
			result := mustParseAPIResponse[oauthCredentialImportSummary](t, response.Body.Bytes())
			if result.Data.Created != 0 || result.Data.Failed != 1 || len(result.Data.Results) != 1 {
				t.Fatalf("import summary = %#v", result.Data)
			}
			if !strings.Contains(result.Data.Results[0].Error, tt.wantError) {
				t.Fatalf("error = %q, want %q", result.Data.Results[0].Error, tt.wantError)
			}
			channels, err := store.ListConfigs(context.Background())
			if err != nil || len(channels) != 0 {
				t.Fatalf("channels = (%#v, %v), want none", channels, err)
			}
		})
	}
}

type archiveCredentialTestEntry struct {
	name string
	body string
}

func makeCredentialZIP(t *testing.T, entries []archiveCredentialTestEntry) bytes.Buffer {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for _, entry := range entries {
		part, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body
}

func makeCredentialTarGz(t *testing.T, entries []archiveCredentialTestEntry) bytes.Buffer {
	t.Helper()
	var body bytes.Buffer
	gzipWriter := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: entry.name,
			Mode: 0o600,
			Size: int64(len(entry.body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarWriter, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return body
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

func TestImportedOAuthCredentialUpsertsSameEmail(t *testing.T) {
	store := newCodexAuthTestStore(t)
	now := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	first := &codexauth.Credential{
		Type: "codex", AccessToken: "at-1", RefreshToken: "rt-1", Expired: now,
		ChatGPTUserID: "user-1", AccountID: "account-1", Email: "user@example.com",
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
		"gpt-image-1.5",
		"gpt-image-2",
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
		ChatGPTUserID: first.ChatGPTUserID, AccountID: "renamed-account", Email: "user@example.com",
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

func TestCodexReauthorizationMigratesLegacyEmailIdentity(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	create := func(name, email, accountID, accessToken string) *model.Config {
		t.Helper()
		credential := &codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-" + accessToken,
			Expired: expiresAt, AccountID: accountID, Email: email, PlanType: "plus",
		}
		payload, err := credential.JSON()
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateConfig(context.Background(), newCodexOAuthChannel(name, payload, credential.PlanType))
		if err != nil {
			t.Fatal(err)
		}
		return channel
	}
	accountFallback := create("Codex-legacy", "", "shared-workspace", "at-legacy-old")
	emailMatch := create("Codex-caidaoli+2@gmail.com", "caidaoli+2@gmail.com", "old-plus-2-account", "at-plus-2-old")
	reauthorized := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-plus-2-new", RefreshToken: "rt-plus-2-new",
		Expired: expiresAt, ChatGPTUserID: "user-plus-2", AccountID: "shared-workspace",
		Email: "caidaoli+2@gmail.com", PlanType: "plus",
	}

	updated, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, reauthorized)
	if err != nil || wasCreated || updated.ID != emailMatch.ID {
		t.Fatalf("reauthorization = (%#v, %v, %v), want channel %d", updated, wasCreated, err, emailMatch.ID)
	}
	updatedCredential, err := codexauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || updatedCredential.ChatGPTUserID != reauthorized.ChatGPTUserID {
		t.Fatalf("migrated credential = (%#v, %v)", updatedCredential, err)
	}
	persistedFallback, err := store.GetConfig(context.Background(), accountFallback.ID)
	if err != nil {
		t.Fatal(err)
	}
	fallbackCredential, err := codexauth.ParseCredential([]byte(persistedFallback.OAuthCredential))
	if err != nil || fallbackCredential.AccessToken != "at-legacy-old" {
		t.Fatalf("account fallback was overwritten = (%#v, %v)", fallbackCredential, err)
	}
}

func TestCodexOAuthCreatesChannelForDifferentUserInSameAccount(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	existing := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-plus-2", RefreshToken: "rt-plus-2",
		Expired: expiresAt, ChatGPTUserID: "user-plus-2", AccountID: "shared-workspace",
		Email: "caidaoli+2@gmail.com", PlanType: "plus",
	}
	existingJSON, err := existing.JSON()
	if err != nil {
		t.Fatal(err)
	}
	plusTwo, err := store.CreateConfig(
		context.Background(),
		newCodexOAuthChannel("Codex-caidaoli+2@gmail.com", existingJSON, existing.PlanType),
	)
	if err != nil {
		t.Fatal(err)
	}
	plusFour := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-plus-4", RefreshToken: "rt-plus-4",
		Expired: expiresAt, ChatGPTUserID: "user-plus-4", AccountID: existing.AccountID,
		Email: "caidaoli+4@gmail.com", PlanType: "team",
	}

	created, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, plusFour)
	if err != nil || !wasCreated || created.ID == plusTwo.ID || created.Name != "Codex-caidaoli+4@gmail.com" {
		t.Fatalf("shared-account authorization = (%#v, %v, %v)", created, wasCreated, err)
	}
	persistedPlusTwo, err := store.GetConfig(context.Background(), plusTwo.ID)
	if err != nil {
		t.Fatal(err)
	}
	plusTwoCredential, err := codexauth.ParseCredential([]byte(persistedPlusTwo.OAuthCredential))
	if err != nil || plusTwoCredential.Email != "caidaoli+2@gmail.com" || plusTwoCredential.AccessToken != "at-plus-2" {
		t.Fatalf("existing +2 channel was overwritten = (%#v, %v)", plusTwoCredential, err)
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("channels after shared-account authorization = (%d, %v), want 2", len(configs), err)
	}
}

func TestCodexOAuthDoesNotOverwriteDifferentUserWithSameEmail(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	first := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-first", RefreshToken: "rt-first",
		Expired: expiresAt, ChatGPTUserID: "user-first", AccountID: "shared-workspace",
		Email: "shared@example.com", PlanType: "team",
	}
	firstChannel, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, first)
	if err != nil || !wasCreated {
		t.Fatalf("first authorization = (%#v, %v, %v)", firstChannel, wasCreated, err)
	}
	second := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-second", RefreshToken: "rt-second",
		Expired: expiresAt, ChatGPTUserID: "user-second", AccountID: first.AccountID,
		Email: first.Email, PlanType: first.PlanType,
	}
	secondChannel, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, second)
	if err != nil || !wasCreated || secondChannel.ID == firstChannel.ID {
		t.Fatalf("second authorization = (%#v, %v, %v)", secondChannel, wasCreated, err)
	}
	persistedFirst, err := store.GetConfig(context.Background(), firstChannel.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstCredential, err := codexauth.ParseCredential([]byte(persistedFirst.OAuthCredential))
	if err != nil || firstCredential.AccessToken != first.AccessToken {
		t.Fatalf("first user credential was overwritten = (%#v, %v)", firstCredential, err)
	}
}

func TestCodexOAuthDoesNotOverwriteUserWhenIncomingUserIDIsMissing(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	existing := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-existing", RefreshToken: "rt-existing",
		Expired: expiresAt, ChatGPTUserID: "user-existing", AccountID: "shared-workspace",
		Email: "shared@example.com", PlanType: "team",
	}
	existingChannel, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, existing)
	if err != nil || !wasCreated {
		t.Fatalf("existing authorization = (%#v, %v, %v)", existingChannel, wasCreated, err)
	}
	incoming := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-incoming", RefreshToken: "rt-incoming",
		Expired: expiresAt, AccountID: existing.AccountID, Email: existing.Email, PlanType: existing.PlanType,
	}
	incomingChannel, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, incoming)
	if err != nil || !wasCreated || incomingChannel.ID == existingChannel.ID {
		t.Fatalf("missing-user-id authorization = (%#v, %v, %v)", incomingChannel, wasCreated, err)
	}
	persistedExisting, err := store.GetConfig(context.Background(), existingChannel.ID)
	if err != nil {
		t.Fatal(err)
	}
	existingCredential, err := codexauth.ParseCredential([]byte(persistedExisting.OAuthCredential))
	if err != nil || existingCredential.AccessToken != existing.AccessToken ||
		existingCredential.ChatGPTUserID != existing.ChatGPTUserID {
		t.Fatalf("existing credential was overwritten = (%#v, %v)", existingCredential, err)
	}
}

func TestCodexOAuthDoesNotUseAccountIDAsUserIdentity(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-initial", RefreshToken: "rt-initial",
		Expired: expiresAt, ChatGPTUserID: "user-initial", AccountID: "shared-workspace",
		Email: "initial@example.com", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, initial)
	if err != nil {
		t.Fatal(err)
	}
	withoutUserIdentity := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-without-identity", RefreshToken: "rt-without-identity",
		Expired: expiresAt, AccountID: initial.AccountID, PlanType: initial.PlanType,
	}
	created, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, withoutUserIdentity)
	if err != nil || !wasCreated || created.ID == channel.ID {
		t.Fatalf("shared account authorization = (%#v, %v, %v)", created, wasCreated, err)
	}
	persistedChannel, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := codexauth.ParseCredential([]byte(persistedChannel.OAuthCredential))
	if err != nil || persisted.AccessToken != initial.AccessToken {
		t.Fatalf("existing credential was overwritten = (%#v, %v)", persisted, err)
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("channels after shared account authorization = (%d, %v), want 2", len(configs), err)
	}
}

func TestCodexReauthorizationMatchesUserIDAfterEmailChanges(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	create := func(name, userID, email, accountID, accessToken string) *model.Config {
		t.Helper()
		credential := &codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-" + accessToken,
			Expired: expiresAt, ChatGPTUserID: userID, AccountID: accountID, Email: email, PlanType: "plus",
		}
		payload, err := credential.JSON()
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateConfig(context.Background(), newCodexOAuthChannel(name, payload, credential.PlanType))
		if err != nil {
			t.Fatal(err)
		}
		return channel
	}
	plusFour := create("Codex-caidaoli+4@gmail.com", "user-plus-4", "caidaoli+4@gmail.com", "shared-workspace", "at-plus-4-old")
	plusTwo := create("Codex-caidaoli+2@gmail.com", "user-plus-2", "caidaoli+2@gmail.com", "old-plus-2-account", "at-plus-2-old")
	reauthorized := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-plus-2-new", RefreshToken: "rt-plus-2-new",
		Expired: expiresAt, ChatGPTUserID: "user-plus-2", AccountID: "shared-workspace",
		Email: "renamed-plus-2@example.com", PlanType: "plus",
	}

	updated, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, reauthorized)
	if err != nil || wasCreated || updated.ID != plusTwo.ID {
		t.Fatalf("reauthorization = (%#v, %v, %v), want channel %d", updated, wasCreated, err, plusTwo.ID)
	}
	persistedPlusTwo, err := codexauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || persistedPlusTwo.AccessToken != "at-plus-2-new" {
		t.Fatalf("+2 credential = (%#v, %v)", persistedPlusTwo, err)
	}
	persistedPlusFour, err := store.GetConfig(context.Background(), plusFour.ID)
	if err != nil {
		t.Fatal(err)
	}
	plusFourCredential, err := codexauth.ParseCredential([]byte(persistedPlusFour.OAuthCredential))
	if err != nil || plusFourCredential.AccessToken != "at-plus-4-old" {
		t.Fatalf("+4 credential was overwritten = (%#v, %v)", plusFourCredential, err)
	}
}

func TestCodexReauthorizationRetriesConcurrentRuntimeMetadataUpdate(t *testing.T) {
	t.Parallel()
	baseStore := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old", Expired: expiresAt,
		ChatGPTUserID: "user-reauthorize", AccountID: "account-reauthorize",
		Email: "reauthorize@example.com", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, initial)
	if err != nil {
		t.Fatal(err)
	}
	sampledAt := time.Now().UTC().Format(time.RFC3339Nano)
	winner := cloneCodexCredential(initial)
	winner.PassiveUsage = &codexauth.PassiveUsage{
		SampledAt: sampledAt,
		Windows: []codexauth.PassiveUsageWindow{{
			Scope: "codex", LimitName: "codex", Kind: "primary", UsedPercent: 25,
			LimitWindowSeconds: 604800, ResetAt: time.Now().Add(7 * 24 * time.Hour).Unix(), SampledAt: sampledAt,
		}},
	}
	winner.OAuthUsage = json.RawMessage(`{"provider":"codex","windows":[]}`)
	winner.QuotaOverdraft = &codexauth.QuotaOverdraft{
		Enabled: true, SuccessfulRequests: 3, CostMicroUSD: 2500,
	}
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &concurrentOAuthWinnerStore{
		Store: baseStore, authType: model.AuthTypeCodexOAuth, winnerJSON: winnerJSON,
	}
	reauthorized := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-new", RefreshToken: "rt-new", Expired: expiresAt,
		ChatGPTUserID: initial.ChatGPTUserID, AccountID: initial.AccountID,
		Email: initial.Email, PlanType: initial.PlanType,
	}

	updated, wasCreated, err := createOrUpdateCodexChannel(context.Background(), store, reauthorized)
	if err != nil || wasCreated || updated.ID != channel.ID {
		t.Fatalf("reauthorization = (%#v, %v, %v)", updated, wasCreated, err)
	}
	persisted, err := codexauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "at-new" || persisted.RefreshToken != "rt-new" {
		t.Fatalf("reauthorization kept stale secrets: %#v", persisted)
	}
	if persisted.PassiveUsage == nil || len(persisted.PassiveUsage.Windows) != 1 ||
		persisted.PassiveUsage.Windows[0].UsedPercent != 25 ||
		!bytes.Equal(persisted.OAuthUsage, winner.OAuthUsage) ||
		persisted.QuotaOverdraft == nil || persisted.QuotaOverdraft.SuccessfulRequests != 3 ||
		persisted.QuotaOverdraft.CostMicroUSD != 2500 {
		t.Fatalf("reauthorization lost runtime metadata: %#v", persisted)
	}
}

func TestImportedOAuthCredentialRemovesModelsUnsupportedByPlan(t *testing.T) {
	store := newCodexAuthTestStore(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	plus := &codexauth.Credential{
		Type: "codex", AccessToken: "at-plus", RefreshToken: "rt-plus", Expired: expiresAt,
		ChatGPTUserID: "user-plan", AccountID: "account-plan", Email: "plan@example.com", PlanType: "plus",
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
		ChatGPTUserID: plus.ChatGPTUserID, AccountID: "account-plan", Email: "plan@example.com", PlanType: "free",
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
		"gpt-image-1.5",
		"gpt-image-2",
	}
	if got := updated.GetModels(); !slices.Equal(got, want) {
		t.Fatalf("free channel models = %v, want %v", got, want)
	}
}

func TestImportedOAuthCredentialModelsFollowPlanType(t *testing.T) {
	allModels := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark", "codex-auto-review",
		"gpt-image-1.5", "gpt-image-2",
	}
	teamModels := []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "codex-auto-review", "gpt-image-1.5", "gpt-image-2",
	}
	freeModels := []string{
		"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4-mini", "codex-auto-review",
		"gpt-image-1.5", "gpt-image-2",
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
	server := &Server{store: store, client: newAcceptedCodexImportClient()}
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

func TestHandleUpdateCodexQuotaOverdraftPersistsSettingAndKeepsStats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.codexCredentials = newCodexCredentialManager(nil, store, nil, nil)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-overdraft-setting", RefreshToken: "rt-overdraft-setting",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-setting",
		QuotaOverdraft: &codexauth.QuotaOverdraft{SuccessfulRequests: 4, CostMicroUSD: 3200},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/channels/%d/codex-quota-overdraft", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPut, path, bytes.NewBufferString(`{"enabled":true}`)))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}

	server.HandleUpdateCodexQuotaOverdraft(c)

	if w.Code != http.StatusOK {
		t.Fatalf("update overdraft status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[codexQuotaOverdraftSettingsResponse](t, w.Body.Bytes())
	if resp.Data.QuotaOverdraft == nil || !resp.Data.QuotaOverdraft.Enabled ||
		resp.Data.QuotaOverdraft.SuccessfulRequests != 4 || resp.Data.QuotaOverdraft.CostMicroUSD != 3200 {
		t.Fatalf("update overdraft response=%#v", resp.Data.QuotaOverdraft)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if persistedCredential.QuotaOverdraft == nil || !persistedCredential.QuotaOverdraft.Enabled ||
		persistedCredential.QuotaOverdraft.SuccessfulRequests != 4 ||
		persistedCredential.QuotaOverdraft.CostMicroUSD != 3200 {
		t.Fatalf("persisted overdraft=%#v", persistedCredential.QuotaOverdraft)
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
		Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), AccountID: "account-refresh", PlanType: "plus",
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

func TestCodexCredentialManagerCASMissReusesConcurrentWinner(t *testing.T) {
	baseStore := newCodexAuthTestStore(t)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), AccountID: "account-cas", PlanType: "free",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, initial)
	if err != nil {
		t.Fatal(err)
	}
	winner := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-winner", RefreshToken: "rt-winner",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-cas", PlanType: "pro",
	}
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &concurrentOAuthWinnerStore{
		Store: baseStore, authType: model.AuthTypeCodexOAuth, winnerJSON: winnerJSON,
	}
	var refreshCount atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("refresh_token"); got != "rt-old" {
			t.Errorf("refresh token = %q, want old token on first attempt", got)
		}
		_, _ = io.WriteString(w, `{"access_token":"at-stale","refresh_token":"rt-stale","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	manager := newCodexCredentialManager(service, store, nil, nil)

	got, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if got.AccessToken != "at-winner" || got.RefreshToken != "rt-winner" {
		t.Fatalf("credential() = %#v, want concurrent winner", got)
	}
	if refreshCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want no retry with stale refresh token", refreshCount.Load())
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.RefreshToken != "rt-winner" {
		t.Fatalf("persisted credential = (%#v, %v), want winner refresh token", persistedCredential, err)
	}
	if !persisted.SupportsModel("gpt-5.4") || !persisted.SupportsModel("gpt-5.6-sol") {
		t.Fatalf("winning pro credential has stale free models: %v", persisted.GetModels())
	}
}

func TestCodexCredentialManagerCASMissMergesPassiveUsageWithoutRefreshingTwice(t *testing.T) {
	baseStore := newCodexAuthTestStore(t)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-once",
		Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), AccountID: "account-passive-cas", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, initial)
	if err != nil {
		t.Fatal(err)
	}
	winner := *initial
	sampledAt := time.Now().UTC()
	winner.PassiveUsage = &codexauth.PassiveUsage{
		SampledAt: sampledAt.Format(time.RFC3339Nano),
		Windows: []codexauth.PassiveUsageWindow{{
			Scope: "codex", LimitName: "codex", Kind: "primary", UsedPercent: 25,
			LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: time.Now().Add(7 * 24 * time.Hour).Unix(),
			SampledAt: sampledAt.Format(time.RFC3339Nano),
		}},
	}
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &concurrentOAuthWinnerStore{
		Store: baseStore, authType: model.AuthTypeCodexOAuth, winnerJSON: winnerJSON,
	}
	var refreshCount atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if refreshCount.Add(1) != 1 {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	manager := newCodexCredentialManager(service, store, nil, nil)

	got, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-new" {
		t.Fatalf("credential() = %#v, want refreshed token", got)
	}
	if refreshCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want exactly one", refreshCount.Load())
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
		persistedCredential.PassiveUsage.Windows[0].UsedPercent != 25 {
		t.Fatalf("persisted credential lost passive quota = (%#v, %v)", persistedCredential, err)
	}
}

func TestCodexQuotaOverdraftStatsRetryAfterConcurrentTokenRefresh(t *testing.T) {
	t.Parallel()
	baseStore := newCodexAuthTestStore(t)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-cas",
		QuotaOverdraft: &codexauth.QuotaOverdraft{Enabled: true, ActiveUntil: time.Now().Add(time.Hour).Unix()},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, initial)
	if err != nil {
		t.Fatal(err)
	}
	winner := cloneCodexCredential(initial)
	winner.AccessToken = "at-refreshed"
	winner.RefreshToken = "rt-refreshed"
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &concurrentOAuthWinnerStore{
		Store: baseStore, authType: model.AuthTypeCodexOAuth, winnerJSON: winnerJSON,
	}
	manager := newCodexCredentialManager(nil, store, nil, nil)
	stats, recorded, err := manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 1750, false, 0)
	if err != nil {
		t.Fatalf("recordQuotaOverdraftSuccess() error = %v", err)
	}
	if !recorded {
		t.Fatal("active overdraft success was not recorded")
	}
	if stats.SuccessfulRequests != 1 || stats.CostMicroUSD != 1750 {
		t.Fatalf("returned overdraft stats = %#v", stats)
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if persistedCredential.AccessToken != "at-refreshed" || persistedCredential.RefreshToken != "rt-refreshed" ||
		persistedCredential.QuotaOverdraft == nil || persistedCredential.QuotaOverdraft.SuccessfulRequests != 1 ||
		persistedCredential.QuotaOverdraft.CostMicroUSD != 1750 {
		t.Fatalf("concurrent refresh was overwritten or stats were lost: %#v", persistedCredential)
	}
}

func TestCodexQuotaOverdraftStatsFollowCredentialActiveCycle(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	now := time.Now().UTC()
	passiveResetAt := now.Add(2 * time.Hour).Unix()
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-overdraft-cycle", RefreshToken: "rt-overdraft-cycle",
		Expired: now.Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-cycle",
		QuotaOverdraft: &codexauth.QuotaOverdraft{Enabled: true},
		PassiveUsage: &codexauth.PassiveUsage{
			SampledAt: now.Format(time.RFC3339Nano),
			Windows: []codexauth.PassiveUsageWindow{{
				Scope: codexauth.ChannelType, LimitName: "codex", Kind: "primary", UsedPercent: 100,
				LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: passiveResetAt, SampledAt: now.Format(time.RFC3339Nano),
			}},
		},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatal(err)
	}
	manager := newCodexCredentialManager(nil, store, nil, nil)

	stats, recorded, err := manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 10, false, 0)
	if err != nil || recorded || stats.SuccessfulRequests != 0 || stats.CostMicroUSD != 0 {
		t.Fatalf("inactive success=(%#v, recorded=%t, err=%v), want no accounting", stats, recorded, err)
	}
	activeUntil := now.Add(time.Hour).Unix()
	stats, recorded, err = manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 10, true, activeUntil)
	if err != nil || !recorded || stats.ActiveUntil != activeUntil ||
		stats.SuccessfulRequests != 1 || stats.CostMicroUSD != 10 {
		t.Fatalf("activating success=(%#v, recorded=%t, err=%v)", stats, recorded, err)
	}
	stats, recorded, err = manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 20, false, 0)
	if err != nil || !recorded || stats.SuccessfulRequests != 2 || stats.CostMicroUSD != 30 {
		t.Fatalf("active follow-up=(%#v, recorded=%t, err=%v)", stats, recorded, err)
	}
	stats, err = manager.setQuotaOverdraftEnabled(context.Background(), channel.ID, false)
	if err != nil || stats.Enabled || stats.ActiveUntil != 0 {
		t.Fatalf("disabled overdraft=%#v err=%v, want inactive state", stats, err)
	}
	stats, recorded, err = manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 40, false, 0)
	if err != nil || recorded || stats.SuccessfulRequests != 2 || stats.CostMicroUSD != 30 {
		t.Fatalf("disabled follow-up=(%#v, recorded=%t, err=%v), want unchanged stats", stats, recorded, err)
	}
}

func TestCodexQuotaOverdraftStatsRetryTransientCredentialWrite(t *testing.T) {
	t.Parallel()
	baseStore := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-overdraft-retry", RefreshToken: "rt-overdraft-retry",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-retry",
		QuotaOverdraft: &codexauth.QuotaOverdraft{Enabled: true, ActiveUntil: time.Now().Add(time.Hour).Unix()},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, credential)
	if err != nil {
		t.Fatal(err)
	}
	store := &transientOAuthCASStore{Store: baseStore}
	manager := newCodexCredentialManager(nil, store, nil, nil)
	stats, recorded, err := manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 12, false, 0)
	if err != nil {
		t.Fatalf("recordQuotaOverdraftSuccess() error = %v", err)
	}
	if !recorded {
		t.Fatal("active overdraft success was not recorded")
	}
	if store.calls.Load() != 2 {
		t.Fatalf("credential CAS calls=%d, want 2", store.calls.Load())
	}
	if stats.SuccessfulRequests != 1 || stats.CostMicroUSD != 12 {
		t.Fatalf("overdraft stats=%#v", stats)
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.QuotaOverdraft == nil ||
		persistedCredential.QuotaOverdraft.SuccessfulRequests != 1 ||
		persistedCredential.QuotaOverdraft.CostMicroUSD != 12 {
		t.Fatalf("persisted overdraft stats=(%#v, %v)", persistedCredential, err)
	}
}

func TestCodexQuotaOverdraftStatsRetryTransientCredentialRead(t *testing.T) {
	t.Parallel()
	baseStore := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-overdraft-read-retry", RefreshToken: "rt-overdraft-read-retry",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-read-retry",
		QuotaOverdraft: &codexauth.QuotaOverdraft{Enabled: true, ActiveUntil: time.Now().Add(time.Hour).Unix()},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, credential)
	if err != nil {
		t.Fatal(err)
	}
	store := &transientOAuthGetStore{Store: baseStore}
	manager := newCodexCredentialManager(nil, store, nil, nil)
	stats, recorded, err := manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 12, false, 0)
	if err != nil {
		t.Fatalf("recordQuotaOverdraftSuccess() error = %v", err)
	}
	if !recorded {
		t.Fatal("active overdraft success was not recorded")
	}
	if store.getCalls.Load() != 2 || store.casCalls.Load() != 1 {
		t.Fatalf("credential calls=(get %d, CAS %d), want (get 2, CAS 1)", store.getCalls.Load(), store.casCalls.Load())
	}
	if stats.SuccessfulRequests != 1 || stats.CostMicroUSD != 12 {
		t.Fatalf("overdraft stats=%#v", stats)
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.QuotaOverdraft == nil ||
		persistedCredential.QuotaOverdraft.SuccessfulRequests != 1 ||
		persistedCredential.QuotaOverdraft.CostMicroUSD != 12 {
		t.Fatalf("persisted overdraft stats=(%#v, %v)", persistedCredential, err)
	}
}

func TestCodexQuotaOverdraftStatsAccumulateConcurrentSuccesses(t *testing.T) {
	t.Parallel()
	store := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-overdraft-concurrent", RefreshToken: "rt-overdraft-concurrent",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-overdraft-concurrent",
		QuotaOverdraft: &codexauth.QuotaOverdraft{Enabled: true, ActiveUntil: time.Now().Add(time.Hour).Unix()},
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatal(err)
	}
	manager := newCodexCredentialManager(nil, store, nil, nil)
	const successes = 8
	errorsCh := make(chan error, successes)
	var wg sync.WaitGroup
	for range successes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, recorded, recordErr := manager.recordQuotaOverdraftSuccess(context.Background(), channel.ID, 12, false, 0)
			if recordErr == nil && !recorded {
				recordErr = errors.New("active overdraft success was not recorded")
			}
			errorsCh <- recordErr
		}()
	}
	wg.Wait()
	close(errorsCh)
	for recordErr := range errorsCh {
		if recordErr != nil {
			t.Fatalf("recordQuotaOverdraftSuccess() error = %v", recordErr)
		}
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.QuotaOverdraft == nil ||
		persistedCredential.QuotaOverdraft.SuccessfulRequests != successes ||
		persistedCredential.QuotaOverdraft.CostMicroUSD != successes*12 {
		t.Fatalf("concurrent overdraft stats=(%#v, %v)", persistedCredential, err)
	}
}

func TestCodexPassiveUsageKeepsLatestResultPerQuotaGroup(t *testing.T) {
	store := newCodexAuthTestStore(t)
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at", RefreshToken: "rt",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-quota-order", PlanType: "pro",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatal(err)
	}
	manager := newCodexCredentialManager(codexauth.NewService(nil), store, nil, nil)
	newerTime := time.Date(2030, 1, 2, 3, 4, 6, 0, time.UTC)
	olderTime := newerTime.Add(-time.Second)
	window := func(scope, name string, used float64, sampledAt time.Time) codexauth.PassiveUsageWindow {
		return codexauth.PassiveUsageWindow{
			Scope: scope, LimitName: name, Kind: "primary", UsedPercent: used,
			LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: 1893542400,
			SampledAt: sampledAt.Format(time.RFC3339Nano),
		}
	}
	if updated, err := manager.updatePassiveUsage(context.Background(), channel, codexPassiveUsageUpdate{
		SampledAt: newerTime.Format(time.RFC3339Nano),
		Windows:   []codexauth.PassiveUsageWindow{window("codex", "codex", 20, newerTime)},
	}); err != nil || !updated {
		t.Fatalf("persist newer generic quota = (%v, %v)", updated, err)
	}
	if updated, err := manager.updatePassiveUsage(context.Background(), channel, codexPassiveUsageUpdate{
		SampledAt: olderTime.Format(time.RFC3339Nano),
		Windows: []codexauth.PassiveUsageWindow{
			window("codex", "codex", 10, olderTime),
			window("bengalfox", "GPT-5.3-Codex-Spark", 25, olderTime),
		},
	}); err != nil || !updated {
		t.Fatalf("merge older independent quota group = (%v, %v)", updated, err)
	}

	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 2 {
		t.Fatalf("persisted ordered quota = (%#v, %v)", persistedCredential, err)
	}
	if persistedCredential.PassiveUsage.Windows[0].UsedPercent != 20 ||
		persistedCredential.PassiveUsage.Windows[1].UsedPercent != 25 {
		t.Fatalf("stale response overwrote newer quota: %#v", persistedCredential.PassiveUsage.Windows)
	}

	sseTime := newerTime.Add(time.Second)
	if updated, err := manager.updatePassiveUsage(context.Background(), channel, codexPassiveUsageUpdate{
		SampledAt: sseTime.Format(time.RFC3339Nano),
		Windows: []codexauth.PassiveUsageWindow{
			window("gpt-5.3-codex-spark", "GPT-5.3-Codex-Spark", 30, sseTime),
		},
	}); err != nil || !updated {
		t.Fatalf("merge SSE quota with header quota = (%v, %v)", updated, err)
	}
	persisted, err = store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err = codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 2 ||
		persistedCredential.PassiveUsage.Windows[1].Scope != "gpt-5.3-codex-spark" ||
		persistedCredential.PassiveUsage.Windows[1].UsedPercent != 30 {
		t.Fatalf("header and SSE quota were not merged by limit name: (%#v, %v)", persistedCredential, err)
	}
}

func TestCodexCredentialManagerReloadsPersistedCredentialBeforeRefresh(t *testing.T) {
	t.Run("forced request reuses a newer access token", func(t *testing.T) {
		store := newCodexAuthTestStore(t)
		initial := &codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
			Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-reload", PlanType: "plus",
		}
		channel, _, err := createOrUpdateCodexChannel(context.Background(), store, initial)
		if err != nil {
			t.Fatal(err)
		}
		winner := *initial
		winner.AccessToken = "at-winner"
		winner.RefreshToken = "rt-winner"
		winnerJSON, err := winner.JSON()
		if err != nil {
			t.Fatal(err)
		}
		updated, err := store.CompareAndSwapOAuthCredential(
			context.Background(), channel.ID, model.AuthTypeCodexOAuth, channel.OAuthCredential, winnerJSON,
		)
		if err != nil || !updated {
			t.Fatalf("persist winner = (%v, %v)", updated, err)
		}

		var refreshCount atomic.Int32
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			refreshCount.Add(1)
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		}))
		defer tokenServer.Close()
		service := codexauth.NewService(tokenServer.Client())
		service.TokenURL = tokenServer.URL
		manager := newCodexCredentialManager(service, store, nil, nil)

		got, err := manager.credential(context.Background(), channel, true)
		if err != nil {
			t.Fatalf("credential() error = %v", err)
		}
		if got.AccessToken != "at-winner" || got.RefreshToken != "rt-winner" {
			t.Fatalf("credential() = %#v, want persisted winner", got)
		}
		if refreshCount.Load() != 0 {
			t.Fatalf("refresh requests = %d, want 0", refreshCount.Load())
		}
	})

	t.Run("expired winner refreshes with the winner refresh token", func(t *testing.T) {
		store := newCodexAuthTestStore(t)
		initial := &codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
			Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), AccountID: "account-refresh-winner", PlanType: "plus",
		}
		channel, _, err := createOrUpdateCodexChannel(context.Background(), store, initial)
		if err != nil {
			t.Fatal(err)
		}
		winner := *initial
		winner.AccessToken = "at-winner"
		winner.RefreshToken = "rt-winner"
		winnerJSON, err := winner.JSON()
		if err != nil {
			t.Fatal(err)
		}
		updated, err := store.CompareAndSwapOAuthCredential(
			context.Background(), channel.ID, model.AuthTypeCodexOAuth, channel.OAuthCredential, winnerJSON,
		)
		if err != nil || !updated {
			t.Fatalf("persist winner = (%v, %v)", updated, err)
		}

		var refreshCount atomic.Int32
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refreshCount.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if got := r.Form.Get("refresh_token"); got != "rt-winner" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"at-refreshed","refresh_token":"rt-refreshed","expires_in":3600}`)
		}))
		defer tokenServer.Close()
		service := codexauth.NewService(tokenServer.Client())
		service.TokenURL = tokenServer.URL
		manager := newCodexCredentialManager(service, store, nil, nil)

		got, err := manager.credential(context.Background(), channel, false)
		if err != nil {
			t.Fatalf("credential() error = %v", err)
		}
		if got.AccessToken != "at-refreshed" || got.RefreshToken != "rt-refreshed" {
			t.Fatalf("credential() = %#v, want refreshed winner", got)
		}
		if refreshCount.Load() != 1 {
			t.Fatalf("refresh requests = %d, want 1", refreshCount.Load())
		}
	})
}

func TestCodexCredentialManagerCachesSQLiteWinnerWhenPrimarySyncFails(t *testing.T) {
	primaryStore, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "primary.db"))
	if err != nil {
		t.Fatal(err)
	}
	replicaStore, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	primary := primaryStore.(*sqlstore.SQLStore)
	replica := replicaStore.(*sqlstore.SQLStore)
	hybrid := storage.NewHybridStore(replica, primary)
	t.Cleanup(func() { _ = hybrid.Close() })
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), AccountID: "account-hybrid", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), hybrid, initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.ExecContext(context.Background(), `
		CREATE TRIGGER reject_oauth_credential_update
		BEFORE UPDATE OF oauth_credential ON channels
		BEGIN
			SELECT RAISE(FAIL, 'oauth credential replica is read only');
		END
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.ExecContext(context.Background(), `
		CREATE TRIGGER reject_oauth_channel_insert
		BEFORE INSERT ON channels
		BEGIN
			SELECT RAISE(FAIL, 'primary is unavailable');
		END
	`); err != nil {
		t.Fatal(err)
	}
	var refreshCount atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	var invalidations atomic.Int32
	manager := newCodexCredentialManager(service, hybrid, nil, func(int64) { invalidations.Add(1) })

	first, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	second, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("cached credential() error = %v", err)
	}
	if first.RefreshToken != "rt-new" || second.RefreshToken != "rt-new" {
		t.Fatalf("credentials = (%#v, %#v), want committed winner", first, second)
	}
	if refreshCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want one", refreshCount.Load())
	}
	if invalidations.Load() != 1 {
		t.Fatalf("invalidations = %d, want one after committed refresh", invalidations.Load())
	}
	persisted, err := replica.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.RefreshToken != "rt-new" {
		t.Fatalf("SQLite credential = (%#v, %v)", persistedCredential, err)
	}
}

func TestCodexReauthorizationLateModelWriteCannotOverrideWinningPlan(t *testing.T) {
	baseStore := newCodexAuthTestStore(t)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	initial := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-initial", RefreshToken: "rt-initial",
		Expired: expires, ChatGPTUserID: "user-plan-cas", AccountID: "account-plan-cas",
		Email: "plan-cas@example.com", PlanType: "plus",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), baseStore, initial)
	if err != nil {
		t.Fatal(err)
	}
	channel.ScheduledCheckModel = "gpt-5.4"
	if _, err := baseStore.UpdateConfig(context.Background(), channel.ID, channel); err != nil {
		t.Fatal(err)
	}

	store := &blockingCodexModelStateStore{
		Store: baseStore, firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	free := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-free", RefreshToken: "rt-free",
		Expired: expires, ChatGPTUserID: initial.ChatGPTUserID, AccountID: "account-plan-cas",
		Email: "plan-cas@example.com", PlanType: "free",
	}
	freeDone := make(chan error, 1)
	go func() {
		_, _, updateErr := createOrUpdateCodexChannel(context.Background(), store, free)
		freeDone <- updateErr
	}()
	<-store.firstStarted

	pro := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-pro", RefreshToken: "rt-pro",
		Expired: expires, ChatGPTUserID: initial.ChatGPTUserID, AccountID: "account-plan-cas",
		Email: "plan-cas@example.com", PlanType: "pro",
	}
	if _, _, err := createOrUpdateCodexChannel(context.Background(), store, pro); err != nil {
		t.Fatalf("winning pro reauthorization error = %v", err)
	}
	close(store.releaseFirst)
	if err := <-freeDone; err != nil {
		t.Fatalf("late free reauthorization error = %v", err)
	}

	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if persistedCredential.PlanType != "pro" || persistedCredential.RefreshToken != "rt-pro" {
		t.Fatalf("winning credential = %#v, want pro", persistedCredential)
	}
	if !persisted.SupportsModel("gpt-5.4") || !persisted.SupportsModel("gpt-5.6-sol") {
		t.Fatalf("pro credential has stale free models: %v", persisted.GetModels())
	}
	if persisted.ScheduledCheckModel != "gpt-5.4" {
		t.Fatalf("scheduled check model = %q, want pro model preserved", persisted.ScheduledCheckModel)
	}
}

func TestAntigravityCredentialManagerCASMissReusesConcurrentWinner(t *testing.T) {
	baseStore := newCodexAuthTestStore(t)
	initial := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
		Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), Email: "cas@example.com", ProjectID: "project-cas",
	}
	initialJSON, err := initial.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := baseStore.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity CAS", initialJSON))
	if err != nil {
		t.Fatal(err)
	}
	winner := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-winner", RefreshToken: "rt-winner",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Email: "cas@example.com", ProjectID: "project-cas",
	}
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &concurrentOAuthWinnerStore{
		Store: baseStore, authType: model.AuthTypeAntigravityOAuth, winnerJSON: winnerJSON,
	}
	var refreshCount atomic.Int32
	client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		body := `{}`
		switch request.URL.Path {
		case "/token":
			refreshCount.Add(1)
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if got := request.Form.Get("refresh_token"); got != "rt-old" {
				t.Errorf("refresh token = %q, want old token on first attempt", got)
			}
			body = `{"access_token":"at-stale","refresh_token":"rt-stale","expires_in":3600}`
		case "/v1internal:loadCodeAssist":
			body = `{"cloudaicompanionProject":"project-cas","paidTier":{"id":"tier"}}`
		default:
			return nil, fmt.Errorf("unexpected Antigravity request: %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	service := antigravityauth.NewService(client)
	service.TokenURL = "https://oauth.test/token"
	service.APIBaseURL = "https://api.test"
	service.DailyAPIBaseURL = "https://api.test"
	manager := newAntigravityCredentialManager(service, store, nil, nil)

	got, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if got.AccessToken != "at-winner" || got.RefreshToken != "rt-winner" {
		t.Fatalf("credential() = %#v, want concurrent winner", got)
	}
	if refreshCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want no retry with stale refresh token", refreshCount.Load())
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := antigravityauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.RefreshToken != "rt-winner" {
		t.Fatalf("persisted credential = (%#v, %v), want winner refresh token", persistedCredential, err)
	}
}

func TestAntigravityCredentialManagerReloadsPersistedCredentialBeforeRefresh(t *testing.T) {
	for _, tc := range []struct {
		name          string
		force         bool
		winnerExpires time.Duration
		wantAccess    string
		wantRefreshes int32
	}{
		{name: "forced request reuses a newer access token", force: true, winnerExpires: time.Hour, wantAccess: "at-winner"},
		{name: "expired winner refreshes with the winner refresh token", winnerExpires: time.Minute, wantAccess: "at-refreshed", wantRefreshes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newCodexAuthTestStore(t)
			initial := &antigravityauth.Credential{
				Type: antigravityauth.ChannelType, AccessToken: "at-old", RefreshToken: "rt-old",
				Expired: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), Email: "reload@example.com", ProjectID: "project-reload",
			}
			initialJSON, err := initial.JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity reload", initialJSON))
			if err != nil {
				t.Fatal(err)
			}
			winner := *initial
			winner.AccessToken = "at-winner"
			winner.RefreshToken = "rt-winner"
			winner.Expired = time.Now().UTC().Add(tc.winnerExpires).Format(time.RFC3339)
			winnerJSON, err := winner.JSON()
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.CompareAndSwapOAuthCredential(
				context.Background(), channel.ID, model.AuthTypeAntigravityOAuth, channel.OAuthCredential, winnerJSON,
			)
			if err != nil || !updated {
				t.Fatalf("persist winner = (%v, %v)", updated, err)
			}

			var refreshCount atomic.Int32
			client := &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
				body := `{"cloudaicompanionProject":"project-reload","paidTier":{"id":"tier"}}`
				if request.URL.Path == "/token" {
					refreshCount.Add(1)
					if err := request.ParseForm(); err != nil {
						return nil, err
					}
					if got := request.Form.Get("refresh_token"); got != "rt-winner" {
						body = `{"error":"invalid_grant"}`
						return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
					}
					body = `{"access_token":"at-refreshed","refresh_token":"rt-refreshed","expires_in":3600}`
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)), Request: request,
				}, nil
			})}
			service := antigravityauth.NewService(client)
			service.TokenURL = "https://oauth.test/token"
			service.APIBaseURL = "https://api.test"
			service.DailyAPIBaseURL = "https://api.test"
			manager := newAntigravityCredentialManager(service, store, nil, nil)

			got, err := manager.credential(context.Background(), channel, tc.force)
			if err != nil {
				t.Fatalf("credential() error = %v", err)
			}
			if got.AccessToken != tc.wantAccess {
				t.Fatalf("credential() = %#v, want access token %q", got, tc.wantAccess)
			}
			if refreshCount.Load() != tc.wantRefreshes {
				t.Fatalf("refresh requests = %d, want %d", refreshCount.Load(), tc.wantRefreshes)
			}
		})
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
		Expired:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
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
	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil ||
		list.Data[0].OAuthUsage.Provider != codexauth.ChannelType || len(list.Data[0].OAuthUsage.Windows) != 3 {
		t.Fatalf("persisted Codex usage = %+v", list.Data)
	}
}

func TestHandleOAuthUsageBatchStreamUsesBoundedConcurrencyAndEmitsPerChannelResults(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	channelIDs := make([]int64, 0, oauthUsageBatchWorkers+2)
	for index := range oauthUsageBatchWorkers + 2 {
		credential := &codexauth.Credential{
			Type:         codexauth.ChannelType,
			AccessToken:  fmt.Sprintf("at-batch-quota-%d", index),
			RefreshToken: fmt.Sprintf("rt-batch-quota-%d", index),
			Expired:      time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			AccountID:    fmt.Sprintf("account-batch-quota-%d", index),
			PlanType:     "plus",
		}
		channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
		if err != nil {
			t.Fatalf("create batch quota channel %d: %v", index, err)
		}
		channelIDs = append(channelIDs, channel.ID)
	}

	var active, maximum atomic.Int32
	started := make(chan struct{}, len(channelIDs))
	release := make(chan struct{})
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		if request.Header.Get("Authorization") == fmt.Sprintf("Bearer at-batch-quota-%d", len(channelIDs)-1) {
			return nil, errors.New("simulated quota failure")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1786163635}}}`,
			)),
			Request: request,
		}, nil
	})}
	server.codexCredentials = newCodexCredentialManager(
		codexauth.NewService(server.client), store,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)

	request := newJSONRequest(t, http.MethodPost, "/admin/channels/oauth-usage/batch/stream", oauthUsageBatchRequest{
		ChannelIDs: channelIDs,
	})
	c, response := newTestContext(t, request)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.HandleOAuthUsageBatchStream(c)
	}()

	for range oauthUsageBatchWorkers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("batch quota refresh did not start its worker pool")
		}
	}
	select {
	case <-started:
		t.Fatalf("batch quota refresh exceeded %d concurrent requests", oauthUsageBatchWorkers)
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("batch quota refresh did not complete")
	}

	if maximum.Load() != oauthUsageBatchWorkers {
		t.Fatalf("maximum batch quota concurrency = %d, want %d", maximum.Load(), oauthUsageBatchWorkers)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("batch quota status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("batch quota content type=%q", contentType)
	}

	events := make([]oauthUsageBatchEvent, 0, len(channelIDs)+2)
	for _, block := range strings.Split(response.Body.String(), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		_, data := parseSSEEventChunk([]byte(block + "\n\n"))
		var event oauthUsageBatchEvent
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatalf("decode batch quota SSE event: %v; block=%q", err, block)
		}
		events = append(events, event)
	}
	if len(events) != len(channelIDs)+2 || events[0].Event != "start" || events[len(events)-1].Event != "complete" {
		t.Fatalf("batch quota events=%#v", events)
	}
	seen := make(map[int64]bool, len(channelIDs))
	failed := 0
	for _, event := range events[1 : len(events)-1] {
		if event.Event != "progress" || event.Result == nil {
			t.Fatalf("batch quota progress event=%#v", event)
		}
		if event.Result.Status == "failed" {
			failed++
			if event.Result.Error == "" || event.Result.Usage != nil {
				t.Fatalf("batch quota failed event=%#v", event)
			}
		} else if event.Result.Status != "succeeded" || event.Result.Usage == nil || len(event.Result.Usage.Windows) != 1 {
			t.Fatalf("batch quota succeeded event=%#v", event)
		}
		seen[event.Result.ChannelID] = true
	}
	complete := events[len(events)-1]
	if len(seen) != len(channelIDs) || complete.Processed != len(channelIDs) ||
		complete.Succeeded != len(channelIDs)-1 || complete.Failed != 1 || failed != 1 {
		t.Fatalf("batch quota complete event=%#v seen=%v", complete, seen)
	}
}

func TestHandleOAuthUsageDoesNotOverwriteNewerSnapshotAfterCASConflict(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-quota-race", RefreshToken: "rt-quota-race",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-quota-race",
	}
	channel, _, err := createOrUpdateCodexChannel(context.Background(), store, credential)
	if err != nil {
		t.Fatal(err)
	}
	newerSnapshot, err := json.Marshal(persistedOAuthUsageSnapshot{
		RequestedAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		SampledAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		Summary: oauthUsageSummary{
			Provider: codexauth.ChannelType,
			Windows: []oauthUsageWindow{{
				LimitName: "codex", Kind: "primary", UsedPercent: 90, RemainingPercent: 10,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	winner := *credential
	winner.OAuthUsage = newerSnapshot
	winnerJSON, err := winner.JSON()
	if err != nil {
		t.Fatal(err)
	}
	raceStore := &concurrentOAuthWinnerStore{
		Store: store, authType: model.AuthTypeCodexOAuth, winnerJSON: winnerJSON,
	}
	server.store = raceStore
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800}}}`,
			)),
			Request: request,
		}, nil
	})}
	server.codexCredentials = newCodexCredentialManager(
		codexauth.NewService(server.client), raceStore,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)
	primeContext, primeResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(primeContext)
	prime := mustParseAPIResponse[[]ChannelWithCooldown](t, primeResponse.Body.Bytes())
	if len(prime.Data) != 1 || prime.Data[0].OAuthUsage != nil {
		t.Fatalf("unexpected usage before refresh: %+v", prime.Data)
	}

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)

	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
	if len(response.Data.Windows) != 1 || response.Data.Windows[0].UsedPercent != 90 {
		t.Fatalf("usage response regressed to stale snapshot: %+v", response.Data)
	}
	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil || len(list.Data[0].OAuthUsage.Windows) != 1 ||
		list.Data[0].OAuthUsage.Windows[0].UsedPercent != 90 {
		t.Fatalf("persisted usage regressed to stale snapshot: %+v", list.Data)
	}
}

func TestHandleOAuthUsageReturnsAnthropicQuotaAndSubscription(t *testing.T) {
	tests := []struct {
		name             string
		profileBody      string
		initialPlan      string
		initialTrial     string
		wantPlan         string
		wantSubscription string
		wantTrialEndsAt  string
		wantWarning      bool
	}{
		{
			name:             "pro",
			profileBody:      `{"account":{"uuid":"account-anthropic-quota","has_claude_pro":true,"has_claude_max":false},"organization":{"uuid":"org-1","organization_type":"claude_pro","rate_limit_tier":"default_claude_zero","claude_code_trial_ends_at":null}}`,
			wantPlan:         "Pro",
			wantSubscription: "default_claude_zero",
		},
		{
			name:             "max 20x",
			profileBody:      `{"account":{"has_claude_pro":false,"has_claude_max":true},"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x","claude_code_trial_ends_at":"2026-09-01T08:30:00+08:00"}}`,
			initialPlan:      "Pro",
			wantPlan:         "Max 20x",
			wantSubscription: "default_claude_max_20x",
			wantTrialEndsAt:  "2026-09-01T00:30:00Z",
		},
		{
			name:            "missing subscription metadata keeps persisted plan",
			profileBody:     `{}`,
			initialPlan:     "Pro",
			initialTrial:    "2026-08-31T00:00:00Z",
			wantPlan:        "Pro",
			wantTrialEndsAt: "2026-08-31T00:00:00Z",
			wantWarning:     true,
		},
		{
			name:         "unknown profile clears stale paid plan and ended trial",
			profileBody:  `{"account":{"uuid":"account-anthropic-quota"},"organization":{"uuid":"org-1","organization_type":"unknown","claude_code_trial_ends_at":null}}`,
			initialPlan:  "Max 20x",
			initialTrial: "2026-08-31T00:00:00Z",
			wantWarning:  true,
		},
		{
			name:        "organization type wins over billing fallback",
			profileBody: `{"subscription_type":"stripe_subscription_contracted","account":{"uuid":"account-anthropic-quota"},"organization":{"uuid":"org-1","organization_type":"claude_team"}}`,
			initialPlan: "Pro",
			wantPlan:    "Team",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, store, cleanup := setupAdminTestServer(t)
			defer cleanup()
			customBaseURL := "https://gateway.example/anthropic"
			expectedUsageURL := customBaseURL + "/api/oauth/usage"
			expectedProfileURL := customBaseURL + "/api/oauth/profile"
			server.configService = newStubConfigService(map[string]string{
				config.AnthropicBaseURLSettingKey: customBaseURL,
			})
			credential := &anthropicauth.Credential{
				Type: anthropicauth.ChannelType, AccessToken: "at-anthropic-quota-secret", RefreshToken: "rt-anthropic-quota-secret",
				Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountUUID: "account-anthropic-quota", EmailAddress: "quota@example.com",
				PlanType: test.initialPlan, ClaudeCodeTrialEndsAt: test.initialTrial,
			}
			channel, _, err := createOrUpdateAnthropicChannel(context.Background(), store, credential)
			if err != nil {
				t.Fatalf("createOrUpdateAnthropicChannel() error = %v", err)
			}

			requestCounts := make(map[string]int)
			server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
				requestCounts[request.URL.String()]++
				if request.Method != http.MethodGet {
					t.Errorf("usage request method = %s", request.Method)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer at-anthropic-quota-secret" {
					t.Errorf("Authorization = %q", got)
				}
				if got := request.Header.Get("User-Agent"); got != anthropicUsageUserAgent {
					t.Errorf("User-Agent = %q", got)
				}
				var responseBody string
				switch request.URL.String() {
				case expectedUsageURL:
					if got := request.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
						t.Errorf("anthropic-beta = %q", got)
					}
					responseBody = `{
						"five_hour":{"utilization":12.5,"resets_at":"2026-08-09T10:00:00Z"},
						"seven_day":{"utilization":40,"resets_at":"2026-08-15T10:00:00Z"},
						"seven_day_sonnet":{"utilization":25,"resets_at":"2026-08-15T11:00:00Z"},
						"seven_day_overage_included":{"utilization":75,"resets_at":"2026-08-15T12:00:00Z"}
					}`
				case expectedProfileURL:
					if got := request.Header.Get("Cache-Control"); got != "no-cache" {
						t.Errorf("Cache-Control = %q", got)
					}
					responseBody = test.profileBody
				default:
					t.Errorf("usage request URL = %s", request.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(responseBody)),
					Request:    request,
				}, nil
			})}
			server.anthropicCredentials = newAnthropicCredentialManager(
				anthropicauth.NewService(server.client), store,
				func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
			)

			path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
			c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
			server.HandleOAuthUsage(c)

			if w.Code != http.StatusOK {
				t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "at-anthropic-quota-secret") || strings.Contains(w.Body.String(), "rt-anthropic-quota-secret") {
				t.Fatalf("usage response leaked credential: %s", w.Body.String())
			}
			response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
			if response.Data.Provider != anthropicauth.ChannelType || response.Data.PlanType != test.wantPlan ||
				response.Data.SubscriptionTier != test.wantSubscription || len(response.Data.Windows) != 4 {
				t.Fatalf("usage summary = %#v", response.Data)
			}
			if test.wantWarning {
				if len(response.Data.Warnings) != 1 || response.Data.Warnings[0] != "Anthropic subscription metadata unavailable" {
					t.Fatalf("usage warnings = %v", response.Data.Warnings)
				}
			} else if len(response.Data.Warnings) != 0 {
				t.Fatalf("usage warnings = %v", response.Data.Warnings)
			}
			if requestCounts[expectedUsageURL] != 1 || requestCounts[expectedProfileURL] != 1 || len(requestCounts) != 2 {
				t.Fatalf("Anthropic request counts = %v", requestCounts)
			}
			persisted, err := store.GetConfig(context.Background(), channel.ID)
			if err != nil {
				t.Fatalf("get persisted Anthropic credential: %v", err)
			}
			persistedCredential, err := anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
			if err != nil || persistedCredential.PlanType != test.wantPlan ||
				persistedCredential.ClaudeCodeTrialEndsAt != test.wantTrialEndsAt {
				t.Fatalf("persisted Anthropic credential metadata = %+v, %v", persistedCredential, err)
			}
			listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
			server.HandleChannels(listContext)
			list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
			if len(list.Data) != 1 || list.Data[0].AnthropicPlanType != test.wantPlan ||
				list.Data[0].OAuthUsage == nil || list.Data[0].OAuthUsage.Provider != anthropicauth.ChannelType ||
				len(list.Data[0].OAuthUsage.Windows) != 4 {
				t.Fatalf("Anthropic channel list metadata = %+v", list.Data)
			}
			windows := response.Data.Windows
			if windows[0].Kind != "five_hour" || windows[0].UsedPercent != 12.5 || windows[0].RemainingPercent != 87.5 || windows[0].LimitWindowSeconds != 5*60*60 {
				t.Fatalf("five-hour window = %#v", windows[0])
			}
			if windows[2].LimitName != "Claude Sonnet" || windows[2].Kind != "seven_day_sonnet" || windows[2].RemainingPercent != 75 {
				t.Fatalf("Sonnet window = %#v", windows[2])
			}
			if windows[3].LimitName != "Claude Fable" || windows[3].Kind != "seven_day_fable" || windows[3].RemainingPercent != 25 {
				t.Fatalf("Fable window = %#v", windows[3])
			}
		})
	}
}

func TestHandleOAuthUsageReturnsRawCredentialRefreshResponse(t *testing.T) {
	const upstreamBody = "  {\"error\":\"invalid_grant\",\"error_description\":\"refresh token expired\"}\n"
	tests := []struct {
		name  string
		setup func(*testing.T, *Server) *model.Config
	}{
		{
			name: "Codex",
			setup: func(t *testing.T, server *Server) *model.Config {
				credential := &codexauth.Credential{
					Type: codexauth.ChannelType, AccessToken: "expired-codex-access", RefreshToken: "expired-codex-refresh",
					Expired: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), AccountID: "expired-codex-account",
				}
				channel, _, err := createOrUpdateCodexChannel(context.Background(), server.store, credential)
				if err != nil {
					t.Fatalf("create Codex channel: %v", err)
				}
				server.codexCredentials = newCodexCredentialManager(
					codexauth.NewService(server.client), server.store,
					func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
				)
				return channel
			},
		},
		{
			name: "Anthropic",
			setup: func(t *testing.T, server *Server) *model.Config {
				credential := &anthropicauth.Credential{
					Type: anthropicauth.ChannelType, AccessToken: "expired-anthropic-access", RefreshToken: "expired-anthropic-refresh",
					Expired: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), AccountUUID: "expired-anthropic-account",
				}
				channel, _, err := createOrUpdateAnthropicChannel(context.Background(), server.store, credential)
				if err != nil {
					t.Fatalf("create Anthropic channel: %v", err)
				}
				server.anthropicCredentials = newAnthropicCredentialManager(
					anthropicauth.NewService(server.client), server.store,
					func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
				)
				return channel
			},
		},
		{
			name: "Antigravity",
			setup: func(t *testing.T, server *Server) *model.Config {
				credential := &antigravityauth.Credential{
					Type: antigravityauth.ChannelType, AccessToken: "expired-antigravity-access", RefreshToken: "expired-antigravity-refresh",
					Expired: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), Email: "expired@example.com", ProjectID: "expired-project",
				}
				raw, err := credential.JSON()
				if err != nil {
					t.Fatalf("encode Antigravity credential: %v", err)
				}
				channel, err := server.store.CreateConfig(
					context.Background(), newAntigravityOAuthChannel("Antigravity-expired", raw),
				)
				if err != nil {
					t.Fatalf("create Antigravity channel: %v", err)
				}
				server.antigravityCredentials = newAntigravityCredentialManager(
					antigravityauth.NewService(server.client), server.store,
					func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
				)
				return channel
			},
		},
		{
			name: "xAI",
			setup: func(t *testing.T, server *Server) *model.Config {
				credential := &xaiauth.Credential{
					Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "expired-xai-access", RefreshToken: "expired-xai-refresh",
					Expired: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
				}
				raw, err := credential.JSON()
				if err != nil {
					t.Fatalf("encode xAI credential: %v", err)
				}
				channel, err := server.store.CreateConfig(context.Background(), &model.Config{
					Name: "xAI-expired", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw, Enabled: true,
					URLs: model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
				})
				if err != nil {
					t.Fatalf("create xAI channel: %v", err)
				}
				server.xaiCredentials = newXAICredentialManager(
					server.store, func(*model.Config) *http.Client { return server.client }, nil,
				)
				return channel
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _, cleanup := setupAdminTestServer(t)
			defer cleanup()
			server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost {
					t.Errorf("token request method = %s", request.Method)
				}
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(upstreamBody)),
					Request:    request,
				}, nil
			})}
			server.antigravityClient = server.client
			channel := test.setup(t, server)

			path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
			c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
			server.HandleOAuthUsage(c)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
			}
			response := mustParseAPIResponse[any](t, w.Body.Bytes())
			if response.Error != upstreamBody {
				t.Fatalf("usage error = %q, want raw upstream body %q", response.Error, upstreamBody)
			}
		})
	}
}

func TestAnthropicModelResponsePersistsPassiveQuotaInCredentialAndChannelList(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "passive-access", RefreshToken: "passive-refresh",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountUUID: "passive-account", PlanType: "Max 20x",
	}
	olderTime := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	activeSnapshot, err := json.Marshal(persistedOAuthUsageSnapshot{
		RequestedAt: olderTime,
		SampledAt:   olderTime,
		Summary: oauthUsageSummary{
			Provider: anthropicauth.ChannelType,
			Windows:  []oauthUsageWindow{{Kind: "five_hour", UsedPercent: 99, RemainingPercent: 1}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	credential.OAuthUsage = activeSnapshot
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("credential JSON: %v", err)
	}
	channel, err := store.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-passive", payload))
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	server.anthropicCredentials = newAnthropicCredentialManager(
		anthropicauth.NewService(server.client), store,
		func(cfg *model.Config) *http.Client { return server.getClientForChannel(cfg) }, nil,
	)
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	reset5h := time.Now().UTC().Add(-time.Minute).Unix()
	reset7d := time.Now().UTC().Add(7 * 24 * time.Hour).Unix()
	reset7dOI := time.Now().UTC().Add(7*24*time.Hour + time.Hour).Unix()
	response.Header.Set(anthropicRateLimit5hUtilization, "0.25")
	response.Header.Set(anthropicRateLimit5hReset, strconv.FormatInt(reset5h*1000, 10))
	response.Header.Set(anthropicRateLimit7dUtilization, "0.4")
	response.Header.Set(anthropicRateLimit7dReset, strconv.FormatInt(reset7d, 10))
	response.Header.Set(anthropicRateLimit7dOIUsage, "0.75")
	response.Header.Set(anthropicRateLimit7dOIReset, strconv.FormatInt(reset7dOI, 10))
	server.persistAnthropicPassiveUsage(context.Background(), channel, response)

	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("get persisted channel: %v", err)
	}
	persistedCredential, err := anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatalf("parse persisted credential: %v", err)
	}
	usage := persistedCredential.PassiveUsage
	if usage == nil || usage.FiveHour == nil || usage.FiveHour.Utilization == nil || *usage.FiveHour.Utilization != 0.25 ||
		usage.FiveHour.ResetAt == nil || *usage.FiveHour.ResetAt != reset5h || usage.SevenDay == nil ||
		usage.SevenDayOverageIncluded == nil || usage.SampledAt == "" {
		t.Fatalf("passive usage = %#v", usage)
	}
	newerValue := 0.99
	updated, err := server.anthropicCredentials.updatePassiveUsage(context.Background(), channel, anthropicPassiveUsageUpdate{
		FiveHour: &anthropicauth.PassiveUsageWindow{Utilization: &newerValue}, SampledAt: usage.SampledAt,
	})
	if err != nil || updated {
		t.Fatalf("equal timestamp passive update = %t, %v", updated, err)
	}
	persisted, err = store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err = anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || persistedCredential.PassiveUsage.FiveHour == nil ||
		persistedCredential.PassiveUsage.FiveHour.Utilization == nil || *persistedCredential.PassiveUsage.FiveHour.Utilization != 0.25 {
		t.Fatalf("equal timestamp overwrote passive usage: %#v, %v", persistedCredential.PassiveUsage, err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(c)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil || len(list.Data[0].OAuthUsage.Windows) != 3 {
		t.Fatalf("channel passive usage = %+v", list.Data)
	}
	windows := list.Data[0].OAuthUsage.Windows
	if windows[0].UsedPercent != 25 || windows[0].ResetAt != reset5h ||
		windows[2].Kind != "seven_day_fable" || windows[2].RemainingPercent != 25 {
		t.Fatalf("projected passive windows = %#v", windows)
	}

	nextWindow := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	nextWindow.Header.Set(anthropicRateLimit5hStatus, "allowed")
	nextWindow.Header.Set(anthropicRateLimit5hUtilization, "0.1")
	nextWindow.Header.Set(anthropicRateLimit5hReset, strconv.FormatInt(time.Now().UTC().Add(5*time.Hour).Unix(), 10))
	server.persistAnthropicPassiveUsage(context.Background(), channel, nextWindow)
	persisted, err = store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("get reset passive usage: %v", err)
	}
	persistedCredential, err = anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || persistedCredential.PassiveUsage.SevenDay != nil ||
		persistedCredential.PassiveUsage.SevenDayOverageIncluded != nil {
		t.Fatalf("passive usage after 5h window reset = %#v, %v", persistedCredential.PassiveUsage, err)
	}
}

func TestHandleOAuthUsageReturnsAntigravityQuotaWithoutLeakingCredential(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	const discoveredUserAgent = "antigravity/hub/9.8.7 darwin/arm64"
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-gravity-quota-secret", RefreshToken: "rt-gravity-quota-secret",
		Expired: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), Email: "quota@example.com", ProjectID: "forward-bonus-fjkxm",
		PaidTier: &antigravityauth.PaidTier{ID: "old-tier", Name: "Old Tier"},
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Antigravity credential JSON: %v", err)
	}
	channel, err := store.CreateConfig(context.Background(), newAntigravityOAuthChannel("Antigravity quota", payload))
	if err != nil {
		t.Fatalf("create Antigravity channel: %v", err)
	}

	var requestURLs []string
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestURLs = append(requestURLs, request.URL.String())
		if request.Method != http.MethodPost {
			t.Errorf("usage request method = %s", request.Method)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer at-gravity-quota-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		responseBody := ""
		switch request.URL.String() {
		case antigravityauth.DefaultDailyAPIBaseURL + "/v1internal:loadCodeAssist":
			if got := request.Header.Get("User-Agent"); got != discoveredUserAgent {
				t.Errorf("loadCodeAssist User-Agent = %q", got)
			}
			responseBody = `{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`
		case antigravityUsageURL:
			if got := request.Header.Get("User-Agent"); got != discoveredUserAgent {
				t.Errorf("quota User-Agent = %q", got)
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
			responseBody = `{
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
		default:
			t.Errorf("usage request URL = %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    request,
		}, nil
	})}
	server.antigravityClient = server.client
	server.antigravityService = antigravityauth.NewService(server.client)
	server.antigravityService.UserAgent = discoveredUserAgent
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
	if len(requestURLs) != 2 || requestURLs[0] != antigravityauth.DefaultDailyAPIBaseURL+"/v1internal:loadCodeAssist" || requestURLs[1] != antigravityUsageURL {
		t.Fatalf("Antigravity usage request order = %v", requestURLs)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := antigravityauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.PaidTier == nil || persistedCredential.PaidTier.DisplayName() != "Google AI Pro" {
		t.Fatalf("persisted paid tier = (%#v, %v)", persistedCredential, err)
	}
	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.HandleChannels(listContext)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, listResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil ||
		list.Data[0].OAuthUsage.Provider != antigravityauth.ChannelType || len(list.Data[0].OAuthUsage.Windows) != 3 {
		t.Fatalf("persisted Antigravity usage = %+v", list.Data)
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

func TestAnthropicOAuthManagerValidatesCombinedCodeStateAndCreatesChannel(t *testing.T) {
	var exchangedState string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode token request: %v", err)
		}
		exchangedState = payload["state"]
		_, _ = io.WriteString(w, `{"access_token":"anthropic-access","refresh_token":"anthropic-refresh","token_type":"Bearer","expires_in":3600,"scope":"user:inference","organization":{"uuid":"org-1"},"account":{"uuid":"account-1","email_address":"user@example.com"}}`)
	}))
	defer tokenServer.Close()
	service := anthropicauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	manager := newAnthropicOAuthManager(service, store, nil)
	defer manager.close()

	_, state, err := manager.start()
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if _, err := manager.submitAuthorizationCode(state, "code-1#wrong-state"); err == nil {
		t.Fatal("state mismatch was accepted")
	}
	if _, err := manager.submitAuthorizationCode(state, "code-1#"+state); err != nil {
		t.Fatalf("submitAuthorizationCode() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, ok := manager.status(state)
		if !ok {
			t.Fatal("OAuth session disappeared")
		}
		if status.Status == "complete" {
			if status.ChannelID == 0 || exchangedState != state {
				t.Fatalf("status=%+v exchanged state=%q", status, exchangedState)
			}
			channel, getErr := store.GetConfig(context.Background(), status.ChannelID)
			if getErr != nil || !channel.UsesAnthropicOAuth() || len(channel.ModelEntries) != len(anthropicOAuthDefaultModels) {
				t.Fatalf("created channel=%+v err=%v", channel, getErr)
			}
			break
		}
		if status.Status == "error" {
			t.Fatalf("OAuth failed: %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("OAuth did not complete: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleAnthropicCookieAuthCreatesChannelWithoutReturningOrPersistingCookie(t *testing.T) {
	const sessionKey = "sk-ant-sid01-handler-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/organizations":
			cookie, err := request.Cookie("sessionKey")
			if err != nil || cookie.Value != sessionKey {
				t.Errorf("organization cookie = %v, err = %v", cookie, err)
			}
			_, _ = io.WriteString(w, `[{"uuid":"cookie-org"}]`)
		case "/v1/oauth/cookie-org/authorize":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode authorization request: %v", err)
			}
			redirect := anthropicauth.RedirectURI + "?code=cookie-code&state=" + url.QueryEscape(payload["state"])
			_ = json.NewEncoder(w).Encode(map[string]string{"redirect_uri": redirect})
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"cookie-access-secret","refresh_token":"cookie-refresh-secret","token_type":"Bearer","expires_in":3600,"scope":"user:inference","organization":{"uuid":"cookie-org"},"account":{"uuid":"cookie-account","email_address":"cookie@example.com"}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer upstream.Close()
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	service := anthropicauth.NewService(upstream.Client())
	service.ClaudeWebURL = upstream.URL
	service.TokenURL = upstream.URL + "/token"
	server.anthropicService = service

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/anthropic/oauth/cookie", map[string]string{
		"session_key": sessionKey,
	}))
	server.HandleAnthropicCookieAuth(c)

	if w.Code != http.StatusOK {
		t.Fatalf("cookie auth status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), sessionKey) || strings.Contains(w.Body.String(), "cookie-access-secret") ||
		strings.Contains(w.Body.String(), "cookie-refresh-secret") {
		t.Fatalf("cookie auth response leaked credentials: %s", w.Body.String())
	}
	var response APIResponse[struct {
		Status    string `json:"status"`
		ChannelID int64  `json:"channel_id"`
		Created   bool   `json:"created"`
	}]
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	if !response.Success || response.Data.Status != "complete" || !response.Data.Created || response.Data.ChannelID == 0 {
		t.Fatalf("cookie auth response = %+v", response)
	}
	channel, err := store.GetConfig(context.Background(), response.Data.ChannelID)
	if err != nil {
		t.Fatalf("get cookie channel: %v", err)
	}
	if !channel.UsesAnthropicOAuth() || strings.Contains(channel.OAuthCredential, sessionKey) {
		t.Fatalf("cookie channel persisted sessionKey: %+v", channel)
	}
	credential, err := anthropicauth.ParseCredential([]byte(channel.OAuthCredential))
	if err != nil || credential.AccessToken != "cookie-access-secret" || credential.RefreshToken != "cookie-refresh-secret" {
		t.Fatalf("stored cookie credential = %+v, err = %v", credential, err)
	}
}

func TestHandleAnthropicCookieAuthReturnsSanitizedUpstreamErrors(t *testing.T) {
	const sessionKey = "sk-ant-sid01-a/b+c="
	var mixedEncodedSecret strings.Builder
	var percentEncodedSecret strings.Builder
	for _, char := range sessionKey {
		_, _ = fmt.Fprintf(&mixedEncodedSecret, "%%5Cu%04x", char)
	}
	for index := range len(sessionKey) {
		_, _ = fmt.Fprintf(&percentEncodedSecret, "%%%02X", sessionKey[index])
	}
	tests := []struct {
		name            string
		failurePath     string
		statusCode      int
		message         string
		reflectedSecret string
		rawFailureBody  string
		expectRedacted  bool
	}{
		{name: "organization", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "organization authorization denied"},
		{name: "authorization", failurePath: "/v1/oauth/cookie-org/authorize", statusCode: http.StatusForbidden, message: "organization cannot use this OAuth client"},
		{name: "token", failurePath: "/token", statusCode: http.StatusBadRequest, message: "authorization code expired"},
		{name: "query-encoded secret", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "reflected credential", reflectedSecret: url.QueryEscape(sessionKey), expectRedacted: true},
		{name: "path-encoded secret", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "reflected credential", reflectedSecret: "sk-ant-sid01-a%2Fb+c=", expectRedacted: true},
		{name: "HTML-encoded secret", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "reflected credential", reflectedSecret: "sk-ant-sid01-a&#47;b&#43;c&#61;", expectRedacted: true},
		{name: "duplicate JSON key", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, rawFailureBody: `{"session_key":"sk-ant-sid01-a\/b\u002bc=","session_key":"safe"}`, expectRedacted: true},
		{name: "multiple JSON values", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, rawFailureBody: "{\"error\":\"safe\"}\n{\"session_key\":\"sk-ant-sid01-a\\/b\\u002bc=\"}", expectRedacted: true},
		{name: "nested JSON escape", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, rawFailureBody: `{"error":"sk-ant-sid01-a\\u002fb\\u002bc="}`, expectRedacted: true},
		{name: "mixed URL and JSON escapes", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "reflected credential", reflectedSecret: mixedEncodedSecret.String(), expectRedacted: true},
		{name: "benign nested JSON escape", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "literal", rawFailureBody: `{"error":"literal \"quoted\" \\u1234"}`},
		{name: "benign Windows path", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "path C", rawFailureBody: `{"error":"path C:\\users\\name"}`},
		{name: "invalid JSON escape before secret", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, rawFailureBody: `{"error":"path C:\\users\\name sk-ant-sid01-a\\u002fb\\u002bc="}`, expectRedacted: true},
		{name: "invalid percent escape before secret", failurePath: "/api/organizations", statusCode: http.StatusUnauthorized, message: "reflected credential", reflectedSecret: "bad%ZZ" + percentEncodedSecret.String(), expectRedacted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == test.failurePath {
					w.WriteHeader(test.statusCode)
					if test.rawFailureBody != "" {
						_, _ = io.WriteString(w, test.rawFailureBody)
						return
					}
					payload := map[string]string{"error": test.message}
					if test.reflectedSecret != "" {
						payload["session_key"] = test.reflectedSecret
					}
					_ = json.NewEncoder(w).Encode(payload)
					return
				}
				switch request.URL.Path {
				case "/api/organizations":
					_, _ = io.WriteString(w, `[{"uuid":"cookie-org"}]`)
				case "/v1/oauth/cookie-org/authorize":
					var payload map[string]string
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Fatalf("decode authorization request: %v", err)
					}
					redirect := anthropicauth.RedirectURI + "?code=cookie-code&state=" + url.QueryEscape(payload["state"])
					_ = json.NewEncoder(w).Encode(map[string]string{"redirect_uri": redirect})
				default:
					http.NotFound(w, request)
				}
			}))
			defer upstream.Close()

			server, _, cleanup := setupAdminTestServer(t)
			defer cleanup()
			service := anthropicauth.NewService(upstream.Client())
			service.ClaudeWebURL = upstream.URL
			service.TokenURL = upstream.URL + "/token"
			server.anthropicService = service

			c, recorder := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/anthropic/oauth/cookie", map[string]string{
				"session_key": sessionKey,
			}))
			server.HandleAnthropicCookieAuth(c)

			body := recorder.Body.String()
			if recorder.Code != http.StatusBadGateway || strings.Contains(body, sessionKey) ||
				strings.Contains(body, url.QueryEscape(sessionKey)) ||
				!strings.Contains(body, fmt.Sprintf("returned HTTP %d", test.statusCode)) {
				t.Fatalf("cookie auth error status=%d body=%s", recorder.Code, body)
			}
			if test.expectRedacted != strings.Contains(body, "[REDACTED]") ||
				(!test.expectRedacted && !strings.Contains(body, test.message)) {
				t.Fatalf("cookie auth error status=%d body=%s", recorder.Code, body)
			}
		})
	}
}

func TestSameAnthropicIdentityNeverUsesSharedOrganization(t *testing.T) {
	first := &anthropicauth.Credential{OrgUUID: "shared-org"}
	second := &anthropicauth.Credential{OrgUUID: "shared-org"}
	if sameAnthropicIdentity(first, second) {
		t.Fatal("organization UUID is not an account identity")
	}
	first.AccountUUID, second.AccountUUID = "account-1", "account-1"
	if !sameAnthropicIdentity(first, second) {
		t.Fatal("matching account UUID should identify the same account")
	}
}

func TestAnthropicCredentialManagerPersistsRotatedRefreshToken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode refresh request: %v", err)
		}
		if payload["refresh_token"] != "old-refresh" {
			t.Errorf("refresh token = %q", payload["refresh_token"])
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	service := anthropicauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired:     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		AccountUUID: "account-1", EmailAddress: "user@example.com",
	}
	raw, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-test", raw))
	if err != nil {
		t.Fatal(err)
	}
	manager := newAnthropicCredentialManager(service, store, nil, nil)
	refreshed, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshed credential = %+v", refreshed)
	}
	persisted, err := store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || winner.RefreshToken != "rotated-refresh" || winner.AccountUUID != "account-1" {
		t.Fatalf("persisted credential=%+v err=%v", winner, err)
	}
}

func TestHandleRefreshAnthropicCredentialReturnsUpstreamErrorDetails(t *testing.T) {
	const upstreamBody = `{"error":"invalid_grant","error_description":"refresh token expired"}`
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode refresh request: %v", err)
		}
		if payload["refresh_token"] != "expired-refresh" {
			t.Errorf("refresh token = %q", payload["refresh_token"])
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer tokenServer.Close()

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "old-access", RefreshToken: "expired-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), AccountUUID: "account-1",
	}
	raw, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-expired", raw))
	if err != nil {
		t.Fatal(err)
	}
	service := anthropicauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	server.anthropicCredentials = newAnthropicCredentialManager(service, store, nil, nil)

	request := newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/anthropic-credential/refresh", channel.ID), nil)
	c, recorder := newTestContext(t, request)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(channel.ID, 10)}}
	server.HandleRefreshAnthropicCredential(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("refresh status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := mustParseAPIResponse[any](t, recorder.Body.Bytes())
	want := fmt.Sprintf("refresh Anthropic credential for channel %d: anthropic token endpoint returned HTTP 400: %s", channel.ID, upstreamBody)
	if response.Success || response.Error != want {
		t.Fatalf("refresh response=%+v, want error %q", response, want)
	}
}

func TestAnthropicCredentialManagerConsumesConcurrentCASWinnerAfterInvalidGrant(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	oldCredential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), AccountUUID: "account-1",
	}
	oldRaw, err := oldCredential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-race", oldRaw))
	if err != nil {
		t.Fatal(err)
	}
	winnerCredential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "winner-access", RefreshToken: "winner-refresh",
		Expired: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339), AccountUUID: "account-1",
	}
	winnerRaw, err := winnerCredential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		updated, updateErr := store.CompareAndSwapOAuthCredential(
			context.Background(), channel.ID, model.AuthTypeAnthropicOAuth, oldRaw, winnerRaw,
		)
		if updateErr != nil || !updated {
			t.Errorf("persist concurrent winner: updated=%v err=%v", updated, updateErr)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer tokenServer.Close()
	service := anthropicauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	manager := newAnthropicCredentialManager(service, store, nil, nil)

	winner, err := manager.credential(context.Background(), channel, true)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if winner.AccessToken != "winner-access" || winner.RefreshToken != "winner-refresh" {
		t.Fatalf("credential() winner = %+v", winner)
	}
}

func TestAnthropicCredentialManagerMergesRepeatedMetadataWinnersWithoutRefreshingTwice(t *testing.T) {
	_, baseStore, cleanup := setupAdminTestServer(t)
	defer cleanup()
	oldCredential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), AccountUUID: "account-1",
	}
	oldRaw, err := oldCredential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := baseStore.CreateConfig(context.Background(), newAnthropicOAuthChannel("Anthropic-metadata-race", oldRaw))
	if err != nil {
		t.Fatal(err)
	}
	store := &anthropicMetadataChurnStore{Store: baseStore}
	store.remaining.Store(5)
	var refreshCount atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if refreshCount.Add(1) != 1 {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	service := anthropicauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	manager := newAnthropicCredentialManager(service, store, nil, nil)

	refreshed, err := manager.credential(context.Background(), channel, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if refreshCount.Load() != 1 || refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "rotated-refresh" ||
		refreshed.PlanType != "Concurrent 5" {
		t.Fatalf("refreshed credential = %+v; refresh count = %d", refreshed, refreshCount.Load())
	}
	persisted, err := baseStore.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := anthropicauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || winner.RefreshToken != "rotated-refresh" || winner.PlanType != "Concurrent 5" {
		t.Fatalf("persisted credential = %+v, %v", winner, err)
	}
}
