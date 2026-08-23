package app

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ==================== 共享数据结构 ====================
// 从admin.go提取共享类型,遵循SRP原则

// ChannelRequest 渠道创建/更新请求结构
type ChannelRequest struct {
	Name                    string                        `json:"name" binding:"required"`
	AuthType                string                        `json:"auth_type,omitempty"`
	APIKey                  string                        `json:"api_key"`
	APIKeys                 []ChannelAPIKeyRequest        `json:"api_keys,omitempty"`
	Websockets              bool                          `json:"websockets,omitempty"`
	ProtocolTransformMode   string                        `json:"protocol_transform_mode,omitempty"`
	KeyStrategy             string                        `json:"key_strategy,omitempty"` // Key使用策略:sequential, round_robin
	URLs                    model.ChannelURLs             `json:"urls" binding:"required,min=1"`
	Priority                int                           `json:"priority"`
	RPMLimit                int                           `json:"rpm_limit"`                       // 每分钟请求数限制，0表示无限制
	MaxConcurrency          int                           `json:"max_concurrency"`                 // 最大并发请求数，0表示无限制
	Models                  []model.ModelEntry            `json:"models" binding:"required,min=1"` // 模型配置（包含重定向）
	Enabled                 bool                          `json:"enabled"`
	ScheduledCheckEnabled   bool                          `json:"scheduled_check_enabled"`
	ScheduledCheckModel     string                        `json:"scheduled_check_model"`
	DailyCostLimit          float64                       `json:"daily_cost_limit"` // 每日成本限额（美元），0表示无限制
	CostMultiplier          float64                       `json:"cost_multiplier"`  // 成本倍率（默认1，0=免费，>=0）
	CustomRequestRules      *model.CustomRequestRules     `json:"custom_request_rules,omitempty"`
	CooldownDetectionRules  *model.CooldownDetectionRules `json:"cooldown_detection_rules,omitempty"`
	ProxyURL                string                        `json:"proxy_url,omitempty"` // 渠道级代理（http/https/socks5/socks5h）
	AvailableTimeStart      string                        `json:"available_time_start,omitempty"`
	AvailableTimeEnd        string                        `json:"available_time_end,omitempty"`
	RetryOtherKeysOnFailure bool                          `json:"retry_other_keys_on_failure"`
}

// ChannelAPIKeyRequest describes one submitted API key and its admin-only note.
type ChannelAPIKeyRequest struct {
	APIKey        string   `json:"api_key"`
	Note          string   `json:"note,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	// allowedModelsSet distinguishes an omitted field from an explicit empty list.
	// Updates preserve an existing scope when old clients do not send the new field.
	allowedModelsSet bool
}

// UnmarshalJSON records whether allowed_models was submitted so updates can preserve omitted scopes.
func (r *ChannelAPIKeyRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		APIKey        string          `json:"api_key"`
		Note          string          `json:"note,omitempty"`
		AllowedModels json.RawMessage `json:"allowed_models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	r.APIKey = raw.APIKey
	r.Note = raw.Note
	r.AllowedModels = nil
	r.allowedModelsSet = raw.AllowedModels != nil
	if !r.allowedModelsSet || string(raw.AllowedModels) == "null" {
		return nil
	}
	return json.Unmarshal(raw.AllowedModels, &r.AllowedModels)
}

const (
	maxAPIKeyNoteLength              = 512
	maxAPIKeyAllowedModelsJSONLength = 2000
)

func (cr *ChannelRequest) normalizeAPIKeys() []ChannelAPIKeyRequest {
	if len(cr.APIKeys) > 0 {
		keys := make([]ChannelAPIKeyRequest, 0, len(cr.APIKeys))
		for _, item := range cr.APIKeys {
			apiKey := strings.TrimSpace(item.APIKey)
			if apiKey == "" {
				continue
			}
			keys = append(keys, ChannelAPIKeyRequest{
				APIKey:           apiKey,
				Note:             strings.TrimSpace(item.Note),
				AllowedModels:    append([]string(nil), item.AllowedModels...),
				allowedModelsSet: item.allowedModelsSet,
			})
		}
		return keys
	}

	legacyKeys := util.ParseAPIKeys(cr.APIKey)
	keys := make([]ChannelAPIKeyRequest, 0, len(legacyKeys))
	for _, apiKey := range legacyKeys {
		keys = append(keys, ChannelAPIKeyRequest{APIKey: apiKey})
	}
	return keys
}

