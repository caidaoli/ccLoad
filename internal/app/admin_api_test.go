package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

// ==================== Admin API 集成测试 ====================

// TestAdminAPI_ExportChannelsCSV 测试CSV导出功能
func TestAdminAPI_ExportChannelsCSV(t *testing.T) {
	// 创建测试环境
	server := newInMemoryServer(t)

	// 先创建测试渠道
	ctx := context.Background()
	testChannels := []*model.Config{
		{
			Name:     "Test-Export-1",
			URL:      "https://api1.example.com",
			Priority: 10,
			ModelEntries: []model.ModelEntry{
				{Model: "model-1", RedirectModel: ""},
			},
			ChannelType: "anthropic",
			Enabled:     true,
		},
		{
			Name:     "Test-Export-2",
			URL:      "https://api2.example.com",
			Priority: 5,
			ModelEntries: []model.ModelEntry{
				{Model: "model-2", RedirectModel: ""},
			},
			ChannelType: "gemini",
			Enabled:     false,
		},
	}

	for _, cfg := range testChannels {
		created, err := server.store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}

		// 创建API Key
		apiKey := &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    0,
			APIKey:      "sk-test-key-" + created.Name,
			KeyStrategy: model.KeyStrategySequential,
		}
		if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{apiKey}); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export", nil))

	// 调用handler
	server.HandleExportChannelsCSV(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d", w.Code)
	}

	// 验证Content-Type
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/csv") {
		t.Errorf("期望 Content-Type 包含 text/csv, 实际: %s", contentType)
	}

	// 验证Content-Disposition
	disposition := w.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "channels-") {
		t.Errorf("期望 Content-Disposition 包含 attachment 和 channels-, 实际: %s", disposition)
	}

	// 解析CSV内容
	csvReader := csv.NewReader(w.Body)
	records, err := csvReader.ReadAll()
	if err != nil {
		t.Fatalf("解析CSV失败: %v", err)
	}

	if len(records) < 3 { // 至少header + 2行数据
		t.Fatalf("期望至少3行记录（含header），实际: %d", len(records))
	}

	// 验证CSV header（实际格式：带UTF-8 BOM + 包含api_key和key_strategy）
	header := records[0]
	// 移除BOM前缀（如果存在）
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}

	expectedHeaders := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "protocol_transforms", "key_strategy", "enabled", "scheduled_check_enabled", "scheduled_check_model"}
	if len(header) != len(expectedHeaders) {
		t.Errorf("Header字段数量不匹配: 期望 %d, 实际: %d\nHeader: %v", len(expectedHeaders), len(header), header)
	}

	for i, expected := range expectedHeaders {
		if i >= len(header) || header[i] != expected {
			t.Errorf("Header[%d] 期望 %s, 实际: %s", i, expected, header[i])
		}
	}

	// 验证数据行（应该有13个字段）
	if len(records[1]) < 13 {
		t.Errorf("数据行字段不足，期望至少13个字段，实际: %d", len(records[1]))
	}
}

