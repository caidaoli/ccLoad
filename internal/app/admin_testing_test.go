package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const antigravityCapacityBodyForAdminTest = `{"error":{"code":503,"message":"No capacity available for model gemini-3-flash on the server","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"error_number":"2010","model":"gemini-3-flash"}}]}}`

func createCodexOAuthChannelForAdminTest(t testing.TB, srv *Server, upstreamURL string) *model.Config {
	t.Helper()
	credential := &codexauth.Credential{
		Type:         "codex",
		AccessToken:  "at-admin-test",
		RefreshToken: "rt-admin-test",
		Expired:      time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339),
		AccountID:    "account-admin-test",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("encode Codex credential: %v", err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name:                  "codex-oauth-admin-test",
		AuthType:              model.AuthTypeCodexOAuth,
		OAuthCredential:       payload,
		URLs:                  model.ChannelURLs{{URL: upstreamURL, Exact: true, Protocols: []string{util.ProtocolCodex}}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig Codex OAuth channel: %v", err)
	}
	return created
}

func createAntigravityOAuthChannelForAdminTest(t testing.TB, srv *Server, upstreamURL string) *model.Config {
	t.Helper()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: "at-gravity-admin", RefreshToken: "rt-gravity-admin",
		Expired: time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339),
		Email:   "gravity-admin@example.com", ProjectID: "gravity-admin-project",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "antigravity-oauth-admin-test", AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: payload,
		URLs:                  model.ChannelURLs{{URL: upstreamURL, Protocols: []string{util.ProtocolGemini}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "gemini-3-flash"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func createXAIOAuthChannelForAdminTest(t testing.TB, srv *Server, upstreamURL string) *model.Config {
	t.Helper()
	credential := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "at-xai-admin", RefreshToken: "rt-xai-admin",
		TokenType: "Bearer", Expired: time.Now().UTC().Add(6 * time.Hour).Format(time.RFC3339),
		TokenEndpoint: xaiauth.TokenURL, BaseURL: xaiauth.CLIBaseURL,
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("encode xAI credential: %v", err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-oauth-admin-test", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: payload,
		URLs:                  model.ChannelURLs{{URL: upstreamURL, Protocols: []string{util.ProtocolCodex}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "grok-4.5"}}, Enabled: true, Websockets: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig xAI OAuth channel: %v", err)
	}
	return created
}

func replaceXAIOAuthCredentialForAdminTest(
	t testing.TB,
	srv *Server,
	cfg *model.Config,
	accessToken, refreshToken string,
	expiresAt time.Time,
) *model.Config {
	t.Helper()
	credential := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: accessToken, RefreshToken: refreshToken,
		TokenType: "Bearer", Expired: expiresAt.UTC().Format(time.RFC3339),
		TokenEndpoint: xaiauth.TokenURL, BaseURL: xaiauth.CLIBaseURL,
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("encode replacement xAI credential: %v", err)
	}
	updated, err := srv.store.CompareAndSwapOAuthCredential(
		context.Background(), cfg.ID, model.AuthTypeXAIOAuth, cfg.OAuthCredential, payload,
	)
	if err != nil || !updated {
		t.Fatalf("replace xAI credential: updated=%v err=%v", updated, err)
	}
	reloaded, err := srv.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("reload replaced xAI credential: %v", err)
	}
	return reloaded
}

func createAnthropicOAuthChannelForAdminTest(t testing.TB, srv *Server, upstreamURL string) *model.Config {
	t.Helper()
	payload, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "at-anthropic-admin", RefreshToken: "rt-anthropic-admin",
		Expired: time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339), AccountUUID: "anthropic-admin-account",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "anthropic-oauth-admin-test", AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: payload,
		URLs:                  model.ChannelURLs{{URL: upstreamURL, Protocols: []string{util.ProtocolAnthropic}}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "claude-sonnet-4-5"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func createCursorOAuthChannelForAdminTest(t testing.TB, srv *Server, upstreamURL string) *model.Config {
	t.Helper()
	payload, err := (&cursorauth.Credential{
		AccessToken: "tok", APIKey: "cursor-user-key", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name:                  "cursor-oauth-admin-test",
		AuthType:              model.AuthTypeCursorOAuth,
		OAuthCredential:       payload,
		URLs:                  model.ChannelURLs{{URL: upstreamURL, Protocols: []string{util.ProtocolAnthropic, util.ProtocolOpenAI}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "grok-4.6"}, {Model: "composer-2.5"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestOAuthCredentialCleanupQueueUsesPriorityThenChannelName(t *testing.T) {
	arrivals := make(chan string, 10)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals <- strings.TrimPrefix(r.URL.Path, "/")
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-cleanup-order\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	type channelSpec struct {
		name     string
		priority int
	}
	// Deliberately insert in the opposite of the required queue order so this
	// test observes the public cleanup behavior instead of storage row order.
	specs := []channelSpec{
		{name: "priority-low", priority: 10},
		{name: "name-z", priority: 20},
		{name: "name-y", priority: 20},
		{name: "priority-high-z", priority: 30},
		{name: "name-x", priority: 20},
		{name: "name-w", priority: 20},
		{name: "name-v", priority: 20},
		{name: "name-b", priority: 20},
		{name: "name-a", priority: 20},
		{name: "priority-high-a", priority: 30},
	}
	for _, spec := range specs {
		credential, err := (&codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: "at-" + spec.name,
			RefreshToken: "rt-" + spec.name, Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			AccountID: "account-" + spec.name,
		}).JSON()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateConfig(context.Background(), &model.Config{
			Name: spec.name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
			URLs: model.ChannelURLs{{
				URL: upstream.URL + "/" + spec.name, Exact: true, Protocols: []string{util.ProtocolCodex},
			}},
			ProtocolTransformMode: model.ProtocolTransformModeUpstream,
			ModelEntries:          []model.ModelEntry{{Model: "gpt-order"}},
			Priority:              spec.priority,
			Enabled:               true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	req := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-order"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	c, w := newTestContext(t, req)
	srv.HandleStartOAuthCredentialCleanupJob(c)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start cleanup status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Success bool                           `json:"success"`
		Data    oauthCredentialCleanupJobStart `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.Total != len(specs) {
		t.Fatalf("cleanup start=%+v", response)
	}

	firstWave := make(map[string]struct{}, oauthCredentialCleanupWorkers)
	deadline := time.After(5 * time.Second)
	for len(firstWave) < oauthCredentialCleanupWorkers {
		select {
		case name := <-arrivals:
			firstWave[name] = struct{}{}
		case <-deadline:
			t.Fatalf("first cleanup wave=%v", firstWave)
		}
	}
	wantFirstWave := map[string]struct{}{
		"priority-high-a": {},
		"priority-high-z": {},
		"name-a":          {},
		"name-b":          {},
		"name-v":          {},
		"name-w":          {},
		"name-x":          {},
		"name-y":          {},
	}
	if !reflect.DeepEqual(firstWave, wantFirstWave) {
		t.Fatalf("first cleanup wave=%v, want %v", firstWave, wantFirstWave)
	}
	select {
	case name := <-arrivals:
		t.Fatalf("channel %q started before a higher-ranked worker slot completed", name)
	case <-time.After(100 * time.Millisecond):
	}

	releaseAll()
	var view oauthCredentialCleanupJobView
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var err error
		view, _, err = srv.oauthCredentialCleanupJobs.Get(response.Data.JobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != oauthCredentialCleanupJobSucceeded {
		t.Fatalf("cleanup status=%q error=%q", view.Status, view.Error)
	}
}

func TestOAuthCredentialCleanupRunsConcurrentlyAndDeletesOnlyRefreshFailures(t *testing.T) {
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var healthyArrivals atomic.Int32
	releaseHealthy := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reject", "/reject-empty-401", "/transient", "/refreshed-reject":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
			return
		case "/refresh-other":
			if !strings.Contains(r.Header.Get("Authorization"), "at-cleanup-refresh-other-next") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
				return
			}
			fallthrough
		case "/healthy-1", "/healthy-2", "/healthy-expired":
			current := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				seen := maxInFlight.Load()
				if current <= seen || maxInFlight.CompareAndSwap(seen, current) {
					break
				}
			}
			if healthyArrivals.Add(1) == 2 {
				close(releaseHealthy)
			}
			select {
			case <-releaseHealthy:
			case <-time.After(time.Second):
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-cleanup\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			return
		default:
			http.NotFound(w, r)
		}
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	var refreshAttempts atomic.Int32
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshAttempts.Add(1)
		requestBody, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(requestBody))
		statusCode := http.StatusBadRequest
		responseBody := `{"error":"invalid_grant"}`
		switch form.Get("refresh_token") {
		case "rt-cleanup-rejected-empty-401":
			statusCode = http.StatusUnauthorized
			responseBody = ""
		case "rt-cleanup-transient":
			statusCode = http.StatusServiceUnavailable
			responseBody = `{"error":"temporarily_unavailable"}`
		case "rt-cleanup-expired-refreshed-rejected":
			statusCode = http.StatusOK
			responseBody = `{"access_token":"at-cleanup-expired-refreshed-rejected-next","refresh_token":"rt-cleanup-rotated","expires_in":3600}`
		case "rt-cleanup-refresh-other":
			statusCode = http.StatusOK
			responseBody = `{"access_token":"at-cleanup-refresh-other-next","refresh_token":"rt-cleanup-refresh-other-next","expires_in":3600}`
		}
		return &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})}
	refreshService := codexauth.NewService(refreshClient)
	refreshService.TokenURL = "https://oauth.test/token"
	srv.codexCredentials.service = refreshService
	srv.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	createChannel := func(name, endpoint string, models []model.ModelEntry, scheduledModel string) *model.Config {
		t.Helper()
		expiresAt := time.Now().UTC().Add(24 * time.Hour)
		if strings.Contains(name, "expired") {
			expiresAt = time.Now().UTC().Add(-time.Hour)
		}
		credential, err := (&codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: "at-" + name, RefreshToken: "rt-" + name,
			Expired: expiresAt.Format(time.RFC3339), AccountID: "account-" + name,
		}).JSON()
		if err != nil {
			t.Fatal(err)
		}
		created, err := srv.store.CreateConfig(context.Background(), &model.Config{
			Name: name, AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
			URLs:                  model.ChannelURLs{{URL: endpoint, Exact: true, Protocols: []string{util.ProtocolCodex}}},
			ProtocolTransformMode: model.ProtocolTransformModeUpstream,
			ModelEntries:          models,
			ScheduledCheckModel:   scheduledModel,
			Enabled:               true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}

	rejected := createChannel("cleanup-rejected", upstream.URL+"/reject", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	rejectedEmpty401 := createChannel("cleanup-rejected-empty-401", upstream.URL+"/reject-empty-401", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	transientRefresh := createChannel("cleanup-transient", upstream.URL+"/transient", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	healthyOne := createChannel("cleanup-healthy-1", upstream.URL+"/healthy-1", []model.ModelEntry{{Model: "gpt-5.4"}, {Model: "gpt-preferred"}}, "gpt-preferred")
	createChannel("cleanup-healthy-2", upstream.URL+"/healthy-2", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	expiredButUsable := createChannel("cleanup-healthy-expired", upstream.URL+"/healthy-expired", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	refreshedRejected := createChannel("cleanup-expired-refreshed-rejected", upstream.URL+"/refreshed-reject", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	networkFailure := createChannel("cleanup-network", "http://127.0.0.1:1/v1/responses", []model.ModelEntry{{Model: "gpt-5.4"}}, "")
	excludedModel := createChannel("cleanup-other-model", upstream.URL+"/healthy-2", []model.ModelEntry{{Model: "gpt-other"}}, "")
	refreshedOtherModel := createChannel("cleanup-refresh-other", upstream.URL+"/refresh-other", []model.ModelEntry{{Model: "gpt-other"}}, "")
	optionsRequest := newRequest(http.MethodGet, "/admin/oauth/credentials/cleanup/options?auth_type=codex_oauth", nil)
	optionsContext, optionsResponse := newTestContext(t, optionsRequest)
	srv.HandleOAuthCredentialCleanupOptions(optionsContext)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("cleanup options status=%d body=%s", optionsResponse.Code, optionsResponse.Body.String())
	}
	var optionsPayload struct {
		Success bool                          `json:"success"`
		Data    oauthCredentialCleanupOptions `json:"data"`
	}
	if err := json.Unmarshal(optionsResponse.Body.Bytes(), &optionsPayload); err != nil {
		t.Fatalf("decode cleanup options: %v", err)
	}
	if !optionsPayload.Success || optionsPayload.Data.AuthType != model.AuthTypeCodexOAuth ||
		optionsPayload.Data.ChannelCount != 10 ||
		!reflect.DeepEqual(optionsPayload.Data.Models, []string{"gpt-5.4", "gpt-other", "gpt-preferred"}) {
		t.Fatalf("cleanup options=%+v", optionsPayload)
	}

	startCleanup := func() oauthCredentialCleanupJobStart {
		t.Helper()
		req := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
			`{"auth_type":"codex_oauth","model":"gpt-5.4","action":"delete"}`,
		))
		req.Header.Set("Idempotency-Key", "cleanup-test-request")
		req.Header.Set("Content-Type", "application/json")
		c, w := newTestContext(t, req)
		srv.HandleStartOAuthCredentialCleanupJob(c)
		if w.Code != http.StatusAccepted {
			t.Fatalf("start cleanup status=%d body=%s", w.Code, w.Body.String())
		}
		var response struct {
			Success bool                           `json:"success"`
			Data    oauthCredentialCleanupJobStart `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode cleanup start: %v", err)
		}
		if !response.Success || response.Data.JobID == "" {
			t.Fatalf("invalid cleanup start response: %s", w.Body.String())
		}
		return response.Data
	}
	started := startCleanup()
	if started.Total != 10 || started.AuthType != model.AuthTypeCodexOAuth || started.Model != "gpt-5.4" ||
		started.Action != oauthCredentialCleanupActionDelete {
		t.Fatalf("cleanup selection=%+v", started)
	}
	recovered := startCleanup()
	if recovered != started {
		t.Fatalf("idempotent start=%+v, want %+v", recovered, started)
	}
	actionConflictReq := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-5.4","action":"disable"}`,
	))
	actionConflictReq.Header.Set("Idempotency-Key", "cleanup-test-request")
	actionConflictReq.Header.Set("Content-Type", "application/json")
	actionConflictContext, actionConflictResponse := newTestContext(t, actionConflictReq)
	srv.HandleStartOAuthCredentialCleanupJob(actionConflictContext)
	if actionConflictResponse.Code != http.StatusConflict {
		t.Fatalf("idempotency action conflict status=%d body=%s", actionConflictResponse.Code, actionConflictResponse.Body.String())
	}
	conflictReq := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-other","action":"delete"}`,
	))
	conflictReq.Header.Set("Idempotency-Key", "cleanup-test-request")
	conflictReq.Header.Set("Content-Type", "application/json")
	conflictContext, conflictResponse := newTestContext(t, conflictReq)
	srv.HandleStartOAuthCredentialCleanupJob(conflictContext)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}

	var view oauthCredentialCleanupJobView
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		view, _, err = srv.oauthCredentialCleanupJobs.Get(started.JobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != oauthCredentialCleanupJobSucceeded {
		t.Fatalf("cleanup status=%q error=%q events=%+v", view.Status, view.Error, view.Events)
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("healthy channel tests were not concurrent: max_in_flight=%d", maxInFlight.Load())
	}
	if refreshAttempts.Load() != 7 {
		t.Fatalf("refresh attempts=%d, want 7", refreshAttempts.Load())
	}
	if _, err := srv.store.GetConfig(context.Background(), rejected.ID); err == nil {
		t.Fatal("channel whose 401 refresh failed was not deleted")
	}
	if _, err := srv.store.GetConfig(context.Background(), rejectedEmpty401.ID); err == nil {
		t.Fatal("channel whose refresh endpoint returned an empty 401 was not deleted")
	}
	if _, err := srv.store.GetConfig(context.Background(), networkFailure.ID); err != nil {
		t.Fatalf("network failure must not delete its channel: %v", err)
	}
	if _, err := srv.store.GetConfig(context.Background(), transientRefresh.ID); err != nil {
		t.Fatalf("transient refresh failure must not delete its channel: %v", err)
	}
	if _, err := srv.store.GetConfig(context.Background(), expiredButUsable.ID); err != nil {
		t.Fatalf("an expired credential whose existing access token still works must be kept: %v", err)
	}
	if _, err := srv.store.GetConfig(context.Background(), refreshedRejected.ID); err == nil {
		t.Fatal("channel tested with an eagerly refreshed credential was not deleted after that refresh token was rejected")
	}
	if _, err := srv.store.GetConfig(context.Background(), excludedModel.ID); err != nil {
		t.Fatalf("all channels of the selected auth type must be tested: %v", err)
	}

	var complete oauthCredentialCleanupEvent
	foundPreferredModel := false
	foundNonSupportingChannel := false
	foundUnsupportedRetest := false
	for _, event := range view.Events {
		if event.Event == "testing" && event.ChannelID == healthyOne.ID {
			foundPreferredModel = event.Model == "gpt-5.4" && len(event.Models) == 2
		}
		if event.Event == "testing" && event.ChannelID == excludedModel.ID {
			foundNonSupportingChannel = event.Model == "gpt-5.4"
		}
		if event.Event == "retesting" && event.ChannelID == refreshedOtherModel.ID {
			foundUnsupportedRetest = event.Model == "gpt-5.4"
		}
		if event.Event == "complete" {
			complete = event
		}
	}
	if !foundPreferredModel {
		t.Fatal("cleanup did not list supported models and select the configured test model")
	}
	if !foundNonSupportingChannel {
		t.Fatal("cleanup did not test a same-auth-type channel lacking the selected model")
	}
	if !foundUnsupportedRetest {
		t.Fatal("cleanup did not retest the selected model after refreshing a channel that lacks it")
	}
	if complete.Deleted != 3 || complete.Healthy != 4 || complete.Refreshed != 1 || complete.Failed != 2 || complete.Processed != 10 {
		t.Fatalf("cleanup summary=%+v", complete)
	}

	resumeAfter := complete.Sequence - 1
	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/oauth/credentials/cleanup/jobs/%s/stream?after=%d", started.JobID, resumeAfter), nil))
	c.Params = gin.Params{{Key: "id", Value: started.JobID}}
	srv.HandleOAuthCredentialCleanupStream(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"event":"complete"`) || strings.Contains(w.Body.String(), `"event":"start"`) {
		t.Fatalf("resumed SSE response status=%d body=%s", w.Code, w.Body.String())
	}

	staleRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(fmt.Sprintf(
		`{"auth_type":"codex_oauth","channel_id":%d,"model":"gpt-other"}`, healthyOne.ID,
	)))
	staleRequest.Header.Set("Content-Type", "application/json")
	staleContext, staleResponse := newTestContext(t, staleRequest)
	srv.HandleStartOAuthCredentialCleanupJob(staleContext)
	if staleResponse.Code != http.StatusBadRequest {
		t.Fatalf("stale channel-scoped cleanup status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	unknownModelRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-not-configured"}`,
	))
	unknownModelRequest.Header.Set("Content-Type", "application/json")
	unknownModelContext, unknownModelResponse := newTestContext(t, unknownModelRequest)
	srv.HandleStartOAuthCredentialCleanupJob(unknownModelContext)
	if unknownModelResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown cleanup model status=%d body=%s", unknownModelResponse.Code, unknownModelResponse.Body.String())
	}
	invalidActionRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-other","action":"archive"}`,
	))
	invalidActionRequest.Header.Set("Content-Type", "application/json")
	invalidActionContext, invalidActionResponse := newTestContext(t, invalidActionRequest)
	srv.HandleStartOAuthCredentialCleanupJob(invalidActionContext)
	if invalidActionResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid cleanup action status=%d body=%s", invalidActionResponse.Code, invalidActionResponse.Body.String())
	}

	secondRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-other"}`,
	))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRequest.Header.Set("Idempotency-Key", "cleanup-second-all-channels")
	secondContext, secondResponse := newTestContext(t, secondRequest)
	srv.HandleStartOAuthCredentialCleanupJob(secondContext)
	if secondResponse.Code != http.StatusAccepted {
		t.Fatalf("second cleanup status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	var secondStart struct {
		Data oauthCredentialCleanupJobStart `json:"data"`
	}
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &secondStart); err != nil {
		t.Fatal(err)
	}
	if secondStart.Data.Total != 7 || secondStart.Data.Model != "gpt-other" {
		t.Fatalf("second cleanup start=%+v", secondStart.Data)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _, err = srv.oauthCredentialCleanupJobs.Get(secondStart.Data.JobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != oauthCredentialCleanupJobSucceeded || len(view.Events) == 0 ||
		view.Events[len(view.Events)-1].Processed != 7 {
		t.Fatalf("second cleanup view=%+v", view)
	}
	createChannel("cleanup-wildcard", upstream.URL+"/healthy-2", []model.ModelEntry{{Model: "*"}}, "")
	wildcardOptionsContext, wildcardOptionsResponse := newTestContext(t, newRequest(
		http.MethodGet, "/admin/oauth/credentials/cleanup/options?auth_type=codex_oauth", nil,
	))
	srv.HandleOAuthCredentialCleanupOptions(wildcardOptionsContext)
	if wildcardOptionsResponse.Code != http.StatusOK {
		t.Fatalf("wildcard cleanup options status=%d body=%s", wildcardOptionsResponse.Code, wildcardOptionsResponse.Body.String())
	}
	if err := json.Unmarshal(wildcardOptionsResponse.Body.Bytes(), &optionsPayload); err != nil {
		t.Fatal(err)
	}
	seenModels := make(map[string]struct{}, len(optionsPayload.Data.Models))
	for _, modelName := range optionsPayload.Data.Models {
		if modelName == "*" {
			t.Fatalf("wildcard model leaked into cleanup options: %+v", optionsPayload.Data)
		}
		if _, duplicate := seenModels[modelName]; duplicate {
			t.Fatalf("duplicate model leaked into cleanup options: %+v", optionsPayload.Data)
		}
		seenModels[modelName] = struct{}{}
	}
	if optionsPayload.Data.ChannelCount != 8 {
		t.Fatalf("wildcard cleanup options=%+v", optionsPayload.Data)
	}
}

func TestOAuthCredentialCleanupCancelDuringRefreshKeepsChannel(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
	}))
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshReturned := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	var returnedOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRefresh) }) })
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		startOnce.Do(func() { close(refreshStarted) })
		<-releaseRefresh
		returnedOnce.Do(func() { close(refreshReturned) })
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			Request:    req,
		}, nil
	})}
	refreshService := codexauth.NewService(refreshClient)
	refreshService.TokenURL = "https://oauth.test/token"
	srv.codexCredentials.service = refreshService
	srv.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	credential, err := (&codexauth.Credential{
		Type:         codexauth.ChannelType,
		AccessToken:  "at-cleanup-stop",
		RefreshToken: "rt-cleanup-stop",
		Expired:      time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		AccountID:    "account-cleanup-stop",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name:            "cleanup-stop",
		AuthType:        model.AuthTypeCodexOAuth,
		OAuthCredential: credential,
		URLs: model.ChannelURLs{{
			URL: upstream.URL, Exact: true, Protocols: []string{util.ProtocolCodex},
		}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-stop"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatal(err)
	}

	startRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-stop"}`,
	))
	startRequest.Header.Set("Content-Type", "application/json")
	startContext, startResponse := newTestContext(t, startRequest)
	srv.HandleStartOAuthCredentialCleanupJob(startContext)
	if startResponse.Code != http.StatusAccepted {
		t.Fatalf("start cleanup status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	var started struct {
		Data oauthCredentialCleanupJobStart `json:"data"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not reach credential refresh")
	}

	cancelContext, cancelResponse := newTestContext(t, newRequest(
		http.MethodPost,
		fmt.Sprintf("/admin/oauth/credentials/cleanup/jobs/%s/cancel", started.Data.JobID),
		nil,
	))
	cancelContext.Params = gin.Params{{Key: "id", Value: started.Data.JobID}}
	srv.HandleCancelOAuthCredentialCleanupJob(cancelContext)
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel cleanup status=%d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var view oauthCredentialCleanupJobView
	for time.Now().Before(deadline) {
		view, _, err = srv.oauthCredentialCleanupJobs.Get(started.Data.JobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != oauthCredentialCleanupJobCancelled {
		t.Fatalf("cancelled cleanup status=%q error=%q events=%+v", view.Status, view.Error, view.Events)
	}
	if _, err := srv.store.GetConfig(context.Background(), created.ID); err != nil {
		t.Fatalf("stopped cleanup deleted its channel: %v", err)
	}

	releaseOnce.Do(func() { close(releaseRefresh) })
	select {
	case <-refreshReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("background refresh did not return")
	}
	if _, err := srv.store.GetConfig(context.Background(), created.ID); err != nil {
		t.Fatalf("late refresh failure deleted a channel after stop: %v", err)
	}

	streamContext, streamResponse := newTestContext(t, newRequest(
		http.MethodGet,
		fmt.Sprintf("/admin/oauth/credentials/cleanup/jobs/%s/stream?after=0", started.Data.JobID),
		nil,
	))
	streamContext.Params = gin.Params{{Key: "id", Value: started.Data.JobID}}
	srv.HandleOAuthCredentialCleanupStream(streamContext)
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamResponse.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancelled cleanup SSE status=%d body=%s", streamResponse.Code, streamResponse.Body.String())
	}
}

func TestOAuthCredentialCleanupDisablesRejectedPersonalAccessTokenByDefault(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAttempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer at-cleanup-pat" {
			t.Errorf("PAT cleanup authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"revoked"}}`)
	}))
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	credential, err := (&codexauth.Credential{
		Type:          codexauth.ChannelType,
		AuthMode:      codexauth.AuthModePersonalAccessToken,
		AccessToken:   "at-cleanup-pat",
		ChatGPTUserID: "cleanup-pat-user",
		AccountID:     "cleanup-pat-account",
		Email:         "cleanup-pat@example.com",
		PlanType:      "plus",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "cleanup-pat", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: upstream.URL, Exact: true, Protocols: []string{util.ProtocolCodex}}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-pat"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCleanup := func(action string) (oauthCredentialCleanupJobStart, oauthCredentialCleanupJobView) {
		t.Helper()
		body := `{"auth_type":"codex_oauth","model":"gpt-pat"}`
		if action != "" {
			body = fmt.Sprintf(`{"auth_type":"codex_oauth","model":"gpt-pat","action":%q}`, action)
		}
		request := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		requestContext, response := newTestContext(t, request)
		srv.HandleStartOAuthCredentialCleanupJob(requestContext)
		if response.Code != http.StatusAccepted {
			t.Fatalf("start PAT cleanup status=%d body=%s", response.Code, response.Body.String())
		}
		var started struct {
			Data oauthCredentialCleanupJobStart `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		var view oauthCredentialCleanupJobView
		for time.Now().Before(deadline) {
			var err error
			view, _, err = srv.oauthCredentialCleanupJobs.Get(started.Data.JobID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if view.Status != oauthCredentialCleanupJobRunning {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return started.Data, view
	}

	started, view := runCleanup("")
	if started.Action != oauthCredentialCleanupActionDisable {
		t.Fatalf("default cleanup action=%q, want disable", started.Action)
	}
	if view.Status != oauthCredentialCleanupJobSucceeded || len(view.Events) == 0 ||
		view.Events[len(view.Events)-1].Disabled != 1 || view.Events[len(view.Events)-1].Deleted != 0 {
		t.Fatalf("PAT cleanup view=%+v", view)
	}
	persisted, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil || persisted.Enabled {
		t.Fatalf("default cleanup did not disable revoked personal access token: channel=%+v err=%v", persisted, err)
	}

	_, repeated := runCleanup("")
	if repeated.Status != oauthCredentialCleanupJobSucceeded || len(repeated.Events) == 0 ||
		repeated.Events[len(repeated.Events)-1].Skipped != 1 || repeated.Events[len(repeated.Events)-1].Disabled != 0 {
		t.Fatalf("repeated disable cleanup view=%+v", repeated)
	}
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("repeated disable cleanup upstream attempts=%d, want 1", got)
	}

	deletedStart, deleted := runCleanup(oauthCredentialCleanupActionDelete)
	if deletedStart.Action != oauthCredentialCleanupActionDelete || deleted.Status != oauthCredentialCleanupJobSucceeded ||
		len(deleted.Events) == 0 || deleted.Events[len(deleted.Events)-1].Deleted != 1 {
		t.Fatalf("delete cleanup start=%+v view=%+v", deletedStart, deleted)
	}
	if _, err := srv.store.GetConfig(context.Background(), created.ID); err == nil {
		t.Fatal("delete cleanup kept the disabled revoked PAT channel")
	}
}

func TestOAuthCredentialCleanupConcurrentEditKeepsCurrentChannel(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRequest) }) })
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		startedOnce.Do(func() { close(requestStarted) })
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
			Request:    req,
		}, nil
	})}
	refreshService := codexauth.NewService(refreshClient)
	refreshService.TokenURL = "https://oauth.test/token"
	srv.codexCredentials.service = refreshService
	srv.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	credential, err := (&codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: "at-concurrent-edit", RefreshToken: "rt-concurrent-edit",
		Expired: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), AccountID: "account-concurrent-edit",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "cleanup-concurrent-edit", AuthType: model.AuthTypeCodexOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: upstream.URL + "/stale", Exact: true, Protocols: []string{util.ProtocolCodex}}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-edit"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	startRequest := newRequest(http.MethodPost, "/admin/oauth/credentials/cleanup/jobs", strings.NewReader(
		`{"auth_type":"codex_oauth","model":"gpt-edit"}`,
	))
	startRequest.Header.Set("Content-Type", "application/json")
	startContext, startResponse := newTestContext(t, startRequest)
	srv.HandleStartOAuthCredentialCleanupJob(startContext)
	if startResponse.Code != http.StatusAccepted {
		t.Fatalf("start cleanup status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	var started struct {
		Data oauthCredentialCleanupJobStart `json:"data"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not start the conversation test")
	}

	current, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.URLs = model.ChannelURLs{{URL: upstream.URL + "/healthy", Exact: true, Protocols: []string{util.ProtocolCodex}}}
	if _, err := srv.store.UpdateConfig(context.Background(), created.ID, current); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(releaseRequest) })

	deadline := time.Now().Add(3 * time.Second)
	var view oauthCredentialCleanupJobView
	for time.Now().Before(deadline) {
		view, _, err = srv.oauthCredentialCleanupJobs.Get(started.Data.JobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != oauthCredentialCleanupJobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != oauthCredentialCleanupJobSucceeded {
		t.Fatalf("cleanup status=%q error=%q events=%+v", view.Status, view.Error, view.Events)
	}
	persisted, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("cleanup deleted the concurrently edited channel: %v", err)
	}
	if got := persisted.URLs[0].URL; got != upstream.URL+"/healthy" {
		t.Fatalf("persisted URL=%q, want concurrent edit", got)
	}
	foundSkipped := false
	for _, event := range view.Events {
		if event.Event == "progress" && event.ChannelID == created.ID && event.Status == "skipped" {
			foundSkipped = true
		}
	}
	if !foundSkipped {
		t.Fatalf("cleanup did not report the snapshot mismatch: %+v", view.Events)
	}
}

