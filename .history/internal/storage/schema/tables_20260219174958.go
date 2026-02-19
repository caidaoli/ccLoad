package schema

// DefineChannelsTable 定义channels表结构
func DefineChannelsTable() *TableBuilder {
	return NewTable("channels").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("name VARCHAR(191) NOT NULL UNIQUE").
		Column("url VARCHAR(191) NOT NULL").
		Column("priority INT NOT NULL DEFAULT 0").
		Column("channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic'").
		Column("enabled TINYINT NOT NULL DEFAULT 1").
		Column("cooldown_until BIGINT NOT NULL DEFAULT 0").
		Column("cooldown_duration_ms BIGINT NOT NULL DEFAULT 0").
		Column("daily_cost_limit DOUBLE NOT NULL DEFAULT 0").
		Column("created_at BIGINT NOT NULL").
		Column("updated_at BIGINT NOT NULL").
		Index("idx_channels_enabled", "enabled").
		Index("idx_channels_priority", "priority DESC").
		Index("idx_channels_type_enabled", "channel_type, enabled").
		Index("idx_channels_cooldown", "cooldown_until")
}

// DefineAPIKeysTable 定义api_keys表结构
func DefineAPIKeysTable() *TableBuilder {
	return NewTable("api_keys").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("channel_id INT NOT NULL").
		Column("key_index INT NOT NULL").
		Column("api_key VARCHAR(512) NOT NULL").
		Column("key_strategy VARCHAR(32) NOT NULL DEFAULT 'sequential'").
		Column("cooldown_until BIGINT NOT NULL DEFAULT 0").
		Column("cooldown_duration_ms BIGINT NOT NULL DEFAULT 0").
		Column("created_at BIGINT NOT NULL").
		Column("updated_at BIGINT NOT NULL").
		Column("UNIQUE KEY uk_channel_key (channel_id, key_index)").
		Column("FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE").
		Index("idx_api_keys_cooldown", "cooldown_until").
		Index("idx_api_keys_channel_cooldown", "channel_id, cooldown_until")
}

// DefineChannelModelsTable 定义channel_models表结构
func DefineChannelModelsTable() *TableBuilder {
	return NewTable("channel_models").
		Column("channel_id INT NOT NULL").
		Column("model VARCHAR(191) NOT NULL").
		Column("redirect_model VARCHAR(191) NOT NULL DEFAULT ''"). // 重定向目标模型（空表示不重定向）
		Column("created_at BIGINT NOT NULL DEFAULT 0").
		Column("PRIMARY KEY (channel_id, model)").
		Column("FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE").
		Index("idx_channel_models_model", "model")
}

// DefineAuthTokensTable 定义auth_tokens表结构
func DefineAuthTokensTable() *TableBuilder {
	return NewTable("auth_tokens").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("token VARCHAR(100) NOT NULL UNIQUE").
		Column("description VARCHAR(512) NOT NULL").
		Column("created_at BIGINT NOT NULL").
		Column("expires_at BIGINT NOT NULL DEFAULT 0").
		Column("last_used_at BIGINT NOT NULL DEFAULT 0").
		Column("is_active TINYINT NOT NULL DEFAULT 1").
		Column("success_count INT NOT NULL DEFAULT 0").
		Column("failure_count INT NOT NULL DEFAULT 0").
		Column("stream_avg_ttfb DOUBLE NOT NULL DEFAULT 0.0").
		Column("non_stream_avg_rt DOUBLE NOT NULL DEFAULT 0.0").
		Column("stream_count INT NOT NULL DEFAULT 0").
		Column("non_stream_count INT NOT NULL DEFAULT 0").
		Column("prompt_tokens_total BIGINT NOT NULL DEFAULT 0").
		Column("completion_tokens_total BIGINT NOT NULL DEFAULT 0").
		Column("cache_read_tokens_total BIGINT NOT NULL DEFAULT 0").
		Column("cache_creation_tokens_total BIGINT NOT NULL DEFAULT 0").
		Column("total_cost_usd DOUBLE NOT NULL DEFAULT 0.0").
		Column("cost_used_microusd BIGINT NOT NULL DEFAULT 0").
		Column("cost_limit_microusd BIGINT NOT NULL DEFAULT 0").
		Index("idx_auth_tokens_active", "is_active").
		Index("idx_auth_tokens_expires", "expires_at")
}