func apiKeyStrings(keys []ChannelAPIKeyRequest) []string {
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key.APIKey)
	}
	return values
}

func validateChannelBaseURL(raw, authType string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url cannot be empty")
	}

	exactURL := model.HasExactUpstreamURLMarker(raw)
	parseRaw := raw
	if exactURL {
		parseRaw = model.StripExactUpstreamURLMarker(raw)
	}

	u, err := neturl.Parse(parseRaw)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url: %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url scheme: %q (allowed: http, https)", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("url must not contain query or fragment")
	}

	// 普通渠道的 URL 不包含协议端点，运行时会追加 /v1/messages 等路径。
	// xAI OAuth 是提供商例外：其固定 base URL 本身以 /v1 结尾，运行时只追加 /responses。
	isXAIVersionedBasePath := model.NormalizeAuthType(authType) == model.AuthTypeXAIOAuth &&
		strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	// Z.ai Coding Plan is a second exception: ZCode publishes the routed
	// endpoint (…/api/v1/ultra-zai/anthropic) and runtime appends /v1/messages.
	isZAIRoutedBasePath := model.NormalizeAuthType(authType) == model.AuthTypeZAIOAuth
	if !exactURL && !isXAIVersionedBasePath && !isZAIRoutedBasePath && strings.Contains(u.Path, "/v1") {
		return "", fmt.Errorf("url should not contain API endpoint path like /v1 (current path: %q)", u.Path)
	}

	// 强制返回标准化格式（scheme://host+path，移除 trailing slash）
	// 例如: "https://example.com/api/" → "https://example.com/api"
	normalizedPath := strings.TrimSuffix(u.Path, "/")
	normalized := u.Scheme + "://" + u.Host + normalizedPath
	if exactURL {
		normalized += model.ExactUpstreamURLMarker
	}
	return normalized, nil
}

func normalizeChannelProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	proxyURL, err := neturl.Parse(raw)
	if err != nil || proxyURL.Host == "" {
		return "", fmt.Errorf("invalid proxy_url: %q", raw)
	}
	switch proxyURL.Scheme {
	case "http", "https", "socks5", "socks5h":
		return raw, nil
	default:
		return "", fmt.Errorf("invalid proxy_url scheme: %q (allowed: http, https, socks5, socks5h)", proxyURL.Scheme)
	}
}

func validateChannelURLConfigs(urls model.ChannelURLs, authType string) (model.ChannelURLs, error) {
	urls = urls.Clone()
	if len(urls) == 0 {
		return nil, fmt.Errorf("urls cannot be empty")
	}
	if err := urls.Normalize(); err != nil {
		return nil, err
	}
	for i := range urls {
		raw := urls[i].URL
		if urls[i].Exact {
			raw += model.ExactUpstreamURLMarker
		}
		normalized, err := validateChannelBaseURL(raw, authType)
		if err != nil {
			return nil, fmt.Errorf("urls[%d]: %w", i, err)
		}
		urls[i].Exact = model.HasExactUpstreamURLMarker(normalized)
		urls[i].URL = model.StripExactUpstreamURLMarker(normalized)
	}
	if err := urls.Normalize(); err != nil {
		return nil, err
	}
	return urls, nil
}