func TestOAuthCredentialRefreshTrackerOwnsDetachedRefreshLifetime(t *testing.T) {
	_, cancelParent := context.WithCancel(context.Background())
	tracker := newOAuthCredentialRefreshTracker()
	trackedCtx, done, err := tracker.begin()
	if err != nil {
		t.Fatal(err)
	}
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- tracker.close(context.Background())
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tracker.mu.Lock()
		closing := tracker.closing
		tracker.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closeResult:
		t.Fatalf("tracker closed before its refresh exited: %v", err)
	default:
	}
	if _, _, err := tracker.begin(); !errors.Is(err, errOAuthCredentialRefreshesClosed) {
		t.Fatalf("begin after close error=%v", err)
	}
	cancelParent()
	select {
	case <-trackedCtx.Done():
		t.Fatal("server cancellation interrupted a refresh before it persisted")
	case <-time.After(20 * time.Millisecond):
	}
	done()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tracker did not finish after refresh exit")
	}
	select {
	case <-trackedCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("closed tracker did not release its owned context")
	}

	timeoutTracker := newOAuthCredentialRefreshTracker()
	timeoutCtx, timeoutDone, err := timeoutTracker.begin()
	if err != nil {
		t.Fatal(err)
	}
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	cancelShutdown()
	if err := timeoutTracker.close(shutdownCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("timed out tracker close error=%v", err)
	}
	select {
	case <-timeoutCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("forced tracker shutdown did not cancel the refresh context")
	}
	timeoutDone()
}

