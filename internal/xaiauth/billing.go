package xaiauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// BillingClassification identifies the actionable result of an xAI billing response.
type BillingClassification string

const (
	// BillingOK and the related values classify successful, credential, entitlement, quota, or unknown outcomes.
	BillingOK BillingClassification = "ok"
	// BillingBadCredential identifies an invalid or rejected access token.
	BillingBadCredential BillingClassification = "bad_credential"
	// BillingEntitlement identifies a missing or inactive entitlement.
	BillingEntitlement BillingClassification = "entitlement"
	// BillingQuota identifies an exhausted known xAI quota.
	BillingQuota BillingClassification = "quota"
	// BillingIndeterminate identifies a response that does not match a known schema.
	BillingIndeterminate BillingClassification = "indeterminate"
)

// BillingURL resolves the billing path against a validated HTTPS base URL.
func BillingURL(baseURL string, credits bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid xAI billing base URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/billing"
	if credits {
		parsed.RawQuery = "format=credits"
	}
	return parsed.String(), nil
}

// ApplyBillingHeaders adds the fixed xAI CLI billing request headers.
func ApplyBillingHeaders(req *http.Request, accessToken string) {
	if req == nil {
		return
	}
	if token := strings.TrimSpace(accessToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CLITokenAuthHeader, CLITokenAuthValue)
	req.Header.Set(CLIClientVersionHeader, CLIClientVersion)
	req.Header.Set("User-Agent", CLIBillingUserAgent)
}

// ClassifyBillingResponse maps a known xAI billing response schema to an actionable class.
func ClassifyBillingResponse(status int, headers http.Header, body []byte) BillingClassification {
	if status >= 200 && status < 300 {
		return BillingOK
	}
	if status == http.StatusUnauthorized {
		return BillingBadCredential
	}
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return BillingIndeterminate
	}
	var envelope struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	var nested struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	_ = json.Unmarshal(envelope.Error, &nested)
	errorType := strings.ToLower(strings.TrimSpace(nested.Type))
	errorCode := strings.ToLower(strings.TrimSpace(nested.Code))
	topLevelCode := strings.ToLower(strings.TrimSpace(envelope.Code))
	contains := func(actual string, allowed ...string) bool {
		for _, candidate := range allowed {
			if strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(candidate)) {
				return true
			}
		}
		return false
	}
	badCredentialCode := func(code string) bool {
		return contains(code, "invalid_token", "invalid_access_token", "bad_credentials", "unauthorized")
	}
	entitlementCode := func(code string) bool {
		return contains(code, "subscription_required", "entitlement_required", "not_entitled", "entitlement_error")
	}
	quotaCode := func(code string) bool {
		return contains(code, "quota_exceeded", "insufficient_quota", "usage_limit_reached")
	}
	if status == http.StatusForbidden && (badCredentialCode(topLevelCode) || errorType == "authentication_error" && badCredentialCode(errorCode)) {
		return BillingBadCredential
	}
	if status == http.StatusForbidden && (entitlementCode(topLevelCode) || contains(errorType, "permission_error", "entitlement_error") && entitlementCode(errorCode)) {
		return BillingEntitlement
	}
	if status == http.StatusForbidden && (contains(headers.Get("Xai-Entitlement-Status"), "subscription_required", "entitlement_required", "not_entitled", "inactive", "denied") ||
		contains(headers.Get("X-Entitlement-Status"), "subscription_required", "entitlement_required", "not_entitled", "inactive", "denied")) {
		return BillingEntitlement
	}
	if status == http.StatusTooManyRequests && (quotaCode(topLevelCode) || errorType == "rate_limit_error" && quotaCode(errorCode)) {
		return BillingQuota
	}
	if status == http.StatusTooManyRequests && exhaustedRateLimitHeader(headers) {
		return BillingQuota
	}
	return BillingIndeterminate
}

func exhaustedRateLimitHeader(headers http.Header) bool {
	for name, values := range headers {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if normalized != "x-ratelimit-remaining-requests" && normalized != "x-ratelimit-remaining-tokens" {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "0" {
				return true
			}
		}
	}
	return false
}
