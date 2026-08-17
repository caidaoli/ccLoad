package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

var (
	errCodexQuotaResetInProgress  = errors.New("codex quota reset is already in progress")
	errCodexQuotaResetUnavailable = errors.New("no Codex quota reset credit is available")
)

type codexQuotaResetCredit struct {
	ExpiresAt string `json:"expires_at"`
}

type codexQuotaResetCredits struct {
	AvailableCount int                     `json:"available_count"`
	Credits        []codexQuotaResetCredit `json:"credits,omitempty"`
}

type codexQuotaResetResponse struct {
	Reset    bool               `json:"reset"`
	Usage    *oauthUsageSummary `json:"usage,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
}

func cloneCodexQuotaResetCredits(source *codexQuotaResetCredits) *codexQuotaResetCredits {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Credits = append([]codexQuotaResetCredit(nil), source.Credits...)
	return &clone
}

func requestCodexResetCredits(
	ctx context.Context,
	client *http.Client,
	credential *codexauth.Credential,
	now time.Time,
) (*codexQuotaResetCredits, error) {
	if client == nil {
		return nil, errors.New("usage: Codex reset credit request is unavailable")
	}
	req, err := newCodexQuotaRequest(ctx, http.MethodGet, codexResetCreditsURL, nil, credential)
	if err != nil {
		return nil, errors.New("usage: Codex reset credit request is unavailable")
	}
	body, err := executeOAuthUsageRequest(client, req, "Codex reset credit")
	if err != nil {
		return nil, err
	}
	return parseCodexResetCredits(body, now)
}

func parseCodexResetCredits(body []byte, now time.Time) (*codexQuotaResetCredits, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("usage: Codex reset credit response is invalid")
	}

	items, hasItems, availableCount, hasCount := codexResetCreditContainer(payload)
	if !hasItems && !hasCount {
		return nil, errors.New("usage: Codex reset credit response is invalid")
	}
	credits := make([]codexQuotaResetCredit, 0, len(items))
	for _, item := range items {
		credit, ok := normalizeCodexResetCredit(item, now)
		if ok {
			credits = append(credits, credit)
		}
	}

	if !hasCount {
		availableCount = len(credits)
	} else if hasItems && availableCount > len(credits) {
		availableCount = len(credits)
	}
	return &codexQuotaResetCredits{AvailableCount: availableCount, Credits: credits}, nil
}

func codexResetCreditContainer(payload any) ([]any, bool, int, bool) {
	switch value := payload.(type) {
	case []any:
		return value, true, 0, false
	case map[string]any:
		count, hasCount := codexResetCreditCount(value)
		for _, key := range []string{"credits", "rate_limit_reset_credits", "items", "data"} {
			nested, ok := value[key]
			if !ok {
				continue
			}
			switch nestedValue := nested.(type) {
			case []any:
				return nestedValue, true, count, hasCount
			case map[string]any:
				items, hasItems, nestedCount, nestedHasCount := codexResetCreditContainer(nestedValue)
				if !hasCount && nestedHasCount {
					count, hasCount = nestedCount, true
				}
				if hasItems {
					return items, true, count, hasCount
				}
			}
		}
		return nil, false, count, hasCount
	default:
		return nil, false, 0, false
	}
}

func codexResetCreditCount(payload map[string]any) (int, bool) {
	for _, key := range []string{"available_count", "availableCount"} {
		value, ok := payload[key]
		if !ok {
			continue
		}
		var parsed int64
		var err error
		switch typed := value.(type) {
		case json.Number:
			parsed, err = strconv.ParseInt(string(typed), 10, 64)
		case string:
			parsed, err = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		default:
			continue
		}
		if err == nil && parsed >= 0 && parsed <= int64(^uint(0)>>1) {
			return int(parsed), true
		}
	}
	return 0, false
}

func normalizeCodexResetCredit(payload any, now time.Time) (codexQuotaResetCredit, bool) {
	item, ok := payload.(map[string]any)
	if !ok {
		return codexQuotaResetCredit{}, false
	}
	resetType := strings.ToLower(strings.TrimSpace(firstStringValue(item, "reset_type", "resetType")))
	if resetType != "" && resetType != "codex_rate_limits" {
		return codexQuotaResetCredit{}, false
	}
	status := strings.ToLower(strings.TrimSpace(firstStringValue(item, "status")))
	if status != "" && status != "available" {
		return codexQuotaResetCredit{}, false
	}
	expiresAt := strings.TrimSpace(firstStringValue(item, "expires_at", "expiresAt"))
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !expires.After(now) {
		return codexQuotaResetCredit{}, false
	}
	return codexQuotaResetCredit{ExpiresAt: expires.UTC().Format(time.RFC3339Nano)}, true
}

func firstStringValue(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			return value
		}
	}
	return ""
}

func (s *Server) consumeCodexResetCredit(
	ctx context.Context,
	client *http.Client,
	credential *codexauth.Credential,
) error {
	payload, err := json.Marshal(map[string]string{"redeem_request_id": util.NewUUIDv4()})
	if err != nil {
		return errors.New("encode Codex quota reset request")
	}
	req, err := newCodexQuotaRequest(
		ctx, http.MethodPost, codexResetCreditConsumeURL, bytes.NewReader(payload), credential,
	)
	if err != nil {
		return errors.New("build Codex quota reset request")
	}
	body, err := executeOAuthUsageRequest(client, req, "Codex quota reset")
	if err != nil {
		return err
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil || response == nil {
		return errors.New("codex quota reset response is invalid")
	}
	return nil
}

// HandleResetCodexQuota consumes one upstream reset credit. Once consumption
// succeeds, all local cleanup is best-effort because the credit is not refundable.
func (s *Server) HandleResetCodexQuota(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusNotFound, errOAuthUsageChannelNotFound)
		return
	}
	if !cfg.UsesCodexOAuth() {
		RespondError(c, http.StatusConflict, errOAuthUsageUnsupported)
		return
	}
	if s.codexCredentials == nil {
		RespondError(c, http.StatusServiceUnavailable, errCodexUsageManagerUnavailable)
		return
	}
	if _, loaded := s.codexQuotaResetInFlight.LoadOrStore(id, struct{}{}); loaded {
		RespondError(c, http.StatusConflict, errCodexQuotaResetInProgress)
		return
	}
	defer s.codexQuotaResetInFlight.Delete(id)

	requestCtx, cancel := context.WithTimeout(c.Request.Context(), oauthUsageTimeout)
	defer cancel()
	credential, err := s.codexCredentials.credential(requestCtx, cfg, false)
	if err != nil {
		RespondError(c, http.StatusBadGateway, oauthUsageCredentialRefreshError(err, "Codex credential refresh failed"))
		return
	}
	client := s.getClientForChannel(cfg)
	credits, err := requestCodexResetCredits(requestCtx, client, credential, time.Now())
	if err != nil {
		RespondError(c, http.StatusBadGateway, errors.New("query Codex quota reset credits failed"))
		return
	}
	if credits.AvailableCount <= 0 {
		RespondError(c, http.StatusConflict, errCodexQuotaResetUnavailable)
		return
	}
	quotaResetAt := time.Now().UTC()
	if err := s.consumeCodexResetCredit(requestCtx, client, credential); err != nil {
		RespondError(c, http.StatusBadGateway, err)
		return
	}

	postCtx, postCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), oauthUsageTimeout)
	defer postCancel()
	warnings := make([]string, 0, 4)
	if err := s.resetOAuthQuotaCostUsage(postCtx, id, quotaResetAt); err != nil {
		warnings = append(warnings, "Codex quota was reset, but local cost usage reset failed")
	}
	if err := s.resetAllChannelCooldowns(postCtx, id); err != nil {
		warnings = append(warnings, "Codex quota was reset, but local cooldown cleanup failed")
	}
	if err := s.codexCredentials.clearQuotaOverdraftWindow(postCtx, id); err != nil {
		warnings = append(warnings, "Codex quota was reset, but quota overdraft state cleanup failed")
	}
	usage, err := s.refreshOAuthUsage(postCtx, id)
	if err != nil {
		warnings = append(warnings, "Codex quota was reset, but refreshed usage is unavailable")
		usage = nil
	}
	RespondJSON(c, http.StatusOK, codexQuotaResetResponse{Reset: true, Usage: usage, Warnings: warnings})
}
