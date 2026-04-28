package app

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// Gemini API 特殊处理
// ============================================================================

func (s *Server) filterVisibleModelsForRequest(c *gin.Context, protocol string, models []string) []string {
	if s.authService == nil {
		return models
	}

	tokenHash, _ := c.Get("token_hash")
	tokenHashStr, _ := tokenHash.(string)
	if tokenHashStr == "" {
		return models
	}

	if allowedChannelSet, hasRestriction := s.authService.getAllowedChannelSet(tokenHashStr); hasRestriction {
		channels, err := s.getEnabledChannelsByExposedProtocol(c.Request.Context(), protocol)
		if err != nil {
			return nil
		}
		modelSet := make(map[string]struct{})
		for _, cfg := range channels {
			if cfg == nil {
				continue
			}
			if _, ok := allowedChannelSet[cfg.ID]; !ok {
				continue
			}
			for _, model := range cfg.GetModels() {
				modelSet[model] = struct{}{}
			}
		}
		models = make([]string, 0, len(modelSet))
		for model := range modelSet {
			models = append(models, model)
		}
	}

	return s.authService.FilterAllowedModels(tokenHashStr, models)
}

// handleListGeminiModels 处理 GET /v1beta/models 请求，返回本地 Gemini 模型列表
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleListGeminiModels(c *gin.Context) {
	ctx := c.Request.Context()

	// 获取所有暴露 gemini 协议的去重模型列表
	models, err := s.getModelsByExposedProtocol(ctx, "gemini")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}
	models = s.filterVisibleModelsForRequest(c, "gemini", models)
	sort.Strings(models)

	// 构造 Gemini API 响应格式
	type ModelInfo struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}

	modelList := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		modelList = append(modelList, ModelInfo{
			Name:        "models/" + model,
			DisplayName: formatModelDisplayName(model),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models": modelList,
	})
}

// detectModelsChannelType 根据请求头判断 /v1/models 应返回哪种渠道类型的模型
// anthropic-version 头存在 → anthropic 渠道；否则 → openai 渠道
func detectModelsChannelType(c *gin.Context) string {
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

// handleListOpenAIModels 处理 GET /v1/models 请求，根据请求类型返回对应渠道的模型列表
func (s *Server) handleListOpenAIModels(c *gin.Context) {
	ctx := c.Request.Context()

	channelType := detectModelsChannelType(c)
	models, err := s.getModelsByExposedProtocol(ctx, channelType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}
	models = s.filterVisibleModelsForRequest(c, channelType, models)
	sort.Strings(models)

	if channelType == "anthropic" {
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
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	modelList := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		modelList = append(modelList, ModelInfo{
			ID:      model,
			Object:  "model",
			Created: 0,
			OwnedBy: "system",
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   modelList,
	})
}
