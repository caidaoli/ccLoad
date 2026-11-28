package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxyregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 一个公开 Responses 模型的路由元数据。
 */
type codexModelMetadata struct {
	serviceTiers    json.RawMessage ///< 路由声明的服务等级，未知时为 JSON null。
	contextWindow   int             ///< 所有候选路由共同支持的上下文窗口，未知时为 0。
	reasoningLevels []string        ///< 实际路由共同可用的等级。
	cursorOnly      bool            ///< 所有候选均能由 Cursor Local Agent 传递内联图片。
	defaultEffort   string          ///< 全部候选一致声明的默认思考档位。
	cursorReasoning bool            ///< 至少有一个候选含 Cursor 账号目录的证据。
}

/**
 * @brief 按已配置的 Responses 路由筛选模型并汇总目录信息，无需请求上游。
 * @param channels 当前令牌可访问的已启用渠道。
 * @param visibleModels 当前令牌可访问的公开模型名。
 * @return 至少有一个文本 Responses 路由的公开模型名。
 * @return 按公开模型名索引的路由元数据；没有候选时可为 nil。
 * @details Cursor 上下文窗口仅在所有候选路由都有相同目录值时声明，思考等级限定为共同支持的等级。
 */
func (s *Server) filterCodexResponsesModels(channels []*model.Config, visibleModels []string) ([]string, map[string]codexModelMetadata) {
	if len(visibleModels) == 0 {
		return visibleModels, nil
	}

	visible := make(map[string]struct{}, len(visibleModels))
	for _, modelID := range visibleModels {
		visible[modelID] = struct{}{}
	}

	metadata := make(map[string]codexModelMetadata, len(visibleModels))
	targets := make(map[string]string, len(visibleModels))
	for _, cfg := range channels {
		cfg = s.withOAuthBaseURLOverride(cfg)
		routes := s.codexResponsesRoutes(cfg)
		if len(routes) == 0 {
			continue
		}
		var sources []codexRouteTierSource
		var cursor *cursorauth.Credential
		for _, modelID := range cfg.GetModels() {
			if _, ok := visible[modelID]; !ok || modelID == "*" {
				continue
			}
			for _, selected := range s.enumerateModelRows(cfg, modelID) {
				for i, route := range routes {
					actualModel := s.resolveFinalUpstreamModel(cfg, selected, string(route.upstream))
					if !codexResponsesTextModel(actualModel) {
						continue
					}
					if sources == nil {
						sources = codexRouteTierSources(cfg, routes)
						cursor = cursorCatalogCredential(cfg)
					}
					declared := sources[i].serviceTiers(actualModel)
					var window int
					var cursorModel, defaultEffort string
					if cursor != nil {
						cursorModel = s.resolveFinalUpstreamModel(cfg, selected, string(protocol.Codex))
						window, _ = cursorCatalogValue(cursor.ContextWindows, cursorModel)
						defaultEffort, _ = cursorCatalogValue(cursor.DefaultEfforts, cursorModel)
					}
					entry, seen := metadata[modelID]
					if !seen {
						entry = codexModelMetadata{serviceTiers: declared, contextWindow: window, defaultEffort: defaultEffort, cursorOnly: cfg.UsesCursorOAuth()}
						targets[modelID] = actualModel
					} else {
						entry.cursorOnly = entry.cursorOnly && cfg.UsesCursorOAuth()
						if entry.defaultEffort != defaultEffort {
							entry.defaultEffort = ""
						}
						if targets[modelID] != actualModel || !bytes.Equal(entry.serviceTiers, declared) {
							entry.serviceTiers = json.RawMessage("null")
						}
						if entry.contextWindow != window {
							entry.contextWindow = 0
						}
					}
					levels, known := cursorCatalogEffortLevels(cursor, cursorModel)
					entry.cursorReasoning = entry.cursorReasoning || known
					if !known {
						levels, _ = reasoningModelLevels(actualModel, route.upstream)
					}
					entry.reasoningLevels = intersectReasoningLevels(entry.reasoningLevels, levels)
					metadata[modelID] = entry
				}
			}
		}
	}

	filtered := make([]string, 0, len(visibleModels))
	for _, modelID := range visibleModels {
		if _, ok := metadata[modelID]; ok {
			filtered = append(filtered, modelID)
		}
	}
	return filtered, metadata
}