// Validate 实现RequestValidator接口
// [FIX] P0-1: 添加白名单校验和标准化（Fail-Fast + 边界防御）
func (cr *ChannelRequest) Validate() error {
	// 必填字段校验（现有逻辑保留）
	if strings.TrimSpace(cr.Name) == "" {
		return fmt.Errorf("name cannot be empty")
	}
	authType := model.NormalizeAuthType(cr.AuthType)
	if authType == "" {
		return fmt.Errorf("invalid auth_type %q", cr.AuthType)
	}
	cr.AuthType = authType
	apiKeys := cr.normalizeAPIKeys()
	if authType == model.AuthTypeAPIKey && len(apiKeys) == 0 {
		return fmt.Errorf("api_key cannot be empty")
	}
	if authType != model.AuthTypeAPIKey && len(apiKeys) != 0 {
		return fmt.Errorf("OAuth channel cannot contain API keys")
	}
	for i, key := range apiKeys {
		if strings.ContainsAny(key.APIKey, "\x00\r\n") {
			return fmt.Errorf("api_keys[%d].api_key contains illegal characters", i)
		}
		if len(key.Note) > maxAPIKeyNoteLength {
			return fmt.Errorf("api_keys[%d].note is too long (max %d bytes)", i, maxAPIKeyNoteLength)
		}
		if strings.Contains(key.Note, "\x00") {
			return fmt.Errorf("api_keys[%d].note contains illegal characters", i)
		}
	}
	if len(cr.Models) == 0 {
		return fmt.Errorf("models cannot be empty")
	}
	// 验证模型条目（DRY: 使用 ModelEntry.Validate()）
	for i := range cr.Models {
		if err := cr.Models[i].Validate(); err != nil {
			return fmt.Errorf("models[%d]: %w", i, err)
		}
	}
	// Fail-Fast: 同一渠道内模型名必须唯一（大小写不敏感，匹配数据库唯一约束语义）
	seenModels := make(map[string]int, len(cr.Models))
	canonicalModels := make(map[string]string, len(cr.Models))
	for i := range cr.Models {
		literalKey := strings.ToLower(cr.Models[i].Model)
		if firstIdx, exists := seenModels[literalKey]; exists {
			return fmt.Errorf("models[%d]: duplicate model %q (already defined at models[%d])", i, cr.Models[i].Model, firstIdx)
		}
		seenModels[literalKey] = i
		routingModel := model.RoutingModelName(cr.Models[i].Model)
		canonicalModels[strings.ToLower(routingModel)] = routingModel
	}
	wildcardModels := canonicalModels["*"] != ""
	for i := range apiKeys {
		allowedModels, err := normalizeAPIKeyAllowedModels(apiKeys[i].AllowedModels, canonicalModels, wildcardModels)
		if err != nil {
			return fmt.Errorf("api_keys[%d].allowed_models: %w", i, err)
		}
		apiKeys[i].AllowedModels = allowedModels
		encoded, err := json.Marshal(allowedModels)
		if err != nil {
			return fmt.Errorf("api_keys[%d].allowed_models: %w", i, err)
		}
		if len(encoded) > maxAPIKeyAllowedModelsJSONLength {
			return fmt.Errorf("api_keys[%d].allowed_models is too long (max %d bytes)", i, maxAPIKeyAllowedModelsJSONLength)
		}
	}
	cr.APIKeys = apiKeys
	cr.APIKey = strings.Join(apiKeyStrings(apiKeys), ",")

	cr.ScheduledCheckModel = strings.TrimSpace(cr.ScheduledCheckModel)
	if cr.ScheduledCheckModel != "" {
		if _, exists := canonicalModels[strings.ToLower(model.RoutingModelName(cr.ScheduledCheckModel))]; !exists {
			return fmt.Errorf("scheduled_check_model %q must exist in models", cr.ScheduledCheckModel)
		}
	}

	// URL 能力属于具体端点；先规范化结构，再逐项验证网络地址。
	var err error
	cr.URLs, err = validateChannelURLConfigs(cr.URLs, authType)
	if err != nil {
		return err
	}

	rawProtocolTransformMode := cr.ProtocolTransformMode
	cr.ProtocolTransformMode = model.NormalizeProtocolTransformMode(rawProtocolTransformMode)
	if cr.ProtocolTransformMode == "" {
		return fmt.Errorf("invalid protocol_transform_mode: %q (allowed: auto, upstream, local)", rawProtocolTransformMode)
	}
	// [FIX] key_strategy 白名单校验 + 标准化
	// 设计：空值允许（使用默认值sequential），非空值必须合法
	cr.KeyStrategy = strings.TrimSpace(cr.KeyStrategy)
	if cr.KeyStrategy != "" {
		// 先标准化（小写化）
		normalized := strings.ToLower(cr.KeyStrategy)
		// 再白名单校验
		if !model.IsValidKeyStrategy(normalized) {
			return fmt.Errorf("invalid key_strategy: %q (allowed: sequential, round_robin)", cr.KeyStrategy)
		}
		cr.KeyStrategy = normalized // 应用标准化结果
	}

	if err := validateCustomRequestRules(cr.CustomRequestRules); err != nil {
		return err
	}
	if cr.CustomRequestRules != nil && cr.CustomRequestRules.IsEmpty() {
		cr.CustomRequestRules = nil
	}
	if err := cooldown.NormalizeCooldownDetectionRules(cr.CooldownDetectionRules); err != nil {
		return err
	}
	if cr.CooldownDetectionRules != nil && cr.CooldownDetectionRules.IsEmpty() {
		cr.CooldownDetectionRules = nil
	}

	normalizedProxyURL, err := normalizeChannelProxyURL(cr.ProxyURL)
	if err != nil {
		return err
	}
	cr.ProxyURL = normalizedProxyURL
	cr.AvailableTimeStart = strings.TrimSpace(cr.AvailableTimeStart)
	cr.AvailableTimeEnd = strings.TrimSpace(cr.AvailableTimeEnd)
	available := &model.Config{
		AvailableTimeStart: cr.AvailableTimeStart,
		AvailableTimeEnd:   cr.AvailableTimeEnd,
	}
	if err := available.NormalizeAvailableTime(); err != nil {
		return err
	}
	cr.AvailableTimeStart = available.AvailableTimeStart
	cr.AvailableTimeEnd = available.AvailableTimeEnd

	if cr.RPMLimit < 0 {
		return fmt.Errorf("rpm_limit must be >= 0 (got %d)", cr.RPMLimit)
	}
	if cr.MaxConcurrency < 0 {
		return fmt.Errorf("max_concurrency must be >= 0 (got %d)", cr.MaxConcurrency)
	}

	// CostMultiplier: 未传视为默认 1；0 表示免费渠道；负数拒绝
	if cr.CostMultiplier == 0 {
		// 0 是合法值（免费渠道），保持不变
	} else if cr.CostMultiplier < 0 {
		return fmt.Errorf("cost_multiplier must be >= 0 (got %v)", cr.CostMultiplier)
	}

	return nil
}

