package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestChannelManagementServiceSaveSettingsMergesAndRedacts(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	ctx := context.Background()
	cfg := createChannelManagementTestConfig(t, server.store, "managed")
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client { return server.client })

	view, err := service.SaveSettings(ctx, cfg, &channelManagementInput{
		Profile:     model.ChannelManagementProfileNewAPI,
		BaseURL:     "https://panel.example.com/",
		AccessToken: "first-private-token",
	})
	if err != nil {
		t.Fatalf("SaveSettings create: %v", err)
	}
	if view.Profile != model.ChannelManagementProfileNewAPI || view.BaseURL != "https://panel.example.com" || !view.CredentialConfigured {
		t.Fatalf("created view = %#v", view)
	}

	stored, err := server.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("ParseChannelManagementEnvelope: %v", err)
	}
	checkinAt := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	envelope.State.LastCheckinStatus = "success"
	envelope.State.LastCheckinAt = &checkinAt
	rawWithState, err := envelope.Marshal()
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	updated, err := server.store.CompareAndSwapChannelManagement(ctx, cfg.ID, stored.OAuthCredential, rawWithState)
	if err != nil || !updated {
		t.Fatalf("seed state = (%v, %v)", updated, err)
	}
	stored.OAuthCredential = rawWithState

	view, err = service.SaveSettings(ctx, stored, &channelManagementInput{
		Profile: model.ChannelManagementProfileNewAPI,
		BaseURL: "https://panel.example.com/",
	})
	if err != nil {
		t.Fatalf("SaveSettings same profile without token: %v", err)
	}
	if view.LastCheckinStatus != "success" || view.LastCheckinAt == nil || !view.LastCheckinAt.Equal(checkinAt) {
		t.Fatalf("settings update lost state: %#v", view)
	}
	stored, err = server.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig updated: %v", err)
	}
	envelope, err = model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("Parse updated envelope: %v", err)
	}
	if envelope.Settings.AccessToken != "first-private-token" {
		t.Fatalf("same-profile empty token replaced credential: %q", envelope.Settings.AccessToken)
	}
	publicJSON, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if bytes.Contains(publicJSON, []byte("first-private-token")) || bytes.Contains(publicJSON, []byte(model.ChannelManagementKind)) {
		t.Fatalf("management view leaked private envelope: %s", publicJSON)
	}

	if _, err = service.SaveSettings(ctx, stored, &channelManagementInput{
		Profile: model.ChannelManagementProfileSub2APIPro,
		BaseURL: "https://pro.example.com",
	}); err == nil {
		t.Fatal("profile switch accepted an empty access token")
	}

	view, err = service.SaveSettings(ctx, stored, &channelManagementInput{})
	if err != nil {
		t.Fatalf("SaveSettings disable: %v", err)
	}
	if view.CredentialConfigured || view.Profile != "" || view.BaseURL != "" {
		t.Fatalf("disabled view = %#v", view)
	}
	stored, err = server.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig disabled: %v", err)
	}
	if stored.OAuthCredential != "" {
		t.Fatalf("disable retained private envelope: %q", stored.OAuthCredential)
	}

	fresh := createChannelManagementTestConfig(t, server.store, "fresh")
	if _, err = service.SaveSettings(ctx, fresh, &channelManagementInput{
		Profile: model.ChannelManagementProfileNewAPI,
		BaseURL: "https://fresh.example.com",
	}); err == nil {
		t.Fatal("new management profile accepted an empty access token")
	}
}