type codexResponsesRoute struct {
	url      model.ChannelURL
	upstream protocol.Protocol
}

func (s *Server) codexResponsesRoutes(cfg *model.Config) []codexResponsesRoute {
	var routes []codexResponsesRoute
	order := localUpstreamProtocolOrder(cfg.URLs)
	for _, candidateURL := range orderChannelAttemptURLs(s.urlSelector, cfg, cfg.GetURLs()) {
		if s.urlSelector.IsDisabled(cfg.ID, candidateURL.url) {
			continue
		}
		entry := cfg.URLs[candidateURL.idx]
		candidates, _ := protocolCandidatesForURL(
			entry, cfg.GetProtocolTransformMode(), protocol.Codex, protocol.RequestFamilyResponses, order,
		)
		for _, candidate := range candidates {
			routes = append(routes, codexResponsesRoute{url: entry, upstream: candidate})
		}
	}
	return routes
}

// codexRouteTierSource is the service-tier evidence one route can provide.
type codexRouteTierSource struct {
	manifest    *codexauth.ModelManifest // Codex OAuth snapshot bound to this route's endpoint
	officialAPI bool                     // static API key on the official OpenAI API
}

// codexRouteTierSources resolves per-route evidence once per channel.
func codexRouteTierSources(cfg *model.Config, routes []codexResponsesRoute) []codexRouteTierSource {
	sources := make([]codexRouteTierSource, len(routes))
	var credential *codexauth.Credential
	if cfg.UsesCodexOAuth() {
		credential, _ = codexauth.ParseCredential([]byte(cfg.OAuthCredential))
	}
	for i, route := range routes {
		// Only native Responses keeps service_tier; Responses -> Chat conversion drops it.
		if route.upstream != protocol.Codex {
			continue
		}
		if cfg.UsesCodexOAuth() {
			endpoint, err := codexauth.ModelsEndpoint(route.url.URL)
			if err == nil && credential != nil && credential.ModelManifest.Matches(credential, endpoint) {
				sources[i].manifest = credential.ModelManifest
			}
			continue
		}
		parsed, err := url.Parse(route.url.URL)
		sources[i].officialAPI = cfg.GetAuthType() == model.AuthTypeAPIKey && err == nil && parsed.Scheme == "https" && parsed.User == nil &&
			strings.EqualFold(parsed.Hostname(), "api.openai.com") && (parsed.Port() == "" || parsed.Port() == "443")
	}
	return sources
}

func (source codexRouteTierSource) serviceTiers(actualModel string) json.RawMessage {
	unknown := json.RawMessage("null")
	if source.manifest != nil {
		for _, entry := range source.manifest.Models {
			if entry.Slug == actualModel && len(bytes.TrimSpace(entry.ServiceTiers)) > 0 {
				return entry.ServiceTiers
			}
		}
		return unknown
	}
	if !source.officialAPI || !util.IsOpenAIAstraModel(actualModel) {
		return unknown
	}
	encoded, _ := json.Marshal([]codexauth.ServiceTier{
		{ID: "priority", Name: "Fast", Description: "Priority processing for lower latency."},
		{ID: "ultrafast", Name: "Ultrafast", Description: fmt.Sprintf("Lowest latency; %gx Standard token pricing.",
			util.OpenAIServiceTierMultiplier(actualModel, "ultrafast", util.OpenAIRateCardAPI, time.Now()))},
	})
	return encoded
}

