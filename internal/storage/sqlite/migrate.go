package sqlite

import (
	"context"
	"fmt"
)

// migrate 创建数据库表结构
// 架构设计：
// - channels 表：渠道配置 + 内联冷却字段 + 轮询指针
// - api_keys 表：API Keys 独立存储 + 内联冷却字段
func (s *SQLiteStore) migrate(ctx context.Context) error {
	// 创建 channels 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			models TEXT NOT NULL,
			model_redirects TEXT DEFAULT '{}',
			channel_type TEXT DEFAULT 'anthropic',
			enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER DEFAULT 0,
			cooldown_duration_ms INTEGER DEFAULT 0,
			rr_key_index INTEGER DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 api_keys 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER NOT NULL,
			key_index INTEGER NOT NULL,
			api_key TEXT NOT NULL,
			key_strategy TEXT DEFAULT 'sequential',
			cooldown_until INTEGER DEFAULT 0,
			cooldown_duration_ms INTEGER DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			UNIQUE(channel_id, key_index),
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create api_keys table: %w", err)
	}

	// 创建性能索引
	if _, err := s.db.ExecContext(ctx, `
		-- 渠道表索引
		CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled);
		CREATE INDEX IF NOT EXISTS idx_channels_priority ON channels(priority DESC);
		CREATE INDEX IF NOT EXISTS idx_channels_type_enabled ON channels(channel_type, enabled);
		CREATE INDEX IF NOT EXISTS idx_channels_cooldown ON channels(cooldown_until);

		-- API Keys 表索引
		CREATE INDEX IF NOT EXISTS idx_api_keys_channel_id ON api_keys(channel_id);
		CREATE INDEX IF NOT EXISTS idx_api_keys_cooldown ON api_keys(cooldown_until);
		CREATE INDEX IF NOT EXISTS idx_api_keys_channel_cooldown ON api_keys(channel_id, cooldown_until);
	`); err != nil {
		return fmt.Errorf("create performance indexes: %w", err)
	}

	return nil
}

// migrateLogDB 创建日志数据库表结构
func (s *SQLiteStore) migrateLogDB(ctx context.Context) error {
	// 创建 logs 表
	if _, err := s.logDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time BIGINT NOT NULL,
			model TEXT,
			channel_id INTEGER,
			status_code INTEGER NOT NULL,
			message TEXT,
			duration REAL,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL,
			api_key_used TEXT
		);
	`); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 创建日志索引
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(time)",
		"CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status_code)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_model ON logs(time, model)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_channel ON logs(time, channel_id)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_status ON logs(time, status_code)",
	}

	for _, indexSQL := range indexes {
		if _, err := s.logDB.ExecContext(ctx, indexSQL); err != nil {
			return fmt.Errorf("create log index: %w", err)
		}
	}

	return nil
}