func TestAnthropicOAuthChannelTestDecodesAdvertisedCompression(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, _ = writer.Write([]byte(`{"id":"msg-test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-5-20250929","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		_ = writer.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	srv := newInMemoryServer(t)
	cfg := createAnthropicOAuthChannelForAdminTest(t, srv, upstream.URL)
	result := srv.executeChannelTestWithCooldown(context.Background(), cfg, cooldown.NoKeyIndex, "at-anthropic-admin", &testutil.TestChannelRequest{
		Model: "claude-sonnet-4-5", ClientProtocol: util.ProtocolAnthropic, Content: "hello",
	}, true)
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("compressed Anthropic channel test result=%+v", result)
	}
	if responseText, _ := result["response_text"].(string); responseText != "ok" {
		t.Fatalf("decoded response_text=%q result=%+v", responseText, result)
	}
}

// TestHandleChannelTest 测试渠道测试功能
func TestHandleChannelTest(t *testing.T) {
	tests := []struct {
		name           string
		channelID      string
		requestBody    map[string]any
		setupData      bool
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:      "无效的渠道ID",
			channelID: "invalid",
			requestBody: map[string]any{
				"model":           "test-model",
				"client_protocol": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "渠道不存在",
			channelID: "999",
			requestBody: map[string]any{
				"model":           "test-model",
				"client_protocol": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:      "无效的请求体",
			channelID: "1",
			requestBody: map[string]any{
				"invalid_field": "value",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试服务器
			srv := newInMemoryServer(t)

			ctx := context.Background()

			// 设置测试数据(如果需要)
			if tt.setupData {
				cfg := &model.Config{
					ID:           1,
					Name:         "test-channel",
					URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
					Priority:     1,
					ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
					Enabled:      true,
				}
				_, err := srv.store.CreateConfig(ctx, cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+tt.channelID+"/test", tt.requestBody))
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleChannelTest(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d, 响应: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
			if resp.Success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v, error=%q", tt.expectSuccess, resp.Success, resp.Error)
			}
		})
	}
}

func TestChannelTestCodexStopsAfterResponseCompleted(t *testing.T) {
	streamBody := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"created_at\":1784768634,\"model\":\"gpt-5.6-sol\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1784768634,\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")

	tests := []struct {
		name           string
		clientProtocol string
		transformMode  string
	}{
		{name: "native", clientProtocol: util.ProtocolCodex, transformMode: model.ProtocolTransformModeUpstream},
		{name: "translated", clientProtocol: util.ProtocolOpenAI, transformMode: model.ProtocolTransformModeLocal},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			srv.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: &errAfterDataReadCloser{
						data: streamBody,
						err:  errors.New("local error: tls: bad record MAC"),
					},
					Request: req,
				}, nil
			})}

			result := srv.testChannelAPI(context.Background(), &model.Config{
				ID:                    int64(i + 1),
				Name:                  tt.name + "-codex-semantic-completion",
				URLs:                  model.ChannelURLs{{URL: "https://upstream.invalid", Protocols: []string{util.ProtocolCodex}}},
				ProtocolTransformMode: tt.transformMode,
				ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
			}, "sk-test", &testutil.TestChannelRequest{
				Model:          "gpt-5.6-sol",
				ClientProtocol: tt.clientProtocol,
				Stream:         true,
				Content:        "hello",
			})

			if success, _ := result["success"].(bool); !success {
				t.Fatalf("completed Responses stream must succeed despite trailing TLS error: %+v", result)
			}
			if got, _ := result["response_text"].(string); got != "hello" {
				t.Fatalf("response_text=%q, want hello; result=%+v", got, result)
			}
			if _, hasError := result["error"]; hasError {
				t.Fatalf("completed Responses stream must not expose trailing TLS error: %+v", result)
			}
		})
	}
}

func TestChannelTestCodexUsesNativeWebsocketWhenEnabled(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("WebSocket 渠道测试错误地走了 HTTP: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization=%q, want bearer test key", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("X-Api-Key must be removed, got %q", got)
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != "codex-tui" {
			t.Errorf("Codex identity headers=%v", r.Header)
		}
		if got := r.Header.Get("X-Codex-Turn-State"); got != "turn-state" {
			t.Errorf("X-Codex-Turn-State=%q, want allowed websocket header", got)
		}
		if r.Header.Get("X-Arbitrary-Client") != "" || r.Header.Get("Accept") != "" || r.Header.Get("Content-Type") != "" {
			t.Errorf("unapproved HTTP headers leaked into websocket handshake: %v", r.Header)
		}
		if r.Header.Get("Session-Id") == "" || r.Header.Get("Thread-Id") == "" {
			t.Errorf("Codex websocket session headers are incomplete: %v", r.Header)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("升级 WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("读取 WebSocket 测试请求: %v", err)
			return
		}
		if got := gjson.GetBytes(requestBody, "type").String(); got != responsesWebsocketRequestCreate {
			t.Errorf("请求 type=%q, want %q", got, responsesWebsocketRequestCreate)
		}
		if !gjson.GetBytes(requestBody, "stream").Bool() {
			t.Error("WebSocket 请求必须强制 stream=true")
		}

		for _, event := range []map[string]any{
			{"type": "response.output_text.delta", "delta": "hello"},
			{
				"type": "response.completed",
				"response": map[string]any{
					"id":     "resp_admin_ws",
					"status": "completed",
					"usage": map[string]any{
						"input_tokens":  3,
						"output_tokens": 1,
					},
				},
			},
		} {
			if err := conn.WriteJSON(event); err != nil {
				t.Errorf("写入 WebSocket 测试响应: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:                    97,
		Name:                  "codex-native-websocket-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		Websockets:            true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
	}, "sk-test", &testutil.TestChannelRequest{
		Model:          "gpt-5.6-sol",
		ClientProtocol: "codex",
		Stream:         true,
		Content:        "hello",
		Headers: map[string]string{
			"X-Codex-Turn-State": "turn-state",
			"X-Arbitrary-Client": "must-not-leak",
		},
	})

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("原生 WebSocket 渠道测试失败: %+v", result)
	}
	if got, _ := result["transport"].(string); got != "websocket" {
		t.Fatalf("transport=%q, want websocket; result=%+v", got, result)
	}
	if got, _ := result["response_text"].(string); got != "hello" {
		t.Fatalf("response_text=%q, want hello; result=%+v", got, result)
	}
	if got, _ := result["upstream_request_url"].(string); !strings.HasPrefix(got, "ws://") {
		t.Fatalf("upstream_request_url=%q, want ws:// URL", got)
	}
	if got, _ := result["upstream_request_body"].(string); gjson.Get(got, "type").String() != responsesWebsocketRequestCreate {
		t.Fatalf("upstream_request_body 未记录实际 WebSocket 帧: %s", got)
	}
}

func TestChannelTestCodexDoesNotHideRejectedWebsocketHandshake(t *testing.T) {
	var httpFallbacks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUpgradeRequired)
			_, _ = io.WriteString(w, `{"error":{"message":"websocket disabled"}}`)
			return
		}
		httpFallbacks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_http","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:                    98,
		Name:                  "codex-rejected-websocket-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		Websockets:            true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.6-sol"}},
	}, "sk-test", &testutil.TestChannelRequest{
		Model:          "gpt-5.6-sol",
		ClientProtocol: "codex",
		Stream:         true,
		Content:        "hello",
	})

	if success, _ := result["success"].(bool); success {
		t.Fatalf("被拒绝的 WebSocket 握手不得被 HTTP 成功掩盖: %+v", result)
	}
	if got, _ := getResultInt(result["status_code"]); got != http.StatusUpgradeRequired {
		t.Fatalf("status_code=%d, want %d; result=%+v", got, http.StatusUpgradeRequired, result)
	}
	if got := httpFallbacks.Load(); got != 0 {
		t.Fatalf("渠道测试在 WebSocket 握手失败后偷偷回退 HTTP: calls=%d", got)
	}
}

func TestHandleChannelWebsocketProbeDetectsSupportedUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Errorf("probe used HTTP instead of WebSocket: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("probe path=%q, want Codex Responses path /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe" {
			t.Errorf("Authorization=%q, want bearer probe key", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("X-Api-Key must be removed, got %q", got)
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != "codex-tui" {
			t.Errorf("Codex identity headers=%v", r.Header)
		}
		if r.Header.Get("Accept") != "" || r.Header.Get("Content-Type") != "" {
			t.Errorf("HTTP-only headers leaked into websocket probe: %v", r.Header)
		}
		if r.Header.Get("Session-Id") == "" || r.Header.Get("Thread-Id") == "" {
			t.Errorf("Codex websocket session headers are incomplete: %v", r.Header)
		}
		if beta := r.Header.Get("OpenAI-Beta"); !strings.Contains(beta, "responses_websockets=") {
			t.Errorf("OpenAI-Beta=%q, want responses_websockets feature", beta)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade probe websocket: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/websocket-probe", map[string]any{
		"url":     upstream.URL,
		"api_key": "sk-probe",
	}))

	srv.HandleChannelWebsocketProbe(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	result := mustParseAPIResponse[struct {
		Supported bool `json:"supported"`
	}](t, w.Body.Bytes())
	if !result.Data.Supported {
		t.Fatalf("supported=false, want true; body=%s", w.Body.String())
	}
}

func TestHandleChannelWebsocketProbeRejectsUnsupportedUpstreamWithoutHTTPFallback(t *testing.T) {
	var httpFallbacks atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		httpFallbacks.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/websocket-probe", map[string]any{
		"url":     upstream.URL,
		"api_key": "sk-probe",
	}))

	srv.HandleChannelWebsocketProbe(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	result := mustParseAPIResponse[struct {
		Supported bool `json:"supported"`
		Status    int  `json:"status"`
	}](t, w.Body.Bytes())
	if result.Data.Supported {
		t.Fatalf("supported=true, want false; body=%s", w.Body.String())
	}
	if result.Data.Status != http.StatusUpgradeRequired {
		t.Fatalf("status=%d, want %d; body=%s", result.Data.Status, http.StatusUpgradeRequired, w.Body.String())
	}
	if got := httpFallbacks.Load(); got != 0 {
		t.Fatalf("probe fell back to HTTP: calls=%d", got)
	}
}

func TestTestChannelAPI_MultiURL5xxDoesNotFallbackOrCooldownURL(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream fail"}}`))
	}))
	defer failUpstream.Close()

	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:           9527,
		Name:         "multi-url-test",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中失败URL，模型级 5xx 不应改打同渠道的第二个 URL。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if success {
		t.Fatalf("expected first 5xx result to be returned, got result=%+v", result)
	}
	if failCalls < 1 || okCalls != 0 {
		t.Fatalf("expected only failing URL attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("model-scoped 5xx must not cool URL, url=%s", failUpstream.URL)
	}
}

func TestTestChannelAPI_MultiURLStreamFailureDoesNotFallbackOrCooldownURL(t *testing.T) {
	tests := []struct {
		name             string
		wantStatus       int
		configureTimeout func(*Server)
		serveFailure     func(http.ResponseWriter, *http.Request)
	}{
		{
			name:       "first_valid_content_timeout",
			wantStatus: util.StatusFirstByteTimeout,
			configureTimeout: func(srv *Server) {
				srv.firstByteTimeout = 25 * time.Millisecond
			},
			serveFailure: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						_, _ = io.WriteString(w, ": keep-alive\n\n")
						flusher.Flush()
					}
				}
			},
		},
		{
			name:       "stream_incomplete",
			wantStatus: util.StatusStreamIncomplete,
			configureTimeout: func(srv *Server) {
				srv.firstByteTimeout = time.Second
				srv.streamTimeout = 25 * time.Millisecond
			},
			serveFailure: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-r.Context().Done()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var failureCalls atomic.Int32
			var fallbackCalls atomic.Int32

			failureUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				failureCalls.Add(1)
				tt.serveFailure(w, r)
			}))
			defer failureUpstream.Close()

			fallbackUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer fallbackUpstream.Close()

			srv := newInMemoryServer(t)
			tt.configureTimeout(srv)
			cfg := &model.Config{
				ID:           9528,
				Name:         "multi-url-stream-failure-test",
				URLs:         channelURLsForTest(failureUpstream.URL, fallbackUpstream.URL),
				Priority:     1,
				ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
				Enabled:      true,
			}

			// 强制第一跳命中流故障 URL。模型级流故障不应改打同渠道的第二个 URL。
			srv.urlSelector.CooldownURL(cfg.ID, fallbackUpstream.URL)
			result := srv.testChannelAPI(context.Background(), cfg, "sk-test", &testutil.TestChannelRequest{
				Model:          "gpt-4o-mini",
				ClientProtocol: "openai",
				Content:        "hello",
				Stream:         true,
			})

			if statusCode, _ := getResultInt(result["status_code"]); statusCode != tt.wantStatus {
				t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, tt.wantStatus, result)
			}
			if got := failureCalls.Load(); got != 1 {
				t.Fatalf("failure URL calls=%d, want 1", got)
			}
			if got := fallbackCalls.Load(); got != 0 {
				t.Fatalf("model-scoped stream failure retried another URL: calls=%d", got)
			}
			if srv.urlSelector.IsCooledDown(cfg.ID, failureUpstream.URL) {
				t.Fatalf("model-scoped stream failure must not cool URL, status=%d url=%s", tt.wantStatus, failureUpstream.URL)
			}
		})
	}
}

func TestExecuteChannelTestWithCooldown_RespectsRPMLimitWithoutCooldown(t *testing.T) {
	hits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:                    9528,
		Name:                  "rpm-limited-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		RPMLimit:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:               true,
	}
	req := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
	}

	first := srv.executeChannelTestWithCooldown(context.Background(), cfg, 0, "sk-test", req, true)
	if success, _ := first["success"].(bool); !success {
		t.Fatalf("first test should succeed, got result=%+v", first)
	}

	second := srv.executeChannelTestWithCooldown(context.Background(), cfg, 0, "sk-test", req, true)
	if success, _ := second["success"].(bool); success {
		t.Fatalf("second test should be RPM limited, got result=%+v", second)
	}
	if limited, _ := second["rpm_limited"].(bool); !limited {
		t.Fatalf("expected rpm_limited marker, got result=%+v", second)
	}
	if action, _ := second["cooldown_action"].(string); action != "rpm_limited_no_cooldown" {
		t.Fatalf("cooldown_action=%q, want rpm_limited_no_cooldown, result=%+v", action, second)
	}
	if retryAfterMs, _ := getResultInt(second["retry_after_ms"]); retryAfterMs <= 0 {
		t.Fatalf("retry_after_ms=%d, want positive value, result=%+v", retryAfterMs, second)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want 1", hits)
	}
}

func TestExecuteChannelTestWithCooldown_ModelCooldownUsesSentModelKey(t *testing.T) {
	const sentModel = "model-c"

	var upstreamModel string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		} else {
			upstreamModel, _ = body["model"].(string)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"model-c","reset_seconds":300}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	cfg, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:     "model-cooldown-sent-key-test",
		URLs:     model.ChannelURLs{{URL: upstream.URL}},
		Priority: 1,
		ModelEntries: []model.ModelEntry{
			{Model: "model-a", RedirectModel: "model-b"},
			{Model: "model-b", RedirectModel: sentModel},
			{Model: sentModel, RedirectModel: "model-d"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	req := &testutil.TestChannelRequest{
		Model:          "model-a",
		ClientProtocol: "openai",
		Content:        "hello",
	}
	result := srv.executeChannelTestWithCooldown(ctx, cfg, 0, "sk-test", req, true)
	if action, _ := result["cooldown_action"].(string); action != "model_cooldown_applied" {
		t.Fatalf("cooldown_action=%q, want model_cooldown_applied; result=%+v", action, result)
	}
	if upstreamModel != sentModel {
		t.Fatalf("upstream model=%q, want %q", upstreamModel, sentModel)
	}

	cooldowns, err := srv.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[cfg.ID][sentModel]; !until.After(time.Now()) {
		t.Fatalf("sent model cooldown=%s, want active cooldown", until.Format(time.RFC3339))
	}
	if _, exists := cooldowns[cfg.ID]["model-d"]; exists {
		t.Fatal("model cooldown must not be re-resolved after the request")
	}
}

func TestTestChannelAPI_MultiURLPlainText502DoesNotFallback(t *testing.T) {
	failCalls := 0
	okCalls := 0

	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("error code: 502"))
	}))
	defer failUpstream.Close()

	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:           9528,
		Name:         "multi-url-plain-502-test",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}

	// 强制第一跳命中 502 的坏 URL，text/plain 错误体也必须保持模型级语义。
	srv.urlSelector.CooldownURL(cfg.ID, okUpstream.URL)

	req := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
	}

	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	success, _ := result["success"].(bool)
	if success {
		t.Fatalf("expected plain 502 failure, got result=%+v", result)
	}
	if failCalls < 1 || okCalls != 0 {
		t.Fatalf("expected only failing URL attempted, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
	if srv.urlSelector.IsCooledDown(cfg.ID, failUpstream.URL) {
		t.Fatalf("model-scoped 502 must not cool URL, url=%s", failUpstream.URL)
	}
}

func TestTestChannelAPI_NonStreamUsesConfiguredTimeout(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(160 * time.Millisecond):
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"late"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.nonStreamTimeout = 25 * time.Millisecond

	cfg := &model.Config{
		ID:           9530,
		Name:         "non-stream-timeout-test",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	req := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
		Stream:         false,
	}

	start := time.Now()
	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	elapsed := time.Since(start)

	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected timeout failure, got result=%+v", result)
	}
	if elapsed >= 120*time.Millisecond {
		t.Fatalf("expected configured timeout before delayed upstream response, elapsed=%v result=%+v", elapsed, result)
	}
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != http.StatusGatewayTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, http.StatusGatewayTimeout, result)
	}
}

func TestTestChannelAPI_StreamFirstValidContentTimeoutIgnoresHeartbeats(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		lateContent := time.NewTimer(500 * time.Millisecond)
		defer lateContent.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": keep-alive\n\n"))
				flusher.Flush()
			case <-lateContent.C:
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				return
			}
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 30 * time.Millisecond

	cfg := &model.Config{
		ID:           9531,
		Name:         "stream-first-content-timeout-test",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	req := &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
		Stream:         true,
	}

	start := time.Now()
	result := srv.testChannelAPI(context.Background(), cfg, "sk-test", req)
	elapsed := time.Since(start)

	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected first valid stream content timeout, got result=%+v", result)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("expected timeout before late content, elapsed=%v result=%+v", elapsed, result)
	}
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}
	if _, ok := result["first_byte_duration_ms"]; ok {
		t.Fatalf("heartbeat must not set first_byte_duration_ms, result=%+v", result)
	}
}

func TestTestChannelAPI_ResponsesMetadataDoesNotStopFirstContentTimeout(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"status":"in_progress"}}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"late"}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 30 * time.Millisecond
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:           9533,
		Name:         "responses-metadata-first-content-timeout-test",
		URLs:         model.ChannelURLs{{URL: upstream.URL, Protocols: []string{"codex"}}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6-sol"}},
		Enabled:      true,
	}, "sk-test", &testutil.TestChannelRequest{
		Model:          "gpt-5.6-sol",
		ClientProtocol: "codex",
		Content:        "hello",
		Stream:         true,
	})

	if success, _ := result["success"].(bool); success {
		t.Fatalf("Responses metadata must not count as valid content, result=%+v", result)
	}
	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}
	if _, ok := result["first_byte_duration_ms"]; ok {
		t.Fatalf("Responses metadata must not set first_byte_duration_ms, result=%+v", result)
	}
}

func TestTestChannelAPI_StreamFirstValidContentTimeoutEOFReturns598(t *testing.T) {
	srv := newInMemoryServer(t)
	srv.firstByteTimeout = 10 * time.Millisecond
	srv.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &heartbeatThenContextEOFBody{ctx: req.Context()},
			Request:    req,
		}, nil
	})}

	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:           9532,
		Name:         "stream-first-content-timeout-eof-test",
		URLs:         model.ChannelURLs{{URL: "http://test-upstream.invalid"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}, "sk-test", &testutil.TestChannelRequest{
		Model:          "gpt-4o-mini",
		ClientProtocol: "openai",
		Content:        "hello",
		Stream:         true,
	})

	if statusCode, _ := getResultInt(result["status_code"]); statusCode != util.StatusFirstByteTimeout {
		t.Fatalf("status_code=%d, want %d, result=%+v", statusCode, util.StatusFirstByteTimeout, result)
	}
}

type heartbeatThenContextEOFBody struct {
	ctx       context.Context
	heartbeat bool
}

func (b *heartbeatThenContextEOFBody) Read(p []byte) (int, error) {
	if !b.heartbeat {
		b.heartbeat = true
		return copy(p, ": keep-alive\n\n"), nil
	}
	<-b.ctx.Done()
	return 0, io.EOF
}

