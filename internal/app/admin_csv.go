package app

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// ==================== CSV导入导出 ====================
// 从admin.go拆分CSV功能,遵循SRP原则

// parseChannelIDsQuery 解析 ids=1,2,3 形式的渠道筛选参数。
// 空参数返回 nil（不过滤）；含非法 ID 时直接报错，避免静默导出到错误的集合。
func parseChannelIDsQuery(raw string) (map[int64]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	ids := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid channel id: %s", part)
		}
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("ids cannot be empty")
	}
	return ids, nil
}

// HandleExportChannelsCSV 导出渠道为CSV
// GET /admin/channels/export
// ids=1,2,3 只导出指定渠道；省略时按列表筛选条件导出。
func (s *Server) HandleExportChannelsCSV(c *gin.Context) {
	selectedIDs, err := parseChannelIDsQuery(c.Query("ids"))
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	cfgs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	cfgs = applyChannelListFilters(
		cfgs,
		c,
		s.loadChannelCooldownSnapshot(c.Request.Context()),
		time.Now(),
	)
	if selectedIDs != nil {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			_, selected := selectedIDs[cfg.ID]
			return selected
		})
	}

	// 批量查询所有API Keys，消除 N+1
	allAPIKeys, err := s.store.GetAllAPIKeys(c.Request.Context())
	if err != nil {
		log.Printf("[WARN] 批量查询API Keys失败: %v", err)
		allAPIKeys = make(map[int64][]*model.APIKey) // 降级:使用空map
	}

	buf := &bytes.Buffer{}
	// 添加 UTF-8 BOM,兼容 Excel 等工具
	buf.WriteString("\ufeff")

	writer := csv.NewWriter(buf)
	defer writer.Flush()

	header := []string{"id", "name", "api_key", "urls", "priority", "rpm_limit", "max_concurrency", "models", "model_redirects", "protocol_transform_mode", "key_strategy", "enabled", "scheduled_check_enabled", "scheduled_check_model", "cooldown_detection_rules", "retry_other_keys_on_failure", "auth_type", "oauth_credential", "websockets"}
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
		keyStrategy := model.KeyStrategySequential // 默认值
		if len(apiKeys) > 0 && apiKeys[0].KeyStrategy != "" {
			keyStrategy = apiKeys[0].KeyStrategy
		}

		// 序列化模型列表和重定向为CSV兼容格式
		// 格式设计：models用逗号分隔（人类可读+Excel友好），redirects用JSON（结构化数据）
		models := make([]string, 0, len(cfg.ModelEntries))
		redirects := make(map[string]string)
		for _, entry := range cfg.ModelEntries {
			models = append(models, entry.Model)
			if entry.RedirectModel != "" {
				redirects[entry.Model] = entry.RedirectModel
			}
		}

		modelRedirectsJSON := "{}"
		if len(redirects) > 0 {
			if jsonBytes, err := sonic.Marshal(redirects); err == nil {
				modelRedirectsJSON = string(jsonBytes)
			}
		}
		cooldownDetectionRulesJSON := ""
		if cfg.CooldownDetectionRules != nil && !cfg.CooldownDetectionRules.IsEmpty() {
			jsonBytes, err := sonic.Marshal(cfg.CooldownDetectionRules)
			if err != nil {
				RespondError(c, http.StatusInternalServerError, fmt.Errorf("serialize cooldown detection rules for channel %d: %w", cfg.ID, err))
				return
			}
			cooldownDetectionRulesJSON = string(jsonBytes)
		}
		urlsJSON, err := sonic.Marshal(cfg.URLs)
		if err != nil {
			RespondError(c, http.StatusInternalServerError, fmt.Errorf("serialize urls for channel %d: %w", cfg.ID, err))
			return
		}

		record := []string{
			strconv.FormatInt(cfg.ID, 10),
			cfg.Name,
			apiKeyStr,
			string(urlsJSON),
			strconv.Itoa(cfg.Priority),
			strconv.Itoa(cfg.RPMLimit),
			strconv.Itoa(cfg.MaxConcurrency),
			strings.Join(models, ","),
			modelRedirectsJSON,
			cfg.GetProtocolTransformMode(),
			keyStrategy,
			strconv.FormatBool(cfg.Enabled),
			strconv.FormatBool(cfg.ScheduledCheckEnabled),
			cfg.ScheduledCheckModel,
			cooldownDetectionRulesJSON,
			strconv.FormatBool(cfg.RetryOtherKeysOnFailure),
			cfg.GetAuthType(),
			cfg.OAuthCredential,
			strconv.FormatBool(cfg.Websockets),
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

// HandleImportChannelsCSV 导入渠道CSV
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
	defer func() { _ = src.Close() }()

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
	required := []string{"name", "urls", "models"}
	for _, key := range required {
		if _, ok := columnIndex[key]; !ok {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("缺少必需列: %s", key))
			return
		}
	}

	_, hasScheduledCheckColumn := columnIndex["scheduled_check_enabled"]
	_, hasScheduledCheckModelColumn := columnIndex["scheduled_check_model"]
	_, hasCooldownDetectionRulesColumn := columnIndex["cooldown_detection_rules"]
	_, hasRetryOtherKeysOnFailureColumn := columnIndex["retry_other_keys_on_failure"]
	_, hasWebsocketsColumn := columnIndex["websockets"]
	existingScheduledCheckByName := make(map[string]bool)
	existingScheduledCheckModelByName := make(map[string]string)
	existingCooldownDetectionRulesByName := make(map[string]*model.CooldownDetectionRules)
	existingRetryOtherKeysOnFailureByName := make(map[string]bool)
	existingWebsocketsByName := make(map[string]bool)
	if !hasScheduledCheckColumn || !hasScheduledCheckModelColumn || !hasCooldownDetectionRulesColumn || !hasRetryOtherKeysOnFailureColumn || !hasWebsocketsColumn {
		existingConfigs, err := s.store.ListConfigs(c.Request.Context())
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		for _, cfg := range existingConfigs {
			existingScheduledCheckByName[cfg.Name] = cfg.ScheduledCheckEnabled
			existingScheduledCheckModelByName[cfg.Name] = cfg.ScheduledCheckModel
			existingCooldownDetectionRulesByName[cfg.Name] = cfg.CooldownDetectionRules.Clone()
			existingRetryOtherKeysOnFailureByName[cfg.Name] = cfg.RetryOtherKeysOnFailure
			existingWebsocketsByName[cfg.Name] = cfg.Websockets
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

		channel, errMsg, skip := s.parseChannelImportRow(
			record,
			columnIndex,
			lineNo,
			hasScheduledCheckColumn,
			hasScheduledCheckModelColumn,
			hasCooldownDetectionRulesColumn,
			hasRetryOtherKeysOnFailureColumn,
			hasWebsocketsColumn,
			existingScheduledCheckByName,
			existingScheduledCheckModelByName,
			existingCooldownDetectionRulesByName,
			existingRetryOtherKeysOnFailureByName,
			existingWebsocketsByName,
		)
		if skip {
			if errMsg != "" {
				summary.Errors = append(summary.Errors, errMsg)
			}
			summary.Skipped++
			continue
		}

		// 收集有效记录
		validChannels = append(validChannels, channel)
	}

	// 批量导入所有有效记录(单事务 + 预编译语句)
	if len(validChannels) > 0 {
		created, updated, err := s.store.ImportChannelBatch(c.Request.Context(), validChannels)
		if err != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("批量导入失败: %v", err))
			RespondErrorWithData(c, http.StatusInternalServerError, err.Error(), summary)
			return
		}
		summary.Created = created
		summary.Updated = updated

		// 导入会更新渠道URL，立即清理 URLSelector 中失效URL状态，避免旧状态长期残留。
		if s.urlSelector != nil {
			seenIDs := make(map[int64]struct{}, len(validChannels))
			for _, channel := range validChannels {
				if channel == nil || channel.Config == nil || channel.Config.ID <= 0 {
					continue
				}
				seenIDs[channel.Config.ID] = struct{}{}
			}
			for channelID := range seenIDs {
				cfg, getErr := s.store.GetConfig(c.Request.Context(), channelID)
				if getErr != nil || cfg == nil {
					continue
				}
				s.urlSelector.PruneChannel(channelID, cfg.GetURLs())
				// 同步清理数据库中已移除URL的禁用状态记录
				s.cleanupOrphanedURLStates(c.Request.Context(), channelID, cfg.GetURLs())
			}
		}
	}

	summary.Processed = summary.Created + summary.Updated + summary.Skipped

	if len(validChannels) > 0 {
		s.InvalidateChannelListCache()
		s.InvalidateAllAPIKeysCache()
		s.invalidateCooldownCache()
	}

	RespondJSON(c, http.StatusOK, summary)
}

