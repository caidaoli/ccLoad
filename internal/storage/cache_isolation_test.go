package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// TestCacheIsolation_GetEnabledChannelsByModel 验证 GetEnabledChannelsByModel 返回深拷贝
// [FIX] P0-2: 防止调用方修改污染缓存
func TestCacheIsolation_GetEnabledChannelsByModel(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "test-channel",
		URL:      "https://test.example.com",
		Priority: 10,
		// 验证缓存深拷贝不会丢字段：DailyCostLimit 需被正确保留，否则成本限额过滤会失效。
		DailyCostLimit: 2.0,
		ModelEntries: []model.ModelEntry{
			{Model: "model-1", RedirectModel: ""},
			{Model: "model-2", RedirectModel: ""},
			{Model: "alias-1", RedirectModel: "model-1"},
		},
		Enabled: true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	// 第一次查询，填充缓存
	channels1, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByModel 失败: %v", err)
	}
	if len(channels1) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels1))
	}

	// 验证深拷贝：修改返回的数据
	originalEntriesLen := len(channels1[0].ModelEntries)

	// 污染尝试1：修改 ModelEntries slice
	channels1[0].ModelEntries = append(channels1[0].ModelEntries, model.ModelEntry{Model: "backdoor-model"})
	if len(channels1[0].ModelEntries) > 0 {
		channels1[0].ModelEntries[0].Model = "POLLUTED"
	}

	// 污染尝试2：修改其他字段
	channels1[0].Name = "POLLUTED_NAME"
	channels1[0].Priority = 9999

	// 第二次查询，验证缓存未被污染
	channels2, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
	if err != nil {
		t.Fatalf("第二次 GetEnabledChannelsByModel 失败: %v", err)
	}
	if len(channels2) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels2))
	}

	ch2 := channels2[0]

	// 验证：ModelEntries slice 未被污染
	if len(ch2.ModelEntries) != originalEntriesLen {
		t.Errorf("ModelEntries slice 长度被污染: 期望 %d, 实际 %d", originalEntriesLen, len(ch2.ModelEntries))
	}
	// 验证是否包含原始模型（顺序无关）
	foundModel1 := false
	for _, e := range ch2.ModelEntries {
		if e.Model == "model-1" {
			foundModel1 = true
		}
		if e.Model == "backdoor-model" || e.Model == "POLLUTED" {
			t.Errorf("ModelEntries slice 包含污染数据: %q", e.Model)
		}
	}
	if !foundModel1 {
		t.Errorf("ModelEntries 不包含 'model-1'")
	}

	// 验证：其他字段未被污染
	if ch2.Name != "test-channel" {
		t.Errorf("Name 被污染: 期望 'test-channel', 实际 %q", ch2.Name)
	}
	if ch2.Priority != 10 {
		t.Errorf("Priority 被污染: 期望 10, 实际 %d", ch2.Priority)
	}
	if ch2.DailyCostLimit != 2.0 {
		t.Errorf("DailyCostLimit 丢失/被污染: 期望 2.0, 实际 %v", ch2.DailyCostLimit)
	}

	// 验证：数据库中的数据未被污染
	dbCfg, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig 失败: %v", err)
	}
	if len(dbCfg.ModelEntries) != originalEntriesLen {
		t.Errorf("数据库中 ModelEntries 被污染: 期望 %d, 实际 %d", originalEntriesLen, len(dbCfg.ModelEntries))
	}
	// 验证数据库中是否包含原始模型（顺序无关）
	foundModel1InDB := false
	for _, e := range dbCfg.ModelEntries {
		if e.Model == "model-1" {
			foundModel1InDB = true
			break
		}
	}
	if !foundModel1InDB {
		t.Errorf("数据库中 ModelEntries 不包含 'model-1'")
	}

	t.Logf("✅ 深拷贝隔离性测试通过：调用方修改未污染缓存或数据库")
}

