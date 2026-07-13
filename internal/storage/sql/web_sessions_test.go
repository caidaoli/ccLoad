package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestWebSessionPersistsIdentityAndExcludesExpiredSessions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "web_sessions.db")
	ctx := context.Background()
	now := time.Now()

	adminToken := "admin-web-session"
	if err := store.CreateWebSession(ctx, adminToken, model.WebSession{
		Role:      model.WebRoleAdmin,
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin web session: %v", err)
	}

	apiTokenSession := "api-token-web-session"
	if err := store.CreateWebSession(ctx, apiTokenSession, model.WebSession{
		Role:        model.WebRoleAPIToken,
		AuthTokenID: 42,
		ExpiresAt:   now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("create api token web session: %v", err)
	}

	if err := store.CreateWebSession(ctx, "expired-web-session", model.WebSession{
		Role:      model.WebRoleAdmin,
		ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create expired web session: %v", err)
	}

	got, exists, err := store.GetWebSession(ctx, apiTokenSession)
	if err != nil {
		t.Fatalf("get api token web session: %v", err)
	}
	if !exists {
		t.Fatal("expected api token web session to exist")
	}
	if got.Role != model.WebRoleAPIToken || got.AuthTokenID != 42 {
		t.Fatalf("identity = (%q, %d), want (%q, 42)", got.Role, got.AuthTokenID, model.WebRoleAPIToken)
	}

	loaded, err := store.LoadWebSessions(ctx)
	if err != nil {
		t.Fatalf("load web sessions: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded session count = %d, want 2", len(loaded))
	}
	if _, ok := loaded[model.HashToken(apiTokenSession)]; !ok {
		t.Fatal("loaded sessions missing hashed api token web session")
	}
}

func TestWebSessionDeleteAndClean(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "web_sessions_clean.db")
	ctx := context.Background()

	if err := store.CreateWebSession(ctx, "delete-me", model.WebSession{
		Role:      model.WebRoleAdmin,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session to delete: %v", err)
	}
	if err := store.DeleteWebSession(ctx, "delete-me"); err != nil {
		t.Fatalf("delete web session: %v", err)
	}
	if _, exists, err := store.GetWebSession(ctx, "delete-me"); err != nil || exists {
		t.Fatalf("deleted session exists=%v err=%v, want false,nil", exists, err)
	}

	for _, session := range []struct {
		token       string
		authTokenID int64
	}{
		{token: "revoke-owner-a", authTokenID: 42},
		{token: "revoke-owner-b", authTokenID: 42},
		{token: "keep-other-owner", authTokenID: 99},
	} {
		if err := store.CreateWebSession(ctx, session.token, model.WebSession{
			Role:        model.WebRoleAPIToken,
			AuthTokenID: session.authTokenID,
			ExpiresAt:   time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create token web session %q: %v", session.token, err)
		}
	}
	if err := store.DeleteWebSessionsByAuthTokenID(ctx, 42); err != nil {
		t.Fatalf("delete token web sessions: %v", err)
	}
	for _, token := range []string{"revoke-owner-a", "revoke-owner-b"} {
		if _, exists, err := store.GetWebSession(ctx, token); err != nil || exists {
			t.Fatalf("revoked session %q exists=%v err=%v, want false,nil", token, exists, err)
		}
	}
	if _, exists, err := store.GetWebSession(ctx, "keep-other-owner"); err != nil || !exists {
		t.Fatalf("unrelated session exists=%v err=%v, want true,nil", exists, err)
	}

	if err := store.CreateWebSession(ctx, "expired", model.WebSession{
		Role:      model.WebRoleAdmin,
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if err := store.CleanExpiredWebSessions(ctx); err != nil {
		t.Fatalf("clean expired web sessions: %v", err)
	}
	if _, exists, err := store.GetWebSession(ctx, "expired"); err != nil || exists {
		t.Fatalf("expired session exists=%v err=%v, want false,nil", exists, err)
	}
}