func TestChannelManagementServiceSaveSettingsPreservesHiddenUserIDForSameAccount(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "preserve-user")
	userID := int64(42)
	checkinAt := time.Date(2026, 8, 25, 7, 30, 0, 0, time.UTC)
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, &model.ChannelManagementEnvelope{
		Kind:    model.ChannelManagementKind,
		Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: "https://panel.example.com", AccessToken: "same-private-token", UserID: &userID,
		},
		State: model.ChannelManagementState{
			LastScheduledDay: "2026-08-25", LastCheckinStatus: "success", LastCheckinAt: &checkinAt,
		},
	})
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client { return server.client })

	view, err := service.SaveSettings(context.Background(), cfg, &channelManagementInput{
		Profile:             model.ChannelManagementProfileNewAPI,
		BaseURL:             "https://panel.example.com/",
		DailyCheckinEnabled: true,
		DailyCheckinTime:    "09:30",
	})
	if err != nil {
		t.Fatalf("SaveSettings ordinary edit: %v", err)
	}
	if !view.UserIDConfigured {
		t.Fatalf("ordinary edit cleared hidden user ID: %#v", view)
	}
	if view.LastCheckinStatus != "success" || view.LastCheckinAt == nil || !view.LastCheckinAt.Equal(checkinAt) {
		t.Fatalf("daily-only edit cleared state: %#v", view)
	}
	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if envelope.Settings.UserID == nil || *envelope.Settings.UserID != userID {
		t.Fatalf("stored user ID = %v, want %d", envelope.Settings.UserID, userID)
	}
	if envelope.State.LastScheduledDay != "2026-08-25" {
		t.Fatalf("daily-only edit cleared scheduler state: %#v", envelope.State)
	}
}

func TestChannelManagementServiceSaveSettingsClearsStateWhenAccountIdentityChanges(t *testing.T) {
	t.Parallel()
	nextUserID := int64(43)
	tests := []struct {
		name  string
		input channelManagementInput
	}{
		{
			name: "profile",
			input: channelManagementInput{
				Profile: model.ChannelManagementProfileSub2API, BaseURL: "https://panel.example.com",
				sub2APISession: &sub2APIManagementSession{
					AccessToken: "next-private-token", RefreshToken: "next-refresh-token",
					ExpiresAt: time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC), AccountID: 43,
				},
			},
		},
		{
			name: "base URL",
			input: channelManagementInput{
				Profile: model.ChannelManagementProfileNewAPI, BaseURL: "https://other.example.com",
			},
		},
		{
			name: "access token",
			input: channelManagementInput{
				Profile: model.ChannelManagementProfileNewAPI, BaseURL: "https://panel.example.com", AccessToken: "next-private-token",
			},
		},
		{
			name: "user ID",
			input: channelManagementInput{
				Profile: model.ChannelManagementProfileNewAPI, BaseURL: "https://panel.example.com", UserID: &nextUserID,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newInMemoryServer(t)
			cfg := createChannelManagementTestConfig(t, server.store, "identity-"+tt.name)
			oldUserID := int64(42)
			checkinAt := time.Date(2026, 8, 25, 7, 30, 0, 0, time.UTC)
			remaining := 12.5
			cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, &model.ChannelManagementEnvelope{
				Kind:    model.ChannelManagementKind,
				Version: model.ChannelManagementVersion,
				Profile: model.ChannelManagementProfileNewAPI,
				Settings: model.ChannelManagementSettings{
					BaseURL: "https://panel.example.com", AccessToken: "same-private-token", UserID: &oldUserID,
				},
				State: model.ChannelManagementState{
					LastScheduledDay: "2026-08-25", LastCheckinStatus: "success", LastCheckinAt: &checkinAt,
					LastBalance: &model.ChannelManagementBalanceSnapshot{BalanceUSD: &remaining, SampledAt: checkinAt},
				},
			})
			service := newChannelManagementService(server.store, func(*model.Config) *http.Client { return server.client })

			view, err := service.SaveSettings(context.Background(), cfg, &tt.input)
			if err != nil {
				t.Fatalf("SaveSettings identity change: %v", err)
			}
			if view.LastCheckinStatus != "" || view.LastCheckinAt != nil || view.Balance != nil {
				t.Fatalf("identity change retained public state: %#v", view)
			}
			stored, err := server.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
			if err != nil {
				t.Fatalf("parse envelope: %v", err)
			}
			if envelope.State.LastScheduledDay != "" || envelope.State.LastCheckinStatus != "" ||
				envelope.State.LastCheckinAt != nil || envelope.State.LastBalance != nil {
				t.Fatalf("identity change retained stored state: %#v", envelope.State)
			}
		})
	}
}

func TestChannelManagementServiceSaveSettingsWrapsValidationAsInvalidRequest(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "invalid-settings")
	const token = "validation-private-token"
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client { return server.client })

	_, err := service.SaveSettings(context.Background(), cfg, &channelManagementInput{
		Profile:     model.ChannelManagementProfileNewAPI,
		BaseURL:     "https://panel.example.com/not-root",
		AccessToken: token,
	})
	if !errors.Is(err, errInvalidManagementRequest) {
		t.Fatalf("validation error = %v, want errors.Is(errInvalidManagementRequest)", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("validation error leaked token: %v", err)
	}
}