func (b *heartbeatThenContextEOFBody) Close() error {
	return nil
}

func TestHandleChannelTest_InvalidRequestDoesNotLeakDecoderError(t *testing.T) {
	srv := newInMemoryServer(t)

	c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/channels/1/test", []byte(`{"model":123,"client_protocol":"anthropic"}`)))
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if resp.Error != "invalid request" {
		t.Fatalf("error=%q, want generic invalid request", resp.Error)
	}
	if strings.Contains(resp.Error, "unmarshal") || strings.Contains(resp.Error, "TestChannelRequest") {
		t.Fatalf("decoder detail leaked in response: %q", resp.Error)
	}
}

func TestHandleChannelTest_RejectsBaseURL(t *testing.T) {
	failCalls := 0
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "channel-test-reject-base-url",
		URLs:         channelURLsForTest(failUpstream.URL, okUpstream.URL),
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/test", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"base_url":        okUpstream.URL,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if resp.Success {
		t.Fatalf("expected success=false, resp=%+v", resp)
	}
	if !strings.Contains(resp.Error, "/test-url") {
		t.Fatalf("expected error to guide /test-url, got %q", resp.Error)
	}
	if failCalls != 0 || okCalls != 0 {
		t.Fatalf("expected no upstream request, failCalls=%d okCalls=%d", failCalls, okCalls)
	}
}

func TestHandleChannelURLTest_UsesForcedURL(t *testing.T) {
	failCalls := 0
	failUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"should not hit this url"}}`))
	}))
	defer failUpstream.Close()

	okCalls := 0
	okUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong protocol path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer okUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "single-url-test",
		URLs:         model.ChannelURLs{{URL: failUpstream.URL, Protocols: []string{"anthropic"}}, {URL: okUpstream.URL, Protocols: []string{"openai"}}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4o-mini"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}
	// selector 和多 URL 顺序都不该影响显式单 URL 测试。
	srv.urlSelector.CooldownURL(created.ID, okUpstream.URL)

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+fmt.Sprintf("%d", created.ID)+"/test-url", map[string]any{
		"model":           "gpt-4o-mini",
		"client_protocol": "openai",
		"base_url":        okUpstream.URL,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelURLTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected success=true, data=%+v", resp.Data)
	}
	if failCalls != 0 {
		t.Fatalf("expected forced base_url to skip fail url, failCalls=%d", failCalls)
	}
	if okCalls != 1 {
		t.Fatalf("expected forced base_url called once, okCalls=%d", okCalls)
	}
}

// TestHandleChannelTest_NoAPIKey 渠道存在但无 API key
func TestHandleChannelTest_NoAPIKey(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	// 创建渠道但不添加 API key
	cfg := &model.Config{
		Name:         "no-key-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "test-model",
		"client_protocol": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	// 状态码 200，但 data 中 success=false
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	// RespondJSON 包装 success=true (外层), data 内部有 success: false
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("外层 APIResponse.Success 应为 true, error=%q", resp.Error)
	}

	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（渠道无 API key）")
	}

	dataError, _ := resp.Data["error"].(string)
	if dataError == "" {
		t.Fatal("data.error 不应为空")
	}
}

func TestHandleChannelTest_CodexOAuthWithoutAPIKey(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-admin-test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-admin-test" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("X-Api-Key must be removed: %q", r.Header.Get("X-Api-Key"))
		}
		if r.Header.Get("User-Agent") != codexUserAgent || r.Header.Get("Originator") != "codex-tui" ||
			(r.Header.Get("Session_id") == "" && r.Header.Get("Session-Id") == "") {
			t.Errorf("incomplete Codex OAuth headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "8")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-At", "1786851417")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_admin\",\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "codex",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("Codex OAuth channel test failed: %+v", resp)
	}
	if got, _ := resp.Data["total_keys"].(float64); got != 0 {
		t.Fatalf("total_keys=%v, want 0", resp.Data["total_keys"])
	}
	if got, _ := resp.Data["tested_key_index"].(float64); got != -1 {
		t.Fatalf("tested_key_index=%v, want -1", resp.Data["tested_key_index"])
	}
	persisted, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	credential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || credential.PassiveUsage == nil || len(credential.PassiveUsage.Windows) != 1 {
		t.Fatalf("persisted Codex quota = (%#v, %v)", credential, err)
	}
	window := credential.PassiveUsage.Windows[0]
	if window.UsedPercent != 8 || window.LimitWindowSeconds != 7*24*60*60 || window.ResetAt != 1786851417 {
		t.Fatalf("persisted Codex quota window = %#v", window)
	}
}

func TestHandleChannelTest_CodexOAuthWithoutQuotaHeadersLeavesUsageEmpty(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_admin\",\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gpt-5.6-sol", "client_protocol": "codex", "stream": true,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	persisted, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	credential, err := codexauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || credential.PassiveUsage != nil {
		t.Fatalf("persisted Codex quota = (%#v, %v)", credential, err)
	}
}

func TestHandleChannelTest_CodexOAuthPersistsQuotaFromSSE(t *testing.T) {
	const rateLimitEvent = `{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":571277,"reset_at":1786851417},"secondary":null},"code_review_rate_limits":null,"additional_rate_limits":{"GPT-5.3-Codex-Spark":{"allowed":true,"limit_reached":false,"primary":{"used_percent":0,"window_minutes":10080,"reset_after_seconds":604800,"reset_at":1786884940},"secondary":null}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"promo":null}`
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+rateLimitEvent+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_admin\",\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gpt-5.6-sol", "client_protocol": "codex", "stream": true,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !response.Success || !success {
		t.Fatalf("Codex OAuth channel test failed: %+v", response)
	}
	if raw, _ := response.Data["raw_response"].(string); !strings.Contains(raw, rateLimitEvent) {
		t.Fatalf("raw_response lost codex.rate_limits event: %q", raw)
	}
	var credential *codexauth.Credential
	deadline := time.Now().Add(2 * time.Second)
	for {
		persisted, err := srv.store.GetConfig(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("GetConfig: %v", err)
		}
		credential, err = codexauth.ParseCredential([]byte(persisted.OAuthCredential))
		if err != nil {
			t.Fatalf("ParseCredential: %v", err)
		}
		if credential.PassiveUsage != nil && len(credential.PassiveUsage.Windows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for persisted Codex SSE quota: %#v", credential)
		}
		time.Sleep(10 * time.Millisecond)
	}
	primary := credential.PassiveUsage.Windows[0]
	if primary.Scope != "codex" || primary.LimitName != "codex" || primary.Kind != "primary" ||
		primary.UsedPercent != 10 || primary.LimitWindowSeconds != 10080*60 || primary.ResetAt != 1786851417 {
		t.Fatalf("persisted Codex primary SSE quota window = %#v", primary)
	}
	additional := credential.PassiveUsage.Windows[1]
	if additional.Scope != "gpt-5.3-codex-spark" || additional.LimitName != "GPT-5.3-Codex-Spark" ||
		additional.Kind != "primary" || additional.UsedPercent != 0 ||
		additional.LimitWindowSeconds != 10080*60 || additional.ResetAt != 1786884940 {
		t.Fatalf("persisted Codex additional SSE quota window = %#v", additional)
	}
}

func TestHandleChannelTest_AntigravityOAuthWithoutAPIKey(t *testing.T) {
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer at-gravity-admin" {
			t.Errorf("unexpected Antigravity request: %s %v", r.URL.String(), r.Header)
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"gravity test answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createAntigravityOAuthChannelForAdminTest(t, srv, upstream.URL)
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gemini-3-flash", "client_protocol": "openai", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("Antigravity OAuth channel test failed: %+v", resp)
	}
	if got, _ := resp.Data["response_text"].(string); got != "gravity test answer" {
		t.Fatalf("response_text=%q data=%+v", got, resp.Data)
	}
	if gjson.GetBytes(upstreamBody, "project").String() != "gravity-admin-project" || gjson.GetBytes(upstreamBody, "request.contents").Array() == nil {
		t.Fatalf("invalid Antigravity request envelope: %s", upstreamBody)
	}
	if got, _ := resp.Data["total_keys"].(float64); got != 0 {
		t.Fatalf("total_keys=%v, want 0", resp.Data["total_keys"])
	}
}

func TestHandleChannelTest_AntigravityCapacityUsesProviderFallbackPolicy(t *testing.T) {
	var mu sync.Mutex
	var baseURLs []string
	var requestTimes []time.Time
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		baseURL := req.URL.Scheme + "://" + req.URL.Host
		mu.Lock()
		baseURLs = append(baseURLs, baseURL)
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()

		status := http.StatusOK
		contentType := "application/json"
		body := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fallback test answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}}`
		if baseURL == antigravityDailyBaseURL {
			status = http.StatusServiceUnavailable
			body = antigravityCapacityBodyForAdminTest
		} else if baseURL == antigravityProdBaseURL {
			t.Fatalf("production Antigravity URL was called: %s", baseURL)
		} else if baseURL != antigravitySandboxDailyBaseURL {
			t.Fatalf("unexpected Antigravity fallback URL: %s", baseURL)
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	srv := newInMemoryServer(t)
	srv.antigravityClient = client
	created := createAntigravityOAuthChannelForAdminTest(t, srv, antigravityDailyBaseURL)
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gemini-3-flash", "client_protocol": "gemini", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("Antigravity fallback test failed: %+v", resp)
	}
	if got, _ := resp.Data["response_text"].(string); got != "fallback test answer" {
		t.Fatalf("response_text=%q data=%+v", got, resp.Data)
	}
	if got, _ := resp.Data["retry_strategy"].(string); got != "model_capacity_retry_1" {
		t.Fatalf("retry_strategy=%q data=%+v", got, resp.Data)
	}

	mu.Lock()
	gotURLs := append([]string(nil), baseURLs...)
	gotTimes := append([]time.Time(nil), requestTimes...)
	mu.Unlock()
	wantURLs := []string{antigravityDailyBaseURL, antigravitySandboxDailyBaseURL}
	if !slices.Equal(gotURLs, wantURLs) {
		t.Fatalf("Antigravity test URLs=%v, want %v", gotURLs, wantURLs)
	}
	if delay := gotTimes[1].Sub(gotTimes[0]); delay < antigravityBaseURLFallbackDelay {
		t.Fatalf("fallback delay=%v, want >= %v", delay, antigravityBaseURLFallbackDelay)
	}
}

func TestHandleChannelTest_AntigravityCapacityExhaustionAppliesCooldownOnce(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(antigravityCapacityBodyForAdminTest)),
			Request:    req,
		}, nil
	})}

	srv := newInMemoryServer(t)
	srv.antigravityClient = client
	created := createAntigravityOAuthChannelForAdminTest(t, srv, antigravityDailyBaseURL)
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gemini-3-flash", "client_protocol": "gemini", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["status_code"].(float64); got != http.StatusTooManyRequests {
		t.Fatalf("status_code=%v data=%+v", resp.Data["status_code"], resp.Data)
	}
	if got, _ := resp.Data["retry_strategy"].(string); got != "model_capacity_retry_1" {
		t.Fatalf("retry_strategy=%q data=%+v", got, resp.Data)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("capacity calls=%d, want 2", got)
	}
	cooldowns, err := srv.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(cooldowns[created.ID]["gemini-3-flash"])
	if remaining < util.ServerErrorInitialCooldown-10*time.Second || remaining > util.ServerErrorInitialCooldown+2*time.Second {
		t.Fatalf("capacity cooldown remaining=%v, want one %v cooldown", remaining, util.ServerErrorInitialCooldown)
	}
}

func TestHandleChannelTest_AntigravityCustomURLDoesNotExpandCapacityFallback(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, antigravityCapacityBodyForAdminTest)
	}))
	t.Cleanup(upstream.Close)

	srv := newInMemoryServer(t)
	srv.antigravityClient = upstream.Client()
	created := createAntigravityOAuthChannelForAdminTest(t, srv, upstream.URL)
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gemini-3-flash", "client_protocol": "gemini", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["status_code"].(float64); got != http.StatusTooManyRequests {
		t.Fatalf("status_code=%v data=%+v", resp.Data["status_code"], resp.Data)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("custom Antigravity URL calls=%d, want 1", got)
	}
	cooldowns, err := srv.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	until := cooldowns[created.ID]["gemini-3-flash"]
	remaining := time.Until(until)
	if remaining < util.ServerErrorInitialCooldown-10*time.Second || remaining > util.ServerErrorInitialCooldown+2*time.Second {
		t.Fatalf("capacity cooldown remaining=%v, want about %v", remaining, util.ServerErrorInitialCooldown)
	}
}

func TestHandleChannelTest_AntigravityGlobalOverrideDoesNotExpandCapacityFallback(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, antigravityCapacityBodyForAdminTest)
	}))
	t.Cleanup(upstream.Close)

	srv := newInMemoryServerWithSettings(t, map[string]string{
		config.AntigravityURLSettingKey: upstream.URL,
	})
	srv.antigravityClient = upstream.Client()
	created := createAntigravityOAuthChannelForAdminTest(t, srv, antigravityDailyBaseURL)
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "gemini-3-flash", "client_protocol": "gemini", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	srv.HandleChannelTest(c)

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["status_code"].(float64); got != http.StatusTooManyRequests {
		t.Fatalf("status_code=%v data=%+v", resp.Data["status_code"], resp.Data)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Antigravity global override calls=%d, want 1", got)
	}
}

func TestHandleChannelTest_XAIOAuthWithoutAPIKeyUsesProviderWire(t *testing.T) {
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-xai-admin" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(xaiauth.CLITokenAuthHeader); got != xaiauth.CLITokenAuthValue {
			t.Errorf("%s = %q", xaiauth.CLITokenAuthHeader, got)
		}
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("ChatGPT-Account-ID") != "" ||
			r.Header.Get("Session-Id") != "" || r.Header.Get("Session_id") != "" {
			t.Errorf("conflicting authentication headers were not removed: %v", r.Header)
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get(xaiauth.CLIClientModeHeader); got != xaiauth.CLIClientMode {
			t.Errorf("%s = %q", xaiauth.CLIClientModeHeader, got)
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"xai test answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_xai_admin\",\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createXAIOAuthChannelForAdminTest(t, srv, upstream.URL+"/v1")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model": "grok-4.5", "client_protocol": "codex", "stream": false, "content": "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("xAI OAuth channel test failed: %+v", resp)
	}
	if got, _ := resp.Data["response_text"].(string); got != "xai test answer" {
		t.Fatalf("response_text=%q data=%+v", got, resp.Data)
	}
	if got, _ := resp.Data["total_keys"].(float64); got != 0 {
		t.Fatalf("total_keys=%v, want 0", resp.Data["total_keys"])
	}
	if got, _ := resp.Data["tested_key_index"].(float64); got != -1 {
		t.Fatalf("tested_key_index=%v, want -1", resp.Data["tested_key_index"])
	}
	if !gjson.GetBytes(upstreamBody, "stream").Bool() || gjson.GetBytes(upstreamBody, "model").String() != "grok-4.5" ||
		gjson.GetBytes(upstreamBody, "prompt_cache_key").String() == "" {
		t.Fatalf("xAI request was not finalized: %s", upstreamBody)
	}
}

func TestHandleChannelURLTest_XAIOAuthAllowsVersionedBaseURL(t *testing.T) {
	requests := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_xai_url_test\",\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createXAIOAuthChannelForAdminTest(t, srv, "https://unused.example.com/v1")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test-url", map[string]any{
		"model": "grok-4.5", "client_protocol": "codex", "stream": false, "content": "hello",
		"base_url": upstream.URL + "/v1",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelURLTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("xAI OAuth URL test failed: %+v", resp)
	}
	if requests != 1 {
		t.Fatalf("upstream requests=%d, want 1", requests)
	}
}

func TestHandleChannelTest_XAIOAuthUsesCurrentAccessTokenBeforeRefreshing(t *testing.T) {
	callTest := func(t *testing.T, srv *Server, channelID int64) APIResponse[map[string]any] {
		t.Helper()
		id := fmt.Sprintf("%d", channelID)
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+id+"/test", map[string]any{
			"model": "grok-4.5", "client_protocol": "codex", "stream": false, "content": "hello",
		}))
		c.Params = gin.Params{{Key: "id", Value: id}}
		srv.HandleChannelTest(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	}

	t.Run("expired metadata does not refresh a working access token", func(t *testing.T) {
		var upstreamRequests atomic.Int32
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamRequests.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer at-current" {
				t.Errorf("Authorization=%q, want current access token", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"current token works\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_current\",\"status\":\"completed\"}}\n\n")
		}))

		srv := newInMemoryServer(t)
		srv.client = upstream.Client()
		created := createXAIOAuthChannelForAdminTest(t, srv, upstream.URL+"/v1")
		created = replaceXAIOAuthCredentialForAdminTest(
			t, srv, created, "at-current", "rt-current", time.Now().Add(-24*time.Hour),
		)
		var refreshRequests atomic.Int32
		srv.xaiCredentials.clientFor = func(*model.Config) *http.Client {
			return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				refreshRequests.Add(1)
				return &http.Response{
					StatusCode: http.StatusInternalServerError, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"error":"unexpected_refresh"}`)), Request: req,
				}, nil
			})}
		}

		response := callTest(t, srv, created.ID)
		if success, _ := response.Data["success"].(bool); !response.Success || !success {
			t.Fatalf("channel test failed: %+v", response)
		}
		if upstreamRequests.Load() != 1 || refreshRequests.Load() != 0 {
			t.Fatalf("upstream=%d refresh=%d, want 1/0", upstreamRequests.Load(), refreshRequests.Load())
		}
	})

	t.Run("unauthorized access token refreshes once then retries", func(t *testing.T) {
		var orderMu sync.Mutex
		order := make([]string, 0, 3)
		record := func(stage string) {
			orderMu.Lock()
			order = append(order, stage)
			orderMu.Unlock()
		}
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Authorization") {
			case "Bearer at-rejected":
				record("test-current")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
			case "Bearer at-refreshed":
				record("test-refreshed")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"refreshed token works\"}\n\n")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_refreshed\",\"status\":\"completed\"}}\n\n")
			default:
				t.Errorf("unexpected Authorization=%q", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusUnauthorized)
			}
		}))

		srv := newInMemoryServer(t)
		srv.client = upstream.Client()
		created := createXAIOAuthChannelForAdminTest(t, srv, upstream.URL+"/v1")
		cached, err := srv.xaiCredentials.credential(context.Background(), created, false)
		if err != nil || cached.AccessToken != "at-xai-admin" {
			t.Fatalf("prime credential cache=%v err=%v", cached, err)
		}
		created = replaceXAIOAuthCredentialForAdminTest(
			t, srv, created, "at-rejected", "rt-current", time.Now().Add(-24*time.Hour),
		)
		var refreshRequests atomic.Int32
		refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			refreshRequests.Add(1)
			record("refresh")
			if req.URL.String() != xaiauth.TokenURL {
				t.Errorf("refresh URL=%s", req.URL)
			}
			if err := req.ParseForm(); err != nil || req.Form.Get("refresh_token") != "rt-current" {
				t.Errorf("refresh form=%v err=%v", req.Form, err)
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body:    io.NopCloser(strings.NewReader(`{"access_token":"at-refreshed","refresh_token":"rt-refreshed","expires_in":3600}`)),
				Request: req,
			}, nil
		})}
		srv.xaiCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

		response := callTest(t, srv, created.ID)
		if success, _ := response.Data["success"].(bool); !response.Success || !success {
			t.Fatalf("channel test failed after refresh: %+v", response)
		}
		if refreshRequests.Load() != 1 || !reflect.DeepEqual(order, []string{"test-current", "refresh", "test-refreshed"}) {
			t.Fatalf("refreshes=%d order=%v", refreshRequests.Load(), order)
		}
		persisted, err := srv.store.GetConfig(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		credential, err := xaiauth.ParseCredential([]byte(persisted.OAuthCredential))
		if err != nil || credential.AccessToken != "at-refreshed" || credential.RefreshToken != "rt-refreshed" {
			t.Fatalf("persisted refreshed credential=%v err=%v", credential, err)
		}
	})
}

