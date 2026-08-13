package app

import (
	"testing"
	"time"
)

func TestCPUUsageTrackerPercent(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	tracker := &cpuUsageTracker{}

	// 首次调用:自启动平均(30 秒 CPU / 120 秒运行 = 25%)
	if got := tracker.percent(30, base, 120); got != 25 {
		t.Fatalf("first call percent=%v, want 25", got)
	}

	// 窗口不足 1s:复用上次结果,不更新采样点
	if got := tracker.percent(31, base.Add(500*time.Millisecond), 120.5); got != 25 {
		t.Fatalf("short window percent=%v, want 25", got)
	}

	// 正常窗口:10 秒内多用 5 秒 CPU = 50%
	if got := tracker.percent(35, base.Add(10*time.Second), 130); got != 50 {
		t.Fatalf("delta percent=%v, want 50", got)
	}

	// CPU 时间回退(理论不可能,防御性):钳为 0
	if got := tracker.percent(30, base.Add(20*time.Second), 140); got != 0 {
		t.Fatalf("negative delta percent=%v, want 0", got)
	}
}

func TestCPUUsageTrackerFirstCallZeroUptime(t *testing.T) {
	t.Parallel()

	tracker := &cpuUsageTracker{}
	if got := tracker.percent(5, time.Unix(1_700_000_000, 0), 0); got != 0 {
		t.Fatalf("zero uptime percent=%v, want 0", got)
	}
}

func TestParseStatmResidentBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		statm    string
		pageSize int
		want     uint64
	}{
		{"normal", "12345 678 90 1 0 234 0\n", 4096, 678 * 4096},
		{"missing fields", "12345", 4096, 0},
		{"garbage", "abc def", 4096, 0},
		{"zero page size", "12345 678", 0, 0},
		{"empty", "", 4096, 0},
	}
	for _, tc := range cases {
		if got := parseStatmResidentBytes(tc.statm, tc.pageSize); got != tc.want {
			t.Errorf("%s: parseStatmResidentBytes=%d, want %d", tc.name, got, tc.want)
		}
	}
}
