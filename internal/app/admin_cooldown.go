package app

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== 冷却管理 ====================
// 从admin.go拆分冷却管理,遵循SRP原则

// handleSetChannelCooldown 设置渠道级别冷却
func (s *Server) HandleSetChannelCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetChannelCooldown(c.Request.Context(), id, until)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 精确计数(手动设置渠道冷却

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("渠道已冷却 %d 毫秒", req.DurationMs),
	})
}

// handleSetKeyCooldown 设置Key级别冷却
func (s *Server) HandleSetKeyCooldown(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel ID")
		return
	}

	keyIndexStr := c.Param("keyIndex")
	keyIndex, err := strconv.Atoi(keyIndexStr)
	if err != nil || keyIndex < 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid key index")
		return
	}

	var req CooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	until := time.Now().Add(time.Duration(req.DurationMs) * time.Millisecond)
	err = s.store.SetKeyCooldown(c.Request.Context(), id, keyIndex, until)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	// 精确计数(手动设置Key冷却

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Key #%d 已冷却 %d 毫秒", keyIndex+1, req.DurationMs),
	})
}
