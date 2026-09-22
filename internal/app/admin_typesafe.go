package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// AdminTestTypeSafe tests a supplied or persisted key without changing settings.
func (s *Server) AdminTestTypeSafe(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		APIKey *string `json:"api_key"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	if c.ShouldBindJSON(&input) != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request")
		return
	}
	key := ""
	if input.APIKey != nil {
		key = strings.TrimSpace(*input.APIKey)
	} else {
		setting, err := s.configService.GetSettingFresh(c.Request.Context(), config.TypeSafeAPIKeySettingKey)
		if err != nil {
			RespondErrorMsg(c, http.StatusInternalServerError, "cannot load TypeSafe API key")
			return
		}
		key = setting.Value
	}
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		RespondErrorMsg(c, http.StatusBadRequest, "TypeSafe API key is required and must be valid header text")
		return
	}
	if !acquireJev(time.Now()) {
		RespondErrorMsg(c, http.StatusTooManyRequests, "TypeSafe is busy; try again shortly")
		return
	}
	defer releaseJev()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "cannot prepare TypeSafe test")
		return
	}
	question := jevQuestion{Type: "choice", Instructions: "Select ok to confirm the API connection.", Criteria: map[string]string{"ok": "The connection test is successful.", "other": "Other."}}
	audit := jevAudit{Version: 1, CallID: hex.EncodeToString(id[:]), Purpose: "credential_test", Request: jevRequest{Model: jevModel, State: jevState{Message: "API connection test"}, Questions: map[string]jevQuestion{"connection": question}}, Adopted: map[string]string{}, Fallback: map[string]string{}}
	payload, _ := json.Marshal(audit.Request)
	ctx, cancel := context.WithTimeout(c.Request.Context(), jevWaitBudget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, jevEndpoint, bytes.NewReader(payload))
	if err != nil {
		RespondErrorMsg(c, http.StatusInternalServerError, "cannot prepare TypeSafe test")
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	client := s.jevClient
	if client == nil {
		client = jevHTTPClient
	}
	started := time.Now()
	status := http.StatusBadGateway
	response := jevResponse{}
	debugData := newJevDebugLog(payload, started)
	defer func() {
		data, _ := json.Marshal(audit)
		debugData.RespStatus = status
		debugData.RespBody = append([]byte(nil), data...)
		s.AddLogAsync(&model.LogEntry{Time: model.JSONTime{Time: started}, LogSource: model.LogSourceJev, Model: jevModel, ResponseModel: response.Model, StatusCode: status, Duration: time.Since(started).Seconds(), InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens, Cost: util.CalculateCostDetailed(jevModel, response.Usage.InputTokens, response.Usage.OutputTokens, 0, 0, 0), Message: string(data), DebugData: debugData})
	}()
	reply, err := client.Do(request)
	if err != nil {
		debugData.UpstreamError = sanitizeJevText(err.Error(), []string{key}, 4096)
		audit.Fallback["call"] = "transport_error"
		if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
			audit.Fallback["call"] = "timeout"
		}
		RespondErrorMsg(c, status, "TypeSafe connection failed or timed out; key validity could not be verified")
		return
	}
	defer func() { _ = reply.Body.Close() }()
	debugData.RespHeaders = encodeDebugHeaders(reply.Header)
	status = reply.StatusCode
	raw, readErr := io.ReadAll(io.LimitReader(reply.Body, jevMaxResponseBytes+1))
	if readErr != nil {
		debugData.UpstreamError = sanitizeJevText(readErr.Error(), []string{key}, 4096)
	}
	// Bound escaped JSON as well as raw bytes; never return upstream text to the browser.
	audit.Truncated = len(raw) > 4096
	audit.Response = sanitizeJevText(string(raw), []string{key}, 4096)
	if status != http.StatusOK {
		audit.Fallback["call"] = "http_error"
		message := "TypeSafe service unavailable; try again later"
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			message = "TypeSafe rejected this API key; check the key and its permissions"
		}
		if status == http.StatusTooManyRequests || status == http.StatusPaymentRequired {
			message = "TypeSafe quota or rate limit reached; check your account and try again later"
		}
		RespondJSON(c, http.StatusOK, gin.H{"valid": false, "message": message, "upstream_status": status})
		return
	}
	if readErr != nil || len(raw) > jevMaxResponseBytes || ctx.Err() != nil || json.Unmarshal(raw, &response) != nil || !strings.HasPrefix(response.Model, "jev-") || len(response.Model) > 80 || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || validJevAnswer(response.Answers["connection"], question.Criteria) != "" {
		audit.Fallback["call"] = "invalid_response"
		response = jevResponse{}
		RespondErrorMsg(c, http.StatusBadGateway, "TypeSafe returned an unexpected response; key validity could not be verified")
		return
	}
	response.Model = sanitizeJevText(response.Model, []string{key}, 80)
	audit.Adopted["connection"] = "verified"
	RespondJSON(c, http.StatusOK, gin.H{"valid": true})
}
