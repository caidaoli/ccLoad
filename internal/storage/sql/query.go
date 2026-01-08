package sql

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
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

// AddCondition 添加SQL WHERE条件子句
//
// 【SQL注入防护约束】
//   - condition参数必须是代码中的字符串字面量或常量，禁止拼接用户输入
//   - 用户输入必须通过args参数传递，自动参数化为占位符(?)
//   - 违反约束将导致SQL注入漏洞，必须通过代码审查/静态分析工具检测
//
// 正确示例:
//
//	wb.AddCondition("channel_id = ?", userInputChannelID)  // ✅ 用户输入通过args传递
//	wb.AddCondition("status IN (?, ?)", "active", "pending") // ✅ 多个占位符
//
// 错误示例:
//
//	wb.AddCondition("channel_id = " + userInput)  // ❌ SQL注入风险！
//	wb.AddCondition(fmt.Sprintf("name LIKE '%%%s%%'", userInput))  // ❌ SQL注入风险！
//
// 静态检查建议: 使用gosec/semgrep扫描所有调用点，确保condition参数不包含fmt.Sprintf/字符串拼接
func (wb *WhereBuilder) AddCondition(condition string, args ...any) *WhereBuilder {
	if condition == "" {
		return wb
	}

	wb.conditions = append(wb.conditions, condition)
	wb.args = append(wb.args, args...)
	return wb
}

// ApplyLogFilter 应用日志过滤器，消除重复的过滤逻辑
func (wb *WhereBuilder) ApplyLogFilter(filter *model.LogFilter) *WhereBuilder {
	if filter == nil {
		return wb
	}

	if filter.ChannelID != nil {
		wb.AddCondition("channel_id = ?", *filter.ChannelID)
	}
	// 注意：ChannelType/ChannelName/ChannelNameLike 不在此处处理。
	// logs 表只有 channel_id；这类过滤应由 SQLStore.applyChannelFilter 先解析出候选 channel_id 集合再 WhereIn。
	if filter.Model != "" {
		wb.AddCondition("model = ?", filter.Model)
	}
	if filter.ModelLike != "" {
		wb.AddCondition("model LIKE ?", "%"+filter.ModelLike+"%")
	}
	if filter.StatusCode != nil {
		wb.AddCondition("status_code = ?", *filter.StatusCode)
	}
	if filter.AuthTokenID != nil {
		wb.AddCondition("auth_token_id = ?", *filter.AuthTokenID)
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

// ScanConfig 扫描单行配置数据（不含模型数据，需要单独查询channel_models表）
func (cs *ConfigScanner) ScanConfig(scanner interface {
	Scan(...any) error
}) (*model.Config, error) {
	var c model.Config
	var enabledInt int
	var createdAtRaw, updatedAtRaw any // 使用any接受任意类型（兼容字符串、整数或RFC3339）

	// 扫描key_count字段（从JOIN查询获取）
	// 注意：不再包含 models 和 model_redirects 字段
	if err := scanner.Scan(&c.ID, &c.Name, &c.URL, &c.Priority,
		&c.ChannelType, &enabledInt,
		&c.CooldownUntil, &c.CooldownDurationMs, &c.DailyCostLimit, &c.KeyCount,
		&createdAtRaw, &updatedAtRaw); err != nil {
		return nil, err
	}

	c.Enabled = enabledInt != 0

	// 转换时间戳（支持不同数据库）
	now := time.Now()
	c.CreatedAt = model.JSONTime{Time: cs.parseTimestampOrNow(createdAtRaw, now)}
	c.UpdatedAt = model.JSONTime{Time: cs.parseTimestampOrNow(updatedAtRaw, now)}

	// ModelEntries 需要通过 LoadModelEntries 方法单独加载
	c.ModelEntries = nil

	return &c, nil
}

// ScanConfigs 扫描多行配置数据
func (cs *ConfigScanner) ScanConfigs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*model.Config, error) {
	var configs []*model.Config

	for rows.Next() {
		config, err := cs.ScanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return configs, nil
}

// parseTimestampOrNow 解析时间戳或使用当前时间（支持Unix时间戳和RFC3339格式）
// 优先级：int64 > int > string(数字) > string(RFC3339) > fallback
func (cs *ConfigScanner) parseTimestampOrNow(val any, fallback time.Time) time.Time {
	switch v := val.(type) {
	case int64:
		if v > 0 {
			return unixToTime(v)
		}
	case int:
		if v > 0 {
			return unixToTime(int64(v))
		}
	case string:
		// 1. 尝试解析字符串为Unix时间戳
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil && ts > 0 {
			return unixToTime(ts)
		}
		// 2. 尝试解析RFC3339格式（Redis恢复场景）
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		// 3. 尝试解析常见ISO8601变体（兼容数据库TIMESTAMP格式）
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999999Z07:00",
			"2006-01-02 15:04:05.999999999 -07:00 MST",
		} {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
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
func (qb *QueryBuilder) ApplyFilter(filter *model.LogFilter) *QueryBuilder {
	qb.wb.ApplyLogFilter(filter)
	return qb
}

// WhereIn 添加 IN 条件，自动生成占位符
func (qb *QueryBuilder) WhereIn(column string, values []any) *QueryBuilder {
	if len(values) == 0 {
		// 无值时添加恒为假的条件，确保不返回记录
		qb.wb.AddCondition("1=0")
		return qb
	}
	// 生成 ?,?,? 占位符
	placeholders := make([]string, len(values))
	for i := range values {
		placeholders[i] = "?"
	}
	cond := fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ","))
	qb.wb.AddCondition(cond, values...)
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
