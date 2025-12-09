package schema

// DefineChannelsTable 定义channels表结构
func DefineChannelsTable() *TableBuilder {
	return NewTable("channels").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("name VARCHAR(191) NOT NULL UNIQUE").
		Column("url VARCHAR(191) NOT NULL").
		Column("priority INT NOT NULL DEFAULT 0").
		Column("models TEXT NOT NULL").
		Column("model_redirects TEXT NOT NULL").
		Column("channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic'").
		Column("enabled TINYINT NOT NULL DEFAULT 1").
		Column("cooldown_until BIGINT NOT NULL DEFAULT 0").
		Column("cooldown_duration_ms BIGINT NOT NULL DEFAULT 0").
		Column("rr_key_index INT NOT NULL DEFAULT 0").
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
		Column("api_key VARCHAR(100) NOT NULL").
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
		Column("total_cost_usd DOUBLE NOT NULL DEFAULT 0.0").
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
		Column("token VARCHAR(64) PRIMARY KEY").
		Column("expires_at BIGINT NOT NULL").
		Column("created_at BIGINT NOT NULL").
		Index("idx_admin_sessions_expires", "expires_at")
}

// DefineLogsTable 定义logs表结构
func DefineLogsTable() *TableBuilder {
	return NewTable("logs").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("time BIGINT NOT NULL").
		Column("model VARCHAR(191) NOT NULL DEFAULT ''").
		Column("channel_id INT NOT NULL DEFAULT 0").
		Column("status_code INT NOT NULL").
		Column("message TEXT NOT NULL").
		Column("duration DOUBLE NOT NULL DEFAULT 0.0").
		Column("is_streaming TINYINT NOT NULL DEFAULT 0").
		Column("first_byte_time DOUBLE NOT NULL DEFAULT 0.0").
		Column("api_key_used VARCHAR(191) NOT NULL DEFAULT ''").
		Column("input_tokens INT NOT NULL DEFAULT 0").
		Column("output_tokens INT NOT NULL DEFAULT 0").
		Column("cache_read_input_tokens INT NOT NULL DEFAULT 0").
		Column("cache_creation_input_tokens INT NOT NULL DEFAULT 0").
		Column("cost DOUBLE NOT NULL DEFAULT 0.0").
		Index("idx_logs_time_model", "time, model").
		Index("idx_logs_time_channel", "time, channel_id").
		Index("idx_logs_time_status", "time, status_code").
		Index("idx_logs_time_channel_model", "time, channel_id, model")
}
