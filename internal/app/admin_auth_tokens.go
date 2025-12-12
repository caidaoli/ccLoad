package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strconv"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// API访问令牌管理 (Admin API)
// ============================================================================

// HandleListAuthTokens 列出所有API访问令牌（支持时间范围统计，2025-12扩展）
// GET /admin/auth-tokens?range=today
func (s *Server) HandleListAuthTokens(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tokens, err := s.store.ListAuthTokens(ctx)
	if err != nil {
		log.Print("❌ 列出令牌失败: " + err.Error())
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 脱敏处理（仅显示前4后4字符）
	for _, t := range tokens {
		t.Token = model.MaskToken(t.Token)
	}

	// 如果请求中包含range参数，则叠加时间范围统计（用于tokens.html页面）
	timeRange := c.Query("range")
	if timeRange != "" {
		params := ParsePaginationParams(c)
		startTime, endTime := params.GetTimeRange()

		// 从logs表聚合时间范围内的统计
		rangeStats, err := s.store.GetAuthTokenStatsInRange(ctx, startTime, endTime)
		if err != nil {
			log.Printf("[WARN]  查询时间范围统计失败: %v", err)
			// 降级处理：统计查询失败不影响token列表返回，仅记录警告
		} else {
			// 将时间范围统计叠加到每个token的响应中
			for _, t := range tokens {
				if stat, ok := rangeStats[t.ID]; ok {
					// 用时间范围统计覆盖累计统计字段（前端透明）
					t.SuccessCount = stat.SuccessCount
					t.FailureCount = stat.FailureCount
					t.PromptTokensTotal = stat.PromptTokens
					t.CompletionTokensTotal = stat.CompletionTokens
					t.CacheReadTokensTotal = stat.CacheReadTokens
					t.CacheCreationTokensTotal = stat.CacheCreationTokens
					t.TotalCostUSD = stat.TotalCost
					t.StreamAvgTTFB = stat.StreamAvgTTFB
					t.NonStreamAvgRT = stat.NonStreamAvgRT
					t.StreamCount = stat.StreamCount
					t.NonStreamCount = stat.NonStreamCount
				} else {
					// 该token在此时间范围内无数据，清零统计字段
					t.SuccessCount = 0
					t.FailureCount = 0
					t.PromptTokensTotal = 0
					t.CompletionTokensTotal = 0
					t.CacheReadTokensTotal = 0
					t.CacheCreationTokensTotal = 0
					t.TotalCostUSD = 0
					t.StreamAvgTTFB = 0
					t.NonStreamAvgRT = 0
					t.StreamCount = 0
					t.NonStreamCount = 0
				}
			}
		}
	}

	RespondJSON(c, http.StatusOK, tokens)
}

// HandleCreateAuthToken 创建新的API访问令牌
// POST /admin/auth-tokens
func (s *Server) HandleCreateAuthToken(c *gin.Context) {
	var req struct {
		Description string `json:"description" binding:"required"`
		ExpiresAt   *int64 `json:"expires_at"` // Unix毫秒时间戳，nil表示永不过期
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	// 生成安全令牌(64字符十六进制)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Print("❌ 生成令牌失败: " + err.Error())
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	tokenPlain := hex.EncodeToString(tokenBytes)

	// 计算SHA256哈希用于存储
	tokenHash := model.HashToken(tokenPlain)

	authToken := &model.AuthToken{
		Token:       tokenHash,
		Description: req.Description,
		ExpiresAt:   req.ExpiresAt,
		IsActive:    true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.store.CreateAuthToken(ctx, authToken); err != nil {
		log.Print("❌ 创建令牌失败: " + err.Error())
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 触发热更新（立即生效）
	if err := s.authService.ReloadAuthTokens(); err != nil {
		log.Print("[WARN]  热更新失败: " + err.Error())
	}

	log.Printf("[INFO] 创建API令牌: ID=%d, 描述=%s", authToken.ID, authToken.Description)

	// 返回明文令牌（仅此一次机会）
	RespondJSON(c, http.StatusOK, gin.H{
		"id":          authToken.ID,
		"token":       tokenPlain, // 明文令牌，仅创建时返回
		"description": authToken.Description,
		"created_at":  authToken.CreatedAt,
		"expires_at":  authToken.ExpiresAt,
		"is_active":   authToken.IsActive,
	})
}

// HandleUpdateAuthToken 更新令牌信息
// PUT /admin/auth-tokens/:id
func (s *Server) HandleUpdateAuthToken(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid token id")
		return
	}

	var req struct {
		Description *string `json:"description"`
		IsActive    *bool   `json:"is_active"`
		ExpiresAt   *int64  `json:"expires_at"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 获取现有令牌
	token, err := s.store.GetAuthToken(ctx, id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "token not found")
		return
	}

	// 更新字段
	if req.Description != nil {
		token.Description = *req.Description
	}
	if req.IsActive != nil {
		token.IsActive = *req.IsActive
	}
	if req.ExpiresAt != nil {
		token.ExpiresAt = req.ExpiresAt
	}

	if err := s.store.UpdateAuthToken(ctx, token); err != nil {
		log.Print("❌ 更新令牌失败: " + err.Error())
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 触发热更新
	if err := s.authService.ReloadAuthTokens(); err != nil {
		log.Print("[WARN]  热更新失败: " + err.Error())
	}

	log.Printf("[INFO] 更新API令牌: ID=%d", id)

	// 返回脱敏后的令牌信息
	token.Token = model.MaskToken(token.Token)
	RespondJSON(c, http.StatusOK, token)
}

// HandleDeleteAuthToken 删除令牌
// DELETE /admin/auth-tokens/:id
func (s *Server) HandleDeleteAuthToken(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid token id")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.store.DeleteAuthToken(ctx, id); err != nil {
		log.Print("❌ 删除令牌失败: " + err.Error())
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 触发热更新
	if err := s.authService.ReloadAuthTokens(); err != nil {
		log.Print("[WARN]  热更新失败: " + err.Error())
	}

	log.Printf("[INFO] 删除API令牌: ID=%d", id)

	RespondJSON(c, http.StatusOK, gin.H{"id": id})
}
