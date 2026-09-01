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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxMergedDebugResponseBodyBytes = 16 * 1024 * 1024

// maskedHeaderUnavailable 标记「脱敏失败，原始头未展示」，与「上游没发请求头」区分开。
const maskedHeaderUnavailable = `{"_masked":"unavailable"}`

// maskSensitiveHeaderJSON 对 JSON string 格式的 headers 做脱敏。
// 脱敏失败时返回可辨识的占位而不是空对象：调试日志的价值就是排查故障时的证据，
// 静默换成 "{}" 会让人以为上游真的没发请求头。
func maskSensitiveHeaderJSON(jsonStr string) string {
	if jsonStr == "" {
		return jsonStr
	}
	raw := []byte(jsonStr)
	if !isMutableJSONObject(raw) {
		return maskedHeaderUnavailable
	}
	// 先按原始快照收集待脱敏的键，再统一写入：ForEach 的迭代对象必须与
	// 被反复重写的 updated 解耦，否则一旦将来改动 key 集合就会读到半成品。
	type maskTarget struct {
		path  string
		value string
	}
	targets := make([]maskTarget, 0, 4)
	gjson.ParseBytes(raw).ForEach(func(key, value gjson.Result) bool {
		keyName := key.String()
		if !isSensitiveHeader(keyName) {
			return true
		}
		path := sjsonObjectPathJoin("", keyName)
		switch value.Type {
		case gjson.String:
			targets = append(targets, maskTarget{path: path, value: value.String()})
		case gjson.JSON:
			if !value.IsArray() {
				return true
			}
			for index, item := range value.Array() {
				if item.Type != gjson.String {
					continue
				}
				targets = append(targets, maskTarget{path: sjsonPathJoin(path, strconv.Itoa(index)), value: item.String()})
			}
		}
		return true
	})
	updated := raw
	for _, target := range targets {
		var err error
		updated, err = sjson.SetBytes(updated, target.path, maskHeaderValue(target.value))
		if err != nil {
			return maskedHeaderUnavailable
		}
	}
	return string(updated)
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
	if entry.UpstreamError != "" {
		resp["upstream_error"] = entry.UpstreamError
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

	if entry.ProtocolTransformed {
		resp["protocol_transformed"] = true
		resp["original_req_url"] = entry.OriginalReqURL
		resp["original_req_headers"] = maskSensitiveHeaderJSON(entry.OriginalReqHeaders)
		addDebugResponseBody(resp, "original_req_body", entry.OriginalReqBody)
		resp["translated_resp_status"] = entry.TranslatedRespStatus
		resp["translated_resp_headers"] = maskSensitiveHeaderJSON(entry.TranslatedRespHeaders)
		addDebugResponseBody(resp, "translated_resp_body", entry.TranslatedRespBody)
	}

	return resp
}

func addDebugResponseBody(resp gin.H, key string, body []byte) {
	if utf8.Valid(body) {
		resp[key] = string(body)
		return
	}
	resp[key] = base64.StdEncoding.EncodeToString(body)
	resp[key+"_encoding"] = "base64"
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
