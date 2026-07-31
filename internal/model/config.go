package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// ProtocolTransformModeAuto tries the client protocol first, then falls back through
	// Anthropic, OpenAI, Codex, Gemini while skipping the native protocol already attempted.
	ProtocolTransformModeAuto = "auto"
	// ProtocolTransformModeUpstream always forwards the client protocol natively.
	ProtocolTransformModeUpstream = "upstream"
	// ProtocolTransformModeLocal translates to URL-declared protocols or the local fallback order.
	ProtocolTransformModeLocal = "local"
	// ExactUpstreamURLMarker marks a configured channel URL as the exact upstream request URL.
	ExactUpstreamURLMarker = "#"
)

// NormalizeProtocolTransformMode normalizes persisted/admin values.
// Empty means the current default policy: automatic negotiation.
func NormalizeProtocolTransformMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", ProtocolTransformModeAuto:
		return ProtocolTransformModeAuto
	case ProtocolTransformModeUpstream:
		return ProtocolTransformModeUpstream
	case ProtocolTransformModeLocal:
		return ProtocolTransformModeLocal
	default:
		return ""
	}
}

// HasExactUpstreamURLMarker reports whether raw ends with the exact upstream URL marker.
func HasExactUpstreamURLMarker(raw string) bool {
	return strings.HasSuffix(strings.TrimSpace(raw), ExactUpstreamURLMarker)
}

// StripExactUpstreamURLMarker trims spaces and removes the exact upstream URL marker when present.
func StripExactUpstreamURLMarker(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), ExactUpstreamURLMarker)
}

var supportedURLProtocols = map[string]struct{}{
	"anthropic": {},
	"codex":     {},
	"openai":    {},
	"gemini":    {},
}

