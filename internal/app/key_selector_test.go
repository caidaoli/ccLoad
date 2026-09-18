package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

// testContextKey 用于测试的 context key 类型
type testContextKey string

const testingContextKey testContextKey = "testing"

// TestSelectAvailableKey_SingleKey 测试单Key场景
func TestSelectAvailableKey_SingleKey(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "single-key-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建单个API Key
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-key",
		KeyStrategy: model.KeyStrategySequential,
	}})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("首次选择", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 0 {
			t.Errorf("期望keyIndex=0，实际%d", keyIndex)
		}

		if apiKey != "sk-single-key" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-single-key，实际%s", apiKey)
		}
	})

	t.Run("排除唯一Key后无可用Key", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true}
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err == nil {
			t.Error("期望返回错误（唯一Key已被排除），但成功返回")
		}
	})
}

// TestSelectAvailableKey_SingleKeyCooldown 测试单Key冷却场景（修复Bug验证）
func TestSelectAvailableKey_SingleKeyCooldown(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector()
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "single-key-cooldown-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建单个API Key
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-single-cooldown-key",
		KeyStrategy: model.KeyStrategySequential,
	}})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 冷却这个唯一的Key
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 0, now, 401)
	if err != nil {
		t.Fatalf("冷却Key失败: %v", err)
	}

	// 预先查询apiKeys（在冷却之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("单Key冷却后应返回错误", func(t *testing.T) {
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err == nil {
			t.Error("期望返回错误（单Key在冷却中），但成功返回")
		}

		// 验证错误消息包含冷却信息
		if !strings.Contains(err.Error(), "cooldown") {
			t.Errorf("错误消息应包含'cooldown'，实际: %v", err)
		}
	})
}

// TestSelectAvailableKey_DescendingPriorities 验证递减优先级保留旧顺序
func TestSelectAvailableKey_DescendingPriorities(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "sequential-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys（顺序策略）
	seqKeys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		seqKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-seq-key-" + string(rune('0'+i)),
			Priority:    2 - i,
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, seqKeys); err != nil {
		t.Fatalf("批量创建API Keys失败: %v", err)
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("首次选择返回第一个Key", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 0 {
			t.Errorf("最高优先级应返回keyIndex=0，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-0" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-seq-key-0，实际%s", apiKey)
		}
	})

	t.Run("排除第一个Key后返回第二个", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true}
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 1 {
			t.Errorf("排除Key0后应返回keyIndex=1，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-1" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-seq-key-1，实际%s", apiKey)
		}
	})

	t.Run("排除前两个Key后返回第三个", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true, 1: true}
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 2 {
			t.Errorf("排除Key0和Key1后应返回keyIndex=2，实际%d", keyIndex)
		}

		if apiKey != "sk-seq-key-2" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-seq-key-2，实际%s", apiKey)
		}
	})

	t.Run("所有Key被排除后返回错误", func(t *testing.T) {
		excludeKeys := map[int]bool{0: true, 1: true, 2: true}
		_, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err == nil {
			t.Error("期望返回错误（所有Key已被排除），但成功返回")
		}
	})
}

// TestSelectAvailableKey_RoundRobin 测试轮询策略
func TestSelectAvailableKey_RoundRobin(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "roundrobin-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys（轮询策略）
	rrKeys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		rrKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-rr-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategyRoundRobin,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, rrKeys); err != nil {
		t.Fatalf("批量创建API Keys失败: %v", err)
	}

	// 预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("连续调用应轮询返回不同Key", func(t *testing.T) {
		// [INFO] Linus风格：轮询指针内存化后，起始位置不确定（每次测试可能不同）
		// 验证策略：确保5次调用真正轮询（没有连续重复，且访问了所有Key）

		var selectedKeys []int
		keysSeen := make(map[int]bool)

		for i := 0; i < 5; i++ {
			keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
			if err != nil {
				t.Fatalf("第%d次SelectAvailableKey失败: %v", i+1, err)
			}
			selectedKeys = append(selectedKeys, keyIndex)
			keysSeen[keyIndex] = true
		}

		// 验证1：5次调用应访问所有3个Key
		if len(keysSeen) != 3 {
			t.Errorf("轮询失败: 只访问了%d个Key，期望3个。序列: %v", len(keysSeen), selectedKeys)
		}

		// 验证2：没有连续两次选择同一个Key（真正轮询）
		for i := 1; i < len(selectedKeys); i++ {
			if selectedKeys[i] == selectedKeys[i-1] {
				t.Errorf("轮询失败: 连续选择了相同Key=%d", selectedKeys[i])
			}
		}
	})

	t.Run("排除当前Key后跳到下一个", func(t *testing.T) {
		// [INFO] 内存化后无需重置索引

		// 第一次排除Key0
		excludeKeys := map[int]bool{0: true}
		keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		if keyIndex != 1 {
			t.Errorf("排除Key0后应返回keyIndex=1，实际%d", keyIndex)
		}
	})
}

