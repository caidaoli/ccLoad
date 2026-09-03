package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"ccLoad/internal/zedauth"
)

func TestZedOAuthManagerCompletesNativeLogin(t *testing.T) {
	testZedOAuthManagerCompletesNativeLogin(t, "9d4b8c17-12ae-4091-96bc-1a79ce2de601")
}

func TestZedOAuthManagerCompletesNativeLoginWithoutSystemID(t *testing.T) {
	// Isolate system identity discovery from the developer's local Zed database.
	t.Setenv("HOME", t.TempDir())
	testZedOAuthManagerCompletesNativeLogin(t, "")
}

func testZedOAuthManagerCompletesNativeLogin(t *testing.T, systemID string) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	expiresAt := time.Now().Add(time.Hour).Unix()
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiresAt))) + ".sig"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/client/llm_tokens":
			if request.Method != http.MethodPost || request.Header.Get("x-zed-system-id") != systemID {
				t.Errorf("mint request = method %s headers %v", request.Method, request.Header)
			}
			_, _ = fmt.Fprintf(writer, `{"token":%q}`, jwt)
		case "/models":
			if request.Header.Get("Authorization") != "Bearer "+jwt {
				t.Errorf("models authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"models":[{"id":"gpt-5.6-sol","provider":"open_ai"},{"id":"claude-sonnet-5","provider":"anthropic"},{"id":"gemini-3.5-flash","provider":"google"}]}`))
		case "/client/users/me":
			if request.Header.Get("x-zed-system-id") != systemID {
				t.Errorf("current user headers = %v", request.Header)
			}
			_, _ = writer.Write([]byte(`{"user":{"username":"octocat-profile","github_login":"octocat"},"plan":{"plan_v3":"zed_student","usage":{"model_requests":{"used":0,"limit":{"limited":0}}}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	service := zedauth.NewService(upstream.Client())
	service.LLMTokensURL = upstream.URL + "/client/llm_tokens"
	service.ModelsURL = upstream.URL + "/models"
	service.CurrentUserURL = upstream.URL + "/client/users/me"
	manager := newZedOAuthManager(service, store, nil)
	defer manager.close()

	authorizationURL, state, err := manager.startWithHint(systemID)
	if err != nil {
		t.Fatal(err)
	}
	parsedAuthorizationURL, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := base64.RawURLEncoding.DecodeString(parsedAuthorizationURL.Query().Get("native_app_public_key"))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.ParsePKCS1PublicKey(publicDER)
	if err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"github_user_id":42,"github_user_login":"octocat","access_token":"native-secret"}`)
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, native, nil)
	if err != nil {
		t.Fatal(err)
	}
	callbackQuery := url.Values{}
	callbackQuery.Set("user_id", "user-42")
	callbackQuery.Set("access_token", base64.RawURLEncoding.EncodeToString(ciphertext))
	callbackURL := "http://127.0.0.1:" + parsedAuthorizationURL.Query().Get("native_app_port") + "/?" + callbackQuery.Encode()
	response, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", response.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		status, ok := manager.status(state)
		if !ok {
			t.Fatal("Zed OAuth session disappeared")
		}
		if status.Status == "complete" {
			if status.ChannelID == 0 {
				t.Fatal("completed Zed OAuth session has no channel")
			}
			persisted, getErr := store.GetConfig(context.Background(), status.ChannelID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			credential, parseErr := zedauth.ParseCredential([]byte(persisted.OAuthCredential))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if credential.UserID != "user-42" || credential.SystemID != systemID || credential.AccessToken != jwt ||
				credential.Username != "octocat-profile" || persisted.Name != "Zed-octocat-profile" ||
				!persisted.SupportsModel("gpt-5.6-sol") || !persisted.SupportsModel("claude-sonnet-5") ||
				!persisted.SupportsModel("gemini-3.5-flash") || len(persisted.ModelEntries) != 3 {
				t.Fatalf("persisted Zed channel = cfg=%+v credential=%+v", persisted, credential)
			}
			wantModels := []string{"claude-sonnet-5", "gemini-3.5-flash", "gpt-5.6-sol"}
			if got := persisted.GetModels(); !reflect.DeepEqual(got, wantModels) {
				t.Fatalf("persisted models = %v, want %v", got, wantModels)
			}
			break
		}
		if status.Status == "error" {
			t.Fatalf("Zed OAuth failed: %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Zed OAuth did not complete: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCreateOrUpdateZedChannelPreservesInstallationAndUsage(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	first, err := zedauth.NewCredential("user-1", "system-original", []byte(`{"github_user_login":"octocat","access_token":"native-old"}`))
	if err != nil {
		t.Fatal(err)
	}
	first.AccessToken = "old.jwt.token"
	first.OAuthUsage = []byte(`{"sample":"keep"}`)
	created, wasCreated, err := createOrUpdateZedChannel(ctx, store, first, []string{"gpt-old"})
	if err != nil || !wasCreated {
		t.Fatalf("create channel: created=%v err=%v", wasCreated, err)
	}
	replacement, err := zedauth.NewCredential("user-1", "system-foreign", []byte(`{"github_user_login":"octocat","access_token":"native-new"}`))
	if err != nil {
		t.Fatal(err)
	}
	replacement.AccessToken = "new.jwt.token"
	updated, wasCreated, err := createOrUpdateZedChannel(ctx, store, replacement, []string{"gpt-new"})
	if err != nil || wasCreated || updated.ID != created.ID {
		t.Fatalf("update channel: channel=%+v created=%v err=%v", updated, wasCreated, err)
	}
	persisted, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := zedauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if credential.SystemID != "system-original" || credential.AccessToken != "new.jwt.token" || string(credential.OAuthUsage) != `{"sample":"keep"}` {
		t.Fatalf("reauthorized credential = %+v", credential)
	}
	if !persisted.SupportsModel("gpt-new") || persisted.SupportsModel("gpt-old") {
		t.Fatalf("model catalog was not replaced: %+v", persisted.ModelEntries)
	}
}

func TestZedRejectedOldTokenDoesNotRefreshNewWinner(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	expiresAt := time.Now().Add(time.Hour).Unix()
	oldToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiresAt))) + ".old"
	newToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiresAt))) + ".new"
	mintCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/client/llm_tokens" {
			http.NotFound(writer, request)
			return
		}
		mintCount++
		_, _ = fmt.Fprintf(writer, `{"token":%q}`, newToken)
	}))
	defer upstream.Close()
	service := zedauth.NewService(upstream.Client())
	service.LLMTokensURL = upstream.URL + "/client/llm_tokens"
	credential, err := zedauth.NewCredential(
		"user-1", "9d4b8c17-12ae-4091-96bc-1a79ce2de601",
		[]byte(`{"github_user_login":"octocat","access_token":"native"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	credential.AccessToken = oldToken
	credential.ExpiresAt = expiresAt
	cfg, _, err := createOrUpdateZedChannel(ctx, store, credential, []string{"gpt-5.6-sol"})
	if err != nil {
		t.Fatal(err)
	}
	manager := newZedCredentialManager(service, store, nil, nil)
	winner, err := manager.credentialAfterUnauthorized(ctx, cfg, oldToken)
	if err != nil {
		t.Fatal(err)
	}
	lateWinner, err := manager.credentialAfterUnauthorized(ctx, cfg, oldToken)
	if err != nil {
		t.Fatal(err)
	}
	if mintCount != 1 || winner.AccessToken != newToken || lateWinner.AccessToken != newToken {
		t.Fatalf("mint_count=%d winner=%q late_winner=%q", mintCount, winner.AccessToken, lateWinner.AccessToken)
	}
}
