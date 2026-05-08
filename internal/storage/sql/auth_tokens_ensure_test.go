package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestEnsureAuthToken_CreatesAndSkipsExistingByToken(t *testing.T) {
	store := newTestStore(t, "auth_tokens_ensure.db")
	ctx := context.Background()

	tokenHash := model.HashToken("seed-token")
	first := &model.AuthToken{
		Token:       tokenHash,
		Description: "first description",
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	created, err := store.EnsureAuthToken(ctx, first)
	if err != nil {
		t.Fatalf("EnsureAuthToken first run failed: %v", err)
	}
	if !created || first.ID == 0 {
		t.Fatalf("first run created=%v id=%d, want created with id", created, first.ID)
	}

	second := &model.AuthToken{
		Token:       tokenHash,
		Description: "second description",
		IsActive:    false,
		CreatedAt:   time.Now().Add(time.Hour),
	}
	created, err = store.EnsureAuthToken(ctx, second)
	if err != nil {
		t.Fatalf("EnsureAuthToken second run failed: %v", err)
	}
	if created {
		t.Fatal("second run created duplicate token")
	}
	if second.ID != first.ID {
		t.Fatalf("second id=%d, want existing id %d", second.ID, first.ID)
	}
	if second.Description != "first description" || !second.IsActive {
		t.Fatalf("second token was not backfilled from existing row: %+v", second)
	}

	all, err := store.ListAuthTokens(ctx)
	if err != nil {
		t.Fatalf("ListAuthTokens failed: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(all)=%d, want 1", len(all))
	}
	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetAuthTokenByValue failed: %v", err)
	}
	if got.Description != "first description" || !got.IsActive {
		t.Fatalf("existing token was modified: %+v", got)
	}
}
