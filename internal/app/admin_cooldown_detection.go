package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

const (
	globalCooldownDetectionRulesSettingKey = "global_cooldown_detection_rules"
	maxCooldownDetectionTestBodyBytes      = 256 * 1024
	maxGlobalCooldownDetectionRulesBytes   = 64 * 1024
	cooldownDetectionRulesSourceChannel    = "channel"
	cooldownDetectionRulesSourceGlobal     = "global"
)

var upstreamStatusLogPattern = regexp.MustCompile(`(?i)\bupstream\s+status\s+(\d{3})\s*:`)

func parseGlobalCooldownDetectionRules(value string) (*model.CooldownDetectionRules, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" || value == "{}" {
		return nil, nil
	}
	if len(value) > maxGlobalCooldownDetectionRulesBytes {
		return nil, fmt.Errorf("exceeds maximum size of %d bytes", maxGlobalCooldownDetectionRulesBytes)
	}

	var rules model.CooldownDetectionRules
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return nil, fmt.Errorf("invalid cooldown detection rules JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid cooldown detection rules JSON: trailing data")
	}
	if err := cooldown.NormalizeCooldownDetectionRules(&rules); err != nil {
		return nil, err
	}
	if rules.IsEmpty() {
		return nil, nil
	}
	return &rules, nil
}

// cooldownDetectionTestRequest is intentionally independent of a persisted
// channel. The editor must be able to test unsaved rule drafts without touching
// any cooldown state.
type cooldownDetectionTestRequest struct {
	CooldownDetectionRules *model.CooldownDetectionRules `json:"cooldown_detection_rules"`
	RulesSource            string                        `json:"rules_source"`
	StatusCode             int                           `json:"status_code"`
	ErrorBody              string                        `json:"error_body"`
	parsedLog              bool
}

func (r *cooldownDetectionTestRequest) Validate() error {
	if len(r.ErrorBody) > maxCooldownDetectionTestBodyBytes {
		return fmt.Errorf("error_body exceeds maximum size of %d bytes", maxCooldownDetectionTestBodyBytes)
	}
	statusCode, errorBody, parsedLog, err := normalizeCooldownDetectionTestInput(r.StatusCode, r.ErrorBody)
	if err != nil {
		return err
	}
	r.StatusCode = statusCode
	r.ErrorBody = errorBody
	r.parsedLog = parsedLog
	r.RulesSource = strings.ToLower(strings.TrimSpace(r.RulesSource))
	if r.RulesSource == "" {
		r.RulesSource = cooldownDetectionRulesSourceChannel
	}
	if r.RulesSource != cooldownDetectionRulesSourceChannel && r.RulesSource != cooldownDetectionRulesSourceGlobal {
		return fmt.Errorf("rules_source must be channel or global")
	}
	if r.StatusCode < http.StatusContinue || r.StatusCode > 599 {
		return fmt.Errorf("status_code must be between 100 and 599")
	}
	return cooldown.NormalizeCooldownDetectionRules(r.CooldownDetectionRules)
}

// normalizeCooldownDetectionTestInput accepts either a raw upstream response
// body or ccLoad's canonical "upstream status N: body" log message. A parsed
// log status is authoritative because it describes the response being tested.
func normalizeCooldownDetectionTestInput(statusCode int, input string) (int, string, bool, error) {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return statusCode, input, false, nil
	}

	indices := upstreamStatusLogPattern.FindStringSubmatchIndex(input)
	if indices == nil {
		return statusCode, input, false, nil
	}
	parsedStatus, err := strconv.Atoi(input[indices[2]:indices[3]])
	if err != nil || parsedStatus < http.StatusContinue || parsedStatus > 599 {
		return 0, "", false, fmt.Errorf("upstream log status must be between 100 and 599")
	}
	body := strings.TrimSpace(input[indices[1]:])
	if body == "" {
		return 0, "", false, fmt.Errorf("upstream log response body is required")
	}
	return parsedStatus, body, true, nil
}

type cooldownDetectionTestResponse struct {
	Code                  string            `json:"code,omitempty"`
	Message               string            `json:"message,omitempty"`
	RulesSource           string            `json:"rules_source"`
	StatusCode            int               `json:"status_code"`
	ParsedLog             bool              `json:"parsed_log"`
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

// HandleCooldownDetectionTest evaluates unsaved cooldown rules. An empty
// channel draft inherits the same process-level global rules as the proxy path.
// It deliberately has no channel ID and never changes real cooldown state.
func (s *Server) HandleCooldownDetectionTest(c *gin.Context) {
	var req cooldownDetectionTestRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	effectiveRules := req.CooldownDetectionRules
	effectiveSource := req.RulesSource
	if effectiveSource == cooldownDetectionRulesSourceChannel {
		var inherited bool
		effectiveRules, inherited = s.effectiveChannelCooldownDetectionRules(effectiveRules)
		if inherited {
			effectiveSource = cooldownDetectionRulesSourceGlobal
		}
	}

	evaluation := cooldown.EvaluateCooldownDetectionRules(effectiveRules, cooldown.DetectionInput{
		StatusCode: req.StatusCode,
		ErrorBody:  []byte(req.ErrorBody),
	}, time.Now())

	response := cooldownDetectionTestResponse{
		Code:              evaluation.Code,
		Message:           evaluation.Message,
		RulesSource:       effectiveSource,
		StatusCode:        req.StatusCode,
		ParsedLog:         req.parsedLog,
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
