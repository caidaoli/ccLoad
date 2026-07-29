package cooldown

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

const (
	maxCooldownDetectionRules        = 32
	maxCooldownDetectionRuleNameSize = 128
	maxCooldownDetectionPatternSize  = 1024
	maxCooldownDetectionTimeField    = 128
	maxCooldownDetectionDuration     = 366 * 24 * time.Hour
	maxCooldownDetectionCacheEntries = 128
	maxCooldownDetectionCacheKeySize = 64 * 1024
)

// DetectionInput contains the upstream response fields available to a
// configured cooldown rule. Network failures deliberately do not enter this
// matcher because they do not have a trustworthy upstream error body.
type DetectionInput struct {
	StatusCode int
	ErrorBody  []byte
}

// DetectionEvaluation explains how configured rules evaluated one upstream
// response. A matched but non-actionable dynamic rule must fall back to the
// built-in classifier instead of guessing a cooldown time.
type DetectionEvaluation struct {
	Code           string
	Message        string
	Matched        bool
	Actionable     bool
	Priority       int
	Scope          string
	Mode           string
	CooldownUntil  time.Time
	Captures       map[string]string
	FallbackReason string
}

type compiledCooldownDetectionRule struct {
	rule           model.CooldownDetectionRule
	messagePattern *regexp.Regexp
	location       *time.Location
}

var cooldownDetectionRuleCache = struct {
	sync.RWMutex
	entries map[string][]*compiledCooldownDetectionRule
}{
	entries: make(map[string][]*compiledCooldownDetectionRule),
}

// NormalizeCooldownDetectionRules validates and canonicalizes a channel-local
// rule set before it is persisted. The rules slice order is the only priority
// source of truth: after sorting by submitted priority it is renumbered 0..N-1.
func NormalizeCooldownDetectionRules(rules *model.CooldownDetectionRules) error {
	if rules == nil || rules.IsEmpty() {
		return nil
	}
	if len(rules.Rules) > maxCooldownDetectionRules {
		return fmt.Errorf("cooldown_detection_rules: too many entries (max %d)", maxCooldownDetectionRules)
	}

	priorities := make(map[int]struct{}, len(rules.Rules))
	for i := range rules.Rules {
		if rules.Rules[i].Priority < 0 {
			return fmt.Errorf("cooldown_detection_rules.rules[%d]: priority must be >= 0", i)
		}
		if _, exists := priorities[rules.Rules[i].Priority]; exists {
			return fmt.Errorf("cooldown_detection_rules.rules[%d]: duplicate priority %d", i, rules.Rules[i].Priority)
		}
		priorities[rules.Rules[i].Priority] = struct{}{}
		if _, err := normalizeCooldownDetectionRule(&rules.Rules[i], fmt.Sprintf("cooldown_detection_rules.rules[%d]", i)); err != nil {
			return err
		}
	}

	sort.SliceStable(rules.Rules, func(i, j int) bool {
		return rules.Rules[i].Priority < rules.Rules[j].Priority
	})
	for i := range rules.Rules {
		rules.Rules[i].Priority = i
	}
	return nil
}

