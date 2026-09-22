package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/util"
)

type jevState struct {
	Status     int                `json:"status"`
	Code       string             `json:"code,omitempty"`
	Message    string             `json:"message,omitempty"`
	Headers    map[string]string  `json:"headers,omitempty"`
	Candidates []jevTimeCandidate `json:"time_candidates,omitempty"`
	Truncated  bool               `json:"truncated,omitempty"`
	receivedAt time.Time
}
type jevTimeCandidate struct {
	ID      string `json:"id"`
	Value   string `json:"value"`
	Context string `json:"context"`
	until   time.Time
}

var jevSensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s"',;]+`),
	regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret|cookie)["']?\s*[=:]\s*["']?[^\s"',;}]+`),
	regexp.MustCompile(`\b(?:sk-|jv_live_|eyJ)[A-Za-z0-9._-]+`),
	regexp.MustCompile(`(?i)https?://[^\s"'<>]+`),
	regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
}
var jevTimePattern = regexp.MustCompile(`(?i)\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}|\s+UTC(?:[+-]\d{1,2}(?::?\d{2})?)?)?|\d+(?:\.\d+)?\s*(?:milliseconds?|seconds?|minutes?|hours?|days?|ms|s|m|h|d)\b|\d+(?:\.\d+)?\s*(?:秒|分钟|小时|天)|\b\d{10,13}\b`)
var jevUTCTimePattern = regexp.MustCompile(`(?i)^(.+?)\s+UTC([+-])(\d{1,2})(?::?(\d{2}))?$`)
var jevSecondsPattern = regexp.MustCompile(`(?i)retry_after(?:_seconds)?:\s*(\d+(?:\.\d+)?)`)
var jevRelativeTimePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(milliseconds?|seconds?|minutes?|hours?|days?|ms|s|m|h|d|秒|分钟|小时|天)$`)

func sanitizeJevText(value string, secrets []string, limit int) string {
	// Redact before truncation so a known credential cannot be cut into an unrecognized prefix.
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	for _, pattern := range jevSensitivePatterns {
		value = pattern.ReplaceAllString(value, "[redacted]")
	}
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		value += "[truncated]"
	}
	return value
}

func buildJevState(in cooldown.ErrorInput, secrets []string) jevState {
	received := in.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	code, message := util.ExtractUpstreamErrorCodeAndMessage(in.ErrorBody)
	// Extract only error fields. Never forward an entire JSON response or request echo.
	if json.Valid(in.ErrorBody) {
		var root map[string]json.RawMessage
		if json.Unmarshal(in.ErrorBody, &root) == nil {
			selected := root
			if response, ok := root["response"]; ok {
				var nested map[string]json.RawMessage
				if json.Unmarshal(response, &nested) == nil && nested["error"] != nil {
					root = nested
					selected = nested
				}
			}
			if nested, ok := root["error"]; ok {
				var obj map[string]json.RawMessage
				if json.Unmarshal(nested, &obj) == nil && obj != nil {
					selected = obj
				}
			}
			message = ""
			var stringError string
			if json.Unmarshal(root["error"], &stringError) == nil {
				message = stringError + "\n"
			}
			for _, field := range []string{"message", "detail", "type", "code", "reset_at", "reset_time", "retry_after", "retry_after_seconds", "retryDelay", "retry_delay", "resets_at"} {
				raw := selected[field]
				if len(raw) == 0 {
					continue
				}
				var value string
				if json.Unmarshal(raw, &value) != nil {
					var number json.Number
					if json.Unmarshal(raw, &number) != nil {
						continue
					}
					value = string(number)
				}
				message += field + ": " + value + "\n"
			}
		}
	}
	state := jevState{Status: in.UpstreamStatusCode, Code: sanitizeJevText(code, secrets, 128), Message: sanitizeJevText(message, secrets, 4096), Headers: map[string]string{}, receivedAt: received, Truncated: len(message) > 4096}
	if state.Status == 0 {
		state.Status = in.StatusCode
	}
	for key, values := range in.Headers {
		lower := strings.ToLower(key)
		switch lower {
		case "retry-after", "x-ratelimit-reset", "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "ratelimit-reset", "anthropic-ratelimit-unified-reset", "x-ratelimit-scope":
			if len(values) > 0 {
				state.Headers[lower] = sanitizeJevText(values[0], secrets, 256)
			}
		}
	}
	add := func(value, context string, until time.Time) {
		if len(state.Candidates) >= 16 {
			return
		}
		state.Candidates = append(state.Candidates, jevTimeCandidate{ID: jevCandidateID(len(state.Candidates)), Value: value, Context: context, until: until})
	}
	keys := make([]string, 0, len(state.Headers))
	for key := range state.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := state.Headers[key]
		until := parseJevTime(value, received)
		if key == "retry-after" {
			if seconds, err := strconv.ParseFloat(value, 64); err == nil {
				if duration, ok := settingDurationFromFloat64(seconds, time.Second); ok {
					until = received.Add(duration)
				}
			} else if parsed, err := http.ParseTime(value); err == nil {
				until = parsed
			}
		}
		if key != "x-ratelimit-scope" {
			add(value, key+": "+value, until)
		}
	}
	for _, match := range jevSecondsPattern.FindAllStringSubmatch(state.Message, 16) {
		seconds, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		until := time.Time{}
		if duration, ok := settingDurationFromFloat64(seconds, time.Second); ok {
			until = received.Add(duration)
		}
		add(match[1], match[0], until)
	}
	for _, span := range jevTimePattern.FindAllStringIndex(state.Message, 16) {
		start := max(0, span[0]-80)
		end := min(len(state.Message), span[1]+80)
		value := state.Message[span[0]:span[1]]
		add(value, strings.ToValidUTF8(state.Message[start:end], ""), parseJevTime(value, received))
	}
	return state
}

func parseJevTime(value string, received time.Time) time.Time {
	value = strings.TrimSpace(value)
	if match := jevUTCTimePattern.FindStringSubmatch(value); match != nil {
		hours, _ := strconv.Atoi(match[3])
		minutes, _ := strconv.Atoi(match[4])
		if hours > 14 || minutes > 59 || (hours == 14 && minutes != 0) {
			return time.Time{}
		}
		value = fmt.Sprintf("%s%s%02d:%02d", match[1], match[2], hours, minutes)
	} else if strings.HasSuffix(strings.ToUpper(value), " UTC") {
		value = value[:len(value)-4] + "Z"
	}
	if parsed, err := time.Parse(time.RFC3339Nano, strings.Replace(value, " ", "T", 1)); err == nil {
		return parsed
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 && duration <= 366*24*time.Hour {
		return received.Add(duration)
	}
	if match := jevRelativeTimePattern.FindStringSubmatch(value); match != nil {
		number, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return time.Time{}
		}
		var unit time.Duration
		switch strings.ToLower(match[2]) {
		case "ms", "millisecond", "milliseconds":
			unit = time.Millisecond
		case "s", "second", "seconds", "秒":
			unit = time.Second
		case "m", "minute", "minutes", "分钟":
			unit = time.Minute
		case "h", "hour", "hours", "小时":
			unit = time.Hour
		case "d", "day", "days", "天":
			unit = 24 * time.Hour
		}
		if duration, ok := settingDurationFromFloat64(number, unit); ok && duration <= 366*24*time.Hour {
			return received.Add(duration)
		}
	}
	if len(value) == 10 || len(value) == 13 {
		if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
			if len(value) == 13 {
				return time.UnixMilli(unix)
			}
			return time.Unix(unix, 0)
		}
	}
	return time.Time{}
}
