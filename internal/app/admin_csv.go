package app

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== CSV导入导出 ====================
// 从admin.go拆分CSV功能,遵循SRP原则

// handleExportChannelsCSV 导出渠道为CSV
// GET /admin/channels/export
func (s *Server) HandleExportChannelsCSV(c *gin.Context) {
	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 批量查询所有API Keys,消除N+1问题(100渠道从100次查询降为1次)
	var allAPIKeys map[int64][]*model.APIKey
	if sqliteStore, ok := s.store.(*sqlite.SQLiteStore); ok {
		allAPIKeys, err = sqliteStore.GetAllAPIKeys(c.Request.Context())
		if err != nil {
			util.SafePrintf("⚠️  警告: 批量查询API Keys失败: %v", err)
			allAPIKeys = make(map[int64][]*model.APIKey) // 降级:使用空map
		}
	} else {
		// 兼容其他Store实现(虽然目前只有SQLite)
		allAPIKeys = make(map[int64][]*model.APIKey)
	}

	buf := &bytes.Buffer{}
	// 添加 UTF-8 BOM,兼容 Excel 等工具
	buf.WriteString("\ufeff")

	writer := csv.NewWriter(buf)
	defer writer.Flush()

	header := []string{"id", "name", "api_key", "url", "priority", "models", "model_redirects", "channel_type", "key_strategy", "enabled"}
	if err := writer.Write(header); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	for _, cfg := range cfgs {
		// 从预加载的map中获取API Keys,O(1)查找
		apiKeys := allAPIKeys[cfg.ID]

		// 格式化API Keys为逗号分隔字符串
		apiKeyStrs := make([]string, 0, len(apiKeys))
		for _, key := range apiKeys {
			apiKeyStrs = append(apiKeyStrs, key.APIKey)
		}
		apiKeyStr := strings.Join(apiKeyStrs, ",")

		// 获取Key策略(从第一个Key)
		keyStrategy := "sequential" // 默认值
		if len(apiKeys) > 0 && apiKeys[0].KeyStrategy != "" {
			keyStrategy = apiKeys[0].KeyStrategy
		}

		// 序列化模型重定向为JSON字符串
		modelRedirectsJSON := "{}"
		if len(cfg.ModelRedirects) > 0 {
			if jsonBytes, err := sonic.Marshal(cfg.ModelRedirects); err == nil {
				modelRedirectsJSON = string(jsonBytes)
			}
		}

		record := []string{
			strconv.FormatInt(cfg.ID, 10),
			cfg.Name,
			apiKeyStr,
			cfg.URL,
			strconv.Itoa(cfg.Priority),
			strings.Join(cfg.Models, ","),
			modelRedirectsJSON,
			cfg.GetChannelType(), // 使用GetChannelType确保默认值
			keyStrategy,
			strconv.FormatBool(cfg.Enabled),
		}
		if err := writer.Write(record); err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	filename := fmt.Sprintf("channels-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, buf.String())
}

// handleImportChannelsCSV 导入渠道CSV
// POST /admin/channels/import
func (s *Server) HandleImportChannelsCSV(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "缺少上传文件")
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	defer src.Close()

	reader := csv.NewReader(src)
	reader.TrimLeadingSpace = true

	headerRow, err := reader.Read()
	if err == io.EOF {
		RespondErrorMsg(c, http.StatusBadRequest, "CSV内容为空")
		return
	}
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	columnIndex := buildCSVColumnIndex(headerRow)
	required := []string{"name", "api_key", "url", "models"}
	for _, key := range required {
		if _, ok := columnIndex[key]; !ok {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("缺少必需列: %s", key))
			return
		}
	}

	summary := ChannelImportSummary{}
	lineNo := 1

	// 批量收集有效记录,最后一次性导入(减少数据库往返)
	validChannels := make([]*model.ChannelWithKeys, 0, 100) // 预分配容量,减少扩容

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		lineNo++

		if err != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行读取失败: %v", lineNo, err))
			summary.Skipped++
			continue
		}

		if isCSVRecordEmpty(record) {
			summary.Skipped++
			continue
		}

		fetch := func(key string) string {
			idx, ok := columnIndex[key]
			if !ok || idx >= len(record) {
				return ""
			}
			return strings.TrimSpace(record[idx])
		}

		name := fetch("name")
		apiKey := fetch("api_key")
		url := fetch("url")
		modelsRaw := fetch("models")
		modelRedirectsRaw := fetch("model_redirects")
		channelType := fetch("channel_type")
		keyStrategy := fetch("key_strategy")

		if name == "" || apiKey == "" || url == "" || modelsRaw == "" {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行缺少必填字段", lineNo))
			summary.Skipped++
			continue
		}

		// 渠道类型规范化与校验(openai → codex,空值 → anthropic)
		channelType = util.NormalizeChannelType(channelType)
		if !util.IsValidChannelType(channelType) {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行渠道类型无效: %s(仅支持anthropic/codex/gemini)", lineNo, channelType))
			summary.Skipped++
			continue
		}

		// 验证Key使用策略(可选字段,默认sequential)
		if keyStrategy == "" {
			keyStrategy = "sequential" // 默认值
		} else if keyStrategy != "sequential" && keyStrategy != "round_robin" {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行Key使用策略无效: %s(仅支持sequential/round_robin)", lineNo, keyStrategy))
			summary.Skipped++
			continue
		}

		models := parseImportModels(modelsRaw)
		if len(models) == 0 {
			summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行模型格式无效", lineNo))
			summary.Skipped++
			continue
		}

		// 解析模型重定向(可选字段)
		var modelRedirects map[string]string
		if modelRedirectsRaw != "" && modelRedirectsRaw != "{}" {
			if err := sonic.Unmarshal([]byte(modelRedirectsRaw), &modelRedirects); err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行模型重定向格式错误: %v", lineNo, err))
				summary.Skipped++
				continue
			}
		}

		priority := 0
		if pRaw := fetch("priority"); pRaw != "" {
			p, err := strconv.Atoi(pRaw)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行优先级格式错误: %v", lineNo, err))
				summary.Skipped++
				continue
			}
			priority = p
		}

		enabled := true
		if eRaw := fetch("enabled"); eRaw != "" {
			if val, ok := parseImportEnabled(eRaw); ok {
				enabled = val
			} else {
				summary.Errors = append(summary.Errors, fmt.Sprintf("第%d行启用状态格式错误: %s", lineNo, eRaw))
				summary.Skipped++
				continue
			}
		}

		// 构建渠道配置
		cfg := &model.Config{
			Name:           name,
			URL:            url,
			Priority:       priority,
			Models:         models,
			ModelRedirects: modelRedirects,
			ChannelType:    channelType,
			Enabled:        enabled,
		}

		// 解析并构建API Keys
		apiKeyList := util.ParseAPIKeys(apiKey)
		apiKeys := make([]model.APIKey, len(apiKeyList))
		for i, key := range apiKeyList {
			apiKeys[i] = model.APIKey{
				KeyIndex:    i,
				APIKey:      key,
				KeyStrategy: keyStrategy,
			}
		}

		// 收集有效记录
		validChannels = append(validChannels, &model.ChannelWithKeys{
			Config:  cfg,
			APIKeys: apiKeys,
		})
	}

	// 批量导入所有有效记录(单事务 + 预编译语句)
	if len(validChannels) > 0 {
		if sqliteStore, ok := s.store.(*sqlite.SQLiteStore); ok {
			created, updated, err := sqliteStore.ImportChannelBatch(c.Request.Context(), validChannels)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("批量导入失败: %v", err))
				RespondJSON(c, http.StatusInternalServerError, summary)
				return
			}
			summary.Created = created
			summary.Updated = updated
		} else {
			// 降级处理:如果不是SQLiteStore,回退到逐条导入(保持兼容性)
			summary.Errors = append(summary.Errors, "不支持的存储类型,批量导入功能不可用")
			RespondJSON(c, http.StatusInternalServerError, summary)
			return
		}
	}

	summary.Processed = summary.Created + summary.Updated + summary.Skipped

	if len(validChannels) > 0 {
		s.InvalidateChannelListCache()
		s.InvalidateAllAPIKeysCache()
		s.invalidateCooldownCache()
	}

	// 导入完成后,检查Redis同步状态(批量导入方法会自动触发同步)
	summary.RedisSyncEnabled = false
	if sqliteStore, ok := s.store.(*sqlite.SQLiteStore); ok && sqliteStore.IsRedisEnabled() {
		summary.RedisSyncEnabled = true
		summary.RedisSyncSuccess = true // 批量导入方法已自动同步
		// 获取当前渠道总数作为同步数量
		if configs, err := s.store.ListConfigs(c.Request.Context()); err == nil {
			summary.RedisSyncedChannels = len(configs)
		}
	}

	RespondJSON(c, http.StatusOK, summary)
}