func TestHandleChannelTest_CodexOAuthUsageLimitCoolsModel(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7260}}`)
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	updated := created.Clone()
	updated.ModelEntries = append(updated.ModelEntries, model.ModelEntry{Model: "gpt-5.4"})
	created, err := srv.store.UpdateConfig(context.Background(), created.ID, updated)
	if err != nil {
		t.Fatalf("add unaffected Codex model: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "codex",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["cooldown_action"].(string); got != "model_cooldown_applied" {
		t.Fatalf("cooldown_action=%q, want model_cooldown_applied, data=%+v", got, resp.Data)
	}
	cooldowns, err := srv.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	until := cooldowns[created.ID]["gpt-5.6-sol"]
	if remaining := time.Until(until); remaining < 7250*time.Second || remaining > 7270*time.Second {
		t.Fatalf("model cooldown remaining=%v, want about 7260s", remaining)
	}
	if _, exists := cooldowns[created.ID]["gpt-5.4"]; exists {
		t.Fatal("unaffected Codex model must not be cooled")
	}
}

func TestHandleChannelTest_CodexOAuthTransformsOpenAIWithoutSSEContentType(t *testing.T) {
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"translated answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	updated := created.Clone()
	updated.ProtocolTransformMode = model.ProtocolTransformModeLocal
	created, err := srv.store.UpdateConfig(context.Background(), created.ID, updated)
	if err != nil {
		t.Fatalf("enable local protocol transform: %v", err)
	}
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "openai",
		"stream":          true,
		"content":         "which header carries the API key?",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("OpenAI -> Codex OAuth channel test failed: %+v", resp)
	}
	if got, _ := resp.Data["response_text"].(string); got != "translated answer" {
		t.Fatalf("response_text=%q, want translated answer; data=%+v", got, resp.Data)
	}
	if len(upstreamBody) == 0 || strings.Contains(string(upstreamBody), `"messages"`) || !strings.Contains(string(upstreamBody), `"input"`) {
		t.Fatalf("request was not converted to Codex Responses: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "reasoning.effort").String(); got != "medium" {
		t.Fatalf("default Codex reasoning.effort=%q, want medium; body=%s", got, upstreamBody)
	}
}

func TestHandleChannelTest_CodexOAuthForcesStreamingUpstreamForNonStreamTest(t *testing.T) {
	var upstreamBody []byte
	var upstreamAccept string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		upstreamAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"forced stream answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_forced\",\"status\":\"completed\"}}\n\n")
	}))

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "gpt-5.6-sol",
		"client_protocol": "codex",
		"stream":          false,
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !resp.Success || !success {
		t.Fatalf("Codex OAuth non-stream channel test failed: %+v", resp)
	}
	if !gjson.GetBytes(upstreamBody, "stream").Bool() {
		t.Fatalf("upstream stream must be true: %s", upstreamBody)
	}
	if upstreamAccept != "text/event-stream" {
		t.Fatalf("upstream Accept=%q, want text/event-stream", upstreamAccept)
	}
	if got, _ := resp.Data["response_text"].(string); got != "forced stream answer" {
		t.Fatalf("response_text=%q, want forced stream answer; data=%+v", got, resp.Data)
	}
}

// TestHandleChannelTest_UnsupportedModel 渠道存在、有 Key，但模型不支持
func TestHandleChannelTest_UnsupportedModel(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	cfg := &model.Config{
		Name:         "limited-model-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 添加 API key
	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "test-key-001"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "gpt-4-not-supported",
		"client_protocol": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（模型不支持）")
	}
}

func TestHandleChannelTest_RejectsMissingClientProtocol(t *testing.T) {
	var gotPath string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "default-protocol-transform-openai",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-4.1"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model": "gpt-4.1",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if gotPath != "" {
		t.Fatalf("missing client protocol must not reach upstream, path=%q", gotPath)
	}
}

func TestHandleChannelTest_RejectsUnknownClientProtocol(t *testing.T) {
	failCalls := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "unsupported-protocol-transform-openai",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4.1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "gpt-4.1",
		"client_protocol": "unknown",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if failCalls != 0 {
		t.Fatalf("expected no upstream request, failCalls=%d", failCalls)
	}
}

func TestHandleChannelTest_UsesSelectedOpenAIProtocol(t *testing.T) {
	var gotPath string
	var gotBody string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPath = r.URL.Path
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_test",
			"object": "chat.completion",
			"choices": [{"message": {"role": "assistant", "content": "native openai ok"}}],
			"model": "claude-3-5-sonnet",
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "anthropic-with-runtime-openai-transform",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "openai",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path=%q, want %q", gotPath, "/v1/chat/completions")
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Fatalf("expected openai request body, body=%s", gotBody)
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if _, ok := apiResp["choices"]; !ok {
		t.Fatalf("expected openai-compatible api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_UsesSelectedCodexProtocolWithBasePathPrefix(t *testing.T) {
	var gotPath string
	var gotBody string
	var gotHeaders http.Header

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPath = r.URL.Path
		gotBody = string(body)
		gotHeaders = r.Header.Clone()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "resp_test",
			"object": "response",
			"status": "completed",
			"output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "native codex ok"}]}],
			"model": "claude-3-5-sonnet",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "anthropic-with-prefixed-base-path",
		URLs:                  model.ChannelURLs{{URL: upstream.URL + "/anthropic"}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "codex",
		"content":         "hello",
		"headers": map[string]string{
			"X-Client-Request-Id": "admin-test-request",
			"X-Codex-Turn-State":  "turn-state",
			"X-Arbitrary-Client":  "must-not-leak",
		},
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if gotPath != "/anthropic/v1/responses" {
		t.Fatalf("path=%q, want %q", gotPath, "/anthropic/v1/responses")
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Fatalf("Authorization=%q, want bearer channel key", got)
	}
	if gotHeaders.Get("X-Api-Key") != "" || gotHeaders.Get("X-Arbitrary-Client") != "" {
		t.Fatalf("unapproved Codex HTTP headers leaked upstream: %v", gotHeaders)
	}
	if gotHeaders.Get("User-Agent") != codexUserAgent ||
		gotHeaders.Get("Originator") != codexOriginator ||
		gotHeaders.Get("Version") != codexVersion {
		t.Fatalf("Codex identity headers=%v", gotHeaders)
	}
	if got := gotHeaders.Get("X-Codex-Turn-State"); got != "turn-state" {
		t.Fatalf("X-Codex-Turn-State=%q, want allowed downstream value", got)
	}
	if got := gotHeaders.Get("X-Client-Request-Id"); got != "admin-test-request" {
		t.Fatalf("X-Client-Request-Id=%q, want allowed downstream value", got)
	}
	if gotHeaders.Get("Content-Type") != "application/json" || gotHeaders.Get("Accept") != "application/json" || gotHeaders.Get("Connection") != "Keep-Alive" {
		t.Fatalf("Codex HTTP transport headers=%v", gotHeaders)
	}
	if !strings.Contains(gotBody, `"input"`) {
		t.Fatalf("expected codex request body, body=%s", gotBody)
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if _, ok := apiResp["object"]; !ok {
		t.Fatalf("expected codex-compatible api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_UsesSelectedProtocolEndpoint(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"endpoint not found"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"claude-3-5-sonnet","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "codex-upstream",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}) {
		t.Fatalf("paths=%v, want Anthropic, OpenAI, then Codex", gotPaths)
	}
	var nativeRequest map[string]any
	if err := json.Unmarshal(gotBodies[0], &nativeRequest); err != nil {
		t.Fatalf("decode native request: %v", err)
	}
	if _, ok := nativeRequest["messages"].([]any); !ok {
		t.Fatalf("expected anthropic messages array, body=%s", gotBodies[0])
	}
	if stream, _ := nativeRequest["stream"].(bool); !stream {
		t.Fatalf("expected stream=true, body=%s", gotBodies[0])
	}
	if _, ok := nativeRequest["max_tokens"]; !ok {
		t.Fatalf("expected anthropic max_tokens, body=%s", gotBodies[0])
	}
	var openAIRequest map[string]any
	if err := json.Unmarshal(gotBodies[1], &openAIRequest); err != nil {
		t.Fatalf("decode OpenAI request: %v", err)
	}
	if _, ok := openAIRequest["messages"].([]any); !ok {
		t.Fatalf("expected OpenAI messages array, body=%s", gotBodies[1])
	}
	var codexRequest map[string]any
	if err := json.Unmarshal(gotBodies[2], &codexRequest); err != nil {
		t.Fatalf("decode Codex request: %v", err)
	}
	if _, ok := codexRequest["input"].([]any); !ok {
		t.Fatalf("expected Codex input array, body=%s", gotBodies[2])
	}

	apiResp, ok := resp.Data["api_response"].(map[string]any)
	if !ok {
		t.Fatalf("expected translated api_response map, data=%+v", resp.Data)
	}
	if apiResp["type"] != "message" {
		t.Fatalf("expected anthropic api_response, got=%+v", apiResp)
	}
}

func TestHandleChannelTest_AutoModePrioritizesAutomaticURLBeforeDeclaredConversion(t *testing.T) {
	var automaticHits atomic.Int64
	automatic := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		automaticHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_auto","object":"response","status":"completed","model":"shared-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"direct"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer automatic.Close()

	var declaredHits atomic.Int64
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		declaredHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"converted"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer declared.Close()

	srv := newInMemoryServer(t)
	srv.client = automatic.Client()
	srv.urlSelector = nil
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "channel-test-auto-original-first",
		URLs: model.ChannelURLs{
			{URL: declared.URL, Protocols: []string{"anthropic"}},
			{URL: automatic.URL},
		},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "shared-model"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key",
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "shared-model",
		"client_protocol": "codex",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got := automaticHits.Load(); got != 1 {
		t.Fatalf("automatic URL hits=%d, want one native Codex request", got)
	}
	if got := declaredHits.Load(); got != 0 {
		t.Fatalf("declared conversion URL hits=%d, want 0 when automatic URL accepts Codex", got)
	}
}

func TestHandleChannelTest_AutoFallsBackOnNonModelDeployment404(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404: Not Found (DEPLOYMENT_NOT_FOUND)\n\nThe requested deployment does not exist."))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-4.5-haiku"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-4.5-haiku",
		"client_protocol": "codex",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{
		"/v1/responses",
		"/v1/chat/completions",
		"/v1/messages",
		"/v1beta/models/claude-4.5-haiku:streamGenerateContent",
	}) {
		t.Fatalf("paths=%v, want native Codex then OpenAI, Anthropic, Gemini", gotPaths)
	}
	if len(gotBodies) != 4 {
		t.Fatalf("request count=%d, want 4", len(gotBodies))
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if got, _ := resp.Data["upstream_protocol"].(string); got != "gemini" {
		t.Fatalf("upstream_protocol=%q, want gemini, data=%+v", got, resp.Data)
	}
	if got, _ := resp.Data["cooldown_action"].(string); got != "channel_cooldown_applied" {
		t.Fatalf("cooldown_action=%q, want channel_cooldown_applied, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnCloudflareBlockPage(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)

		switch {
		case strings.Contains(r.URL.Path, ":streamGenerateContent"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		case r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body><h1>Sorry, you have been blocked</h1><p>Cloudflare Ray ID: test</p></body></html>`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto-cloudflare-block",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "test-model",
		"client_protocol": "gemini",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{
		"/v1beta/models/test-model:streamGenerateContent",
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
	}) {
		t.Fatalf("paths=%v, want native Gemini then OpenAI, Anthropic, Codex", gotPaths)
	}
	if !gjson.GetBytes(gotBodies[0], "contents").IsArray() {
		t.Fatalf("native Gemini request must use contents: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("OpenAI request must use messages: %s", gotBodies[1])
	}
	if !gjson.GetBytes(gotBodies[2], "messages").IsArray() {
		t.Fatalf("Anthropic request must use messages: %s", gotBodies[2])
	}
	if got := gjson.GetBytes(gotBodies[2], "messages.0.role").String(); got != "user" {
		t.Fatalf("Anthropic request role=%q, want user: %s", got, gotBodies[2])
	}
	if !gjson.GetBytes(gotBodies[3], "input").IsArray() {
		t.Fatalf("Codex request must use input: %s", gotBodies[3])
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "codex" {
		t.Fatalf("upstream_protocol=%q, want codex, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnUnsupportedAnthropicBeta(t *testing.T) {
	var gotPaths []string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"尚未验证或不支持的 anthropic-beta：claude-code-20250219"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "auto-unsupported-anthropic-beta",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "deepseek-v4-flash"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "deepseek-v4-flash",
		"client_protocol": "anthropic",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/messages", "/v1/chat/completions"}) {
		t.Fatalf("paths=%v, want native Anthropic then OpenAI", gotPaths)
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "openai" {
		t.Fatalf("upstream_protocol=%q, want openai, data=%+v", got, resp.Data)
	}
	if _, exists := resp.Data["cooldown_action"]; exists {
		t.Fatalf("capability fallback must not apply cooldown, data=%+v", resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnResponsesModelNotSupported(t *testing.T) {
	var gotPaths []string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"当前模型不支持 Responses API：deepseek-v4-flash","type":"invalid_request_error","param":null,"code":"RESPONSES_MODEL_NOT_SUPPORTED"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "auto-responses-model-not-supported",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "deepseek-v4-flash"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "deepseek-v4-flash",
		"client_protocol": "codex",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/responses", "/v1/chat/completions"}) {
		t.Fatalf("paths=%v, want client Codex then OpenAI", gotPaths)
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "openai" {
		t.Fatalf("upstream_protocol=%q, want openai, data=%+v", got, resp.Data)
	}
	if _, exists := resp.Data["cooldown_action"]; exists {
		t.Fatalf("capability fallback must not apply cooldown, data=%+v", resp.Data)
	}
}

func TestHandleChannelTest_AutoTriesClientThenFallbackProtocols(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "auto-native-then-fallback",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "test-model",
		"client_protocol": "gemini",
		"content":         "hello",
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{
		"/v1beta/models/test-model:generateContent",
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
	}) {
		t.Fatalf("paths=%v, want client Gemini then OpenAI, Anthropic, Codex", gotPaths)
	}
	if !gjson.GetBytes(gotBodies[0], "contents").IsArray() {
		t.Fatalf("native Gemini request must contain contents: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("OpenAI request must contain messages: %s", gotBodies[1])
	}
	if !gjson.GetBytes(gotBodies[2], "messages").IsArray() {
		t.Fatalf("Anthropic request must contain messages: %s", gotBodies[2])
	}
	if !gjson.GetBytes(gotBodies[3], "input").IsArray() {
		t.Fatalf("Codex request must contain input: %s", gotBodies[3])
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "codex" {
		t.Fatalf("upstream_protocol=%q, want codex, data=%+v", got, resp.Data)
	}
}

func TestHandleChannelTest_AutoFallsBackOnConvertRequestNotImplemented(t *testing.T) {
	var gotPaths []string
	var gotBodies [][]byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		gotPaths = append(gotPaths, r.URL.Path)
		gotBodies = append(gotBodies, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		case "/v1/messages":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"not implemented (request id: req_test)","type":"new_api_error","param":"","code":"convert_request_failed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"claude-4.5-haiku","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-auto-convert-not-implemented",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "claude-4.5-haiku"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "claude-4.5-haiku",
		"client_protocol": "openai",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}) {
		t.Fatalf("paths=%v, want client OpenAI then Anthropic and Codex", gotPaths)
	}
	if len(gotBodies) != 3 {
		t.Fatalf("request count=%d, want 3", len(gotBodies))
	}
	if !gjson.GetBytes(gotBodies[0], "messages").IsArray() {
		t.Fatalf("client OpenAI request must use messages: %s", gotBodies[0])
	}
	if !gjson.GetBytes(gotBodies[1], "messages").IsArray() {
		t.Fatalf("Anthropic request must use messages: %s", gotBodies[1])
	}
	if !gjson.GetBytes(gotBodies[2], "input").IsArray() {
		t.Fatalf("Codex fallback request must use input: %s", gotBodies[2])
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "codex" {
		t.Fatalf("upstream_protocol=%q, want codex, data=%+v", got, resp.Data)
	}
}

func TestChannelTest_StrictProtocolTransformModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantPath    string
		wantSuccess bool
	}{
		{name: "upstream", mode: model.ProtocolTransformModeUpstream, wantPath: "/v1/messages"},
		{name: "local", mode: model.ProtocolTransformModeLocal, wantPath: "/v1/responses", wantSuccess: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			var bodies [][]byte
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, body)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/messages" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":{"message":"endpoint not found"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()

			srv := newInMemoryServer(t)
			srv.client = upstream.Client()
			urlEntry := model.ChannelURL{URL: upstream.URL}
			if tt.mode == model.ProtocolTransformModeLocal {
				urlEntry.Protocols = []string{util.ProtocolCodex}
			}
			result := srv.testChannelAPI(context.Background(), &model.Config{
				ID: 1, Name: "strict", URLs: model.ChannelURLs{urlEntry}, ProtocolTransformMode: tt.mode,
				ModelEntries: []model.ModelEntry{{Model: "test-model"}},
			}, "sk-test", &testutil.TestChannelRequest{
				Model: "test-model", ClientProtocol: "anthropic", Content: "hello",
			})

			if success, _ := result["success"].(bool); success != tt.wantSuccess {
				t.Fatalf("success=%v, want %v, result=%+v", success, tt.wantSuccess, result)
			}
			if !reflect.DeepEqual(paths, []string{tt.wantPath}) {
				t.Fatalf("paths=%v, want [%s]", paths, tt.wantPath)
			}
			var requestBody map[string]any
			if err := json.Unmarshal(bodies[0], &requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if tt.mode == model.ProtocolTransformModeLocal {
				if _, ok := requestBody["input"].([]any); !ok {
					t.Fatalf("local request must use Codex input: %+v", requestBody)
				}
			} else if _, ok := requestBody["messages"].([]any); !ok {
				t.Fatalf("upstream request must use Anthropic messages: %+v", requestBody)
			}
		})
	}
}

func TestChannelTest_LocalModeUsesDeclaredProtocolForAutomaticBackupURL(t *testing.T) {
	var declaredPaths []string
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		declaredPaths = append(declaredPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
	}))
	defer declared.Close()

	var backupPaths []string
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupPaths = append(backupPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer backup.Close()

	srv := newInMemoryServer(t)
	result := srv.testChannelAPI(context.Background(), &model.Config{
		ID:   1,
		Name: "local-declared-protocol-backup",
		URLs: model.ChannelURLs{
			{URL: backup.URL},
			{URL: declared.URL, Protocols: []string{util.ProtocolCodex}},
		},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
	}, "sk-test", &testutil.TestChannelRequest{
		Model: "test-model", ClientProtocol: util.ProtocolOpenAI, Content: "hello",
	})

	if success, _ := result["success"].(bool); !success {
		t.Fatalf("expected backup success, result=%+v", result)
	}
	if !reflect.DeepEqual(declaredPaths, []string{"/v1/responses"}) {
		t.Fatalf("declared paths=%v, want codex first", declaredPaths)
	}
	if !reflect.DeepEqual(backupPaths, []string{"/v1/responses"}) {
		t.Fatalf("backup paths=%v, want declared codex protocol", backupPaths)
	}
	if got, _ := result["upstream_protocol"].(string); got != util.ProtocolCodex {
		t.Fatalf("upstream_protocol=%q, want codex; result=%+v", got, result)
	}
}

// TestHandleChannelTest_SuccessfulAPI 使用 mock server 模拟成功的 API 调用
func TestHandleChannelTest_SuccessfulAPI(t *testing.T) {
	// 创建 mock 上游服务器，返回成功的 Anthropic 响应
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	// 替换 HTTP client 以使用 mock server
	srv.client = upstream.Client()

	ctx := context.Background()

	cfg := &model.Config{
		Name:         "test-success-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("外层 APIResponse.Success 应为 true, error=%q", resp.Error)
	}

	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true（API 调用成功）, data=%+v", resp.Data)
	}

	stats := srv.urlSelector.GetURLStats(created.ID, created.GetURLs())
	if len(stats) != 1 || stats[0].Requests != 1 || stats[0].Failures != 0 {
		t.Fatalf("模型测试成功应计入 URL 调用统计: %+v", stats)
	}
}

func TestHandleChannelTest_OpenAIRequestIncludesSessionID(t *testing.T) {
	var gotSessionID string
	var gotBody []byte

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = r.Header.Get("Session_id")
		if got := r.Header.Get("Session-Id"); got != "" {
			t.Fatalf("Session-Id header should be omitted, got %q", got)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_test",
			"object": "chat.completion",
			"model": "gpt-test",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-test-session-id",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-test"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "gpt-test",
		"client_protocol": "openai",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}
	if !uuidPattern.MatchString(gotSessionID) {
		t.Fatalf("Session_id header missing or invalid: %q", gotSessionID)
	}
	var upstreamBody map[string]any
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("unmarshal upstream body failed: %v; body=%s", err, gotBody)
	}
	if got, _ := upstreamBody["user"].(string); got != gotSessionID {
		t.Fatalf("body user = %q, want session id %q; body=%s", got, gotSessionID, gotBody)
	}
	if got, _ := upstreamBody["prompt_cache_key"].(string); got != gotSessionID {
		t.Fatalf("body prompt_cache_key = %q, want session id %q; body=%s", got, gotSessionID, gotBody)
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true, data=%+v", resp.Data)
	}
}

// TestHandleChannelTest_FailedAPI 使用 mock server 模拟失败的 API 调用
func TestHandleChannelTest_FailedAPI(t *testing.T) {
	// 创建 mock 上游服务器，返回 401 错误
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()

	cfg := &model.Config{
		Name:         "test-fail-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-invalid-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatal("data.success 应为 false（API 调用失败 401）")
	}

	// 验证冷却决策被记录
	if action, ok := resp.Data["cooldown_action"].(string); ok {
		if action == "" {
			t.Fatal("失败时应有冷却决策记录")
		}
		t.Logf("冷却决策: %s", action)
	}

	stats := srv.urlSelector.GetURLStats(created.ID, created.GetURLs())
	if len(stats) != 1 || stats[0].Requests != 0 || stats[0].Failures != 1 {
		t.Fatalf("模型测试失败应计入 URL 调用统计: %+v", stats)
	}
}

func TestHandleChannelTest_HonorsRequestedKeyIndexEvenIfCooled(t *testing.T) {
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-honor-cooled-key-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-cooled"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-fresh"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"key_index":       0,
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if dataSuccess, _ := resp.Data["success"].(bool); !dataSuccess {
		t.Fatalf("data.success=false, data=%+v", resp.Data)
	}
	if gotAuth != "Bearer sk-cooled" {
		t.Fatalf("Authorization=%q, want Bearer sk-cooled (requested key must be honored even if cooled)", gotAuth)
	}
	if gotIndex, _ := resp.Data["tested_key_index"].(float64); gotIndex != 0 {
		t.Fatalf("tested_key_index=%v, want 0", resp.Data["tested_key_index"])
	}
}

// TestHandleChannelTest_RejectsUnknownKeyIndex 验证：请求一个不存在的 key_index 时直接报错，
// 不再静默回退到其他可用 Key（既往会调用 SelectAvailableKey）。配合 HonorsRequestedKeyIndexEvenIfCooled
// 共同保证"显式 key_index 即真"语义。
func TestHandleChannelTest_RejectsUnknownKeyIndex(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-reject-unknown-key-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-only"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"key_index":       99, // 不存在
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("data.success=true, want false; data=%+v", resp.Data)
	}
	dataError, _ := resp.Data["error"].(string)
	if !strings.Contains(dataError, "Key #99") {
		t.Fatalf("data.error=%q, want mention of Key #99", dataError)
	}
}

func TestHandleChannelTest_UsesRequestAPIKeyWithoutTouchingSavedCooldown(t *testing.T) {
	mockResp := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello"}],
		"model": "claude-3-5-sonnet",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	var gotAuth string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "test-request-key-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-saved-key"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}
	coolUntil := time.Now().Add(10 * time.Minute)
	if err := srv.store.SetKeyCooldown(ctx, created.ID, 0, coolUntil); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"key_index":       1,
		"api_key":         "sk-unsaved-key",
	}))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if dataSuccess, _ := resp.Data["success"].(bool); !dataSuccess {
		t.Fatalf("data.success=false, data=%+v", resp.Data)
	}
	if gotAuth != "Bearer sk-unsaved-key" {
		t.Fatalf("Authorization=%q, want request api key", gotAuth)
	}

	keys, err := srv.store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys len=%d, want 1", len(keys))
	}
	if keys[0].CooldownUntil == 0 {
		t.Fatalf("saved key cooldown was reset for an unsaved request key")
	}
}

