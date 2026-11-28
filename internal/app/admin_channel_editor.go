package app

import (
	"encoding/json"
	"log"
	"net/http"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type channelEditorModelStats struct {
	Available bool                `json:"available"`
	Items     []ChannelModelStats `json:"items"`
}

type channelEditorURLStats struct {
	Items []URLStat `json:"items"`
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
	Version             string                       `json:"version"` ///< 编辑版本，保存时加引号作为强 ETag 放入 If-Match；只放响应体，代理压缩会弱化或删除 ETag 头
}

/**
 * @brief 聚合渠道编辑器的配置、凭证与统计，避免前端拼装不同时间的快照。
 * @param c 携带路由参数、管理员身份和响应写入器的 HTTP 上下文。
 * @note 处理 GET /admin/channels/:id/editor；私有凭证的可见性由访问权限决定，列表摘要不替代此接口。
 */
func (s *Server) HandleChannelEditor(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}

	cfg, apiKeys, version, err := s.store.GetChannelEditSnapshot(c.Request.Context(), id)
	if err != nil {
		respondChannelEditError(c, err)
		return
	}
	detail, err := s.buildChannelDetail(c.Request.Context(), cfg, apiKeys)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	// 凭证合法性由 channelKeysForAdmin 解析合成 Key 时校验；Session 等原始凭证仅在管理员编辑器返回。
	var oauthCredential json.RawMessage
	var oauthCredentialInfo *codexauth.IDTokenInfo
	if cfg.UsesOAuth() {
		oauthCredential = append(json.RawMessage(nil), cfg.OAuthCredential...)
	}
	if cfg.UsesCodexOAuth() {
		credential, parseErr := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil {
			RespondError(c, http.StatusInternalServerError, parseErr)
			return
		}
		oauthCredentialInfo = credential.DecodedIDToken()
	}
	apiKeys, err = channelKeysForAdmin(cfg, apiKeys)
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

	urlStats := channelEditorURLStats{Items: s.urlSelector.GetURLStats(id, cfg.GetURLs())}

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

	c.Header("Cache-Control", "no-store")
	RespondJSON(c, http.StatusOK, channelEditorData{
		Channel:             detail,
		Keys:                apiKeys,
		ManagementAccount:   managementAccount,
		OAuthCredential:     oauthCredential,
		OAuthCredentialInfo: oauthCredentialInfo,
		ModelStats:          modelStats,
		URLStats:            urlStats,
		Version:             version,
	})
}
