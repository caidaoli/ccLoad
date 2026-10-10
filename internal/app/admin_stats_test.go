package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestStatsHealthTimeline_UsesSecondsForAvgTimes(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "timeline-channel",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "claude-test"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	end := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	start := end.Add(-4 * time.Hour)
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:          model.JSONTime{Time: end.Add(-30 * time.Minute)},
		Model:         "claude-test",
		ActualModel:   "claude-test",
		ChannelID:     cfg.ID,
		LogSource:     model.LogSourceProxy,
		StatusCode:    200,
		Message:       "ok",
		Duration:      2.3,
		IsStreaming:   true,
		FirstByteTime: 1.5,
	}); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	query := fmt.Sprintf("/admin/stats?range=custom&start_time=%d&end_time=%d&health_timeline=1", start.UnixMilli(), end.UnixMilli())
	c, w := newTestContext(t, newRequest(http.MethodGet, query, nil))
	server.HandleStats(c)
	type statsResp struct {
		Stats []model.StatsEntry `json:"stats"`
	}
	stats := mustParseAPIResponse[statsResp](t, w.Body.Bytes()).Data.Stats
	if len(stats) != 1 {
		t.Fatalf("stats=%d entries, want 1: %s", len(stats), w.Body.String())
	}
	if len(stats[0].HealthTimeline) != 48 {
		t.Fatalf("期望 health timeline 长度=48，实际=%d", len(stats[0].HealthTimeline))
	}

	var found bool
	for _, point := range stats[0].HealthTimeline {
		if point.SuccessCount == 1 && point.ErrorCount == 0 {
			found = true
			if math.Abs(point.AvgFirstByteTime-1.5) > 1e-9 {
				t.Fatalf("AvgFirstByteTime 期望≈1.5(秒)，实际=%v", point.AvgFirstByteTime)
			}
			if math.Abs(point.AvgDuration-2.3) > 1e-9 {
				t.Fatalf("AvgDuration 期望≈2.3(秒)，实际=%v", point.AvgDuration)
			}
			break
		}
	}
	if !found {
		t.Fatalf("未找到包含写入日志的时间桶")
	}
}

// 当日开始不足4小时时，窗口仍须以当前时刻结尾：最近的请求落在最后一个桶，而不是让末尾桶落在未来。
func TestHealthTimelineToday_EndsAtNowWhenDayIsShorterThanWindow(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "timeline-early-day",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "claude-test"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: now.Add(-time.Minute)},
		Model:      "claude-test",
		ChannelID:  cfg.ID,
		LogSource:  model.LogSourceProxy,
		StatusCode: 200,
		Message:    "ok",
	}); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	lf := model.LogFilter{LogSource: model.LogSourceProxy}
	stats, err := store.GetStats(ctx, dayStart, now, &lf, true)
	if err != nil || len(stats) != 1 {
		t.Fatalf("GetStats: %v, entries=%d", err, len(stats))
	}
	params := healthTimelineParams(dayStart, now, &lf, true)
	rows, err := store.GetHealthTimeline(ctx, params)
	if err != nil {
		t.Fatalf("GetHealthTimeline: %v", err)
	}
	fillHealthTimeline(stats, params, rows)

	points := stats[0].HealthTimeline
	if len(points) != healthTimelineBuckets {
		t.Fatalf("health points=%d, want %d", len(points), healthTimelineBuckets)
	}
	if want := now.Add(-4 * time.Hour); !points[0].Ts.Equal(want) {
		t.Fatalf("first bucket ts=%v, want %v", points[0].Ts, want)
	}
	if last := points[len(points)-1]; last.SuccessCount != 1 {
		t.Fatalf("最近一分钟的请求应落在最后一个桶，实际 last=%+v", last)
	}
}

