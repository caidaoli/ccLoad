package app

import (
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestTimeHelpers(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC+8", 8*3600)

	// 找一个确定的周日，用来覆盖 beginningOfWeek/endOfWeek 的 Sunday 分支。
	var sunday time.Time
	for day := 1; day <= 31; day++ {
		candidate := time.Date(2026, 1, day, 12, 0, 0, 0, loc)
		if candidate.Weekday() == time.Sunday {
			sunday = candidate
			break
		}
	}
	if sunday.IsZero() {
		t.Fatal("failed to find a Sunday in Jan 2026 (should never happen)")
	}

	gotBeginWeek := beginningOfWeek(sunday)
	wantBeginWeek := beginningOfDay(sunday.AddDate(0, 0, -6)) // 周日视为7，回退到周一
	if !gotBeginWeek.Equal(wantBeginWeek) {
		t.Fatalf("beginningOfWeek(Sunday)=%v, want %v", gotBeginWeek, wantBeginWeek)
	}

	gotEndWeek := endOfWeek(sunday)
	wantEndWeek := endOfDay(sunday) // 周日本身
	if !gotEndWeek.Equal(wantEndWeek) {
		t.Fatalf("endOfWeek(Sunday)=%v, want %v", gotEndWeek, wantEndWeek)
	}

	// endOfDay/beginningOfMonth/endOfMonth：用闰年2月验证最后一天逻辑。
	feb := time.Date(2024, 2, 15, 1, 2, 3, 4, loc)
	if got := endOfDay(feb); got.Hour() != 23 || got.Minute() != 59 || got.Second() != 59 || got.Nanosecond() != 999999999 {
		t.Fatalf("endOfDay() unexpected: %v", got)
	}
	if got := beginningOfMonth(feb); !got.Equal(time.Date(2024, 2, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("beginningOfMonth()=%v, want 2024-02-01 00:00:00", got)
	}
	if got := endOfMonth(feb); !got.Equal(time.Date(2024, 2, 29, 23, 59, 59, 999999999, loc)) {
		t.Fatalf("endOfMonth()=%v, want 2024-02-29 23:59:59.999999999", got)
	}
}

func TestRespondErrorWithData(t *testing.T) {
	t.Parallel()

	c, w := newTestContext(t, newRequest(http.MethodGet, "/", nil))

	RespondErrorWithData(c, http.StatusBadRequest, "bad", map[string]any{"reason": "x"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}

	type data struct {
		Reason string `json:"reason"`
	}
	got := mustParseAPIResponse[data](t, w.Body.Bytes())
	if got.Success {
		t.Fatal("expected success=false")
	}
	if got.Error != "bad" {
		t.Fatalf("error=%q, want %q", got.Error, "bad")
	}
	if got.Data.Reason != "x" {
		t.Fatalf("data.reason=%v, want %v", got.Data.Reason, "x")
	}
}

func TestGetTimeRange_AllBranches(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC", 0)
	now := time.Date(2026, 1, 15, 12, 34, 56, 0, loc) // 固定时间，避免跨午夜/DST导致用例抖动

	cases := []struct {
		name  string
		rng   string
		check func(t *testing.T, start, end time.Time)
	}{
		{
			name: "today",
			rng:  "today",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-15 00:00:00", start)
				}
				if !end.Equal(now) {
					t.Fatalf("end=%v, want now=%v", end, now)
				}
			},
		},
		{
			name: "yesterday",
			rng:  "yesterday",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2026, 1, 14, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-14 00:00:00", start)
				}
				if !end.Equal(time.Date(2026, 1, 14, 23, 59, 59, 999999999, loc)) {
					t.Fatalf("end=%v, want 2026-01-14 23:59:59.999999999", end)
				}
			},
		},
		{
			name: "day_before_yesterday",
			rng:  "day_before_yesterday",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2026, 1, 13, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-13 00:00:00", start)
				}
				if !end.Equal(time.Date(2026, 1, 13, 23, 59, 59, 999999999, loc)) {
					t.Fatalf("end=%v, want 2026-01-13 23:59:59.999999999", end)
				}
			},
		},
		{
			name: "this_week",
			rng:  "this_week",
			check: func(t *testing.T, start, end time.Time) {
				// 2026-01-15 是周四；本周一为 2026-01-12
				if !start.Equal(time.Date(2026, 1, 12, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-12 00:00:00", start)
				}
				if !end.Equal(now) {
					t.Fatalf("end=%v, want now=%v", end, now)
				}
			},
		},
		{
			name: "last_week",
			rng:  "last_week",
			check: func(t *testing.T, start, end time.Time) {
				// 上周：2026-01-05(周一) ~ 2026-01-11(周日)
				if !start.Equal(time.Date(2026, 1, 5, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-05 00:00:00", start)
				}
				if !end.Equal(time.Date(2026, 1, 11, 23, 59, 59, 999999999, loc)) {
					t.Fatalf("end=%v, want 2026-01-11 23:59:59.999999999", end)
				}
			},
		},
		{
			name: "this_month",
			rng:  "this_month",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-01 00:00:00", start)
				}
				if !end.Equal(now) {
					t.Fatalf("end=%v, want now=%v", end, now)
				}
			},
		},
		{
			name: "last_month",
			rng:  "last_month",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2025, 12, 1, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2025-12-01 00:00:00", start)
				}
				if !end.Equal(time.Date(2025, 12, 31, 23, 59, 59, 999999999, loc)) {
					t.Fatalf("end=%v, want 2025-12-31 23:59:59.999999999", end)
				}
			},
		},
		{
			name: "unknown_defaults_to_today",
			rng:  "invalid_range",
			check: func(t *testing.T, start, end time.Time) {
				if !start.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, loc)) {
					t.Fatalf("start=%v, want 2026-01-15 00:00:00", start)
				}
				if !end.Equal(now) {
					t.Fatalf("end=%v, want now=%v", end, now)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &PaginationParams{Range: tc.rng}
			start, end := p.GetTimeRangeAt(now)
			tc.check(t, start, end)
		})
	}
}

