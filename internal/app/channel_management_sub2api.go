package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
)

const (
	sub2APIAuthMePath                 = "/api/v1/auth/me"
	sub2APISubscriptionSummaryPath    = "/api/v1/subscriptions/summary"
	sub2APISubscriptionActivePath     = "/api/v1/subscriptions/active"
	sub2APIProCheckinStatusPath       = "/api/v1/redeem/checkin/status"
	sub2APIProCheckinPath             = "/api/v1/redeem/checkin"
	sub2APIReasonCheckinDisabled      = "DAILY_CHECKIN_DISABLED"
	sub2APIReasonCheckinRoleForbidden = "DAILY_CHECKIN_ROLE_FORBIDDEN"
	sub2APIReasonCheckinAlreadyDone   = "DAILY_CHECKIN_ALREADY_DONE"
	sub2APICheckinCredentialForbidden = "credential_forbidden"
)

type sub2APIResponse[T any] struct {
	Code     int               `json:"code"`
	Message  string            `json:"message"`
	Reason   string            `json:"reason"`
	Metadata map[string]string `json:"metadata"`
	Data     T                 `json:"data"`
}

type sub2APIAuthMe struct {
	Balance *float64 `json:"balance"`
}

type sub2APISummary struct {
	ActiveCount   *int                  `json:"active_count"`
	TotalUsedUSD  *float64              `json:"total_used_usd"`
	Subscriptions *[]sub2APISummaryItem `json:"subscriptions"`
}

type sub2APISummaryItem struct {
	ID              int64    `json:"id"`
	GroupName       string   `json:"group_name"`
	DailyUsedUSD    *float64 `json:"daily_used_usd"`
	DailyLimitUSD   *float64 `json:"daily_limit_usd"`
	WeeklyUsedUSD   *float64 `json:"weekly_used_usd"`
	WeeklyLimitUSD  *float64 `json:"weekly_limit_usd"`
	MonthlyUsedUSD  *float64 `json:"monthly_used_usd"`
	MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	ExpiresAt       string   `json:"expires_at"`
}

type sub2APIActiveSubscription struct {
	ID              int64    `json:"id"`
	DailyUsageUSD   *float64 `json:"daily_usage_usd"`
	WeeklyUsageUSD  *float64 `json:"weekly_usage_usd"`
	MonthlyUsageUSD *float64 `json:"monthly_usage_usd"`
	ExpiresAt       string   `json:"expires_at"`
	Group           *struct {
		Name            string   `json:"name"`
		DailyLimitUSD   *float64 `json:"daily_limit_usd"`
		WeeklyLimitUSD  *float64 `json:"weekly_limit_usd"`
		MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	} `json:"group"`
}

type sub2APIRawResponse struct {
	Code     *int              `json:"code"`
	Message  string            `json:"message"`
	Reason   string            `json:"reason"`
	Metadata map[string]string `json:"metadata"`
	Data     json.RawMessage   `json:"data"`
}

type sub2APIProCheckinStatus struct {
	Enabled        *bool `json:"enabled"`
	CheckedInToday *bool `json:"checked_in_today"`
}

type sub2APIProCheckinSuccess struct {
	RewardAmount *float64 `json:"reward_amount"`
	NewBalance   *float64 `json:"new_balance"`
	CheckedInAt  *string  `json:"checked_in_at"`
}