// DefineSystemSettingsTable 定义system_settings表结构
func DefineSystemSettingsTable() *TableBuilder {
	return NewTable("system_settings").
		Column("`key` VARCHAR(128) PRIMARY KEY").
		Column("value TEXT NOT NULL").
		Column("value_type VARCHAR(32) NOT NULL").
		Column("description VARCHAR(512) NOT NULL").
		Column("default_value VARCHAR(512) NOT NULL").
		Column("updated_at BIGINT NOT NULL")
}

// DefineAdminSessionsTable 定义admin_sessions表结构
func DefineAdminSessionsTable() *TableBuilder {
	return NewTable("admin_sessions").
		Column("token VARCHAR(64) PRIMARY KEY"). // SHA256哈希(64字符十六进制,2025-12改为存储哈希而非明文)
		Column("expires_at BIGINT NOT NULL").
		Column("created_at BIGINT NOT NULL").
		Index("idx_admin_sessions_expires", "expires_at")
}

// DefineSchemaMigrationsTable 定义schema_migrations表结构（迁移版本控制）
func DefineSchemaMigrationsTable() *TableBuilder {
	return NewTable("schema_migrations").
		Column("version VARCHAR(64) PRIMARY KEY"). // 迁移版本标识
		Column("applied_at BIGINT NOT NULL")       // 应用时间（Unix秒）
}

// DefineLogsTable 定义logs表结构
func DefineLogsTable() *TableBuilder {
	return NewTable("logs").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("time BIGINT NOT NULL").
		Column("minute_bucket BIGINT NOT NULL DEFAULT 0"). // time/60000，用于RPM类聚合避免运行时FLOOR
		Column("model VARCHAR(191) NOT NULL DEFAULT ''").
		Column("actual_model VARCHAR(191) NOT NULL DEFAULT ''"). // 实际转发的模型（空表示未重定向）
		Column("channel_id INT NOT NULL DEFAULT 0").
		Column("status_code INT NOT NULL").
		Column("message TEXT NOT NULL").
		Column("duration DOUBLE NOT NULL DEFAULT 0.0").
		Column("is_streaming TINYINT NOT NULL DEFAULT 0").
		Column("first_byte_time DOUBLE NOT NULL DEFAULT 0.0").
		Column("api_key_used VARCHAR(191) NOT NULL DEFAULT ''").
		Column("api_key_hash VARCHAR(64) NOT NULL DEFAULT ''"). // API Key SHA256（用于精确定位 key_index）
		Column("auth_token_id BIGINT NOT NULL DEFAULT 0").      // 客户端使用的API令牌ID（新增2025-12）
		Column("client_ip VARCHAR(45) NOT NULL DEFAULT ''").    // 客户端IP地址（新增2025-12）
		Column("input_tokens INT NOT NULL DEFAULT 0").
		Column("output_tokens INT NOT NULL DEFAULT 0").
		Column("cache_read_input_tokens INT NOT NULL DEFAULT 0").
		Column("cache_creation_input_tokens INT NOT NULL DEFAULT 0"). // 5m+1h缓存总和（兼容字段）
		Column("cache_5m_input_tokens INT NOT NULL DEFAULT 0").       // 5分钟缓存写入Token数（新增2025-12）
		Column("cache_1h_input_tokens INT NOT NULL DEFAULT 0").       // 1小时缓存写入Token数（新增2025-12）
		Column("cost DOUBLE NOT NULL DEFAULT 0.0").
		Index("idx_logs_time_model", "time, model").
		Index("idx_logs_time_status", "time, status_code").
		Index("idx_logs_time_channel_model", "time, channel_id, model").
		Index("idx_logs_minute_channel_model", "minute_bucket, channel_id, model").
		Index("idx_logs_time_auth_token", "time, auth_token_id"). // 按时间+令牌查询
		Index("idx_logs_time_actual_model", "time, actual_model") // 按时间+实际模型查询
}