// TestCacheIsolation_GetEnabledChannelsByType 验证 GetEnabledChannelsByType 返回深拷贝
func TestCacheIsolation_GetEnabledChannelsByType(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation_type.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 创建测试渠道
	cfg := &model.Config{
		Name:           "test-anthropic",
		ChannelType:    "anthropic",
		URL:            "https://test.example.com",
		Priority:       10,
		DailyCostLimit: 2.0,
		ModelEntries: []model.ModelEntry{
			{Model: "claude-3-sonnet", RedirectModel: ""},
			{Model: "claude", RedirectModel: "claude-3-sonnet"},
		},
		Enabled: true,
	}
	_, err = store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	// 第一次查询，填充缓存
	channels1, err := cache.GetEnabledChannelsByType(ctx, "anthropic")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByType 失败: %v", err)
	}
	if len(channels1) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels1))
	}

	// 污染尝试：修改返回的数据
	channels1[0].ModelEntries = append(channels1[0].ModelEntries, model.ModelEntry{Model: "backdoor-model"})

	// 第二次查询，验证缓存未被污染
	channels2, err := cache.GetEnabledChannelsByType(ctx, "anthropic")
	if err != nil {
		t.Fatalf("第二次 GetEnabledChannelsByType 失败: %v", err)
	}
	if len(channels2) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels2))
	}

	ch2 := channels2[0]

	// 验证：未被污染（顺序无关）
	if len(ch2.ModelEntries) != 2 {
		t.Errorf("ModelEntries 长度被污染: 期望 2, 实际 %d", len(ch2.ModelEntries))
	}
	if ch2.DailyCostLimit != 2.0 {
		t.Errorf("DailyCostLimit 丢失/被污染: 期望 2.0, 实际 %v", ch2.DailyCostLimit)
	}
	// 验证包含原始模型
	foundClaude3Sonnet := false
	foundClaude := false
	for _, e := range ch2.ModelEntries {
		if e.Model == "claude-3-sonnet" {
			foundClaude3Sonnet = true
		}
		if e.Model == "claude" {
			foundClaude = true
		}
		if e.Model == "backdoor-model" {
			t.Errorf("ModelEntries 包含污染数据: 'backdoor-model'")
		}
	}
	if !foundClaude3Sonnet || !foundClaude {
		t.Errorf("ModelEntries 缺少原始模型: foundClaude3Sonnet=%v, foundClaude=%v", foundClaude3Sonnet, foundClaude)
	}

	t.Logf("✅ GetEnabledChannelsByType 深拷贝隔离性测试通过")
}

// TestCacheIsolation_MultipleQueries 验证多次查询的隔离性
func TestCacheIsolation_MultipleQueries(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation_multi.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "multi-query-test",
		URL:      "https://test.example.com",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "model-1", RedirectModel: ""},
			{Model: "model-2", RedirectModel: ""},
		},
		Enabled: true,
	}
	_, err = store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	// 并发查询和修改
	for i := 0; i < 10; i++ {
		channels, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
		if err != nil {
			t.Fatalf("查询 %d 失败: %v", i, err)
		}
		if len(channels) != 1 {
			t.Fatalf("查询 %d: 期望1个渠道，实际 %d 个", i, len(channels))
		}

		// 每次都尝试污染
		channels[0].ModelEntries = append(channels[0].ModelEntries, model.ModelEntry{Model: "backdoor"})
	}

	// 最终验证：缓存应该保持干净
	channels, err := cache.GetEnabledChannelsByModel(ctx, "model-1")
	if err != nil {
		t.Fatalf("最终查询失败: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("期望1个渠道，实际 %d 个", len(channels))
	}

	ch := channels[0]
	if len(ch.ModelEntries) != 2 {
		t.Errorf("ModelEntries 长度被污染: 期望 2, 实际 %d", len(ch.ModelEntries))
	}
	if ch.ModelEntries[0].Model != "model-1" || ch.ModelEntries[1].Model != "model-2" {
		t.Errorf("ModelEntries 内容被污染: %v", ch.ModelEntries)
	}

	t.Logf("✅ 多次查询隔离性测试通过：10次污染尝试均被隔离")
}

// TestCacheIsolation_WildcardQuery 验证通配符查询的深拷贝
func TestCacheIsolation_WildcardQuery(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation_wildcard.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 创建多个测试渠道
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     "wildcard-test-" + string(rune('A'+i-1)),
			URL:      "https://test.example.com",
			Priority: i * 10,
			ModelEntries: []model.ModelEntry{
				{Model: "model-common", RedirectModel: ""},
			},
			Enabled: true,
		}
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建渠道 %d 失败: %v", i, err)
		}
	}

	cache := storage.NewChannelCache(store, 1*time.Minute)

	// 通配符查询
	channels1, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("通配符查询失败: %v", err)
	}
	if len(channels1) != 3 {
		t.Fatalf("期望3个渠道，实际 %d 个", len(channels1))
	}

	// 污染所有返回的渠道
	for i := range channels1 {
		channels1[i].ModelEntries = append(channels1[i].ModelEntries, model.ModelEntry{Model: "POLLUTED"})
		channels1[i].Name = "POLLUTED"
	}

	// 第二次查询
	channels2, err := cache.GetEnabledChannelsByModel(ctx, "*")
	if err != nil {
		t.Fatalf("第二次通配符查询失败: %v", err)
	}
	if len(channels2) != 3 {
		t.Fatalf("期望3个渠道，实际 %d 个", len(channels2))
	}

	// 验证：所有渠道都未被污染
	for i, ch := range channels2 {
		if len(ch.ModelEntries) != 1 || ch.ModelEntries[0].Model != "model-common" {
			t.Errorf("渠道 %d ModelEntries 被污染: %v", i, ch.ModelEntries)
		}
		if ch.Name == "POLLUTED" {
			t.Errorf("渠道 %d Name 被污染", i)
		}
	}

	t.Logf("✅ 通配符查询深拷贝隔离性测试通过")
}
