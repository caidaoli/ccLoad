package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
)

type cursorOAuthTestUpstream struct {
	mu         sync.Mutex
	polls      int
	readyAfter int
	failed     bool
}

func (u *cursorOAuthTestUpstream) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == cursorauth.AuthPollPath:
			u.mu.Lock()
			u.polls++
			ready := u.polls > u.readyAfter
			failed := u.failed
			u.mu.Unlock()
			switch {
			case failed:
				w.WriteHeader(http.StatusForbidden)
			case ready:
				_, _ = io.WriteString(w, `{"accessToken":"tok","refreshToken":"ref"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case r.URL.Path == cursorauth.GetMeRPC:
			_, _ = io.WriteString(w, `{"authId":"auth-1","email":"user@example.com","firstName":"Ada","lastName":"Lovelace"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newCursorOAuthTestManager(
	t *testing.T,
	upstream *cursorOAuthTestUpstream,
	commit func(context.Context, *cursorauth.Credential) (int64, string, error),
) *cursorOAuthManager {
	t.Helper()
	server := httptest.NewServer(upstream.handler(t))
	t.Cleanup(server.Close)
	service := cursorauth.NewService(server.Client())
	service.APIBaseURL = server.URL
	service.WebsiteURL = "https://cursor.com"
	manager := newCursorOAuthManager(context.Background(), service, commit)
	manager.sleep = func(context.Context, time.Duration) {}
	t.Cleanup(manager.close)
	return manager
}

func waitForCursorOAuthStatus(t *testing.T, manager *cursorOAuthManager, session, state string) cursorOAuthStatusResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := manager.status(session, state)
		if ok && status.Status != "pending" && status.Status != "committing" {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cursor OAuth session never settled")
	return cursorOAuthStatusResponse{}
}

func TestCursorOAuthManagerCommitsAuthorizedAccount(t *testing.T) {
	t.Parallel()
	upstream := &cursorOAuthTestUpstream{readyAfter: 2}
	var committed *cursorauth.Credential
	manager := newCursorOAuthTestManager(t, upstream,
		func(_ context.Context, credential *cursorauth.Credential) (int64, string, error) {
			committed = credential
			return 7, "Cursor-user@example.com", nil
		},
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if !strings.Contains(started.URL, "loginDeepControl") || started.State == "" {
		t.Fatalf("start = %+v", started)
	}
	status := waitForCursorOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "complete" || status.ChannelID != 7 {
		t.Fatalf("status = %+v", status)
	}
	if committed == nil || committed.AccessToken != "tok" || committed.Email != "user@example.com" {
		t.Fatalf("committed = %+v", committed)
	}
	manager.mu.Lock()
	remaining := manager.sessions[started.State].verifier
	manager.mu.Unlock()
	if remaining != "" {
		t.Fatal("verifier must be cleared once the session settles")
	}
}

func TestCursorOAuthManagerReportsUpstreamRejection(t *testing.T) {
	t.Parallel()
	upstream := &cursorOAuthTestUpstream{failed: true}
	manager := newCursorOAuthTestManager(t, upstream,
		func(context.Context, *cursorauth.Credential) (int64, string, error) {
			t.Error("a rejected authorization must not create a channel")
			return 0, "", nil
		},
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	status := waitForCursorOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "error" || status.Error == "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestCursorOAuthManagerScopesSessionsToAdministrator(t *testing.T) {
	t.Parallel()
	upstream := &cursorOAuthTestUpstream{readyAfter: 1 << 30}
	manager := newCursorOAuthTestManager(t, upstream,
		func(context.Context, *cursorauth.Credential) (int64, string, error) { return 0, "", nil },
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if _, ok := manager.status("admin-2", started.State); ok {
		t.Fatal("another administrator must not observe the session")
	}
	if err := manager.cancel("admin-1", started.State); err != nil {
		t.Fatalf("cancel() error = %v", err)
	}
}

func TestCursorOAuthManagerRejectsAnonymousStart(t *testing.T) {
	t.Parallel()
	manager := newCursorOAuthTestManager(t, &cursorOAuthTestUpstream{},
		func(context.Context, *cursorauth.Credential) (int64, string, error) { return 0, "", nil },
	)
	if _, err := manager.start(context.Background(), "  "); err == nil {
		t.Fatal("start() must require an administrator session")
	}
}

func TestCursorOAuthManagerSurfacesCommitFailure(t *testing.T) {
	t.Parallel()
	manager := newCursorOAuthTestManager(t, &cursorOAuthTestUpstream{},
		func(context.Context, *cursorauth.Credential) (int64, string, error) {
			return 0, "", errors.New("channel write failed")
		},
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	status := waitForCursorOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "error" || !strings.Contains(status.Error, "channel write failed") {
		t.Fatalf("status = %+v", status)
	}
}
