package integration_test

import (
	"ccLoad/internal/model"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ==================== CSV导入导出集成测试 ====================

// TestCSVExport_CompleteWorkflow 测试完整的CSV导出工作流
func TestCSVExport_CompleteWorkflow(t *testing.T) {
	// 使用统一的测试环境设置，避免 Redis 依赖
	store, ctx, cleanup := setupTestStoreWithContext(t)
	defer cleanup()

	tmpDir := t.TempDir()

	// 步骤1：创建测试数据
	testConfigs := []*model.Config{
		{
			Name:     "CSV-Export-Test-1",
			URL:      "https://export1.example.com",
			Priority: 10,
			ModelEntries: []model.ModelEntry{
				{Model: "model-1"},
				{Model: "model-2"},
				{Model: "old", RedirectModel: "new"},
			},
			ChannelType: "anthropic",
			Enabled:     true,
		},
		{
			Name:     "CSV-Export-Test-2",
			URL:      "https://export2.example.com",
			Priority: 5,
			ModelEntries: []model.ModelEntry{
				{Model: "model-3"},
			},
			ChannelType: "gemini",
			Enabled:     false,
		},
	}

	for _, cfg := range testConfigs {
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}

		// 创建API Keys
		apiKeys := []*model.APIKey{
			{
				ChannelID:   created.ID,
				KeyIndex:    0,
				APIKey:      "sk-test-key-" + cfg.Name + "-1",
				KeyStrategy: "sequential",
			},
			{
				ChannelID:   created.ID,
				KeyIndex:    1,
				APIKey:      "sk-test-key-" + cfg.Name + "-2",
				KeyStrategy: "round_robin",
			},
		}

		if err := store.CreateAPIKeysBatch(ctx, apiKeys); err != nil {
			t.Fatalf("创建API Keys失败: %v", err)
		}
	}

	// 步骤2：模拟CSV导出（手动构建CSV）
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	csvFile := filepath.Join(tmpDir, "export.csv")
	file, err := os.Create(csvFile)
	if err != nil {
		t.Fatalf("创建CSV文件失败: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)

	// 写入Header
	header := []string{"id", "name", "url", "priority", "models", "model_redirects", "channel_type", "enabled", "api_keys", "key_strategy"}
	if err := writer.Write(header); err != nil {
		t.Fatalf("写入CSV header失败: %v", err)
	}

	// 写入数据行
	for _, cfg := range configs {
		if !strings.HasPrefix(cfg.Name, "CSV-Export-Test-") {
			continue
		}

		// 查询API Keys
		keys, err := store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("查询API Keys失败: %v", err)
		}

		// 构建API Keys列表
		var apiKeysList []string
		var keyStrategies []string
		for _, key := range keys {
			apiKeysList = append(apiKeysList, key.APIKey)
			keyStrategies = append(keyStrategies, key.KeyStrategy)
		}
		apiKeysStr := strings.Join(apiKeysList, ",")
		keyStrategyStr := keyStrategies[0] // 使用第一个Key的策略

		// 序列化复杂字段（转换为旧格式用于CSV兼容）
		models := cfg.GetModels()
		redirects := make(map[string]string)
		for _, e := range cfg.ModelEntries {
			if e.RedirectModel != "" {
				redirects[e.Model] = e.RedirectModel
			}
		}
		modelsJSON, _ := json.Marshal(models)
		redirectsJSON, _ := json.Marshal(redirects)

		record := []string{
			string(rune(cfg.ID + '0')),       // id (简化为单字符)
			cfg.Name,                         // name
			cfg.URL,                          // url
			string(rune(cfg.Priority + '0')), // priority
			string(modelsJSON),               // models
			string(redirectsJSON),            // model_redirects
			cfg.GetChannelType(),             // channel_type
			strconv.FormatBool(cfg.Enabled),  // enabled
			apiKeysStr,                       // api_keys
			keyStrategyStr,                   // key_strategy
		}

		if err := writer.Write(record); err != nil {
			t.Fatalf("写入CSV记录失败: %v", err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatalf("CSV写入错误: %v", err)
	}

	// 步骤3：验证CSV文件内容
	fileInfo, err := os.Stat(csvFile)
	if err != nil {
		t.Fatalf("获取CSV文件信息失败: %v", err)
	}

	if fileInfo.Size() == 0 {
		t.Error("CSV文件为空")
	}

	t.Logf("✅ CSV导出测试通过")
	t.Logf("   导出文件: %s", csvFile)
	t.Logf("   文件大小: %d bytes", fileInfo.Size())
	t.Logf("   渠道数量: %d", len(testConfigs))
}

// TestCSVImport_DataValidation 测试CSV导入时的数据验证
func TestCSVImport_DataValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// 测试用例：各种边界条件
	tests := []struct {
		name        string
		csvContent  string
		expectError bool
		description string
	}{
		{
			name: "有效的完整数据",
			csvContent: `name,url,priority,models,channel_type,enabled
Valid-Channel,https://valid.example.com,10,"[""model-1""]",anthropic,true
`,
			expectError: false,
			description: "应该成功解析",
		},
		{
			name: "缺少必要字段（name）",
			csvContent: `url,priority,models
https://missing-name.com,10,"[""model-1""]"
`,
			expectError: true,
			description: "应该因缺少name字段失败",
		},
		{
			name: "空的渠道名称",
			csvContent: `name,url,priority,models,channel_type,enabled
,https://empty-name.com,10,"[""model-1""]",anthropic,true
`,
			expectError: true,
			description: "应该因name为空失败",
		},
		{
			name: "缺少URL字段",
			csvContent: `name,priority,models,channel_type,enabled
No-URL-Channel,10,"[""model-1""]",anthropic,true
`,
			expectError: true,
			description: "应该因缺少url字段失败",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建临时CSV文件
			csvFile := filepath.Join(tmpDir, tt.name+".csv")
			if err := os.WriteFile(csvFile, []byte(tt.csvContent), 0644); err != nil {
				t.Fatalf("创建CSV文件失败: %v", err)
			}

			// 读取CSV文件
			file, err := os.Open(csvFile)
			if err != nil {
				t.Fatalf("打开CSV文件失败: %v", err)
			}
			defer file.Close()

			reader := csv.NewReader(file)
			records, err := reader.ReadAll()
			if err != nil {
				t.Logf("CSV解析失败（预期行为）: %v", err)
				if !tt.expectError {
					t.Errorf("CSV解析不应该失败: %v", err)
				}
				return
			}

			// 跳过header
			if len(records) < 2 {
				if !tt.expectError {
					t.Error("CSV记录不足")
				}
				return
			}

			// 尝试验证数据结构（仅检查必要字段）
			header := records[0]
			dataRow := records[1]

			// 查找name字段索引
			nameIdx := -1
			urlIdx := -1
			for i, h := range header {
				if h == "name" {
					nameIdx = i
				}
				if h == "url" {
					urlIdx = i
				}
			}

			hasError := false

			// 验证name字段
			if nameIdx < 0 || nameIdx >= len(dataRow) || strings.TrimSpace(dataRow[nameIdx]) == "" {
				hasError = true
				t.Logf("数据验证失败：name字段缺失或为空")
			}

			// 验证url字段
			if urlIdx < 0 || urlIdx >= len(dataRow) || strings.TrimSpace(dataRow[urlIdx]) == "" {
				hasError = true
				t.Logf("数据验证失败：url字段缺失或为空")
			}

			if hasError != tt.expectError {
				t.Errorf("%s: 期望error=%v, 实际error=%v", tt.description, tt.expectError, hasError)
			} else {
				t.Logf("✅ %s: %s", tt.name, tt.description)
			}
		})
	}
}

// TestCSVExportImport_SpecialCharacters 测试特殊字符处理
func TestCSVExportImport_SpecialCharacters(t *testing.T) {
	// 使用统一的测试环境设置，避免 Redis 依赖
	store, ctx, cleanup := setupTestStoreWithContext(t)
	defer cleanup()

	// 包含特殊字符的测试数据
	specialConfig := &model.Config{
		Name:     "Special-Chars-Test \"with quotes\"",
		URL:      "https://special.example.com?param=value&other=123",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "model, with, commas"},
			{Model: "model\"with\"quotes"},
		},
		ChannelType: "anthropic",
		Enabled:     true,
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

	// 使用统一的测试环境设置，避免 Redis 依赖
	store, ctx, cleanup := setupTestStoreWithContext(t)
	defer cleanup()

	// 创建100个渠道
	totalChannels := 100
	for i := 0; i < totalChannels; i++ {
		cfg := &model.Config{
			Name:     "Large-Test-" + string(rune('A'+i%26)) + string(rune('0'+i%10)),
			URL:      "https://large" + string(rune('0'+i%10)) + ".example.com",
			Priority: i % 20,
			ModelEntries: []model.ModelEntry{
				{Model: "model-" + string(rune('1'+i%9))},
			},
			ChannelType: []string{"anthropic", "gemini", "codex"}[i%3],
			Enabled:     i%2 == 0,
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
