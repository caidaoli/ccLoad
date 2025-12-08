package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migrate 创建MySQL数据库表结构（兼容MySQL 5.6）
func (s *MySQLStore) migrate(ctx context.Context) error {
	// 创建 channels 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channels (
			id INT PRIMARY KEY AUTO_INCREMENT,
			name VARCHAR(191) NOT NULL UNIQUE,
			url VARCHAR(191) NOT NULL,
			priority INT NOT NULL DEFAULT 0,
			models TEXT NOT NULL,
			model_redirects TEXT NOT NULL,
			channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic',
			enabled TINYINT NOT NULL DEFAULT 1,
			cooldown_until BIGINT NOT NULL DEFAULT 0,
			cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
			rr_key_index INT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		) ;
	`); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 api_keys 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS api_keys (
			id INT PRIMARY KEY AUTO_INCREMENT,
			channel_id INT NOT NULL,
			key_index INT NOT NULL,
			api_key VARCHAR(100) NOT NULL,
			key_strategy VARCHAR(32) NOT NULL DEFAULT 'sequential',
			cooldown_until BIGINT NOT NULL DEFAULT 0,
			cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			UNIQUE KEY uk_channel_key (channel_id, key_index),
			FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE
		) ;
	`); err != nil {
		return fmt.Errorf("create api_keys table: %w", err)
	}

	// 创建 channel_models 索引表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channel_models (
			channel_id INT NOT NULL,
			model VARCHAR(191) NOT NULL,
			created_at BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (channel_id, model),
			FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE
		) ;
	`); err != nil {
		return fmt.Errorf("create channel_models table: %w", err)
	}

	// 创建 channel_models 索引（MySQL 5.6不支持IF NOT EXISTS，忽略错误）
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_channel_models_model ON channel_models(model)")

	// 创建 auth_tokens 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS auth_tokens (
			id INT PRIMARY KEY AUTO_INCREMENT,
			token VARCHAR(100) NOT NULL UNIQUE,
			description VARCHAR(512) NOT NULL,
			created_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL DEFAULT 0,
			last_used_at BIGINT NOT NULL DEFAULT 0,
			is_active TINYINT NOT NULL DEFAULT 1,
			success_count INT NOT NULL DEFAULT 0,
			failure_count INT NOT NULL DEFAULT 0,
			stream_avg_ttfb DOUBLE NOT NULL DEFAULT 0.0,
			non_stream_avg_rt DOUBLE NOT NULL DEFAULT 0.0,
			stream_count INT NOT NULL DEFAULT 0,
			non_stream_count INT NOT NULL DEFAULT 0,
			prompt_tokens_total BIGINT NOT NULL DEFAULT 0,
			completion_tokens_total BIGINT NOT NULL DEFAULT 0,
			total_cost_usd DOUBLE NOT NULL DEFAULT 0.0
		) ;
	`); err != nil {
		return fmt.Errorf("create auth_tokens table: %w", err)
	}

	// 创建渠道表索引
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_channels_enabled ON channels(enabled)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_channels_priority ON channels(priority DESC)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_channels_type_enabled ON channels(channel_type, enabled)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_channels_cooldown ON channels(cooldown_until)")
	// 删除：name字段UNIQUE约束已创建索引，重复索引是浪费

	// API Keys 表索引
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_api_keys_cooldown ON api_keys(cooldown_until)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_api_keys_channel_cooldown ON api_keys(channel_id, cooldown_until)")

	// Auth Tokens 表索引
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_auth_tokens_active ON auth_tokens(is_active)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_auth_tokens_expires ON auth_tokens(expires_at)")

	// 创建系统配置表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS system_settings (
			`+"`key`"+` VARCHAR(128) PRIMARY KEY,
			value TEXT NOT NULL,
			value_type VARCHAR(32) NOT NULL,
			description VARCHAR(512) NOT NULL,
			default_value VARCHAR(512) NOT NULL,
			updated_at BIGINT NOT NULL
		) ;
	`); err != nil {
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
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
			INSERT IGNORE INTO system_settings (`+"`key`"+`, value, value_type, description, default_value, updated_at)
			VALUES (?, ?, ?, ?, ?, %s)
		`, nowUnix), setting.key, setting.value, setting.valueType, setting.desc, setting.defaultVal); err != nil {
			return fmt.Errorf("insert default setting %s: %w", setting.key, err)
		}
	}

	// 创建管理员会话表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS admin_sessions (
			token VARCHAR(64) PRIMARY KEY,
			expires_at BIGINT NOT NULL,
			created_at BIGINT NOT NULL
		) ;
	`); err != nil {
		return fmt.Errorf("create admin_sessions table: %w", err)
	}
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_admin_sessions_expires ON admin_sessions(expires_at)")

	// 创建 logs 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id INT PRIMARY KEY AUTO_INCREMENT,
			time BIGINT NOT NULL,
			model VARCHAR(191) NOT NULL DEFAULT '',
			channel_id INT NOT NULL DEFAULT 0,
			status_code INT NOT NULL,
			message TEXT NOT NULL,
			duration DOUBLE NOT NULL DEFAULT 0.0,
			is_streaming TINYINT NOT NULL DEFAULT 0,
			first_byte_time DOUBLE NOT NULL DEFAULT 0.0,
			api_key_used VARCHAR(191) NOT NULL DEFAULT '',
			input_tokens INT NOT NULL DEFAULT 0,
			output_tokens INT NOT NULL DEFAULT 0,
			cache_read_input_tokens INT NOT NULL DEFAULT 0,
			cache_creation_input_tokens INT NOT NULL DEFAULT 0,
			cost DOUBLE NOT NULL DEFAULT 0.0
		) ;
	`); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 日志表索引
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_logs_time_model ON logs(time, model)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_logs_time_channel ON logs(time, channel_id)")
	s.createIndexIgnoreError(ctx, "CREATE INDEX idx_logs_time_status ON logs(time, status_code)")

	return nil
}

// createIndexIgnoreError 创建索引，忽略已存在错误（MySQL 5.6不支持IF NOT EXISTS）
func (s *MySQLStore) createIndexIgnoreError(ctx context.Context, sql string) {
	if _, err := s.db.ExecContext(ctx, sql); err != nil {
		// 忽略索引已存在错误 (Error 1061: Duplicate key name)
		if !strings.Contains(err.Error(), "Duplicate key name") {
			// 记录其他错误但不中断
			fmt.Printf("Warning: create index failed: %v\n", err)
		}
	}
}

// Migrate 执行MySQL数据库迁移（独立函数，供factory调用）
func Migrate(ctx context.Context, db any) error {
	// 临时包装为store结构以复用现有逻辑
	s := &MySQLStore{db: db.(*sql.DB)}
	return s.migrate(ctx)
}
