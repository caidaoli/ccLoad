package app

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

var fetchModelsHTTPStatusPattern = regexp.MustCompile(`HTTP\s+(\d{3})`)

// ============================================================
// Admin API: 获取渠道可用模型列表
// ============================================================

// FetchModelsRequest 获取模型列表请求参数
type FetchModelsRequest struct {
	URLs                   model.ChannelURLs `json:"urls" binding:"required,min=1"`
	Protocol               string            `json:"protocol,omitempty"`
	APIKey                 string            `json:"api_key" binding:"required"`
	LowercaseModels        bool              `json:"lowercase_models,omitempty"`
	StripModelSourcePrefix bool              `json:"strip_model_source_prefix,omitempty"`
}

// FetchModelsResponse 获取模型列表响应
type FetchModelsResponse struct {
	Models   []model.ModelEntry `json:"models"`          // 模型列表（包含redirect_model便于编辑）
	Protocol string             `json:"protocol"`        // 成功请求使用的实际上游协议
	Source   string             `json:"source"`          // 数据来源: "api"(从API获取) 或 "predefined"(预定义)
	Debug    *FetchModelsDebug  `json:"debug,omitempty"` // 调试信息（仅开发环境）
}

// FetchModelsDebug 调试信息结构
type FetchModelsDebug struct {
	NormalizedProtocol string `json:"normalized_protocol"` // 规范化后的上游协议
	Fetcher            string `json:"fetcher"`             // 使用的 Fetcher 实现
	ChannelURL         string `json:"channel_url"`         // 渠道URL（脱敏）
}

// BatchRefreshModelsRequest 批量刷新模型请求
type BatchRefreshModelsRequest struct {
	ChannelIDs             []int64 `json:"channel_ids"`
	Mode                   string  `json:"mode"`                                // merge(增量,默认) / replace(覆盖)
	Protocol               string  `json:"protocol,omitempty"`                  // 可选：限制模型发现使用的上游协议
	LowercaseModels        bool    `json:"lowercase_models,omitempty"`          // 客户端模型别名转小写，保留原始上游模型名
	StripModelSourcePrefix bool    `json:"strip_model_source_prefix,omitempty"` // 客户端模型别名仅保留最后一段，保留原始上游模型名
}

// BatchRefreshModelsItem 批量刷新单渠道结果
type BatchRefreshModelsItem struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	Status      string `json:"status"` // updated / unchanged / failed
	Error       string `json:"error,omitempty"`
	Fetched     int    `json:"fetched"`
	Added       int    `json:"added,omitempty"`   // merge模式
	Removed     int    `json:"removed,omitempty"` // replace模式
	Total       int    `json:"total"`             // 刷新后总模型数
}

// HandleFetchModels 获取指定渠道的可用模型列表
// 路由: GET /admin/channels/:id/models/fetch
// 功能:
//   - 根据 URL 声明的协议调用对应的 Models API
//   - Anthropic/Codex/OpenAI/Gemini: 调用官方/v1/models接口
//   - 其它渠道: 返回预定义列表
//
// 设计模式: 适配器模式(Adapter Pattern) + 策略模式(Strategy Pattern)
func (s *Server) HandleFetchModels(c *gin.Context) {
	// 1. 解析路径参数
	channelID, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "无效的渠道ID")
		return
	}

	// 2. 查询渠道配置
	channel, err := s.channelCache.GetConfig(c.Request.Context(), channelID)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "渠道不存在")
		return
	}

	// 3. 获取第一个已启用的 API Key（用于调用 Models API）
	keys, err := s.store.GetAPIKeys(c.Request.Context(), channelID)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "该渠道没有可用的API Key")
		return
	}
	apiKey := firstEnabledAPIKey(keys)
	if apiKey == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "该渠道没有已启用的API Key")
		return
	}

	response, err := s.fetchModelsWithURLFallback(c.Request.Context(), channel.ID, channel.URLs, c.Query("protocol"), apiKey)
	if err != nil {
		// [INFO] 修复：统一返回200，通过success字段区分成功/失败（上游错误是预期内的）
		RespondErrorMsg(c, http.StatusOK, err.Error())
		return
	}

	RespondJSON(c, http.StatusOK, response)
}

