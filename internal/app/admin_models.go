package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// ============================================================
// Admin API: 获取渠道可用模型列表
// ============================================================

// FetchModelsRequest 获取模型列表请求参数
type FetchModelsRequest struct {
	// 无需额外参数,从路径参数获取channel_id
}

// FetchModelsResponse 获取模型列表响应
type FetchModelsResponse struct {
	Models      []string          `json:"models"`          // 模型列表
	ChannelType string            `json:"channel_type"`    // 渠道类型
	Source      string            `json:"source"`          // 数据来源: "api"(从API获取) 或 "predefined"(预定义)
	Debug       *FetchModelsDebug `json:"debug,omitempty"` // 调试信息（仅开发环境）
}

// FetchModelsDebug 调试信息结构
type FetchModelsDebug struct {
	NormalizedType string `json:"normalized_type"` // 规范化后的渠道类型
	FetcherType    string `json:"fetcher_type"`    // 使用的Fetcher类型
	ChannelURL     string `json:"channel_url"`     // 渠道URL（脱敏）
}

// HandleFetchModels 获取指定渠道的可用模型列表
// 路由: GET /admin/channels/:id/models/fetch
// 功能:
//   - 根据渠道类型调用对应的Models API
//   - Anthropic/Codex: 返回预定义列表(官方无API)
//   - OpenAI/Gemini: 调用官方/v1/models接口
//
// 设计模式: 适配器模式(Adapter Pattern) + 策略模式(Strategy Pattern)
func (s *Server) HandleFetchModels(c *gin.Context) {
	// 1. 解析路径参数
	idStr := c.Param("id")
	channelID, err := strconv.ParseInt(idStr, 10, 64)
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

	// 3. 获取第一个API Key（用于调用Models API）
	keys, err := s.store.GetAPIKeys(c.Request.Context(), channelID)
	if err != nil || len(keys) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "该渠道没有可用的API Key")
		return
	}
	apiKey := keys[0].APIKey

	// 4. 根据渠道类型创建对应的Fetcher（工厂模式）
	fetcher := util.NewModelsFetcher(channel.ChannelType)
	normalizedType := util.NormalizeChannelType(channel.ChannelType)
	source := determineSource(channel.ChannelType)

	// 5. 调用Models API（超时5秒）
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	models, err := fetcher.FetchModels(ctx, channel.URL, apiKey)
	if err != nil {
		// 错误处理：返回用户友好的错误信息
		RespondErrorMsg(c, http.StatusBadGateway, fmt.Sprintf(
			"获取模型列表失败(渠道类型:%s, 规范化类型:%s, 数据来源:%s): %v",
			channel.ChannelType, normalizedType, source, err))
		return
	}

	// 6. 返回响应（增强调试信息）
	response := FetchModelsResponse{
		Models:      models,
		ChannelType: channel.ChannelType, // 原始值
		Source:      source,
		Debug: &FetchModelsDebug{
			NormalizedType: normalizedType,
			FetcherType:    fmt.Sprintf("%T", fetcher),
			ChannelURL:     channel.URL,
		},
	}
	RespondJSON(c, http.StatusOK, response)
}

// determineSource 判断模型列表来源（辅助函数）
func determineSource(channelType string) string {
	switch util.NormalizeChannelType(channelType) {
	case util.ChannelTypeOpenAI, util.ChannelTypeGemini:
		return "api" // 从API获取
	default:
		return "predefined" // 预定义列表
	}
}
