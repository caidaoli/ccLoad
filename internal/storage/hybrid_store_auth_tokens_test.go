package storage

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestHybridStore_EnsureAuthToken_SyncsExistingIDToSQLite(t *testing.T) {
	mysql := createTestSQLiteStore(t)
	sqlite := createTestSQLiteStore(t)
	defer func() {
		_ = sqlite.Close()
		_ = mysql.Close()
	}()

	hybrid := NewHybridStore(sqlite, mysql)
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

	mysqlToken, err := mysql.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("mysql GetAuthTokenByValue failed: %v", err)
	}
	sqliteToken, err := sqlite.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("sqlite GetAuthTokenByValue failed: %v", err)
	}
	if mysqlToken.ID != sqliteToken.ID || mysqlToken.ID != first.ID {
		t.Fatalf("id mismatch mysql=%d sqlite=%d first=%d", mysqlToken.ID, sqliteToken.ID, first.ID)
	}

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