// HandleFetchModelsPreview 支持未保存的渠道配置直接测试模型列表
// 路由: POST /admin/channels/models/fetch
func (s *Server) HandleFetchModelsPreview(c *gin.Context) {
	var req FetchModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "参数无效: "+err.Error())
		return
	}

	req.Protocol = strings.TrimSpace(req.Protocol)
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "urls、api_key为必填字段")
		return
	}

	var err error
	req.URLs, err = validateChannelURLConfigs(req.URLs)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "urls无效: "+err.Error())
		return
	}

	response, err := s.fetchModelsWithURLFallback(c.Request.Context(), 0, req.URLs, req.Protocol, req.APIKey)
	if err != nil {
		// [INFO] 修复：统一返回200，通过success字段区分成功/失败（上游错误是预期内的）
		RespondErrorMsg(c, http.StatusOK, err.Error())
		return
	}
	if req.LowercaseModels || req.StripModelSourcePrefix {
		response.Models = normalizeModelEntriesForSave(response.Models, modelNormalizationOptions{
			lowercaseModels:        req.LowercaseModels,
			stripModelSourcePrefix: req.StripModelSourcePrefix,
		})
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleBatchRefreshModels 批量获取并刷新渠道模型
// 路由: POST /admin/channels/models/refresh-batch
func (s *Server) HandleBatchRefreshModels(c *gin.Context) {
	var req BatchRefreshModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "参数无效: "+err.Error())
		return
	}

	channelIDs := normalizeBatchChannelIDs(req.ChannelIDs)
	if len(channelIDs) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "channel_ids不能为空")
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "replace" {
		RespondErrorMsg(c, http.StatusBadRequest, "mode 仅支持 merge 或 replace")
		return
	}

	overrideProtocol := strings.TrimSpace(req.Protocol)
	normalization := modelNormalizationOptions{
		lowercaseModels:        req.LowercaseModels,
		stripModelSourcePrefix: req.StripModelSourcePrefix,
	}
	ctx := c.Request.Context()

	results := make([]BatchRefreshModelsItem, 0, len(channelIDs))
	updated := 0
	unchanged := 0
	failed := 0
	changed := false

	for _, channelID := range channelIDs {
		item := BatchRefreshModelsItem{ChannelID: channelID}

		cfg, err := s.store.GetConfig(ctx, channelID)
		if err != nil {
			item.Status = "failed"
			item.Error = "渠道不存在"
			failed++
			results = append(results, item)
			continue
		}
		item.ChannelName = cfg.Name

		keys, err := s.store.GetAPIKeys(ctx, channelID)
		if err != nil {
			item.Status = "failed"
			item.Error = "该渠道没有可用的API Key"
			failed++
			results = append(results, item)
			continue
		}

		apiKey := firstEnabledAPIKey(keys)
		if apiKey == "" {
			item.Status = "failed"
			item.Error = "该渠道没有已启用的API Key"
			failed++
			results = append(results, item)
			continue
		}

		resp, err := s.fetchModelsWithURLFallback(ctx, cfg.ID, cfg.URLs, overrideProtocol, apiKey)
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			failed++
			results = append(results, item)
			continue
		}

		fetched := normalizeModelEntriesForSave(resp.Models, normalization)
		if len(fetched) == 0 {
			item.Status = "failed"
			item.Error = "获取到的模型列表为空，拒绝刷新"
			failed++
			results = append(results, item)
			continue
		}
		item.Fetched = len(fetched)

		modelEntriesChanged := false
		if (req.LowercaseModels || req.StripModelSourcePrefix) && mode == "merge" {
			normalizedExisting := normalizeModelEntriesForSave(cfg.ModelEntries, normalization)
			modelEntriesChanged = !modelEntriesEqual(cfg.ModelEntries, normalizedExisting)
			cfg.ModelEntries = normalizedExisting
		}

		switch mode {
		case "replace":
			removed, hasChange := replaceModelEntries(cfg, fetched, normalization)
			item.Removed = removed
			item.Total = len(cfg.ModelEntries)
			modelEntriesChanged = hasChange
		default: // merge
			added, hasChange := mergeModelEntries(cfg, fetched)
			item.Added = added
			item.Total = len(cfg.ModelEntries)
			modelEntriesChanged = modelEntriesChanged || hasChange
		}

		scheduledCheckChanged := reconcileScheduledCheckModel(cfg, normalization)
		if !modelEntriesChanged && !scheduledCheckChanged {
			item.Status = "unchanged"
			unchanged++
			results = append(results, item)
			continue
		}

		if _, err := s.store.UpdateConfig(ctx, channelID, cfg); err != nil {
			item.Status = "failed"
			item.Error = "保存模型失败: " + err.Error()
			failed++
			results = append(results, item)
			continue
		}

		item.Status = "updated"
		updated++
		changed = true
		results = append(results, item)
	}

	if changed {
		s.InvalidateChannelListCache()
	}

	RespondJSON(c, http.StatusOK, gin.H{
		"mode":      mode,
		"total":     len(channelIDs),
		"updated":   updated,
		"unchanged": unchanged,
		"failed":    failed,
		"results":   results,
	})
}

