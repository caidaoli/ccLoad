package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"ccLoad/internal/storage/schema"
)

// Migrate 执行SQLite数据库迁移
func Migrate(ctx context.Context, db any) error {
	return migrateImpl(ctx, db.(*sql.DB))
}

func migrateImpl(ctx context.Context, db *sql.DB) error {
	// 创建 channels 表
	if _, err := db.ExecContext(ctx,
		schema.DefineChannelsTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 api_keys 表
	if _, err := db.ExecContext(ctx,
		schema.DefineAPIKeysTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create api_keys table: %w", err)
	}

	// 创建 channel_models 索引表
	if _, err := db.ExecContext(ctx,
		schema.DefineChannelModelsTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create channel_models table: %w", err)
	}

	// 创建 channel_models 索引
	for _, idx := range schema.DefineChannelModelsTable().GetIndexesSQLite() {
		if _, err := db.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create channel_models index: %w", err)
		}
	}

	// 创建 auth_tokens 表
	if _, err := db.ExecContext(ctx,
		schema.DefineAuthTokensTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create auth_tokens table: %w", err)
	}

	// 创建所有表的索引
	allIndexes := []string{}
	allIndexes = append(allIndexes, schema.DefineChannelsTable().GetIndexesSQLite()...)
	allIndexes = append(allIndexes, schema.DefineAPIKeysTable().GetIndexesSQLite()...)
	allIndexes = append(allIndexes, schema.DefineAuthTokensTable().GetIndexesSQLite()...)

	for _, idx := range allIndexes {
		if _, err := db.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}

	// 创建系统配置表
	if _, err := db.ExecContext(ctx,
		schema.DefineSystemSettingsTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create system_settings table: %w", err)
	}

	// 初始化默认配置
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
		if _, err := db.ExecContext(ctx, `
			INSERT OR IGNORE INTO system_settings (key, value, value_type, description, default_value, updated_at)
			VALUES (?, ?, ?, ?, ?, unixepoch())
		`, setting.key, setting.value, setting.valueType, setting.desc, setting.defaultVal); err != nil {
			return fmt.Errorf("insert default setting %s: %w", setting.key, err)
		}
	}

	// 创建管理员会话表
	if _, err := db.ExecContext(ctx,
		schema.DefineAdminSessionsTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create admin_sessions table: %w", err)
	}

	// 创建会话表索引
	for _, idx := range schema.DefineAdminSessionsTable().GetIndexesSQLite() {
		if _, err := db.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create admin_sessions index: %w", err)
		}
	}

	// 创建 logs 表
	if _, err := db.ExecContext(ctx,
		schema.DefineLogsTable().BuildSQLite(),
	); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 创建日志表索引
	for _, idx := range schema.DefineLogsTable().GetIndexesSQLite() {
		if _, err := db.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create logs index: %w", err)
		}
	}

	return nil
}
