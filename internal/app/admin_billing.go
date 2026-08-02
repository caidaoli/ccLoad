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
	"strings"
	"time"

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

type fetchSub2APIBillingRequest struct {
	BaseURL string `json:"base_url" binding:"required"`
	APIKey  string `json:"api_key" binding:"required"`
}

type fetchSub2APIBillingResponse struct {
	EffectiveRateMultiplier float64 `json:"effective_rate_multiplier"`
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

// HandleFetchSub2APIBilling probes an unsaved channel draft without persisting it.
func (s *Server) HandleFetchSub2APIBilling(c *gin.Context) {
	var input fetchSub2APIBillingRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "base_url、api_key为必填字段")
		return
	}

	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.APIKey = strings.TrimSpace(input.APIKey)
	if input.BaseURL == "" || input.APIKey == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "base_url、api_key为必填字段")
		return
	}

	endpoint, err := buildSub2APIBillingURL(input.BaseURL)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "base_url无效: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), sub2APIBillingTimeout)
	defer cancel()

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	result, err := requestSub2APIBilling(ctx, client, endpoint, input.APIKey)
	if err != nil {
		var probeErr *sub2APIBillingProbeError
		if !errors.As(err, &probeErr) {
			probeErr = &sub2APIBillingProbeError{code: sub2APIBillingErrorAPI}
		}
		RespondErrorWithData(c, http.StatusOK, sub2APIBillingErrorMessage(probeErr.code), gin.H{"code": probeErr.code})
		return
	}

	RespondJSON(c, http.StatusOK, fetchSub2APIBillingResponse{
		EffectiveRateMultiplier: *result.EffectiveRateMultiplier,
	})
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

	probeClient := &http.Client{
		Transport: client.Transport,
		Timeout:   client.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := probeClient.Do(req)
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

func sub2APIBillingErrorMessage(code string) string {
	switch code {
	case sub2APIBillingErrorAuthentication:
		return "Sub2API API Key无效"
	case sub2APIBillingErrorPermission:
		return "Sub2API API Key未绑定分组"
	case sub2APIBillingErrorUnsupported:
		return "上游不支持Sub2API倍率查询"
	case sub2APIBillingErrorTimeout:
		return "Sub2API倍率查询超时"
	case sub2APIBillingErrorInvalid:
		return "Sub2API返回了无效的倍率响应"
	default:
		return "Sub2API倍率查询失败"
	}
}
