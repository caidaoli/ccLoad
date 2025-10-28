package app

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== 冷却管理 ====================
// ✅ P1重构 (2025-10-28): 从admin.go拆分冷却管理,遵循SRP原则

// handleSetChannelCooldown 设置渠道级别冷却
func (s *Server) handleSetChannelCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid channel ID"})
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetChannelCooldown(c.Request.Context(), id, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	// 精确计数(P1): 手动设置渠道冷却

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("渠道已冷却 %d 毫秒", req.DurationMs),
	})
}

// handleSetKeyCooldown 设置Key级别冷却
func (s *Server) handleSetKeyCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid channel ID"})
		return
	}

	keyIndexStr := c.Param("keyIndex")
	keyIndex, err := strconv.Atoi(keyIndexStr)
	if err != nil || keyIndex < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid key index"})
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetKeyCooldown(c.Request.Context(), id, keyIndex, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	// 精确计数(P1): 手动设置Key冷却

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Key #%d 已冷却 %d 毫秒", keyIndex+1, req.DurationMs),
	})
}