func TestHandleChannelTest_WritesManualTestLog(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	now := time.Now().Add(-time.Minute)

	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:         "manual-test-log-channel",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-invalid-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
	}))
	c.Request.RemoteAddr = "198.51.100.10:12345"
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	logs, err := srv.store.ListLogs(ctx, now, 10, 0, &model.LogFilter{LogSource: model.LogSourceManualTest})
	if err != nil {
		t.Fatalf("ListLogs failed: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 manual test log, got %d", len(logs))
	}
	entry := logs[0]
	if entry.LogSource != model.LogSourceManualTest {
		t.Fatalf("LogSource=%q, want %q", entry.LogSource, model.LogSourceManualTest)
	}
	if entry.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode=%d, want %d", entry.StatusCode, http.StatusUnauthorized)
	}
	if entry.ClientIP != "198.51.100.10" {
		t.Fatalf("ClientIP=%q, want %q", entry.ClientIP, "198.51.100.10")
	}
	if entry.AuthTokenID != 0 {
		t.Fatalf("AuthTokenID=%d, want 0", entry.AuthTokenID)
	}
	if entry.BaseURL != upstream.URL {
		t.Fatalf("BaseURL=%q, want %q", entry.BaseURL, upstream.URL)
	}
}

func TestHandleChannelTest_SSESoftErrorTriggersCooldown(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: \n")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"code\":\"1113\",\"message\":\"Insufficient balance or no resource package. Please recharge.\"},\"request_id\":\"req_1113\"}\n\n")
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "test-sse-soft-error",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "HTTP 200 soft error", Priority: 0, StatusCodes: []int{http.StatusOK},
			MessagePattern: "Insufficient balance", Scope: model.CooldownScopeChannel,
			Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-soft-error"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "claude-3-5-sonnet",
		"client_protocol": "anthropic",
		"stream":          true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("外层 APIResponse.Success 应为 true, error=%q", resp.Error)
	}

	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	if got, _ := resp.Data["error"].(string); got != "Insufficient balance or no resource package. Please recharge." {
		t.Fatalf("错误信息不对，got=%q data=%+v", got, resp.Data)
	}

	if got, _ := resp.Data["cooldown_action"].(string); got != "channel_cooldown_applied" {
		t.Fatalf("1113 软错误在单 Key 渠道应升级为渠道冷却，got=%q data=%+v", got, resp.Data)
	}

	cooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[created.ID]
	if !exists {
		t.Fatalf("HTTP 200 自定义规则未写入渠道冷却")
	}
	if remaining := time.Until(until); remaining < 85*time.Second || remaining > 95*time.Second {
		t.Fatalf("渠道冷却剩余时间=%v，期望约 90 秒", remaining)
	}
}

func TestHandleChannelTest_EventStreamHeaderWithJSONBodyFallback(t *testing.T) {
	// 模拟“Content-Type=event-stream，但实际返回完整JSON”场景
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"resp_test",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"content":[{"type":"output_text","text":"fallback text"}]
				}
			],
			"usage":{"input_tokens":12,"output_tokens":8}
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "test-codex-json-fallback",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.2"}},
		Enabled:               true,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "gpt-5.2",
		"client_protocol": "codex",
		"stream":          false,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if !dataSuccess {
		t.Fatalf("data.success 应为 true, data=%+v", resp.Data)
	}

	responseText, _ := resp.Data["response_text"].(string)
	if responseText == "" {
		t.Fatalf("应解析出 response_text, data=%+v", resp.Data)
	}
	if responseText != "fallback text" {
		t.Fatalf("response_text 解析错误: %q", responseText)
	}

	message, _ := resp.Data["message"].(string)
	if message != "API测试成功" {
		t.Fatalf("应按非流式成功文案返回，实际: %q", message)
	}
}

