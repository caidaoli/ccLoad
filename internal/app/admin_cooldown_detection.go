package app

import (
	"fmt"
	"net/http"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

const maxCooldownDetectionTestBodyBytes = 256 * 1024

// cooldownDetectionTestRequest is intentionally independent of a persisted
// channel. The editor must be able to test unsaved rule drafts without touching
// any cooldown state.
type cooldownDetectionTestRequest struct {
	CooldownDetectionRules *model.CooldownDetectionRules `json:"cooldown_detection_rules"`
	StatusCode             int                           `json:"status_code"`
	ErrorBody              string                        `json:"error_body"`
}

func (r *cooldownDetectionTestRequest) Validate() error {
	if r.StatusCode < http.StatusContinue || r.StatusCode > 599 {
		return fmt.Errorf("status_code must be between 100 and 599")
	}
	if len(r.ErrorBody) > maxCooldownDetectionTestBodyBytes {
		return fmt.Errorf("error_body exceeds maximum size of %d bytes", maxCooldownDetectionTestBodyBytes)
	}
	return cooldown.NormalizeCooldownDetectionRules(r.CooldownDetectionRules)
}

type cooldownDetectionTestResponse struct {
	Code                  string            `json:"code,omitempty"`
	Message               string            `json:"message,omitempty"`
	Matched               bool              `json:"matched"`
	Actionable            bool              `json:"actionable"`
	Priority              *int              `json:"priority,omitempty"`
	Scope                 string            `json:"scope,omitempty"`
	Mode                  string            `json:"mode,omitempty"`
	CooldownUntil         *time.Time        `json:"cooldown_until,omitempty"`
	Captures              map[string]string `json:"captures,omitempty"`
	FallbackToBuiltin     bool              `json:"fallback_to_builtin"`
	BuiltinFallbackReason string            `json:"builtin_fallback_reason,omitempty"`
}

// HandleCooldownDetectionTest evaluates unsaved channel-local cooldown rules.
// It deliberately has no channel ID and no storage dependency: it must never
// create or change a real cooldown while the user is editing a channel.
func (s *Server) HandleCooldownDetectionTest(c *gin.Context) {
	var req cooldownDetectionTestRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	evaluation := cooldown.EvaluateCooldownDetectionRules(req.CooldownDetectionRules, cooldown.DetectionInput{
		StatusCode: req.StatusCode,
		ErrorBody:  []byte(req.ErrorBody),
	}, time.Now())

	response := cooldownDetectionTestResponse{
		Code:              evaluation.Code,
		Message:           evaluation.Message,
		Matched:           evaluation.Matched,
		Actionable:        evaluation.Actionable,
		Scope:             evaluation.Scope,
		Mode:              evaluation.Mode,
		Captures:          evaluation.Captures,
		FallbackToBuiltin: !evaluation.Actionable,
	}
	if evaluation.Matched {
		priority := evaluation.Priority
		response.Priority = &priority
	}
	if evaluation.Actionable {
		until := evaluation.CooldownUntil
		response.CooldownUntil = &until
	} else if evaluation.FallbackReason != "" {
		response.BuiltinFallbackReason = evaluation.FallbackReason
	} else {
		response.BuiltinFallbackReason = "no_configured_rule_match"
	}

	RespondJSON(c, http.StatusOK, response)
}