func normalizeAPIKeyAllowedModels(values []string, canonicalModels map[string]string, wildcard bool) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		modelName := model.RoutingModelName(strings.TrimSpace(value))
		if modelName == "" {
			continue
		}
		if modelName == "*" {
			return nil, nil
		}
		key := strings.ToLower(modelName)
		if _, exists := seen[key]; exists {
			continue
		}
		canonical, exists := canonicalModels[key]
		if !exists && !wildcard {
			return nil, fmt.Errorf("model %q must exist in channel models", modelName)
		}
		if exists {
			modelName = canonical
		}
		seen[key] = struct{}{}
		result = append(result, modelName)
	}
	return result, nil
}

// ToConfig 转换为Config结构(不包含API Key,API Key单独处理)
// 规范化重定向模型：如果 RedirectModel == Model 则清空（透传语义，节省存储）
func (cr *ChannelRequest) ToConfig() *model.Config {
	// 规范化模型条目：同名重定向清空为透传
	normalizedModels := make([]model.ModelEntry, len(cr.Models))
	for i, m := range cr.Models {
		normalizedModels[i] = m
		if m.RedirectModel == m.Model {
			normalizedModels[i].RedirectModel = ""
		}
	}

	return &model.Config{
		Name:                    strings.TrimSpace(cr.Name),
		AuthType:                cr.AuthType,
		Websockets:              cr.Websockets,
		ProtocolTransformMode:   cr.ProtocolTransformMode,
		URLs:                    cr.URLs.Clone(),
		Priority:                cr.Priority,
		RPMLimit:                cr.RPMLimit,
		MaxConcurrency:          cr.MaxConcurrency,
		ModelEntries:            normalizedModels,
		Enabled:                 cr.Enabled,
		ScheduledCheckEnabled:   cr.ScheduledCheckEnabled,
		ScheduledCheckModel:     cr.ScheduledCheckModel,
		DailyCostLimit:          cr.DailyCostLimit,
		CostMultiplier:          cr.CostMultiplier,
		CustomRequestRules:      cr.CustomRequestRules.Clone(),
		CooldownDetectionRules:  cr.CooldownDetectionRules.Clone(),
		ProxyURL:                cr.ProxyURL,
		AvailableTimeStart:      cr.AvailableTimeStart,
		AvailableTimeEnd:        cr.AvailableTimeEnd,
		RetryOtherKeysOnFailure: cr.RetryOtherKeysOnFailure,
	}
}