func (s *channelManagementService) checkInSub2APIPro(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) (*channelCheckinResult, *model.ChannelManagementBalanceSnapshot, error) {
	if s == nil || cfg == nil || envelope == nil || envelope.Profile != model.ChannelManagementProfileSub2APIPro {
		return nil, nil, errInvalidManagementRequest
	}
	checkedAt := s.now()
	baseURL := strings.TrimRight(envelope.Settings.BaseURL, "/")
	statusResponse, statusResult, err := s.getSub2APIProCheckinStatus(ctx, cfg, envelope, baseURL)
	if err != nil {
		if status := classifySub2APIProCheckinFailure(managementStatusCode(statusResult), sub2APIReason(statusResponse)); status != "" {
			return newAPICheckinResult(status, managementStatusCode(statusResult), checkedAt), nil, nil
		}
		return nil, nil, err
	}
	if !*statusResponse.Data.Enabled {
		return newAPICheckinResult(newAPICheckinSkippedDisabled, statusResult.StatusCode, checkedAt), nil, nil
	}
	if *statusResponse.Data.CheckedInToday {
		result := newAPICheckinResult(newAPICheckinAlreadyChecked, statusResult.StatusCode, checkedAt)
		return s.finishSub2APIProCheckin(ctx, cfg, envelope, result)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	postResult, postErr := s.doManagementRequest(
		ctx,
		cfg,
		http.MethodPost,
		baseURL+sub2APIProCheckinPath,
		envelope.Settings.AccessToken,
		[]byte(`{}`),
		headers,
	)
	if postErr == nil {
		postResponse, decodeErr := decodeSub2APIResponse[sub2APIProCheckinSuccess](postResult.Body)
		if postResult.StatusCode >= http.StatusOK && postResult.StatusCode < http.StatusMultipleChoices {
			if decodeErr != nil || postResponse.Code != 0 {
				return nil, nil, errInvalidManagementResponse
			}
			reward, responseCheckedAt, valid := validSub2APIProCheckinSuccess(postResponse.Data)
			if !valid {
				return nil, nil, errInvalidManagementResponse
			}
			result := newAPICheckinResult(newAPICheckinSuccess, postResult.StatusCode, responseCheckedAt)
			result.Reward = &reward
			return s.finishSub2APIProCheckin(ctx, cfg, envelope, result)
		}
		if status := classifySub2APIProCheckinFailure(postResult.StatusCode, sub2APIReason(postResponse)); status != "" {
			result := newAPICheckinResult(status, postResult.StatusCode, checkedAt)
			if status == newAPICheckinAlreadyChecked {
				return s.finishSub2APIProCheckin(ctx, cfg, envelope, result)
			}
			return result, nil, nil
		}
		if !postResult.WroteRequest {
			return nil, nil, errManagementRequestFailed
		}
		return s.resolveAmbiguousSub2APIProCheckin(ctx, cfg, envelope, baseURL, postResult, checkedAt)
	}
	if postResult == nil || !postResult.WroteRequest {
		return nil, nil, postErr
	}
	return s.resolveAmbiguousSub2APIProCheckin(ctx, cfg, envelope, baseURL, postResult, checkedAt)
}

func (s *channelManagementService) resolveAmbiguousSub2APIProCheckin(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	baseURL string,
	postResult *managementHTTPResult,
	checkedAt time.Time,
) (*channelCheckinResult, *model.ChannelManagementBalanceSnapshot, error) {
	readback, _, readbackErr := s.getSub2APIProCheckinStatus(ctx, cfg, envelope, baseURL)
	if readbackErr == nil && *readback.Data.CheckedInToday {
		result := newAPICheckinResult(newAPICheckinAlreadyChecked, managementStatusCode(postResult), checkedAt)
		return s.finishSub2APIProCheckin(ctx, cfg, envelope, result)
	}
	return newAPICheckinResult(newAPICheckinUncertain, managementStatusCode(postResult), checkedAt), nil, nil
}

func (s *channelManagementService) getSub2APIProCheckinStatus(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	baseURL string,
) (*sub2APIResponse[sub2APIProCheckinStatus], *managementHTTPResult, error) {
	result, err := s.doManagementRequest(
		ctx,
		cfg,
		http.MethodGet,
		baseURL+sub2APIProCheckinStatusPath,
		envelope.Settings.AccessToken,
		nil,
	)
	if err != nil {
		return nil, result, err
	}
	response, decodeErr := decodeSub2APIResponse[sub2APIProCheckinStatus](result.Body)
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		if decodeErr != nil {
			return nil, result, errManagementRequestFailed
		}
		return response, result, errManagementRequestFailed
	}
	if decodeErr != nil || response.Code != 0 || response.Data.Enabled == nil || response.Data.CheckedInToday == nil {
		return nil, result, errInvalidManagementResponse
	}
	return response, result, nil
}

func (s *channelManagementService) finishSub2APIProCheckin(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	result *channelCheckinResult,
) (*channelCheckinResult, *model.ChannelManagementBalanceSnapshot, error) {
	snapshot, _, err := s.refreshSub2APIBalance(ctx, cfg, envelope)
	if err != nil {
		return result, nil, nil
	}
	result.Balance = channelManagementBalanceViewFromSnapshot(snapshot)
	return result, snapshot, nil
}

func validSub2APIProCheckinSuccess(data sub2APIProCheckinSuccess) (float64, time.Time, bool) {
	if data.RewardAmount == nil || data.NewBalance == nil || data.CheckedInAt == nil ||
		!finiteNonNegative(*data.RewardAmount) || !finiteNonNegative(*data.NewBalance) {
		return 0, time.Time{}, false
	}
	checkedAt, err := time.Parse(time.RFC3339, *data.CheckedInAt)
	if err != nil {
		return 0, time.Time{}, false
	}
	return *data.RewardAmount, checkedAt, true
}