func TestAdminAPI_ImportChannelsCSV(t *testing.T) {
	// 创建测试环境
	server := newInMemoryServer(t)

	// 创建测试CSV文件（注意：列名是api_key而不是api_keys）
	csvContent := `name,url,priority,models,model_redirects,channel_type,protocol_transforms,enabled,api_key,key_strategy,scheduled_check_model
Import-Test-1,https://import1.example.com,10,test-model-1,{},anthropic,openai,true,sk-import-key-1,sequential,test-model-1
Import-Test-2,https://import2.example.com,5,"test-model-2,test-model-3","{""old"":""new""}",gemini,"openai,anthropic",false,sk-import-key-2,round_robin,test-model-3
`

	// 创建multipart表单
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// 添加文件字段
	part, err := writer.CreateFormFile("file", "test-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	// [INFO] 修复：使用bytes.NewReader创建新的读取器，避免buffer读取位置问题
	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	// 调用handler
	server.HandleImportChannelsCSV(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	// [INFO] 调试：输出原始响应内容
	t.Logf("原始响应内容: %s", w.Body.String())

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)

	// 验证导入结果
	totalImported := summary.Created + summary.Updated
	if totalImported != 2 {
		t.Errorf("期望导入2条记录，实际: %d (Created: %d, Updated: %d)", totalImported, summary.Created, summary.Updated)
	}

	// 输出完整的summary信息用于调试
	t.Logf("导入Summary: Created=%d, Updated=%d, Skipped=%d, Processed=%d",
		summary.Created, summary.Updated, summary.Skipped, summary.Processed)

	// 如果有错误，输出错误信息
	if len(summary.Errors) > 0 {
		t.Logf("导入过程中的错误: %v", summary.Errors)
	}

	// 验证数据库中的数据（数据库中的实际结果）
	ctx := context.Background()
	configs, err := server.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	// 查找导入的渠道
	var importedConfigs []*model.Config
	for _, cfg := range configs {
		if strings.HasPrefix(cfg.Name, "Import-Test-") {
			importedConfigs = append(importedConfigs, cfg)
		}
	}

	if len(importedConfigs) != 2 {
		t.Errorf("数据库中应有2个导入的渠道，实际: %d", len(importedConfigs))
	}

	// 验证API Keys是否正确导入
	for _, cfg := range importedConfigs {
		keys, err := server.store.GetAPIKeys(ctx, cfg.ID)
		if err != nil {
			t.Errorf("查询API Keys失败 (渠道 %s): %v", cfg.Name, err)
			continue
		}

		if len(keys) != 1 {
			t.Errorf("渠道 %s 应有1个API Key，实际: %d", cfg.Name, len(keys))
		}
		if cfg.Name == "Import-Test-1" && cfg.ScheduledCheckModel != "test-model-1" {
			t.Errorf("渠道 %s scheduled_check_model = %q", cfg.Name, cfg.ScheduledCheckModel)
		}
		if cfg.Name == "Import-Test-2" && cfg.ScheduledCheckModel != "test-model-3" {
			t.Errorf("渠道 %s scheduled_check_model = %q", cfg.Name, cfg.ScheduledCheckModel)
		}
		if cfg.Name == "Import-Test-1" && (len(cfg.ProtocolTransforms) != 1 || cfg.ProtocolTransforms[0] != "openai") {
			t.Errorf("渠道 %s protocol_transforms = %#v", cfg.Name, cfg.ProtocolTransforms)
		}
		if cfg.Name == "Import-Test-2" && (len(cfg.ProtocolTransforms) != 2 || cfg.ProtocolTransforms[0] != "anthropic" || cfg.ProtocolTransforms[1] != "openai") {
			t.Errorf("渠道 %s protocol_transforms = %#v", cfg.Name, cfg.ProtocolTransforms)
		}
	}
}

func TestAdminAPI_ImportChannelsCSV_UsesExplicitIDForRename(t *testing.T) {
	server := newInMemoryServer(t)
	ctx := context.Background()

	created, err := server.store.CreateConfig(ctx, &model.Config{
		Name:         "Import-Rename-Source",
		URL:          "https://old-id.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "old-model", RedirectModel: ""}},
		ChannelType:  "openai",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建现有渠道失败: %v", err)
	}
	if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-old-id-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建现有 key 失败: %v", err)
	}

	csvContent := fmt.Sprintf(`id,name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
%d,Import-Rename-Target,https://new-id.example.com,20,new-model,{},openai,true,sk-new-id-key,sequential
,Import-Rename-Brand-New,https://brand-new.example.com,5,brand-model,{},anthropic,true,sk-brand-new,sequential
`, created.ID)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "explicit-id-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Updated != 1 || summary.Created != 1 {
		t.Fatalf("期望更新1条并创建1条，实际 summary=%+v", summary)
	}

	updated, err := server.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询按ID更新后的渠道失败: %v", err)
	}
	if updated.Name != "Import-Rename-Target" {
		t.Fatalf("期望按ID更新名称，实际为 %q", updated.Name)
	}
	if updated.URL != "https://new-id.example.com" {
		t.Fatalf("期望按ID更新 URL，实际为 %q", updated.URL)
	}
	if len(updated.ModelEntries) != 1 || updated.ModelEntries[0].Model != "new-model" {
		t.Fatalf("期望按ID更新模型，实际为 %+v", updated.ModelEntries)
	}
	keys, err := server.store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询按ID更新后的 key 失败: %v", err)
	}
	if len(keys) != 1 || keys[0].APIKey != "sk-new-id-key" {
		t.Fatalf("期望按ID更新 key，实际为 %+v", keys)
	}

	configs, err := server.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}
	newCount := 0
	for _, cfg := range configs {
		if cfg.Name == "Import-Rename-Brand-New" {
			newCount++
			if cfg.ID == created.ID {
				t.Fatalf("新渠道不应复用旧 ID")
			}
		}
		if cfg.Name == "Import-Rename-Source" {
			t.Fatalf("旧名称渠道不应保留为额外记录")
		}
	}
	if newCount != 1 {
		t.Fatalf("期望新增渠道1条，实际: %d", newCount)
	}
}

