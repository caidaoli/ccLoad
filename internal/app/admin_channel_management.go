package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func (s *Server) rollbackCreatedChannel(ctx context.Context, channelID int64) {
	if err := s.store.DeleteConfig(ctx, channelID); err != nil {
		log.Printf("[ERROR] rollback channel after management account save failure (channel=%d): %v", channelID, err)
	}
}

func (s *Server) managementAccountView(cfg *model.Config) *channelManagementView {
	if s == nil || s.channelManagement == nil || !supportsChannelManagement(cfg) ||
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
	if !supportsChannelManagement(cfg) || strings.TrimSpace(cfg.OAuthCredential) == "" {
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
	case errors.Is(err, errManagementTwoFactorRequired):
		return http.StatusConflict, "totp_required"
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
	if detail := managementErrorDetail(err); detail != "" {
		RespondErrorMsg(c, status, detail)
		return
	}
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

/**
 * @brief 将管理账户签到结果与失败状态归一为渠道审计记录。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param result 管理账户签到结果，可为 nil。
 * @param operationErr 业务操作错误；成功时为 nil。
 * @return 序列化或写库失败时返回错误，成功返回 nil。
 * @note 写入签到来源的日志；结果缺失时由操作错误补充状态。
 */
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
	return s.writeChannelCheckinAuditLog(ctx, cfg, checkedAt, statusCode,
		channelCheckinAuditMessage{Profile: managementProfileForAudit(cfg), Status: status, Balance: balance})
}

/**
 * @brief 按统一渠道字段写入提供商的签到审计结果。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param checkedAt 提供商签到结果的采样时间。
 * @param statusCode 本次签到的 HTTP 或归一状态码。
 * @param payload 不含认证材料且可编码为 JSON 的提供商审计消息。
 * @return 编码或持久化失败时返回错误，成功返回 nil。
 * @note 写入渠道级日志并记录渠道倍率快照，避免零倍率被解释为免费请求。
 */
func (s *Server) writeChannelCheckinAuditLog(ctx context.Context, cfg *model.Config, checkedAt time.Time, statusCode int, payload any) error {
	message, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: checkedAt},
		ChannelID:  cfg.ID,
		StatusCode: statusCode,
		LogSource:  model.LogSourceCheckin,
		// 签到属于渠道级操作，倍率快照取渠道级，避免零值被渲染成免费渠道。
		CostMultiplier: cfg.CostMultiplier,
		Message:        string(message),
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

type sub2APILoginRequest struct {
	Profile  string `json:"profile"`
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
	ProxyURL string `json:"proxy_url"`
}

type sub2APILoginView struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    int64     `json:"account_id"`
}

// HandleChannelManagementSub2APILogin exchanges Sub2API email/password for a
// token session without writing the channel. The editor displays the refresh
// token and only persists it when the channel is saved.
func (s *Server) HandleChannelManagementSub2APILogin(c *gin.Context) {
	if s == nil || s.channelManagement == nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "uncertain")
		return
	}
	var req sub2APILoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid_response")
		return
	}
	session, err := s.channelManagement.PreviewSub2APILogin(c.Request.Context(), &model.Config{
		AuthType: model.AuthTypeAPIKey,
		ProxyURL: strings.TrimSpace(req.ProxyURL),
	}, &channelManagementInput{
		Profile: req.Profile, BaseURL: req.BaseURL, Email: req.Email, Password: req.Password, TOTPCode: req.TOTPCode,
	})
	if err != nil || session == nil {
		if err == nil {
			err = errInvalidManagementResponse
		}
		respondChannelManagementError(c, err)
		return
	}
	RespondJSON(c, http.StatusOK, sub2APILoginView{
		RefreshToken: session.RefreshToken,
		AccessToken:  session.AccessToken,
		ExpiresAt:    session.ExpiresAt,
		AccountID:    session.AccountID,
	})
}