func TestSelectAvailableKey_RoundRobinIsolatedByCandidatePool(t *testing.T) {
	t.Parallel()

	selector := NewKeySelector()
	firstPool := []*model.APIKey{
		{KeyIndex: 0, APIKey: "sk-a-0", KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 1, APIKey: "sk-a-1", KeyStrategy: model.KeyStrategyRoundRobin},
	}
	secondPool := []*model.APIKey{
		{KeyIndex: 2, APIKey: "sk-b-0", KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 3, APIKey: "sk-b-1", KeyStrategy: model.KeyStrategyRoundRobin},
	}
	seenFirst := make(map[int]bool)
	seenSecond := make(map[int]bool)
	for range 2 {
		firstIndex, _, err := selector.SelectAvailableKey(123, firstPool, nil)
		if err != nil {
			t.Fatalf("select first pool: %v", err)
		}
		secondIndex, _, err := selector.SelectAvailableKey(123, secondPool, nil)
		if err != nil {
			t.Fatalf("select second pool: %v", err)
		}
		seenFirst[firstIndex] = true
		seenSecond[secondIndex] = true
	}
	if len(seenFirst) != 2 || len(seenSecond) != 2 {
		t.Fatalf("candidate pools did not rotate independently: first=%v second=%v", seenFirst, seenSecond)
	}
}

// TestSelectAvailableKey_RoundRobin_NonContiguousKeyIndex 验证RR不依赖KeyIndex连续性
// [REGRESSION] 这个测试防止回归到"假设KeyIndex=0..N-1连续"的错误实现
func TestSelectAvailableKey_RoundRobin_NonContiguousKeyIndex(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector()
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "rr-noncontig-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建非连续KeyIndex的Keys（模拟删除Key后留洞的场景）
	// 故意留洞: 0, 2, 5 (缺少1, 3, 4)
	nonContiguousIndexes := []int{0, 2, 5}
	nonContigKeys := make([]*model.APIKey, len(nonContiguousIndexes))
	for i, idx := range nonContiguousIndexes {
		nonContigKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    idx,
			APIKey:      "sk-noncontig-" + string(rune('0'+idx)),
			KeyStrategy: model.KeyStrategyRoundRobin,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, nonContigKeys); err != nil {
		t.Fatalf("批量创建非连续API Keys失败: %v", err)
	}

	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("非连续KeyIndex仍能轮询所有Key", func(t *testing.T) {
		keysSeen := make(map[int]bool)

		// 轮询6次，每个Key应至少被选中2次
		for i := 0; i < 6; i++ {
			keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)
			if err != nil {
				t.Fatalf("第%d次SelectAvailableKey失败: %v", i+1, err)
			}
			keysSeen[keyIndex] = true
		}

		// 验证所有3个非连续KeyIndex都被访问到
		if len(keysSeen) != len(nonContiguousIndexes) {
			t.Errorf("轮询失败: 只访问了%d个Key，期望%d个。seen=%v",
				len(keysSeen), len(nonContiguousIndexes), keysSeen)
		}

		for _, expectedIdx := range nonContiguousIndexes {
			if !keysSeen[expectedIdx] {
				t.Errorf("轮询未覆盖KeyIndex=%d，seen=%v", expectedIdx, keysSeen)
			}
		}
	})

	t.Run("排除非连续KeyIndex中的特定Key", func(t *testing.T) {
		// 排除KeyIndex=2（中间的那个）
		excludeKeys := map[int]bool{2: true}

		keysSeen := make(map[int]bool)
		for i := 0; i < 4; i++ {
			keyIndex, _, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)
			if err != nil {
				t.Fatalf("第%d次SelectAvailableKey失败: %v", i+1, err)
			}
			keysSeen[keyIndex] = true
		}

		// 验证只访问了KeyIndex 0和5，没有访问被排除的2
		if keysSeen[2] {
			t.Errorf("被排除的KeyIndex=2不应被选中")
		}
		if !keysSeen[0] || !keysSeen[5] {
			t.Errorf("未被排除的KeyIndex 0和5应被选中，seen=%v", keysSeen)
		}
	})
}