type channelManagementConflictStore struct {
	storage.Store
	mu       sync.Mutex
	conflict bool
	contexts []context.Context
}

func (s *channelManagementConflictStore) CompareAndSwapChannelManagement(
	ctx context.Context,
	channelID int64,
	expectedEnvelope string,
	nextEnvelope string,
) (bool, error) {
	s.mu.Lock()
	s.contexts = append(s.contexts, ctx)
	forceConflict := !s.conflict
	if forceConflict {
		s.conflict = true
	}
	s.mu.Unlock()
	if !forceConflict {
		return s.Store.CompareAndSwapChannelManagement(ctx, channelID, expectedEnvelope, nextEnvelope)
	}

	concurrent, err := model.ParseChannelManagementEnvelope(expectedEnvelope)
	if err != nil {
		return false, err
	}
	concurrent.State.LastCheckinStatus = "concurrent-success"
	concurrentRaw, err := concurrent.Marshal()
	if err != nil {
		return false, err
	}
	updated, err := s.Store.CompareAndSwapChannelManagement(ctx, channelID, expectedEnvelope, concurrentRaw)
	if err != nil || !updated {
		return false, err
	}
	return false, nil
}

func TestChannelManagementServiceSaveSettingsCASConflictMergesLatestLocalEnvelope(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	ctx := context.Background()
	cfg := createChannelManagementTestConfig(t, server.store, "cas")
	seed := &model.ChannelManagementEnvelope{
		Kind:    model.ChannelManagementKind,
		Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileSub2API,
		Settings: model.ChannelManagementSettings{
			BaseURL: "https://old.example.com", AccessToken: "old-private-token",
		},
		State: model.ChannelManagementState{LastCheckinStatus: "old-success"},
	}
	seedRaw, err := seed.Marshal()
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	updated, err := server.store.CompareAndSwapChannelManagement(ctx, cfg.ID, "", seedRaw)
	if err != nil || !updated {
		t.Fatalf("seed CAS = (%v, %v)", updated, err)
	}
	cfg.OAuthCredential = seedRaw

	conflicts := &channelManagementConflictStore{Store: server.store}
	service := newChannelManagementService(conflicts, func(*model.Config) *http.Client { return server.client })
	view, err := service.SaveSettings(ctx, cfg, &channelManagementInput{
		Profile: model.ChannelManagementProfileSub2API,
		BaseURL: "https://old.example.com/",
	})
	if err != nil {
		t.Fatalf("SaveSettings after CAS conflict: %v", err)
	}
	if view.LastCheckinStatus != "concurrent-success" {
		t.Fatalf("CAS retry overwrote concurrent state: %#v", view)
	}
	stored, err := server.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse final envelope: %v", err)
	}
	if envelope.Settings.AccessToken != "old-private-token" || envelope.Settings.BaseURL != "https://old.example.com" {
		t.Fatalf("CAS retry merged envelope = %#v", envelope)
	}
	conflicts.mu.Lock()
	defer conflicts.mu.Unlock()
	if len(conflicts.contexts) != 2 || conflicts.contexts[0] != conflicts.contexts[1] {
		t.Fatalf("CAS retry did not reuse one operation context: %#v", conflicts.contexts)
	}
}

type channelManagementBlockingStore struct {
	storage.Store
	blockID int64
	entered chan int64
	release chan struct{}

	mu        sync.Mutex
	deadlines map[int64]time.Time
}

func (s *channelManagementBlockingStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.deadlines[id] = deadline
		s.mu.Unlock()
	}
	s.entered <- id
	if id == s.blockID {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.GetConfig(ctx, id)
}

