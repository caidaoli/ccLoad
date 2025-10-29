package sqlite

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"

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

// AddCondition 添加条件
// P0修复 (2025-10-05): 强制参数化查询，防止SQL注入
func (wb *WhereBuilder) AddCondition(condition string, args ...any) *WhereBuilder {
	if condition == "" {
		return wb
	}

	// SQL注入防护：如果提供了参数，条件中必须包含占位符
	if len(args) > 0 && !strings.Contains(condition, "?") {
		panic(fmt.Sprintf("安全错误: SQL条件必须使用占位符 '?'，禁止直接拼接参数。条件: %s", condition))
	}

	// SQL注入防护：检查条件字符串是否包含危险关键字（基础黑名单）
	conditionLower := strings.ToLower(condition)
	dangerousPatterns := []string{
		"; drop ",
		"; delete ",
		"; update ",
		"; insert ",
		"-- ",     // SQL注释
		"/*",      // 多行注释开始
		"*/",      // 多行注释结束
		"union ",  // UNION注入
		" or 1=1", // 经典注入
		" or '1'='1",
	}

	for _, pattern := range dangerousPatterns {
		if strings.Contains(conditionLower, pattern) {
			panic(fmt.Sprintf("安全错误: 检测到潜在SQL注入模式 '%s'。条件: %s", pattern, condition))
		}
	}

	wb.conditions = append(wb.conditions, condition)
	wb.args = append(wb.args, args...)
	return wb
}

// ApplyLogFilter 应用日志过滤器，消除重复的过滤逻辑
// 重构：移除表别名，直接使用列名（修复SQL错误）
func (wb *WhereBuilder) ApplyLogFilter(filter *model.LogFilter) *WhereBuilder {
	if filter == nil {
		return wb
	}

	if filter.ChannelID != nil {
		wb.AddCondition("channel_id = ?", *filter.ChannelID)
	}
	// 注意：ChannelName和ChannelNameLike需要JOIN channels表才能使用
	// 当前ListLogs查询不包含JOIN，因此这些过滤器会被忽略
	// 如需支持，需要修改sqlite_store.go的ListLogs查询添加LEFT JOIN
	if filter.Model != "" {
		wb.AddCondition("model = ?", filter.Model)
	}
	if filter.ModelLike != "" {
		wb.AddCondition("model LIKE ?", "%"+filter.ModelLike+"%")
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
}) (*model.Config, error) {
	var c model.Config
	var modelsStr, modelRedirectsStr string
	var enabledInt int
	var createdAtRaw, updatedAtRaw any // 使用any接受任意类型（兼容字符串、整数或RFC3339）

	// ✅ Linus风格：删除rr_key_index字段（已改用内存计数器）
	var rrKeyIndex int // 临时变量，读取后丢弃
	// 🔧 P1优化：扫描key_count字段（从JOIN查询获取）
	if err := scanner.Scan(&c.ID, &c.Name, &c.URL, &c.Priority,
		&modelsStr, &modelRedirectsStr, &c.ChannelType, &enabledInt,
		&c.CooldownUntil, &c.CooldownDurationMs, &c.KeyCount,
		&rrKeyIndex, &createdAtRaw, &updatedAtRaw); err != nil {
		return nil, err
	}

	c.Enabled = enabledInt != 0

	// 转换时间戳为JSONTime（支持Unix时间戳和RFC3339格式）
	now := time.Now()
	c.CreatedAt = model.JSONTime{Time: parseTimestampOrNow(createdAtRaw, now)}
	c.UpdatedAt = model.JSONTime{Time: parseTimestampOrNow(updatedAtRaw, now)}

	if err := parseModelsJSON(modelsStr, &c.Models); err != nil {
		c.Models = nil // 解析失败时使用空切片
	}
	if err := parseModelRedirectsJSON(modelRedirectsStr, &c.ModelRedirects); err != nil {
		c.ModelRedirects = nil // 解析失败时使用空映射
	}
	return &c, nil
}

// ScanConfigs 扫描多行配置数据
func (cs *ConfigScanner) ScanConfigs(rows interface {
	Next() bool
	Scan(...any) error
}) ([]*model.Config, error) {
	var configs []*model.Config

	for rows.Next() {
		config, err := cs.ScanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}

	return configs, nil
}

// parseTimestampOrNow 解析时间戳或使用当前时间（支持Unix时间戳和RFC3339格式）
// 优先级：int64 > int > string(数字) > string(RFC3339) > fallback
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
		// 1. 尝试解析字符串为Unix时间戳
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil && ts > 0 {
			return time.Unix(ts, 0)
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

// WhereIn 添加 IN 条件，自动生成占位符，防止SQL注入
// ✅ P1 修复 (2025-01-XX): 添加字段名白名单验证，防止SQL注入
func (qb *QueryBuilder) WhereIn(column string, values []any) *QueryBuilder {
	// P1 安全修复：验证字段名是否在白名单中
	if err := ValidateFieldName(column); err != nil {
		// 触发 panic 确保开发期间立即发现问题
		panic(fmt.Sprintf("SQL注入防护: %v", err))
	}

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

// 辅助函数：解析模型JSON
func parseModelsJSON(modelsStr string, models *[]string) error {
	if modelsStr == "" {
		*models = []string{}
		return nil
	}

	// 使用现有的sonic库进行解析
	return sonic.Unmarshal([]byte(modelsStr), models)
}

// 辅助函数：解析模型重定向JSON
func parseModelRedirectsJSON(redirectsStr string, redirects *map[string]string) error {
	if redirectsStr == "" || redirectsStr == "{}" {
		*redirects = make(map[string]string)
		return nil
	}

	return sonic.Unmarshal([]byte(redirectsStr), redirects)
}
