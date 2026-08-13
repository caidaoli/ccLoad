package app_test

import (
	"context"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
)

// setupTestStoreWithContext 创建测试用的 Store 和 Context
func setupTestStoreWithContext(t *testing.T) (storage.Store, context.Context, func()) {
	t.Helper()

	store, cleanup := testutil.SetupTestStore(t)
	ctx := context.Background()

	return store, ctx, cleanup
}

// ==================== CSV导入导出集成测试 ====================

// TestCSVExportImport_SpecialCharacters 测试特殊字符处理
func TestCSVExportImport_SpecialCharacters(t *testing.T) {
	// 使用统一的测试环境设置
	store, ctx, cleanup := setupTestStoreWithContext(t)
	defer cleanup()

	// 包含特殊字符的测试数据
	specialConfig := &model.Config{
		Name:     "Special-Chars-Test \"with quotes\"",
		URLs:     model.ChannelURLs{{URL: "https://special.example.com?param=value&other=123"}},
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "model, with, commas"},
			{Model: "model\"with\"quotes"},
		},
		Enabled: true,
	}

	created, err := store.CreateConfig(ctx, specialConfig)
	if err != nil {
		t.Fatalf("创建特殊字符渠道失败: %v", err)
	}

	// 验证数据正确保存
	retrieved, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询渠道失败: %v", err)
	}

	if retrieved.Name != specialConfig.Name {
		t.Errorf("Name不匹配: 期望 %q, 实际 %q", specialConfig.Name, retrieved.Name)
	}

	if len(retrieved.ModelEntries) != len(specialConfig.ModelEntries) {
		t.Errorf("ModelEntries数量不匹配: 期望 %d, 实际 %d", len(specialConfig.ModelEntries), len(retrieved.ModelEntries))
	}

	t.Logf("✅ 特殊字符处理测试通过")
	t.Logf("   原始Name: %s", specialConfig.Name)
	t.Logf("   恢复Name: %s", retrieved.Name)
}

// TestCSVExportImport_LargeData 测试大量数据导出导入
func TestCSVExportImport_LargeData(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过性能测试（使用 -short 标志）")
	}

	// 使用统一的测试环境设置
	store, ctx, cleanup := setupTestStoreWithContext(t)
	defer cleanup()

	// 创建100个渠道
	totalChannels := 100
	for i := 0; i < totalChannels; i++ {
		cfg := &model.Config{
			Name:     "Large-Test-" + string(rune('A'+i%26)) + string(rune('0'+i%10)),
			URLs:     model.ChannelURLs{{URL: "https://large" + string(rune('0'+i%10)) + ".example.com"}},
			Priority: i % 20,
			ModelEntries: []model.ModelEntry{
				{Model: "model-" + string(rune('1'+i%9))},
			},
			Enabled: i%2 == 0,
		}

		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建渠道 %d 失败: %v", i, err)
		}

		// 每个渠道创建2个API Keys
		keys := make([]*model.APIKey, 2)
		for j := 0; j < 2; j++ {
			keys[j] = &model.APIKey{
				ChannelID:   created.ID,
				KeyIndex:    j,
				APIKey:      "sk-large-test-" + string(rune('0'+i%10)) + "-" + string(rune('0'+j)),
				KeyStrategy: []string{"sequential", "round_robin"}[j%2],
			}
		}
		if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
			t.Fatalf("创建API Keys失败: %v", err)
		}
	}

	// 验证数据创建成功
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	largeConfigsCount := 0
	for _, cfg := range configs {
		if strings.HasPrefix(cfg.Name, "Large-Test-") {
			largeConfigsCount++
		}
	}

	if largeConfigsCount != totalChannels {
		t.Errorf("创建的渠道数量不匹配: 期望 %d, 实际 %d", totalChannels, largeConfigsCount)
	}

	t.Logf("✅ 大量数据测试通过")
	t.Logf("   创建渠道数: %d", totalChannels)
	t.Logf("   总渠道数: %d", len(configs))
	t.Logf("   API Keys: %d (每个渠道2个)", totalChannels*2)
}