func firstEnabledAPIKey(keys []*model.APIKey) string {
	for _, key := range keys {
		if key == nil || key.Disabled {
			continue
		}
		if apiKey := strings.TrimSpace(key.APIKey); apiKey != "" {
			return apiKey
		}
	}
	return ""
}

// fetchModelsWithURLFallback 按URL排序顺序抓取模型列表。
// 设计目标：多URL渠道下，单个URL异常不应导致整个管理操作失败。
func (s *Server) fetchModelsWithURLFallback(
	ctx context.Context,
	channelID int64,
	configuredURLs model.ChannelURLs,
	overrideProtocol, apiKey string,
) (*FetchModelsResponse, error) {
	if len(configuredURLs) == 0 {
		return nil, fmt.Errorf("渠道URL为空")
	}
	overrideProtocol = strings.ToLower(strings.TrimSpace(overrideProtocol))
	if overrideProtocol != "" && !protocol.IsValid(protocol.Protocol(overrideProtocol)) {
		return nil, fmt.Errorf("不支持的上游协议: %s", overrideProtocol)
	}

	selectorEnabled := s != nil && s.urlSelector != nil && channelID > 0
	var selector *URLSelector
	if selectorEnabled {
		selector = s.urlSelector
	}
	runtimeURLs := make([]string, len(configuredURLs))
	for i := range configuredURLs {
		runtimeURLs[i] = configuredURLs[i].RuntimeURL()
	}
	sortedURLs := orderURLsWithSelector(selector, channelID, runtimeURLs)
	sortedURLs = prioritizeDeclaredProtocolURLs(sortedURLs, configuredURLs)
	localProtocolOrder := localUpstreamProtocolOrder(configuredURLs)

	var lastErr error
	for _, sorted := range sortedURLs {
		if sorted.idx < 0 || sorted.idx >= len(configuredURLs) {
			continue
		}
		entry := configuredURLs[sorted.idx]
		protocols := entry.Protocols
		if overrideProtocol != "" {
			if !entry.SupportsProtocol(overrideProtocol) {
				continue
			}
			protocols = []string{overrideProtocol}
		} else if len(protocols) == 0 {
			protocols = make([]string, len(localProtocolOrder))
			for i, candidate := range localProtocolOrder {
				protocols[i] = string(candidate)
			}
		}
		for _, upstreamProtocol := range protocols {
			start := time.Now()
			resp, err := fetchModelsForConfig(ctx, upstreamProtocol, sorted.url, apiKey)
			if err == nil {
				if selectorEnabled {
					latency := time.Since(start)
					if latency <= 0 {
						latency = time.Millisecond
					}
					s.urlSelector.RecordLatency(channelID, sorted.url, latency)
				}
				return resp, nil
			}
			lastErr = err
			if selectorEnabled && shouldCooldownURLOnFetchModelsError(err) {
				s.urlSelector.CooldownURL(channelID, sorted.url)
				break
			}
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("获取模型列表失败: 未找到可用URL")
}

func shouldCooldownURLOnFetchModelsError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	if statusCode, body, ok := parseFetchModelsStatus(errMsg); ok {
		classification := util.ClassifyHTTPResponseWithMeta(statusCode, nil, []byte(body))
		return classification.Level == util.ErrorLevelChannel
	}

	msgLower := strings.ToLower(errMsg)
	networkErrorMarkers := []string{
		"请求失败:",
		"读取响应失败:",
		"context deadline exceeded",
		"i/o timeout",
		"connection refused",
		"connection reset",
		"no route to host",
	}
	for _, marker := range networkErrorMarkers {
		if strings.Contains(msgLower, marker) {
			return true
		}
	}
	return false
}

func parseFetchModelsStatus(errMsg string) (statusCode int, body string, ok bool) {
	matches := fetchModelsHTTPStatusPattern.FindStringSubmatch(errMsg)
	if len(matches) < 2 {
		return 0, "", false
	}

	code, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, "", false
	}

	body = errMsg
	if fullMatch := matches[0]; fullMatch != "" {
		if idx := strings.Index(errMsg, fullMatch); idx >= 0 {
			body = strings.TrimLeft(errMsg[idx+len(fullMatch):], "): \t")
		}
	}
	return code, strings.TrimSpace(body), true
}