// ChannelURL is one configured upstream endpoint. Protocols is the ordered list
// of wire protocols accepted by this endpoint; an empty list means automatic detection.
type ChannelURL struct {
	URL       string   `json:"url"`
	Exact     bool     `json:"exact,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
}

// ChannelURLs is the persisted ordered URL configuration.
type ChannelURLs []ChannelURL

// UsesAutomaticProtocolDetection reports whether runtime capability learning owns
// protocol selection for this URL.
func (u ChannelURL) UsesAutomaticProtocolDetection() bool {
	return len(u.Protocols) == 0
}

// SupportsProtocol reports whether this URL can accept protocol. URLs without an
// explicit declaration remain eligible for automatic detection.
func (u ChannelURL) SupportsProtocol(value string) bool {
	if u.UsesAutomaticProtocolDetection() {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	return slices.Contains(u.Protocols, value)
}

// RuntimeURL returns the existing forwarding key used by URL selection and exact
// URL handling. The marker is derived at runtime and is never persisted.
func (u ChannelURL) RuntimeURL() string {
	if u.Exact {
		return u.URL + ExactUpstreamURLMarker
	}
	return u.URL
}

// Normalize validates and canonicalizes URL entries in place.
func (urls *ChannelURLs) Normalize() error {
	if urls == nil {
		return errors.New("urls cannot be nil")
	}
	if len(*urls) == 0 {
		return errors.New("urls cannot be empty")
	}
	seenURLs := make(map[string]int, len(*urls))
	for i := range *urls {
		entry := &(*urls)[i]
		entry.URL = strings.TrimSpace(entry.URL)
		if entry.URL == "" {
			return fmt.Errorf("urls[%d].url cannot be empty", i)
		}
		if strings.HasSuffix(entry.URL, ExactUpstreamURLMarker) {
			return fmt.Errorf("urls[%d].url must not contain exact marker", i)
		}

		selected := make(map[string]struct{}, len(entry.Protocols))
		normalized := make([]string, 0, len(entry.Protocols))
		for _, rawProtocol := range entry.Protocols {
			value := strings.ToLower(strings.TrimSpace(rawProtocol))
			if _, ok := supportedURLProtocols[value]; !ok {
				return fmt.Errorf("urls[%d].protocols contains unsupported protocol %q", i, rawProtocol)
			}
			if _, exists := selected[value]; exists {
				continue
			}
			selected[value] = struct{}{}
			normalized = append(normalized, value)
		}
		entry.Protocols = normalized
		if len(entry.Protocols) == 0 {
			entry.Protocols = nil
		}

		runtimeURL := entry.RuntimeURL()
		if previous, ok := seenURLs[runtimeURL]; ok {
			return fmt.Errorf("urls[%d] duplicates urls[%d]", i, previous)
		}
		seenURLs[runtimeURL] = i
	}
	return nil
}

// Clone returns a deep copy of URL configuration.
func (urls ChannelURLs) Clone() ChannelURLs {
	if urls == nil {
		return nil
	}
	clone := make(ChannelURLs, len(urls))
	for i := range urls {
		clone[i] = urls[i]
		clone[i].Protocols = append([]string(nil), urls[i].Protocols...)
	}
	return clone
}

// Value serializes ChannelURLs for the channels.url TEXT column.
func (urls ChannelURLs) Value() (driver.Value, error) {
	clone := urls.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(clone)
	if err != nil {
		return nil, fmt.Errorf("marshal channel urls: %w", err)
	}
	return string(encoded), nil
}

// Scan decodes the structured channels.url column.
func (urls *ChannelURLs) Scan(src any) error {
	var raw []byte
	switch value := src.(type) {
	case string:
		raw = []byte(value)
	case []byte:
		raw = value
	default:
		return fmt.Errorf("scan channel urls from %T", src)
	}
	if err := json.Unmarshal(raw, urls); err != nil {
		return fmt.Errorf("decode structured channel urls: %w", err)
	}
	if err := urls.Normalize(); err != nil {
		return fmt.Errorf("normalize structured channel urls: %w", err)
	}
	return nil
}

// ModelEntry 模型配置条目
type ModelEntry struct {
	Model         string `json:"model"`                    // 模型名称
	RedirectModel string `json:"redirect_model,omitempty"` // 重定向目标模型（空表示不重定向）
	Disabled      bool   `json:"disabled,omitempty"`       // 是否停用该渠道的此模型
}

// Validate 验证并规范化模型条目
// 返回 error 如果验证失败，否则返回 nil
// 副作用：会 trim 空白字符并写回 Model 和 RedirectModel 字段
func (e *ModelEntry) Validate() error {
	e.Model = strings.TrimSpace(e.Model)
	if e.Model == "" {
		return errors.New("model cannot be empty")
	}
	if strings.ContainsAny(e.Model, "\x00\r\n") {
		return errors.New("model contains illegal characters")
	}

	e.RedirectModel = strings.TrimSpace(e.RedirectModel)
	if strings.ContainsAny(e.RedirectModel, "\x00\r\n") {
		return errors.New("redirect_model contains illegal characters")
	}
	return nil
}

// 自定义请求规则动作常量
const (
	RuleActionRemove   = "remove"
	RuleActionOverride = "override"
	RuleActionAppend   = "append"
)

// CustomHeaderRule 单条自定义 HTTP 请求头规则
type CustomHeaderRule struct {
	Action string `json:"action"`          // remove | override | append
	Name   string `json:"name"`            // header 名，保持原大小写
	Value  string `json:"value,omitempty"` // remove 时忽略
}

// CustomBodyRule 单条自定义 JSON 请求体规则
type CustomBodyRule struct {
	Action string          `json:"action"`          // remove | override
	Path   string          `json:"path"`            // 点分路径，支持整数数组索引
	Value  json.RawMessage `json:"value,omitempty"` // remove 时忽略；任意 JSON 字面量
}

// CustomRequestRules 渠道级自定义请求改写规则集
type CustomRequestRules struct {
	Headers []CustomHeaderRule `json:"headers,omitempty"`
	Body    []CustomBodyRule   `json:"body,omitempty"`
}

// IsEmpty 当两类规则均为空时返回 true
func (r *CustomRequestRules) IsEmpty() bool {
	if r == nil {
		return true
	}
	return len(r.Headers) == 0 && len(r.Body) == 0
}

// Clone returns an independent copy suitable for config-cache boundaries.
func (r *CustomRequestRules) Clone() *CustomRequestRules {
	if r == nil {
		return nil
	}
	out := &CustomRequestRules{
		Headers: append([]CustomHeaderRule(nil), r.Headers...),
		Body:    make([]CustomBodyRule, len(r.Body)),
	}
	for i, rule := range r.Body {
		out.Body[i] = rule
		out.Body[i].Value = append(json.RawMessage(nil), rule.Value...)
	}
	return out
}

const (
	// CooldownScopeKey cools the currently selected API key.
	CooldownScopeKey = "key"
	// CooldownScopeModel cools the actual upstream model for this channel.
	CooldownScopeModel = "model"
	// CooldownScopeChannel cools the entire channel.
	CooldownScopeChannel = "channel"

	// CooldownModeFixed uses a configured fixed duration.
	CooldownModeFixed = "fixed"
	// CooldownModeResetTime parses a named regex capture into an exact reset time.
	CooldownModeResetTime = "reset_time"

	// CooldownTimeFormatDateTime parses the capture with a Go time layout.
	CooldownTimeFormatDateTime = "datetime"
	// CooldownTimeFormatTimeOfDay resolves a captured clock value to its next occurrence.
	CooldownTimeFormatTimeOfDay = "time_of_day"
	// CooldownTimeFormatUnix treats the capture as Unix seconds.
	CooldownTimeFormatUnix = "unix"
	// CooldownTimeFormatUnixMilliseconds treats the capture as Unix milliseconds.
	CooldownTimeFormatUnixMilliseconds = "unix_ms"
	// CooldownTimeFormatDurationSeconds treats the capture as seconds after the response.
	CooldownTimeFormatDurationSeconds = "duration_seconds"
)

// CooldownDetectionRule describes one configured upstream error policy.
// Rules are evaluated by ascending Priority; the first match wins.
type CooldownDetectionRule struct {
	Enabled        bool   `json:"enabled"`
	Name           string `json:"name,omitempty"`
	Priority       int    `json:"priority"`
	StatusCodes    []int  `json:"status_codes,omitempty"`
	MessagePattern string `json:"message_pattern,omitempty"`

	Scope string `json:"scope"` // key | model | channel
	Mode  string `json:"mode"`  // fixed | reset_time

	CooldownSeconds int64  `json:"cooldown_seconds,omitempty"`
	TimeCapture     string `json:"time_capture,omitempty"`
	TimeFormat      string `json:"time_format,omitempty"` // datetime | time_of_day | unix | unix_ms | duration_seconds
	TimeLayout      string `json:"time_layout,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
}