func TestChannelManagementServiceGateAndOperationDeadline(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	first := createChannelManagementTestConfig(t, server.store, "first")
	second := createChannelManagementTestConfig(t, server.store, "second")
	store := &channelManagementBlockingStore{
		Store: server.store, blockID: first.ID,
		entered: make(chan int64, 4), release: make(chan struct{}), deadlines: make(map[int64]time.Time),
	}
	service := newChannelManagementService(store, func(*model.Config) *http.Client { return server.client })

	firstDone := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := service.RefreshBalance(context.Background(), first.ID)
		firstDone <- err
	}()
	if got := <-store.entered; got != first.ID {
		t.Fatalf("first operation entered channel %d", got)
	}

	blockedCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	blockedDone := make(chan error, 1)
	go func() {
		_, err := service.CheckIn(blockedCtx, first.ID)
		blockedDone <- err
	}()

	otherDone := make(chan error, 1)
	go func() {
		_, err := service.RefreshBalance(context.Background(), second.ID)
		otherDone <- err
	}()
	select {
	case got := <-store.entered:
		if got != second.ID {
			t.Fatalf("same-channel operation bypassed gate before channel %d", second.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("different channel was blocked by channel gate")
	}
	if err := <-otherDone; !errors.Is(err, errChannelManagementNotConfigured) {
		t.Fatalf("different-channel RefreshBalance error = %v", err)
	}
	if err := <-blockedDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked same-channel CheckIn error = %v", err)
	}
	select {
	case got := <-store.entered:
		t.Fatalf("timed-out same-channel operation reached store for channel %d", got)
	default:
	}
	close(store.release)
	if err := <-firstDone; !errors.Is(err, errChannelManagementNotConfigured) {
		t.Fatalf("first RefreshBalance error = %v", err)
	}

	store.mu.Lock()
	deadline := store.deadlines[first.ID]
	store.mu.Unlock()
	remainingAtStart := deadline.Sub(started)
	if remainingAtStart < 29*time.Second || remainingAtStart > 31*time.Second {
		t.Fatalf("operation deadline = %s from start, want 30s", remainingAtStart)
	}
}

func TestManagementRequestBoundaries(t *testing.T) {
	t.Parallel()
	cfg := &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}

	t.Run("Bearer and HTTPS context marker", func(t *testing.T) {
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer private-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if req.Context().Value(chromeUTLSContextKey{}) == nil {
				t.Fatal("HTTPS management request missing context uTLS marker")
			}
			if req.Header.Get("X-CCLoad-UTLS") != "" {
				t.Fatalf("management request leaked an internal marker header: %v", req.Header)
			}
			return managementTestResponse(req, http.StatusOK, "ok"), nil
		})
		base := &http.Client{Transport: transport, Timeout: time.Nanosecond}
		service := newChannelManagementService(nil, func(*model.Config) *http.Client { return base })
		result, err := service.doManagementRequest(context.Background(), cfg, http.MethodGet, "https://management.example/status", "private-token", nil)
		if err != nil {
			t.Fatalf("doManagementRequest: %v", err)
		}
		if result.StatusCode != http.StatusOK || string(result.Body) != "ok" {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("HTTP is never marked for TLS", func(t *testing.T) {
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Context().Value(chromeUTLSContextKey{}) != nil {
				t.Fatal("HTTP management request carried the uTLS context marker")
			}
			return managementTestResponse(req, http.StatusOK, "ok"), nil
		})
		service := newChannelManagementService(nil, func(*model.Config) *http.Client {
			return &http.Client{Transport: transport}
		})
		if _, err := service.doManagementRequest(context.Background(), cfg, http.MethodGet, "http://management.example/status", "private-token", nil); err != nil {
			t.Fatalf("HTTP request: %v", err)
		}
	})

	t.Run("response body limit", func(t *testing.T) {
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return managementTestResponse(req, http.StatusOK, strings.Repeat("x", 64*1024+1)), nil
		})
		service := newChannelManagementService(nil, func(*model.Config) *http.Client {
			return &http.Client{Transport: transport}
		})
		result, err := service.doManagementRequest(context.Background(), cfg, http.MethodGet, "https://management.example/large", "private-token", nil)
		if result == nil || result.Body != nil || result.StatusCode != http.StatusOK || err == nil || err.Error() != "invalid_response" {
			t.Fatalf("oversized response = (%#v, %v), want invalid_response", result, err)
		}
	})

	t.Run("redirect is returned and never followed", func(t *testing.T) {
		var followed atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/final" {
				followed.Add(1)
				_, _ = io.WriteString(w, "secret-final-body")
				return
			}
			http.Redirect(w, req, "/final", http.StatusFound)
		}))
		defer upstream.Close()
		service := newChannelManagementService(nil, func(*model.Config) *http.Client { return upstream.Client() })
		result, err := service.doManagementRequest(context.Background(), cfg, http.MethodGet, upstream.URL+"/start", "private-token", nil)
		if err != nil {
			t.Fatalf("redirect request: %v", err)
		}
		if result.StatusCode != http.StatusFound || followed.Load() != 0 {
			t.Fatalf("redirect result = %#v, followed = %d", result, followed.Load())
		}
	})

	t.Run("safe error and POST write boundary", func(t *testing.T) {
		const token = "post-private-token"
		const upstreamSecret = "raw-upstream-secret"
		var calls atomic.Int32
		var lastRequest atomic.Value
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			lastRequest.Store(req)
			if req.GetBody == nil {
				t.Fatal("management POST body must be replayable before it is written")
			}
			if _, replayErr := req.GetBody(); replayErr != nil {
				t.Fatalf("pre-write replay refused: %v", replayErr)
			}
			trace := httptrace.ContextClientTrace(req.Context())
			if trace == nil || trace.WroteRequest == nil {
				t.Fatal("POST request missing WroteRequest trace")
			}
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			return nil, errors.New(upstreamSecret + " " + token)
		})
		service := newChannelManagementService(nil, func(*model.Config) *http.Client {
			return &http.Client{Transport: transport}
		})
		result, err := service.doManagementRequest(context.Background(), cfg, http.MethodPost, "https://management.example/checkin", token, []byte(`{}`))
		if err == nil || err.Error() != "management_request_failed" {
			t.Fatalf("POST error = %v", err)
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), upstreamSecret) {
			t.Fatalf("POST error leaked secret: %v", err)
		}
		if result == nil || !result.WroteRequest || calls.Load() != 1 {
			t.Fatalf("POST result = %#v, calls = %d", result, calls.Load())
		}
		// 请求已写出：任何后续重放都必须被拒绝，否则 uTLS 回退会重复签到。
		if _, replayErr := lastRequest.Load().(*http.Request).GetBody(); replayErr == nil {
			t.Fatal("management POST replayed after the request was already written")
		}
	})
}

