package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestAuthToken_CreateAndGet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "auth_tokens.db")

	ctx := context.Background()

	// 创建 Auth Token
	token := &model.AuthToken{
		Token:             "test-token-hash",
		Description:       "Test Token",
		IsActive:          true,
		CostLimitMicroUSD: 1000000, // $1
		AllowedModels:     []string{"gpt-4", "claude-3"},
		CreatedAt:         time.Now(),
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 通过 ID 获取
	got, err := store.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("get auth token by id: %v", err)
	}
	if got.Description != "Test Token" {
		t.Errorf("description: got %q, want %q", got.Description, "Test Token")
	}
	if !got.IsActive {
		t.Error("expected is_active=true")
	}

	// 通过 Token 值获取
	gotByValue, err := store.GetAuthTokenByValue(ctx, "test-token-hash")
	if err != nil {
		t.Fatalf("get auth token by value: %v", err)
	}
	if gotByValue.ID != got.ID {
		t.Errorf("id mismatch: by value=%d, by id=%d", gotByValue.ID, got.ID)
	}

	// 获取不存在的 token
	_, err = store.GetAuthToken(ctx, 99999)
	if err == nil {
		t.Error("expected error for non-existent token")
	}
}

func TestAuthToken_List(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "list.db")

	ctx := context.Background()

	// 创建多个 Auth Tokens
	for i := 0; i < 3; i++ {
		token := &model.AuthToken{
			Token:       "token-" + string(rune('A'+i)),
			Description: "Token " + string(rune('A'+i)),
			IsActive:    i%2 == 0, // A, C 是 active
			CreatedAt:   time.Now(),
		}
		if err := store.CreateAuthToken(ctx, token); err != nil {
			t.Fatalf("create token %d: %v", i, err)
		}
	}

	// 列出所有 tokens
	allTokens, err := store.ListAuthTokens(ctx)
	if err != nil {
		t.Fatalf("list auth tokens: %v", err)
	}
	if len(allTokens) != 3 {
		t.Errorf("expected 3 tokens, got %d", len(allTokens))
	}

	// 列出活跃的 tokens
	activeTokens, err := store.ListActiveAuthTokens(ctx)
	if err != nil {
		t.Fatalf("list active auth tokens: %v", err)
	}
	if len(activeTokens) != 2 {
		t.Errorf("expected 2 active tokens, got %d", len(activeTokens))
	}
}

func TestAuthToken_Update(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "update.db")

	ctx := context.Background()

	// 创建 token
	expiresAt := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	lastUsedAt := time.Now().UnixMilli()
	token := &model.AuthToken{
		Token:       "update-test-token",
		Description: "Original Description",
		IsActive:    true,
		ExpiresAt:   &expiresAt,
		LastUsedAt:  &lastUsedAt,
		CreatedAt:   time.Now(),
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 更新 token
	token.Description = "Updated Description"
	token.IsActive = false
	token.CostLimitMicroUSD = 5000000 // $5

	if err := store.UpdateAuthToken(ctx, token); err != nil {
		t.Fatalf("update auth token: %v", err)
	}

	// 验证更新
	got, err := store.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.Description != "Updated Description" {
		t.Errorf("description: got %q, want %q", got.Description, "Updated Description")
	}
	if got.IsActive {
		t.Error("expected is_active=false")
	}
	if got.CostLimitMicroUSD != 5000000 {
		t.Errorf("cost limit: got %d, want %d", got.CostLimitMicroUSD, 5000000)
	}
}

func TestAuthToken_Delete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "delete.db")

	ctx := context.Background()

	// 创建 token
	token := &model.AuthToken{
		Token:       "delete-test-token",
		Description: "To Delete",
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 删除 token
	if err := store.DeleteAuthToken(ctx, token.ID); err != nil {
		t.Fatalf("delete auth token: %v", err)
	}

	// 验证已删除
	_, err := store.GetAuthToken(ctx, token.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestAuthToken_UpdateLastUsed(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "last_used.db")

	ctx := context.Background()

	// 创建 token
	token := &model.AuthToken{
		Token:       "last-used-test",
		Description: "Last Used Test",
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	// 初始时 last_used_at 在 DB 是 0，但 scan 会把 0 映射为 nil（omitempty 语义）
	got, err := store.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("get auth token: %v", err)
	}
	if got.LastUsedAt != nil {
		t.Fatalf("expected last_used_at to be nil initially, got=%v", got.LastUsedAt)
	}

	// 更新 last_used_at
	if err := store.UpdateTokenLastUsed(ctx, "last-used-test", time.Now()); err != nil {
		t.Fatalf("update token last used: %v", err)
	}

	// 验证更新
	got, err = store.GetAuthToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("get auth token after update: %v", err)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt <= 0 {
		t.Fatalf("expected last_used_at to be set, got=%v", got.LastUsedAt)
	}
}