// CooldownDetectionRules groups configured upstream error rules.
type CooldownDetectionRules struct {
	Rules []CooldownDetectionRule `json:"rules,omitempty"`
}

// IsEmpty reports whether there are no configured cooldown detection rules.
func (r *CooldownDetectionRules) IsEmpty() bool {
	return r == nil || len(r.Rules) == 0
}

// Clone returns an independent copy suitable for config-cache boundaries.
func (r *CooldownDetectionRules) Clone() *CooldownDetectionRules {
	if r == nil {
		return nil
	}
	out := &CooldownDetectionRules{Rules: make([]CooldownDetectionRule, len(r.Rules))}
	for i, rule := range r.Rules {
		out.Rules[i] = rule
		out.Rules[i].StatusCodes = append([]int(nil), rule.StatusCodes...)
	}
	return out
}

// Config 渠道配置
type Config struct {
	ID                    int64       `json:"id"`
	Name                  string      `json:"name"`
	Websockets            bool        `json:"websockets,omitempty"`
	ProtocolTransformMode string      `json:"protocol_transform_mode"`
	URLs                  ChannelURLs `json:"urls"`
	Priority              int         `json:"priority"`
	RPMLimit              int         `json:"rpm_limit"`       // 每分钟请求数限制，0表示无限制
	MaxConcurrency        int         `json:"max_concurrency"` // 最大并发请求数，0表示无限制
	Enabled               bool        `json:"enabled"`
	ScheduledCheckEnabled bool        `json:"scheduled_check_enabled"`
	ScheduledCheckModel   string      `json:"scheduled_check_model"`

	// 模型配置（统一管理模型和重定向）
	ModelEntries []ModelEntry `json:"models"`

	// 渠道级冷却（从cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`       // Unix秒时间戳，0表示无冷却
	CooldownDurationMs int64 `json:"cooldown_duration_ms"` // 冷却持续时间（毫秒）

	// 每日成本限额
	DailyCostLimit float64 `json:"daily_cost_limit"` // 每日成本限额（美元），0表示无限制

	// 成本倍率：标准成本×倍率=实际计费成本，默认1
	CostMultiplier float64 `json:"cost_multiplier"`

	// 自定义请求规则（nil 表示无改写）
	CustomRequestRules *CustomRequestRules `json:"custom_request_rules,omitempty"`

	// 渠道级上游错误冷却探测规则（nil 表示仅使用内置分类器）
	CooldownDetectionRules *CooldownDetectionRules `json:"cooldown_detection_rules,omitempty"`

	// 渠道级代理（http/https/socks5/socks5h），空串=环境变量代理
	ProxyURL string `json:"proxy_url,omitempty"`

	// 渠道故障时先将当前 Key 冷却并尝试同渠道其他 Key。
	// 用于一个中转站下的 Key 实际对应不同上游服务商的场景；默认关闭，保持原有渠道/模型级切换语义。
	RetryOtherKeysOnFailure bool `json:"retry_other_keys_on_failure"`

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 缓存Key数量，避免冷却判断时的N+1查询
	KeyCount int `json:"key_count"` // API Key数量（查询时JOIN计算）

	// 运行时路由标记：该候选来自“所有渠道冷却”兜底，不持久化、不序列化。
	CooldownFallback bool `json:"-"`

	// 模型查找索引（懒加载，不序列化）
	modelIndex map[string]*ModelEntry `json:"-"`
	indexMu    sync.RWMutex           `json:"-"` // 保护索引的并发访问
}