func classifySub2APIProCheckinFailure(statusCode int, reason string) string {
	if statusCode == http.StatusUnauthorized {
		return newAPICheckinCredentialError
	}
	switch reason {
	case sub2APIReasonCheckinDisabled:
		return newAPICheckinSkippedDisabled
	case sub2APIReasonCheckinRoleForbidden:
		return sub2APICheckinCredentialForbidden
	case sub2APIReasonCheckinAlreadyDone:
		return newAPICheckinAlreadyChecked
	}
	switch statusCode {
	case http.StatusForbidden:
		return newAPICheckinCredentialError
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return newAPICheckinUnsupported
	default:
		return ""
	}
}

func sub2APIReason[T any](response *sub2APIResponse[T]) string {
	if response == nil {
		return ""
	}
	return response.Reason
}

func (s *channelManagementService) refreshSub2APIBalance(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) (*model.ChannelManagementBalanceSnapshot, int, error) {
	if s == nil || cfg == nil || envelope == nil ||
		(envelope.Profile != model.ChannelManagementProfileSub2API && envelope.Profile != model.ChannelManagementProfileSub2APIPro) {
		return nil, 0, errInvalidManagementRequest
	}
	baseURL := strings.TrimRight(envelope.Settings.BaseURL, "/")
	authResult, err := s.doManagementRequest(
		ctx, cfg, http.MethodGet, baseURL+sub2APIAuthMePath, envelope.Settings.AccessToken, nil,
	)
	if err != nil {
		return nil, managementStatusCode(authResult), err
	}
	if authResult.StatusCode < http.StatusOK || authResult.StatusCode >= http.StatusMultipleChoices {
		return nil, authResult.StatusCode, errManagementRequestFailed
	}
	authResponse, err := decodeSub2APIResponse[sub2APIAuthMe](authResult.Body)
	if err != nil || authResponse.Code != 0 || authResponse.Data.Balance == nil ||
		!finiteNonNegative(*authResponse.Data.Balance) {
		return nil, authResult.StatusCode, errInvalidManagementResponse
	}

	subscriptions, _, _ := s.fetchSub2APISubscriptions(ctx, cfg, envelope, baseURL)
	balance := *authResponse.Data.Balance
	return &model.ChannelManagementBalanceSnapshot{
		BalanceUSD:    &balance,
		Subscriptions: subscriptions,
		SampledAt:     s.now(),
	}, authResult.StatusCode, nil
}

func (s *channelManagementService) fetchSub2APISubscriptions(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	baseURL string,
) ([]model.ChannelManagementSubscriptionSnapshot, int, error) {
	summaryResult, err := s.doManagementRequest(
		ctx, cfg, http.MethodGet, baseURL+sub2APISubscriptionSummaryPath, envelope.Settings.AccessToken, nil,
	)
	if err != nil {
		return nil, managementStatusCode(summaryResult), err
	}
	summaryResponse, decodeErr := decodeSub2APIResponse[sub2APISummary](summaryResult.Body)
	fallback := summaryResult.StatusCode == http.StatusNotFound || summaryResult.StatusCode == http.StatusMethodNotAllowed ||
		decodeErr == nil && summaryResponse.Code != 0
	if fallback {
		return s.fetchSub2APIActiveSubscriptions(ctx, cfg, envelope, baseURL)
	}
	if summaryResult.StatusCode < http.StatusOK || summaryResult.StatusCode >= http.StatusMultipleChoices {
		return nil, summaryResult.StatusCode, errManagementRequestFailed
	}
	if decodeErr != nil || summaryResponse.Code != 0 || summaryResponse.Data.Subscriptions == nil ||
		summaryResponse.Data.ActiveCount != nil && *summaryResponse.Data.ActiveCount < 0 ||
		summaryResponse.Data.TotalUsedUSD != nil && !finiteNonNegative(*summaryResponse.Data.TotalUsedUSD) {
		return nil, summaryResult.StatusCode, errInvalidManagementResponse
	}
	return sub2APISummarySubscriptions(*summaryResponse.Data.Subscriptions), summaryResult.StatusCode, nil
}

func (s *channelManagementService) fetchSub2APIActiveSubscriptions(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
	baseURL string,
) ([]model.ChannelManagementSubscriptionSnapshot, int, error) {
	activeResult, err := s.doManagementRequest(
		ctx, cfg, http.MethodGet, baseURL+sub2APISubscriptionActivePath, envelope.Settings.AccessToken, nil,
	)
	if err != nil {
		return nil, managementStatusCode(activeResult), err
	}
	if activeResult.StatusCode < http.StatusOK || activeResult.StatusCode >= http.StatusMultipleChoices {
		return nil, activeResult.StatusCode, errManagementRequestFailed
	}
	activeResponse, err := decodeSub2APIResponse[[]sub2APIActiveSubscription](activeResult.Body)
	if err != nil || activeResponse.Code != 0 {
		return nil, activeResult.StatusCode, errInvalidManagementResponse
	}
	return sub2APIActiveSubscriptions(activeResponse.Data), activeResult.StatusCode, nil
}

