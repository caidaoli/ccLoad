package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
)

// Usage contains account credits, including usable add-on and organization funds.
type Usage struct {
	Plan      string        `json:"plan,omitempty"`
	Unit      string        `json:"unit,omitempty"`
	Total     float64       `json:"total"`
	Remaining float64       `json:"remaining"`
	Used      float64       `json:"used"`
	Buckets   []UsageBucket `json:"buckets,omitempty"`
}

// UsageBucket describes one independently spendable source of quota.
type UsageBucket struct {
	Kind      string  `json:"kind"`
	Unit      string  `json:"unit,omitempty"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
}

func authNumber(raw json.RawMessage) (float64, bool) {
	var value float64
	if err := json.Unmarshal(raw, &value); err == nil && len(raw) > 0 && string(raw) != "null" {
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(text, 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// FetchUsage queries quota and, when available, the account's plan label.
func (s *Service) FetchUsage(ctx context.Context, credential *Credential) (*Usage, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.AccessToken == "" {
		return nil, errors.New("qoder access_token is required")
	}
	data, _, err := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, "/api/v2/quota/usage"), nil, authHeaders(c))
	if err != nil {
		return nil, err
	}
	usage := &Usage{}
	for _, kind := range []string{"userQuota", "addOnQuota", "orgResourcePackage"} {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data[kind], &fields) != nil || fields == nil {
			continue
		}
		if kind == "orgResourcePackage" {
			var available bool
			if raw, exists := fields["available"]; exists {
				if json.Unmarshal(raw, &available) != nil {
					return nil, errors.New("invalid Qoder organization quota availability")
				}
				if !available {
					continue
				}
			}
		}
		total, hasTotal := authNumber(fields["total"])
		if kind == "orgResourcePackage" {
			if capValue, ok := authNumber(fields["cap"]); ok {
				total, hasTotal = capValue, true
			}
		}
		used, hasUsed := authNumber(fields["used"])
		remaining, hasRemaining := authNumber(fields["remaining"])
		if !hasRemaining && hasTotal && hasUsed {
			remaining, hasRemaining = math.Max(0, total-used), true
		}
		if !hasRemaining {
			continue
		}
		if !hasTotal && hasUsed {
			total = remaining + used
		}
		if !hasUsed && hasTotal {
			used = math.Max(0, total-remaining)
		}
		if total < 0 || used < 0 || remaining < 0 {
			return nil, errors.New("invalid negative Qoder quota")
		}
		unit := authText(fields, "unit")
		if unit != "" {
			if usage.Unit != "" && usage.Unit != unit {
				return nil, errors.New("qoder quota buckets use different units")
			}
			usage.Unit = unit
		}
		usage.Buckets = append(usage.Buckets, UsageBucket{Kind: kind, Unit: unit, Total: total, Remaining: remaining, Used: used})
		usage.Total += total
		usage.Remaining += remaining
		usage.Used += used
	}
	if len(usage.Buckets) == 0 {
		return nil, errors.New("qoder usage response has no usable quota buckets")
	}
	plan, _, planErr := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, "/api/v2/user/plan"), nil, authHeaders(c))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if planErr == nil {
		usage.Plan = authText(plan, "plan_tier_name")
	}
	return usage, nil
}
