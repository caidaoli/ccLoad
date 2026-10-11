package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const reasoningAutoSettingKey = "reasoning_auto"

// ParseReasoningAutoConfig validates the persisted automatic effort policy.
func ParseReasoningAutoConfig(raw string) (model.ReasoningAutoConfig, error) {
	cfg := model.ReasoningAutoConfig{BusinessModels: []string{}, TimeoutMS: 2000}
	var fields struct {
		Enabled           json.RawMessage `json:"enabled"`
		DecisionChannelID json.RawMessage `json:"decision_channel_id"`
		DecisionModel     json.RawMessage `json:"decision_model"`
		BusinessModels    json.RawMessage `json:"business_models"`
		TimeoutMS         json.RawMessage `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object: %w", err)
	}
	if strings.TrimSpace(raw) == "null" {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object")
	}
	for field, value := range map[string]json.RawMessage{
		"enabled": fields.Enabled, "decision_channel_id": fields.DecisionChannelID,
		"decision_model": fields.DecisionModel, "business_models": fields.BusinessModels, "timeout_ms": fields.TimeoutMS,
	} {
		if strings.TrimSpace(string(value)) == "null" {
			return cfg, fmt.Errorf("%s must not be null", field)
		}
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object: %w", err)
	}
	if cfg.TimeoutMS < 100 || cfg.TimeoutMS > 10000 {
		return cfg, fmt.Errorf("timeout_ms must be between 100 and 10000")
	}
	cfg.DecisionModel = strings.TrimSpace(cfg.DecisionModel)
	if cfg.DecisionChannelID < 0 {
		return cfg, fmt.Errorf("decision_channel_id cannot be negative")
	}
	seen := make(map[string]bool)
	for i, name := range cfg.BusinessModels {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, "*?") || name != model.RoutingModelName(name) {
			return cfg, fmt.Errorf("business_models[%d] must be an exact model name without a thinking suffix or wildcard", i)
		}
		if seen[name] {
			return cfg, fmt.Errorf("business_models contains duplicate model %q", name)
		}
		seen[name] = true
		cfg.BusinessModels[i] = name
	}
	if cfg.Enabled && (cfg.DecisionChannelID == 0 || cfg.DecisionModel == "" || len(cfg.BusinessModels) == 0) {
		return cfg, fmt.Errorf("enabled reasoning_auto requires a decision channel, decision model and business models")
	}
	return cfg, nil
}

func (s *Server) validateReasoningAutoChannel(ctx context.Context, cfg model.ReasoningAutoConfig) error {
	if !cfg.Enabled {
		return nil
	}
	for _, name := range cfg.BusinessModels {
		channels, err := s.store.GetEnabledChannelsByModel(ctx, name)
		if err != nil {
			return fmt.Errorf("load business model %q: %w", name, err)
		}
		if len(channels) == 0 {
			return fmt.Errorf("business model %q has no enabled chat channel", name)
		}
	}
	channel, err := s.store.GetConfig(ctx, cfg.DecisionChannelID)
	if err != nil {
		return fmt.Errorf("decision channel is unavailable: %w", err)
	}
	if !channel.Enabled {
		return fmt.Errorf("decision channel must be enabled")
	}
	// Ordinary chat channels are too slow to decide before every business request.
	if !channel.IsSystemOne() {
		return fmt.Errorf("decision channel must be a System One channel")
	}
	if !channel.SupportsModel(cfg.DecisionModel) {
		return fmt.Errorf("decision model %q is not enabled on the channel", cfg.DecisionModel)
	}
	keys, err := s.store.GetAPIKeys(ctx, channel.ID)
	if err != nil {
		return fmt.Errorf("load decision channel credentials: %w", err)
	}
	selected, found := s.firstModelRow(channel, cfg.DecisionModel)
	if !found {
		return fmt.Errorf("decision model has no usable row")
	}
	for _, key := range keys {
		if !key.Disabled && strings.TrimSpace(key.APIKey) != "" && s.keyModelScopeAllowsRow(channel, key, selected, []protocol.Protocol{protocol.SystemOne}) {
			return nil
		}
	}
	return fmt.Errorf("decision channel has no usable credential for the decision model")
}