func normalizeCooldownDetectionRule(rule *model.CooldownDetectionRule, path string) (*compiledCooldownDetectionRule, error) {
	rule.Name = strings.TrimSpace(rule.Name)
	rule.MessagePattern = strings.TrimSpace(rule.MessagePattern)
	rule.Scope = strings.ToLower(strings.TrimSpace(rule.Scope))
	rule.Mode = strings.ToLower(strings.TrimSpace(rule.Mode))
	rule.TimeFormat = strings.ToLower(strings.TrimSpace(rule.TimeFormat))

	if rule.Name == "" {
		return nil, fmt.Errorf("%s.name: required", path)
	}
	if len(rule.Name) > maxCooldownDetectionRuleNameSize {
		return nil, fmt.Errorf("%s.name: too long (max %d bytes)", path, maxCooldownDetectionRuleNameSize)
	}
	if len(rule.StatusCodes) == 0 && rule.MessagePattern == "" {
		return nil, fmt.Errorf("%s: at least one match condition is required", path)
	}
	if len(rule.StatusCodes) > 0 {
		seen := make(map[int]struct{}, len(rule.StatusCodes))
		for i, statusCode := range rule.StatusCodes {
			if statusCode < 100 || statusCode > 599 {
				return nil, fmt.Errorf("%s.status_codes[%d]: must be between 100 and 599", path, i)
			}
			if _, exists := seen[statusCode]; exists {
				return nil, fmt.Errorf("%s.status_codes[%d]: duplicate status code %d", path, i, statusCode)
			}
			seen[statusCode] = struct{}{}
		}
		sort.Ints(rule.StatusCodes)
	}

	if rule.Scope != model.CooldownScopeKey && rule.Scope != model.CooldownScopeModel && rule.Scope != model.CooldownScopeChannel {
		return nil, fmt.Errorf("%s.scope: invalid value %q (allowed: key, model, channel)", path, rule.Scope)
	}
	if rule.Mode != model.CooldownModeFixed && rule.Mode != model.CooldownModeResetTime {
		return nil, fmt.Errorf("%s.mode: invalid value %q (allowed: fixed, reset_time)", path, rule.Mode)
	}

	compiled := &compiledCooldownDetectionRule{rule: *rule}
	if rule.MessagePattern != "" {
		if len(rule.MessagePattern) > maxCooldownDetectionPatternSize {
			return nil, fmt.Errorf("%s.message_pattern: too long (max %d bytes)", path, maxCooldownDetectionPatternSize)
		}
		pattern, err := regexp.Compile(rule.MessagePattern)
		if err != nil {
			return nil, fmt.Errorf("%s.message_pattern: invalid regex: %w", path, err)
		}
		if err := validateNamedCaptureGroups(pattern, path+".message_pattern"); err != nil {
			return nil, err
		}
		compiled.messagePattern = pattern
	}

	switch rule.Mode {
	case model.CooldownModeFixed:
		if rule.CooldownSeconds <= 0 {
			return nil, fmt.Errorf("%s.cooldown_seconds: must be > 0 for fixed mode", path)
		}
		if durationExceedsMax(rule.CooldownSeconds) {
			return nil, fmt.Errorf("%s.cooldown_seconds: exceeds maximum %s", path, maxCooldownDetectionDuration)
		}
		rule.TimeCapture = ""
		rule.TimeFormat = ""
		rule.TimeLayout = ""
		rule.Timezone = ""
	case model.CooldownModeResetTime:
		if compiled.messagePattern == nil {
			return nil, fmt.Errorf("%s.message_pattern: required for reset_time mode", path)
		}
		rule.TimeCapture = strings.TrimSpace(rule.TimeCapture)
		rule.TimeLayout = strings.TrimSpace(rule.TimeLayout)
		rule.Timezone = strings.TrimSpace(rule.Timezone)
		if rule.TimeFormat == "" {
			rule.TimeFormat = model.CooldownTimeFormatDateTime
		}
		if !isValidCaptureName(rule.TimeCapture) {
			return nil, fmt.Errorf("%s.time_capture: must be a named regex capture", path)
		}
		if compiled.messagePattern.SubexpIndex(rule.TimeCapture) < 0 {
			return nil, fmt.Errorf("%s.time_capture: %q is not present in message_pattern", path, rule.TimeCapture)
		}
		switch rule.TimeFormat {
		case model.CooldownTimeFormatDateTime, model.CooldownTimeFormatTimeOfDay:
			if rule.TimeLayout == "" || len(rule.TimeLayout) > maxCooldownDetectionTimeField {
				return nil, fmt.Errorf("%s.time_layout: required and limited to %d bytes", path, maxCooldownDetectionTimeField)
			}
			if rule.Timezone == "" || len(rule.Timezone) > maxCooldownDetectionTimeField {
				return nil, fmt.Errorf("%s.timezone: required and limited to %d bytes", path, maxCooldownDetectionTimeField)
			}
			location, err := time.LoadLocation(rule.Timezone)
			if err != nil {
				return nil, fmt.Errorf("%s.timezone: invalid IANA timezone %q", path, rule.Timezone)
			}
			compiled.location = location
		case model.CooldownTimeFormatUnix, model.CooldownTimeFormatUnixMilliseconds, model.CooldownTimeFormatDurationSeconds:
			rule.TimeLayout = ""
			rule.Timezone = ""
		default:
			return nil, fmt.Errorf("%s.time_format: invalid value %q (allowed: datetime, time_of_day, unix, unix_ms, duration_seconds)", path, rule.TimeFormat)
		}
		rule.CooldownSeconds = 0
	}

	compiled.rule = *rule
	compiled.rule.StatusCodes = append([]int(nil), rule.StatusCodes...)
	return compiled, nil
}