func TestHealthPointJSON_EmptyBucketOnlyCarriesTimestampAndRate(t *testing.T) {
	ts := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		point model.HealthPoint
		want  map[string]any
	}{
		{"no data", model.HealthPoint{Ts: ts, SuccessRate: -1}, map[string]any{"ts": "2026-10-06T00:00:00Z", "rate": -1.0}},
		// 全失败桶的 rate=0 必须保留，前端靠它区分“无数据”和“全部失败”
		{"all failed", model.HealthPoint{Ts: ts, SuccessRate: 0, ErrorCount: 2}, map[string]any{"ts": "2026-10-06T00:00:00Z", "rate": 0.0, "error": 2.0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.point)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("字段集合不符: got=%s", raw)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("%s: got=%v want=%v (%s)", k, got[k], v, raw)
				}
			}
		})
	}
}

// 健康时间线、逐渠道拆分和 RPM 都是额外查询/大体积字段，只有显式需要的页面才拿到。
func TestStatsEndpoints_HeavyFieldsAreOptIn(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "opt-in-channel",
		URLs:         model.ChannelURLs{{URL: "https://example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:           model.JSONTime{Time: time.Now()},
		ChannelID:      cfg.ID,
		Model:          "m1",
		ClientProtocol: "anthropic",
		LogSource:      model.LogSourceProxy,
		StatusCode:     http.StatusOK,
		Duration:       0.5,
	}); err != nil {
		t.Fatalf("AddLog: %v", err)
	}

	type statsResp struct {
		Stats []map[string]json.RawMessage `json:"stats"`
	}
	statsHas := func(query, field string) bool {
		t.Helper()
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/stats?range=today"+query, nil))
		server.HandleStats(c)
		stats := mustParseAPIResponse[statsResp](t, w.Body.Bytes()).Data.Stats
		if len(stats) != 1 {
			t.Fatalf("stats%s=%d entries, want 1: %s", query, len(stats), w.Body.String())
		}
		_, ok := stats[0][field]
		return ok
	}
	if statsHas("", "health_timeline") {
		t.Fatal("默认 stats 不应附带 health_timeline")
	}
	if !statsHas("&health_timeline=1", "health_timeline") {
		t.Fatal("health_timeline=1 时应附带 health_timeline")
	}
	// 统计结果有缓存，附带时间线的请求不能污染之后的默认请求
	if statsHas("", "health_timeline") {
		t.Fatal("health_timeline=1 之后的默认 stats 不应附带 health_timeline")
	}

	metricsHasChannels := func(query string) bool {
		t.Helper()
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/metrics?range=today&bucket_min=5"+query, nil))
		server.HandleMetrics(c)
		for _, point := range mustParseAPIResponse[[]map[string]json.RawMessage](t, w.Body.Bytes()).Data {
			if _, ok := point["channels"]; ok {
				return true
			}
		}
		return false
	}
	if metricsHasChannels("") {
		t.Fatal("默认 metrics 不应附带逐渠道 channels")
	}
	if !metricsHasChannels("&by_channel=1") {
		t.Fatal("by_channel=1 时应附带逐渠道 channels")
	}

	summaryFields := func(handler func(*gin.Context), identity *WebIdentity) map[string]json.RawMessage {
		t.Helper()
		c, w := newTestContext(t, newRequest(http.MethodGet, "/summary?range=today", nil))
		if identity != nil {
			c.Set(webIdentityContextKey, *identity)
		}
		handler(c)
		return mustParseAPIResponse[map[string]json.RawMessage](t, w.Body.Bytes()).Data
	}
	dashboard := summaryFields(server.HandleDashboardSummary, &WebIdentity{Role: model.WebRoleAdmin})
	if _, ok := dashboard["rpm_stats"]; ok {
		t.Fatalf("dashboard summary 不应计算 rpm_stats: %v", dashboard)
	}
	if string(dashboard["total_requests"]) != "1" {
		t.Fatalf("dashboard total_requests=%s, want 1", dashboard["total_requests"])
	}
	public := summaryFields(server.HandlePublicSummary, nil)
	for _, field := range []string{"rpm_stats", "duration_seconds", "is_today", "range"} {
		if _, ok := public[field]; !ok {
			t.Fatalf("public summary 缺少 %s: %v", field, public)
		}
	}
}
