package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func prepareChannelManagementCreate(req *ChannelRequest) (string, error) {
	if req == nil || !req.managementAccountSet || req.ManagementAccount == nil {
		return "", nil
	}
	_, raw, err := mergeChannelManagementSettings("", req.ManagementAccount)
	return raw, err
}

func (s *Server) saveCreatedChannelManagement(ctx context.Context, cfg *model.Config, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	updated, err := s.store.CompareAndSwapChannelManagement(ctx, cfg.ID, "", raw)
	if err != nil {
		return err
	}
	if !updated {
		return errInvalidManagementRequest
	}
	cfg.OAuthCredential = raw
	return nil
}

func (s *Server) rollbackCreatedChannel(ctx context.Context, channelID int64) {
	if err := s.store.DeleteConfig(ctx, channelID); err != nil {
		log.Printf("[ERROR] rollback channel after management account save failure (channel=%d): %v", channelID, err)
	}
}

func (s *Server) managementAccountView(cfg *model.Config) *channelManagementView {
	if s == nil || s.channelManagement == nil || cfg == nil || cfg.AuthType != model.AuthTypeAPIKey ||
		strings.TrimSpace(cfg.OAuthCredential) == "" {
		return nil
	}
	view, err := s.channelManagement.View(cfg)
	if err != nil {
		log.Printf("[WARN] invalid channel management envelope omitted from admin response (channel=%d)", cfg.ID)
		return nil
	}
	return view
}

func (s *Server) requireConfiguredManagementAccount(c *gin.Context) (*model.Config, bool) {
	channelID, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid_response")
		return nil, false
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), channelID)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "credential_invalid")
		return nil, false
	}
	if cfg.AuthType != model.AuthTypeAPIKey || strings.TrimSpace(cfg.OAuthCredential) == "" {
		RespondErrorMsg(c, http.StatusConflict, "credential_invalid")
		return nil, false
	}
	return cfg, true
}

func channelManagementErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, errChannelManagementNotConfigured):
		return http.StatusConflict, "credential_invalid"
	case errors.Is(err, errChannelManagementProviderUnavailable):
		return http.StatusConflict, "unsupported"
	case errors.Is(err, errInvalidManagementRequest):
		return http.StatusBadRequest, "invalid_response"
	case errors.Is(err, errInvalidManagementResponse):
		return http.StatusBadGateway, "invalid_response"
	case errors.Is(err, errManagementRequestFailed), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusBadGateway, "upstream_error"
	default:
		return http.StatusInternalServerError, "uncertain"
	}
}

func respondChannelManagementError(c *gin.Context, err error) {
	status, code := channelManagementErrorCode(err)
	RespondErrorMsg(c, status, code)
}

type channelCheckinAuditBalance struct {
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`
}

type channelCheckinAuditMessage struct {
	Profile string                      `json:"profile"`
	Status  string                      `json:"status"`
	Balance *channelCheckinAuditBalance `json:"balance,omitempty"`
}

func (s *Server) addChannelCheckinAuditLog(ctx context.Context, cfg *model.Config, result *channelCheckinResult, operationErr error) error {
	status := ""
	statusCode := 0
	var balance *channelCheckinAuditBalance
	var checkedAt = s.channelManagement.now()
	if result != nil {
		status = result.Status
		statusCode = result.StatusCode
		if result.CheckedInAt != nil {
			checkedAt = *result.CheckedInAt
		}
		if result.Balance != nil {
			balance = &channelCheckinAuditBalance{Remaining: result.Balance.Remaining, Unit: result.Balance.Unit}
		}
	}
	if status == "" && operationErr != nil {
		_, status = channelManagementErrorCode(operationErr)
	}
	message, err := json.Marshal(channelCheckinAuditMessage{Profile: managementProfileForAudit(cfg), Status: status, Balance: balance})
	if err != nil {
		return err
	}
	return s.store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: checkedAt},
		ChannelID:  cfg.ID,
		StatusCode: statusCode,
		LogSource:  model.LogSourceCheckin,
		Message:    string(message),
	})
}

func managementProfileForAudit(cfg *model.Config) string {
	if cfg == nil {
		return ""
	}
	envelope, err := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if err != nil {
		return ""
	}
	return envelope.Profile
}

// HandleChannelManagementBalance refreshes the configured upstream account balance.
func (s *Server) HandleChannelManagementBalance(c *gin.Context) {
	cfg, ok := s.requireConfiguredManagementAccount(c)
	if !ok {
		return
	}
	view, err := s.channelManagement.RefreshBalance(c.Request.Context(), cfg.ID)
	if err != nil {
		respondChannelManagementError(c, err)
		return
	}
	RespondJSON(c, http.StatusOK, view)
}

// HandleChannelManagementCheckin performs one manual upstream check-in.
func (s *Server) HandleChannelManagementCheckin(c *gin.Context) {
	cfg, ok := s.requireConfiguredManagementAccount(c)
	if !ok {
		return
	}
	result, err := s.channelManagement.CheckIn(c.Request.Context(), cfg.ID)
	if auditErr := s.addChannelCheckinAuditLog(c.Request.Context(), cfg, result, err); auditErr != nil {
		respondChannelManagementError(c, auditErr)
		return
	}
	if err != nil {
		respondChannelManagementError(c, err)
		return
	}
	RespondJSON(c, http.StatusOK, result)
}
