package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

type failingEnsureAuthTokenStore struct {
	storage.Store
}

func (f failingEnsureAuthTokenStore) EnsureAuthToken(context.Context, *model.AuthToken) (bool, error) {
	return false, errors.New("database down")
}

func TestParseProvisionedAuthTokens(t *testing.T) {
	tokens, err := parseProvisionedAuthTokens(" seed-one | production , seed-token-two ")
	if err != nil {
		t.Fatalf("parseProvisionedAuthTokens failed: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("len(tokens)=%d, want 2", len(tokens))
	}
	if tokens[0].PlainToken != "seed-one" || tokens[0].Description != "production" {
		t.Fatalf("first token=%+v, want seed-one|production", tokens[0])
	}
	if tokens[1].PlainToken != "seed-token-two" || tokens[1].Description != "Provisioned token seed****-two" {
		t.Fatalf("second token=%+v, want default masked description", tokens[1])
	}
}

func TestParseProvisionedAuthTokens_RejectsInvalidEntries(t *testing.T) {
	tests := []string{
		",",
		"seed-one,,seed-two",
		"|production",
		"seed-one|",
		"seed-one|prod|extra",
		"seed-one,seed-one",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseProvisionedAuthTokens(raw); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestProvisionedAuthTokensEnvValue(t *testing.T) {
	t.Run("uses alias when primary is empty", func(t *testing.T) {
		t.Setenv(EnvProvisionedAuthTokens, "")
		t.Setenv(EnvProvisionedAuthTokensAlias, "alias-token")

		raw, err := provisionedAuthTokensEnvValue()
		if err != nil {
			t.Fatalf("provisionedAuthTokensEnvValue failed: %v", err)
		}
		if raw != "alias-token" {
			t.Fatalf("raw=%q, want alias-token", raw)
		}
	})

	t.Run("rejects conflicting primary and alias", func(t *testing.T) {
		t.Setenv(EnvProvisionedAuthTokens, "primary-token")
		t.Setenv(EnvProvisionedAuthTokensAlias, "alias-token")

		if _, err := provisionedAuthTokensEnvValue(); err == nil {
			t.Fatal("expected conflict error")
		}
	})
}

func TestProvisionAuthTokens_CreatesMissingTokensIdempotently(t *testing.T) {
	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	result, err := ProvisionAuthTokens(ctx, store, "seed-one|production,seed-two")
	if err != nil {
		t.Fatalf("ProvisionAuthTokens failed: %v", err)
	}
	if result.Configured != 2 || result.Created != 2 {
		t.Fatalf("result=%+v, want configured=2 created=2", result)
	}

	seedOne, err := store.GetAuthTokenByValue(ctx, model.HashToken("seed-one"))
	if err != nil {
		t.Fatalf("GetAuthTokenByValue(seed-one) failed: %v", err)
	}
	if seedOne.Token == "seed-one" {
		t.Fatal("stored token must be hash, got plaintext")
	}
	if seedOne.Description != "production" || !seedOne.IsActive {
		t.Fatalf("seedOne=%+v, want active production token", seedOne)
	}

	result, err = ProvisionAuthTokens(ctx, store, "seed-one|changed,seed-two")
	if err != nil {
		t.Fatalf("ProvisionAuthTokens second run failed: %v", err)
	}
	if result.Configured != 2 || result.Created != 0 {
		t.Fatalf("second result=%+v, want configured=2 created=0", result)
	}

	all, err := store.ListAuthTokens(ctx)
	if err != nil {
		t.Fatalf("ListAuthTokens failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all)=%d, want 2", len(all))
	}
	seedOneAgain, err := store.GetAuthTokenByValue(ctx, model.HashToken("seed-one"))
	if err != nil {
		t.Fatalf("GetAuthTokenByValue(seed-one) second run failed: %v", err)
	}
	if seedOneAgain.Description != "production" {
		t.Fatalf("existing description was changed to %q", seedOneAgain.Description)
	}
}

func TestProvisionAuthTokens_ErrorDoesNotLeakToken(t *testing.T) {
	_, err := ProvisionAuthTokens(context.Background(), failingEnsureAuthTokenStore{}, "secret-token-value|production")
	if err == nil {
		t.Fatal("expected provisioning error")
	}

	msg := err.Error()
	for _, leaked := range []string{"secret-token-value", "secret", "value", "secr****alue"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("error leaked token fragment %q: %s", leaked, msg)
		}
	}
	if !strings.Contains(msg, "position 1") {
		t.Fatalf("error=%q, want position without token data", msg)
	}
}

func TestNewServer_ProvisionedAuthTokensFromEnvLoadedImmediately(t *testing.T) {
	t.Setenv(EnvProvisionedAuthTokens, "boot-token|boot")
	t.Setenv(EnvProvisionedAuthTokensAlias, "")

	srv := newInMemoryServer(t)
	tokenHash := model.HashToken("boot-token")

	srv.authService.authTokensMux.RLock()
	_, exists := srv.authService.authTokens[tokenHash]
	tokenID := srv.authService.authTokenIDs[tokenHash]
	srv.authService.authTokensMux.RUnlock()

	if !exists {
		t.Fatal("provisioned token was not loaded into auth cache")
	}
	if tokenID == 0 {
		t.Fatal("provisioned token id was not loaded into auth cache")
	}
}