const (
	maxCustomRuleEntries = 32
	maxCustomRuleValue   = 8 * 1024
	maxCustomRuleName    = 256
)

// validateCustomRequestRules 校验渠道自定义请求规则；副作用：修剪名称/路径空白并丢弃 remove 规则的 value。
func validateCustomRequestRules(r *model.CustomRequestRules) error {
	if r == nil {
		return nil
	}
	if len(r.Headers) > maxCustomRuleEntries {
		return fmt.Errorf("custom_request_rules.headers: too many entries (max %d)", maxCustomRuleEntries)
	}
	if len(r.Body) > maxCustomRuleEntries {
		return fmt.Errorf("custom_request_rules.body: too many entries (max %d)", maxCustomRuleEntries)
	}

	for i := range r.Headers {
		h := &r.Headers[i]
		action := strings.ToLower(strings.TrimSpace(h.Action))
		if action != model.RuleActionRemove && action != model.RuleActionOverride && action != model.RuleActionAppend {
			return fmt.Errorf("custom_request_rules.headers[%d]: invalid action %q (allowed: remove, override, append)", i, h.Action)
		}
		h.Action = action

		name := strings.TrimSpace(h.Name)
		if name == "" {
			return fmt.Errorf("custom_request_rules.headers[%d]: name cannot be empty", i)
		}
		if len(name) > maxCustomRuleName {
			return fmt.Errorf("custom_request_rules.headers[%d]: name too long (max %d)", i, maxCustomRuleName)
		}
		if strings.ContainsAny(name, "\r\n\x00") {
			return fmt.Errorf("custom_request_rules.headers[%d]: name contains illegal characters", i)
		}
		h.Name = name

		// remove：value 为空=删整条；非空=按逗号 token 精确移除（与 override/append 同等做校验）
		if len(h.Value) > maxCustomRuleValue {
			return fmt.Errorf("custom_request_rules.headers[%d]: value too long (max %d bytes)", i, maxCustomRuleValue)
		}
		if strings.ContainsAny(h.Value, "\r\n\x00") {
			return fmt.Errorf("custom_request_rules.headers[%d]: value contains illegal characters", i)
		}
	}

	for i := range r.Body {
		b := &r.Body[i]
		action := strings.ToLower(strings.TrimSpace(b.Action))
		if action != model.RuleActionRemove && action != model.RuleActionOverride {
			return fmt.Errorf("custom_request_rules.body[%d]: invalid action %q (allowed: remove, override)", i, b.Action)
		}
		b.Action = action

		path := strings.TrimSpace(b.Path)
		if path == "" {
			return fmt.Errorf("custom_request_rules.body[%d]: path cannot be empty", i)
		}
		if len(path) > maxCustomRuleName {
			return fmt.Errorf("custom_request_rules.body[%d]: path too long (max %d)", i, maxCustomRuleName)
		}
		if !isValidCustomRulePath(path) {
			return fmt.Errorf("custom_request_rules.body[%d]: path contains illegal characters (allowed: letters, digits, _, -, .)", i)
		}
		b.Path = path

		if action == model.RuleActionRemove {
			b.Value = nil
			continue
		}
		if len(b.Value) == 0 {
			return fmt.Errorf("custom_request_rules.body[%d]: override requires value", i)
		}
		if len(b.Value) > maxCustomRuleValue {
			return fmt.Errorf("custom_request_rules.body[%d]: value too long (max %d bytes)", i, maxCustomRuleValue)
		}
		var parsed any
		if err := json.Unmarshal(b.Value, &parsed); err != nil {
			return fmt.Errorf("custom_request_rules.body[%d]: value is not valid JSON (%v)", i, err)
		}
	}
	return nil
}

