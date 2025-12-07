package mysql

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

func NewWhereBuilder() *WhereBuilder {
	return &WhereBuilder{
		conditions: make([]string, 0),
		args:       make([]any, 0),
	}
}

func (wb *WhereBuilder) AddCondition(condition string, args ...any) *WhereBuilder {
	if condition == "" {
		return wb
	}

	if len(args) > 0 && !strings.Contains(condition, "?") {
		panic(fmt.Sprintf("安全错误: SQL条件必须使用占位符 '?'，禁止直接拼接参数。条件: %s", condition))
	}

	conditionLower := strings.ToLower(condition)
	dangerousPatterns := []string{
		"; drop ", "; delete ", "; update ", "; insert ",
		"-- ", "/*", "*/", "union ", " or 1=1", " or '1'='1",
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

func (wb *WhereBuilder) ApplyLogFilter(filter *model.LogFilter) *WhereBuilder {
	if filter == nil {
		return wb
	}

	if filter.ChannelID != nil {
		wb.AddCondition("channel_id = ?", *filter.ChannelID)
	}
	if filter.Model != "" {
		wb.AddCondition("model = ?", filter.Model)
	}
	if filter.ModelLike != "" {
		wb.AddCondition("model LIKE ?", "%"+filter.ModelLike+"%")
	}
	if filter.StatusCode != nil {
		wb.AddCondition("status_code = ?", *filter.StatusCode)
	}
	return wb
}

func (wb *WhereBuilder) Build() (string, []any) {
	if len(wb.conditions) == 0 {
		return "", wb.args
	}
	return strings.Join(wb.conditions, " AND "), wb.args
}

func (wb *WhereBuilder) BuildWithPrefix(prefix string) (string, []any) {
	whereClause, args := wb.Build()
	if whereClause == "" {
		return "", args
	}
	return prefix + " " + whereClause, args
}

// ConfigScanner 统一的 Config 扫描器
type ConfigScanner struct{}

func NewConfigScanner() *ConfigScanner {
	return &ConfigScanner{}
}

func (cs *ConfigScanner) ScanConfig(scanner interface {
	Scan(...any) error
}) (*model.Config, error) {
	var c model.Config
	var modelsStr, modelRedirectsStr string
	var enabledInt int
	var createdAtRaw, updatedAtRaw any
	var rrKeyIndex int

	if err := scanner.Scan(&c.ID, &c.Name, &c.URL, &c.Priority,
		&modelsStr, &modelRedirectsStr, &c.ChannelType, &enabledInt,
		&c.CooldownUntil, &c.CooldownDurationMs, &c.KeyCount,
		&rrKeyIndex, &createdAtRaw, &updatedAtRaw); err != nil {
		return nil, err
	}

	c.Enabled = enabledInt != 0

	now := time.Now()
	c.CreatedAt = model.JSONTime{Time: parseTimestampOrNow(createdAtRaw, now)}
	c.UpdatedAt = model.JSONTime{Time: parseTimestampOrNow(updatedAtRaw, now)}

	if err := parseModelsJSON(modelsStr, &c.Models); err != nil {
		c.Models = nil
	}
	if err := parseModelRedirectsJSON(modelRedirectsStr, &c.ModelRedirects); err != nil {
		c.ModelRedirects = nil
	}
	return &c, nil
}

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
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil && ts > 0 {
			return time.Unix(ts, 0)
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return fallback
}

// QueryBuilder 通用查询构建器
type QueryBuilder struct {
	baseQuery string
	wb        *WhereBuilder
}

func NewQueryBuilder(baseQuery string) *QueryBuilder {
	return &QueryBuilder{
		baseQuery: baseQuery,
		wb:        NewWhereBuilder(),
	}
}

func (qb *QueryBuilder) Where(condition string, args ...any) *QueryBuilder {
	qb.wb.AddCondition(condition, args...)
	return qb
}

func (qb *QueryBuilder) ApplyFilter(filter *model.LogFilter) *QueryBuilder {
	qb.wb.ApplyLogFilter(filter)
	return qb
}

func (qb *QueryBuilder) WhereIn(column string, values []any) *QueryBuilder {
	if err := ValidateFieldName(column); err != nil {
		panic(fmt.Sprintf("SQL注入防护: %v", err))
	}

	if len(values) == 0 {
		qb.wb.AddCondition("1=0")
		return qb
	}
	placeholders := make([]string, len(values))
	for i := range values {
		placeholders[i] = "?"
	}
	cond := fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ","))
	qb.wb.AddCondition(cond, values...)
	return qb
}

func (qb *QueryBuilder) Build() (string, []any) {
	whereClause, args := qb.wb.BuildWithPrefix("WHERE")

	query := qb.baseQuery
	if whereClause != "" {
		query += " " + whereClause
	}

	return query, args
}

func (qb *QueryBuilder) BuildWithSuffix(suffix string) (string, []any) {
	query, args := qb.Build()
	if suffix != "" {
		query += " " + suffix
	}
	return query, args
}

func parseModelsJSON(modelsStr string, models *[]string) error {
	if modelsStr == "" {
		*models = []string{}
		return nil
	}
	return sonic.Unmarshal([]byte(modelsStr), models)
}

func parseModelRedirectsJSON(redirectsStr string, redirects *map[string]string) error {
	if redirectsStr == "" || redirectsStr == "{}" {
		*redirects = make(map[string]string)
		return nil
	}
	return sonic.Unmarshal([]byte(redirectsStr), redirects)
}