func durationExceedsMax(seconds int64) bool {
	return seconds > int64(maxCooldownDetectionDuration/time.Second)
}

func (r *compiledCooldownDetectionRule) parseCapturedResetTime(capture string, now time.Time) (time.Time, string) {
	capture = strings.TrimSpace(capture)
	switch r.rule.TimeFormat {
	case model.CooldownTimeFormatDateTime:
		until, err := time.ParseInLocation(r.rule.TimeLayout, capture, r.location)
		if err != nil {
			return time.Time{}, "reset_time_parse_failed"
		}
		return until, ""
	case model.CooldownTimeFormatTimeOfDay:
		clock, err := time.ParseInLocation(r.rule.TimeLayout, capture, r.location)
		if err != nil {
			return time.Time{}, "reset_time_parse_failed"
		}
		localNow := now.In(r.location)
		until := time.Date(
			localNow.Year(), localNow.Month(), localNow.Day(),
			clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(),
			r.location,
		)
		if !until.After(localNow) {
			until = until.AddDate(0, 0, 1)
		}
		return until, ""
	case model.CooldownTimeFormatUnix:
		seconds, err := strconv.ParseInt(capture, 10, 64)
		if err != nil {
			return time.Time{}, "reset_time_parse_failed"
		}
		return time.Unix(seconds, 0).UTC(), ""
	case model.CooldownTimeFormatUnixMilliseconds:
		milliseconds, err := strconv.ParseInt(capture, 10, 64)
		if err != nil {
			return time.Time{}, "reset_time_parse_failed"
		}
		return time.UnixMilli(milliseconds).UTC(), ""
	case model.CooldownTimeFormatDurationSeconds:
		seconds, err := strconv.ParseInt(capture, 10, 64)
		if err != nil {
			return time.Time{}, "reset_time_parse_failed"
		}
		if seconds <= 0 {
			return time.Time{}, "reset_time_not_future"
		}
		if durationExceedsMax(seconds) {
			return time.Time{}, "reset_time_too_far"
		}
		return now.Add(time.Duration(seconds) * time.Second), ""
	default:
		return time.Time{}, "reset_time_parse_failed"
	}
}

