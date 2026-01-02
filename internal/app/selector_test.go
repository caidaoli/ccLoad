package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// TestSelectRouteCandidates_NormalRequest 测试普通请求的路由选择
func TestSelectRouteCandidates_NormalRequest(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建测试渠道，支持不同模型
	channels := []*model.Config{
		{Name: "high-priority", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "claude-3-opus", RedirectModel: ""}, {Model: "claude-3-sonnet", RedirectModel: ""}}, Enabled: true},
		{Name: "mid-priority", URL: "https://api2.com", Priority: 50, ModelEntries: []model.ModelEntry{{Model: "claude-3-sonnet", RedirectModel: ""}, {Model: "claude-3-haiku", RedirectModel: ""}}, Enabled: true},
		{Name: "low-priority", URL: "https://api3.com", Priority: 10, ModelEntries: []model.ModelEntry{{Model: "claude-3-haiku", RedirectModel: ""}}, Enabled: true},
	}

	for _, cfg := range channels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	tests := []struct {
		name          string
		model         string
		expectedCount int
		checkPriority bool
	}{
		{
			name:          "查询claude-3-opus模型",
			model:         "claude-3-opus",
			expectedCount: 1, // 只有high-priority支持
			checkPriority: false,
		},
		{
			name:          "查询claude-3-sonnet模型",
			model:         "claude-3-sonnet",
			expectedCount: 2, // high-priority和mid-priority支持
			checkPriority: true,
		},
		{
			name:          "查询claude-3-haiku模型",
			model:         "claude-3-haiku",
			expectedCount: 2, // mid-priority和low-priority支持
			checkPriority: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidates, err := server.selectCandidatesByModelAndType(ctx, tt.model, "")

			if err != nil {
				t.Errorf("selectCandidates失败: %v", err)
			}

			if len(candidates) != tt.expectedCount {
				t.Errorf("期望%d个候选渠道，实际%d个", tt.expectedCount, len(candidates))
			}

			// 验证优先级排序（降序）
			if tt.checkPriority && len(candidates) > 1 {
				for i := 0; i < len(candidates)-1; i++ {
					if candidates[i].Priority < candidates[i+1].Priority {
						t.Errorf("优先级排序错误: %s(优先级%d) 应该在 %s(优先级%d) 之前",
							candidates[i].Name, candidates[i].Priority,
							candidates[i+1].Name, candidates[i+1].Priority)
					}
				}
				t.Logf("[INFO] 优先级排序正确: %s(%d) > %s(%d)",
					candidates[0].Name, candidates[0].Priority,
					candidates[1].Name, candidates[1].Priority)
			}
		})
	}
}

