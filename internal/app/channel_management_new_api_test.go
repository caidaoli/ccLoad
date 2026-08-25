package app

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestNewAPIBalancePreservesRawQuotaAndHeaders(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	userID := int64(42)
	tests := []struct {
		name       string
		userID     *int64
		wantHeader string
		body       string
		wantUsed   *int64
		wantTotal  *int64
	}{
		{
			name:       "explicit user id",
			userID:     &userID,
			wantHeader: "42",
			body:       `{"success":true,"message":"","data":{"id":42,"quota":750000,"used_quota":250000}}`,
			wantUsed:   new(int64(250000)),
			wantTotal:  new(int64(1000000)),
		},
		{
			name: "missing used quota remains unknown",
			body: `{"success":true,"message":"","data":{"id":7,"quota":750000}}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet || req.URL.String() != "https://panel.example.com/api/user/self" {
						t.Fatalf("balance request = %s %s", req.Method, req.URL.String())
					}
					if got := req.Header.Get("Authorization"); got != "Bearer private-token" {
						t.Fatalf("Authorization = %q", got)
					}
					if got := req.Header.Get("New-API-User"); got != tt.wantHeader {
						t.Fatalf("New-API-User = %q, want %q", got, tt.wantHeader)
					}
					return newAPIHTTPResponse(req, http.StatusOK, tt.body), nil
				})}
			})
			service.now = func() time.Time { return fixedNow }
			envelope := newAPITestEnvelope(tt.userID)
			snapshot, status, err := service.refreshNewAPIBalance(context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}, envelope)
			if err != nil {
				t.Fatalf("refreshNewAPIBalance: %v", err)
			}
			if status != http.StatusOK || snapshot == nil || snapshot.RemainingRaw == nil || *snapshot.RemainingRaw != 750000 {
				t.Fatalf("balance result = (%#v, %d)", snapshot, status)
			}
			if snapshot.Divisor != 500000 || snapshot.SampledAt != fixedNow || snapshot.BalanceUSD != nil {
				t.Fatalf("raw balance snapshot = %#v", snapshot)
			}
			assertNewAPIOptionalInt64(t, "used", snapshot.UsedRaw, tt.wantUsed)
			assertNewAPIOptionalInt64(t, "total", snapshot.TotalRaw, tt.wantTotal)

			view := channelManagementBalanceViewFromSnapshot(snapshot)
			if view == nil || view.Remaining != 1.5 {
				t.Fatalf("balance view = %#v", view)
			}
			if tt.wantUsed == nil {
				if view.Used != nil || view.Total != nil || view.AvailablePercent != nil {
					t.Fatalf("missing used quota fabricated derived values: %#v", view)
				}
				return
			}
			if view.Used == nil || *view.Used != 0.5 || view.Total == nil || *view.Total != 2 || view.AvailablePercent == nil || *view.AvailablePercent != 75 {
				t.Fatalf("derived balance view = %#v", view)
			}
		})
	}
}

func TestNewAPIBalanceRejectsUnsafeResponses(t *testing.T) {
	t.Parallel()
	const token = "private-token"
	const secret = "raw-upstream-secret"
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "negative remaining", status: http.StatusOK, body: `{"success":true,"data":{"id":7,"quota":-1}}`, wantStatus: http.StatusOK},
		{name: "negative used", status: http.StatusOK, body: `{"success":true,"data":{"id":7,"quota":1,"used_quota":-1}}`, wantStatus: http.StatusOK},
		{name: "total overflow", status: http.StatusOK, body: `{"success":true,"data":{"id":7,"quota":` + strconv.FormatInt(math.MaxInt64, 10) + `,"used_quota":1}}`, wantStatus: http.StatusOK},
		{name: "business failure", status: http.StatusOK, body: `{"success":false,"message":"` + secret + ` ` + token + `"}`, wantStatus: http.StatusOK},
		{name: "missing data", status: http.StatusOK, body: `{"success":true}`, wantStatus: http.StatusOK},
		{name: "null data", status: http.StatusOK, body: `{"success":true,"data":null}`, wantStatus: http.StatusOK},
		{name: "non json", status: http.StatusOK, body: secret, wantStatus: http.StatusOK},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", managementResponseBodyLimit+1), wantStatus: http.StatusOK},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"message":"` + secret + `"}`, wantStatus: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{"message":"` + token + `"}`, wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return newAPIHTTPResponse(req, tt.status, tt.body), nil
				})}
			})
			snapshot, status, err := service.refreshNewAPIBalance(context.Background(), &model.Config{ID: 1, AuthType: model.AuthTypeAPIKey}, newAPITestEnvelope(nil))
			if err == nil || snapshot != nil || status != tt.wantStatus {
				t.Fatalf("unsafe balance result = (%#v, %d, %v)", snapshot, status, err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), tt.body) {
				t.Fatalf("balance error leaked response or credential: %v", err)
			}
		})
	}
}

func TestNewAPICheckinStateMachine(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	statusEnabled := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/status",
		status: http.StatusOK, body: `{"success":true,"data":{"checkin_enabled":true}}`,
	}
	monthUnchecked := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
		status: http.StatusOK, body: `{"success":true,"data":{"stats":{"checked_in_today":false}}}`,
	}
	monthChecked := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
		status: http.StatusOK, body: `{"success":true,"data":{"stats":{"checked_in_today":true}}}`,
	}
	balance := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/user/self",
		status: http.StatusOK, body: `{"success":true,"data":{"id":42,"quota":750000,"used_quota":250000}}`,
	}
	tests := []struct {
		name           string
		steps          []newAPIStep
		wantStatus     string
		wantStatusCode int
		wantReward     *float64
		wantBalance    bool
		wantError      bool
		wantPosts      int
		wantReadbacks  int
	}{
		{
			name: "disabled",
			steps: []newAPIStep{{
				method: http.MethodGet, target: "https://panel.example.com/api/status",
				status: http.StatusOK, body: `{"success":true,"data":{"checkin_enabled":false}}`,
			}},
			wantStatus: "skipped_disabled", wantStatusCode: http.StatusOK,
		},
		{
			name:       "already checked without post",
			steps:      []newAPIStep{statusEnabled, monthChecked, balance},
			wantStatus: "already_checked", wantStatusCode: http.StatusOK, wantBalance: true,
		},
		{
			name: "success",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":true,"data":{"quota_awarded":250000,"checkin_date":"2026-08-25"}}`,
			}, balance},
			wantStatus: "success", wantStatusCode: http.StatusOK, wantReward: new(0.5), wantBalance: true, wantPosts: 1,
		},
		{
			name: "business failure readback wins",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"ambiguous raw-upstream-secret"}`,
			}, monthChecked, balance},
			wantStatus: "already_checked", wantStatusCode: http.StatusOK, wantBalance: true, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "turnstile requires manual action",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"Turnstile verification required raw-upstream-secret"}`,
			}, monthUnchecked},
			wantStatus: "manual_required", wantStatusCode: http.StatusOK, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "unsupported",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusMethodNotAllowed, wrote: true, body: `{"message":"raw-upstream-secret"}`,
			}, monthUnchecked},
			wantStatus: "unsupported", wantStatusCode: http.StatusMethodNotAllowed, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "credential invalid",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusForbidden, wrote: true, body: `{"message":"private-token"}`,
			}, monthUnchecked},
			wantStatus: "credential_invalid", wantStatusCode: http.StatusForbidden, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "credential classification survives forbidden readback",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusForbidden, wrote: true, body: `{"message":"private-token"}`,
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				status: http.StatusForbidden, body: `{"message":"private-token"}`,
			}},
			wantStatus: "credential_invalid", wantStatusCode: http.StatusForbidden, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "not found classification survives transport readback failure",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusNotFound, wrote: true, body: `{"message":"raw-upstream-secret"}`,
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				err: errors.New("raw-upstream-secret"),
			}},
			wantStatus: "unsupported", wantStatusCode: http.StatusNotFound, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "method not allowed classification survives non-2xx readback",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusMethodNotAllowed, wrote: true, body: `{"message":"raw-upstream-secret"}`,
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				status: http.StatusServiceUnavailable, body: `{"message":"raw-upstream-secret"}`,
			}},
			wantStatus: "unsupported", wantStatusCode: http.StatusMethodNotAllowed, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "turnstile classification survives transport readback failure",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"Turnstile verification required raw-upstream-secret"}`,
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				err: errors.New("raw-upstream-secret"),
			}},
			wantStatus: "manual_required", wantStatusCode: http.StatusOK, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "turnstile classification survives non-2xx readback",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"Turnstile verification required raw-upstream-secret"}`,
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				status: http.StatusServiceUnavailable, body: `{"message":"raw-upstream-secret"}`,
			}},
			wantStatus: "manual_required", wantStatusCode: http.StatusOK, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "checked readback overrides turnstile",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"Turnstile verification required raw-upstream-secret"}`,
			}, monthChecked, balance},
			wantStatus: "already_checked", wantStatusCode: http.StatusOK, wantBalance: true, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "checked readback overrides unsupported",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusNotFound, wrote: true, body: `{"message":"raw-upstream-secret"}`,
			}, monthChecked, balance},
			wantStatus: "already_checked", wantStatusCode: http.StatusNotFound, wantBalance: true, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "checked readback overrides invalid credential",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusForbidden, wrote: true, body: `{"message":"private-token"}`,
			}, monthChecked, balance},
			wantStatus: "already_checked", wantStatusCode: http.StatusForbidden, wantBalance: true, wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "written post and failed readback is uncertain",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				wrote: true, err: errors.New("raw-upstream-secret private-token"),
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				err: errors.New("raw-upstream-secret"),
			}},
			wantStatus: "uncertain", wantPosts: 1, wantReadbacks: 1,
		},
		{
			name: "unwritten post is an ordinary safe error",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				err: errors.New("raw-upstream-secret private-token"),
			}},
			wantError: true, wantPosts: 1,
		},
		{
			name: "status endpoint unsupported without post",
			steps: []newAPIStep{{
				method: http.MethodGet, target: "https://panel.example.com/api/status",
				status: http.StatusNotFound, body: `{"message":"raw-upstream-secret"}`,
			}},
			wantStatus: "unsupported", wantStatusCode: http.StatusNotFound,
		},
		{
			name: "monthly endpoint rejects credential without post",
			steps: []newAPIStep{statusEnabled, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				status: http.StatusUnauthorized, body: `{"message":"private-token"}`,
			}},
			wantStatus: "credential_invalid", wantStatusCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			script := &newAPIScript{t: t, steps: tt.steps}
			service := newChannelManagementService(nil, func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			})
			service.now = func() time.Time { return fixedNow }
			envelope := newAPITestEnvelope(new(int64(42)))
			result, snapshot, err := service.checkInNewAPI(
				context.Background(),
				&model.Config{ID: 1, AuthType: model.AuthTypeAPIKey},
				envelope,
			)
			requests := script.finishedRequests()
			if tt.wantError {
				if err == nil || result != nil || snapshot != nil {
					t.Fatalf("ordinary failure result = (%#v, %#v, %v)", result, snapshot, err)
				}
				if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "raw-upstream-secret") {
					t.Fatalf("checkin error leaked a secret: %v", err)
				}
			} else {
				if err != nil || result == nil || result.Status != tt.wantStatus || result.StatusCode != tt.wantStatusCode {
					t.Fatalf("checkin result = (%#v, %#v, %v)", result, snapshot, err)
				}
				if result.CheckedInAt == nil || *result.CheckedInAt != fixedNow {
					t.Fatalf("checkin time = %v, want %v", result.CheckedInAt, fixedNow)
				}
				if tt.wantReward == nil {
					if result.Reward != nil {
						t.Fatalf("reward = %v, want nil", *result.Reward)
					}
				} else if result.Reward == nil || *result.Reward != *tt.wantReward {
					t.Fatalf("reward = %v, want %v", result.Reward, *tt.wantReward)
				}
				if tt.wantBalance != (snapshot != nil) {
					t.Fatalf("balance snapshot = %#v, want present %t", snapshot, tt.wantBalance)
				}
			}
			posts := 0
			readbacks := 0
			sawPost := false
			for _, request := range requests {
				if request.authorization != "Bearer private-token" || request.userID != "42" {
					t.Fatalf("request leaked or omitted New API headers: %#v", request)
				}
				if request.method == http.MethodPost {
					posts++
					sawPost = true
					if request.body != `{}` || request.contentType != "application/json" {
						t.Fatalf("POST contract = %#v", request)
					}
				} else if sawPost && request.method == http.MethodGet &&
					request.target == "https://panel.example.com/api/user/checkin?month=2026-08" {
					readbacks++
				}
			}
			if posts != tt.wantPosts || readbacks != tt.wantReadbacks {
				t.Fatalf(
					"request counts = (POST %d, readback %d), want (%d, %d); requests = %#v",
					posts, readbacks, tt.wantPosts, tt.wantReadbacks, requests,
				)
			}
		})
	}
}

func TestChannelManagementServiceNewAPIRefreshBalancePersistsSnapshot(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "new-api-balance")
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, newAPITestEnvelope(new(int64(42))))
	script := &newAPIScript{t: t, steps: []newAPIStep{{
		method: http.MethodGet, target: "https://panel.example.com/api/user/self",
		status: http.StatusOK, body: `{"success":true,"data":{"id":42,"quota":750000,"used_quota":250000}}`,
	}}}
	service := newChannelManagementService(server.store, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	view, err := service.RefreshBalance(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("RefreshBalance: %v", err)
	}
	if view.Balance == nil || view.Balance.Remaining != 1.5 || view.Balance.Used == nil || *view.Balance.Used != 0.5 {
		t.Fatalf("balance view = %#v", view)
	}
	script.finishedRequests()
	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse stored envelope: %v", err)
	}
	if envelope.State.LastBalance == nil || envelope.State.LastBalance.RemainingRaw == nil ||
		*envelope.State.LastBalance.RemainingRaw != 750000 || envelope.State.LastBalance.BalanceUSD != nil {
		t.Fatalf("stored balance = %#v", envelope.State.LastBalance)
	}
}

func TestChannelManagementServiceNewAPICheckinCASConflictMergesWithoutReplaying(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	cfg := createChannelManagementTestConfig(t, server.store, "new-api-checkin-cas")
	seed := newAPITestEnvelope(new(int64(42)))
	seed.Settings.DailyCheckinEnabled = true
	seed.Settings.DailyCheckinTime = "09:30"
	cfg = seedChannelManagementTestEnvelope(t, server.store, cfg, seed)

	steps := []newAPIStep{
		{
			method: http.MethodGet, target: "https://panel.example.com/api/status",
			status: http.StatusOK, body: `{"success":true,"data":{"checkin_enabled":true}}`,
		},
		{
			method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
			status: http.StatusOK, body: `{"success":true,"data":{"stats":{"checked_in_today":false}}}`,
		},
		{
			method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
			status: http.StatusOK, wrote: true,
			body: `{"success":true,"data":{"quota_awarded":250000,"checkin_date":"2026-08-25"}}`,
		},
		{
			method: http.MethodGet, target: "https://panel.example.com/api/user/self",
			status: http.StatusOK, body: `{"success":true,"data":{"id":42,"quota":750000,"used_quota":250000}}`,
		},
	}
	script := &newAPIScript{t: t, steps: steps}
	conflicts := &newAPIManagementConflictStore{Store: server.store}
	service := newChannelManagementService(conflicts, func(*model.Config) *http.Client {
		return &http.Client{Transport: script}
	})
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	result, err := service.CheckIn(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if result.Status != "success" || result.CheckedInAt == nil || *result.CheckedInAt != fixedNow ||
		result.Balance == nil || result.Balance.Remaining != 1.5 {
		t.Fatalf("checkin result = %#v", result)
	}
	requests := script.finishedRequests()
	posts := 0
	for _, request := range requests {
		if request.method == http.MethodPost {
			posts++
		}
	}
	if len(requests) != 4 || posts != 1 {
		t.Fatalf("CAS replayed upstream requests: %#v", requests)
	}

	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("parse stored envelope: %v", err)
	}
	if envelope.State.LastScheduledDay != "concurrent-day" ||
		envelope.State.LastCheckinStatus != "success" ||
		envelope.State.LastCheckinAt == nil || *envelope.State.LastCheckinAt != fixedNow ||
		envelope.State.LastBalance == nil || envelope.State.LastBalance.RemainingRaw == nil ||
		*envelope.State.LastBalance.RemainingRaw != 750000 {
		t.Fatalf("CAS merge lost state or split checkin and balance: %#v", envelope.State)
	}

	conflicts.mu.Lock()
	defer conflicts.mu.Unlock()
	if len(conflicts.contexts) != 2 || conflicts.contexts[0] != conflicts.contexts[1] {
		t.Fatalf("CAS retry did not reuse one operation context: %#v", conflicts.contexts)
	}
	for index, candidate := range conflicts.candidates {
		if candidate.State.LastCheckinStatus != "success" || candidate.State.LastBalance == nil {
			t.Fatalf("CAS candidate %d did not contain one complete state update: %#v", index, candidate.State)
		}
	}
}

type newAPIManagementConflictStore struct {
	storage.Store
	mu         sync.Mutex
	conflicted bool
	contexts   []context.Context
	candidates []*model.ChannelManagementEnvelope
}

func (s *newAPIManagementConflictStore) CompareAndSwapChannelManagement(
	ctx context.Context,
	channelID int64,
	expectedEnvelope string,
	nextEnvelope string,
) (bool, error) {
	candidate, err := model.ParseChannelManagementEnvelope(nextEnvelope)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.contexts = append(s.contexts, ctx)
	s.candidates = append(s.candidates, candidate)
	forceConflict := !s.conflicted
	if forceConflict {
		s.conflicted = true
	}
	s.mu.Unlock()
	if !forceConflict {
		return s.Store.CompareAndSwapChannelManagement(ctx, channelID, expectedEnvelope, nextEnvelope)
	}

	concurrent, err := model.ParseChannelManagementEnvelope(expectedEnvelope)
	if err != nil {
		return false, err
	}
	concurrent.State.LastScheduledDay = "concurrent-day"
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

type newAPIStep struct {
	method string
	target string
	status int
	body   string
	err    error
	wrote  bool
}

type newAPIRecordedRequest struct {
	method        string
	target        string
	body          string
	authorization string
	userID        string
	contentType   string
}

type newAPIScript struct {
	t        *testing.T
	mu       sync.Mutex
	steps    []newAPIStep
	requests []newAPIRecordedRequest
}

func (s *newAPIScript) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := len(s.requests)
	if index >= len(s.steps) {
		s.t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
	}
	step := s.steps[index]
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
	}
	s.requests = append(s.requests, newAPIRecordedRequest{
		method: req.Method, target: req.URL.String(), body: string(body),
		authorization: req.Header.Get("Authorization"), userID: req.Header.Get("New-API-User"),
		contentType: req.Header.Get("Content-Type"),
	})
	if req.Method != step.method || req.URL.String() != step.target {
		s.t.Fatalf("request %d = %s %s, want %s %s", index, req.Method, req.URL.String(), step.method, step.target)
	}
	if step.wrote {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil || trace.WroteRequest == nil {
			s.t.Fatal("written POST missing WroteRequest trace")
		}
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	if step.err != nil {
		return nil, step.err
	}
	return newAPIHTTPResponse(req, step.status, step.body), nil
}

func (s *newAPIScript) finishedRequests() []newAPIRecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != len(s.steps) {
		s.t.Fatalf("request count = %d, want %d", len(s.requests), len(s.steps))
	}
	return append([]newAPIRecordedRequest(nil), s.requests...)
}

func newAPITestEnvelope(userID *int64) *model.ChannelManagementEnvelope {
	return &model.ChannelManagementEnvelope{
		Kind:    model.ChannelManagementKind,
		Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: "https://panel.example.com", AccessToken: "private-token", UserID: userID,
		},
	}
}

func newAPIHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func assertNewAPIOptionalInt64(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s = %d, want %d", name, *got, *want)
	}
}