// TestSelectAvailableKey_SingleKey_NonZeroKeyIndex 验证单Key场景下KeyIndex≠0时排除逻辑正确
// [REGRESSION] 防止回归到"excludeKeys[0]"硬编码的错误实现
func TestSelectAvailableKey_SingleKey_NonZeroKeyIndex(t *testing.T) {
	selector := NewKeySelector()

	// 模拟单Key但KeyIndex=5的场景（如删除其他Key后只剩一个）
	apiKeys := []*model.APIKey{
		{
			ChannelID:   1,
			KeyIndex:    5, // 非0的KeyIndex
			APIKey:      "sk-single-nonzero",
			KeyStrategy: model.KeyStrategySequential,
		},
	}

	t.Run("单Key非零KeyIndex正常选择", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(1, apiKeys, nil)
		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}
		if keyIndex != 5 {
			t.Errorf("期望keyIndex=5，实际%d", keyIndex)
		}
		if apiKey != "sk-single-nonzero" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-single-nonzero，实际%s", apiKey)
		}
	})

	t.Run("单Key非零KeyIndex排除正确", func(t *testing.T) {
		// 排除真实的KeyIndex=5，而非硬编码的0
		excludeKeys := map[int]bool{5: true}
		_, _, err := selector.SelectAvailableKey(1, apiKeys, excludeKeys)
		if err == nil {
			t.Errorf("排除唯一Key后应返回错误")
		}
		// 验证错误信息包含正确的KeyIndex
		if !strings.Contains(err.Error(), "index=5") {
			t.Errorf("错误信息应包含正确的KeyIndex=5: %v", err)
		}
	})

	t.Run("排除错误的KeyIndex不影响选择", func(t *testing.T) {
		// 排除KeyIndex=0（不存在），应该不影响真实KeyIndex=5的选择
		excludeKeys := map[int]bool{0: true}
		keyIndex, _, err := selector.SelectAvailableKey(1, apiKeys, excludeKeys)
		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}
		if keyIndex != 5 {
			t.Errorf("排除不存在的KeyIndex=0不应影响KeyIndex=5的选择，实际%d", keyIndex)
		}
	})
}

// TestSelectAvailableKey_KeyCooldown 测试Key冷却过滤
func TestSelectAvailableKey_KeyCooldown(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "cooldown-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建3个API Keys
	cdKeys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		cdKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-cooldown-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, cdKeys); err != nil {
		t.Fatalf("批量创建API Keys失败: %v", err)
	}

	// 冷却Key0
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 0, now, 401)
	if err != nil {
		t.Fatalf("冷却Key0失败: %v", err)
	}

	// 预先查询apiKeys（在冷却Key0之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	t.Run("冷却的Key被跳过", func(t *testing.T) {
		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		// 应该跳过冷却的Key0，返回Key1
		if keyIndex != 1 {
			t.Errorf("期望跳过冷却的Key0返回keyIndex=1，实际%d", keyIndex)
		}

		if apiKey != "sk-cooldown-key-1" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-cooldown-key-1，实际%s", apiKey)
		}
	})

	t.Run("冷却多个Key", func(t *testing.T) {
		// 再冷却Key1
		_, err = store.BumpKeyCooldown(ctx, cfg.ID, 1, now, 401)
		if err != nil {
			t.Fatalf("冷却Key1失败: %v", err)
		}

		// 重新查询apiKeys以获取最新冷却状态
		apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err != nil {
			t.Fatalf("SelectAvailableKey失败: %v", err)
		}

		// 应该跳过冷却的Key0和Key1，返回Key2
		if keyIndex != 2 {
			t.Errorf("期望跳过Key0和Key1返回keyIndex=2，实际%d", keyIndex)
		}

		if apiKey != "sk-cooldown-key-2" { //nolint:gosec // 测试用的假 API Key
			t.Errorf("期望apiKey=sk-cooldown-key-2，实际%s", apiKey)
		}
	})

	t.Run("所有Key冷却后返回错误", func(t *testing.T) {
		// 再冷却Key2
		_, err = store.BumpKeyCooldown(ctx, cfg.ID, 2, now, 401)
		if err != nil {
			t.Fatalf("冷却Key2失败: %v", err)
		}

		// 重新查询apiKeys以获取最新冷却状态
		apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		_, _, err = selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

		if err == nil {
			t.Error("期望返回错误（所有Key都在冷却），但成功返回")
		}
	})
}

