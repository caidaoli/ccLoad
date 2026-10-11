package app

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ccLoad/internal/protocol"

	"github.com/gin-gonic/gin"
)

type tokenRPMExceededError struct {
	retryAfter time.Duration
}

func (e *tokenRPMExceededError) Error() string {
	return "Token RPM limit exceeded"
}

func (e *tokenRPMExceededError) retryAfterSeconds() int {
	return max(1, int((e.retryAfter+time.Second-1)/time.Second))
}

func (s *AuthService) reserveTokenRPM(tokenID int64) *tokenRPMExceededError {
	if s == nil || tokenID <= 0 {
		return nil
	}
	s.authTokensMux.RLock()
	defer s.authTokensMux.RUnlock()
	reservation := s.tokenRPMLimiter.reserve(tokenID, s.authTokenRPMLimits[tokenID])
	if reservation.allowed {
		return nil
	}
	return &tokenRPMExceededError{retryAfter: reservation.retryAfter}
}

// Request admission follows global and token concurrency admission. It runs
// once before body parsing, so later failures and upstream retries keep their
// original reservation. Read-only routes and unsupported request families do
// not consume the token's rolling window.
func (s *Server) admitHTTPTokenRPM(c *gin.Context) bool {
	_, path := clientRequestMetadata(c)
	if c.Request.Method != http.MethodPost || protocol.DetectRequestFamily(path) == protocol.RequestFamilyUnknown {
		return true
	}
	if rpmErr := s.authService.reserveTokenRPM(c.GetInt64("token_id")); rpmErr != nil {
		seconds := rpmErr.retryAfterSeconds()
		c.Header("Retry-After", strconv.Itoa(seconds))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{
				"type": "rate_limit_error", "code": "token_rpm_exceeded",
				"message": fmt.Sprintf("Token RPM limit exceeded; retry in %d seconds", seconds),
			},
		})
		return false
	}
	return true
}