func TestAdminAPI_ImportChannelsCSV_MissingScheduledCheckColumnPreservesExistingValue(t *testing.T) {
	server := newInMemoryServer(t)
	ctx := context.Background()

	created, err := server.store.CreateConfig(ctx, &model.Config{
		Name:                  "Import-Preserve-Scheduled",
		URL:                   "https://old.example.com",
		Priority:              10,
		ModelEntries:          []model.ModelEntry{{Model: "old-model", RedirectModel: ""}},
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ScheduledCheckModel:   "old-model",
	})
	if err != nil {
		t.Fatalf("创建现有渠道失败: %v", err)
	}
	if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-old-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建现有 key 失败: %v", err)
	}

	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Import-Preserve-Scheduled,https://new.example.com,20,"old-model,new-model",{},openai,true,sk-new-key,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "legacy-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Updated != 1 {
		t.Fatalf("期望更新1条记录，实际 summary=%+v", summary)
	}

	updated, err := server.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询更新后的渠道失败: %v", err)
	}
	if !updated.ScheduledCheckEnabled {
		t.Fatalf("缺少 scheduled_check_enabled 列时应保留旧值 true")
	}
	if updated.ScheduledCheckModel != "old-model" {
		t.Fatalf("缺少 scheduled_check_model 列时应保留旧值 old-model，实际为 %q", updated.ScheduledCheckModel)
	}
	if updated.URL != "https://new.example.com" {
		t.Fatalf("期望 URL 已更新，实际为 %s", updated.URL)
	}
	if len(updated.ModelEntries) != 2 || updated.ModelEntries[0].Model != "old-model" || updated.ModelEntries[1].Model != "new-model" {
		t.Fatalf("期望模型已更新，实际为 %+v", updated.ModelEntries)
	}
	keys, err := server.store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询更新后的 key 失败: %v", err)
	}
	if len(keys) != 1 || keys[0].APIKey != "sk-new-key" {
		t.Fatalf("期望 key 已更新，实际为 %+v", keys)
	}
}

func TestAdminAPI_ImportChannelsCSV_MissingScheduledCheckColumnClearsInvalidLegacyValue(t *testing.T) {
	server := newInMemoryServer(t)
	ctx := context.Background()

	created, err := server.store.CreateConfig(ctx, &model.Config{
		Name:                  "Import-Clear-Scheduled",
		URL:                   "https://old.example.com",
		Priority:              10,
		ModelEntries:          []model.ModelEntry{{Model: "old-model", RedirectModel: ""}},
		ChannelType:           "openai",
		Enabled:               true,
		ScheduledCheckEnabled: true,
		ScheduledCheckModel:   "old-model",
	})
	if err != nil {
		t.Fatalf("创建现有渠道失败: %v", err)
	}
	if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-old-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建现有 key 失败: %v", err)
	}

	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Import-Clear-Scheduled,https://new.example.com,20,new-model,{},openai,true,sk-new-key,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "legacy-import-clear.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Updated != 1 {
		t.Fatalf("期望更新1条记录，实际 summary=%+v", summary)
	}

	updated, err := server.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询更新后的渠道失败: %v", err)
	}
	if updated.ScheduledCheckModel != "" {
		t.Fatalf("缺少 scheduled_check_model 列且旧值失效时应清空，实际为 %q", updated.ScheduledCheckModel)
	}
}

