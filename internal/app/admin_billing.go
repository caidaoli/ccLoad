package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

const (
	sub2APIBillingTimeout      = 10 * time.Second
	maxSub2APIBillingBodyBytes = 64 * 1024
)

const (
	sub2APIBillingErrorAuthentication = "authentication_error"
	sub2APIBillingErrorPermission     = "permission_error"
	sub2APIBillingErrorUnsupported    = "not_supported"
	sub2APIBillingErrorTimeout        = "timeout"
	sub2APIBillingErrorInvalid        = "invalid_response"
	sub2APIBillingErrorAPI            = "api_error"
)

type fetchKeyRateRequest struct {
	Profile     string `json:"profile" binding:"required"`
	BaseURL     string `json:"base_url" binding:"required"`
	APIKey      string `json:"api_key" binding:"required"`
	AccessToken string `json:"access_token,omitempty"`
	UserID      *int64 `json:"user_id,omitempty"`
}

type fetchKeyRateResponse struct {
	EffectiveRateMultiplier float64 `json:"effective_rate_multiplier"`
}

type newAPIKeySearchPage struct {
	Items []struct {
		Group string `json:"group"`
	} `json:"items"`
	Total int `json:"total"`
}

type newAPIGroupRate struct {
	Ratio json.RawMessage `json:"ratio"`
}

type newAPIKeyOwner struct {
	Group string `json:"group"`
}

