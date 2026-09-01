package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/zaiauth"
)

type zaiOAuthTestUpstream struct {
	mu         sync.Mutex
	polls      int
	readyAfter int
	failed     bool
}

func (u *zaiOAuthTestUpstream) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/cli/init"):
			if r.Header.Get("Authorization") == "" {
				t.Errorf("init request is missing the poll token")
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"flow-1","poll_token":"poll","authorize_url":"https://zcode.z.ai/oauth/authorize","expires_at":0,"poll_interval_sec":0}}`))
		case strings.Contains(r.URL.Path, "/oauth/cli/poll/"):
			u.mu.Lock()
			u.polls++
			ready := u.polls > u.readyAfter
			failed := u.failed
			u.mu.Unlock()
			switch {
			case failed:
				_, _ = w.Write([]byte(`{"code":0,"data":{"status":"failed"}}`))
			case ready:
				_, _ = w.Write([]byte(`{"code":0,"data":{"status":"ready","token":"jwt","user":{"user_id":"u-1","email":"user@example.com","name":"User"},"zai":{"access_token":"zai-access"}}}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
}

func newZAIOAuthTestManager(
	t *testing.T,
	upstream *zaiOAuthTestUpstream,
	resolve func(context.Context, string) (*zaiauth.Credential, error),
	commit func(context.Context, *zaiauth.Credential) (int64, string, error),
) *zaiOAuthManager {
	t.Helper()
	server := httptest.NewServer(upstream.handler(t))
	t.Cleanup(server.Close)
	service := zaiauth.NewService(server.Client())
	service.OAuthBaseURL = server.URL + "/api/v1"
	manager := newZAIOAuthManager(context.Background(), service, resolve, commit)
	manager.sleep = func(context.Context, time.Duration) {}
	t.Cleanup(manager.close)
	return manager
}

func waitForZAIOAuthStatus(t *testing.T, manager *zaiOAuthManager, session, state string) zaiOAuthStatusResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := manager.status(session, state)
		if ok && status.Status != "pending" && status.Status != "committing" {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("z.ai OAuth session never settled")
	return zaiOAuthStatusResponse{}
}

func TestZAIOAuthManagerCommitsAuthorizedAccount(t *testing.T) {
	t.Parallel()
	upstream := &zaiOAuthTestUpstream{readyAfter: 2}
	var resolvedToken string
	var committed *zaiauth.Credential
	manager := newZAIOAuthTestManager(t, upstream,
		func(_ context.Context, accessToken string) (*zaiauth.Credential, error) {
			resolvedToken = accessToken
			return &zaiauth.Credential{APIKey: "key.secret"}, nil
		},
		func(_ context.Context, credential *zaiauth.Credential) (int64, string, error) {
			committed = credential
			return 42, "Z.ai-user@example.com", nil
		},
	)

	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if started.URL != "https://zcode.z.ai/oauth/authorize" || started.State != "flow-1" || started.Status != "pending" {
		t.Fatalf("start = %+v", started)
	}
	status := waitForZAIOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "complete" || status.ChannelID != 42 || status.ChannelName != "Z.ai-user@example.com" {
		t.Fatalf("status = %+v", status)
	}
	if resolvedToken != "zai-access" {
		t.Fatalf("resolved token = %q", resolvedToken)
	}
	if committed == nil || committed.Email != "user@example.com" || committed.UserID != "u-1" ||
		committed.JWTToken != "jwt" || committed.AccessToken != "zai-access" {
		t.Fatalf("committed = %+v", committed)
	}
	// 会话落终态即撤销轮询上下文——这才是"凭证不再可用"的实际保证；
	// 轮询凭证只活在 run 的栈上，会话里本就查不到。
	manager.mu.Lock()
	settled := manager.sessions[started.State].ctx.Err()
	manager.mu.Unlock()
	if settled == nil {
		t.Fatal("a settled session must cancel its poll context")
	}
}