func TestAdminAPI_ImportChannelsCSV_InvalidURLRejected(t *testing.T) {
	server := newInMemoryServer(t)

	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Bad-URL,https://bad.example.com/v1,10,test-model,{},anthropic,true,sk-import-key-1,sequential
Good-URL,https://good.example.com,10,test-model,{},anthropic,true,sk-import-key-2,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)

	imported := summary.Created + summary.Updated
	if imported != 1 {
		t.Fatalf("期望导入1条记录，实际: %d (Created: %d, Updated: %d, Skipped: %d, Errors: %v)",
			imported, summary.Created, summary.Updated, summary.Skipped, summary.Errors)
	}
	if summary.Skipped != 1 {
		t.Fatalf("期望Skipped=1，实际: %d (Errors: %v)", summary.Skipped, summary.Errors)
	}
	if len(summary.Errors) == 0 {
		t.Fatalf("期望有错误信息，但为空")
	}

	ctx := context.Background()
	configs, err := server.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	var hasBad, hasGood bool
	for _, cfg := range configs {
		switch cfg.Name {
		case "Bad-URL":
			hasBad = true
		case "Good-URL":
			hasGood = true
		}
	}
	if hasBad {
		t.Fatalf("Bad-URL 不应被导入")
	}
	if !hasGood {
		t.Fatalf("Good-URL 应被导入")
	}
}

func TestAdminAPI_ImportChannelsCSV_InvalidScheduledCheckModelRejected(t *testing.T) {
	server := newInMemoryServer(t)

	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy,scheduled_check_model
Bad-Scheduled-Model,https://bad.example.com,10,test-model,{},anthropic,true,sk-import-key-1,sequential,missing-model
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Skipped != 1 || len(summary.Errors) == 0 {
		t.Fatalf("期望该行被跳过并返回错误，实际 summary=%+v", summary)
	}
	if !strings.Contains(summary.Errors[0], "scheduled_check_model") {
		t.Fatalf("期望错误包含 scheduled_check_model，实际: %v", summary.Errors)
	}
}

