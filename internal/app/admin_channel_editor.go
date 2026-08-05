package app

import (
	"fmt"
	"log"
	"net/http"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type channelEditorModelStats struct {
	Available bool                `json:"available"`
	Items     []ChannelModelStats `json:"items"`
}

type channelEditorURLStats struct {
	Available bool      `json:"available"`
	Items     []URLStat `json:"items"`
}

type channelEditorFeatures struct {
	ScheduledCheckEnabled bool `json:"scheduled_check_enabled"`
}

type channelEditorData struct {
	Channel    ChannelWithCooldown     `json:"channel"`
	Keys       []*model.APIKey         `json:"keys"`
	ModelStats channelEditorModelStats `json:"model_stats"`
	URLStats   channelEditorURLStats   `json:"url_stats"`
	Features   channelEditorFeatures   `json:"features"`
}

// HandleChannelEditor 聚合编辑器首次打开所需的数据，避免前端拼装多个快照。
// GET /admin/channels/:id/editor
func (s *Server) HandleChannelEditor(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, fmt.Errorf("channel not found"))
		return
	}
	detail, apiKeys, err := s.buildChannelDetail(c.Request.Context(), id, cfg)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	modelStats := channelEditorModelStats{Available: true, Items: make([]ChannelModelStats, 0)}
	if stats, statsErr := s.getChannelModelStats(c.Request.Context(), id); statsErr != nil {
		modelStats.Available = false
		log.Printf("[WARN] 查询渠道模型统计失败 (channel=%d): %v", id, statsErr)
	} else {
		modelStats.Items = stats
	}

	urlStats := channelEditorURLStats{Items: make([]URLStat, 0)}
	if s.urlSelector != nil {
		urlStats.Available = true
		urlStats.Items = s.urlSelector.GetURLStats(id, cfg.GetURLs())
	}

	scheduledCheckEnabled := false
	if s.configService != nil {
		scheduledCheckEnabled = s.configService.GetFloat("channel_check_interval_hours", defaultChannelCheckIntervalHours) > 0
	}

	RespondJSON(c, http.StatusOK, channelEditorData{
		Channel:    detail,
		Keys:       apiKeys,
		ModelStats: modelStats,
		URLStats:   urlStats,
		Features: channelEditorFeatures{
			ScheduledCheckEnabled: scheduledCheckEnabled,
		},
	})
}