func fetchModelsForConfig(ctx context.Context, upstreamProtocol, channelURL, apiKey string) (*FetchModelsResponse, error) {
	normalizedProtocol := util.NormalizeProtocol(upstreamProtocol)
	source := determineSource(upstreamProtocol)

	var (
		modelNames []string
		fetcherStr string
		err        error
	)

	// 没有模型发现接口的协议直接返回预设列表。
	if source == "predefined" {
		modelNames = util.PredefinedModels(normalizedProtocol)
		if len(modelNames) == 0 {
			return nil, fmt.Errorf("协议 %s 暂无预设模型列表", normalizedProtocol)
		}
		fetcherStr = "predefined"
	} else {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		fetcher := util.NewModelsFetcher(upstreamProtocol)
		fetcherStr = fmt.Sprintf("%T", fetcher)

		modelNames, err = fetcher.FetchModels(ctx, channelURL, apiKey)
		if err != nil {
			return nil, fmt.Errorf(
				"获取模型列表失败(上游协议:%s, 规范化协议:%s, 数据来源:%s): %w",
				upstreamProtocol, normalizedProtocol, source, err,
			)
		}
	}

	// 转换为 ModelEntry 格式，填充 RedirectModel 为 Model（方便前端编辑）
	models := make([]model.ModelEntry, len(modelNames))
	for i, name := range modelNames {
		models[i] = model.ModelEntry{
			Model:         name,
			RedirectModel: name, // 填充为请求模型名称
		}
	}

	return &FetchModelsResponse{
		Models:   models,
		Protocol: upstreamProtocol,
		Source:   source,
		Debug: &FetchModelsDebug{
			NormalizedProtocol: normalizedProtocol,
			Fetcher:            fetcherStr,
			ChannelURL:         channelURL,
		},
	}, nil
}

// determineSource 判断模型列表来源（辅助函数）
func determineSource(upstreamProtocol string) string {
	switch util.NormalizeProtocol(upstreamProtocol) {
	case util.ProtocolOpenAI, util.ProtocolGemini, util.ProtocolAnthropic, util.ProtocolCodex:
		return "api" // 从API获取
	default:
		return "predefined" // 预定义列表
	}
}

type modelNormalizationOptions struct {
	lowercaseModels        bool
	stripModelSourcePrefix bool
}

type normalizedModelCandidate struct {
	upstreamModel   string
	sourcePrefixed  bool
	exactAliasMatch bool
}

func normalizeModelEntriesForSave(entries []model.ModelEntry, options modelNormalizationOptions) []model.ModelEntry {
	if len(entries) == 0 {
		return nil
	}

	seen := make(map[string]int, len(entries))
	candidates := make([]normalizedModelCandidate, 0, len(entries))
	normalized := make([]model.ModelEntry, 0, len(entries))
	for _, entry := range entries {
		clean := entry
		if err := clean.Validate(); err != nil {
			continue
		}
		if clean.Model == "" {
			continue
		}

		upstreamModel := clean.RedirectModel
		if upstreamModel == "" {
			upstreamModel = clean.Model
		}
		alias, sourcePrefixed := normalizeModelAlias(clean.Model, options)
		clean.Model = alias
		if upstreamModel == alias {
			clean.RedirectModel = ""
		} else {
			clean.RedirectModel = upstreamModel
		}

		candidate := normalizedModelCandidate{
			upstreamModel:   upstreamModel,
			sourcePrefixed:  sourcePrefixed,
			exactAliasMatch: upstreamModel == alias,
		}
		key := strings.ToLower(clean.Model)
		if index, exists := seen[key]; exists {
			if preferNormalizedModelCandidate(candidate, candidates[index]) {
				candidates[index] = candidate
				normalized[index] = clean
			}
			continue
		}
		seen[key] = len(normalized)
		candidates = append(candidates, candidate)
		normalized = append(normalized, clean)
	}
	return normalized
}

