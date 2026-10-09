package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestStatsCache_SeparatesTodayRPM(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name: "rpm-cache", URLs: model.ChannelURLs{{URL: "https://example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "test-model"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := createTestToken(t, server, "rpm-cache")
	end := time.Now()
	start := end.Add(-time.Hour)
	if err := store.AddLog(ctx, &model.LogEntry{
		Time: model.JSONTime{Time: end}, ChannelID: cfg.ID, Model: "test-model",
		AuthTokenID: token.ID, LogSource: model.LogSourceProxy, StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		load func(*StatsCache, bool) (float64, error)
	}{
		{"stats", func(cache *StatsCache, today bool) (float64, error) {
			stats, err := cache.GetStats(ctx, start, end, nil, today)
			if err != nil {
				return 0, err
			}
			if len(stats) != 1 {
				return 0, fmt.Errorf("stats entries = %d, want 1", len(stats))
			}
			if stats[0].RecentRPM == nil {
				return 0, nil
			}
			return *stats[0].RecentRPM, nil
		}},
		{"rpm", func(cache *StatsCache, today bool) (float64, error) {
			stats, err := cache.GetRPMStats(ctx, start, end, nil, today)
			if err != nil {
				return 0, err
			}
			return stats.RecentRPM, nil
		}},
		{"auth_token_range", func(cache *StatsCache, today bool) (float64, error) {
			stats, err := cache.GetAuthTokenRangeStats(ctx, start, end, today)
			if err != nil {
				return 0, err
			}
			if stats[token.ID] == nil {
				return 0, fmt.Errorf("missing token stats")
			}
			return stats[token.ID].RecentRPM, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, firstToday := range []bool{false, true} {
				cache := NewStatsCache(store)
				t.Cleanup(cache.Close)
				// 两种请求顺序均验证，并再次读取两种口径的缓存结果。
				for _, today := range []bool{firstToday, !firstToday, firstToday, !firstToday} {
					got, err := tc.load(cache, today)
					if err != nil {
						t.Fatal(err)
					}
					want := 0.0
					if today {
						want = 1
					}
					if got != want {
						t.Fatalf("firstToday=%v today=%v: RecentRPM=%v, want %v", firstToday, today, got, want)
					}
				}
			}
		})
	}
}

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
	// 空 filter
	emptyFilter := &model.LogFilter{}
	hash1 := hashFilter(emptyFilter)

	// 带字段的 filter
	channelID := int64(123)
	filter := &model.LogFilter{
		ChannelID: &channelID,
		Model:     "gpt-4",
	}
	hash2 := hashFilter(filter)

	// 不同 filter 应产生不同 hash
	if hash1 == hash2 {
		t.Error("不同 filter 应该产生不同的 hash")
	}

	statusCode := 200
	filterWithName := &model.LogFilter{ChannelName: "primary"}
	filterWithStatus := &model.LogFilter{StatusCode: &statusCode}
	filterWithClientProtocol := &model.LogFilter{ClientProtocol: "openai"}
	filterWithSource := &model.LogFilter{LogSource: model.LogSourceManualTest}
	seen := map[string]bool{hash1: true}
	for _, h := range []string{hashFilter(filterWithName), hashFilter(filterWithStatus), hashFilter(filterWithClientProtocol), hashFilter(filterWithSource)} {
		if seen[h] {
			t.Fatalf("影响统计结果的 filter 字段未进入 hash: %s", h)
		}
		seen[h] = true
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

func TestStatsCache_BuildCacheKey_BucketsLiveEndTime(t *testing.T) {
	now := time.Now()
	startTime := beginningOfDay(now)
	endTime := now.Truncate(time.Minute).Add(5 * time.Second)

	key1 := buildCacheKey("stats", startTime, endTime, nil)
	key2 := buildCacheKey("stats", startTime, endTime.Add(10*time.Second), nil)

	if key1 != key2 {
		t.Fatalf("实时统计缓存键未按 TTL 分桶: %q != %q", key1, key2)
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

func TestStatsCache_CleanupExpired_ConcurrentDoesNotUnderflow(t *testing.T) {
	tmpDB := t.TempDir() + "/stats_cache_underflow_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	cache := NewStatsCache(store)
	defer cache.Close()

	cache.cache.Store("expired-key", &cachedStats{
		data:   []model.StatsEntry{},
		expiry: time.Now().Add(-1 * time.Hour),
	})
	cache.entryCount.Store(1)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cache.cleanupExpired()
		}()
	}
	close(start)
	wg.Wait()

	if got := cache.entryCount.Load(); got != 0 {
		t.Fatalf("entryCount 漂移: got %d, want 0", got)
	}
}
