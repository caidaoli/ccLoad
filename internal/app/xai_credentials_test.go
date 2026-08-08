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
