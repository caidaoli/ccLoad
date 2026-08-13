package app

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// cpuUsageTracker 在两次运行状态查询之间计算 CPU 占用率。
// 只在 HandleRuntimeMetrics 被调用时采样,平时没有任何后台开销。
type cpuUsageTracker struct {
	mu          sync.Mutex
	sampled     bool
	lastCPU     float64 // 上次采样的累计 CPU 秒数(user+system)
	lastAt      time.Time
	lastPercent float64
}

// percent 返回 top 风格 CPU 占用率(多核可超过 100)。
// 首次调用返回自启动平均;两次调用间隔 <1s 时复用上次结果,避免窗口过短抖动。
func (t *cpuUsageTracker) percent(totalCPUSeconds float64, now time.Time, uptimeSeconds float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.sampled {
		t.sampled = true
		t.lastCPU = totalCPUSeconds
		t.lastAt = now
		if uptimeSeconds > 0 {
			t.lastPercent = totalCPUSeconds / uptimeSeconds * 100
		}
		return t.lastPercent
	}

	window := now.Sub(t.lastAt).Seconds()
	if window < 1 {
		return t.lastPercent
	}
	percent := (totalCPUSeconds - t.lastCPU) / window * 100
	if percent < 0 {
		percent = 0
	}
	t.lastCPU = totalCPUSeconds
	t.lastAt = now
	t.lastPercent = percent
	return percent
}

// parseStatmResidentBytes 解析 /proc/self/statm 的第二列(常驻页数)并换算为字节。
func parseStatmResidentBytes(statm string, pageSize int) uint64 {
	fields := strings.Fields(statm)
	if len(fields) < 2 || pageSize <= 0 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(pageSize)
}