func TestPaginationParams_SetDefaults(t *testing.T) {
	t.Parallel()

	p := &PaginationParams{}
	p.SetDefaults()

	if p.Range != "today" {
		t.Errorf("Range=%q, want %q", p.Range, "today")
	}
	if p.Limit != 200 {
		t.Errorf("Limit=%d, want 200", p.Limit)
	}

	// 已设置的值不应被覆盖
	p2 := &PaginationParams{Range: "yesterday", Limit: 50}
	p2.SetDefaults()
	if p2.Range != "yesterday" {
		t.Errorf("Range should not change, got %q", p2.Range)
	}
	if p2.Limit != 50 {
		t.Errorf("Limit should not change, got %d", p2.Limit)
	}
}

func TestBuildLogFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		check func(t *testing.T, lf model.LogFilter)
	}{
		{
			name:  "channel_id",
			query: "channel_id=123",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelID == nil || *lf.ChannelID != 123 {
					t.Error("expected ChannelID=123")
				}
			},
		},
		{
			name:  "channel_name",
			query: "channel_name=test-channel",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelName != "test-channel" {
					t.Errorf("ChannelName=%q, want %q", lf.ChannelName, "test-channel")
				}
			},
		},
		{
			name:  "channel_name_like",
			query: "channel_name_like=test",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelNameLike != "test" {
					t.Errorf("ChannelNameLike=%q, want %q", lf.ChannelNameLike, "test")
				}
			},
		},
		{
			name:  "model",
			query: "model=gpt-4o",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.Model != "gpt-4o" {
					t.Errorf("Model=%q, want %q", lf.Model, "gpt-4o")
				}
			},
		},
		{
			name:  "model_like",
			query: "model_like=gpt",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ModelLike != "gpt" {
					t.Errorf("ModelLike=%q, want %q", lf.ModelLike, "gpt")
				}
			},
		},
		{
			name:  "status_code",
			query: "status_code=200",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.StatusCode == nil || *lf.StatusCode != 200 {
					t.Error("expected StatusCode=200")
				}
			},
		},
		{
			name:  "channel_type",
			query: "channel_type=openai",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelType != "openai" {
					t.Errorf("ChannelType=%q, want %q", lf.ChannelType, "openai")
				}
			},
		},
		{
			name:  "auth_token_id",
			query: "auth_token_id=456",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.AuthTokenID == nil || *lf.AuthTokenID != 456 {
					t.Error("expected AuthTokenID=456")
				}
			},
		},
		{
			name:  "invalid_channel_id_ignored",
			query: "channel_id=invalid",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelID != nil {
					t.Error("expected nil ChannelID for invalid input")
				}
			},
		},
		{
			name:  "combined_filters",
			query: "channel_id=1&model=gpt-4&status_code=500",
			check: func(t *testing.T, lf model.LogFilter) {
				if lf.ChannelID == nil || *lf.ChannelID != 1 {
					t.Error("expected ChannelID=1")
				}
				if lf.Model != "gpt-4" {
					t.Errorf("Model=%q, want %q", lf.Model, "gpt-4")
				}
				if lf.StatusCode == nil || *lf.StatusCode != 500 {
					t.Error("expected StatusCode=500")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestContext(t, newRequest(http.MethodGet, "/test?"+tc.query, nil))

			lf := BuildLogFilter(c)
			tc.check(t, lf)
		})
	}
}