// Clone 返回 Config 的深拷贝。
// 拷贝所有可变字段，
// 重置懒加载索引（modelIndex + indexMu），避免共享 sync.RWMutex 与指向旧 slice 的 map。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	dst := &Config{
		ID:                      c.ID,
		Name:                    c.Name,
		Websockets:              c.Websockets,
		ProtocolTransformMode:   c.ProtocolTransformMode,
		URLs:                    c.URLs.Clone(),
		Priority:                c.Priority,
		RPMLimit:                c.RPMLimit,
		MaxConcurrency:          c.MaxConcurrency,
		Enabled:                 c.Enabled,
		ScheduledCheckEnabled:   c.ScheduledCheckEnabled,
		ScheduledCheckModel:     c.ScheduledCheckModel,
		CooldownUntil:           c.CooldownUntil,
		CooldownDurationMs:      c.CooldownDurationMs,
		DailyCostLimit:          c.DailyCostLimit,
		CostMultiplier:          c.CostMultiplier,
		CustomRequestRules:      c.CustomRequestRules.Clone(),
		CooldownDetectionRules:  c.CooldownDetectionRules.Clone(),
		ProxyURL:                c.ProxyURL,
		RetryOtherKeysOnFailure: c.RetryOtherKeysOnFailure,
		CreatedAt:               c.CreatedAt,
		UpdatedAt:               c.UpdatedAt,
		KeyCount:                c.KeyCount,
		CooldownFallback:        c.CooldownFallback,
	}
	if c.ModelEntries != nil {
		dst.ModelEntries = make([]ModelEntry, len(c.ModelEntries))
		copy(dst.ModelEntries, c.ModelEntries)
	}
	return dst
}

// GetModels 获取所有已启用的模型名称列表
func (c *Config) GetModels() []string {
	models := make([]string, 0, len(c.ModelEntries))
	for _, e := range c.ModelEntries {
		if e.Disabled {
			continue
		}
		models = append(models, e.Model)
	}
	return models
}

