package app

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
)

// TestSelectRouteCandidates_NormalRequest 测试普通请求的路由选择
func TestSelectRouteCandidates_NormalRequest(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	if err := store.UpdateSetting(ctx, "cooldown_fallback_enabled", "0"); err != nil {
		t.Fatalf("设置cooldown_fallback_enabled失败: %v", err)
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

func TestSelectRouteCandidates_AllCooled_MixedCooldown_RespectsChannelCooldown(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
	ctx := context.Background()
	now := time.Now()

	channels := []*model.Config{
		{Name: "channel-cooled-long", URL: "https://api1.com", Priority: 100, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
		{Name: "keys-cooled-short", URL: "https://api2.com", Priority: 90, ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}}, Enabled: true},
	}

	var ids []int64
	for _, cfg := range channels {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
		ids = append(ids, created.ID)

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

	// 渠道1：渠道级冷却很久，但Key较早解禁（真实可用时间应由渠道冷却主导）
	if err := store.SetChannelCooldown(ctx, ids[0], now.Add(2*time.Minute)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}
	for keyIndex := 0; keyIndex < 2; keyIndex++ {
		if err := store.SetKeyCooldown(ctx, ids[0], keyIndex, now.Add(10*time.Second)); err != nil {
			t.Fatalf("设置Key冷却失败: %v", err)
		}
	}

	// 渠道2：仅Key全冷却，较早解禁（应被选中）
	for keyIndex := 0; keyIndex < 2; keyIndex++ {
		if err := store.SetKeyCooldown(ctx, ids[1], keyIndex, now.Add(30*time.Second)); err != nil {
			t.Fatalf("设置Key冷却失败: %v", err)
		}
	}

	candidates, err := server.selectCandidatesByModelAndType(ctx, "test-model", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("期望全冷却(混合)兜底返回1个候选渠道，实际%d个", len(candidates))
	}
	if candidates[0].Name != "keys-cooled-short" {
		t.Fatalf("期望选择 keys-cooled-short，实际返回%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_DisabledChannels 测试禁用渠道过滤
func TestSelectRouteCandidates_DisabledChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

// TestSelectRouteCandidates_ModelFuzzyMatch 测试"模型模糊匹配"功能
// 场景：请求无日期后缀模型，渠道配置带日期后缀模型
func TestSelectRouteCandidates_ModelFuzzyMatch(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 渠道配置"带日期后缀"的模型
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

	// 1) 默认关闭：模糊匹配不生效
	serverDisabled := &Server{store: store, modelFuzzyMatch: false}
	candidates, err := serverDisabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("期望0个匹配渠道（模糊匹配关闭），实际%d个", len(candidates))
	}

	// 2) 开启后：无日期后缀可匹配到带日期后缀的模型
	serverEnabled := &Server{store: store, modelFuzzyMatch: true}
	candidates, err = serverEnabled.selectCandidatesByModelAndType(ctx, "claude-sonnet-4-5", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道（模糊匹配开启），实际%d个", len(candidates))
	}
	if candidates[0].Name != "dated-model-channel" {
		t.Fatalf("期望命中dated-model-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelFuzzyMatch_PreferExact 测试"优先精确匹配"
func TestSelectRouteCandidates_ModelFuzzyMatch_PreferExact(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// base 渠道：配置无日期后缀
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

	// dated 渠道：配置带日期后缀
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

	server := &Server{store: store, modelFuzzyMatch: true}

	// 请求无日期后缀时，应优先精确匹配
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

func TestSelectRouteCandidates_ModelFuzzyMatch_AfterCooldownFiltering(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	// 精确匹配渠道：但处于冷却中
	exactCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "exact-cooled-channel",
		URL:          "https://api-exact.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "gemini-3-flash", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建exact渠道失败: %v", err)
	}
	if err := store.SetChannelCooldown(ctx, exactCfg.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("设置exact渠道冷却失败: %v", err)
	}

	// 模糊匹配渠道：可用
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:         "fuzzy-available-channel",
		URL:          "https://api-fuzzy.com",
		Priority:     90,
		ModelEntries: []model.ModelEntry{{Model: "gemini-3-flash-preview", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建fuzzy渠道失败: %v", err)
	}

	server := &Server{
		store:           store,
		channelBalancer: NewSmoothWeightedRR(),
		modelFuzzyMatch: true,
	}

	candidates, err := server.selectCandidatesByModelAndType(ctx, "gemini-3-flash", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道，实际%d个", len(candidates))
	}
	if candidates[0].Name != "fuzzy-available-channel" {
		t.Fatalf("期望命中fuzzy-available-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_ModelFuzzyMatch_SubstringMatch 测试子串模糊匹配
// 场景：请求简短模型名如 "sonnet"，匹配到完整模型名
func TestSelectRouteCandidates_ModelFuzzyMatch_SubstringMatch(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "claude-channel",
		URL:          "https://api.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-5-20250929", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	server := &Server{store: store, modelFuzzyMatch: true}

	// 请求 "sonnet" 应匹配到 "claude-sonnet-4-5-20250929"
	candidates, err := server.selectCandidatesByModelAndType(ctx, "sonnet", "")
	if err != nil {
		t.Fatalf("selectCandidates失败: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望1个匹配渠道，实际%d个", len(candidates))
	}
	if candidates[0].Name != "claude-channel" {
		t.Fatalf("期望命中claude-channel，实际命中%s", candidates[0].Name)
	}
}

// TestSelectRouteCandidates_MixedPriorities 测试混合优先级排序
func TestSelectRouteCandidates_MixedPriorities(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

// TestBalanceSamePriorityChannels 测试相同优先级渠道的负载均衡（确定性轮询）
func TestBalanceSamePriorityChannels(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	server := &Server{store: store, channelBalancer: NewSmoothWeightedRR()}
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

	t.Logf("[STATS] 负载均衡统计（%d次查询）:", iterations)
	t.Logf("  - channel-22 首位出现: %d次 (%.1f%%)",
		firstPositionCount["channel-22"],
		float64(firstPositionCount["channel-22"])/float64(iterations)*100)
	t.Logf("  - channel-23 首位出现: %d次 (%.1f%%)",
		firstPositionCount["channel-23"],
		float64(firstPositionCount["channel-23"])/float64(iterations)*100)

	// 相同权重的确定性轮询：两者应该严格接近 50/50。
	// iterations 为偶数时应精确对半；为奇数时允许相差1。
	diff := firstPositionCount["channel-22"] - firstPositionCount["channel-23"]
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Errorf("分布异常: channel-22=%d, channel-23=%d（diff=%d，期望<=1）",
			firstPositionCount["channel-22"], firstPositionCount["channel-23"], diff)
	}

	t.Logf("[INFO] 相同优先级渠道负载均衡正常")
}

func TestSortChannelsByHealth_WeightedByKeyCount(t *testing.T) {
	// 期望：healthCache 开启时，同有效优先级组内也要按 KeyCount 分流（容量大的拿更多流量）
	// 这里把健康惩罚权重设为0，确保两个渠道有效优先级完全相同，只验证“组内加权打散”。

	server := &Server{
		healthCache: &HealthCache{
			config: model.HealthScoreConfig{
				Enabled:                  true,
				SuccessRatePenaltyWeight: 0,
				MinConfidentSample:       0,
			},
		},
		channelBalancer: NewSmoothWeightedRR(),
	}
	empty := make(map[int64]model.ChannelHealthStats)
	server.healthCache.healthStats.Store(&empty)

	iterations := 1000
	firstPositionCount := make(map[string]int)

	for i := 0; i < iterations; i++ {
		channels := []*model.Config{
			{ID: 1, Name: "channel-A", Priority: 10, KeyCount: 10},
			{ID: 2, Name: "channel-B", Priority: 10, KeyCount: 2},
		}

		result := server.sortChannelsByHealth(channels, nil, time.Now())
		firstPositionCount[result[0].Name]++
	}

	ratioA := float64(firstPositionCount["channel-A"]) / float64(iterations) * 100
	ratioB := float64(firstPositionCount["channel-B"]) / float64(iterations) * 100

	t.Logf("[STATS] healthCache组内加权统计（%d次）:", iterations)
	t.Logf("  - channel-A (10 Keys) 首位: %d次 (%.1f%%), 期望≈83%%", firstPositionCount["channel-A"], ratioA)
	t.Logf("  - channel-B (2 Keys)  首位: %d次 (%.1f%%), 期望≈17%%", firstPositionCount["channel-B"], ratioB)

	// 验证加权分布：A应该在70%-95%范围，B在5%-30%范围
	if ratioA < 70 || ratioA > 95 {
		t.Errorf("加权分布异常: channel-A出现%.1f%%，期望70%%-95%%", ratioA)
	}
	if ratioB < 5 || ratioB > 30 {
		t.Errorf("加权分布异常: channel-B出现%.1f%%，期望5%%-30%%", ratioB)
	}
}

func TestSortChannelsByHealth_WeightedByEffectiveKeyCount(t *testing.T) {
	// 期望：当部分Key冷却时，使用有效Key数量（排除冷却中的Key）进行加权
	// channel-A: 10 keys, 8个冷却 → 有效2个
	// channel-B: 2 keys, 0个冷却 → 有效2个
	// 结果：两者应该各占约50%

	server := &Server{
		healthCache: &HealthCache{
			config: model.HealthScoreConfig{
				Enabled:                  true,
				SuccessRatePenaltyWeight: 0,
				MinConfidentSample:       0,
			},
		},
		channelBalancer: NewSmoothWeightedRR(),
	}
	empty := make(map[int64]model.ChannelHealthStats)
	server.healthCache.healthStats.Store(&empty)

	now := time.Now()
	// 模拟channel-A的8个key处于冷却中
	keyCooldowns := map[int64]map[int]time.Time{
		1: { // channel-A
			0: now.Add(time.Minute), // 冷却中
			1: now.Add(time.Minute),
			2: now.Add(time.Minute),
			3: now.Add(time.Minute),
			4: now.Add(time.Minute),
			5: now.Add(time.Minute),
			6: now.Add(time.Minute),
			7: now.Add(time.Minute),
			// key 8, 9 不在冷却中
		},
	}

	iterations := 1000
	firstPositionCount := make(map[string]int)

	for i := 0; i < iterations; i++ {
		channels := []*model.Config{
			{ID: 1, Name: "channel-A", Priority: 10, KeyCount: 10},
			{ID: 2, Name: "channel-B", Priority: 10, KeyCount: 2},
		}

		result := server.sortChannelsByHealth(channels, keyCooldowns, now)
		firstPositionCount[result[0].Name]++
	}

	ratioA := float64(firstPositionCount["channel-A"]) / float64(iterations) * 100
	ratioB := float64(firstPositionCount["channel-B"]) / float64(iterations) * 100

	t.Logf("[STATS] 冷却感知加权统计（%d次）:", iterations)
	t.Logf("  - channel-A (10 Keys, 8冷却, 有效2) 首位: %d次 (%.1f%%), 期望≈50%%", firstPositionCount["channel-A"], ratioA)
	t.Logf("  - channel-B (2 Keys, 0冷却, 有效2)  首位: %d次 (%.1f%%), 期望≈50%%", firstPositionCount["channel-B"], ratioB)

	// 验证：两者都应在40%-60%范围（有效Key数量相同时接近均匀分布）
	if ratioA < 40 || ratioA > 60 {
		t.Errorf("冷却感知加权分布异常: channel-A出现%.1f%%，期望40%%-60%%", ratioA)
	}
	if ratioB < 40 || ratioB > 60 {
		t.Errorf("冷却感知加权分布异常: channel-B出现%.1f%%，期望40%%-60%%", ratioB)
	}
}

// ========== 辅助函数 ==========

func setupTestStore(t *testing.T) (storage.Store, func()) {
	return testutil.SetupTestStore(t)
}
