package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"
	"ccLoad/internal/zaiauth"
	"ccLoad/internal/zedauth"

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

// channelManagementEditorView is returned only by the authenticated channel
// editor endpoint. List/detail responses keep using channelManagementView,
// which intentionally contains only configuration flags and runtime state.
type channelManagementEditorView struct {
	*channelManagementView
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Email        string `json:"email,omitempty"`
	Password     string `json:"password,omitempty"`
	UserID       *int64 `json:"user_id,omitempty"`
}

type channelEditorData struct {
	Channel             ChannelWithCooldown          `json:"channel"`
	Keys                []*model.APIKey              `json:"keys"`
	ManagementAccount   *channelManagementEditorView `json:"management_account,omitempty"`
	OAuthCredential     json.RawMessage              `json:"oauth_credential,omitempty"`
	OAuthCredentialInfo *codexauth.IDTokenInfo       `json:"oauth_credential_info,omitempty"`
	ModelStats          channelEditorModelStats      `json:"model_stats"`
	URLStats            channelEditorURLStats        `json:"url_stats"`
	Features            channelEditorFeatures        `json:"features"`
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
	var oauthCredential json.RawMessage
	var oauthCredentialInfo *codexauth.IDTokenInfo
	if cfg.UsesCodexOAuth() {
		credential, parseErr := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
		oauthCredentialInfo = credential.DecodedIDToken()
	} else if cfg.UsesAntigravityOAuth() {
		_, parseErr := antigravityauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	} else if cfg.UsesXAIOAuth() {
		if _, parseErr := xaiauth.ParseCredential([]byte(cfg.OAuthCredential)); parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	} else if cfg.UsesAnthropicOAuth() {
		_, parseErr := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	} else if cfg.UsesZAIOAuth() {
		_, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	} else if cfg.UsesCursorOAuth() {
		_, parseErr := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	} else if cfg.UsesZedOAuth() {
		_, parseErr := zedauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	}
	if cfg.UsesOAuth() {
		// 编辑器与普通 Key 端点共用同一份 OAuth 合成行：凭证掩码、备注和当前倍率必须一致。
		apiKeys, err = channelKeysForAdmin(cfg, nil)
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
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

	var managementAccount *channelManagementEditorView
	if detail.ManagementAccount != nil {
		envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		managementAccount = &channelManagementEditorView{
			channelManagementView: detail.ManagementAccount,
		}
		switch envelope.Profile {
		case model.ChannelManagementProfileNewAPI:
			managementAccount.AccessToken = envelope.Settings.AccessToken
			managementAccount.UserID = envelope.Settings.UserID
		case model.ChannelManagementProfileSub2API, model.ChannelManagementProfileSub2APIPro:
			managementAccount.RefreshToken = envelope.Settings.RefreshToken
			managementAccount.Email = envelope.Settings.Email
			managementAccount.Password = envelope.Settings.Password
		}
	}

	scheduledCheckEnabled := false
	if s.configService != nil {
		hours := normalizeChannelCheckIntervalHours(
			s.configService.GetFloat("channel_check_interval_hours", defaultChannelCheckIntervalHours),
		)
		scheduledCheckEnabled = hours > 0
	}

	RespondJSON(c, http.StatusOK, channelEditorData{
		Channel:             detail,
		Keys:                apiKeys,
		ManagementAccount:   managementAccount,
		OAuthCredential:     oauthCredential,
		OAuthCredentialInfo: oauthCredentialInfo,
		ModelStats:          modelStats,
		URLStats:            urlStats,
		Features: channelEditorFeatures{
			ScheduledCheckEnabled: scheduledCheckEnabled,
		},
	})
}
