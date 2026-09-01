package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestManagementCheckinDueUsesServerLocalDateAndTime(t *testing.T) {
	t.Parallel()
	// 时区由 now 自带，不改进程级 time.Local——那会和同包并行测试里的 time.Now() 打架。
	loc := time.FixedZone("server", 8*60*60)
	base := time.Date(2026, 8, 26, 9, 0, 0, 0, loc)
	newEnvelope := func(day string) *model.ChannelManagementEnvelope {
		return &model.ChannelManagementEnvelope{
			Profile:  model.ChannelManagementProfileNewAPI,
			Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"},
			State:    model.ChannelManagementState{LastScheduledDay: day},
		}
	}
	for name, tc := range map[string]struct {
		now  time.Time
		day  string
		want bool
	}{
		"before time":          {now: base.Add(-time.Minute), want: false},
		"at time":              {now: base, want: true},
		"same local day":       {now: base.Add(time.Hour), day: "2026-08-26", want: false},
		"previous day can run": {now: base, day: "2026-08-25", want: true},
		"local date matters":   {now: time.Date(2026, 8, 26, 1, 0, 0, 0, loc), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isManagementCheckinDue(newEnvelope(tc.day), tc.now); got != tc.want {
				t.Fatalf("isManagementCheckinDue()=%v, want %v at %s", got, tc.want, tc.now)
			}
		})
	}
}

func TestManagementCheckinDueSkipsUnsupportedOrDisabledProfiles(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.FixedZone("server", 8*60*60))
	cases := []model.ChannelManagementEnvelope{
		{Profile: model.ChannelManagementProfileSub2API, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "invalid"}},
	}
	for _, envelope := range cases {
		if isManagementCheckinDue(&envelope, now) {
			t.Fatalf("unsupported/disabled/invalid envelope was due: %#v", envelope)
		}
	}
}
func newDueNewAPIEnvelope(baseURL string) *model.ChannelManagementEnvelope {
	return &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: baseURL, AccessToken: "test-token",
			DailyCheckinEnabled: true, DailyCheckinTime: "00:00",
		},
	}
}

func TestManagementCheckinSchedulerExecutesAuditsAndDoesNotRetry(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case "/api/user/checkin":
			if r.Method == http.MethodPost {
				posts.Add(1)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"success":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	server.channelManagement.now = func() time.Time { return time.Now() }
	cfg := seedManagementEnvelope(t, server, "scheduler-no-retry", newDueNewAPIEnvelope(upstream.URL))

	err := server.runDueManagementCheckins(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST check-in count=%d, want 1", got)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin, ChannelID: &cfg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("check-in audit count=%d, want 1", len(logs))
	}
}

func TestManagementCheckinSchedulerExecutesInitiallyDisabledChannel(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-28"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-initially-disabled", newDueNewAPIEnvelope(upstream.URL))
	cfg.Enabled = false
	if _, err := server.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}

	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("disabled channel POST count=%d, want 1", got)
	}
}

func TestManagementCheckinSchedulerMaxFourWorkersAndSameChannelOnce(t *testing.T) {
	var active, maximum, posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		defer active.Add(-1)
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case "/api/user/checkin":
			if r.Method == http.MethodPost {
				posts.Add(1)
				_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-26"}}`))
			} else {
				_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
			}
		case "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	var cfgs []*model.Config
	for i := range 6 {
		cfgs = append(cfgs, seedManagementEnvelope(t, server, "scheduler-concurrent-"+string(rune('a'+i)), newDueNewAPIEnvelope(upstream.URL)))
	}
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := maximum.Load(); got > channelManagementScheduleWorkers {
		t.Fatalf("maximum concurrent upstream requests=%d, want <=%d", got, channelManagementScheduleWorkers)
	}
	if got := posts.Load(); got != int32(len(cfgs)) {
		t.Fatalf("POST count=%d, want %d", got, len(cfgs))
	}
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("second runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != int32(len(cfgs)) {
		t.Fatalf("same-day duplicate POST count=%d, want %d", got, len(cfgs))
	}
}

type schedulerStore struct {
	storage.Store
	mu             sync.Mutex
	conflictID     int64
	conflicted     bool
	claimErrorID   int64
	disableAfterID int64
	day            string
}

func (s *schedulerStore) CompareAndSwapChannelManagement(ctx context.Context, id int64, expected, next string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.claimErrorID {
		return false, errors.New("forced claim failure")
	}
	if id == s.conflictID && !s.conflicted {
		s.conflicted = true
		return false, nil
	}
	updated, err := s.Store.CompareAndSwapChannelManagement(ctx, id, expected, next)
	if err == nil && updated && id == s.disableAfterID {
		cfg, getErr := s.Store.GetConfig(ctx, id)
		if getErr == nil {
			cfg.Enabled = false
			_, err = s.UpdateConfig(ctx, id, cfg)
		}
	}
	return updated, err
}

func (s *schedulerStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	cfg, err := s.Store.GetConfig(ctx, id)
	if err != nil || cfg == nil || id != s.conflictID || s.day == "" {
		return cfg, err
	}
	envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if parseErr != nil {
		return cfg, nil
	}
	envelope.State.LastScheduledDay = s.day
	cfg.OAuthCredential, _ = envelope.Marshal()
	return cfg, nil
}

func TestManagementCheckinSchedulerClaimConflictSkipsUpstream(t *testing.T) {
	var requests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	}))
	server := newInMemoryServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-claim-conflict", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, conflictID: cfg.ID, day: time.Now().In(time.Local).Format("2006-01-02")}
	server.store = wrapped
	server.channelManagement.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("claim conflict made %d upstream requests, want 0", requests.Load())
	}
}

func TestManagementCheckinSchedulerClaimFailureKeepsEarlierClaims(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-26"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	first := seedManagementEnvelope(t, server, "scheduler-claim-first", newDueNewAPIEnvelope(upstream.URL))
	second := seedManagementEnvelope(t, server, "scheduler-claim-error", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, claimErrorID: second.ID}
	server.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err == nil {
		t.Fatal("claim failure returned nil, want scan error")
	}
	_ = first
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST count=%d, want 1 earlier claimed channel to execute", got)
	}
}

func TestManagementCheckinSchedulerRereadsDisabledAndExecutes(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-28"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-reread-disabled", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, disableAfterID: cfg.ID}
	server.store = wrapped
	server.channelManagement.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("disabled reread POST count=%d, want 1", got)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin, ChannelID: &cfg.ID})
	if err != nil || len(logs) != 1 || !strings.Contains(logs[0].Message, newAPICheckinSuccess) {
		t.Fatalf("disabled audit logs=%#v err=%v", logs, err)
	}
}

func TestManagementCheckinSchedulerCancellationSkipsFalseAuditAndLoopStops(t *testing.T) {
	started := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
	}))
	server := newInMemoryServer(t)
	seedManagementEnvelope(t, server, "scheduler-cancel", newDueNewAPIEnvelope(upstream.URL))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.runDueManagementCheckins(ctx, time.Now()) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scheduler error=%v, want context canceled", err)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("canceled scheduler wrote %d audit logs, want 0", len(logs))
	}

	loop := &Server{baseCtx: context.Background(), shutdownCh: make(chan struct{})}
	loop.wg.Add(1)
	loopDone := make(chan struct{})
	go func() { loop.managementCheckinLoop(); close(loopDone) }()
	close(loop.shutdownCh)
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("managementCheckinLoop did not stop on shutdown")
	}
}
