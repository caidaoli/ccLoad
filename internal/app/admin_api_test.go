package app

import (
	"bytes"
	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== Admin API 集成测试 ====================

func init() {
	// 测试环境使用测试模式
	gin.SetMode(gin.TestMode)
}

// TestAdminAPI_ExportChannelsCSV 测试CSV导出功能
func TestAdminAPI_ExportChannelsCSV(t *testing.T) {
	// 创建测试环境
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// 先创建测试渠道
	ctx := context.Background()
	testChannels := []*model.Config{
		{
			Name:        "Test-Export-1",
			URL:         "https://api1.example.com",
			Priority:    10,
			Models:      []string{"model-1"},
			ChannelType: "anthropic",
			Enabled:     true,
		},
		{
			Name:        "Test-Export-2",
			URL:         "https://api2.example.com",
			Priority:    5,
			Models:      []string{"model-2"},
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
			KeyStrategy: "sequential",
		}
		if err := server.store.CreateAPIKey(ctx, apiKey); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	// 创建Gin测试上下文
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/channels/export", nil)

	// 调用handler
	server.handleExportChannelsCSV(c)

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

	expectedHeaders := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "key_strategy", "enabled"}
	if len(header) != len(expectedHeaders) {
		t.Errorf("Header字段数量不匹配: 期望 %d, 实际: %d\nHeader: %v", len(expectedHeaders), len(header), header)
	}

	for i, expected := range expectedHeaders {
		if i >= len(header) || header[i] != expected {
			t.Errorf("Header[%d] 期望 %s, 实际: %s", i, expected, header[i])
		}
	}

	// 验证数据行（应该有10个字段）
	if len(records[1]) < 10 {
		t.Errorf("数据行字段不足，期望至少10个字段，实际: %d", len(records[1]))
	}

	t.Logf("✅ CSV导出成功，共 %d 行记录（含header）", len(records))
	t.Logf("   CSV Header: %v", header)
	t.Logf("   第一行数据: %v", records[1])
}

// TestAdminAPI_ImportChannelsCSV 测试CSV导入功能
func TestAdminAPI_ImportChannelsCSV(t *testing.T) {
	// 创建测试环境
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// 创建测试CSV文件（注意：列名是api_key而不是api_keys）
	csvContent := `name,url,priority,models,model_redirects,channel_type,enabled,api_key,key_strategy
Import-Test-1,https://import1.example.com,10,test-model-1,{},anthropic,true,sk-import-key-1,sequential
Import-Test-2,https://import2.example.com,5,"test-model-2,test-model-3","{""old"":""new""}",gemini,false,sk-import-key-2,round_robin
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
	writer.Close()

	// 创建Gin测试上下文
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/channels/import", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())

	// 调用handler
	server.handleImportChannelsCSV(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 200, 实际 %d, 响应: %s", w.Code, w.Body.String())
	}

	// 解析为ChannelImportSummary结构
	var summary ChannelImportSummary
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析响应失败: %v, 响应内容: %s", err, w.Body.String())
	}

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
	}

	t.Logf("✅ CSV导入成功，导入 %d 条记录 (Created: %d, Updated: %d)", totalImported, summary.Created, summary.Updated)
	t.Logf("   导入的渠道: %v", importedConfigs)
}

// TestAdminAPI_ExportImportRoundTrip 测试完整的导出-导入循环
func TestAdminAPI_ExportImportRoundTrip(t *testing.T) {
	// 创建测试环境
	server, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 步骤1：创建原始测试数据
	originalConfig := &model.Config{
		Name:           "RoundTrip-Test",
		URL:            "https://roundtrip.example.com",
		Priority:       15,
		Models:         []string{"model-a", "model-b"},
		ModelRedirects: map[string]string{"old-model": "new-model"},
		ChannelType:    "anthropic",
		Enabled:        true,
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
			KeyStrategy: "sequential",
		},
		{
			ChannelID:   created.ID,
			KeyIndex:    1,
			APIKey:      "sk-roundtrip-key-2",
			KeyStrategy: "sequential",
		},
	}

	for _, key := range apiKeys {
		if err := server.store.CreateAPIKey(ctx, key); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	// 步骤2：导出CSV
	exportW := httptest.NewRecorder()
	exportC, _ := gin.CreateTestContext(exportW)
	exportC.Request = httptest.NewRequest(http.MethodGet, "/admin/channels/export", nil)
	server.handleExportChannelsCSV(exportC)

	if exportW.Code != http.StatusOK {
		t.Fatalf("导出失败，状态码: %d", exportW.Code)
	}

	exportedCSV := exportW.Body.Bytes()
	t.Logf("✅ 导出CSV成功，大小: %d bytes", len(exportedCSV))

	// 步骤3：删除原始数据
	if err := server.store.DeleteConfig(ctx, created.ID); err != nil {
		t.Fatalf("删除原始渠道失败: %v", err)
	}

	// 步骤4：重新导入CSV
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "roundtrip.csv")
	part.Write(exportedCSV)
	writer.Close()

	importW := httptest.NewRecorder()
	importC, _ := gin.CreateTestContext(importW)
	importC.Request = httptest.NewRequest(http.MethodPost, "/admin/channels/import", body)
	importC.Request.Header.Set("Content-Type", writer.FormDataContentType())
	server.handleImportChannelsCSV(importC)

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

	if len(restoredConfig.Models) != len(originalConfig.Models) {
		t.Errorf("Models数量不匹配: 期望 %d, 实际 %d", len(originalConfig.Models), len(restoredConfig.Models))
	}

	if len(restoredConfig.ModelRedirects) != len(originalConfig.ModelRedirects) {
		t.Errorf("ModelRedirects数量不匹配: 期望 %d, 实际 %d", len(originalConfig.ModelRedirects), len(restoredConfig.ModelRedirects))
	}

	// 验证API Keys
	restoredKeys, err := server.store.GetAPIKeys(ctx, restoredConfig.ID)
	if err != nil {
		t.Fatalf("查询恢复的API Keys失败: %v", err)
	}

	if len(restoredKeys) != len(apiKeys) {
		t.Errorf("API Keys数量不匹配: 期望 %d, 实际 %d", len(apiKeys), len(restoredKeys))
	}

	t.Logf("✅ 导出-导入循环测试通过")
	t.Logf("   原始渠道ID: %d", created.ID)
	t.Logf("   恢复渠道ID: %d", restoredConfig.ID)
	t.Logf("   API Keys: %d → %d", len(apiKeys), len(restoredKeys))
}

// ==================== 辅助函数 ====================

// setupTestServer 创建测试服务器环境
func setupTestServer(t *testing.T) (*Server, func()) {
	// 使用内存数据库
	os.Setenv("CCLOAD_USE_MEMORY_DB", "true")
	defer os.Unsetenv("CCLOAD_USE_MEMORY_DB")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := sqlite.NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	server := &Server{
		store:       store,
		keySelector: NewKeySelector(store, nil),
	}

	cleanup := func() {
		if err := store.Close(); err != nil {
			t.Logf("关闭数据库失败: %v", err)
		}
	}

	return server, cleanup
}

// ==================== 边界条件测试 ====================

// TestAdminAPI_ImportCSV_InvalidFormat 测试无效CSV格式
func TestAdminAPI_ImportCSV_InvalidFormat(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// 缺少必要字段的CSV
	invalidCSV := `name,url
Test-Invalid,https://invalid.com
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "invalid.csv")
	io.WriteString(part, invalidCSV)
	writer.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/channels/import", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	server.handleImportChannelsCSV(c)

	// 应该返回错误或部分成功
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	t.Logf("✅ 无效格式处理: status=%v, message=%v", resp["status"], resp["message"])
}