func TestHandleChannelTest_CodexJSONFailedResponseShouldBeFailure(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"resp_failed",
			"object":"response",
			"status":"failed",
			"error":{
				"code":"server_error",
				"message":"upstream failed"
			},
			"output":[]
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "test-codex-json-failed",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:               true,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "gpt-5.4",
		"client_protocol": "codex",
		"stream":          false,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	if errorMsg != "upstream failed" {
		t.Fatalf("应返回上游错误信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}

	if message, _ := resp.Data["message"].(string); message != "" {
		t.Fatalf("失败响应不应返回成功文案，实际: %q", message)
	}
}

func TestHandleChannelTest_StringAPIErrorShouldExposeUpstreamMessage(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{
			"error":"由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试",
			"type":"error"
		}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "test-string-api-error",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:      true,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "gpt-5.4",
		"client_protocol": "openai",
		"stream":          false,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("data.success 应为 false, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	expected := "由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试"
	if errorMsg != expected {
		t.Fatalf("应返回上游字符串错误信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}
}

func TestHandleChannelTest_HTMLBlockPageShouldBeFailure(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
	<meta charset="UTF-8">
	<title>您的IP已被封锁</title>
</head>
<body>
	<div class="container">
		<h1>当前 IP 已被封锁</h1>
		<p>暂时无法访问本站内容。</p>
	</div>
</body>
</html>`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()

	ctx := context.Background()
	cfg := &model.Config{
		Name:                  "test-html-block-page",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-5.4"}},
		Enabled:               true,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"},
	})
	if err != nil {
		t.Fatalf("添加 API key 失败: %v", err)
	}

	channelID := fmt.Sprintf("%d", created.ID)
	reqBody := map[string]any{
		"model":           "gpt-5.4",
		"client_protocol": "openai",
		"stream":          false,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+channelID+"/test", reqBody))
	c.Params = gin.Params{{Key: "id", Value: channelID}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	dataSuccess, _ := resp.Data["success"].(bool)
	if dataSuccess {
		t.Fatalf("HTML 封禁页必须判定为失败, data=%+v", resp.Data)
	}

	errorMsg, _ := resp.Data["error"].(string)
	if !strings.Contains(errorMsg, "IP") || !strings.Contains(errorMsg, "封锁") {
		t.Fatalf("应提炼出上游封禁信息，实际: %q, data=%+v", errorMsg, resp.Data)
	}

	rawResp, _ := resp.Data["raw_response"].(string)
	if !strings.Contains(rawResp, "<title>您的IP已被封锁</title>") {
		t.Fatalf("应保留原始 HTML 响应，实际: %q", rawResp)
	}

	if message, _ := resp.Data["message"].(string); message != "" {
		t.Fatalf("失败响应不应返回成功文案，实际: %q", message)
	}
}

func TestShouldFallbackToNextURL_StructuredSoftErrors(t *testing.T) {
	t.Run("key_level_soft_error_should_not_fallback_or_cooldown_url", func(t *testing.T) {
		result := map[string]any{
			"success":     false,
			"status_code": http.StatusOK,
			"api_error": map[string]any{
				"error": map[string]any{
					"code":    "1113",
					"message": "Insufficient balance or no resource package. Please recharge.",
				},
			},
			"response_headers": map[string]string{
				"Content-Type": "text/event-stream",
			},
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(result)
		if continueFallback || shouldCooldown {
			t.Fatalf("Key级软错误不应继续切URL或冷却URL，got fallback=%v cooldown=%v", continueFallback, shouldCooldown)
		}
	})

	t.Run("channel_level_soft_error_should_fallback_and_cooldown_url", func(t *testing.T) {
		result := map[string]any{
			"success":     false,
			"status_code": http.StatusOK,
			"api_error": map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream overloaded",
				},
			},
		}

		continueFallback, shouldCooldown := shouldFallbackToNextURL(result)
		if !continueFallback || !shouldCooldown {
			t.Fatalf("渠道级软错误应继续切URL并冷却当前URL，got fallback=%v cooldown=%v", continueFallback, shouldCooldown)
		}
	})
}

func TestExtractSSEErrorMessage_ResponseFailedNestedError(t *testing.T) {
	obj := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":     "resp_5ca0fb7943504d6a93576c7fb7e3a760",
			"object": "response",
			"model":  "gpt-5.6-sol",
			"status": "failed",
			"output": []any{},
			"error": map[string]any{
				"code":    "rate_limit_exceeded",
				"message": "Upstream rate limit exceeded, please retry later",
			},
		},
	}
	msg, raw, matched := extractSSEErrorMessage(obj)
	if !matched {
		t.Fatal("response.failed nested error must match")
	}
	if msg != "Upstream rate limit exceeded, please retry later" {
		t.Fatalf("msg=%q, want nested error message", msg)
	}
	if raw == nil {
		t.Fatal("raw payload must be returned")
	}
}

func TestHandleChannelImageGeneration_ForwardsImagesWireContract(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotAuthorization string
	var gotAPIKeyHeader string
	var gotBody map[string]any

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode image request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"created": 1720000000,
			"background": "transparent",
			"output_format": "webp",
			"quality": "high",
			"size": "1024x1024",
			"data": [
				{"b64_json": "aW1hZ2U=", "revised_prompt": "A white cat"},
				{"url": "https://example.com/generated.webp"}
			],
			"usage": {"input_tokens": 4, "output_tokens": 7}
		}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "openai-images-admin-test",
		URLs:                  model.ChannelURLs{{URL: upstream.URL, Protocols: []string{util.ProtocolOpenAI}}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries: []model.ModelEntry{{
			Model:         "image-alias",
			RedirectModel: "gpt-image-2",
		}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-unused"},
		{ChannelID: created.ID, KeyIndex: 3, APIKey: "sk-image-test"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "image-alias",
		"prompt":         "A white cat",
		"size":           "auto",
		"quality":        "high",
		"background":     "transparent",
		"output_format":  "webp",
		"key_index":      3,
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/images/generations" {
		t.Fatalf("upstream request=%s %s", gotMethod, gotPath)
	}
	if gotAuthorization != "Bearer sk-image-test" {
		t.Fatalf("Authorization=%q", gotAuthorization)
	}
	if gotAPIKeyHeader != "sk-image-test" {
		t.Fatalf("x-api-key=%q", gotAPIKeyHeader)
	}
	if gotBody["model"] != "gpt-image-2" || gotBody["prompt"] != "A white cat" {
		t.Fatalf("upstream body=%v", gotBody)
	}
	if _, exists := gotBody["size"]; exists {
		t.Fatalf("automatic size must be omitted, body=%v", gotBody)
	}
	if gotBody["quality"] != "high" || gotBody["background"] != "transparent" || gotBody["output_format"] != "webp" {
		t.Fatalf("upstream image options=%v", gotBody)
	}
	if _, exists := gotBody["n"]; exists {
		t.Fatalf("image test must use the upstream single-image default, body=%v", gotBody)
	}

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("image generation failed: %v", response.Data)
	}
	if response.Data["actual_model"] != "gpt-image-2" || response.Data["tested_key_index"] != float64(3) {
		t.Fatalf("response routing metadata=%v", response.Data)
	}
	images, ok := response.Data["images"].([]any)
	if !ok || len(images) != 2 {
		t.Fatalf("response images=%v", response.Data["images"])
	}
	if _, exists := response.Data["upstream_response_body"]; exists {
		t.Fatal("successful base64 response must not be duplicated in upstream_response_body")
	}
}

func TestHandleChannelImageGeneration_RejectsUnsupportedOAuthChannel(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	created := createAntigravityOAuthChannelForAdminTest(t, srv, upstream.URL)
	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "gemini-3-flash",
		"prompt":         "A white cat",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); success {
		t.Fatalf("OAuth Images request unexpectedly succeeded: %v", response.Data)
	}
	if message, _ := response.Data["error"].(string); !strings.Contains(message, "Chat Completions") {
		t.Fatalf("error=%q, want Chat Completions guidance", message)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("OAuth Images request reached upstream %d times", upstreamCalls.Load())
	}
}

func TestHandleChannelImageGeneration_CodexOAuthUsesDirectImagesAPI(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotAuthorization string
	var gotAccountID string
	var gotOriginator string
	var gotVersion string
	var gotUserAgent string
	var gotSessionID string
	var gotAcceptEncoding string
	var gotBody map[string]any
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("ChatGPT-Account-ID")
		gotOriginator = r.Header.Get("Originator")
		gotVersion = r.Header.Get("Version")
		gotUserAgent = r.Header.Get("User-Agent")
		gotSessionID = r.Header.Get("Session_id")
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode Codex Images request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}],"output_format":"png","size":"1024x1024"}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createCodexOAuthChannelForAdminTest(t, srv, upstream.URL+"/backend-api/codex/responses")
	updated := created.Clone()
	updated.ModelEntries = []model.ModelEntry{{Model: "gpt-image-2"}}
	if _, err := srv.store.UpdateConfig(context.Background(), created.ID, updated); err != nil {
		t.Fatalf("enable Codex image model: %v", err)
	}

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "codex/gpt-image-2",
		"prompt":         "A white cat",
		"size":           "1024x1024",
		"quality":        "high",
		"background":     "transparent",
		"output_format":  "png",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/backend-api/codex/images/generations" {
		t.Fatalf("Codex upstream request=%s %s", gotMethod, gotPath)
	}
	if gotAuthorization != "Bearer at-admin-test" || gotAccountID != "account-admin-test" {
		t.Fatalf("Codex auth headers: Authorization=%q Account-ID=%q", gotAuthorization, gotAccountID)
	}
	if gotOriginator != codexOriginator || gotVersion != codexVersion || gotUserAgent != codexUserAgent || gotSessionID == "" {
		t.Fatalf("Codex identity headers: Originator=%q Version=%q User-Agent=%q Session_id=%q", gotOriginator, gotVersion, gotUserAgent, gotSessionID)
	}
	if gotAcceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding=%q, want identity", gotAcceptEncoding)
	}
	if gotBody["model"] != "gpt-image-2" || gotBody["prompt"] != "A white cat" || gotBody["size"] != "1024x1024" {
		t.Fatalf("Codex Images body=%v", gotBody)
	}
	if gotBody["quality"] != "high" || gotBody["background"] != "transparent" || gotBody["output_format"] != "png" {
		t.Fatalf("Codex Images options=%v", gotBody)
	}

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("Codex image generation failed: %v", response.Data)
	}
	if response.Data["actual_model"] != "gpt-image-2" || response.Data["tested_key_index"] != float64(cooldown.NoKeyIndex) {
		t.Fatalf("Codex response routing metadata=%v", response.Data)
	}
}

func TestHandleChannelImageGeneration_AntigravityUsesChatCompletions(t *testing.T) {
	var gotPath string
	var gotAuthorization string
	var gotAcceptEncoding string
	var gotBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, `{
			"response": {
				"modelVersion": "gemini-3.1-flash-image",
				"candidates": [{
					"index": 0,
					"content": {"role": "model", "parts": [{
						"inlineData": {"mimeType": "image/webp", "data": "aW1hZ2U="}
					}]},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 3, "totalTokenCount": 5}
			}
		}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	created := createAntigravityOAuthChannelForAdminTest(t, srv, upstream.URL)
	created.ModelEntries = []model.ModelEntry{{Model: "gemini-3.1-flash-image"}}
	updated, err := srv.store.UpdateConfig(context.Background(), created.ID, created)
	if err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	created = updated

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIChatCompletions,
		"model":          "gemini-3.1-flash-image",
		"prompt":         "A white cat",
		"size":           "3:2@2k",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotPath != "/v1internal:generateContent" {
		t.Fatalf("Antigravity path=%q, want /v1internal:generateContent", gotPath)
	}
	if gotAuthorization != "Bearer at-gravity-admin" {
		t.Fatalf("Authorization=%q", gotAuthorization)
	}
	if gotAcceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding=%q, want identity", gotAcceptEncoding)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gemini-3.1-flash-image" {
		t.Fatalf("upstream model=%q body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "requestType").String(); got != "image_gen" {
		t.Fatalf("requestType=%q body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "request.generationConfig.responseModalities.0").String(); got != "IMAGE" {
		t.Fatalf("response modality=%q body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "request.generationConfig.imageConfig.aspectRatio").String(); got != "3:2" {
		t.Fatalf("aspectRatio=%q body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "request.generationConfig.imageConfig.imageSize").String(); got != "2K" {
		t.Fatalf("imageSize=%q body=%s", got, gotBody)
	}

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("Antigravity image generation failed: %v", response.Data)
	}
	if response.Data["generation_api"] != imageGenerationAPIChatCompletions || response.Data["upstream_protocol"] != util.ProtocolGemini {
		t.Fatalf("response routing metadata=%v", response.Data)
	}
	images, ok := response.Data["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("response images=%v", response.Data["images"])
	}
	image, _ := images[0].(map[string]any)
	if image["b64_json"] != "aW1hZ2U=" || image["mime_type"] != "image/webp" || response.Data["output_format"] != "webp" {
		t.Fatalf("normalized image=%v response=%v", image, response.Data)
	}
	for _, duplicatedField := range []string{"api_response", "upstream_response_body", "cost_usd"} {
		if _, exists := response.Data[duplicatedField]; exists {
			t.Fatalf("image response must omit %s: %v", duplicatedField, response.Data)
		}
	}
}

func TestHandleChannelImageGeneration_AntigravityNoImagePersistsDebugBody(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"response": {
				"modelVersion": "gemini-3.1-flash-image",
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "No image produced"}]},
					"finishReason": "STOP"
				}]
			}
		}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	srv.configService.mu.Lock()
	srv.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	srv.configService.mu.Unlock()
	created := createAntigravityOAuthChannelForAdminTest(t, srv, upstream.URL)
	created.ModelEntries = []model.ModelEntry{{Model: "gemini-3.1-flash-image"}}
	updated, err := srv.store.UpdateConfig(context.Background(), created.ID, created)
	if err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	created = updated

	started := time.Now()
	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIChatCompletions,
		"model":          "gemini-3.1-flash-image",
		"prompt":         "A white cat",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); w.Code != http.StatusOK || success {
		t.Fatalf("status=%d response=%v, want failed upstream result", w.Code, response.Data)
	}
	logs, err := srv.store.ListLogsRange(
		context.Background(), started.Add(-time.Second), time.Now().Add(time.Second), 10, 0,
		&model.LogFilter{LogSource: model.LogSourceDetection},
	)
	if err != nil || len(logs) != 1 {
		t.Fatalf("ListLogsRange logs=%v err=%v", logs, err)
	}
	debugLog, err := srv.store.GetDebugLogByLogID(context.Background(), logs[0].ID)
	if err != nil || debugLog == nil {
		t.Fatalf("GetDebugLogByLogID debug=%v err=%v", debugLog, err)
	}
	if !strings.Contains(string(debugLog.RespBody), "No image produced") {
		t.Fatalf("debug response body=%q", debugLog.RespBody)
	}
	if !strings.Contains(string(debugLog.TranslatedRespBody), "No image produced") {
		t.Fatalf("translated debug response body=%q", debugLog.TranslatedRespBody)
	}
}

func TestHandleChannelImageGeneration_RejectsUnsupportedInterfaceOptions(t *testing.T) {
	t.Run("chat completions", func(t *testing.T) {
		srv := newInMemoryServer(t)
		req := newJSONRequest(t, http.MethodPost, "/admin/channels/1/images/generations", map[string]any{
			"generation_api": imageGenerationAPIChatCompletions,
			"model":          "gemini-3.1-flash-image",
			"prompt":         "A white cat",
			"quality":        "high",
		})
		c, w := newTestContext(t, req)
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		srv.HandleChannelImageGeneration(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("xAI Images", func(t *testing.T) {
		srv := newInMemoryServer(t)
		created := createXAIOAuthChannelForAdminTest(t, srv, "https://unused.example.com/v1")
		req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
			"generation_api": imageGenerationAPIImages,
			"model":          "grok-imagine-image",
			"prompt":         "A white cat",
			"output_format":  "webp",
		})
		c, w := newTestContext(t, req)
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
		srv.HandleChannelImageGeneration(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
		}
	})
}

func TestHandleChannelImageGeneration_XAIOAuthUsesNativeImagesAPI(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotAuthorization string
	var gotContentType string
	var gotAccept string
	var gotAcceptEncoding string
	var gotProhibitedHeaders map[string]string
	var gotBody map[string]any
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		gotProhibitedHeaders = make(map[string]string)
		for _, name := range []string{
			"x-api-key", xaiauth.CLITokenAuthHeader, xaiauth.CLIClientVersionHeader,
			"x-grok-client-identifier", "x-authenticateresponse", "x-grok-conv-id",
		} {
			gotProhibitedHeaders[name] = r.Header.Get(name)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode xAI Images request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// Reproduce gateways that already decompressed JSON but forgot to remove
		// the upstream gzip marker. The client must not try to decompress it again.
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"iVBORw0KGgo="}]}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServerWithSettings(t, map[string]string{
		config.XAIBaseURLSettingKey: upstream.URL + "/v1",
	})
	srv.client = upstream.Client()
	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-image-token", RefreshToken: "refresh",
		TokenType: "Bearer", Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		TokenEndpoint: xaiauth.TokenURL, BaseURL: xaiauth.CLIBaseURL,
	})
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-images-admin-test", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: credential,
		URLs: model.ChannelURLs{{
			URL: "https://cli-chat-proxy.grok.com/v1", Protocols: []string{util.ProtocolCodex},
		}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "grok-imagine-image-quality"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "XAI/Grok-Imagine-Image-Quality",
		"prompt":         "A white cat",
		"size":           "3:2@1k",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/images/generations" {
		t.Fatalf("upstream request=%s %s", gotMethod, gotPath)
	}
	if gotAuthorization != "Bearer xai-image-token" {
		t.Fatalf("Authorization=%q", gotAuthorization)
	}
	if gotContentType != "application/json" || gotAccept != "application/json" {
		t.Fatalf("content negotiation: Content-Type=%q Accept=%q", gotContentType, gotAccept)
	}
	if gotAcceptEncoding != "identity" {
		t.Fatalf("Accept-Encoding=%q, want identity", gotAcceptEncoding)
	}
	for name, value := range gotProhibitedHeaders {
		if value != "" {
			t.Fatalf("unexpected xAI Images header %s=%q", name, value)
		}
	}
	if gotBody["model"] != "grok-imagine-image-quality" || gotBody["prompt"] != "A white cat" {
		t.Fatalf("xAI Images body=%v", gotBody)
	}
	if gotBody["response_format"] != "b64_json" || gotBody["aspect_ratio"] != "3:2" || gotBody["resolution"] != "1k" {
		t.Fatalf("xAI Images native options=%v", gotBody)
	}
	for _, unsupported := range []string{"size", "quality", "background", "output_format"} {
		if _, exists := gotBody[unsupported]; exists {
			t.Fatalf("xAI Images body contains unsupported %q: %v", unsupported, gotBody)
		}
	}

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("xAI image generation failed: %v", response.Data)
	}
	if response.Data["actual_model"] != "grok-imagine-image-quality" || response.Data["tested_key_index"] != float64(cooldown.NoKeyIndex) {
		t.Fatalf("response routing metadata=%v", response.Data)
	}
	if response.Data["output_format"] != "png" {
		t.Fatalf("xAI response must report the detected output format: %v", response.Data)
	}
	images, _ := response.Data["images"].([]any)
	image, _ := images[0].(map[string]any)
	if image["mime_type"] != "image/png" {
		t.Fatalf("xAI response must report the detected MIME type: %v", response.Data)
	}
}

func TestHandleChannelImageGeneration_XAIOAuthRefreshesRejectedTokenOnce(t *testing.T) {
	var orderMu sync.Mutex
	order := make([]string, 0, 3)
	record := func(stage string) {
		orderMu.Lock()
		order = append(order, stage)
		orderMu.Unlock()
	}
	var upstreamRequests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer rejected-image-token":
			record("test-current")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
		case "Bearer refreshed-image-token":
			record("test-refreshed")
			_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`)
		default:
			t.Errorf("unexpected Authorization=%q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	srv := newInMemoryServerWithSettings(t, map[string]string{
		config.XAIBaseURLSettingKey: upstream.URL + "/v1",
	})
	srv.client = upstream.Client()
	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "rejected-image-token", RefreshToken: "refresh-image-token",
		TokenType: "Bearer", Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		TokenEndpoint: xaiauth.TokenURL, BaseURL: xaiauth.CLIBaseURL,
	})
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-images-refresh-admin-test", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: credential,
		URLs:                  model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{util.ProtocolCodex}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ModelEntries:          []model.ModelEntry{{Model: "grok-imagine-image"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	var refreshRequests atomic.Int32
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshRequests.Add(1)
		record("refresh")
		if req.URL.String() != xaiauth.TokenURL {
			t.Errorf("refresh URL=%s", req.URL)
		}
		if err := req.ParseForm(); err != nil || req.Form.Get("refresh_token") != "refresh-image-token" {
			t.Errorf("refresh form=%v err=%v", req.Form, err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"access_token":"refreshed-image-token","refresh_token":"refreshed-image-refresh","expires_in":3600}`,
			)),
			Request: req,
		}, nil
	})}
	srv.xaiCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "grok-imagine-image", "prompt": "A white cat",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); w.Code != http.StatusOK || !success {
		t.Fatalf("image generation after refresh failed: status=%d response=%v", w.Code, response.Data)
	}
	if upstreamRequests.Load() != 2 || refreshRequests.Load() != 1 {
		t.Fatalf("upstream=%d refresh=%d, want 2/1", upstreamRequests.Load(), refreshRequests.Load())
	}
	if !reflect.DeepEqual(order, []string{"test-current", "refresh", "test-refreshed"}) {
		t.Fatalf("request order=%v", order)
	}
	persisted, err := srv.store.GetConfig(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := xaiauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || refreshed.AccessToken != "refreshed-image-token" || refreshed.RefreshToken != "refreshed-image-refresh" {
		t.Fatalf("persisted refreshed credential=%v err=%v", refreshed, err)
	}
}