func decodeSub2APIResponse[T any](body []byte) (*sub2APIResponse[T], error) {
	var raw sub2APIRawResponse
	if err := json.Unmarshal(body, &raw); err != nil || raw.Code == nil {
		return nil, errInvalidManagementResponse
	}
	response := &sub2APIResponse[T]{
		Code: *raw.Code, Message: raw.Message, Reason: raw.Reason, Metadata: raw.Metadata,
	}
	if response.Code != 0 {
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

func sub2APISummarySubscriptions(items []sub2APISummaryItem) []model.ChannelManagementSubscriptionSnapshot {
	subscriptions := make([]model.ChannelManagementSubscriptionSnapshot, 0, len(items)*3)
	for _, item := range items {
		subscriptions = append(subscriptions,
			newSub2APISubscriptionWindow(item.ID, item.GroupName, "daily", item.DailyUsedUSD, item.DailyLimitUSD, item.ExpiresAt),
			newSub2APISubscriptionWindow(item.ID, item.GroupName, "weekly", item.WeeklyUsedUSD, item.WeeklyLimitUSD, item.ExpiresAt),
			newSub2APISubscriptionWindow(item.ID, item.GroupName, "monthly", item.MonthlyUsedUSD, item.MonthlyLimitUSD, item.ExpiresAt),
		)
	}
	sortSub2APISubscriptions(subscriptions)
	return subscriptions
}

func sub2APIActiveSubscriptions(items []sub2APIActiveSubscription) []model.ChannelManagementSubscriptionSnapshot {
	subscriptions := make([]model.ChannelManagementSubscriptionSnapshot, 0, len(items)*3)
	for _, item := range items {
		var name string
		var dailyLimit, weeklyLimit, monthlyLimit *float64
		if item.Group != nil {
			name = item.Group.Name
			dailyLimit = item.Group.DailyLimitUSD
			weeklyLimit = item.Group.WeeklyLimitUSD
			monthlyLimit = item.Group.MonthlyLimitUSD
		}
		subscriptions = append(subscriptions,
			newSub2APISubscriptionWindow(item.ID, name, "daily", item.DailyUsageUSD, dailyLimit, item.ExpiresAt),
			newSub2APISubscriptionWindow(item.ID, name, "weekly", item.WeeklyUsageUSD, weeklyLimit, item.ExpiresAt),
			newSub2APISubscriptionWindow(item.ID, name, "monthly", item.MonthlyUsageUSD, monthlyLimit, item.ExpiresAt),
		)
	}
	sortSub2APISubscriptions(subscriptions)
	return subscriptions
}

func newSub2APISubscriptionWindow(
	id int64,
	name string,
	window string,
	used *float64,
	limit *float64,
	expiresAt string,
) model.ChannelManagementSubscriptionSnapshot {
	snapshot := model.ChannelManagementSubscriptionSnapshot{
		ID: id, Name: name, Window: window, UsedUSD: finiteFloat64Pointer(used), LimitUSD: finiteFloat64Pointer(limit), ExpiresAt: expiresAt,
	}
	if snapshot.UsedUSD != nil && snapshot.LimitUSD != nil && *snapshot.LimitUSD > 0 {
		available := (*snapshot.LimitUSD - *snapshot.UsedUSD) / *snapshot.LimitUSD * 100
		if math.IsNaN(available) || math.IsInf(available, 0) {
			return snapshot
		}
		snapshot.AvailablePercent = &available
	}
	return snapshot
}

func finiteFloat64Pointer(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	copy := *value
	return &copy
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func sortSub2APISubscriptions(subscriptions []model.ChannelManagementSubscriptionSnapshot) {
	// Windows are created from this fixed set; use an ordinal instead of allocating a lookup map.
	sort.SliceStable(subscriptions, func(i, j int) bool {
		if subscriptions[i].ID != subscriptions[j].ID {
			return subscriptions[i].ID < subscriptions[j].ID
		}
		return sub2APIWindowOrder(subscriptions[i].Window) < sub2APIWindowOrder(subscriptions[j].Window)
	})
}

func sub2APIWindowOrder(window string) int {
	switch window {
	case "daily":
		return 0
	case "weekly":
		return 1
	default:
		return 2
	}
}
