package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxyregistry "ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// filterCodexResponsesModels uses configured route capabilities without probing upstreams.
func (s *Server) filterCodexResponsesModels(c *gin.Context, visibleModels []string) ([]string, error) {
	if len(visibleModels) == 0 {
		return visibleModels, nil
	}
	channels, err := s.GetEnabledChannelsByModel(c.Request.Context(), "*")
	if err != nil {
		return nil, err
	}

	visible := make(map[string]struct{}, len(visibleModels))
	for _, modelID := range visibleModels {
		visible[modelID] = struct{}{}
	}

	tokenHash, _ := c.Get("token_hash")
	tokenHashStr, _ := tokenHash.(string)
	var restriction model.ChannelRestriction
	var hasRestriction bool
	if s.authService != nil && tokenHashStr != "" {
		restriction, hasRestriction = s.authService.getChannelRestriction(tokenHashStr)
	}

	available := make(map[string]struct{}, len(visibleModels))
	for _, cfg := range channels {
		if cfg == nil || (hasRestriction && !restriction.Allows(cfg.ID)) {
			continue
		}
		upstreamProtocols := s.codexResponsesUpstreamProtocols(cfg)
		if len(upstreamProtocols) == 0 {
			continue
		}
		for _, modelID := range cfg.GetModels() {
			if modelID == "*" {
				continue
			}
			if _, ok := visible[modelID]; !ok {
				continue
			}
			for _, upstream := range upstreamProtocols {
				actualModel := s.resolveFinalUpstreamModel(cfg, modelID, string(upstream))
				if codexResponsesTextModel(actualModel) {
					available[modelID] = struct{}{}
					break
				}
			}
		}
	}

	filtered := make([]string, 0, len(visibleModels))
	for _, modelID := range visibleModels {
		if _, ok := available[modelID]; ok {
			filtered = append(filtered, modelID)
		}
	}
	return filtered, nil
}

func (s *Server) codexResponsesUpstreamProtocols(cfg *model.Config) []protocol.Protocol {
	seen := make(map[protocol.Protocol]struct{})
	var protocols []protocol.Protocol
	order := localUpstreamProtocolOrder(cfg.URLs)
	for _, candidateURL := range orderChannelAttemptURLs(s.urlSelector, cfg, cfg.GetURLs()) {
		entry := cfg.URLs[candidateURL.idx]
		candidates, _ := protocolCandidatesForURL(
			entry, cfg.GetProtocolTransformMode(), protocol.Codex, protocol.RequestFamilyResponses, order,
		)
		for _, candidate := range candidates {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			protocols = append(protocols, candidate)
		}
	}
	return protocols
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
		for _, modality := range modalities {
			if modality == "text" {
				return true
			}
		}
		return false
	}
	// Unknown custom aliases keep the administrator's configured route semantics.
	return true
}

func handleListCodexModels(c *gin.Context, modelIDs []string, multiAgent bool) {
	models := make([]map[string]any, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		models = append(models, codexModelDescriptor(modelID, multiAgent))
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
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func codexModelDescriptor(modelID string, multiAgent bool) map[string]any {
	openAIInfo := cliproxyregistry.LookupModelInfo(modelID, "openai")
	info := openAIInfo
	if info == nil {
		info = cliproxyregistry.LookupModelInfo(modelID)
	}
	preset := codexModelPresets[modelID]
	defaultReasoning, reasoningLevels := codexModelReasoningLevels(openAIInfo, preset)
	contextWindow := 128000
	if info != nil && info.ContextLength > 0 {
		contextWindow = info.ContextLength
	} else if preset.contextWindow > 0 {
		contextWindow = preset.contextWindow
	}
	supportsImage := preset.supportsImage
	if info != nil {
		for _, modality := range info.InputModalities {
			if modality == "image" {
				supportsImage = true
				break
			}
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
		"service_tiers":                []any{},
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
	"gpt-6-sol":  {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-6-luna": {contextWindow: 272000, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}, supportsImage: true},
	"gpt-4o":     {contextWindow: 128000, supportsImage: true},
}

func codexModelReasoningLevels(info *cliproxyregistry.ModelInfo, preset codexModelPreset) (string, []map[string]string) {
	fallback := []map[string]string{{"effort": "none", "description": "No reasoning effort"}}
	var supported []string
	if info != nil && info.Thinking != nil {
		supported = info.Thinking.Levels
	} else {
		supported = preset.reasoningLevels
	}

	descriptions := map[string]string{
		"low":    "Fast responses with lighter reasoning",
		"medium": "Balanced reasoning for everyday tasks",
		"high":   "Greater reasoning depth for complex tasks",
		"xhigh":  "Extra-high reasoning depth for difficult tasks",
		"max":    "Maximum reasoning depth for the hardest tasks",
	}
	levels := make([]map[string]string, 0, len(supported))
	defaultLevel := ""
	for _, level := range supported {
		if description, ok := descriptions[level]; ok {
			levels = append(levels, map[string]string{"effort": level, "description": description})
			if defaultLevel == "" || level == "medium" {
				defaultLevel = level
			}
		}
	}
	if len(levels) == 0 {
		return "none", fallback
	}
	return defaultLevel, levels
}
