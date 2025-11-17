package sqlite

import (
	"context"
	"fmt"
	"strings"
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

	// 兼容性迁移：为现有数据库添加 rr_key_index 列
	if _, err := s.db.ExecContext(ctx, `
		ALTER TABLE channels ADD COLUMN rr_key_index INTEGER DEFAULT 0;
	`); err != nil {
		// 忽略列已存在的错误（SQLite error code 1: "duplicate column name"）
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add rr_key_index column: %w", err)
		}
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

	// 迁移数据：如果 key_rr 表存在，将轮询指针数据迁移到 channels 表
	if _, err := s.db.ExecContext(ctx, `
		UPDATE channels 
		SET rr_key_index = (
			SELECT COALESCE(idx, 0) 
			FROM key_rr 
			WHERE key_rr.channel_id = channels.id
		)
		WHERE EXISTS (SELECT 1 FROM key_rr WHERE key_rr.channel_id = channels.id);
	`); err != nil {
		// 忽略 key_rr 表不存在的错误
		if !strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("migrate rr_index data: %w", err)
		}
	}

	// 删除旧的 key_rr 表（如果存在）
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS key_rr;`); err != nil {
		return fmt.Errorf("drop legacy key_rr table: %w", err)
	}

	// 创建 channel_models 索引表（性能优化：消除JSON查询）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channel_models (
			channel_id INTEGER NOT NULL,
			model TEXT NOT NULL,
			created_at BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
			PRIMARY KEY (channel_id, model),
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create channel_models table: %w", err)
	}

	// 为 channel_models 创建高性能索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_channel_models_model ON channel_models(model);
		CREATE INDEX IF NOT EXISTS idx_channel_models_channel ON channel_models(channel_id);
		CREATE INDEX IF NOT EXISTS idx_channel_models_model_channel ON channel_models(model, channel_id);
	`); err != nil {
		return fmt.Errorf("create channel_models indexes: %w", err)
	}

	// 数据迁移：同步现有渠道的模型数据到 channel_models 表
	if _, err := s.db.ExecContext(ctx, `
		-- 清空现有的索引数据（避免重复）
		DELETE FROM channel_models;

		-- 从 channels 表的 JSON 数据同步到 channel_models 表
		INSERT INTO channel_models (channel_id, model)
		SELECT
			c.id,
			je.value as model
		FROM channels c
		-- 使用 json_each 解析现有的 JSON 数据（仅用于迁移）
		JOIN json_each(c.models) je
		WHERE c.enabled = 1
		  AND je.value IS NOT NULL
		  AND je.value != ''
		ON CONFLICT(channel_id, model) DO NOTHING;
	`); err != nil {
		// 如果 json_each 不支持（某些SQLite版本），则跳过迁移
		// 新增/更新的渠道会通过应用层逻辑同步
		fmt.Printf("Warning: Failed to migrate existing model data: %v\n", err)
	}

	// 创建 auth_tokens 表 (API访问令牌管理)
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER,
			last_used_at INTEGER,
			is_active INTEGER NOT NULL DEFAULT 1,
			CHECK (is_active IN (0, 1))
		);
	`); err != nil {
		return fmt.Errorf("create auth_tokens table: %w", err)
	}

	// 兼容性迁移：为现有数据库添加统计字段（2025-11新增）
	statsColumns := []struct {
		name       string
		definition string
	}{
		{"success_count", "INTEGER DEFAULT 0"},
		{"failure_count", "INTEGER DEFAULT 0"},
		{"stream_avg_ttfb", "REAL DEFAULT 0.0"},
		{"non_stream_avg_rt", "REAL DEFAULT 0.0"},
		{"stream_count", "INTEGER DEFAULT 0"},
		{"non_stream_count", "INTEGER DEFAULT 0"},
	}

	for _, col := range statsColumns {
		if _, err := s.db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE auth_tokens ADD COLUMN %s %s;", col.name, col.definition),
		); err != nil {
			// 忽略列已存在的错误（SQLite error: "duplicate column name"）
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("add column %s: %w", col.name, err)
			}
		}
	}

	// 创建性能索引
	if _, err := s.db.ExecContext(ctx, `
		-- 渠道表索引
		CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled);
		CREATE INDEX IF NOT EXISTS idx_channels_priority ON channels(priority DESC);
		CREATE INDEX IF NOT EXISTS idx_channels_type_enabled ON channels(channel_type, enabled);
		CREATE INDEX IF NOT EXISTS idx_channels_cooldown ON channels(cooldown_until);
		CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(name);

		-- API Keys 表索引
		CREATE INDEX IF NOT EXISTS idx_api_keys_channel_id ON api_keys(channel_id);
		CREATE INDEX IF NOT EXISTS idx_api_keys_cooldown ON api_keys(cooldown_until);
		CREATE INDEX IF NOT EXISTS idx_api_keys_channel_cooldown ON api_keys(channel_id, cooldown_until);

		-- Auth Tokens 表索引
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_active ON auth_tokens(is_active);
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_expires ON auth_tokens(expires_at);
		CREATE INDEX IF NOT EXISTS idx_auth_tokens_token ON auth_tokens(token);
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
			model TEXT DEFAULT '',
			channel_id INTEGER DEFAULT 0,
			status_code INTEGER NOT NULL,
			message TEXT DEFAULT '',
			duration REAL DEFAULT 0.0,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL DEFAULT 0.0,
			api_key_used TEXT DEFAULT ''
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
		"CREATE INDEX IF NOT EXISTS idx_logs_streaming_firstbyte ON logs(is_streaming, first_byte_time) WHERE is_streaming = 1 AND first_byte_time > 0",
	}

	for _, idx := range indexes {
		if _, err := s.logDB.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create log index: %w", err)
		}
	}

	// 兼容性迁移：为现有数据库添加 token 统计字段和成本字段（2025-11新增）
	tokenColumns := []struct {
		name       string
		definition string
	}{
		{"input_tokens", "INTEGER DEFAULT NULL"},
		{"output_tokens", "INTEGER DEFAULT NULL"},
		{"cache_read_input_tokens", "INTEGER DEFAULT NULL"},
		{"cache_creation_input_tokens", "INTEGER DEFAULT NULL"},
		{"cost", "REAL DEFAULT NULL"}, // 请求成本（美元，使用REAL存储浮点数）
	}

	for _, col := range tokenColumns {
		if _, err := s.logDB.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE logs ADD COLUMN %s %s;", col.name, col.definition),
		); err != nil {
			// 忽略列已存在的错误（SQLite error: "duplicate column name"）
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("add column %s: %w", col.name, err)
			}
		}
	}

	return nil
}
