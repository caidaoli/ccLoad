package app

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	cliproxyregistry "ccLoad/internal/protocol/cliproxy/registry"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// Gemini API 特殊处理
// ============================================================================

/**
 * @brief 从同一份渠道快照取得令牌可见的模型和提供这些模型的渠道。
 * @param c 模型列表请求，包含可选的令牌身份。
 * @return 已应用模型权限的公开模型名。
 * @return 已应用渠道权限的启用渠道，供协议目录读取对应模型元数据。
 * @return 渠道读取失败时返回底层错误。
 */
func (s *Server) visibleModelCatalog(c *gin.Context) ([]string, []*model.Config, error) {
	channels, err := s.GetEnabledChannelsByModel(c.Request.Context(), "*")
	if err != nil {
		return nil, nil, err
	}
	tokenHash := c.GetString("token_hash")
	if tokenHash == "" {
		return modelNamesFromChannels(channels), channels, nil
	}
	channels, _ = s.authService.FilterAllowedChannels(tokenHash, channels)
	return s.authService.FilterAllowedModels(tokenHash, modelNamesFromChannels(channels)), channels, nil
}

/**
 * @brief 读取 Cursor 渠道凭证中的目录快照，无需请求上游。
 * @param cfg 已启用的渠道。
 * @return 该渠道的 Cursor 凭证；非 Cursor 渠道或凭证无法解析时为 nil。
 */
func cursorCatalogCredential(cfg *model.Config) *cursorauth.Credential {
	if !cfg.UsesCursorOAuth() {
		return nil
	}
	credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil
	}
	return credential
}

/**
 * @brief 汇总同一 Gemini 模型名在可见渠道上的 Cursor 窗口。
 * @param channels 通过令牌渠道限制的配置。
 * @return 公开模型名到窗口 token 数的映射；记录缺失或各目标不一致时取零。
 */
func (s *Server) geminiCursorContextWindows(channels []*model.Config) map[string]int {
	windows := make(map[string]int)
	for _, cfg := range channels {
		catalog := cursorCatalogCredential(cfg)
		for _, name := range cfg.GetModels() {
			if catalog == nil {
				windows[name] = 0
				continue
			}
			for _, selected := range s.enumerateModelRows(cfg, name) {
				target := s.resolveFinalUpstreamModel(cfg, selected, string(protocol.Gemini))
				limit, _ := cursorCatalogValue(catalog.ContextWindows, target)
				if previous, seen := windows[name]; seen && previous != limit {
					limit = 0
				}
				windows[name] = limit
			}
		}
	}
	return windows
}

/**
 * @brief 输出 GET /v1beta/models 的本地目录及可确认的 token 上限。
 * @param c 当前 Gemini 请求。
 */
func (s *Server) handleListGeminiModels(c *gin.Context) {
	models, channels, err := s.visibleModelCatalog(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}
	windows := s.geminiCursorContextWindows(channels)
	sort.Strings(models)

	// 构造 Gemini API 响应格式
	type ModelInfo struct {
		Name                       string   `json:"name"`
		DisplayName                string   `json:"displayName"`
		InputTokenLimit            int      `json:"inputTokenLimit,omitempty"`
		OutputTokenLimit           int      `json:"outputTokenLimit,omitempty"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	}

	modelList := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		inputLimit, outputLimit := geminiModelTokenLimits(model)
		if window := windows[model]; window > 0 {
			inputLimit = window
		}
		modelList = append(modelList, ModelInfo{
			Name:                       "models/" + model,
			DisplayName:                formatModelDisplayName(model),
			InputTokenLimit:            inputLimit,
			OutputTokenLimit:           outputLimit,
			SupportedGenerationMethods: []string{"generateContent"},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models": modelList,
	})
}

// geminiModelTokenLimits 从内嵌目录读取 token 上限；Zed 等客户端会丢弃缺少上限的
// 模型。未收录的模型返回 0，由 omitempty 省略字段，不猜测默认值。
func geminiModelTokenLimits(modelID string) (int, int) {
	info := cliproxyregistry.LookupModelInfo(modelID, "gemini")
	if info == nil {
		info = cliproxyregistry.LookupModelInfo(modelID)
	}
	if info == nil {
		return 0, 0
	}
	input, output := info.InputTokenLimit, info.OutputTokenLimit
	if input <= 0 {
		input = info.ContextLength
	}
	if output <= 0 {
		output = info.MaxCompletionTokens
	}
	return input, output
}

// detectModelsClientProtocol 根据请求头判断 /v1/models 的客户端协议。
// anthropic-version 头存在 → anthropic 渠道；否则 → openai 渠道
func detectModelsClientProtocol(c *gin.Context) string {
	if c.GetHeader("anthropic-version") != "" {
		return "anthropic"
	}
	if strings.HasPrefix(strings.ToLower(c.GetHeader("User-Agent")), "claude-cli") {
		return "anthropic"
	}
	if strings.Contains(strings.ToLower(c.GetHeader("User-Agent")), "codex") {
		return "codex"
	}
	return "openai"
}

/**
 * @brief 根据客户端协议及 client_version 返回令牌可见的模型。
 * @param c 当前模型列表请求。
 */
func (s *Server) handleListOpenAIModels(c *gin.Context) {
	clientProtocol := detectModelsClientProtocol(c)
	models, channels, err := s.visibleModelCatalog(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}
	sort.Strings(models)
	if c.Query("client_version") != "" {
		filtered, metadata := s.filterCodexResponsesModels(channels, models)
		handleListCodexModels(c, filtered, clientProtocol == "codex", metadata)
		return
	}

	if clientProtocol == "anthropic" {
		type ModelInfo struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
			CreatedAt   string `json:"created_at"`
		}
		modelList := make([]ModelInfo, 0, len(models))
		for _, model := range models {
			modelList = append(modelList, ModelInfo{
				ID:          model,
				DisplayName: formatModelDisplayName(model),
				Type:        "model",
				CreatedAt:   time.Unix(0, 0).UTC().Format(time.RFC3339),
			})
		}

		resp := gin.H{
			"data":     modelList,
			"has_more": false,
		}
		if len(modelList) > 0 {
			resp["first_id"] = modelList[0].ID
			resp["last_id"] = modelList[len(modelList)-1].ID
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// 构造 OpenAI API 响应格式
	type ModelInfo struct {
		ID                string `json:"id"`
		Object            string `json:"object"`
		Created           int64  `json:"created"`
		OwnedBy           string `json:"owned_by"`
		MultiAgentVersion string `json:"multi_agent_version,omitempty"`
	}

	modelList := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		modelList = append(modelList, ModelInfo{
			ID:      model,
			Object:  "model",
			Created: 0,
			OwnedBy: "system",
			MultiAgentVersion: func() string {
				if clientProtocol == "codex" {
					return "v2"
				}
				return ""
			}(),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   modelList,
	})
}
