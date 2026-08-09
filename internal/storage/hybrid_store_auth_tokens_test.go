package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestHybridStore_EnsureAuthTokenKeepsSQLiteIDAndConvergesPrimary(t *testing.T) {
	primary := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	hybrid := NewHybridStore(sqlite, primary)
	defer func() { _ = hybrid.Close() }()

	ctx := context.Background()
	tokenHash := model.HashToken("hybrid-seed")
	first := &model.AuthToken{
		Token:       tokenHash,
		Description: "hybrid first",
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	created, err := hybrid.EnsureAuthToken(ctx, first)
	if err != nil {
		t.Fatalf("EnsureAuthToken first run failed: %v", err)
	}
	if !created || first.ID == 0 {
		t.Fatalf("first run created=%v id=%d, want created with id", created, first.ID)
	}

	sqliteToken, err := sqlite.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("sqlite GetAuthTokenByValue failed: %v", err)
	}
	waitForCondition(t, 3*time.Second, func() bool {
		primaryToken, getErr := primary.GetAuthTokenByValue(ctx, tokenHash)
		return getErr == nil && primaryToken.ID == sqliteToken.ID && primaryToken.ID == first.ID
	})

	second := &model.AuthToken{
		Token:       tokenHash,
		Description: "hybrid changed",
		IsActive:    false,
		CreatedAt:   time.Now().Add(time.Hour),
	}
	created, err = hybrid.EnsureAuthToken(ctx, second)
	if err != nil {
		t.Fatalf("EnsureAuthToken second run failed: %v", err)
	}
	if created {
		t.Fatal("second run created duplicate token")
	}
	if second.ID != first.ID {
		t.Fatalf("second id=%d, want existing id %d", second.ID, first.ID)
	}

	sqliteToken, err = sqlite.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("sqlite GetAuthTokenByValue second run failed: %v", err)
	}
	if sqliteToken.Description != "hybrid first" || !sqliteToken.IsActive {
		t.Fatalf("sqlite token was modified on duplicate ensure: %+v", sqliteToken)
	}
}