// 真实 uTLS 传输在首候选失败后会换传输重发，必须能重放尚未写出的请求体，
// 否则所有管理 POST（签到）在真实链路上永远发不出去。
func TestManagementCheckinPOSTSurvivesUTLSFallback(t *testing.T) {
	t.Parallel()
	cfg := &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}

	type received struct {
		method string
		body   string
	}
	posts := make(chan received, 4)
	// 仅 HTTP/1.1：uTLS 的 h2 首候选握手失败，强制走回退候选。
	upstream, _ := newCapturedTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		posts <- received{method: r.Method, body: string(raw)}
		_, _ = io.WriteString(w, `{"success":true}`)
	}))

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client := newUpstreamHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })

	service := newChannelManagementService(nil, func(*model.Config) *http.Client { return client })
	result, err := service.doManagementRequest(
		context.Background(), cfg, http.MethodPost,
		"https://management.example/api/user/checkin", "private-token", []byte(`{}`),
	)
	if err != nil {
		t.Fatalf("management POST over uTLS fallback: %v", err)
	}
	if result.StatusCode != http.StatusOK || string(result.Body) != `{"success":true}` {
		t.Fatalf("result = %#v", result)
	}

	close(posts)
	var got []received
	for r := range posts {
		got = append(got, r)
	}
	if len(got) != 1 {
		t.Fatalf("upstream received %d requests, want exactly 1 (no double checkin): %#v", len(got), got)
	}
	if got[0].method != http.MethodPost || got[0].body != `{}` {
		t.Fatalf("upstream request = %#v, want POST with body {}", got[0])
	}
}

func createChannelManagementTestConfig(t *testing.T, store storage.Store, name string) *model.Config {
	t.Helper()
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name: name, AuthType: model.AuthTypeAPIKey, URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	return cfg
}

func seedChannelManagementTestEnvelope(
	t *testing.T,
	store storage.Store,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) *model.Config {
	t.Helper()
	raw, err := envelope.Marshal()
	if err != nil {
		t.Fatalf("marshal seed envelope: %v", err)
	}
	updated, err := store.CompareAndSwapChannelManagement(context.Background(), cfg.ID, cfg.OAuthCredential, raw)
	if err != nil || !updated {
		t.Fatalf("seed envelope CAS = (%v, %v)", updated, err)
	}
	seeded := cfg.Clone()
	seeded.OAuthCredential = raw
	return seeded
}

func managementTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