func codexResponsesTextModel(modelID string) bool {
	if _, imageOnly := canonicalCodexImageModel(modelID); imageOnly {
		return false
	}
	baseModelID := modelID
	if slash := strings.LastIndexByte(baseModelID, '/'); slash >= 0 {
		baseModelID = baseModelID[slash+1:]
	}
	if util.ModelFamily(modelID) == "text-embedding" || strings.HasPrefix(strings.ToLower(baseModelID), "text-embedding-") {
		return false
	}
	if modalities, known := util.ModelOutputModalities(modelID); known {
		return slices.Contains(modalities, "text")
	}
	// Unknown custom aliases keep the administrator's configured route semantics.
	return true
}

/**
 * @brief 写出 Codex 模型目录并处理 ETag 条件请求。
 * @param c 当前 HTTP 请求和响应。
 * @param modelIDs 可以提供 Responses 的公开模型名。
 * @param multiAgent 是否为 Codex 客户端声明多代理能力。
 * @param metadata 按公开模型名索引的路由元数据。
 */
func handleListCodexModels(c *gin.Context, modelIDs []string, multiAgent bool, metadata map[string]codexModelMetadata) {
	models := make([]map[string]any, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		models = append(models, codexModelDescriptor(modelID, multiAgent, metadata[modelID]))
	}
	body, err := json.Marshal(struct {
		Models []map[string]any `json:"models"`
	}{Models: models})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode models"})
		return
	}

	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	c.Header("Cache-Control", "private, no-cache")
	c.Header("ETag", etag)
	if matchesCodexModelsETag(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func matchesCodexModelsETag(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

/**
 * @brief 为公开模型生成 Codex 目录项。
 * @param modelID 对客户端公开的模型名。
 * @param multiAgent 是否声明多代理能力。
 * @param metadata 已从候选路由确认的服务等级、上下文窗口和思考等级。
 * @return Codex 模型目录项；`service_tiers` 保留未知、空列表与已声明列表的区别。
 */
func codexModelDescriptor(modelID string, multiAgent bool, metadata codexModelMetadata) map[string]any {
	serviceTiers := metadata.serviceTiers
	if len(serviceTiers) == 0 {
		serviceTiers = json.RawMessage("null")
	}
	openAIInfo := cliproxyregistry.LookupModelInfo(modelID, "openai")
	info := openAIInfo
	if info == nil {
		info = cliproxyregistry.LookupModelInfo(modelID)
	}
	preset := codexModelPresets[modelID]
	defaultReasoning, reasoningLevels := codexModelReasoningLevels(openAIInfo, preset, metadata)
	contextWindow := metadata.contextWindow
	if contextWindow == 0 {
		contextWindow = 128000
		if info != nil && info.ContextLength > 0 {
			contextWindow = info.ContextLength
		} else if preset.contextWindow > 0 {
			contextWindow = preset.contextWindow
		}
	}
	supportsImage := metadata.cursorOnly || preset.supportsImage
	if info != nil {
		if slices.Contains(info.InputModalities, "image") {
			supportsImage = true
		}
	}
	inputModalities := []string{"text"}
	if supportsImage {
		inputModalities = append(inputModalities, "image")
	}
	model := map[string]any{
		"slug":                         modelID,
		"display_name":                 formatModelDisplayName(modelID),
		"description":                  "Model available through ccLoad.",
		"default_reasoning_level":      defaultReasoning,
		"supported_reasoning_levels":   reasoningLevels,
		"multi_agent_reasoning_effort": nil,
		"shell_type":                   "unified_exec",
		"visibility":                   "list",
		"supported_in_api":             true,
		"priority":                     50,
		"additional_speed_tiers":       []string{},
		"service_tiers":                serviceTiers,
		"default_service_tier":         nil,
		"availability_nux":             nil,
		"upgrade":                      nil,
		"model_messages": map[string]any{
			"instructions_template":  "You are a coding assistant. Follow the user's instructions and any applicable repository guidance.",
			"instructions_variables": nil,
			"approvals":              nil,
			"collaboration_modes":    nil,
			"auto_review":            nil,
			"permissions":            nil,
			"multi_agent":            nil,
			"token_budget":           nil,
			"guardian_v2":            nil,
		},
		"include_skills_usage_instructions":    false,
		"include_plugin_usage_instructions":    false,
		"include_apps_usage_instructions":      false,
		"supports_reasoning_summary_parameter": false,
		"default_reasoning_summary":            "auto",
		"support_verbosity":                    false,
		"default_verbosity":                    nil,
		"apply_patch_tool_type":                nil,
		"web_search_tool_type":                 "text",
		"truncation_policy":                    map[string]any{"mode": "bytes", "limit": 10000},
		"supports_image_detail_original":       false,
		"supports_parallel_tool_calls":         false,
		"context_window":                       contextWindow,
		"max_context_window":                   nil,
		"auto_compact_token_limit":             nil,
		"comp_hash":                            nil,
		"effective_context_window_percent":     95,
		"experimental_supported_tools":         []string{},
		"input_modalities":                     inputModalities,
		"supports_search_tool":                 false,
		"use_responses_lite":                   false,
		"node_repl_auto_review_required":       false,
		"node_repl_disabled":                   false,
		"auto_review_model_override":           nil,
		"model_specialty":                      nil,
		"tool_mode":                            nil,
		"multi_agent_version":                  nil,
	}
	if multiAgent {
		model["multi_agent_version"] = "v2"
	}
	return model
}

type codexModelPreset struct {
	contextWindow   int
	reasoningLevels []string
	supportsImage   bool
}

var codexModelPresets = map[string]codexModelPreset{
	"gpt-6.2-astra": {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-6.2-sol":   {contextWindow: 500000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-6.1-sol":   {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-6-sol":     {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-6-luna":    {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-4o":        {contextWindow: 128000, supportsImage: true},
}

/**
 * @brief 生成 Codex 可选档位列表及其显示默认值。
 * @param info 公开模型名对应的 OpenAI 内置目录项，未收录时为 nil。
 * @param preset 公开模型名的补充静态目录。
 * @param metadata 可见路由中的 Cursor 账号等级证据。
 * @return 优先取候选一致的目录默认值，再选 medium 或首个可用档位；无档位时为 none。
 * @return Codex 思考等级对象列表；没有共同等级时只列 none。
 */
func codexModelReasoningLevels(info *cliproxyregistry.ModelInfo, preset codexModelPreset, metadata codexModelMetadata) (string, []map[string]string) {
	fallback := []map[string]string{{"effort": "none", "description": "No reasoning effort"}}
	var supported []string
	if info != nil && info.Thinking != nil {
		supported = info.Thinking.Levels
	} else {
		supported = preset.reasoningLevels
	}
	if metadata.cursorReasoning {
		supported = metadata.reasoningLevels
	}

	descriptions := map[string]string{
		"low":    "Fast responses with lighter reasoning",
		"medium": "Balanced reasoning for everyday tasks",
		"high":   "Greater reasoning depth for complex tasks",
		"xhigh":  "Extra-high reasoning depth for difficult tasks",
		"max":    "Maximum reasoning depth for the hardest tasks",
	}
	if metadata.cursorReasoning {
		descriptions[string(thinking.LevelNone)] = "No reasoning effort"
		descriptions[string(thinking.LevelMinimal)] = "Minimal reasoning for simple tasks"
	}
	levels := make([]map[string]string, 0, len(supported))
	defaultLevel := ""
	for _, level := range supported {
		if description, ok := descriptions[level]; ok {
			levels = append(levels, map[string]string{"effort": level, "description": description})
			if defaultLevel == "" || (defaultLevel == "none" && level != "none") || level == "medium" {
				defaultLevel = level
			}
		}
	}
	if len(levels) == 0 {
		return "none", fallback
	}
	if metadata.defaultEffort != "" && slices.Contains(supported, metadata.defaultEffort) {
		defaultLevel = metadata.defaultEffort
	}
	return defaultLevel, levels
}
