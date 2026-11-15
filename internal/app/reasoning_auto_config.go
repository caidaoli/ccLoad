package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const reasoningAutoSettingKey = "reasoning_auto"

/**
 * @brief 解析并验证持久化的动态思考配置；旧 decision_channel_id 字段作为未知字段忽略
 * @param raw 系统设置中的 JSON 对象
 * @return 规范化后的配置，失败时可能只完成部分解析
 * @return JSON、字段类型或配置约束非法时返回错误
 */
func ParseReasoningAutoConfig(raw string) (model.ReasoningAutoConfig, error) {
	cfg := model.ReasoningAutoConfig{BusinessModels: []string{}, TimeoutMS: 2000}
	var fields struct {
		Enabled        json.RawMessage `json:"enabled"`
		DecisionModel  json.RawMessage `json:"decision_model"`
		BusinessModels json.RawMessage `json:"business_models"`
		TimeoutMS      json.RawMessage `json:"timeout_ms"`
		PromptTemplate json.RawMessage `json:"prompt_template"`
		CustomPrompt   json.RawMessage `json:"custom_prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object: %w", err)
	}
	if strings.TrimSpace(raw) == "null" {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object")
	}
	for field, value := range map[string]json.RawMessage{
		"enabled":        fields.Enabled,
		"decision_model": fields.DecisionModel, "business_models": fields.BusinessModels, "timeout_ms": fields.TimeoutMS,
		"prompt_template": fields.PromptTemplate, "custom_prompt": fields.CustomPrompt,
	} {
		if strings.TrimSpace(string(value)) == "null" {
			return cfg, fmt.Errorf("%s must not be null", field)
		}
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("reasoning_auto must be a JSON object: %w", err)
	}
	if err := validateReasoningPromptConfig(cfg, fields.CustomPrompt); err != nil {
		return cfg, err
	}
	if cfg.TimeoutMS < 100 || cfg.TimeoutMS > 10000 {
		return cfg, fmt.Errorf("timeout_ms must be between 100 and 10000")
	}
	cfg.DecisionModel = strings.TrimSpace(cfg.DecisionModel)
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
	if cfg.Enabled && (cfg.DecisionModel == "" || len(cfg.BusinessModels) == 0) {
		return cfg, fmt.Errorf("enabled reasoning_auto requires a decision model and business models")
	}
	return cfg, nil
}

/**
 * @brief 验证持久化模板选择和自定义快照的完整性
 * @param cfg 已完成 JSON 类型解析的配置
 * @param rawPrompt 自定义模板原始 JSON，用于限制持久化内容大小
 * @return 模板名、字段或文本边界不合法时返回错误
 */
func validateReasoningPromptConfig(cfg model.ReasoningAutoConfig, rawPrompt json.RawMessage) error {
	switch cfg.PromptTemplate {
	case "", "auto", "jev", "aliyun", "gemini", "openai":
		if cfg.CustomPrompt != nil {
			return fmt.Errorf("custom_prompt requires prompt_template custom")
		}
		return nil
	case "custom":
		if cfg.CustomPrompt == nil {
			return fmt.Errorf("prompt_template custom requires custom_prompt")
		}
	default:
		return fmt.Errorf("unknown prompt_template %q", cfg.PromptTemplate)
	}
	if len(rawPrompt) > 65536 {
		return fmt.Errorf("custom_prompt must not exceed 65536 bytes")
	}
	prompt := cfg.CustomPrompt
	decoder := json.NewDecoder(bytes.NewReader(rawPrompt))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(prompt); err != nil {
		return fmt.Errorf("custom_prompt: %w", err)
	}
	if err := validateReasoningPromptQuestion(prompt.Instructions, prompt.Criteria, []string{"low", "medium", "high", "xhigh", "max"}); err != nil {
		return fmt.Errorf("custom_prompt: %w", err)
	}
	if prompt.Depth != nil {
		if err := validateReasoningPromptQuestion(prompt.Depth.Instructions, prompt.Depth.Criteria, []string{"high", "xhigh", "max"}); err != nil {
			return fmt.Errorf("custom_prompt.depth: %w", err)
		}
	}
	return nil
}

/**
 * @brief 保存配置时确认业务模型和判断模型至少存在可用的渠道配置
 * @param ctx 数据库读取上下文
 * @param cfg 已通过字段校验的配置
 * @return 模型无启用渠道、判断模型无授权凭据或读取失败时返回错误；关闭判断时为 nil
 */
func (s *Server) validateReasoningAutoModels(ctx context.Context, cfg model.ReasoningAutoConfig) error {
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
	channels, err := s.store.GetEnabledSystemOneChannelsByModel(ctx, cfg.DecisionModel)
	if err != nil {
		return fmt.Errorf("load decision model channels: %w", err)
	}
	for _, channel := range channels {
		keys, err := s.store.GetAPIKeys(ctx, channel.ID)
		if err != nil {
			return fmt.Errorf("load decision channel credentials: %w", err)
		}
		for _, row := range s.enumerateModelRows(channel, cfg.DecisionModel) {
			for _, key := range keys {
				if !key.Disabled && strings.TrimSpace(key.APIKey) != "" && s.keyModelScopeAllowsRow(channel, key, row, []protocol.Protocol{protocol.SystemOne}) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("decision model %q has no enabled System One channel with a usable credential", cfg.DecisionModel)
}