// TestSelectRouteCandidates_CooledDownChannels 测试冷却渠道过滤
func TestSelectRouteCandidates_CooledDownChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()
	now := time.Now()

	// 创建3个渠道，其中2个处于冷却状态
	channels := []*model.Config{
		{Name: "active-channel", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "cooled-channel-1", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "cooled-channel-2", URL: "https://api3.com", Priority: 80, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	var createdIDs []int64
	for _, cfg := range channels {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		createdIDs = append(createdIDs, created.ID)
	}

	// 冷却第2和第3个渠道
	_, err := store.BumpChannelCooldown(ctx, createdIDs[1], now, 500)
	if err != nil {
		t.Fatalf("冷却渠道1失败: %v", err)
	}
	_, err = store.BumpChannelCooldown(ctx, createdIDs[2], now, 503)
	if err != nil {
		t.Fatalf("冷却渠道2失败: %v", err)
	}

	// 查询可用渠道
	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证只返回未冷却的渠道
	if len(candidates) != 1 {
		t.Errorf("期望1个可用渠道（排除2个冷却渠道），实际%d个", len(candidates))
	}

	if len(candidates) > 0 && candidates[0].Name != "active-channel" {
		t.Errorf("期望返回active-channel，实际返回%s", candidates[0].Name)
	}

	t.Logf("[INFO] 冷却过滤正确: 3个渠道中2个被冷却，只返回1个可用渠道")
}

func TestSelectRouteCandidates_AllCooled_FallbackChoosesEarliestChannelCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()
	now := time.Now()

	channels := []*model.Config{
		{Name: "cooldown-long", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "cooldown-short", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	var ids []int64
	for _, cfg := range channels {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		ids = append(ids, created.ID)
	}

	// 手动设置不同的冷却时间，制造“全冷却”场景
	if err := store.SetChannelCooldown(ctx, ids[0], now.Add(2*time.Minute)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}
	if err := store.SetChannelCooldown(ctx, ids[1], now.Add(30*time.Second)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}

	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("期望全冷却兜底返回1个候选渠道，实际%d个", len(candidates))
	}
	if candidates[0].Name != "cooldown-short" {
		t.Fatalf("期望选择最早恢复的渠道 cooldown-short，实际返回%s", candidates[0].Name)
	}
}

func TestSelectRouteCandidates_AllCooled_FallbackDisabledWhenThresholdZero(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	if err := store.UpdateSetting(ctx, "cooldown_fallback_threshold", "0"); err != nil {
		t.Fatalf("设置cooldown_fallback_threshold失败: %v", err)
	}

	cs := NewConfigService(store)
	if err := cs.LoadDefaults(ctx); err != nil {
		t.Fatalf("ConfigService加载失败: %v", err)
	}

	server := &Server{store: store, configService: cs}

	channels := []*model.Config{
		{Name: "cooldown-long", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "cooldown-short", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	var ids []int64
	for _, cfg := range channels {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		ids = append(ids, created.ID)
	}

	// 全冷却场景：兜底被禁用时应返回空，触发上层503
	if err := store.SetChannelCooldown(ctx, ids[0], now.Add(2*time.Minute)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}
	if err := store.SetChannelCooldown(ctx, ids[1], now.Add(30*time.Second)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}

	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	if len(candidates) != 0 {
		t.Fatalf("期望兜底禁用时返回0个候选渠道，实际%d个", len(candidates))
	}
}

func TestSelectRouteCandidates_AllCooledByKeys_FallbackChoosesEarliestKeyCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()
	now := time.Now()

	channels := []*model.Config{
		{Name: "keys-long", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "keys-short", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	var ids []int64
	for _, cfg := range channels {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		ids = append(ids, created.ID)

		// 每个渠道创建2个Key，使 KeyCount 生效
		keys := make([]*model.APIKey, 2)
		for keyIndex := 0; keyIndex < 2; keyIndex++ {
			keys[keyIndex] = &model.APIKey{
				ChannelID:   created.ID,
				KeyIndex:    keyIndex,
				APIKey:      "sk-test",
				KeyStrategy: model.KeyStrategySequential,
				CreatedAt:   model.JSONTime{Time: now},
				UpdatedAt:   model.JSONTime{Time: now},
			}
		}
		if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
			t.Fatalf("创建API Keys失败: %v", err)
		}
	}

	// 让两个渠道都“全Key冷却”，但解禁时间不同
	for keyIndex := 0; keyIndex < 2; keyIndex++ {
		if err := store.SetKeyCooldown(ctx, ids[0], keyIndex, now.Add(2*time.Minute)); err != nil {
			t.Fatalf("设置Key冷却失败: %v", err)
		}
		if err := store.SetKeyCooldown(ctx, ids[1], keyIndex, now.Add(20*time.Second)); err != nil {
			t.Fatalf("设置Key冷却失败: %v", err)
		}
	}

	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("期望全冷却(Key)兜底返回1个候选渠道，实际%d个", len(candidates))
	}
	if candidates[0].Name != "keys-short" {
		t.Fatalf("期望选择最早恢复的渠道 keys-short，实际返回%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_DisabledChannels 测试禁用渠道过滤
func TestSelectRouteCandidates_DisabledChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建2个渠道，1个启用，1个禁用
	enabledCfg := &model.Config{
		Name:         "enabled-channel",
		URL:          "https://api1.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	}
	disabledCfg := &model.Config{
		Name:         "disabled-channel",
		URL:          "https://api2.com",
		Priority:     90,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      false,
	}

	_, err := store.CreateConfig(ctx, enabledCfg)
	if err != nil {
		t.Fatalf("创建启用渠道失败: %v", err)
	}
	_, err = store.CreateConfig(ctx, disabledCfg)
	if err != nil {
		t.Fatalf("创建禁用渠道失败: %v", err)
	}

	// 查询可用渠道
	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证只返回启用的渠道
	if len(candidates) != 1 {
		t.Errorf("期望1个启用渠道，实际%d个", len(candidates))
	}

	if len(candidates) > 0 && candidates[0].Name != "enabled-channel" {
		t.Errorf("期望返回enabled-channel，实际返回%s", candidates[0].Name)
	}

	t.Logf("[INFO] 禁用渠道过滤正确")
}

// TestSelectRouteCandidates_PriorityGrouping 测试优先级分组和轮询
func TestSelectRouteCandidates_PriorityGrouping(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建相同优先级的多个渠道
	samePriorityChannels := []*model.Config{
		{Name: "channel-a", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "channel-b", URL: "https://api2.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "channel-c", URL: "https://api3.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	for _, cfg := range samePriorityChannels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 查询渠道
	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证所有相同优先级的渠道都被返回
	if len(candidates) != 3 {
		t.Errorf("期望3个相同优先级的渠道，实际%d个", len(candidates))
	}

	// 验证所有渠道优先级相同
	for i, c := range candidates {
		if c.Priority != 100 {
			t.Errorf("渠道%d优先级错误: 期望100，实际%d", i, c.Priority)
		}
	}

	t.Logf("[INFO] 相同优先级渠道分组正确，返回%d个渠道", len(candidates))
}

// TestSelectCandidates_FilterByChannelType 测试按渠道类型过滤
func TestSelectCandidates_FilterByChannelType(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	channels := []*model.Config{
		{Name: "anthropic-channel", URL: "https://anthropic.example.com", Priority: 50, ModelEntries: []model.ModelEntry{{Model: "gpt-4", RedirectModel: ""}}, ChannelType: "anthropic", Enabled: true},
		{Name: "codex-channel", URL: "https://openai.example.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "gpt-4", RedirectModel: ""}}, ChannelType: "codex", Enabled: true},
	}

	for _, cfg := range channels {
		if _, err := store.CreateConfig(ctx, cfg); err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	allCandidates, err := server.selectCandidatesByModelAndType(ctx, "gpt-4", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(allCandidates) != 2 {
		t.Fatalf("预期返回2个候选渠道，实际%d个", len(allCandidates))
	}

	filtered, err := server.selectCandidatesByModelAndType(ctx, "gpt-4", "codex")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndType失败: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Name != "codex-channel" {
		t.Fatalf("渠道类型过滤失败，返回结果: %+v", filtered)
	}

	// 保证类型过滤支持大小写输入
	filteredUpper, err := server.selectCandidatesByModelAndType(ctx, "gpt-4", "CODEX")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndType(大写)失败: %v", err)
	}
	if len(filteredUpper) != 1 || filteredUpper[0].Name != "codex-channel" {
		t.Fatalf("渠道类型大小写规范化失败，返回结果: %+v", filteredUpper)
	}

	// 未匹配到指定类型时应返回空切片
	filteredNone, err := server.selectCandidatesByModelAndType(ctx, "gpt-4", "gemini")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndType(无匹配)失败: %v", err)
	}
	if len(filteredNone) != 0 {
		t.Fatalf("预期无匹配渠道，实际返回%d个", len(filteredNone))
	}
}

// TestSelectCandidatesByChannelType_GeminiFilter 测试按渠道类型选择（Gemini）
func TestSelectCandidatesByChannelType_GeminiFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建不同类型的渠道
	channels := []*model.Config{
		{Name: "gemini-channel", URL: "https://gemini.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "gemini-pro", RedirectModel: ""}}, ChannelType: "gemini", Enabled: true},
		{Name: "anthropic-channel", URL: "https://api.anthropic.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "claude-3", RedirectModel: ""}}, ChannelType: "anthropic", Enabled: true},
		{Name: "codex-channel", URL: "https://api.openai.com", Priority: 80, ModelEntries: []model.ModelEntry{{Model: "gpt-4", RedirectModel: ""}}, ChannelType: "codex", Enabled: true},
	}

	for _, cfg := range channels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 查询Gemini类型渠道
	candidates, err := server.selectCandidatesByChannelType(ctx, "gemini")
	if err != nil {
		t.Fatalf("selectCandidatesByChannelType失败: %v", err)
	}

	// 验证只返回Gemini渠道
	if len(candidates) != 1 {
		t.Errorf("期望1个Gemini渠道，实际%d个", len(candidates))
	}

	if len(candidates) > 0 {
		if candidates[0].ChannelType != "gemini" {
			t.Errorf("期望渠道类型为gemini，实际为%s", candidates[0].ChannelType)
		}
		if candidates[0].Name != "gemini-channel" {
			t.Errorf("期望返回gemini-channel，实际返回%s", candidates[0].Name)
		}
	}

	t.Logf("[INFO] 渠道类型过滤正确")
}

// TestSelectRouteCandidates_WildcardModel 测试通配符模型
func TestSelectRouteCandidates_WildcardModel(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建多个支持不同模型的渠道
	channels := []*model.Config{
		{Name: "channel-1", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "model-a", RedirectModel: ""}}, Enabled: true},
		{Name: "channel-2", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "model-b", RedirectModel: ""}}, Enabled: true},
		{Name: "channel-3", URL: "https://api3.com", Priority: 80, ModelEntries: []model.ModelEntry{{Model: "model-c", RedirectModel: ""}}, Enabled: true},
	}

	for _, cfg := range channels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 使用通配符"*"查询所有启用渠道
	candidates, err := server.selectCandidatesByModelAndType(ctx, "*", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证返回所有启用渠道
	if len(candidates) != 3 {
		t.Errorf("期望3个渠道（通配符匹配所有），实际%d个", len(candidates))
	}

	// 验证优先级排序
	if len(candidates) >= 2 {
		if candidates[0].Priority < candidates[1].Priority {
			t.Errorf("优先级排序错误")
		}
		t.Logf("[INFO] 通配符查询正确，返回%d个渠道，优先级排序正确", len(candidates))
	}
}

// TestSelectRouteCandidates_NoMatchingChannels 测试无匹配渠道场景
func TestSelectRouteCandidates_NoMatchingChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建只支持特定模型的渠道
	cfg := &model.Config{
		Name:         "specific-channel",
		URL:          "https://api.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "specific-model", RedirectModel: ""}},
		Enabled:      true,
	}
	_, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 查询不存在的模型
	candidates, err := server.selectCandidatesByModelAndType(ctx, "non-existent-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证返回空列表
	if len(candidates) != 0 {
		t.Errorf("期望0个匹配渠道，实际%d个", len(candidates))
	}

	t.Logf("[INFO] 无匹配渠道场景处理正确")
}

// TestSelectRouteCandidates_ModelDateSuffixFallback 测试“模型日期后缀回退匹配”功能开关
func TestSelectRouteCandidates_ModelDateSuffixFallback(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 渠道仅配置“无日期后缀”的模型
	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "base-model-channel",
		URL:          "https://api.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 1) 默认关闭：完全匹配失败后不回退
	serverDisabled := &Server{store: store}
	candidates, err := serverDisabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5-20250929", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("期望0个匹配渠道（回退关闭），实际%d个", len(candidates))
	}

	// 2) 开启后：完全匹配失败时去除末尾-YYYYMMDD回退
	serverEnabled := &Server{store: store, modelLookupStripDateSuffix: true}
	candidates, err = serverEnabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5-20250929", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（回退开启），实际%d个", len(candidates))
	}
	if candidates[0].Name != "base-model-channel" {
		t.Fatalf("期望命中base-model-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelDateSuffixFallback_PreferExact 测试“优先完全匹配”
func TestSelectRouteCandidates_ModelDateSuffixFallback_PreferExact(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// base 渠道：仅配置无日期后缀
	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "base-model-channel",
		URL:          "https://api-base.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建base渠道失败: %v", err)
	}

	// dated 渠道：配置带日期后缀（完全匹配应命中它）
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "dated-model-channel",
		URL:          "https://api-dated.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5-20250929", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建dated渠道失败: %v", err)
	}

	server := &Server{store: store, modelLookupStripDateSuffix: true}
	candidates, err := server.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5-20250929", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（完全匹配直接命中），实际%d个", len(candidates))
	}
	if candidates[0].Name != "dated-model-channel" {
		t.Fatalf("期望优先命中dated-model-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelDateSuffixFallback_Reverse 测试“请求无日期→匹配渠道带日期模型”
func TestSelectRouteCandidates_ModelDateSuffixFallback_Reverse(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 渠道仅配置“带日期后缀”的模型
	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "dated-model-channel",
		URL:          "https://api.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5-20250929", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 1) 默认关闭：完全匹配失败后不回退
	serverDisabled := &Server{store: store}
	candidates, err := serverDisabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("期望0个匹配渠道（回退关闭），实际%d个", len(candidates))
	}

	// 2) 开启后：完全匹配失败时允许匹配到带日期后缀的模型配置
	serverEnabled := &Server{store: store, modelLookupStripDateSuffix: true}
	candidates, err = serverEnabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（回退开启），实际%d个", len(candidates))
	}
	if candidates[0].Name != "dated-model-channel" {
		t.Fatalf("期望命中dated-model-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelDateSuffixFallback_PreferExactBase 测试“请求无日期时也优先精确匹配”
func TestSelectRouteCandidates_ModelDateSuffixFallback_PreferExactBase(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "base-model-channel",
		URL:          "https://api-base.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建base渠道失败: %v", err)
	}

	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "dated-model-channel",
		URL:          "https://api-dated.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5-20250929", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建dated渠道失败: %v", err)
	}

	server := &Server{store: store, modelLookupStripDateSuffix: true}
	candidates, err := server.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（精确匹配直接命中），实际%d个", len(candidates))
	}
	if candidates[0].Name != "base-model-channel" {
		t.Fatalf("期望优先命中base-model-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelDateSuffixFallback_CrossChannelType 测试跨渠道类型的日期后缀回退
// 场景：请求 anthropic 类型的 claude-sonnet-4-5，但精确匹配只找到 openai 类型渠道
// 应该回退到 anthropic 类型的带日期后缀渠道，而不是被 openai 渠道阻塞
func TestSelectRouteCandidates_ModelDateSuffixFallback_CrossChannelType(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 创建一个 openai 类型渠道，配置了无日期后缀的模型
	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "openai-channel",
		URL:          "https://api.openai.com",
		ChannelType:  "openai",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建openai渠道失败: %v", err)
	}

	// 创建一个 anthropic 类型渠道，只配置了带日期后缀的模型
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "anthropic-channel",
		URL:          "https://api.anthropic.com",
		ChannelType:  "anthropic",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5-20250929", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建anthropic渠道失败: %v", err)
	}

	server := &Server{store: store, modelLookupStripDateSuffix: true}

	// 请求 anthropic 类型 + claude-sonnet-4-5
	// 应该回退匹配到 anthropic-channel（而不是被 openai-channel 阻塞）
	candidates, err := server.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "anthropic")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（跨类型回退），实际%d个", len(candidates))
	}
	if candidates[0].Name != "anthropic-channel" {
		t.Fatalf("期望命中anthropic-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_MixedPriorities 测试混合优先级排序
func TestSelectRouteCandidates_MixedPriorities(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建不同优先级的渠道
	channels := []*model.Config{
		{Name: "low-1", URL: "https://api1.com", Priority: 10, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "high-1", URL: "https://api2.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "mid-1", URL: "https://api3.com", Priority: 50, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "high-2", URL: "https://api4.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "mid-2", URL: "https://api5.com", Priority: 50, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	for _, cfg := range channels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 查询渠道
	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	// 验证返回所有渠道
	if len(candidates) != 5 {
		t.Errorf("期望5个渠道，实际%d个", len(candidates))
	}

	// 验证优先级严格降序排列
	expectedOrder := []string{"high-1", "high-2", "mid-1", "mid-2", "low-1"}
	for i := range candidates {
		if i > 0 {
			if candidates[i].Priority > candidates[i-1].Priority {
				t.Errorf("优先级排序错误: 位置%d的渠道优先级(%d)大于位置%d的渠道优先级(%d)",
					i, candidates[i].Priority, i-1, candidates[i-1].Priority)
			}
		}

		// 验证名称顺序（在相同优先级内按ID升序，即创建顺序）
		expectedPrefix := expectedOrder[i]
		if candidates[i].Name != expectedPrefix {
			t.Logf("位置%d: 期望%s，实际%s（优先级%d）",
				i, expectedPrefix, candidates[i].Name, candidates[i].Priority)
		}
	}

	t.Logf("[INFO] 混合优先级排序正确: %v", func() []string {
		names := make([]string, len(candidates))
		for i, c := range candidates {
			names[i] = c.Name
		}
		return names
	}())
}

// TestShuffleSamePriorityChannels 测试相同优先级渠道的随机化
func TestShuffleSamePriorityChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store}
	ctx := context.Background()

	// 创建两个相同优先级的渠道（模拟渠道22和23）
	channels := []*model.Config{
		{Name: "channel-22", URL: "https://api22.com", Priority: 20, ModelEntries: []model.ModelEntry{{Model: "qwen-3-32b", RedirectModel: ""}}, ChannelType: "codex", Enabled: true},
		{Name: "channel-23", URL: "https://api23.com", Priority: 20, ModelEntries: []model.ModelEntry{{Model: "qwen-3-32b", RedirectModel: ""}}, ChannelType: "codex", Enabled: true},
	}

	for _, cfg := range channels {
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 多次查询，统计渠道22和23出现在第一位的次数
	iterations := 100
	firstPositionCount := make(map[string]int)

	for i := 0; i < iterations; i++ {
		candidates, err := server.selectCandidatesByModelAndType(ctx, "qwen-3-32b", "codex")
		if err != nil {
			t.Fatalf("selectCandidatesByModelAndType失败: %v", err)
		}

		if len(candidates) != 2 {
			t.Fatalf("期望2个渠道，实际%d个", len(candidates))
		}

		// 统计第一个渠道
		firstPositionCount[candidates[0].Name]++
	}

	t.Logf("[STATS] 随机化统计（%d次查询）:", iterations)
	t.Logf("  - channel-22 首位出现: %d次 (%.1f%%)",
		firstPositionCount["channel-22"],
		float64(firstPositionCount["channel-22"])/float64(iterations)*100)
	t.Logf("  - channel-23 首位出现: %d次 (%.1f%%)",
		firstPositionCount["channel-23"],
		float64(firstPositionCount["channel-23"])/float64(iterations)*100)

	// 验证两个渠道都有机会出现在第一位（允许一定的随机偏差）
	// 理论上应该各50%，但允许30%-70%的范围
	if firstPositionCount["channel-22"] < 30 || firstPositionCount["channel-22"] > 70 {
		t.Errorf("随机化分布异常: channel-22出现%d次，期望30-70次", firstPositionCount["channel-22"])
	}
	if firstPositionCount["channel-23"] < 30 || firstPositionCount["channel-23"] > 70 {
		t.Errorf("随机化分布异常: channel-23出现%d次，期望30-70次", firstPositionCount["channel-23"])
	}

	t.Logf("[INFO] 相同优先级渠道随机化正常，负载均衡有效")
}

// ========== 辅助函数 ==========

func setupTestStore(t *testing.T) (storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/selector_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	cleanup := func() {
		store.Close()
	}

	return store, cleanup
}
