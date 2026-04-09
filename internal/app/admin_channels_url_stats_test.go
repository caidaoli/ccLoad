package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestHandleChannelURLStats_NilSelectorReturnsEmpty(t *testing.T) {
	srv := newInMemoryServer(t)

	cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name:         "url-stats-nil-selector",
		URL:          "https://a.example\nhttps://b.example",
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-20250514"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	srv.urlSelector = nil

	target := fmt.Sprintf("/admin/channels/%d/url-stats", cfg.ID)
	c, w := newTestContext(t, newRequest(http.MethodGet, target, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}

	srv.HandleChannelURLStats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[[]URLStat](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("expected success=true, resp=%+v", resp)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("expected empty stats when selector is nil, got %+v", resp.Data)
	}
}

func TestNewServer_LoadsTodayURLStatsFromLogsOnStartup(t *testing.T) {
	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}

	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "url-stats-from-logs",
		URL:          "https://a.example\nhttps://b.example",
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-20250514"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	now := time.Now()
	startOfToday := todayStart(now)
	entries := []*model.LogEntry{
		{
			Time:          model.JSONTime{Time: startOfToday.Add(2 * time.Hour)},
			Model:         "claude-sonnet-4-20250514",
			ChannelID:     cfg.ID,
			StatusCode:    http.StatusOK,
			Message:       "ok-a",
			FirstByteTime: 0.25,
			Duration:      1.2,
			BaseURL:       "https://a.example",
		},
		{
			Time:       model.JSONTime{Time: startOfToday.Add(3 * time.Hour)},
			Model:      "claude-sonnet-4-20250514",
			ChannelID:  cfg.ID,
			StatusCode: http.StatusBadGateway,
			Message:    "fail-b",
			Duration:   0.9,
			BaseURL:    "https://b.example",
		},
		{
			Time:          model.JSONTime{Time: startOfToday.Add(-time.Minute)},
			Model:         "claude-sonnet-4-20250514",
			ChannelID:     cfg.ID,
			StatusCode:    http.StatusOK,
			Message:       "yesterday-ignored",
			FirstByteTime: 0.4,
			Duration:      0.8,
			BaseURL:       "https://a.example",
		},
	}
	for _, entry := range entries {
		if err := store.AddLog(context.Background(), entry); err != nil {
			t.Fatalf("AddLog failed: %v", err)
		}
	}

	srv := NewServer(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Server.Shutdown failed: %v", err)
		}
	})

	stats := srv.urlSelector.GetURLStats(cfg.ID, cfg.GetURLs())
	if len(stats) != 2 {
		t.Fatalf("expected 2 url stats, got %+v", stats)
	}

	statsByURL := make(map[string]URLStat, len(stats))
	for _, stat := range stats {
		statsByURL[stat.URL] = stat
	}

	statA, ok := statsByURL["https://a.example"]
	if !ok {
		t.Fatalf("missing stat for https://a.example")
	}
	if statA.Requests != 1 || statA.Failures != 0 {
		t.Fatalf("expected startup to load today's success count for a.example, got %+v", statA)
	}
	if statA.LatencyMs <= 0 {
		t.Fatalf("expected startup to load today's latency for a.example, got %+v", statA)
	}

	statB, ok := statsByURL["https://b.example"]
	if !ok {
		t.Fatalf("missing stat for https://b.example")
	}
	if statB.Requests != 0 || statB.Failures != 1 {
		t.Fatalf("expected startup to load today's failure count for b.example, got %+v", statB)
	}
	if statB.LatencyMs >= 0 {
		t.Fatalf("expected failed-only url to stay unknown latency, got %+v", statB)
	}
}
