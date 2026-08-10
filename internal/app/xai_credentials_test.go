package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"
)

func TestXAICredentialManagerRefreshesAndPersistsCompleteCredential(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	old := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Email: "old@example.com", Subject: "subject",
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: old,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		if req.URL.String() != xaiauth.TokenURL {
			t.Fatalf("refresh URL = %s", req.URL)
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "refresh_token=old-refresh") {
			t.Fatalf("refresh body = %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600,"token_type":"Bearer"}`,
			)),
			Request: req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)

	credential, err := manager.credential(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if credential.AccessToken != "new-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshed credential = %s", credential)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", refreshes.Load())
	}
	persisted, err := store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := xaiauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AccessToken != "new-access" || parsed.RefreshToken != "rotated-refresh" || parsed.Email != "old@example.com" || parsed.Subject != "subject" {
		t.Fatalf("persisted credential = %s", parsed)
	}
}

func TestXAICredentialManagerKeepsFreshCredentialWithoutNetwork(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "fresh-access", RefreshToken: "refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-fresh", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client {
		t.Fatal("fresh credential attempted network refresh")
		return nil
	}, nil)

	credential, err := manager.credential(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if credential.AccessToken != "fresh-access" {
		t.Fatalf("access token = %q", credential.AccessToken)
	}
}