// TestSelectAvailableKey_CooldownAndExclude 测试冷却与排除组合
func TestSelectAvailableKey_CooldownAndExclude(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)
	now := time.Now()

	// 创建渠道
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "combined-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 创建4个API Keys
	combKeys := make([]*model.APIKey, 4)
	for i := 0; i < 4; i++ {
		combKeys[i] = &model.APIKey{
			ChannelID:   cfg.ID,
			KeyIndex:    i,
			APIKey:      "sk-combined-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, combKeys); err != nil {
		t.Fatalf("批量创建API Keys失败: %v", err)
	}

	// 冷却Key1
	_, err = store.BumpKeyCooldown(ctx, cfg.ID, 1, now, 401)
	if err != nil {
		t.Fatalf("冷却Key1失败: %v", err)
	}

	// 预先查询apiKeys（在冷却Key1之后，包含冷却状态）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	// 排除Key0和Key2
	excludeKeys := map[int]bool{0: true, 2: true}

	keyIndex, apiKey, err := selector.SelectAvailableKey(cfg.ID, apiKeys, excludeKeys)

	if err != nil {
		t.Fatalf("SelectAvailableKey失败: %v", err)
	}

	// 应该跳过排除的Key0和Key2、冷却的Key1，返回Key3
	if keyIndex != 3 {
		t.Errorf("期望返回keyIndex=3（跳过排除和冷却的Key），实际%d", keyIndex)
	}

	if apiKey != "sk-combined-key-3" { //nolint:gosec // 测试用的假 API Key
		t.Errorf("期望apiKey=sk-combined-key-3，实际%s", apiKey)
	}
}

// TestSelectAvailableKey_NoKeys 测试无Key配置场景
func TestSelectAvailableKey_NoKeys(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()

	selector := NewKeySelector() // 移除store参数
	ctx := context.WithValue(context.Background(), testingContextKey, true)

	// 创建渠道（不配置API Keys）
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "no-keys-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 预先查询apiKeys（应该为空）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	_, _, err = selector.SelectAvailableKey(cfg.ID, apiKeys, nil)

	if err == nil {
		t.Error("期望返回错误（渠道未配置API Keys），但成功返回")
	}
}

func TestKeySelector_CleanupInactiveCounters(t *testing.T) {
	ks := NewKeySelector()

	expiredKeys := []*model.APIKey{
		{KeyIndex: 10, APIKey: "k10", KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 11, APIKey: "k11", KeyStrategy: model.KeyStrategyRoundRobin},
	}
	activeKeys := []*model.APIKey{
		{KeyIndex: 20, APIKey: "k20", KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 21, APIKey: "k21", KeyStrategy: model.KeyStrategyRoundRobin},
	}

	// 同一渠道的不同候选池必须独立过期。
	if _, _, err := ks.SelectAvailableKey(100, expiredKeys, nil); err != nil {
		t.Fatalf("SelectAvailableKey(expired pool) failed: %v", err)
	}
	if _, _, err := ks.SelectAvailableKey(100, activeKeys, nil); err != nil {
		t.Fatalf("SelectAvailableKey(active pool) failed: %v", err)
	}

	expiredScope := newRRCounterScope(100, expiredKeys)
	activeScope := newRRCounterScope(100, activeKeys)
	expired := ks.getOrCreateCounter(expiredScope)
	expired.lastAccess.Store(time.Now().Add(-48 * time.Hour).UnixNano())
	active := ks.getOrCreateCounter(activeScope)
	active.lastAccess.Store(time.Now().UnixNano())

	ks.CleanupInactiveCounters(24 * time.Hour)

	ks.rrMutex.RLock()
	_, okExpired := ks.rrCounters[expiredScope]
	_, okActive := ks.rrCounters[activeScope]
	ks.rrMutex.RUnlock()

	if okExpired {
		t.Fatalf("expected channel=100 counter to be cleaned up")
	}
	if !okActive {
		t.Fatalf("expected channel=200 counter to remain")
	}
}