func TestHandleChannelImageGeneration_ClientCancellationDoesNotCooldown(t *testing.T) {
	requestStarted := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	var fallbackCalls atomic.Int32
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`)
	}))
	defer fallback.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	srv.urlSelector = nil
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "cancelled-openai-images-admin-test",
		URLs: model.ChannelURLs{
			{URL: upstream.URL, Protocols: []string{util.ProtocolOpenAI}},
			{URL: fallback.URL, Protocols: []string{util.ProtocolOpenAI}},
		},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-image-1"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-image-test"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-image-spare"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	requestContext, cancel := context.WithCancel(context.Background())
	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "gpt-image-1",
		"prompt":         "A white cat",
	}).WithContext(requestContext)
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	done := make(chan struct{})
	go func() {
		srv.HandleChannelImageGeneration(c)
		close(done)
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("image request did not reach upstream")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled image request did not finish")
	}

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if response.Data["status_code"] != float64(util.StatusClientClosedRequest) {
		t.Fatalf("status_code=%v, want %d", response.Data["status_code"], util.StatusClientClosedRequest)
	}
	if response.Data["cooldown_action"] != "client_error_no_cooldown" {
		t.Fatalf("cooldown_action=%v", response.Data["cooldown_action"])
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("cancelled request fell back to another URL %d times", fallbackCalls.Load())
	}
}

func TestHandleChannelImageGeneration_ClassifiesHTTP200StructuredError(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"type":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2099-12-09 18:08:11 重置。"}}`)
	}))
	defer upstream.Close()
	var fallbackCalls atomic.Int32
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`)
	}))
	defer fallback.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	srv.urlSelector = nil
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "soft-error-openai-images-admin-test",
		URLs: model.ChannelURLs{
			{URL: upstream.URL, Protocols: []string{util.ProtocolOpenAI}},
			{URL: fallback.URL, Protocols: []string{util.ProtocolOpenAI}},
		},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeUpstream,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-image-1"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-image-test"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-image-spare"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	req := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/images/generations", created.ID), map[string]any{
		"generation_api": imageGenerationAPIImages,
		"model":          "gpt-image-1",
		"prompt":         "A white cat",
	})
	c, w := newTestContext(t, req)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}
	srv.HandleChannelImageGeneration(c)

	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := response.Data["success"].(bool); success {
		t.Fatalf("structured error unexpectedly succeeded: %v", response.Data)
	}
	if _, ok := response.Data["api_error"].(map[string]any); !ok {
		t.Fatalf("structured error missing api_error: %v", response.Data)
	}
	if response.Data["cooldown_action"] != "key_cooldown_applied" {
		t.Fatalf("cooldown_action=%v, want key_cooldown_applied", response.Data["cooldown_action"])
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("key-scoped structured error fell back to another URL %d times", fallbackCalls.Load())
	}
}

// TestDownstreamEndpointPath 锁住渠道基础 URL 带子路径时的端点还原：协议族谓词
// 判定的是下游端点，拿上游完整路径去判定会让 ZCode 这类渠道的指纹路径整体失效。
func TestDownstreamEndpointPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fullURL string
		baseURL string
		want    string
	}{
		{
			name:    "zcode base url carries a sub path",
			fullURL: "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages?beta=true",
			baseURL: "https://zcode.z.ai/api/v1/ultra-zai/anthropic",
			want:    "/v1/messages",
		},
		{
			name:    "plain base url",
			fullURL: "https://api.anthropic.com/v1/messages?beta=true",
			baseURL: "https://api.anthropic.com",
			want:    "/v1/messages",
		},
		{
			name:    "base url with trailing slash",
			fullURL: "https://gw.example.test/claude/v1/messages",
			baseURL: "https://gw.example.test/claude/",
			want:    "/v1/messages",
		},
		{
			name:    "xai base url already ends with /v1",
			fullURL: "https://cli-chat-proxy.grok.com/v1/v1/responses",
			baseURL: "https://cli-chat-proxy.grok.com/v1",
			want:    "/v1/responses",
		},
		{
			name:    "exact url is the endpoint itself",
			fullURL: "https://chatgpt.com/backend-api/codex/responses",
			baseURL: "https://chatgpt.com/backend-api/codex/responses#",
			want:    "/backend-api/codex/responses",
		},
		{
			name:    "base url path is not a prefix",
			fullURL: "https://api.example.test/v1/messages",
			baseURL: "https://other.example.test/mismatch",
			want:    "/v1/messages",
		},
		{
			name:    "base path /v1 does not steal /v1beta",
			fullURL: "https://host.example.test/v1beta/models/x:generateContent",
			baseURL: "https://host.example.test/v1",
			want:    "/v1beta/models/x:generateContent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := downstreamEndpointPath(test.fullURL, test.baseURL); got != test.want {
				t.Fatalf("downstreamEndpointPath(%q, %q) = %q, want %q",
					test.fullURL, test.baseURL, got, test.want)
			}
		})
	}
}

// 管理测试与代理链路必须发出同一套上游契约，思考后缀在两边都要落进请求体。
func TestBuildTestUpstreamRequestPlanAppliesThinkingSuffix(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{
		ID: 11, Name: "codex-test", AuthType: model.AuthTypeAPIKey,
		URLs:         model.ChannelURLs{{URL: "https://upstream.example.com", Protocols: []string{util.ProtocolCodex}}},
		ModelEntries: []model.ModelEntry{{Model: "gpt-5.6-luna"}},
	}
	testReq := &testutil.TestChannelRequest{
		Model: "gpt-5.6-luna", Content: "hello", ClientProtocol: util.ProtocolCodex,
	}

	_, plan, err := srv.buildTestUpstreamRequestPlan(
		cfg, "sk-test", testReq, "gpt-5.6-luna(max)",
		util.ProtocolCodex, util.ProtocolCodex, "https://upstream.example.com",
	)
	if err != nil {
		t.Fatalf("buildTestUpstreamRequestPlan: %v", err)
	}
	if effort := gjson.GetBytes(plan.requestBody, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort=%q, want xhigh. body=%s", effort, plan.requestBody)
	}
}

// 跨协议转到 Codex 时，patch 不能用模板默认 medium 盖掉后缀。
func TestBuildTestUpstreamRequestPlanKeepsThinkingSuffixAcrossCodexTransform(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{
		ID: 12, Name: "codex-transform-test", AuthType: model.AuthTypeAPIKey,
		URLs:         model.ChannelURLs{{URL: "https://upstream.example.com", Protocols: []string{util.ProtocolCodex}}},
		ModelEntries: []model.ModelEntry{{Model: "claude-opus-4-6"}},
	}
	testReq := &testutil.TestChannelRequest{
		Model: "claude-opus-4-6", Content: "hello", ClientProtocol: util.ProtocolAnthropic,
	}

	_, plan, err := srv.buildTestUpstreamRequestPlan(
		cfg, "sk-test", testReq, "claude-opus-4-6(high)",
		util.ProtocolAnthropic, util.ProtocolCodex, "https://upstream.example.com",
	)
	if err != nil {
		t.Fatalf("buildTestUpstreamRequestPlan: %v", err)
	}
	if effort := gjson.GetBytes(plan.requestBody, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort=%q, want high. body=%s", effort, plan.requestBody)
	}
}

func TestChannelTestLogIdentityStripsThinkingSuffix(t *testing.T) {
	t.Parallel()

	logModel, effort := channelTestLogIdentity("gpt-5.6-luna(max)", "low")
	if logModel != "gpt-5.6-luna" {
		t.Fatalf("log model=%q, want gpt-5.6-luna", logModel)
	}
	if effort != "max" {
		t.Fatalf("thinking effort=%q, want max from suffix not fallback", effort)
	}
}

func TestAdminTestZAICodingPlanEmitsZCodeWireContract(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := newZAITestChannel()
	cfg.ModelEntries = []model.ModelEntry{{Model: "glm-4.7"}}
	testReq := &testutil.TestChannelRequest{
		Model: "glm-4.7", Content: "hello", ClientProtocol: util.ProtocolAnthropic,
	}

	cfgForBuild, plan, err := srv.buildTestUpstreamRequestPlan(
		cfg, "key-id.secret", testReq, testReq.Model, util.ProtocolAnthropic, util.ProtocolAnthropic, zaiauth.CodingPlanProxyBaseURL,
	)
	if err != nil {
		t.Fatalf("buildTestUpstreamRequestPlan: %v", err)
	}
	if plan.endpointPath != "/v1/messages" {
		t.Fatalf("endpointPath = %q, want /v1/messages after stripping the ZCode base path", plan.endpointPath)
	}
	identity := decodeZAIRequestIdentity(t, plan.requestBody)
	if identity.DeviceID != cfg.ZAIDeviceID {
		t.Fatalf("metadata.user_id device = %q, want ZCode fingerprint %q", identity.DeviceID, cfg.ZAIDeviceID)
	}

	req, cancel, err := srv.newTestUpstreamRequest(context.Background(), cfgForBuild, testReq, plan)
	if err != nil {
		t.Fatalf("newTestUpstreamRequest: %v", err)
	}
	defer cancel()

	if got := headerValueFold(req.Header, "x-api-key"); got != "key-id.secret" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := headerValueFold(req.Header, "User-Agent"); got != "ZCode/"+zaiauth.AppVersion {
		t.Fatalf("User-Agent = %q, want ZCode identity", got)
	}
	if got := headerValueFold(req.Header, "x-session-id"); got == "" {
		t.Fatal("x-session-id missing")
	}
	if got := headerValueFold(req.Header, "Authorization"); got != "" {
		t.Fatalf("Authorization = %q, ZCode authenticates with x-api-key only", got)
	}
}

func TestAdminTestNativeAnthropicDoesNotDoubleAppendHeaderRules(t *testing.T) {
	srv := newInMemoryServer(t)
	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "access", RefreshToken: "refresh",
		Expired: "2030-01-01T00:00:00Z", AccountUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "11111111-2222-4333-8444-555555555555"
	userID := fmt.Sprintf(`{"device_id":%q,"account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":%q}`,
		credential.DeviceID, sessionID)
	const helperBetas = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05"
	cfg := &model.Config{
		AuthType: model.AuthTypeAnthropicOAuth, OAuthCredential: credentialJSON,
		URLs: model.ChannelURLs{{URL: "https://api.anthropic.com"}},
		CustomRequestRules: &model.CustomRequestRules{Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionAppend, Name: "Anthropic-Beta", Value: "context-1m-2025-08-07"},
		}},
	}
	plan := &channelTestRequestPlan{
		upstreamProtocol: util.ProtocolAnthropic,
		apiKey:           "oauth-access",
		fullURL:          "https://api.anthropic.com/v1/messages",
		endpointPath:     "/v1/messages",
		requestBody: []byte(fmt.Sprintf(
			`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"helper probe"}],"metadata":{"user_id":%q}}`,
			userID,
		)),
		headers: http.Header{
			"Accept": {"application/json"}, "Accept-Encoding": {"gzip"}, "Content-Type": {"application/json"},
			"User-Agent": {"claude-cli/2.1.220 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {helperBetas},
			"Anthropic-Version": {"2023-06-01"}, "Anthropic-Dangerous-Direct-Browser-Access": {"true"},
			"X-Claude-Code-Session-Id": {sessionID}, "X-Client-Request-Id": {"66666666-7777-4888-8999-aaaaaaaaaaaa"},
			"X-Stainless-Lang": {"js"}, "X-Stainless-Runtime": {"node"}, "X-Stainless-Package-Version": {"0.94.0"},
			"X-Stainless-Runtime-Version": {"v26.3.0"}, "X-Stainless-OS": {"MacOS"}, "X-Stainless-Arch": {"arm64"},
			"X-Stainless-Retry-Count": {"0"}, "X-Stainless-Timeout": {"600"},
		},
	}

	req, cancel, err := srv.newTestUpstreamRequest(context.Background(), cfg, &testutil.TestChannelRequest{Model: "claude-haiku-4-5-20251001"}, plan)
	if err != nil {
		t.Fatalf("newTestUpstreamRequest: %v", err)
	}
	defer cancel()

	var betaValues []string
	for name, values := range req.Header {
		if strings.EqualFold(name, "anthropic-beta") {
			betaValues = append(betaValues, values...)
		}
	}
	betas := strings.Join(betaValues, ", ")
	if !strings.Contains(betas, helperBetas) {
		t.Fatalf("anthropic-beta = %q, want the native helper profile preserved", betas)
	}
	if strings.Contains(betas, "claude-code-20250219") {
		t.Fatalf("anthropic-beta = %q, helper request was rebuilt as a cloaked CLI fingerprint", betas)
	}
	if strings.Count(betas, "context-1m-2025-08-07") != 1 {
		t.Fatalf("anthropic-beta = %q, append rule must not run twice on the native admin-test path", betas)
	}
}

func TestAdminTestCursorOAuthUsesSDKBridgeInsteadOfHTTP(t *testing.T) {
	t.Parallel()
	upstreamHits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Not Found","message":"Route POST:`+r.URL.Path+` not found","statusCode":404}`)
	}))
	srv := newInMemoryServer(t)
	cfg := createCursorOAuthChannelForAdminTest(t, srv, upstream.URL)
	runner := &fakeCursorRunner{text: "ok from sdk bridge"}
	srv.cursorRunner = runner

	result := srv.executeChannelTestWithCooldown(context.Background(), cfg, cooldown.NoKeyIndex, "tok", &testutil.TestChannelRequest{
		Model: "grok-4.6", ClientProtocol: util.ProtocolAnthropic, Content: "2025 年 1 月 20 日发生了什么大事？",
	}, true)
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cursor admin test result=%+v", result)
	}
	if upstreamHits != 0 {
		t.Fatalf("Cursor OAuth admin test must not HTTP-forward, hits=%d", upstreamHits)
	}
	if got, _ := result["upstream_protocol"].(string); got != "cursor-sdk-bridge" {
		t.Fatalf("upstream_protocol=%q", got)
	}
	if got, _ := result["response_text"].(string); got != "ok from sdk bridge" {
		t.Fatalf("response_text=%q result=%+v", got, result)
	}
	if runner.model != "grok-4.6" {
		t.Fatalf("SDK model=%q, want exact catalog ID", runner.model)
	}
	if !strings.Contains(runner.prompt, "2025 年 1 月 20 日发生了什么大事？") {
		t.Fatalf("prompt=%q", runner.prompt)
	}
	if body, _ := result["upstream_request_body"].(string); !strings.Contains(body, `"messages"`) || strings.Contains(body, "chat/completions") {
		t.Fatalf("client body must stay Anthropic messages, got %s", body)
	}
}

func TestHandleChannelTestCursorWritesOneManualLogWithDebug(t *testing.T) {
	srv := newInMemoryServerWithSettings(t, map[string]string{"debug_log_enabled": "true"})
	cfg := createCursorOAuthChannelForAdminTest(t, srv, "https://unused.example.com")
	srv.cursorRunner = &fakeCursorRunner{text: "ok from sdk bridge"}
	request := newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", cfg.ID), map[string]any{
		"model":           "grok-4.6",
		"client_protocol": "anthropic",
		"content":         "hello",
	})
	c, recorder := newTestContext(t, request)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}
	srv.HandleChannelTest(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := mustParseAPIResponse[map[string]any](t, recorder.Body.Bytes())
	if success, _ := response.Data["success"].(bool); !success {
		t.Fatalf("response=%v", response.Data)
	}
	// 等待异步代理日志的完整刷新周期，确保没有迟到的重复记录。
	time.Sleep(config.LogBatchTimeout + 250*time.Millisecond)
	logs, err := srv.store.ListLogs(context.Background(), time.Time{}, 10, 0, &model.LogFilter{LogSource: model.LogSourceAll})
	if err != nil {
		t.Fatalf("ListLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].LogSource != model.LogSourceManualTest {
		t.Fatalf("logs=%+v, want one manual-test log", logs)
	}
	debug, err := srv.store.GetDebugLogByLogID(context.Background(), logs[0].ID)
	if err != nil || debug == nil {
		t.Fatalf("debug=%+v err=%v", debug, err)
	}
	if !strings.Contains(debug.ReqURL, "SdkAgentService/CreateAgent+Send") ||
		!strings.Contains(string(debug.ReqBody), `"id":"grok-4.6"`) ||
		strings.Contains(string(debug.ReqBody), "cursor-user-key") ||
		!strings.Contains(string(debug.RespBody), "ok from sdk bridge") ||
		!strings.Contains(string(debug.TranslatedRespBody), "ok from sdk bridge") {
		t.Fatalf("debug=%+v", debug)
	}
}

func TestAdminTestCursorOAuthReportsMissingBridge(t *testing.T) {
	t.Parallel()
	upstreamHits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusTeapot)
	}))
	srv := newInMemoryServer(t)
	cfg := createCursorOAuthChannelForAdminTest(t, srv, upstream.URL)
	srv.cursorRunner = &fakeCursorRunner{err: cursorauth.ErrAgentMissing}

	result := srv.executeChannelTestWithCooldown(context.Background(), cfg, cooldown.NoKeyIndex, "tok", &testutil.TestChannelRequest{
		Model: "grok-4.6", ClientProtocol: util.ProtocolAnthropic, Content: "hello",
	}, true)
	if success, _ := result["success"].(bool); success {
		t.Fatalf("missing bridge must fail, result=%+v", result)
	}
	if upstreamHits != 0 {
		t.Fatalf("missing bridge must not HTTP-forward, hits=%d", upstreamHits)
	}
	if status, _ := result["status_code"].(int); status != http.StatusServiceUnavailable {
		t.Fatalf("status_code=%v result=%+v", result["status_code"], result)
	}
	if action, _ := result["cooldown_action"].(string); action != "client_error_no_cooldown" {
		t.Fatalf("cooldown_action=%q result=%+v", action, result)
	}
	if errMsg, _ := result["error"].(string); !strings.Contains(errMsg, "cursor-sdk-bridge is not installed") {
		t.Fatalf("error=%q", errMsg)
	}
}

func TestAdminTestCursorRequiresUserAPIKey(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := createCursorOAuthChannelForAdminTest(t, srv, "https://example.invalid")
	payload, err := (&cursorauth.Credential{AccessToken: "tok", Email: "user@example.com"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OAuthCredential = payload
	result := srv.executeChannelTestWithCooldown(context.Background(), cfg, cooldown.NoKeyIndex, "tok", &testutil.TestChannelRequest{
		Model: "grok-4.6", ClientProtocol: util.ProtocolAnthropic, Content: "hello",
	}, true)
	if status, _ := result["status_code"].(int); status != http.StatusUnauthorized {
		t.Fatalf("status_code=%v result=%+v", result["status_code"], result)
	}
	if errMsg, _ := result["error"].(string); !strings.Contains(errMsg, "User API Key") {
		t.Fatalf("error=%q data=%+v", errMsg, result)
	}
}