func TestAdminAPI_ImportChannelsCSV_InvalidProtocolTransformsRejected(t *testing.T) {
	server := newInMemoryServer(t)

	csvContent := `name,url,priority,models,model_redirects,channel_type,protocol_transforms,enabled,api_key,key_strategy
Bad-Transforms,https://bad.example.com,10,test-model,{},anthropic,"openai,gemini,openai",true,sk-import-key-1,sequential
Good-Transforms,https://good.example.com,10,test-model,{},anthropic,openai,true,sk-import-key-2,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test-import.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Skipped != 1 || len(summary.Errors) == 0 {
		t.Fatalf("期望非法 protocol_transforms 行被跳过并返回错误，实际 summary=%+v", summary)
	}
	if !strings.Contains(summary.Errors[0], "protocol_transforms") {
		t.Fatalf("期望错误包含 protocol_transforms，实际: %v", summary.Errors)
	}

	ctx := context.Background()
	configs, err := server.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	var hasBad, hasGood bool
	for _, cfg := range configs {
		switch cfg.Name {
		case "Bad-Transforms":
			hasBad = true
		case "Good-Transforms":
			hasGood = true
		}
	}
	if hasBad {
		t.Fatalf("Bad-Transforms 不应被导入")
	}
	if !hasGood {
		t.Fatalf("Good-Transforms 应被导入")
	}
}

func TestAdminAPI_ImportChannelsCSV_PrunesURLSelectorStateForUpdatedChannel(t *testing.T) {
	server := newInMemoryServer(t)
	ctx := context.Background()

	targetCfg, err := server.store.CreateConfig(ctx, &model.Config{
		Name:         "Import-Prune-Target",
		URL:          "https://old-import.example.com\nhttps://keep-import.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		ChannelType:  "anthropic",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建目标渠道失败: %v", err)
	}
	if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   targetCfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-target-import",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建目标渠道 key 失败: %v", err)
	}

	otherCfg, err := server.store.CreateConfig(ctx, &model.Config{
		Name:         "Import-Prune-Other",
		URL:          "https://other-import.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		ChannelType:  "anthropic",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}

	server.urlSelector.RecordLatency(targetCfg.ID, "https://old-import.example.com", 10*time.Millisecond)
	server.urlSelector.RecordLatency(targetCfg.ID, "https://keep-import.example.com", 20*time.Millisecond)
	server.urlSelector.CooldownURL(targetCfg.ID, "https://old-import.example.com")
	server.urlSelector.RecordLatency(otherCfg.ID, "https://other-import.example.com", 30*time.Millisecond)

	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Import-Prune-Target,https://keep-import.example.com,10,m1,{},anthropic,true,sk-target-import,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "import-prune.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, csvContent); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)

	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	var summary ChannelImportSummary
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &summary)
	if summary.Updated < 1 {
		t.Fatalf("期望至少更新1条记录，实际 summary=%+v", summary)
	}

	if _, ok := server.urlSelector.latencies[urlKey{channelID: targetCfg.ID, url: "https://old-import.example.com"}]; ok {
		t.Fatalf("期望导入更新后旧URL latency状态被清理")
	}
	if _, ok := server.urlSelector.cooldowns[urlKey{channelID: targetCfg.ID, url: "https://old-import.example.com"}]; ok {
		t.Fatalf("期望导入更新后旧URL cooldown状态被清理")
	}
	if _, ok := server.urlSelector.latencies[urlKey{channelID: targetCfg.ID, url: "https://keep-import.example.com"}]; !ok {
		t.Fatalf("期望保留更新后URL的状态")
	}
	if _, ok := server.urlSelector.latencies[urlKey{channelID: otherCfg.ID, url: "https://other-import.example.com"}]; !ok {
		t.Fatalf("期望其他渠道状态不受影响")
	}
}

// TestAdminAPI_ExportImportRoundTrip 测试完整的导出-导入循环
func TestAdminAPI_ExportImportRoundTrip(t *testing.T) {
	// 创建测试环境
	server := newInMemoryServer(t)

	ctx := context.Background()

	// 步骤1：创建原始测试数据
	originalConfig := &model.Config{
		Name:     "RoundTrip-Test",
		URL:      "https://roundtrip.example.com",
		Priority: 15,
		ModelEntries: []model.ModelEntry{
			{Model: "model-a", RedirectModel: ""},
			{Model: "model-b", RedirectModel: ""},
			{Model: "old-model", RedirectModel: "new-model"},
		},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	created, err := server.store.CreateConfig(ctx, originalConfig)
	if err != nil {
		t.Fatalf("创建原始渠道失败: %v", err)
	}

	// 创建API Keys
	apiKeys := []*model.APIKey{
		{
			ChannelID:   created.ID,
			KeyIndex:    0,
			APIKey:      "sk-roundtrip-key-1",
			KeyStrategy: model.KeyStrategySequential,
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    1,
			APIKey:      "sk-roundtrip-key-2",
			KeyStrategy: model.KeyStrategySequential,
		},
	}

	if err := server.store.CreateAPIKeysBatch(ctx, apiKeys); err != nil {
		t.Fatalf("创建API Keys失败: %v", err)
	}

	// 步骤2：导出CSV
	exportC, exportW := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export", nil))
	server.HandleExportChannelsCSV(exportC)

	if exportW.Code != http.StatusOK {
		t.Fatalf("导出失败，状态码: %d", exportW.Code)
	}

	exportedCSV := exportW.Body.Bytes()

	// 步骤3：删除原始数据
	if err := server.store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("删除原始渠道失败: %v", err)
	}

	// 步骤4：重新导入CSV
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "roundtrip.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := part.Write(exportedCSV); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	// [INFO] 修复：使用bytes.NewReader创建新的读取器
	importReq := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	importReq.Header.Set("Content-Type", writer.FormDataContentType())
	importC, importW := newTestContext(t, importReq)
	server.HandleImportChannelsCSV(importC)

	if importW.Code != http.StatusOK {
		t.Fatalf("导入失败，状态码: %d, 响应: %s", importW.Code, importW.Body.String())
	}

	// 步骤5：验证数据完整性
	configs, err := server.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("查询渠道列表失败: %v", err)
	}

	var restoredConfig *model.Config
	for _, cfg := range configs {
		if cfg.Name == "RoundTrip-Test" {
			restoredConfig = cfg
			break
		}
	}

	if restoredConfig == nil {
		t.Fatalf("未找到恢复的渠道 RoundTrip-Test")
	}

	// 验证字段完整性
	if restoredConfig.URL != originalConfig.URL {
		t.Errorf("URL不匹配: 期望 %s, 实际 %s", originalConfig.URL, restoredConfig.URL)
	}

	if restoredConfig.Priority != originalConfig.Priority {
		t.Errorf("Priority不匹配: 期望 %d, 实际 %d", originalConfig.Priority, restoredConfig.Priority)
	}

	if len(restoredConfig.ModelEntries) != len(originalConfig.ModelEntries) {
		t.Errorf("ModelEntries数量不匹配: 期望 %d, 实际 %d", len(originalConfig.ModelEntries), len(restoredConfig.ModelEntries))
	}

	// 验证API Keys
	restoredKeys, err := server.store.GetAPIKeys(ctx, restoredConfig.ID)
	if err != nil {
		t.Fatalf("查询恢复的API Keys失败: %v", err)
	}

	if len(restoredKeys) != len(apiKeys) {
		t.Errorf("API Keys数量不匹配: 期望 %d, 实际 %d", len(apiKeys), len(restoredKeys))
	}
}

// ==================== 边界条件测试 ====================

// TestAdminAPI_ImportCSV_InvalidFormat 测试无效CSV格式
func TestAdminAPI_ImportCSV_InvalidFormat(t *testing.T) {
	server := newInMemoryServer(t)

	// 缺少必要字段的CSV
	invalidCSV := `name,url
Test-Invalid,https://invalid.com
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "invalid.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, invalidCSV); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	// [INFO] 修复：使用bytes.NewReader创建新的读取器
	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)
	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望状态码 400, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if resp.Success {
		t.Fatalf("期望 success=false, 实际=true, data=%s", string(resp.Data))
	}
	if !strings.Contains(resp.Error, "缺少必需列") {
		t.Fatalf("期望错误包含“缺少必需列”，实际 error=%q", resp.Error)
	}
}