func TestSelectAvailableKey_PriorityTiers(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{model.KeyStrategySequential, model.KeyStrategyRoundRobin, "", "unknown"} {
		t.Run(strategy, func(t *testing.T) {
			selector := NewKeySelector()
			keys := []*model.APIKey{
				{KeyIndex: 2, APIKey: "low", Priority: -10, KeyStrategy: strategy},
				{KeyIndex: 7, APIKey: "high-a", Priority: 20, KeyStrategy: strategy},
				{KeyIndex: 11, APIKey: "high-b", Priority: 20, KeyStrategy: strategy},
				{KeyIndex: 15, APIKey: "disabled", Priority: 100, Disabled: true, KeyStrategy: strategy},
				{KeyIndex: 21, APIKey: "middle", Priority: 0, KeyStrategy: strategy},
			}
			seen := map[int]bool{}
			for range 2 {
				index, _, err := selector.SelectAvailableKey(1, keys, nil)
				if err != nil {
					t.Fatal(err)
				}
				seen[index] = true
			}
			if !seen[7] || !seen[11] {
				t.Fatalf("equal priorities must rotate for legacy strategy %s: %v", strategy, seen)
			}
			excluded := map[int]bool{}
			for _, priority := range []int{20, 20, 0, -10} {
				index, _, err := selector.SelectAvailableKey(1, keys, excluded)
				if err != nil {
					t.Fatal(err)
				}
				var selected *model.APIKey
				for _, key := range keys {
					if key.KeyIndex == index {
						selected = key
					}
				}
				if selected == nil || selected.Priority != priority || excluded[index] {
					t.Fatalf("selected %v, want priority %d", selected, priority)
				}
				excluded[index] = true
			}
			if _, _, err := selector.SelectAvailableKey(1, keys, excluded); err == nil {
				t.Fatal("exhausted keys selected")
			}
			for _, key := range keys[1:3] {
				key.CooldownUntil = time.Now().Add(time.Hour).Unix()
			}
			if index, _, err := selector.SelectAvailableKey(1, keys, nil); err != nil || index != 21 {
				t.Fatalf("cooling top tier: index=%d err=%v", index, err)
			}
			keys[1].CooldownUntil = time.Now().Add(-time.Second).Unix()
			if index, _, err := selector.SelectAvailableKey(1, keys, nil); err != nil || index != 7 {
				t.Fatalf("recovered top tier: index=%d err=%v", index, err)
			}
		})
	}
}

func TestSelectAvailableKey_RoundRobinIsolatedByPriority(t *testing.T) {
	t.Parallel()
	selector := NewKeySelector()
	keys := []*model.APIKey{
		{KeyIndex: 2, APIKey: "low-a", Priority: -1, KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 4, APIKey: "high-a", Priority: 1, KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 8, APIKey: "low-b", Priority: -1, KeyStrategy: model.KeyStrategyRoundRobin},
		{KeyIndex: 16, APIKey: "high-b", Priority: 1, KeyStrategy: model.KeyStrategyRoundRobin},
	}
	seenHigh, seenLow := map[int]bool{}, map[int]bool{}
	for range 2 {
		index, _, err := selector.SelectAvailableKey(1, keys, nil)
		if err != nil {
			t.Fatal(err)
		}
		seenHigh[index] = true
		index, _, err = selector.SelectAvailableKey(1, keys, map[int]bool{4: true, 16: true})
		if err != nil {
			t.Fatal(err)
		}
		seenLow[index] = true
	}
	if !seenHigh[4] || !seenHigh[16] || !seenLow[2] || !seenLow[8] {
		t.Fatalf("tiers did not rotate: %v %v", seenHigh, seenLow)
	}
}

func TestSelectCooldownFallbackKey_PriorityTieBreak(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(time.Hour).Unix()
	keys := []*model.APIKey{
		{KeyIndex: 1, APIKey: "low", Priority: -1, CooldownUntil: until},
		{KeyIndex: 9, APIKey: "high-later-index", Priority: 5, CooldownUntil: until},
		{KeyIndex: 7, APIKey: "high", Priority: 5, CooldownUntil: until},
		{KeyIndex: 15, APIKey: "earlier", Priority: -10, CooldownUntil: until - 1},
		{KeyIndex: 20, APIKey: "disabled", Priority: 99, Disabled: true, CooldownUntil: until - 2},
	}
	selector := NewKeySelector()
	excluded := map[int]bool{}
	for _, want := range []int{15, 7, 9, 1} {
		index, _, err := selector.SelectCooldownFallbackKey(1, keys, excluded)
		if err != nil || index != want {
			t.Fatalf("fallback index=%d want=%d err=%v", index, want, err)
		}
		excluded[index] = true
	}
	keys[0].CooldownUntil, keys[2].CooldownUntil = 0, 0
	if index, _, err := selector.SelectCooldownFallbackKey(1, keys, nil); err != nil || index != 7 {
		t.Fatalf("recovered keys must use priority: index=%d err=%v", index, err)
	}
}
