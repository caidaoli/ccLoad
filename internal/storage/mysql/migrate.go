package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ccLoad/internal/storage/schema"
)

// Migrate 执行MySQL数据库迁移（兼容MySQL 5.6）
func Migrate(ctx context.Context, db any) error {
	sqlDB := db.(*sql.DB)

	// 创建 channels 表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineChannelsTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 api_keys 表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineAPIKeysTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create api_keys table: %w", err)
	}

	// 创建 channel_models 索引表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineChannelModelsTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create channel_models table: %w", err)
	}

	// 创建 channel_models 索引（MySQL 5.6不支持IF NOT EXISTS，忽略错误）
	for _, idx := range schema.DefineChannelModelsTable().GetIndexesMySQL() {
		createIndexIgnoreError(ctx, sqlDB, idx)
	}

	// 创建 auth_tokens 表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineAuthTokensTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create auth_tokens table: %w", err)
	}

	// 创建所有表的索引（MySQL 5.6不支持IF NOT EXISTS，忽略错误）
	allIndexes := []string{}
	allIndexes = append(allIndexes, schema.DefineChannelsTable().GetIndexesMySQL()...)
	allIndexes = append(allIndexes, schema.DefineAPIKeysTable().GetIndexesMySQL()...)
	allIndexes = append(allIndexes, schema.DefineAuthTokensTable().GetIndexesMySQL()...)

	for _, idx := range allIndexes {
		createIndexIgnoreError(ctx, sqlDB, idx)
	}

	// 创建系统配置表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineSystemSettingsTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create system_settings table: %w", err)
	}

	// 初始化默认配置（使用INSERT IGNORE代替ON CONFLICT）
	nowUnix := "UNIX_TIMESTAMP()"
	defaultSettings := []struct {
		key, value, valueType, desc, defaultVal string
	}{
		{"log_retention_days", "7", "int", "日志保留天数(-1永久保留,1-365天)", "7"},
		{"max_key_retries", "3", "int", "单渠道最大Key重试次数", "3"},
		{"upstream_first_byte_timeout", "0", "duration", "上游首字节超时(秒,0=禁用)", "0"},
		{"88code_free_only", "false", "bool", "仅允许使用88code免费订阅", "false"},
		{"skip_tls_verify", "false", "bool", "跳过TLS证书验证", "false"},
		{"channel_test_content", "sonnet 4.0的发布日期是什么", "string", "渠道测试默认内容", "sonnet 4.0的发布日期是什么"},
		{"channel_stats_range", "today", "string", "渠道管理费用统计范围", "today"},
	}

	for _, setting := range defaultSettings {
		if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf(`
			INSERT IGNORE INTO system_settings (`+"`key`"+`, value, value_type, description, default_value, updated_at)
			VALUES (?, ?, ?, ?, ?, %s)
		`, nowUnix), setting.key, setting.value, setting.valueType, setting.desc, setting.defaultVal); err != nil {
			return fmt.Errorf("insert default setting %s: %w", setting.key, err)
		}
	}

	// 创建管理员会话表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineAdminSessionsTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create admin_sessions table: %w", err)
	}

	// 创建会话表索引
	for _, idx := range schema.DefineAdminSessionsTable().GetIndexesMySQL() {
		createIndexIgnoreError(ctx, sqlDB, idx)
	}

	// 创建 logs 表
	if _, err := sqlDB.ExecContext(ctx,
		schema.DefineLogsTable().BuildMySQL(),
	); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 创建日志表索引
	for _, idx := range schema.DefineLogsTable().GetIndexesMySQL() {
		createIndexIgnoreError(ctx, sqlDB, idx)
	}

	return nil
}

// createIndexIgnoreError 创建索引，忽略已存在错误（MySQL 5.6不支持IF NOT EXISTS）
func createIndexIgnoreError(ctx context.Context, db *sql.DB, sql string) {
	if _, err := db.ExecContext(ctx, sql); err != nil {
		// 忽略索引已存在错误 (Error 1061: Duplicate key name)
		if !strings.Contains(err.Error(), "Duplicate key name") {
			// 记录其他错误但不中断
			fmt.Printf("Warning: create index failed: %v\n", err)
		}
	}
}