// TestAdminAPI_ImportCSV_DuplicateNames 测试重复渠道名称处理
func TestAdminAPI_ImportCSV_DuplicateNames(t *testing.T) {
	server := newInMemoryServer(t)

	ctx := context.Background()

	// 先创建一个渠道
	existing := &model.Config{
		Name:         "Duplicate-Test",
		URL:          "https://existing.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		ChannelType:  "anthropic",
		Enabled:      true,
	}

	_, err := server.store.CreateConfig(ctx, existing)
	if err != nil {
		t.Fatalf("创建现有渠道失败: %v", err)
	}

	// 尝试导入同名渠道 - [INFO] 修复：添加必需的api_key和key_strategy列
	duplicateCSV := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Duplicate-Test,https://duplicate.com,5,model-2,{},gemini,false,sk-duplicate-key,sequential
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "duplicate.csv")
	if err != nil {
		t.Fatalf("创建表单文件字段失败: %v", err)
	}
	if _, err := io.WriteString(part, duplicateCSV); err != nil {
		t.Fatalf("写入CSV内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭writer失败: %v", err)
	}

	// [INFO] 修复：使用bytes.NewReader创建新的读取器
	req := newRequest(http.MethodPost, "/admin/channels/import", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)
	server.HandleImportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}
	resp := mustParseAPIResponse[ChannelImportSummary](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.Created != 0 || resp.Data.Updated != 1 || resp.Data.Skipped != 0 || resp.Data.Processed != 1 {
		t.Fatalf("summary=%+v, want created=0 updated=1 skipped=0 processed=1", resp.Data)
	}

	// 验证数据库中只有一个渠道
	configs, _ := server.store.ListConfigs(ctx)
	duplicateCount := 0
	for _, cfg := range configs {
		if cfg.Name == "Duplicate-Test" {
			duplicateCount++
		}
	}

	if duplicateCount > 1 {
		t.Errorf("数据库中不应有重复的渠道名称，实际数量: %d", duplicateCount)
	}
}

// TestAdminAPI_ExportCSV_EmptyDatabase 测试空数据库导出
func TestAdminAPI_ExportCSV_EmptyDatabase(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export", nil))
	server.HandleExportChannelsCSV(c)

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d", w.Code)
	}

	// 解析CSV
	csvReader := csv.NewReader(w.Body)
	records, err := csvReader.ReadAll()
	if err != nil {
		t.Fatalf("解析CSV失败: %v", err)
	}

	// 空数据库应该只有header行
	if len(records) != 1 {
		t.Errorf("空数据库导出应该只有1行（header），实际: %d", len(records))
	}
}

// TestHealthEndpoint 测试健康检查端点
func TestHealthEndpoint(t *testing.T) {
	server := newInMemoryServer(t)

	r := gin.New()
	server.SetupRoutes(r)

	// 测试健康检查端点
	w := serveHTTP(t, r, newRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200，实际: %d, 响应: %s", w.Code, w.Body.String())
	}

	type healthData struct {
		Status string `json:"status"`
	}
	resp := mustParseAPIResponse[healthData](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.Status != "ok" {
		t.Fatalf("期望 status='ok'，实际: %v", resp.Data.Status)
	}
}
