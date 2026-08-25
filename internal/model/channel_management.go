package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// ChannelManagementKind identifies the private channel-management envelope
// schema; the remaining constants define its current version and profiles.
const (
	ChannelManagementKind    = "channel_management"
	ChannelManagementVersion = 1

	ChannelManagementProfileNewAPI     = "new_api"
	ChannelManagementProfileSub2API    = "sub2api"
	ChannelManagementProfileSub2APIPro = "sub2api_pro"
)

// ChannelManagementSettings contains the private upstream account settings.
type ChannelManagementSettings struct {
	BaseURL             string `json:"base_url"`
	AccessToken         string `json:"access_token"`
	UserID              *int64 `json:"user_id,omitempty"`
	DailyCheckinEnabled bool   `json:"daily_checkin_enabled,omitempty"`
	DailyCheckinTime    string `json:"daily_checkin_time,omitempty"`
}

// ChannelManagementSubscriptionSnapshot records one sampled quota window.
type ChannelManagementSubscriptionSnapshot struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name,omitempty"`
	Window           string   `json:"window"`
	UsedUSD          *float64 `json:"used_usd,omitempty"`
	LimitUSD         *float64 `json:"limit_usd,omitempty"`
	AvailablePercent *float64 `json:"available_percent,omitempty"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
}

// ChannelManagementBalanceSnapshot records the most recent upstream balance.
type ChannelManagementBalanceSnapshot struct {
	RemainingRaw  *int64                                  `json:"remaining_raw,omitempty"`
	UsedRaw       *int64                                  `json:"used_raw,omitempty"`
	TotalRaw      *int64                                  `json:"total_raw,omitempty"`
	Divisor       int64                                   `json:"divisor,omitempty"`
	BalanceUSD    *float64                                `json:"balance_usd,omitempty"`
	Subscriptions []ChannelManagementSubscriptionSnapshot `json:"subscriptions,omitempty"`
	SampledAt     time.Time                               `json:"sampled_at"`
}

// ChannelManagementState contains persisted check-in and balance state.
type ChannelManagementState struct {
	LastScheduledDay  string                            `json:"last_scheduled_day,omitempty"`
	LastCheckinAt     *time.Time                        `json:"last_checkin_at,omitempty"`
	LastCheckinStatus string                            `json:"last_checkin_status,omitempty"`
	LastBalance       *ChannelManagementBalanceSnapshot `json:"last_balance,omitempty"`
}

// ChannelManagementEnvelope is the versioned private payload stored for an API-key channel.
type ChannelManagementEnvelope struct {
	Kind     string                    `json:"kind"`
	Version  int                       `json:"version"`
	Profile  string                    `json:"profile"`
	Settings ChannelManagementSettings `json:"settings"`
	State    ChannelManagementState    `json:"state"`
}

// ParseChannelManagementEnvelope strictly decodes and validates a private envelope.
func ParseChannelManagementEnvelope(raw string) (*ChannelManagementEnvelope, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("channel management envelope cannot be empty")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope ChannelManagementEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode channel management envelope: %w", err)
	}
	if err := ensureChannelManagementJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := envelope.Validate(); err != nil {
		return nil, err
	}
	return &envelope, nil
}

func ensureChannelManagementJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing channel management data: %w", err)
	}
	return errors.New("channel management envelope contains trailing JSON data")
}

// Validate checks the envelope schema and normalizes its management base URL.
func (e *ChannelManagementEnvelope) Validate() error {
	if e == nil {
		return errors.New("channel management envelope cannot be nil")
	}
	if e.Kind != ChannelManagementKind {
		return fmt.Errorf("invalid channel management kind %q", e.Kind)
	}
	if e.Version != ChannelManagementVersion {
		return fmt.Errorf("unsupported channel management version %d", e.Version)
	}
	switch e.Profile {
	case ChannelManagementProfileNewAPI:
		if e.Settings.UserID != nil && *e.Settings.UserID <= 0 {
			return errors.New("new_api user_id must be positive")
		}
	case ChannelManagementProfileSub2API:
		if e.Settings.UserID != nil {
			return errors.New("sub2api does not accept user_id")
		}
		if e.Settings.DailyCheckinEnabled || e.Settings.DailyCheckinTime != "" {
			return errors.New("sub2api does not support daily checkin")
		}
	case ChannelManagementProfileSub2APIPro:
		if e.Settings.UserID != nil {
			return errors.New("sub2api_pro does not accept user_id")
		}
	default:
		return fmt.Errorf("unsupported channel management profile %q", e.Profile)
	}

	normalizedBaseURL, err := normalizeChannelManagementBaseURL(e.Settings.BaseURL)
	if err != nil {
		return err
	}
	e.Settings.BaseURL = normalizedBaseURL
	if strings.TrimSpace(e.Settings.AccessToken) == "" {
		return errors.New("channel management access_token cannot be empty")
	}
	if e.Settings.DailyCheckinEnabled && e.Settings.DailyCheckinTime == "" {
		return errors.New("daily_checkin_time is required when daily checkin is enabled")
	}
	if e.Settings.DailyCheckinTime != "" {
		parsedTime, err := time.Parse("15:04", e.Settings.DailyCheckinTime)
		if err != nil || parsedTime.Format("15:04") != e.Settings.DailyCheckinTime {
			return fmt.Errorf("invalid daily_checkin_time %q", e.Settings.DailyCheckinTime)
		}
	}
	return nil
}

func normalizeChannelManagementBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("channel management base_url cannot be empty")
	}
	if strings.Contains(raw, "#") {
		return "", errors.New("channel management base_url must not include a fragment")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid channel management base_url: %w", err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("channel management base_url must use http or https")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("channel management base_url must include a host")
	}
	if parsed.User != nil {
		return "", errors.New("channel management base_url must not include userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("channel management base_url must not include a query")
	}
	if parsed.Fragment != "" {
		return "", errors.New("channel management base_url must not include a fragment")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", errors.New("channel management base_url must be a root address")
	}
	parsed.Path = ""
	return parsed.String(), nil
}

// Marshal validates and encodes the complete private envelope.
func (e *ChannelManagementEnvelope) Marshal() (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("marshal channel management envelope: %w", err)
	}
	return string(raw), nil
}
