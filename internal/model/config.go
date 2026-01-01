package model

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ModelEntry 模型配置条目
type ModelEntry struct {
	Model         string `json:"model"`                    // 模型名称
	RedirectModel string `json:"redirect_model,omitempty"` // 重定向目标模型（空表示不重定向）
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

// Config 渠道配置
type Config struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"` // 渠道类型: "anthropic" | "codex" | "openai" | "gemini"，默认anthropic
	URL         string `json:"url"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`

	// 模型配置（统一管理模型和重定向）
	ModelEntries []ModelEntry `json:"models"`

	// 渠道级冷却（从cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`       // Unix秒时间戳，0表示无冷却
	CooldownDurationMs int64 `json:"cooldown_duration_ms"` // 冷却持续时间（毫秒）

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 缓存Key数量，避免冷却判断时的N+1查询
	KeyCount int `json:"key_count"` // API Key数量（查询时JOIN计算）

	// 模型查找索引（懒加载，不序列化）
	modelIndex map[string]*ModelEntry `json:"-"`
	indexOnce  sync.Once              `json:"-"` // 保证线程安全的单次初始化
}

// GetModels 获取所有支持的模型名称列表
func (c *Config) GetModels() []string {
	models := make([]string, 0, len(c.ModelEntries))
	for _, e := range c.ModelEntries {
		models = append(models, e.Model)
	}
	return models
}

// buildIndexIfNeeded 懒加载构建模型查找索引（性能优化：O(n) → O(1)）
// 使用 sync.Once 保证并发安全，避免竞态条件
func (c *Config) buildIndexIfNeeded() {
	c.indexOnce.Do(func() {
		c.modelIndex = make(map[string]*ModelEntry, len(c.ModelEntries))
		for i := range c.ModelEntries {
			c.modelIndex[c.ModelEntries[i].Model] = &c.ModelEntries[i]
		}
	})
}

// ResetModelIndex 重置模型索引缓存
// 用于 deepCopy 或 ModelEntries 被外部修改后，确保下次访问时重新构建索引
// [FIX] P0: 收敛索引生命周期管理，避免 sync.Once 复制和索引指向旧数据
func (c *Config) ResetModelIndex() {
	c.modelIndex = nil
	c.indexOnce = sync.Once{}
}

// GetRedirectModel 获取模型的重定向目标
// 返回 (目标模型, 是否有重定向)
func (c *Config) GetRedirectModel(model string) (string, bool) {
	c.buildIndexIfNeeded()
	if entry, exists := c.modelIndex[model]; exists && entry.RedirectModel != "" {
		return entry.RedirectModel, true
	}
	return "", false
}

// SupportsModel 检查渠道是否支持指定模型
func (c *Config) SupportsModel(model string) bool {
	c.buildIndexIfNeeded()
	_, exists := c.modelIndex[model]
	return exists
}

// GetChannelType 默认返回"anthropic"（Claude API）
func (c *Config) GetChannelType() string {
	if c.ChannelType == "" {
		return "anthropic"
	}
	return c.ChannelType
}

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

type APIKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyIndex  int    `json:"key_index"`
	APIKey    string `json:"api_key"`

	KeyStrategy string `json:"key_strategy"` // "sequential" | "round_robin"

	// Key级冷却（从key_cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`
	CooldownDurationMs int64 `json:"cooldown_duration_ms"`

	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

func (k *APIKey) IsCoolingDown(now time.Time) bool {
	return k.CooldownUntil > now.Unix()
}

// ChannelWithKeys 用于Redis完整同步
// 设计目标：解决Redis恢复后渠道缺少API Keys的问题
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
func sortModelsByVersion(models []string) {
	for i := 0; i < len(models)-1; i++ {
		for j := i + 1; j < len(models); j++ {
			if compareModelVersion(models[i], models[j]) < 0 {
				models[i], models[j] = models[j], models[i]
			}
		}
	}
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
