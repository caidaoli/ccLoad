package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

func TestUpsertAuthTokenAllFields_SQLite(t *testing.T) {
	store := newTestStore(t, "auth_tokens_upsert.db")
	ctx := context.Background()

	ss := store.(*sqlstore.SQLStore)

	exp := time.Now().Add(1 * time.Hour).UnixMilli()
	last := time.Now().Add(-1 * time.Minute).UnixMilli()

	token := &model.AuthToken{
		ID:          123,
		Token:       model.HashToken("plain"),
		Description: "d",
		CreatedAt:   time.Now(),
		ExpiresAt:   &exp,
		LastUsedAt:  &last,
		IsActive:    true,
		AllowedModels: []string{
			"gpt-4o",
			"claude-3-5-sonnet-latest",
		},
		CostUsedMicroUSD:  10,
		CostLimitMicroUSD: 100,
		SuccessCount:      1,
		FailureCount:      2,
	}

	if err := ss.UpsertAuthTokenAllFields(ctx, token); err != nil {
		t.Fatalf("UpsertAuthTokenAllFields failed: %v", err)
	}

	got, err := store.GetAuthToken(ctx, 123)
	if err != nil {
		t.Fatalf("GetAuthToken failed: %v", err)
	}
	if got.Token != token.Token || got.Description != "d" || !got.IsActive {
		t.Fatalf("unexpected token: %+v", got)
	}
	if got.CostLimitMicroUSD != 100 || got.CostUsedMicroUSD != 10 {
		t.Fatalf("unexpected cost fields: %+v", got)
	}
	if len(got.AllowedModels) != 2 {
		t.Fatalf("unexpected allowed_models: %+v", got.AllowedModels)
	}
}