// GetProtocolTransformMode returns the normalized channel policy.
func (c *Config) GetProtocolTransformMode() string {
	mode := NormalizeProtocolTransformMode(c.ProtocolTransformMode)
	if mode == "" {
		return ProtocolTransformModeAuto
	}
	return mode
}

// GetURLs returns the runtime URL keys used by forwarding and URL state.
func (c *Config) GetURLs() []string {
	urls := make([]string, len(c.URLs))
	for i := range c.URLs {
		urls[i] = c.URLs[i].RuntimeURL()
	}
	return urls
}

// buildIndexIfNeeded 懒加载构建模型查找索引（性能优化：O(n) → O(1)）
// 使用双重检查锁定（DCL）模式保证并发安全
func (c *Config) buildIndexIfNeeded() {
	// 快路径：读锁检查
	c.indexMu.RLock()
	if c.modelIndex != nil {
		c.indexMu.RUnlock()
		return
	}
	c.indexMu.RUnlock()

	// 慢路径：写锁构建
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	// 双重检查：可能其他 goroutine 已构建
	if c.modelIndex != nil {
		return
	}
	c.modelIndex = make(map[string]*ModelEntry, len(c.ModelEntries))
	for i := range c.ModelEntries {
		if c.ModelEntries[i].Disabled {
			continue
		}
		c.modelIndex[c.ModelEntries[i].Model] = &c.ModelEntries[i]
	}
}

// GetRedirectModel 获取模型的重定向目标
// 返回 (目标模型, 是否有重定向)
func (c *Config) GetRedirectModel(model string) (string, bool) {
	c.buildIndexIfNeeded()
	c.indexMu.RLock()
	defer c.indexMu.RUnlock()
	if entry, exists := c.modelIndex[model]; exists && entry.RedirectModel != "" {
		return entry.RedirectModel, true
	}
	return "", false
}

// SupportsModel 检查渠道是否支持指定模型
func (c *Config) SupportsModel(model string) bool {
	c.buildIndexIfNeeded()
	c.indexMu.RLock()
	defer c.indexMu.RUnlock()
	_, exists := c.modelIndex[model]
	return exists
}

// IsCoolingDown 检查渠道是否处于冷却状态
func (c *Config) IsCoolingDown(now time.Time) bool {
	return c.CooldownUntil > now.Unix()
}

// KeyStrategy 常量定义
const (
	KeyStrategySequential = "sequential"  // 顺序选择：按索引顺序尝试Key
	KeyStrategyRoundRobin = "round_robin" // 轮询选择：均匀分布请求到各个Key
)

// IsValidKeyStrategy 验证KeyStrategy是否有效
func IsValidKeyStrategy(s string) bool {
	return s == "" || s == KeyStrategySequential || s == KeyStrategyRoundRobin
}

// APIKey 表示渠道的 API 密钥配置
type APIKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyIndex  int    `json:"key_index"`
	APIKey    string `json:"api_key"`
	Note      string `json:"note"`

	KeyStrategy string `json:"key_strategy"` // "sequential" | "round_robin"
	Disabled    bool   `json:"disabled"`

	// Key级冷却（从key_cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`
	CooldownDurationMs int64 `json:"cooldown_duration_ms"`

	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

// IsCoolingDown 检查密钥是否处于冷却状态
func (k *APIKey) IsCoolingDown(now time.Time) bool {
	return k.CooldownUntil > now.Unix()
}

// ChannelWithKeys 渠道和API Keys的完整数据
// 用于批量导入导出等需要完整渠道数据的场景
type ChannelWithKeys struct {
	Config  *Config  `json:"config"`
	APIKeys []APIKey `json:"api_keys"` // 不使用指针避免额外分配
}

