package app

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

const maxMergedDebugResponseBodyBytes = 16 * 1024 * 1024

// maskSensitiveHeaderJSON 对 JSON string 格式的 headers 做脱敏
func maskSensitiveHeaderJSON(jsonStr string) string {
	if jsonStr == "" {
		return jsonStr
	}
	var headers map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &headers); err != nil {
		return jsonStr
	}
	for k, v := range headers {
		if !isSensitiveHeader(k) {
			continue
		}
		switch val := v.(type) {
		case string:
			headers[k] = maskHeaderValue(val)
		case []any:
			for i, item := range val {
				if s, ok := item.(string); ok {
					val[i] = maskHeaderValue(s)
				}
			}
		}
	}
	out, err := json.Marshal(headers)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

type debugLogUnavailableInfo struct {
	Reason                   string               `json:"reason"`
	DebugLogEnabled          *model.SystemSetting `json:"debug_log_enabled,omitempty"`
	DebugLogRetentionMinutes *model.SystemSetting `json:"debug_log_retention_minutes,omitempty"`
}

func (s *Server) buildDebugLogUnavailableInfo(ctx context.Context) debugLogUnavailableInfo {
	info := debugLogUnavailableInfo{
		Reason: "debug_log_not_found",
	}

	if setting, err := s.configService.GetSettingFresh(ctx, "debug_log_enabled"); err == nil {
		info.DebugLogEnabled = setting
	}
	if setting, err := s.configService.GetSettingFresh(ctx, "debug_log_retention_minutes"); err == nil {
		info.DebugLogRetentionMinutes = setting
	}

	return info
}

func debugLogResponse(entry *model.DebugLogEntry) gin.H {
	resp := gin.H{
		"log_id":       entry.LogID,
		"created_at":   entry.CreatedAt,
		"req_method":   entry.ReqMethod,
		"req_url":      entry.ReqURL,
		"req_headers":  maskSensitiveHeaderJSON(entry.ReqHeaders),
		"resp_status":  entry.RespStatus,
		"resp_headers": maskSensitiveHeaderJSON(entry.RespHeaders),
	}

	if utf8.Valid(entry.ReqBody) {
		resp["req_body"] = string(entry.ReqBody)
	} else {
		resp["req_body"] = base64.StdEncoding.EncodeToString(entry.ReqBody)
		resp["req_body_encoding"] = "base64"
	}

	if utf8.Valid(entry.RespBody) {
		resp["resp_body"] = string(entry.RespBody)
	} else {
		resp["resp_body"] = base64.StdEncoding.EncodeToString(entry.RespBody)
		resp["resp_body_encoding"] = "base64"
	}

	return resp
}

// HandleGetDebugLog 获取指定 log_id 对应的调试日志
// GET /admin/debug-logs/:log_id
func (s *Server) HandleGetDebugLog(c *gin.Context) {
	logIDStr := c.Param("log_id")
	logID, err := strconv.ParseInt(logIDStr, 10, 64)
	if err != nil || logID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid log_id")
		return
	}

	entry, err := s.store.GetDebugLogByLogID(c.Request.Context(), logID)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if entry == nil {
		RespondErrorWithData(c, http.StatusNotFound, "debug log unavailable", s.buildDebugLogUnavailableInfo(c.Request.Context()))
		return
	}

	RespondJSON(c, http.StatusOK, debugLogResponse(entry))
}

type mergeDebugResponseRequest struct {
	RespBody string `json:"resp_body"`
}

// HandleMergeDebugResponse merges an already-loaded upstream response body.
// The caller sends the current modal content, so this endpoint does not depend on
// debug log retention or on a second database lookup.
func (s *Server) HandleMergeDebugResponse(c *gin.Context) {
	body, err := readMaybeCompressedJSONBody(c.Request, maxMergedDebugResponseBodyBytes)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	var req mergeDebugResponseRequest
	if err := json.Unmarshal(body, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request")
		return
	}
	RespondJSON(c, http.StatusOK, mergeResponseBody(req.RespBody))
}

func readMaybeCompressedJSONBody(req *http.Request, limit int64) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, errors.New("empty request body")
	}
	defer func() { _ = req.Body.Close() }()

	var reader io.Reader = req.Body
	switch strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, errors.New("invalid gzip request body")
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	default:
		return nil, errors.New("unsupported content encoding")
	}

	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read request body failed")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("request body too large")
	}
	if len(body) == 0 {
		return nil, errors.New("empty request body")
	}
	return body, nil
}
