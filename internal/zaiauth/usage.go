package zaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Coding Plan quota.
//
// Z.ai's subscription panel reads its quota from the monitor API. The endpoint
// is undocumented but accepts the same Coding Plan key used for inference, so
// ccLoad can report the plan's own windows instead of guessing from traffic.

// QuotaLimitKind classifies one quota window.
type QuotaLimitKind string

const (
	// QuotaLimitTokens is the token allowance window (5 hour and weekly).
	QuotaLimitTokens QuotaLimitKind = "tokens"
	// QuotaLimitOther is any non-token window, such as monthly MCP tool usage.
	QuotaLimitOther QuotaLimitKind = "other"
)

// tokensLimitType is the upstream discriminator for token allowance windows.
const tokensLimitType = "TOKENS_LIMIT"

// QuotaLimit is one window of the Coding Plan allowance.
type QuotaLimit struct {
	// Type is the upstream discriminator, e.g. TOKENS_LIMIT.
	Type string
	// Unit and Number describe the window length; (3,5) is 5 hours and (6,1)
	// is one week. Upstream sends no duration in seconds.
	Unit   int
	Number int
	// UsedPercent is the consumed share of the window, 0..100.
	UsedPercent float64
	// ResetAtMillis is when the window rolls over, in Unix milliseconds.
	ResetAtMillis int64
}

// Kind reports whether this window meters tokens.
func (q QuotaLimit) Kind() QuotaLimitKind {
	if strings.EqualFold(strings.TrimSpace(q.Type), tokensLimitType) {
		return QuotaLimitTokens
	}
	return QuotaLimitOther
}

// WindowSeconds returns the window length, or 0 when upstream describes one
// ccLoad cannot express.
func (q QuotaLimit) WindowSeconds() int64 {
	switch {
	case q.Unit == 3 && q.Number > 0: // hours
		return int64(q.Number) * 3600
	case q.Unit == 6 && q.Number > 0: // weeks
		return int64(q.Number) * 7 * 86400
	default:
		return 0
	}
}

// Name returns a stable window identifier for ccLoad's usage summary.
func (q QuotaLimit) Name() string {
	if q.Kind() == QuotaLimitTokens {
		switch {
		case q.Unit == 3 && q.Number == 5:
			return "five_hour"
		case q.Unit == 6 && q.Number == 1:
			return "weekly"
		}
	}
	if name := strings.ToLower(strings.TrimSpace(q.Type)); name != "" {
		return name
	}
	return "unknown"
}

// FetchQuotaLimits reads the Coding Plan allowance windows for one key.
func (s *Service) FetchQuotaLimits(ctx context.Context, apiKey string) ([]QuotaLimit, error) {
	if s == nil || s.Client == nil || strings.TrimSpace(s.QuotaLimitURL) == "" {
		return nil, errors.New("z.ai quota endpoint is unavailable")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("z.ai API key is required")
	}
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.QuotaLimitURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build z.ai quota request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	ApplySourceHeaders(request.Header)
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("z.ai quota request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read z.ai quota response: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, errors.New("z.ai rejected the Coding Plan API key")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("z.ai quota endpoint returned HTTP %d", response.StatusCode)
	}
	return parseQuotaLimits(body)
}

// parseQuotaLimits reads the monitor envelope. The endpoint answers HTTP 200
// even for a rejected key, so the envelope decides success.
func parseQuotaLimits(body []byte) ([]QuotaLimit, error) {
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Msg     string          `json:"msg"`
		Success *bool           `json:"success"`
		Data    struct {
			Limits []struct {
				Type          string   `json:"type"`
				Unit          *float64 `json:"unit"`
				Number        *float64 `json:"number"`
				Percentage    *float64 `json:"percentage"`
				NextResetTime *int64   `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode z.ai quota response: %w", err)
	}
	succeeded := envelopeCodeSucceeded(envelope.Code)
	if envelope.Success != nil {
		succeeded = *envelope.Success
	}
	if !succeeded {
		message := strings.TrimSpace(envelope.Msg)
		if message == "" {
			message = "unknown error"
		}
		return nil, fmt.Errorf("z.ai quota endpoint failed: %s", message)
	}
	limits := make([]QuotaLimit, 0, len(envelope.Data.Limits))
	for _, raw := range envelope.Data.Limits {
		limit := QuotaLimit{Type: strings.TrimSpace(raw.Type)}
		if raw.Unit != nil {
			limit.Unit = int(*raw.Unit)
		}
		if raw.Number != nil {
			limit.Number = int(*raw.Number)
		}
		if raw.Percentage != nil {
			limit.UsedPercent = *raw.Percentage
		}
		if raw.NextResetTime != nil {
			limit.ResetAtMillis = *raw.NextResetTime
		}
		limits = append(limits, limit)
	}
	if len(limits) == 0 {
		return nil, errors.New("z.ai quota response has no limits")
	}
	return limits, nil
}