// FuzzyMatchModel 模糊匹配模型名称
// 当精确匹配失败时，查找包含 query 子串的模型，按版本排序返回最新的
// 返回 (匹配到的模型名, 是否匹配成功)
func (c *Config) FuzzyMatchModel(query string) (string, bool) {
	if query == "" {
		return "", false
	}

	queryLower := strings.ToLower(query)
	var matches []string

	for _, entry := range c.ModelEntries {
		if entry.Disabled {
			continue
		}
		if strings.Contains(strings.ToLower(entry.Model), queryLower) {
			matches = append(matches, entry.Model)
		}
	}

	if len(matches) == 0 {
		return "", false
	}
	if len(matches) == 1 {
		return matches[0], true
	}

	// 多个匹配：按版本排序，取最新
	sortModelsByVersion(matches)
	return matches[0], true
}

// sortModelsByVersion 按版本排序模型列表（最新优先）
// 排序优先级：1.日期后缀 2.版本数字 3.字典序
// 使用标准库 slices.SortFunc，O(n log n) 复杂度
func sortModelsByVersion(models []string) {
	slices.SortFunc(models, func(a, b string) int {
		return -compareModelVersion(a, b) // 降序（最新优先）
	})
}

// compareModelVersion 比较两个模型版本
// 返回 >0 表示 a 更新，<0 表示 b 更新，0 表示相同
func compareModelVersion(a, b string) int {
	// 1. 日期后缀优先（YYYYMMDD）
	dateA := extractDateSuffix(a)
	dateB := extractDateSuffix(b)
	if dateA != dateB {
		if dateA > dateB {
			return 1
		}
		return -1
	}

	// 2. 版本数字序列比较
	verA := extractVersionNumbers(a)
	verB := extractVersionNumbers(b)
	maxLen := len(verA)
	if len(verB) > maxLen {
		maxLen = len(verB)
	}
	for i := 0; i < maxLen; i++ {
		va, vb := 0, 0
		if i < len(verA) {
			va = verA[i]
		}
		if i < len(verB) {
			vb = verB[i]
		}
		if va != vb {
			return va - vb
		}
	}

	// 3. 兜底：字典序
	if a > b {
		return 1
	} else if a < b {
		return -1
	}
	return 0
}

// extractDateSuffix 提取模型名称末尾的日期后缀（YYYYMMDD）
// 返回日期字符串，无日期返回空串
func extractDateSuffix(model string) string {
	// 查找最后一个分隔符
	lastDash := strings.LastIndexByte(model, '-')
	lastDot := strings.LastIndexByte(model, '.')
	lastSep := lastDash
	if lastDot > lastSep {
		lastSep = lastDot
	}
	if lastSep < 0 {
		return ""
	}

	suffix := model[lastSep+1:]
	if len(suffix) != 8 {
		return ""
	}

	// 验证是否全数字
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return ""
		}
	}

	// 简单验证年份范围
	year := (int(suffix[0]-'0') * 1000) + (int(suffix[1]-'0') * 100) +
		(int(suffix[2]-'0') * 10) + int(suffix[3]-'0')
	if year < 2000 || year > 2100 {
		return ""
	}

	return suffix
}

// extractVersionNumbers 提取模型名称中的版本数字
// 例如：gpt-5.2 → [5,2], claude-sonnet-4-5-20250929 → [4,5]
func extractVersionNumbers(model string) []int {
	// 移除日期后缀避免干扰
	if date := extractDateSuffix(model); date != "" {
		model = model[:len(model)-len(date)-1]
	}

	var nums []int
	var current int
	inNumber := false

	for i := 0; i < len(model); i++ {
		c := model[i]
		if c >= '0' && c <= '9' {
			current = current*10 + int(c-'0')
			inNumber = true
		} else {
			if inNumber {
				nums = append(nums, current)
				current = 0
				inNumber = false
			}
		}
	}
	if inNumber {
		nums = append(nums, current)
	}

	return nums
}

// HeaderRules 返回自定义请求头规则，nil-safe
func (c *Config) HeaderRules() []CustomHeaderRule {
	if c == nil || c.CustomRequestRules == nil {
		return nil
	}
	return c.CustomRequestRules.Headers
}

// BodyRules 返回自定义请求体规则，nil-safe
func (c *Config) BodyRules() []CustomBodyRule {
	if c == nil || c.CustomRequestRules == nil {
		return nil
	}
	return c.CustomRequestRules.Body
}
