package app

import (
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestStatsCache_CalculateTTL(t *testing.T) {
	tests := []struct {
		name    string
		endTime time.Time
		wantTTL time.Duration
	}{
		{
			name:    "最近1小时内",
			endTime: time.Now().Add(-30 * time.Minute),
			wantTTL: 30 * time.Second,
		},
		{
			name:    "今天（1-24小时前）",
			endTime: time.Now().Add(-12 * time.Hour),
			wantTTL: 5 * time.Minute,
		},
		{
			name:    "最近7天（1-7天前）",
			endTime: time.Now().Add(-3 * 24 * time.Hour),
			wantTTL: 30 * time.Minute,
		},
		{
			name:    "历史数据（7天以上）",
			endTime: time.Now().Add(-10 * 24 * time.Hour),
			wantTTL: 2 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateTTL(tt.endTime)
			if got != tt.wantTTL {
				t.Errorf("calculateTTL() = %v, want %v", got, tt.wantTTL)
			}
		})
	}
}

func TestStatsCache_HashFilter(t *testing.T) {
	// nil filter
	if got := hashFilter(nil); got != "nil" {
		t.Errorf("hashFilter(nil) = %s, want nil", got)
	}

	// 空 filter
	emptyFilter := &model.LogFilter{}
	hash1 := hashFilter(emptyFilter)
	if len(hash1) != 16 {
		t.Errorf("hashFilter 返回长度应为 16, got %d", len(hash1))
	}

	// 带字段的 filter
	channelID := int64(123)
	filter := &model.LogFilter{
		ChannelID:   &channelID,
		ChannelType: "openai",
		Model:       "gpt-4",
	}
	hash2 := hashFilter(filter)
	if len(hash2) != 16 {
		t.Errorf("hashFilter 返回长度应为 16, got %d", len(hash2))
	}

	// 不同 filter 应产生不同 hash
	if hash1 == hash2 {
		t.Error("不同 filter 应该产生不同的 hash")
	}
}

func TestStatsCache_BuildCacheKey(t *testing.T) {
	startTime := time.Unix(1000000, 0)
	endTime := time.Unix(2000000, 0)

	key1 := buildCacheKey("stats", startTime, endTime, nil)
	key2 := buildCacheKey("rpm", startTime, endTime, nil)

	// 不同类型应产生不同 key
	if key1 == key2 {
		t.Error("不同类型应产生不同的 key")
	}

	// 相同参数应产生相同 key
	key3 := buildCacheKey("stats", startTime, endTime, nil)
	if key1 != key3 {
		t.Error("相同参数应产生相同的 key")
	}
}

func TestStatsCache_CleanupExpired(t *testing.T) {
	tmpDB := t.TempDir() + "/stats_cache_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	cache := NewStatsCache(store)
	defer cache.Close()

	// 手动插入一个过期条目
	expiredEntry := &cachedStats{
		data:   []model.StatsEntry{},
		expiry: time.Now().Add(-1 * time.Hour), // 已过期
	}
	cache.cache.Store("expired-key", expiredEntry)

	// 插入一个未过期条目
	validEntry := &cachedStats{
		data:   []model.StatsEntry{},
		expiry: time.Now().Add(1 * time.Hour), // 未过期
	}
	cache.cache.Store("valid-key", validEntry)

	// 执行清理
	cache.cleanupExpired()

	// 验证过期条目被删除
	if _, ok := cache.cache.Load("expired-key"); ok {
		t.Error("过期条目应该被清理")
	}

	// 验证未过期条目仍存在
	if _, ok := cache.cache.Load("valid-key"); !ok {
		t.Error("未过期条目不应该被清理")
	}
}
