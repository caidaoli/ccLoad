package sql_test

import (
	"context"
	"testing"
	"time"
)

func TestAdminSession_CreateAndGet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "sessions.db")

	ctx := context.Background()
	token := "test-session-token-12345"
	expiresAt := time.Now().Add(24 * time.Hour)

	// 创建会话
	if err := store.CreateAdminSession(ctx, token, expiresAt); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	// 获取会话
	gotExpires, exists, err := store.GetAdminSession(ctx, token)
	if err != nil {
		t.Fatalf("get admin session: %v", err)
	}
	if !exists {
		t.Error("expected session to exist")
	}
	// 验证过期时间（允许1秒误差）
	if gotExpires.Sub(expiresAt).Abs() > time.Second {
		t.Errorf("expires at: got %v, want ~%v", gotExpires, expiresAt)
	}

	// 获取不存在的会话
	_, exists, err = store.GetAdminSession(ctx, "non-existent-token")
	if err != nil {
		t.Fatalf("get non-existent session: %v", err)
	}
	if exists {
		t.Error("expected session to not exist")
	}
}

func TestAdminSession_Delete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "delete.db")

	ctx := context.Background()
	token := "token-to-delete"
	expiresAt := time.Now().Add(1 * time.Hour)

	// 创建会话
	if err := store.CreateAdminSession(ctx, token, expiresAt); err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	// 验证存在
	_, exists, err := store.GetAdminSession(ctx, token)
	if err != nil {
		t.Fatalf("get admin session: %v", err)
	}
	if !exists {
		t.Error("expected session to exist before delete")
	}

	// 删除会话
	if err := store.DeleteAdminSession(ctx, token); err != nil {
		t.Fatalf("delete admin session: %v", err)
	}

	// 验证已删除
	_, exists, err = store.GetAdminSession(ctx, token)
	if err != nil {
		t.Fatalf("get admin session after delete: %v", err)
	}
	if exists {
		t.Error("expected session to be deleted")
	}
}

func TestAdminSession_CleanExpired(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "clean.db")

	ctx := context.Background()

	// 创建一个过期的会话
	expiredToken := "expired-token"
	if err := store.CreateAdminSession(ctx, expiredToken, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	// 创建一个有效的会话
	validToken := "valid-token"
	if err := store.CreateAdminSession(ctx, validToken, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("create valid session: %v", err)
	}

	// 清理过期会话
	if err := store.CleanExpiredSessions(ctx); err != nil {
		t.Fatalf("clean expired sessions: %v", err)
	}

	// 验证过期会话被删除
	_, exists, err := store.GetAdminSession(ctx, expiredToken)
	if err != nil {
		t.Fatalf("get expired session: %v", err)
	}
	if exists {
		t.Error("expected expired session to be cleaned")
	}

	// 验证有效会话仍存在
	_, exists, err = store.GetAdminSession(ctx, validToken)
	if err != nil {
		t.Fatalf("get valid session: %v", err)
	}
	if !exists {
		t.Error("expected valid session to still exist")
	}
}

func TestAdminSession_LoadAll(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "load_all.db")

	ctx := context.Background()

	// 创建多个会话
	for i := 0; i < 3; i++ {
		token := "session-token-" + string(rune('A'+i))
		expiresAt := time.Now().Add(time.Duration(i+1) * time.Hour)
		if err := store.CreateAdminSession(ctx, token, expiresAt); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	// 创建一个已过期的会话（不应被加载）
	if err := store.CreateAdminSession(ctx, "expired", time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	// 加载所有未过期会话
	sessions, err := store.LoadAllSessions(ctx)
	if err != nil {
		t.Fatalf("load all sessions: %v", err)
	}

	// 应该只有3个未过期的会话
	if len(sessions) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(sessions))
	}
}
