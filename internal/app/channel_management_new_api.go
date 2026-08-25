package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
)

const newAPIQuotaDivisor int64 = 500000
const (
	newAPICheckinSkippedDisabled = "skipped_disabled"
	newAPICheckinAlreadyChecked  = "already_checked"
	newAPICheckinSuccess         = "success"
	newAPICheckinManualRequired  = "manual_required"
	newAPICheckinUnsupported     = "unsupported"
	newAPICheckinCredentialError = "credential_invalid"
	newAPICheckinUncertain       = "uncertain"
)

type newAPIResponse[T any] struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type newAPIUserSelf struct {
	ID        int64  `json:"id"`
	Quota     int64  `json:"quota"`
	UsedQuota *int64 `json:"used_quota"`
}
type newAPIStatus struct {
	CheckinEnabled bool `json:"checkin_enabled"`
}

type newAPICheckinStatus struct {
	Stats struct {
		CheckedInToday bool `json:"checked_in_today"`
	} `json:"stats"`
}

type newAPICheckinSuccessData struct {
	QuotaAwarded *int64 `json:"quota_awarded"`
	CheckinDate  string `json:"checkin_date"`
}

type newAPIRawResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (s *channelManagementService) refreshNewAPIBalance(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) (*model.ChannelManagementBalanceSnapshot, int, error) {
	if s == nil || cfg == nil || envelope == nil || envelope.Profile != model.ChannelManagementProfileNewAPI {
		return nil, 0, errInvalidManagementRequest
	}
	headers := newAPIHeaders(envelope.Settings.UserID, false)
	result, err := s.doManagementRequest(
		ctx,
		cfg,
		http.MethodGet,
		strings.TrimRight(envelope.Settings.BaseURL, "/")+"/api/user/self",
		envelope.Settings.AccessToken,
		nil,
		headers,
	)
	if err != nil {
		return nil, managementStatusCode(result), err
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return nil, result.StatusCode, errManagementRequestFailed
	}
	response, err := decodeNewAPIResponse[newAPIUserSelf](result.Body)
	if err != nil || !response.Success {
		return nil, result.StatusCode, errInvalidManagementResponse
	}
	if response.Data.Quota < 0 || response.Data.UsedQuota != nil && *response.Data.UsedQuota < 0 {
		return nil, result.StatusCode, errInvalidManagementResponse
	}

	remaining := response.Data.Quota
	snapshot := &model.ChannelManagementBalanceSnapshot{
		RemainingRaw: &remaining,
		Divisor:      newAPIQuotaDivisor,
		SampledAt:    s.now(),
	}
	if response.Data.UsedQuota == nil {
		return snapshot, result.StatusCode, nil
	}
	used := *response.Data.UsedQuota
	if remaining > math.MaxInt64-used {
		return nil, result.StatusCode, errInvalidManagementResponse
	}
	total := remaining + used
	snapshot.UsedRaw = &used
	snapshot.TotalRaw = &total
	return snapshot, result.StatusCode, nil
}
func (s *channelManagementService) checkInNewAPI(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) (*channelCheckinResult, *model.ChannelManagementBalanceSnapshot, error) {
	if s == nil || cfg == nil || envelope == nil || envelope.Profile != model.ChannelManagementProfileNewAPI {
		return nil, nil, errInvalidManagementRequest
	}
	checkedAt := s.now()
	baseURL := strings.TrimRight(envelope.Settings.BaseURL, "/")
	statusResponse, statusResult, err := getNewAPI[newAPIStatus](
		s, ctx, cfg, envelope, baseURL+"/api/status",
	)
	if err != nil {
		if status := classifyNewAPICheckinHTTP(managementStatusCode(statusResult)); status != "" {
			return newAPICheckinResult(status, managementStatusCode(statusResult), checkedAt), nil, nil
		}
		return nil, nil, err
	}
	if !statusResponse.Data.CheckinEnabled {
		return newAPICheckinResult(newAPICheckinSkippedDisabled, statusResult.StatusCode, checkedAt), nil, nil
	}

	monthTarget := baseURL + "/api/user/checkin?month=" + checkedAt.Format("2006-01")
	monthlyResponse, monthlyResult, err := getNewAPI[newAPICheckinStatus](
		s, ctx, cfg, envelope, monthTarget,
	)
	if err != nil {
		if status := classifyNewAPICheckinHTTP(managementStatusCode(monthlyResult)); status != "" {
			return newAPICheckinResult(status, managementStatusCode(monthlyResult), checkedAt), nil, nil
		}
		return nil, nil, err
	}
	if monthlyResponse.Data.Stats.CheckedInToday {
		result := newAPICheckinResult(newAPICheckinAlreadyChecked, monthlyResult.StatusCode, checkedAt)
		return s.finishNewAPICheckin(ctx, cfg, envelope, result)
	}

	postResult, postErr := s.doManagementRequest(
		ctx,
		cfg,
		http.MethodPost,
		baseURL+"/api/user/checkin",
		envelope.Settings.AccessToken,
		[]byte(`{}`),
		newAPIHeaders(envelope.Settings.UserID, true),
	)
	var postMessage string
	if postErr == nil && postResult.StatusCode >= http.StatusOK && postResult.StatusCode < http.StatusMultipleChoices {
		postResponse, decodeErr := decodeNewAPIResponse[newAPICheckinSuccessData](postResult.Body)
		if decodeErr == nil {
			postMessage = postResponse.Message
			if postResponse.Success && validNewAPICheckinSuccess(postResponse.Data) {
				reward := float64(*postResponse.Data.QuotaAwarded) / float64(newAPIQuotaDivisor)
				result := newAPICheckinResult(newAPICheckinSuccess, postResult.StatusCode, checkedAt)
				result.Reward = &reward
				return s.finishNewAPICheckin(ctx, cfg, envelope, result)
			}
		}
	}
	if postResult == nil || !postResult.WroteRequest {
		if postErr != nil {
			return nil, nil, postErr
		}
		return nil, nil, errManagementRequestFailed
	}

	readback, _, readbackErr := getNewAPI[newAPICheckinStatus](
		s, ctx, cfg, envelope, monthTarget,
	)
	if readbackErr != nil {
		return newAPICheckinResult(newAPICheckinUncertain, managementStatusCode(postResult), checkedAt), nil, nil
	}
	if readback.Data.Stats.CheckedInToday {
		result := newAPICheckinResult(newAPICheckinAlreadyChecked, managementStatusCode(postResult), checkedAt)
		return s.finishNewAPICheckin(ctx, cfg, envelope, result)
	}
	if isNewAPITurnstileMessage(postMessage) {
		return newAPICheckinResult(newAPICheckinManualRequired, managementStatusCode(postResult), checkedAt), nil, nil
	}
	if status := classifyNewAPICheckinHTTP(managementStatusCode(postResult)); status != "" {
		return newAPICheckinResult(status, managementStatusCode(postResult), checkedAt), nil, nil
	}
	return nil, nil, errManagementRequestFailed
}