// parseChannelImportRow 解析单行 CSV 记录为渠道配置。
// 返回三态：
//   - skip=true,  errMsg=="": 空行,调用方仅累加 Skipped
//   - skip=true,  errMsg!="": 解析错误,调用方追加 errors 并 Skipped++
//   - skip=false, channel!=nil: 解析成功,调用方追加 validChannels
func (s *Server) parseChannelImportRow(
	record []string,
	columnIndex map[string]int,
	lineNo int,
	hasScheduledCheckColumn bool,
	hasScheduledCheckModelColumn bool,
	hasCooldownDetectionRulesColumn bool,
	hasRetryOtherKeysOnFailureColumn bool,
	hasWebsocketsColumn bool,
	existingScheduledCheckByName map[string]bool,
	existingScheduledCheckModelByName map[string]string,
	existingCooldownDetectionRulesByName map[string]*model.CooldownDetectionRules,
	existingRetryOtherKeysOnFailureByName map[string]bool,
	existingWebsocketsByName map[string]bool,
) (channel *model.ChannelWithKeys, errMsg string, skip bool) {
	if isCSVRecordEmpty(record) {
		return nil, "", true
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
	rawAuthType := fetch("auth_type")
	oauthCredential := fetch("oauth_credential")
	urlsRaw := fetch("urls")
	modelsRaw := fetch("models")
	modelRedirectsRaw := fetch("model_redirects")
	rawProtocolTransformMode := fetch("protocol_transform_mode")
	keyStrategy := fetch("key_strategy")

	authType := model.NormalizeAuthType(rawAuthType)
	if authType == "" {
		return nil, fmt.Sprintf("第%d行认证类型无效: %s", lineNo, rawAuthType), true
	}

	var missing []string
	if name == "" {
		missing = append(missing, "name")
	}
	if authType == model.AuthTypeAPIKey && apiKey == "" {
		missing = append(missing, "api_key")
	}
	if authType != model.AuthTypeAPIKey && oauthCredential == "" {
		missing = append(missing, "oauth_credential")
	}
	if urlsRaw == "" {
		missing = append(missing, "urls")
	}
	if modelsRaw == "" {
		missing = append(missing, "models")
	}
	if len(missing) > 0 {
		return nil, fmt.Sprintf("第%d行缺少必填字段: %s", lineNo, strings.Join(missing, ", ")), true
	}
	if authType == model.AuthTypeAPIKey && oauthCredential != "" {
		return nil, fmt.Sprintf("第%d行 API Key 渠道不能包含 OAuth 凭证", lineNo), true
	}
	if authType != model.AuthTypeAPIKey && apiKey != "" {
		return nil, fmt.Sprintf("第%d行 OAuth 渠道不能包含 API Key", lineNo), true
	}
	if authType != model.AuthTypeAPIKey {
		var err error
		oauthCredential, err = normalizeCSVImportOAuthCredential(authType, oauthCredential)
		if err != nil {
			return nil, fmt.Sprintf("第%d行 OAuth 凭证无效: %v", lineNo, err), true
		}
	}

	var urls model.ChannelURLs
	if err := sonic.Unmarshal([]byte(urlsRaw), &urls); err != nil {
		return nil, fmt.Sprintf("第%d行 urls JSON无效: %v", lineNo, err), true
	}
	urls, err := validateChannelURLConfigs(urls, authType)
	if err != nil {
		return nil, fmt.Sprintf("第%d行URL无效: %v", lineNo, err), true
	}

	protocolTransformMode := model.NormalizeProtocolTransformMode(rawProtocolTransformMode)
	if protocolTransformMode == "" {
		return nil, fmt.Sprintf("第%d行 protocol_transform_mode 无效: %s", lineNo, rawProtocolTransformMode), true
	}

	// 验证Key使用策略(可选字段,默认sequential)
	if keyStrategy == "" {
		keyStrategy = model.KeyStrategySequential // 默认值
	} else if !model.IsValidKeyStrategy(keyStrategy) {
		return nil, fmt.Sprintf("第%d行Key使用策略无效: %s(仅支持sequential/round_robin)", lineNo, keyStrategy), true
	}
	models := parseImportModels(modelsRaw)
	if len(models) == 0 {
		return nil, fmt.Sprintf("第%d行模型格式无效", lineNo), true
	}

	// 解析模型重定向(可选字段)
	var modelRedirects map[string]string
	if modelRedirectsRaw != "" && modelRedirectsRaw != "{}" {
		if err := sonic.Unmarshal([]byte(modelRedirectsRaw), &modelRedirects); err != nil {
			return nil, fmt.Sprintf("第%d行模型重定向格式错误: %v", lineNo, err), true
		}
	}

	priority := 0
	if pRaw := fetch("priority"); pRaw != "" {
		p, err := strconv.Atoi(pRaw)
		if err != nil {
			return nil, fmt.Sprintf("第%d行优先级格式错误: %v", lineNo, err), true
		}
		priority = p
	}

	rpmLimit := 0
	if rpmRaw := fetch("rpm_limit"); rpmRaw != "" {
		parsed, err := strconv.Atoi(rpmRaw)
		if err != nil || parsed < 0 {
			return nil, fmt.Sprintf("第%d行RPM限制格式错误: %s", lineNo, rpmRaw), true
		}
		rpmLimit = parsed
	}

	maxConcurrency := 0
	if raw := fetch("max_concurrency"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return nil, fmt.Sprintf("第%d行并发限制格式错误: %s", lineNo, raw), true
		}
		maxConcurrency = parsed
	}

	enabled := true
	if eRaw := fetch("enabled"); eRaw != "" {
		if val, ok := parseImportEnabled(eRaw); ok {
			enabled = val
		} else {
			return nil, fmt.Sprintf("第%d行启用状态格式错误: %s", lineNo, eRaw), true
		}
	}

	scheduledCheckEnabled := existingScheduledCheckByName[name]
	if raw := fetch("scheduled_check_enabled"); raw != "" {
		if val, ok := parseImportEnabled(raw); ok {
			scheduledCheckEnabled = val
		} else {
			return nil, fmt.Sprintf("第%d行定时检测开关格式错误: %s", lineNo, raw), true
		}
	} else if hasScheduledCheckColumn {
		scheduledCheckEnabled = false
	}

	rawScheduledCheckModel := fetch("scheduled_check_model")
	scheduledCheckModel := existingScheduledCheckModelByName[name]
	shouldValidateScheduledCheckModel := false
	if rawScheduledCheckModel != "" {
		scheduledCheckModel = rawScheduledCheckModel
		shouldValidateScheduledCheckModel = true
	} else if hasScheduledCheckModelColumn {
		scheduledCheckModel = ""
	}

	cooldownDetectionRules := existingCooldownDetectionRulesByName[name].Clone()
	if raw := fetch("cooldown_detection_rules"); raw != "" {
		var parsed model.CooldownDetectionRules
		if err := sonic.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, fmt.Sprintf("第%d行 cooldown_detection_rules JSON无效: %v", lineNo, err), true
		}
		if err := cooldown.NormalizeCooldownDetectionRules(&parsed); err != nil {
			return nil, fmt.Sprintf("第%d行 cooldown_detection_rules 无效: %v", lineNo, err), true
		}
		if parsed.IsEmpty() {
			cooldownDetectionRules = nil
		} else {
			cooldownDetectionRules = &parsed
		}
	} else if hasCooldownDetectionRulesColumn {
		cooldownDetectionRules = nil
	}

	retryOtherKeysOnFailure := existingRetryOtherKeysOnFailureByName[name]
	if raw := fetch("retry_other_keys_on_failure"); raw != "" {
		val, ok := parseImportEnabled(raw)
		if !ok {
			return nil, fmt.Sprintf("第%d行 retry_other_keys_on_failure 格式错误: %s", lineNo, raw), true
		}
		retryOtherKeysOnFailure = val
	} else if hasRetryOtherKeysOnFailureColumn {
		retryOtherKeysOnFailure = false
	}

	websockets := existingWebsocketsByName[name]
	if raw := fetch("websockets"); raw != "" {
		val, ok := parseImportEnabled(raw)
		if !ok {
			return nil, fmt.Sprintf("第%d行 websockets 格式错误: %s", lineNo, raw), true
		}
		websockets = val
	} else if hasWebsocketsColumn {
		websockets = false
	}

	// 构建模型条目（合并models和modelRedirects）
	modelEntries := make([]model.ModelEntry, 0, len(models))
	for _, m := range models {
		entry := model.ModelEntry{Model: m}
		if redirect, ok := modelRedirects[m]; ok {
			entry.RedirectModel = redirect
		}
		modelEntries = append(modelEntries, entry)
	}
	if scheduledCheckModel != "" {
		declared := false
		for _, entry := range modelEntries {
			if entry.Model == scheduledCheckModel {
				declared = true
				break
			}
		}
		if !declared {
			if shouldValidateScheduledCheckModel {
				return nil, fmt.Sprintf("第%d行 scheduled_check_model 无效: %s", lineNo, scheduledCheckModel), true
			}
			scheduledCheckModel = ""
		}
	}

	// 构建渠道配置
	// CSV 中的 id 只在导出实例内有意义，跨库导入必须按渠道名称匹配。
	cfg := &model.Config{
		Name:                    name,
		AuthType:                authType,
		OAuthCredential:         oauthCredential,
		Websockets:              websockets,
		URLs:                    urls,
		Priority:                priority,
		RPMLimit:                rpmLimit,
		MaxConcurrency:          maxConcurrency,
		ModelEntries:            modelEntries,
		ProtocolTransformMode:   protocolTransformMode,
		Enabled:                 enabled,
		ScheduledCheckEnabled:   scheduledCheckEnabled,
		ScheduledCheckModel:     scheduledCheckModel,
		CooldownDetectionRules:  cooldownDetectionRules,
		RetryOtherKeysOnFailure: retryOtherKeysOnFailure,
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

	return &model.ChannelWithKeys{
		Config:  cfg,
		APIKeys: apiKeys,
	}, "", false
}

