package app

import (
	"net/http"

	"ccLoad/internal/codexauth"

	"github.com/gin-gonic/gin"
)

type codexQuotaOverdraftSettingsRequest struct {
	Enabled *bool `json:"enabled"`
}

type codexQuotaOverdraftSettingsResponse struct {
	QuotaOverdraft *codexauth.QuotaOverdraft `json:"quota_overdraft"`
}

// HandleUpdateCodexQuotaOverdraft updates only the private Codex credential.
// PUT /admin/channels/:id/codex-quota-overdraft
func (s *Server) HandleUpdateCodexQuotaOverdraft(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	var req codexQuotaOverdraftSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		RespondErrorMsg(c, http.StatusBadRequest, "enabled is required")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesCodexOAuth() {
		RespondErrorMsg(c, http.StatusBadRequest, "channel is not Codex OAuth")
		return
	}
	if s.codexCredentials == nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "Codex credential manager is unavailable")
		return
	}
	overdraft, err := s.codexCredentials.setQuotaOverdraftEnabled(c.Request.Context(), id, *req.Enabled)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, codexQuotaOverdraftSettingsResponse{QuotaOverdraft: overdraft})
}