func TestZAIOAuthManagerReportsUpstreamRejection(t *testing.T) {
	t.Parallel()
	upstream := &zaiOAuthTestUpstream{failed: true}
	manager := newZAIOAuthTestManager(t, upstream,
		func(context.Context, string) (*zaiauth.Credential, error) {
			t.Error("a rejected authorization must not resolve a key")
			return nil, nil
		},
		func(context.Context, *zaiauth.Credential) (int64, string, error) {
			t.Error("a rejected authorization must not create a channel")
			return 0, "", nil
		},
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	status := waitForZAIOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "error" || status.Error == "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestZAIOAuthManagerSurfacesCommitFailure(t *testing.T) {
	t.Parallel()
	manager := newZAIOAuthTestManager(t, &zaiOAuthTestUpstream{},
		func(context.Context, string) (*zaiauth.Credential, error) {
			return &zaiauth.Credential{APIKey: "key.secret"}, nil
		},
		func(context.Context, *zaiauth.Credential) (int64, string, error) {
			return 0, "", errors.New("channel write failed")
		},
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	status := waitForZAIOAuthStatus(t, manager, "admin-1", started.State)
	if status.Status != "error" || !strings.Contains(status.Error, "channel write failed") {
		t.Fatalf("status = %+v", status)
	}
}

// One administrator owns one live login; sessions are never readable by another.
func TestZAIOAuthManagerScopesSessionsToAdministrator(t *testing.T) {
	t.Parallel()
	upstream := &zaiOAuthTestUpstream{readyAfter: 1 << 30}
	manager := newZAIOAuthTestManager(t, upstream,
		func(context.Context, string) (*zaiauth.Credential, error) { return nil, errors.New("unused") },
		func(context.Context, *zaiauth.Credential) (int64, string, error) { return 0, "", nil },
	)
	started, err := manager.start(context.Background(), "admin-1")
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if _, ok := manager.status("admin-2", started.State); ok {
		t.Fatal("another administrator must not observe the session")
	}
	if err := manager.cancel("admin-2", started.State); err == nil {
		t.Fatal("another administrator must not cancel the session")
	}
	if err := manager.cancel("admin-1", started.State); err != nil {
		t.Fatalf("cancel() error = %v", err)
	}
	status, ok := manager.status("admin-1", started.State)
	if !ok || status.Status != "cancelled" {
		t.Fatalf("status = %+v ok = %v", status, ok)
	}
	if err := manager.cancel("admin-1", started.State); err == nil {
		t.Fatal("a settled session cannot be cancelled twice")
	}
}

func TestZAIOAuthManagerRejectsAnonymousStart(t *testing.T) {
	t.Parallel()
	manager := newZAIOAuthTestManager(t, &zaiOAuthTestUpstream{},
		func(context.Context, string) (*zaiauth.Credential, error) { return nil, nil },
		func(context.Context, *zaiauth.Credential) (int64, string, error) { return 0, "", nil },
	)
	if _, err := manager.start(context.Background(), "  "); err == nil {
		t.Fatal("start() must require an administrator session")
	}
}

// The hosted flow is enabled per ZCode release; an empty 404 must be reported
// as an upstream availability fact so the UI can offer the key path instead.
func TestZAIOAuthManagerReportsUnavailableFlow(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	service := zaiauth.NewService(server.Client())
	service.OAuthBaseURL = server.URL + "/api/v1"
	manager := newZAIOAuthManager(context.Background(), service,
		func(context.Context, string) (*zaiauth.Credential, error) { return nil, nil },
		func(context.Context, *zaiauth.Credential) (int64, string, error) { return 0, "", nil },
	)
	t.Cleanup(manager.close)
	_, err := manager.start(context.Background(), "admin-1")
	if !errors.Is(err, zaiauth.ErrOAuthFlowUnavailable) {
		t.Fatalf("start() error = %v", err)
	}
}