func getNewAPI[T any](
	s *channelManagementService,
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	target string,
) (*newAPIResponse[T], *managementHTTPResult, error) {
	result, err := s.doManagementRequest(
		ctx,
		cfg,
		http.MethodGet,
		target,
		envelope.Settings.AccessToken,
		nil,
		newAPIHeaders(envelope.Settings.UserID, false),
	)
	if err != nil {
		return nil, result, err
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return nil, result, errManagementRequestFailed
	}
	response, err := decodeNewAPIResponse[T](result.Body)
	if err != nil || !response.Success {
		return nil, result, errInvalidManagementResponse
	}
	return response, result, nil
}

func (s *channelManagementService) finishNewAPICheckin(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	result *channelCheckinResult,
) (*channelCheckinResult, *model.ChannelManagementBalanceSnapshot, error) {
	snapshot, _, err := s.refreshNewAPIBalance(ctx, cfg, envelope)
	if err != nil {
		return result, nil, nil
	}
	result.Balance = channelManagementBalanceViewFromSnapshot(snapshot)
	return result, snapshot, nil
}

func newAPICheckinResult(status string, statusCode int, checkedAt time.Time) *channelCheckinResult {
	return &channelCheckinResult{
		Status:      status,
		StatusCode:  statusCode,
		CheckedInAt: &checkedAt,
	}
}

func validNewAPICheckinSuccess(data newAPICheckinSuccessData) bool {
	if data.QuotaAwarded == nil || *data.QuotaAwarded < 0 || data.CheckinDate == "" {
		return false
	}
	parsed, err := time.Parse("2006-01-02", data.CheckinDate)
	return err == nil && parsed.Format("2006-01-02") == data.CheckinDate
}

func classifyNewAPICheckinHTTP(statusCode int) string {
	switch statusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return newAPICheckinUnsupported
	case http.StatusUnauthorized, http.StatusForbidden:
		return newAPICheckinCredentialError
	default:
		return ""
	}
}

func isNewAPITurnstileMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "turnstile") || strings.Contains(message, "captcha")
}

func decodeNewAPIResponse[T any](body []byte) (*newAPIResponse[T], error) {
	var raw newAPIRawResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errInvalidManagementResponse
	}
	response := &newAPIResponse[T]{Success: raw.Success, Message: raw.Message}
	if !raw.Success {
		return response, nil
	}
	if len(raw.Data) == 0 || string(raw.Data) == "null" {
		return nil, errInvalidManagementResponse
	}
	if err := json.Unmarshal(raw.Data, &response.Data); err != nil {
		return nil, errInvalidManagementResponse
	}
	return response, nil
}

func newAPIHeaders(userID *int64, jsonBody bool) http.Header {
	headers := make(http.Header)
	if userID != nil {
		headers.Set("New-API-User", strconv.FormatInt(*userID, 10))
	}
	if jsonBody {
		headers.Set("Content-Type", "application/json")
	}
	return headers
}

func managementStatusCode(result *managementHTTPResult) int {
	if result == nil {
		return 0
	}
	return result.StatusCode
}