func normalizeModelAlias(modelName string, options modelNormalizationOptions) (string, bool) {
	alias := modelName
	sourcePrefixed := false
	if options.stripModelSourcePrefix {
		if separator := strings.LastIndexByte(alias, '/'); separator >= 0 && separator+1 < len(alias) {
			alias = alias[separator+1:]
			sourcePrefixed = true
		}
	}
	if options.lowercaseModels {
		alias = strings.ToLower(alias)
	}
	return alias, sourcePrefixed
}

func preferNormalizedModelCandidate(candidate, current normalizedModelCandidate) bool {
	if candidate.exactAliasMatch != current.exactAliasMatch {
		return candidate.exactAliasMatch
	}
	if candidate.sourcePrefixed != current.sourcePrefixed {
		return !candidate.sourcePrefixed
	}
	return candidate.upstreamModel < current.upstreamModel
}

func modelEntriesEqual(left, right []model.ModelEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// reconcileScheduledCheckModel 在刷新归一化或移除模型后，保证定时检测别名仍然有效。
func reconcileScheduledCheckModel(cfg *model.Config, options modelNormalizationOptions) bool {
	if cfg == nil || cfg.ScheduledCheckModel == "" {
		return false
	}

	original := cfg.ScheduledCheckModel
	normalizedOriginal, _ := normalizeModelAlias(original, options)
	for _, entry := range cfg.ModelEntries {
		if strings.EqualFold(entry.Model, original) ||
			strings.EqualFold(entry.Model, normalizedOriginal) ||
			strings.EqualFold(entry.RedirectModel, original) {
			cfg.ScheduledCheckModel = entry.Model
			return cfg.ScheduledCheckModel != original
		}
	}

	cfg.ScheduledCheckModel = ""
	return true
}

func mergeModelEntries(cfg *model.Config, fetched []model.ModelEntry) (added int, changed bool) {
	occupied := make(map[string]struct{}, len(cfg.ModelEntries)*2)
	for _, entry := range cfg.ModelEntries {
		occupied[strings.ToLower(entry.Model)] = struct{}{}
		if entry.RedirectModel != "" {
			occupied[strings.ToLower(entry.RedirectModel)] = struct{}{}
		}
	}

	for _, entry := range fetched {
		modelKey := strings.ToLower(entry.Model)
		_, modelExists := occupied[modelKey]
		redirectKey := strings.ToLower(entry.RedirectModel)
		_, redirectExists := occupied[redirectKey]
		if modelExists || (entry.RedirectModel != "" && redirectExists) {
			continue
		}
		cfg.ModelEntries = append(cfg.ModelEntries, entry)
		occupied[modelKey] = struct{}{}
		if entry.RedirectModel != "" {
			occupied[redirectKey] = struct{}{}
		}
		added++
	}

	return added, added > 0
}

func replaceModelEntries(cfg *model.Config, fetched []model.ModelEntry, options modelNormalizationOptions) (removed int, changed bool) {
	oldEntries := cfg.ModelEntries
	oldSet := make(map[string]struct{}, len(oldEntries))
	disabledAliases := make(map[string]struct{}, len(oldEntries))
	newSet := make(map[string]struct{}, len(fetched))

	for _, entry := range oldEntries {
		key := strings.ToLower(entry.Model)
		oldSet[key] = struct{}{}
		if !entry.Disabled {
			continue
		}
		disabledAliases[key] = struct{}{}
		normalizedModel, _ := normalizeModelAlias(entry.Model, options)
		disabledAliases[strings.ToLower(normalizedModel)] = struct{}{}
		if entry.RedirectModel != "" {
			disabledAliases[strings.ToLower(entry.RedirectModel)] = struct{}{}
		}
	}
	for i := range fetched {
		key := strings.ToLower(fetched[i].Model)
		newSet[key] = struct{}{}
		_, disabled := disabledAliases[key]
		if !disabled && fetched[i].RedirectModel != "" {
			_, disabled = disabledAliases[strings.ToLower(fetched[i].RedirectModel)]
		}
		fetched[i].Disabled = fetched[i].Disabled || disabled
	}
	for key := range oldSet {
		if _, exists := newSet[key]; !exists {
			removed++
		}
	}

	if modelEntriesEqual(oldEntries, fetched) {
		return 0, false
	}

	cfg.ModelEntries = fetched
	return removed, true
}