// ==================== CSV辅助函数 ====================

// buildCSVColumnIndex 构建CSV列索引映射
func buildCSVColumnIndex(header []string) map[string]int {
	index := make(map[string]int, len(header))
	for i, col := range header {
		norm := normalizeCSVHeader(col)
		if norm == "" {
			continue
		}
		index[norm] = i
	}
	return index
}

// normalizeCSVHeader 规范化CSV列名
func normalizeCSVHeader(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	lower := strings.ToLower(trimmed)
	switch lower {
	case "apikey", "api-key", "api key":
		return "api_key"
	case "model", "model_list", "model(s)":
		return "models"
	case "model_redirect", "model-redirects", "modelredirects", "redirects":
		return "model_redirects"
	case "key_strategy", "key-strategy", "keystrategy", "策略", "使用策略":
		return "key_strategy"
	case "status":
		return "enabled"
	default:
		return lower
	}
}

// isCSVRecordEmpty 检查CSV记录是否为空
func isCSVRecordEmpty(record []string) bool {
	for _, cell := range record {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// parseImportModels 解析CSV中的模型列表
func parseImportModels(raw string) []string {
	if raw == "" {
		return nil
	}
	splitter := func(r rune) bool {
		switch r {
		case ',', ';', '|', '\n', '\r', '\t':
			return true
		default:
			return false
		}
	}
	parts := strings.FieldsFunc(raw, splitter)
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		clean := strings.TrimSpace(p)
		if clean == "" {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// parseImportEnabled 解析CSV中的启用状态
func parseImportEnabled(raw string) (bool, bool) {
	val := strings.TrimSpace(strings.ToLower(raw))
	switch val {
	case "1", "true", "yes", "y", "启用", "enabled", "on":
		return true, true
	case "0", "false", "no", "n", "禁用", "disabled", "off":
		return false, true
	default:
		return false, false
	}
}
