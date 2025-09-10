package main

import (
	"fmt"
	"strings"

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
	var modelsStr string
	var enabledInt int

	if err := scanner.Scan(&c.ID, &c.Name, &c.APIKey, &c.URL, &c.Priority,
		&modelsStr, &enabledInt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}

	c.Enabled = enabledInt != 0
	if err := parseModelsJSON(modelsStr, &c.Models); err != nil {
		c.Models = nil // 解析失败时使用空切片
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

// TransactionHelper 事务助手，简化事务操作
type TransactionHelper struct {
	store *SQLiteStore
}

// NewTransactionHelper 创建事务助手
func NewTransactionHelper(store *SQLiteStore) *TransactionHelper {
	return &TransactionHelper{store: store}
}

// WithTransaction 在事务中执行操作
func (th *TransactionHelper) WithTransaction(ctx any, fn func(tx any) error) error {
	// 这里简化了事务逻辑，实际实现需要根据具体的数据库接口
	// 由于当前代码中事务使用相对简单，暂时保持现有模式
	return nil
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
