package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ccLoad/internal/model"
)

// Only Codex and Anthropic return account quota and expose the switch; other
// channels keep their original passthrough output.
func hideOAuthQuota(cfg *model.Config) bool {
	return (cfg.UsesCodexOAuth() || cfg.UsesAnthropicOAuth()) && !cfg.OAuthQuotaPassthrough
}

// Quota headers are account metadata. Keep request IDs and Codex turn-state,
// which are needed for tracing and routing, rather than dropping all x-codex-*.
func isOAuthQuotaHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(name, "anthropic-ratelimit-") || strings.HasPrefix(name, "x-ratelimit-") || strings.HasPrefix(name, "ratelimit-") {
		return true
	}
	if name == "x-codex-plan-type" {
		return true
	}
	if !strings.HasPrefix(name, "x-codex-") {
		return false
	}
	for _, part := range []string{"primary-", "secondary-", "credits-", "-limit", "-quota", "-usage", "-used-percent", "-reset-", "-window-"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

func stripOAuthQuotaHeaders(headers http.Header) {
	for name := range headers {
		if isOAuthQuotaHeader(name) {
			delete(headers, name)
		}
	}
}

func isOAuthQuotaEvent(name string) bool {
	switch strings.TrimSpace(name) {
	case "codex.rate_limits", "rate_limits.updated", "rate_limits":
		return true
	default:
		return false
	}
}

// Only traverse protocol envelopes. Never inspect model text, tool arguments,
// output items or token usage, even if user data contains quota-like keys.
func stripOAuthQuotaObject(obj map[string]json.RawMessage, errorDetail bool) bool {
	changed := false
	for _, key := range []string{"rate_limits", "rate_limit", "code_review_rate_limits", "additional_rate_limits", "plan_type", "credits", "metered_limit_name", "resets_at", "resets_in_seconds", "quotaResetDelay", "quotaResetTime", "quota_limit", "quota_remaining"} {
		if _, ok := obj[key]; ok {
			delete(obj, key)
			changed = true
		}
	}
	keys := []string{"response", "error"}
	var eventType string
	_ = json.Unmarshal(obj["type"], &eventType)
	if errorDetail || eventType == "codex.response.metadata" || eventType == "response.metadata" {
		keys = append(keys, "metadata")
	}
	for _, key := range keys {
		var nested map[string]json.RawMessage
		if json.Unmarshal(obj[key], &nested) == nil && nested != nil && stripOAuthQuotaObject(nested, errorDetail || key == "error") {
			obj[key], _ = json.Marshal(nested)
			changed = true
		}
	}
	if errorDetail {
		var details []map[string]json.RawMessage
		if json.Unmarshal(obj["details"], &details) == nil {
			modified := false
			for _, detail := range details {
				modified = stripOAuthQuotaObject(detail, true) || modified
			}
			if modified {
				obj["details"], _ = json.Marshal(details)
				changed = true
			}
		}
	}
	var headers map[string]json.RawMessage
	if json.Unmarshal(obj["headers"], &headers) == nil && headers != nil {
		removed := false
		for name := range headers {
			if isOAuthQuotaHeader(name) {
				delete(headers, name)
				removed = true
			}
		}
		if removed {
			obj["headers"], _ = json.Marshal(headers)
			changed = true
		}
	}
	return changed
}

// Every key or event stripped below contains one of these markers; header
// names are only inspected under a "headers" object. Checking them first keeps
// ordinary deltas off the JSON decoder.
var oauthQuotaMarkers = [][]byte{
	[]byte("rate_limit"), []byte("credits"), []byte("quota"), []byte("resets_"),
	[]byte("metered_limit_name"), []byte("plan_type"), []byte(`"headers"`),
}

func mayContainOAuthQuota(data []byte) bool {
	for _, marker := range oauthQuotaMarkers {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

func stripOAuthQuotaJSON(data []byte) []byte {
	if !mayContainOAuthQuota(data) {
		return data
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return data
	}
	var eventType string
	_ = json.Unmarshal(obj["type"], &eventType)
	if isOAuthQuotaEvent(eventType) {
		return nil
	}
	if !stripOAuthQuotaObject(obj, false) {
		return data
	}
	result, _ := json.Marshal(obj)
	return result
}

func stripOAuthQuotaFrame(frame []byte) []byte {
	event, data := parseSSEEventChunk(frame)
	if isOAuthQuotaEvent(event) {
		return nil
	}
	updated := stripOAuthQuotaJSON(data)
	if bytes.Equal(updated, data) {
		return frame
	}
	if len(updated) == 0 {
		return nil
	}
	var result []byte
	wrote := false
	for _, line := range bytes.SplitAfter(frame, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			result = append(result, line...)
			continue
		}
		if !wrote {
			result = append(result, "data: "...)
			result = append(result, updated...)
			result = append(result, '\n')
			wrote = true
		}
	}
	return result
}

// This writer sits outside parsing and deferred commit: accounting sees the
// original bytes, and each forwarding attempt owns its own pending frames.
type oauthQuotaResponseWriter struct {
	http.ResponseWriter
	pending   []byte
	streaming bool
	started   bool
	wrote     bool
}

func (w *oauthQuotaResponseWriter) isSSE() bool {
	if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		return true
	}
	data := bytes.TrimSpace(w.pending)
	return w.streaming && (bytes.HasPrefix(data, []byte("event:")) || bytes.HasPrefix(data, []byte("data:")) || bytes.HasPrefix(data, []byte(":")))
}

func (w *oauthQuotaResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *oauthQuotaResponseWriter) WriteHeader(status int) {
	stripOAuthQuotaHeaders(w.Header())
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}
func (w *oauthQuotaResponseWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.started = true
		stripOAuthQuotaHeaders(w.Header())
		w.Header().Del("Content-Length")
	}
	w.pending = append(w.pending, data...)
	if w.isSSE() {
		for {
			end := firstSSEEventEnd(w.pending)
			if end < 0 {
				break
			}
			frame := stripOAuthQuotaFrame(w.pending[:end])
			if len(frame) > 0 {
				w.wrote = true
				if _, err := w.ResponseWriter.Write(frame); err != nil {
					return 0, err
				}
			}
			w.pending = w.pending[end:]
		}
		if len(w.pending) > maxSSEEventBytes {
			return 0, fmt.Errorf("OAuth response event exceeds size limit")
		}
	}
	return len(data), nil
}
func (w *oauthQuotaResponseWriter) Flush() {
	// Flush complete frames even when the next frame is still pending.
	if f, ok := w.ResponseWriter.(http.Flusher); ok && w.wrote {
		f.Flush()
	}
}
func (w *oauthQuotaResponseWriter) finish() error {
	if len(w.pending) == 0 {
		return nil
	}
	data := w.pending
	if w.isSSE() {
		data = stripOAuthQuotaFrame(data)
	} else {
		data = stripOAuthQuotaJSON(data)
	}
	w.pending = nil
	if len(data) == 0 {
		return nil
	}
	_, err := w.ResponseWriter.Write(data)
	return err
}

func filterOAuthQuotaResult(cfg *model.Config, result *proxyResult) {
	if !hideOAuthQuota(cfg) || result == nil {
		return
	}
	result.header = result.header.Clone()
	stripOAuthQuotaHeaders(result.header)
	result.body = stripOAuthQuotaJSON(result.body)
}