func TestXAICredentialManagerForceRefreshConsumesConcurrentWinner(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldRaw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-concurrent", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: oldRaw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	winnerRaw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "winner-access", RefreshToken: "winner-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	updated, err := store.CompareAndSwapOAuthCredential(
		context.Background(), cfg.ID, model.AuthTypeXAIOAuth, oldRaw, winnerRaw,
	)
	if err != nil || !updated {
		t.Fatalf("persist concurrent winner: updated=%v err=%v", updated, err)
	}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client {
		t.Fatal("concurrent CAS winner must be reused without another refresh")
		return nil
	}, nil)

	credential, err := manager.credential(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if credential.AccessToken != "winner-access" || credential.RefreshToken != "winner-refresh" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestXAICredentialManagerForceRefreshesRejectedCachedWinnerWithStaleConfig(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldRaw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "old-access", RefreshToken: "old-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-stale-config", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: oldRaw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	winner := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "rejected-winner", RefreshToken: "winner-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	winnerRaw := mustXAICredentialJSON(t, winner)
	updated, err := store.CompareAndSwapOAuthCredential(
		context.Background(), cfg.ID, model.AuthTypeXAIOAuth, oldRaw, winnerRaw,
	)
	if err != nil || !updated {
		t.Fatalf("persist winner: updated=%v err=%v", updated, err)
	}
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "refresh_token=winner-refresh") {
			t.Fatalf("refresh body = %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"final-access","refresh_token":"final-refresh","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	manager.cache(cfg.ID, winner)

	credential, err := manager.credential(context.Background(), cfg, true)
	if err != nil {
		t.Fatalf("credential() error = %v", err)
	}
	if credential.AccessToken != "final-access" || refreshes.Load() != 1 {
		t.Fatalf("credential=%#v refreshes=%d", credential, refreshes.Load())
	}
}

func TestXAICredentialManagerSeparatesConcurrentRejectedAccessTokens(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accessA := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-a", RefreshToken: "refresh-a",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	cfgA, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-concurrent-rejected", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: accessA,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	aStarted := make(chan struct{})
	bStarted := make(chan struct{})
	releaseA := make(chan struct{})
	var aStartedOnce sync.Once
	var bStartedOnce sync.Once
	var releaseAOnce sync.Once
	t.Cleanup(func() { releaseAOnce.Do(func() { close(releaseA) }) })
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		var body string
		switch req.Form.Get("refresh_token") {
		case "refresh-a":
			aStartedOnce.Do(func() { close(aStarted) })
			<-releaseA
			body = `{"access_token":"access-a2","refresh_token":"refresh-a2","expires_in":3600}`
		case "refresh-b":
			bStartedOnce.Do(func() { close(bStarted) })
			body = `{"access_token":"access-c","refresh_token":"refresh-c","expires_in":3600}`
		default:
			return nil, fmt.Errorf("unexpected refresh token %q", req.Form.Get("refresh_token"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	type credentialResult struct {
		credential *xaiauth.Credential
		err        error
	}
	aResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credentialAfterUnauthorized(context.Background(), cfgA, "access-a")
		aResult <- credentialResult{credential: credential, err: callErr}
	}()
	<-aStarted

	accessB := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-b", RefreshToken: "refresh-b",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	updated, err := store.CompareAndSwapOAuthCredential(
		context.Background(), cfgA.ID, model.AuthTypeXAIOAuth, accessA, accessB,
	)
	if err != nil || !updated {
		t.Fatalf("persist access B: updated=%v err=%v", updated, err)
	}
	cfgB, err := store.GetConfig(context.Background(), cfgA.ID)
	if err != nil {
		t.Fatal(err)
	}
	bResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credentialAfterUnauthorized(context.Background(), cfgB, "access-b")
		bResult <- credentialResult{credential: credential, err: callErr}
	}()

	bRefreshStarted := false
	select {
	case <-bStarted:
		bRefreshStarted = true
	case <-time.After(2 * time.Second):
	}
	if bRefreshStarted {
		result := <-bResult
		if result.err != nil || result.credential == nil || result.credential.AccessToken != "access-c" {
			t.Fatalf("refresh B credential=%#v err=%v", result.credential, result.err)
		}
	}
	releaseAOnce.Do(func() { close(releaseA) })
	resultA := <-aResult
	if resultA.err != nil || resultA.credential == nil || resultA.credential.AccessToken != "access-c" {
		t.Fatalf("refresh A winner=%#v err=%v", resultA.credential, resultA.err)
	}
	if !bRefreshStarted {
		resultB := <-bResult
		t.Fatalf("refresh B was coalesced with rejected A: credential=%#v err=%v", resultB.credential, resultB.err)
	}
	if refreshes.Load() != 2 {
		t.Fatalf("refreshes=%d, want 2", refreshes.Load())
	}
}

func TestXAICredentialManagerDoesNotRefreshWinnerUnderStaleFlightKey(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accessB := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-b", RefreshToken: "refresh-b",
		Expired: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	cfgB, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-stale-flight-key", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: accessB,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var refreshStartedOnce sync.Once
	var releaseRefreshOnce sync.Once
	t.Cleanup(func() { releaseRefreshOnce.Do(func() { close(releaseRefresh) }) })
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		if got := req.Form.Get("refresh_token"); got != "refresh-b" {
			return nil, fmt.Errorf("refresh token=%q, want refresh-b", got)
		}
		refreshStartedOnce.Do(func() { close(refreshStarted) })
		<-releaseRefresh
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-c","refresh_token":"refresh-c","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	manager.cache(cfgB.ID, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-a", RefreshToken: "refresh-a",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	type credentialResult struct {
		credential *xaiauth.Credential
		err        error
	}
	aResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credentialAfterUnauthorized(context.Background(), cfgB, "access-a")
		aResult <- credentialResult{credential: credential, err: callErr}
	}()

	select {
	case result := <-aResult:
		if result.err != nil || result.credential == nil || result.credential.AccessToken != "access-b" {
			t.Fatalf("stale A flight winner=%#v err=%v", result.credential, result.err)
		}
	case <-refreshStarted:
		releaseRefreshOnce.Do(func() { close(releaseRefresh) })
		result := <-aResult
		t.Fatalf("stale A flight refreshed B: credential=%#v err=%v", result.credential, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("stale A flight did not return the persisted B winner")
	}

	bResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credentialAfterUnauthorized(context.Background(), cfgB, "access-b")
		bResult <- credentialResult{credential: credential, err: callErr}
	}()
	select {
	case <-refreshStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("B flight did not start its refresh")
	}
	releaseRefreshOnce.Do(func() { close(releaseRefresh) })
	resultB := <-bResult
	if resultB.err != nil || resultB.credential == nil || resultB.credential.AccessToken != "access-c" {
		t.Fatalf("B flight credential=%#v err=%v", resultB.credential, resultB.err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d, want 1", refreshes.Load())
	}
}

func TestXAICredentialManagerCoalescesNeededAndUnauthorizedRefresh(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-a", RefreshToken: "refresh-a",
		Expired: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-needed-and-unauthorized", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var firstStartedOnce sync.Once
	var secondStartedOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch refreshes.Add(1) {
		case 1:
			firstStartedOnce.Do(func() { close(firstStarted) })
		default:
			secondStartedOnce.Do(func() { close(secondStarted) })
		}
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-b","refresh_token":"refresh-b","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	type credentialResult struct {
		credential *xaiauth.Credential
		err        error
	}
	neededResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credential(context.Background(), cfg, false)
		neededResult <- credentialResult{credential: credential, err: callErr}
	}()
	<-firstStarted
	unauthorizedResult := make(chan credentialResult, 1)
	go func() {
		credential, callErr := manager.credentialAfterUnauthorized(context.Background(), cfg, "access-a")
		unauthorizedResult <- credentialResult{credential: credential, err: callErr}
	}()

	duplicateRefresh := false
	select {
	case <-secondStarted:
		duplicateRefresh = true
	case result := <-unauthorizedResult:
		t.Fatalf("unauthorized waiter returned before shared refresh completed: credential=%#v err=%v", result.credential, result.err)
	case <-time.After(200 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	needed := <-neededResult
	unauthorized := <-unauthorizedResult
	if duplicateRefresh {
		t.Fatalf("needed and unauthorized paths started %d token refreshes", refreshes.Load())
	}
	for name, result := range map[string]credentialResult{"needed": needed, "unauthorized": unauthorized} {
		if result.err != nil || result.credential == nil || result.credential.AccessToken != "access-b" {
			t.Fatalf("%s credential=%#v err=%v", name, result.credential, result.err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d, want 1", refreshes.Load())
	}
}

func TestXAICredentialManagerMergesRefreshIntoConcurrentUsageUpdate(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	initial := &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "access-a", RefreshToken: "refresh-a",
		Expired: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	raw := mustXAICredentialJSON(t, initial)
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-refresh-usage-cas", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	usage := *initial
	usage.OAuthUsage = []byte(`{"sampled_at":"2030-01-01T00:00:00Z"}`)
	usageRaw := mustXAICredentialJSON(t, &usage)
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		updated, updateErr := store.CompareAndSwapOAuthCredential(
			context.Background(), cfg.ID, model.AuthTypeXAIOAuth, raw, usageRaw,
		)
		if updateErr != nil || !updated {
			return nil, fmt.Errorf("persist concurrent usage: updated=%v err=%v", updated, updateErr)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-b","refresh_token":"refresh-b","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	credential, err := manager.credentialAfterUnauthorized(context.Background(), cfg, "access-a")
	if err != nil || credential == nil || credential.AccessToken != "access-b" ||
		string(credential.OAuthUsage) != `{"sampled_at":"2030-01-01T00:00:00Z"}` {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	persisted, err := store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := xaiauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil || persistedCredential.AccessToken != "access-b" ||
		string(persistedCredential.OAuthUsage) != `{"sampled_at":"2030-01-01T00:00:00Z"}` {
		t.Fatalf("persisted credential=%#v err=%v", persistedCredential, err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d, want 1", refreshes.Load())
	}
}

func TestXAICredentialManagerCoalescesConcurrentRefresh(t *testing.T) {
	t.Parallel()

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "expired-access", RefreshToken: "shared-refresh",
		Expired: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "xai-singleflight", AuthType: model.AuthTypeXAIOAuth, OAuthCredential: raw,
		URLs:    model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "grok-4.5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"shared-access","refresh_token":"shared-refresh-new","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}
	manager := newXAICredentialManager(store, func(*model.Config) *http.Client { return client }, nil)
	const callers = 12
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			credential, err := manager.credential(context.Background(), cfg, false)
			if err == nil && credential.AccessToken != "shared-access" {
				err = fmt.Errorf("access token = %q", credential.AccessToken)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes=%d, want 1", got)
	}
}

func mustXAICredentialJSON(t *testing.T, credential *xaiauth.Credential) string {
	t.Helper()
	raw, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