// isValidCustomRulePath 允许字符：字母、数字、下划线、连字符、点。
func isValidCustomRulePath(p string) bool {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// KeyCooldownInfo Key级别冷却信息
type KeyCooldownInfo struct {
	KeyIndex            int        `json:"key_index"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

// ModelCooldownInfo 模型级冷却信息。Model 是实际上游模型名，不是外部别名。
type ModelCooldownInfo struct {
	Model               string     `json:"model"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

// ChannelModelStats 是渠道编辑器需要的轻量模型统计，避免复用完整统计接口时额外计算 RPM 和健康时间线。
type ChannelModelStats struct {
	Model                   string   `json:"model"`
	Success                 int      `json:"success"`
	Error                   int      `json:"error"`
	Total                   int      `json:"total"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"`
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`
}

// ChannelWithCooldown 带冷却状态的渠道响应结构
type ChannelWithCooldown struct {
	*model.Config
	CodexPlanType                string              `json:"codex_plan_type,omitempty"`
	CodexSubscriptionActiveUntil *time.Time          `json:"codex_subscription_active_until,omitempty"`
	AnthropicPlanType            string              `json:"anthropic_plan_type,omitempty"`
	OAuthUsage                   *oauthUsageSummary  `json:"oauth_usage,omitempty"`
	AntigravityPaidTier          string              `json:"antigravity_paid_tier,omitempty"`
	XAIEmail                     string              `json:"xai_email,omitempty"`
	XAISubscriptionTier          string              `json:"xai_subscription_tier,omitempty"`
	XAIEntitlementStatus         string              `json:"xai_entitlement_status,omitempty"`
	KeyStrategy                  string              `json:"key_strategy,omitempty"` // [INFO] 修复 (2025-10-11): 添加key_strategy字段
	CooldownUntil                *time.Time          `json:"cooldown_until,omitempty"`
	CooldownRemainingMS          int64               `json:"cooldown_remaining_ms,omitempty"`
	KeyCooldowns                 []KeyCooldownInfo   `json:"key_cooldowns,omitempty"`
	ModelCooldowns               []ModelCooldownInfo `json:"model_cooldowns,omitempty"`
	EffectivePriority            *float64            `json:"effective_priority,omitempty"` // 健康度模式下的有效优先级
	SuccessRate                  *float64            `json:"success_rate,omitempty"`       // 成功率(0-1)
}

// ChannelImportSummary 导入结果统计
type ChannelImportSummary struct {
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Processed int      `json:"processed"`
	Errors    []string `json:"errors,omitempty"`
}

// CooldownRequest 冷却设置请求
type CooldownRequest struct {
	DurationMs int64 `json:"duration_ms" binding:"required,min=1000"` // 最少1秒
}

// SettingUpdateRequest 系统配置更新请求
type SettingUpdateRequest struct {
	Value string `json:"value" binding:"required"`
}

// CheckDuplicateRequest 渠道重复检测请求
type CheckDuplicateRequest struct {
	URLs model.ChannelURLs `json:"urls" binding:"required,min=1"`
}

// Validate implements RequestValidator.
func (r *CheckDuplicateRequest) Validate() error {
	urls, err := validateChannelURLConfigs(r.URLs, model.AuthTypeAPIKey)
	if err != nil {
		return err
	}
	r.URLs = urls
	return nil
}

// DuplicateChannelInfo 重复渠道信息
type DuplicateChannelInfo struct {
	ID   int64             `json:"id"`
	Name string            `json:"name"`
	URLs model.ChannelURLs `json:"urls"`
}

// CheckDuplicateResponse 重复检测响应
type CheckDuplicateResponse struct {
	Duplicates []DuplicateChannelInfo `json:"duplicates"`
}
