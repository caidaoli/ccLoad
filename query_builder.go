package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// WhereBuilder SQL WHERE 子句构建器
type WhereBuilder struct {
	conditions []string
	args       []any
}

// NewWhereBuilder 创建新的 WHERE 构建器
func NewWhereBuilder() *WhereBuilder {
	return &WhereBuilder{
		conditions: make([]string, 0),
		args:       make([]any, 0),
	}
}

// AddCondition 添加条件
func (wb *WhereBuilder) AddCondition(condition string, args ...any) *WhereBuilder {
	if condition != "" {
		wb.conditions = append(wb.conditions, condition)
		wb.args = append(wb.args, args...)
	}
	return wb
}

// AddTimeRange 添加时间范围条件
func (wb *WhereBuilder) AddTimeRange(timeField string, since any) *WhereBuilder {
	return wb.AddCondition(fmt.Sprintf("%s >= ?", timeField), since)
}

// ApplyLogFilter 应用日志过滤器，消除重复的过滤逻辑
func (wb *WhereBuilder) ApplyLogFilter(filter *LogFilter) *WhereBuilder {
	if filter == nil {
		return wb
	}

	if filter.ChannelID != nil {
		wb.AddCondition("l.channel_id = ?", *filter.ChannelID)
	}
	if filter.ChannelName != "" {
		wb.AddCondition("c.name = ?", filter.ChannelName)
	}
	if filter.ChannelNameLike != "" {
		wb.AddCondition("c.name LIKE ?", "%"+filter.ChannelNameLike+"%")
	}
	if filter.Model != "" {
		wb.AddCondition("l.model = ?", filter.Model)
	}
	if filter.ModelLike != "" {
		wb.AddCondition("l.model LIKE ?", "%"+filter.ModelLike+"%")
	}
	return wb
}

// Build 构建最终的 WHERE 子句和参数
func (wb *WhereBuilder) Build() (string, []any) {
	if len(wb.conditions) == 0 {
		return "", wb.args
	}
	return strings.Join(wb.conditions, " AND "), wb.args
}

// BuildWithPrefix 构建带前缀的 WHERE 子句
func (wb *WhereBuilder) BuildWithPrefix(prefix string) (string, []any) {
	whereClause, args := wb.Build()
	if whereClause == "" {
		return "", args
	}
	return prefix + " " + whereClause, args
}

// ConfigScanner 统一的 Config 扫描器
type ConfigScanner struct{}

// NewConfigScanner 创建新的配置扫描器
func NewConfigScanner() *ConfigScanner {
	return &ConfigScanner{}
}

// ScanConfig 扫描单行配置数据，消除重复的扫描逻辑
func (cs *ConfigScanner) ScanConfig(scanner interface {
	Scan(...any) error
}) (*Config, error) {
	var c Config
	var modelsStr, modelRedirectsStr, apiKeysStr string
	var enabledInt int
	var createdAtRaw, updatedAtRaw any // 使用any接受任意类型（ultrathink：兼容字符串或整数）

	if err := scanner.Scan(&c.ID, &c.Name, &c.APIKey, &apiKeysStr, &c.KeyStrategy, &c.URL, &c.Priority,
		&modelsStr, &modelRedirectsStr, &c.ChannelType, &enabledInt, &createdAtRaw, &updatedAtRaw); err != nil {
		return nil, err
	}

	c.Enabled = enabledInt != 0

	// 转换时间戳为time.Time（ultrathink：简单容错，非unixtime直接用当前时间）
	now := time.Now()
	c.CreatedAt = parseTimestampOrNow(createdAtRaw, now)
	c.UpdatedAt = parseTimestampOrNow(updatedAtRaw, now)

	if err := parseModelsJSON(modelsStr, &c.Models); err != nil {
		c.Models = nil // 解析失败时使用空切片
	}
	if err := parseModelRedirectsJSON(modelRedirectsStr, &c.ModelRedirects); err != nil {
		c.ModelRedirects = nil // 解析失败时使用空映射
	}
	// 解析多Key数组（如果存在）
	if apiKeysStr != "" && apiKeysStr != "[]" {
		if err := sonic.Unmarshal([]byte(apiKeysStr), &c.APIKeys); err != nil {
			c.APIKeys = nil
		}
	}
	return &c, nil
}