type sub2APIBillingResponse struct {
	Object                  string   `json:"object"`
	SchemaVersion           int      `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64 `json:"resolved_rate_multiplier"`
	EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	ObservedAt              string   `json:"observed_at"`
}

type sub2APIBillingProbeError struct {
	code string
}

func (e *sub2APIBillingProbeError) Error() string {
	return e.code
}

// HandleFetchKeyRate probes an unsaved API Key channel draft without persisting it.
// The management profile is authoritative: New API and Sub2API have different
// upstream contracts and must never be guessed from URL shape.
func (s *Server) HandleFetchKeyRate(c *gin.Context) {
	var input fetchKeyRateRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "profile、base_url、api_key为必填字段")
		return
	}

	input.Profile = strings.ToLower(strings.TrimSpace(input.Profile))
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	if input.Profile == "" || input.BaseURL == "" || input.APIKey == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "profile、base_url、api_key为必填字段")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), sub2APIBillingTimeout)
	defer cancel()

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}

	var rate float64
	var err error
	switch input.Profile {
	case model.ChannelManagementProfileNewAPI:
		if input.AccessToken == "" {
			RespondErrorWithData(c, http.StatusOK, keyRateErrorMessage(sub2APIBillingErrorAuthentication), gin.H{"code": sub2APIBillingErrorAuthentication})
			return
		}
		if input.UserID != nil && *input.UserID <= 0 {
			RespondErrorMsg(c, http.StatusBadRequest, "user_id必须为正整数")
			return
		}
		rate, err = requestNewAPIKeyRate(
			ctx, client, input.BaseURL, input.APIKey, input.AccessToken, input.UserID,
		)
	case model.ChannelManagementProfileSub2API, model.ChannelManagementProfileSub2APIPro:
		var endpoint string
		endpoint, err = buildSub2APIBillingURL(input.BaseURL)
		if err == nil {
			var result *sub2APIBillingResponse
			result, err = requestSub2APIBilling(ctx, client, endpoint, input.APIKey)
			if err == nil {
				rate = *result.EffectiveRateMultiplier
			}
		}
	default:
		RespondErrorMsg(c, http.StatusBadRequest, "不支持的管理账户类型")
		return
	}
	if err != nil && !isKeyRateProbeError(err) {
		RespondErrorMsg(c, http.StatusBadRequest, "倍率查询参数无效: "+err.Error())
		return
	}
	if err != nil {
		var probeErr *sub2APIBillingProbeError
		if !errors.As(err, &probeErr) {
			probeErr = &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
		}
		RespondErrorWithData(c, http.StatusOK, keyRateErrorMessage(probeErr.code), gin.H{"code": probeErr.code})
		return
	}

	RespondJSON(c, http.StatusOK, fetchKeyRateResponse{
		EffectiveRateMultiplier: rate,
	})
}

func isKeyRateProbeError(err error) bool {
	var probeErr *sub2APIBillingProbeError
	return errors.As(err, &probeErr)
}

func requestNewAPIKeyRate(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, accessToken string,
	userID *int64,
) (float64, error) {
	searchURL, err := buildNewAPIKeySearchURL(baseURL, apiKey)
	if err != nil {
		return 0, err
	}
	searchPage, err := requestNewAPIData[newAPIKeySearchPage](ctx, client, searchURL, accessToken, userID)
	if err != nil {
		return 0, err
	}
	if searchPage.Total != 1 || len(searchPage.Items) != 1 {
		return 0, &sub2APIBillingProbeError{code: sub2APIBillingErrorPermission}
	}

	group := strings.TrimSpace(searchPage.Items[0].Group)
	if strings.EqualFold(group, "auto") {
		return 0, &sub2APIBillingProbeError{code: sub2APIBillingErrorUnsupported}
	}
	if group == "" {
		ownerURL, buildErr := buildNewAPIURL(baseURL, "/api/user/self", nil)
		if buildErr != nil {
			return 0, buildErr
		}
		owner, requestErr := requestNewAPIData[newAPIKeyOwner](ctx, client, ownerURL, accessToken, userID)
		if requestErr != nil {
			return 0, requestErr
		}
		group = strings.TrimSpace(owner.Group)
		if group == "" || strings.EqualFold(group, "auto") {
			return 0, &sub2APIBillingProbeError{code: sub2APIBillingErrorUnsupported}
		}
	}

	groupsURL, err := buildNewAPIURL(baseURL, "/api/user/self/groups", nil)
	if err != nil {
		return 0, err
	}
	groups, err := requestNewAPIData[map[string]newAPIGroupRate](ctx, client, groupsURL, accessToken, userID)
	if err != nil {
		return 0, err
	}
	groupRate, ok := groups[group]
	if !ok {
		return 0, &sub2APIBillingProbeError{code: sub2APIBillingErrorPermission}
	}

	var rate *float64
	if err := json.Unmarshal(groupRate.Ratio, &rate); err != nil ||
		rate == nil || *rate < 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0) {
		return 0, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}
	return *rate, nil
}

func buildNewAPIKeySearchURL(baseURL, apiKey string) (string, error) {
	// New API relay auth treats everything after the first post-prefix dash as
	// a channel selector. Token search does not, so normalize with relay auth's
	// exact rule or valid sk-<token>-<channel_id> keys will never be found.
	lookupToken := strings.TrimPrefix(strings.TrimSpace(apiKey), "sk-")
	if separator := strings.IndexByte(lookupToken, '-'); separator >= 0 {
		lookupToken = lookupToken[:separator]
	}
	if lookupToken == "" {
		return "", fmt.Errorf("invalid New API key")
	}
	query := make(neturl.Values)
	query.Set("token", lookupToken)
	query.Set("p", "1")
	query.Set("page_size", "1")
	return buildNewAPIURL(baseURL, "/api/token/search", query)
}

func buildNewAPIURL(baseURL, path string, query neturl.Values) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	u, err := neturl.Parse(baseURL)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url scheme: %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("url must not contain query or fragment")
	}
	if (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", fmt.Errorf("url must be a root address")
	}

	u.Path = path
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func requestNewAPIData[T any](
	ctx context.Context,
	client *http.Client,
	endpoint, accessToken string,
	userID *int64,
) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return zero, &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if userID != nil {
		if *userID <= 0 {
			return zero, fmt.Errorf("user_id must be positive")
		}
		req.Header.Set("New-API-User", strconv.FormatInt(*userID, 10))
	}

	resp, err := keyRateProbeClient(client).Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return zero, &sub2APIBillingProbeError{code: sub2APIBillingErrorTimeout}
		}
		return zero, &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return zero, &sub2APIBillingProbeError{code: classifySub2APIBillingStatus(resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSub2APIBillingBodyBytes+1))
	if err != nil || len(body) > maxSub2APIBillingBodyBytes {
		return zero, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}
	result, err := decodeNewAPIResponse[T](body)
	if err != nil || !result.Success {
		return zero, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}
	return result.Data, nil
}

func buildSub2APIBillingURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	u, err := neturl.Parse(baseURL)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url scheme: %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("url must not contain query or fragment")
	}

	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/v1") {
		u.Path = path + "/sub2api/billing"
	} else {
		u.Path = path + "/v1/sub2api/billing"
	}
	u.RawPath = ""
	return u.String(), nil
}

func requestSub2APIBilling(
	ctx context.Context,
	client *http.Client,
	endpoint, apiKey string,
) (*sub2APIBillingResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := keyRateProbeClient(client).Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorTimeout}
		}
		return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &sub2APIBillingProbeError{code: classifySub2APIBillingStatus(resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSub2APIBillingBodyBytes+1))
	if err != nil || len(body) > maxSub2APIBillingBodyBytes {
		return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}

	var result sub2APIBillingResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}
	if err := validateSub2APIBillingResponse(&result); err != nil {
		return nil, &sub2APIBillingProbeError{code: sub2APIBillingErrorInvalid}
	}
	return &result, nil
}

func keyRateProbeClient(client *http.Client) *http.Client {
	return &http.Client{
		Transport: client.Transport,
		Timeout:   client.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func classifySub2APIBillingStatus(statusCode int) string {
	switch statusCode {
	case http.StatusUnauthorized:
		return sub2APIBillingErrorAuthentication
	case http.StatusForbidden:
		return sub2APIBillingErrorPermission
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return sub2APIBillingErrorUnsupported
	default:
		return sub2APIBillingErrorAPI
	}
}

func validateSub2APIBillingResponse(result *sub2APIBillingResponse) error {
	if result.Object != "sub2api.key_billing" || result.SchemaVersion != 1 || result.BillingScope != "token" {
		return fmt.Errorf("unsupported billing response")
	}
	if !validSub2APIMultiplier(result.GroupRateMultiplier) ||
		!validSub2APIMultiplier(result.ResolvedRateMultiplier) ||
		!validSub2APIMultiplier(result.EffectiveRateMultiplier) {
		return fmt.Errorf("invalid required multiplier")
	}
	if result.UserRateMultiplier != nil && !validSub2APIMultiplier(result.UserRateMultiplier) {
		return fmt.Errorf("invalid user multiplier")
	}

	expectedResolved := *result.GroupRateMultiplier
	if result.UserRateMultiplier != nil {
		expectedResolved = *result.UserRateMultiplier
	}
	if *result.ResolvedRateMultiplier != expectedResolved {
		return fmt.Errorf("inconsistent resolved multiplier")
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ObservedAt); err != nil {
		return fmt.Errorf("invalid observed_at")
	}
	return nil
}

func validSub2APIMultiplier(value *float64) bool {
	return value != nil && *value >= 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func keyRateErrorMessage(code string) string {
	switch code {
	case sub2APIBillingErrorAuthentication:
		return "倍率查询凭据无效"
	case sub2APIBillingErrorPermission:
		return "无法确定API Key所属倍率分组"
	case sub2APIBillingErrorUnsupported:
		return "上游不支持固定倍率查询"
	case sub2APIBillingErrorTimeout:
		return "倍率查询超时"
	case sub2APIBillingErrorInvalid:
		return "上游返回了无效的倍率响应"
	default:
		return "倍率查询失败"
	}
}