func normalizeCSVImportOAuthCredential(authType, raw string) (string, error) {
	switch authType {
	case model.AuthTypeCodexOAuth:
		credential, err := codexauth.ParseCredential([]byte(raw))
		if err != nil {
			return "", err
		}
		return credential.JSON()
	case model.AuthTypeAntigravityOAuth:
		credential, err := antigravityauth.ParseCredential([]byte(raw))
		if err != nil {
			return "", err
		}
		return credential.JSON()
	case model.AuthTypeXAIOAuth:
		credential, err := xaiauth.ParseCredential([]byte(raw))
		if err != nil {
			return "", err
		}
		return credential.JSON()
	case model.AuthTypeAnthropicOAuth:
		credential, err := anthropicauth.ParseCredential([]byte(raw))
		if err != nil {
			return "", err
		}
		return credential.JSON()
	case model.AuthTypeZAIOAuth:
		credential, err := zaiauth.ParseCredential([]byte(raw))
		if err != nil {
			return "", err
		}
		return credential.JSON()
	default:
		return "", fmt.Errorf("unsupported auth_type %q", authType)
	}
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
	case "rpm-limit", "rpmlimit", "rpm limit":
		return "rpm_limit"
	case "max-concurrency", "maxconcurrency", "max concurrency", "concurrency", "concurrency_limit", "concurrency-limit":
		return "max_concurrency"
	case "scheduled-check-enabled", "scheduledcheckenabled", "scheduled check enabled":
		return "scheduled_check_enabled"
	case "scheduled-check-model", "scheduledcheckmodel", "scheduled check model":
		return "scheduled_check_model"
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
	return util.ParseBool(raw)
}