// ScanConfigs 扫描多行配置数据
func (cs *ConfigScanner) ScanConfigs(rows interface {
	Next() bool
	Scan(...any) error
}) ([]*Config, error) {
	var configs []*Config

	for rows.Next() {
		config, err := cs.ScanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}

	return configs, nil
}

// parseTimestampOrNow 解析时间戳或使用当前时间（ultrathink：简单容错）
// 如果val是有效的Unix时间戳（int64 > 0），转换为time.Time
// 否则使用fallback（通常是当前时间）
func parseTimestampOrNow(val any, fallback time.Time) time.Time {
	switch v := val.(type) {
	case int64:
		if v > 0 {
			return time.Unix(v, 0)
		}
	case int:
		if v > 0 {
			return time.Unix(int64(v), 0)
		}
	case string:
		// 尝试解析字符串为整数
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil && ts > 0 {
			return time.Unix(ts, 0)
		}
	}
	// 非法值：返回fallback
	return fallback
}

// QueryBuilder 通用查询构建器
type QueryBuilder struct {
	baseQuery string
	wb        *WhereBuilder
}

// NewQueryBuilder 创建新的查询构建器
func NewQueryBuilder(baseQuery string) *QueryBuilder {
	return &QueryBuilder{
		baseQuery: baseQuery,
		wb:        NewWhereBuilder(),
	}
}

// Where 添加 WHERE 条件
func (qb *QueryBuilder) Where(condition string, args ...any) *QueryBuilder {
	qb.wb.AddCondition(condition, args...)
	return qb
}

// ApplyFilter 应用过滤器
func (qb *QueryBuilder) ApplyFilter(filter *LogFilter) *QueryBuilder {
	qb.wb.ApplyLogFilter(filter)
	return qb
}

// Build 构建最终查询
func (qb *QueryBuilder) Build() (string, []any) {
	whereClause, args := qb.wb.BuildWithPrefix("WHERE")

	query := qb.baseQuery
	if whereClause != "" {
		query += " " + whereClause
	}

	return query, args
}

// BuildWithSuffix 构建带后缀的查询（如 ORDER BY, LIMIT 等）
func (qb *QueryBuilder) BuildWithSuffix(suffix string) (string, []any) {
	query, args := qb.Build()
	if suffix != "" {
		query += " " + suffix
	}
	return query, args
}

// 辅助函数：解析模型JSON
func parseModelsJSON(modelsStr string, models *[]string) error {
	if modelsStr == "" {
		*models = []string{}
		return nil
	}

	// 使用现有的sonic库进行解析
	return sonic.Unmarshal([]byte(modelsStr), models)
}

// 辅助函数：序列化模型为JSON
func serializeModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}

	bytes, err := sonic.Marshal(models)
	if err != nil {
		return "[]", err
	}
	return string(bytes), nil
}

// 辅助函数：解析模型重定向JSON
func parseModelRedirectsJSON(redirectsStr string, redirects *map[string]string) error {
	if redirectsStr == "" || redirectsStr == "{}" {
		*redirects = make(map[string]string)
		return nil
	}

	return sonic.Unmarshal([]byte(redirectsStr), redirects)
}

// 辅助函数：序列化模型重定向为JSON
func serializeModelRedirects(redirects map[string]string) (string, error) {
	if len(redirects) == 0 {
		return "{}", nil
	}

	bytes, err := sonic.Marshal(redirects)
	if err != nil {
		return "{}", err
	}
	return string(bytes), nil
}