func validateNamedCaptureGroups(pattern *regexp.Regexp, path string) error {
	seen := make(map[string]struct{})
	for _, name := range pattern.SubexpNames() {
		if name == "" {
			continue
		}
		if !isValidCaptureName(name) {
			return fmt.Errorf("%s: invalid named capture %q", path, name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%s: duplicate named capture %q", path, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func isValidCaptureName(name string) bool {
	if name == "" {
		return false
	}
	for i, ch := range name {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' || (i > 0 && ch >= '0' && ch <= '9') {
			continue
		}
		return false
	}
	return true
}

// EvaluateCooldownDetectionRules evaluates the configured rules in
// priority order. It does not persist state and can therefore be shared by the
// proxy path and the admin rule-test endpoint.
func EvaluateCooldownDetectionRules(rules *model.CooldownDetectionRules, input DetectionInput, now time.Time) DetectionEvaluation {
	code, message := util.ExtractUpstreamErrorCodeAndMessage(input.ErrorBody)
	evaluation := DetectionEvaluation{Code: code, Message: message}
	if rules == nil || rules.IsEmpty() {
		return evaluation
	}

	compiledRules, err := compileCooldownDetectionRules(rules)
	if err != nil {
		return evaluation
	}
	for _, compiled := range compiledRules {
		if !compiled.rule.Enabled {
			continue
		}
		if !compiled.matches(input.StatusCode, message) {
			continue
		}

		evaluation.Matched = true
		evaluation.Priority = compiled.rule.Priority
		evaluation.Scope = compiled.rule.Scope
		evaluation.Mode = compiled.rule.Mode
		captures := compiled.captures(message)
		evaluation.Captures = captures

		if compiled.rule.Mode == model.CooldownModeFixed {
			evaluation.Actionable = true
			evaluation.CooldownUntil = now.Add(time.Duration(compiled.rule.CooldownSeconds) * time.Second)
			return evaluation
		}

		capture := captures[compiled.rule.TimeCapture]
		until, fallbackReason := compiled.parseCapturedResetTime(capture, now)
		if fallbackReason != "" {
			evaluation.FallbackReason = fallbackReason
			return evaluation
		}
		if !until.After(now) {
			evaluation.FallbackReason = "reset_time_not_future"
			return evaluation
		}
		if until.Sub(now) > maxCooldownDetectionDuration {
			evaluation.FallbackReason = "reset_time_too_far"
			return evaluation
		}
		evaluation.Actionable = true
		evaluation.CooldownUntil = until
		return evaluation
	}
	return evaluation
}

func compileCooldownDetectionRules(rules *model.CooldownDetectionRules) ([]*compiledCooldownDetectionRule, error) {
	cacheKey := cooldownDetectionRulesCacheKey(rules)
	if cacheKey != "" {
		cooldownDetectionRuleCache.RLock()
		cached, ok := cooldownDetectionRuleCache.entries[cacheKey]
		cooldownDetectionRuleCache.RUnlock()
		if ok {
			return cached, nil
		}
	}

	ordered := append([]model.CooldownDetectionRule(nil), rules.Rules...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority < ordered[j].Priority
	})
	compiledRules := make([]*compiledCooldownDetectionRule, 0, len(ordered))
	for index := range ordered {
		rule := ordered[index]
		rule.StatusCodes = append([]int(nil), rule.StatusCodes...)
		compiled, err := normalizeCooldownDetectionRule(&rule, fmt.Sprintf("cooldown_detection_rules.rules[%d]", index))
		if err != nil {
			return nil, err
		}
		compiledRules = append(compiledRules, compiled)
	}

	if cacheKey == "" {
		return compiledRules, nil
	}
	cooldownDetectionRuleCache.Lock()
	defer cooldownDetectionRuleCache.Unlock()
	if cached, ok := cooldownDetectionRuleCache.entries[cacheKey]; ok {
		return cached, nil
	}
	if len(cooldownDetectionRuleCache.entries) >= maxCooldownDetectionCacheEntries {
		cooldownDetectionRuleCache.entries = make(map[string][]*compiledCooldownDetectionRule)
	}
	cooldownDetectionRuleCache.entries[cacheKey] = compiledRules
	return compiledRules, nil
}

func cooldownDetectionRulesCacheKey(rules *model.CooldownDetectionRules) string {
	encoded, err := json.Marshal(rules)
	if err != nil || len(encoded) > maxCooldownDetectionCacheKeySize {
		return ""
	}
	return string(encoded)
}

func (r *compiledCooldownDetectionRule) matches(statusCode int, message string) bool {
	if len(r.rule.StatusCodes) > 0 && !containsStatusCode(r.rule.StatusCodes, statusCode) {
		return false
	}
	return r.messagePattern == nil || r.messagePattern.MatchString(message)
}

func (r *compiledCooldownDetectionRule) captures(message string) map[string]string {
	if r.messagePattern == nil {
		return nil
	}
	values := r.messagePattern.FindStringSubmatch(message)
	if values == nil {
		return nil
	}
	names := r.messagePattern.SubexpNames()
	var captures map[string]string
	for i := 1; i < len(values) && i < len(names); i++ {
		if names[i] == "" {
			continue
		}
		if captures == nil {
			captures = make(map[string]string)
		}
		captures[names[i]] = values[i]
	}
	return captures
}

func containsStatusCode(statusCodes []int, target int) bool {
	index := sort.SearchInts(statusCodes, target)
	return index < len(statusCodes) && statusCodes[index] == target
}
