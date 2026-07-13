package app

import (
	"errors"
	"net/http"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

const webIdentityContextKey = "ccLoad.webIdentity"

// WebIdentity is the authorization scope attached to an authenticated web request.
type WebIdentity struct {
	Role        model.WebRole `json:"role"`
	AuthTokenID int64         `json:"auth_token_id,omitempty"`
}

// WebIdentityFromContext returns the authenticated Web identity.
func WebIdentityFromContext(c *gin.Context) (WebIdentity, bool) {
	value, ok := c.Get(webIdentityContextKey)
	if !ok {
		return WebIdentity{}, false
	}
	identity, ok := value.(WebIdentity)
	return identity, ok
}

// HandleWebSession returns non-sensitive information about the current session.
func (s *AuthService) HandleWebSession(c *gin.Context) {
	identity, ok := WebIdentityFromContext(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "未授权访问，请先登录")
		return
	}
	if identity.Role == model.WebRoleAdmin {
		RespondJSON(c, http.StatusOK, gin.H{"role": model.WebRoleAdmin})
		return
	}

	token, err := s.store.GetAuthToken(c.Request.Context(), identity.AuthTokenID)
	if err != nil {
		if errors.Is(err, model.ErrAuthTokenNotFound) {
			RespondErrorMsg(c, http.StatusUnauthorized, "API Token 已失效")
			return
		}
		RespondErrorMsg(c, http.StatusInternalServerError, "读取 API Token 会话失败")
		return
	}
	if token == nil || !token.IsValid() {
		RespondErrorMsg(c, http.StatusUnauthorized, "API Token 已失效")
		return
	}
	allowedModels := token.AllowedModels
	if allowedModels == nil {
		allowedModels = make([]string, 0)
	}
	RespondJSON(c, http.StatusOK, gin.H{
		"role":            model.WebRoleAPIToken,
		"auth_token_id":   token.ID,
		"description":     token.Description,
		"allowed_models":  allowedModels,
		"cost_used_usd":   token.CostUsedUSD(),
		"cost_limit_usd":  token.CostLimitUSD(),
		"max_concurrency": token.MaxConcurrency,
	})
}