// TestAdminAPI_ImportCSV_DuplicateNames 测试重复渠道名称处理
func TestAdminAPI_ImportCSV_DuplicateNames(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 先创建一个渠道
	existing := &model.Config{
		Name:        "Duplicate-Test",
		URL:         "https://existing.com",
		Priority:    10,
		Models:      []string{"model-1"},
		ChannelType: "anthropic",
		Enabled:     true,
	}

	_, err := server.store.CreateConfig(ctx, existing)
	if err != nil {
		t.Fatalf("创建现有渠道失败: %v", err)
	}

	// 尝试导入同名渠道
	duplicateCSV := `name,url,priority,models,channel_type,enabled
Duplicate-Test,https://duplicate.com,5,"[""model-2""]",gemini,false
`

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "duplicate.csv")
	io.WriteString(part, duplicateCSV)
	writer.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/channels/import", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	server.handleImportChannelsCSV(c)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	t.Logf("✅ 重复名称处理: status=%v, message=%v", resp["status"], resp["message"])

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
	server, cleanup := setupTestServer(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/channels/export", nil)
	server.handleExportChannelsCSV(c)

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

	t.Logf("✅ 空数据库导出测试通过，CSV行数: %d", len(records))
}

// TestAdminAPI_LargeCSVImport 测试大文件导入性能
func TestAdminAPI_LargeCSVImport(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过性能测试（使用 -short 标志）")
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()

	// 生成大型CSV（100条记录）
	var csvBuilder strings.Builder
	csvBuilder.WriteString("name,url,priority,models,channel_type,enabled\n")

	for i := 0; i < 100; i++ {
		csvBuilder.WriteString(
			"Large-Test-" + string(rune('A'+i%26)) + string(rune('0'+i%10)) + "," +
				"https://large" + string(rune('0'+i%10)) + ".example.com," +
				"10," +
				`"[""model-1""]",` +
				"anthropic," +
				"true\n")
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "large.csv")
	io.WriteString(part, csvBuilder.String())
	writer.Close()

	// 测试导入性能
	startTime := time.Now()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/channels/import", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	server.handleImportChannelsCSV(c)

	duration := time.Since(startTime)

	if w.Code != http.StatusOK {
		t.Fatalf("大文件导入失败，状态码: %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	data := resp["data"].(map[string]any)
	imported := int(data["imported"].(float64))

	t.Logf("✅ 大文件导入测试通过")
	t.Logf("   记录数: %d", imported)
	t.Logf("   耗时: %v", duration)
	t.Logf("   平均速度: %.2f records/sec", float64(imported)/duration.Seconds())

	// 性能断言：100条记录应该在5秒内完成
	if duration > 5*time.Second {
		t.Errorf("导入性能不符合预期，耗时: %v (期望 <5s)", duration)
	}
}
