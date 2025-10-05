package main

import (
	"testing"
	"time"
)

// BenchmarkCalculateBackoffDuration_AuthError 基准测试：401认证错误首次冷却
func BenchmarkCalculateBackoffDuration_AuthError(b *testing.B) {
	statusCode := 401
	now := time.Now()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateBackoffDuration(0, time.Time{}, now, &statusCode)
	}
}

// BenchmarkCalculateBackoffDuration_OtherError 基准测试：500服务器错误首次冷却
func BenchmarkCalculateBackoffDuration_OtherError(b *testing.B) {
	statusCode := 500
	now := time.Now()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateBackoffDuration(0, time.Time{}, now, &statusCode)
	}
}

// BenchmarkCalculateBackoffDuration_ExponentialBackoff 基准测试：指数退避计算
func BenchmarkCalculateBackoffDuration_ExponentialBackoff(b *testing.B) {
	statusCode := 401
	now := time.Now()
	prevMs := int64(5 * time.Minute / time.Millisecond)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateBackoffDuration(prevMs, time.Unix(0, 0), now, &statusCode)
	}
}

// BenchmarkCalculateBackoffDuration_NilStatusCode 基准测试：无状态码场景（网络错误）
func BenchmarkCalculateBackoffDuration_NilStatusCode(b *testing.B) {
	now := time.Now()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateBackoffDuration(0, time.Time{}, now, nil)
	}
}

// BenchmarkCalculateBackoffDuration_MaxLimit 基准测试：达到上限30分钟场景
func BenchmarkCalculateBackoffDuration_MaxLimit(b *testing.B) {
	statusCode := 401
	now := time.Now()
	prevMs := int64(20 * time.Minute / time.Millisecond) // 20分钟 * 2 = 40分钟（超过上限）

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateBackoffDuration(prevMs, time.Unix(0, 0), now, &statusCode)
	}
}

// BenchmarkScanUnixTimestamp 基准测试：Unix时间戳扫描
func BenchmarkScanUnixTimestamp(b *testing.B) {
	// Mock scanner
	scanner := &mockScanner{unixTime: time.Now().Unix()}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = scanUnixTimestamp(scanner)
	}
}

// mockScanner 用于基准测试的Mock扫描器
type mockScanner struct {
	unixTime int64
}

func (m *mockScanner) Scan(dest ...any) error {
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*int64); ok {
			*ptr = m.unixTime
		}
	}
	return nil
}

// BenchmarkToUnixTimestamp 基准测试：time.Time转Unix时间戳
func BenchmarkToUnixTimestamp(b *testing.B) {
	now := time.Now()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = toUnixTimestamp(now)
	}
}

// BenchmarkCalculateCooldownDuration 基准测试：计算冷却持续时间
func BenchmarkCalculateCooldownDuration(b *testing.B) {
	now := time.Now()
	until := now.Add(5 * time.Minute)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = calculateCooldownDuration(until, now)
	}
}
