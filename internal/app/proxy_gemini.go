package app

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// Gemini API 特殊处理
// ============================================================================

// handleListGeminiModels 处理 GET /v1beta/models 请求，返回本地 Gemini 模型列表
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleListGeminiModels(c *gin.Context) {
	ctx := c.Request.Context()

	// 获取所有 gemini 渠道的去重模型列表
	models, err := s.getModelsByChannelType(ctx, "gemini")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}

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

// handleListOpenAIModels 处理 GET /v1/models 请求，返回本地 OpenAI 模型列表
func (s *Server) handleListOpenAIModels(c *gin.Context) {
	ctx := c.Request.Context()

	// 获取所有 openai 渠道的去重模型列表
	models, err := s.getModelsByChannelType(ctx, "openai")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
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
