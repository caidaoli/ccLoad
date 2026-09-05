package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage/schema"
)

const (
	channelModelsRedirectMigrationVersion  = "v1_channel_models_redirect"
	channelModelsOrderRepairVersion        = "v2_channel_models_created_at_order"
	structuredChannelURLsMigrationVersion  = "v4_structured_channel_urls"
	clientProtocolBackfillMigrationVersion = "v5_logs_client_protocol_backfill"
	previousAntigravitySensitiveWords      = `["API","proxy"]`
)

// migrateDailyChannelChecks consumes the legacy setting atomically with its backfill.
// Absence of the setting makes subsequent startups a no-op.
func migrateDailyChannelChecks(ctx context.Context, db *sql.DB, dialect Dialect) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	keyColumn := quoteKeyIdent(dialect)
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM system_settings WHERE "+keyColumn+" = 'channel_check_interval_hours'").Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	hours, parseErr := strconv.ParseFloat(raw, 64)
	minutes := model.DefaultScheduledCheckIntervalMinutes
	if parseErr == nil && hours > 0 && !math.IsInf(hours, 0) {
		minutes = int(math.Min(1440, math.Max(1, math.Ceil(hours*60))))
	}
	query := "UPDATE channels SET scheduled_check_interval_minutes = ?, scheduled_check_start_time = '00:00'"
	if parseErr == nil && hours == 0 {
		query += ", scheduled_check_enabled = 0"
	}
	if _, err := tx.ExecContext(ctx, rebindIfPostgres(dialect, query), minutes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM system_settings WHERE "+keyColumn+" = 'channel_check_interval_hours'"); err != nil {
		return err
	}
	return tx.Commit()
}

// Dialect 数据库方言
type Dialect int

// Dialect 数据库方言常量
const (
	// DialectSQLite SQLite数据库方言
	DialectSQLite Dialect = iota
	// DialectMySQL MySQL数据库方言
	DialectMySQL
	// DialectPostgres PostgreSQL数据库方言
	DialectPostgres
)

// migrateSQLite 执行SQLite数据库迁移
func migrateSQLite(ctx context.Context, db *sql.DB) error {
	// 新库在建表前开启 auto_vacuum。旧库不在启动路径执行完整 VACUUM。
	if err := ensureSQLiteAutoVacuum(ctx, db); err != nil {
		return fmt.Errorf("enable auto_vacuum: %w", err)
	}

	return migrate(ctx, db, DialectSQLite)
}

// migrateMySQL 执行MySQL数据库迁移
func migrateMySQL(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, DialectMySQL)
}

// migratePostgres 执行 PostgreSQL 数据库迁移
func migratePostgres(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, DialectPostgres)
}

// migrate 统一迁移逻辑
func migrate(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// 迁移约束：不得在启动迁移中删除废弃字段或表。
	// 新库由当前 schema 决定不创建；旧库保留原结构和数据，以支持版本回退。
	// 确需执行破坏性迁移时，必须脱离启动路径，由显式运维操作完成。
	// 表定义（顺序重要：外键依赖）
	tables := []func() *schema.TableBuilder{
		schema.DefineSchemaMigrationsTable, // 迁移版本表必须最先创建
		schema.DefineChannelsTable,
		schema.DefineAPIKeysTable,
		schema.DefineChannelModelsTable,
		schema.DefineChannelModelCooldownsTable,
		schema.DefineChannelURLStatesTable,
		schema.DefineAuthTokensTable,
		schema.DefineSystemSettingsTable,
		schema.DefineWebSessionsTable,
		schema.DefineLogsTable,
		schema.DefineDebugLogsTable,
	}

	// 一次性预查全库索引，避免每张表单独 SELECT 网络往返
	allIndexes, err := loadAllExistingIndexes(ctx, db, dialect)
	if err != nil {
		return fmt.Errorf("load all existing indexes: %w", err)
	}

	// 创建表和索引
	for _, defineTable := range tables {
		tb := defineTable()

		// Pre-create hook: debug_logs 表改用 log_id 作为主键（2026-04 重构）
		if tb.Name() == "debug_logs" {
			if err := rebuildDebugLogsPrimaryKey(ctx, db, dialect); err != nil {
				return fmt.Errorf("rebuild debug_logs primary key: %w", err)
			}
			if err := relaxDebugLogsRespBodyNullable(ctx, db, dialect); err != nil {
				return fmt.Errorf("relax debug_logs.resp_body nullability: %w", err)
			}
			if err := rebuildDebugLogsForProtocolPayloads(ctx, db, dialect); err != nil {
				return fmt.Errorf("rebuild debug_logs for protocol payloads: %w", err)
			}
			delete(allIndexes, "debug_logs")
		}

		// Pre-create hook: channel_url_states 主键从 (channel_id, url) 重建为 (channel_id, url_hash)
		// （MySQL utf8mb4 下 VARCHAR(500) 超过 InnoDB 索引列 767 字节上限）
		if tb.Name() == "channel_url_states" {
			if err := rebuildChannelURLStatesPrimaryKey(ctx, db, dialect); err != nil {
				return fmt.Errorf("rebuild channel_url_states primary key: %w", err)
			}
			delete(allIndexes, "channel_url_states")
		}

		// 创建表
		if _, err := db.ExecContext(ctx, buildDDL(tb, dialect)); err != nil {
			return fmt.Errorf("create %s table: %w", tb.Name(), err)
		}
		if tb.Name() == "debug_logs" {
			if err := ensureDebugLogsProtocolMetadata(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate debug_logs protocol metadata: %w", err)
			}
		}

		// 增量迁移：确保logs表新字段存在（2025-12新增）
		if tb.Name() == "logs" {
			if err := ensureLogsNewColumns(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs new columns: %w", err)
			}
			if err := ensureLogsCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs cost_multiplier: %w", err)
			}
			if err := ensureLogsUpstreamWebsocket(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs upstream_websocket: %w", err)
			}
			if err := ensureLogsClientProtocol(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs client_protocol: %w", err)
			}
			if err := ensureLogsUpstreamProtocol(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate logs upstream_protocol: %w", err)
			}
		}

		// 增量迁移：确保channels表有daily_cost_limit字段（2026-01新增）
		if tb.Name() == "channels" {
			if err := ensureChannelsDailyCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels daily_cost_limit: %w", err)
			}
			if err := ensureChannelsRPMLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels rpm_limit: %w", err)
			}
			if err := ensureChannelsMaxConcurrency(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels max_concurrency: %w", err)
			}
			if err := ensureChannelsScheduledCheckEnabled(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels scheduled_check_enabled: %w", err)
			}
			if err := ensureChannelsScheduledCheckModel(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels scheduled_check_model: %w", err)
			}
			if err := ensureChannelsScheduledCheckSchedule(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels daily check schedule: %w", err)
			}
			if err := ensureChannelsCustomRequestRules(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels custom_request_rules: %w", err)
			}
			if err := ensureChannelsCooldownDetectionRules(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels cooldown_detection_rules: %w", err)
			}
			if err := ensureChannelsCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels cost_multiplier: %w", err)
			}
			if err := ensureChannelsProxyURL(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels proxy_url: %w", err)
			}
			if err := ensureChannelsAvailableTime(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels available time: %w", err)
			}
			if err := ensureChannelsRetryOtherKeysOnFailure(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels retry_other_keys_on_failure: %w", err)
			}
			if err := ensureChannelsWebsockets(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels websockets: %w", err)
			}
			if err := ensureChannelsProtocolTransformMode(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels protocol_transform_mode: %w", err)
			}
			if err := ensureChannelsAuthType(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels auth_type: %w", err)
			}
			if err := ensureChannelsOAuthCredential(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels oauth_credential: %w", err)
			}
			// 增量迁移：将url字段从VARCHAR(191)扩展为TEXT（支持多URL存储）
			if err := migrateChannelsURLToText(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels url to text: %w", err)
			}
			if err := migrateChannelURLsToStructuredJSON(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channels url to structured JSON: %w", err)
			}
		}

		// 增量迁移：修复 api_keys.api_key 历史长度漂移（旧版可能为 VARCHAR(64)）
		if tb.Name() == "api_keys" {
			if err := ensureAPIKeysAPIKeyLength(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys api_key column: %w", err)
			}
			if err := ensureAPIKeysDisabled(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys disabled: %w", err)
			}
			if err := ensureAPIKeysNote(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys note: %w", err)
			}
			if err := ensureAPIKeysAllowedModels(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys allowed_models: %w", err)
			}
			if err := ensureAPIKeysModelScopeEmpty(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys model_scope_empty: %w", err)
			}
			if err := ensureAPIKeysCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate api_keys cost_multiplier: %w", err)
			}
			// 一次性回填：api_key 渠道的倍率从 channels.cost_multiplier 下沉到每条 Key
			if err := backfillAPIKeysCostMultiplier(ctx, db, dialect); err != nil {
				return fmt.Errorf("backfill api_keys cost_multiplier: %w", err)
			}
		}

		// 增量迁移：确保auth_tokens表有缓存token字段（2025-12新增）
		if tb.Name() == "auth_tokens" {
			if err := ensureAuthTokensCacheFields(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cache fields: %w", err)
			}
			if err := ensureAuthTokensAllowedModels(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens allowed_models: %w", err)
			}
			if err := validateAuthTokensAllowedModelsJSON(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens allowed_models: %w", err)
			}
			if err := ensureAuthTokensAllowedChannelIDs(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens allowed_channel_ids: %w", err)
			}
			if err := validateAuthTokensAllowedChannelIDsJSON(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens allowed_channel_ids: %w", err)
			}
			if err := ensureAuthTokensChannelRestrictionMode(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens channel_restriction_mode: %w", err)
			}
			if err := validateAuthTokensChannelRestrictionMode(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens channel_restriction_mode: %w", err)
			}
			if err := ensureAuthTokensCostLimit(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens cost_limit: %w", err)
			}
			if err := ensureAuthTokensPeriodCostLimits(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens period cost limits: %w", err)
			}
			if err := ensureAuthTokensMaxConcurrency(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate auth_tokens max_concurrency: %w", err)
			}
			if err := backfillAuthTokensCostLimitMaxConcurrency(ctx, db, dialect); err != nil {
				return fmt.Errorf("backfill auth_tokens max_concurrency: %w", err)
			}
			if err := validateAuthTokensMaxConcurrency(ctx, db); err != nil {
				return fmt.Errorf("validate auth_tokens max_concurrency: %w", err)
			}
		}

		// 增量迁移：channel_models表添加redirect_model字段，迁移数据后删除channels冗余字段
		if tb.Name() == "channel_models" {
			if err := migrateChannelModelsSchema(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channel_models schema: %w", err)
			}
			if err := ensureChannelModelsDisabled(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channel_models disabled: %w", err)
			}
			if err := repairLegacyChannelModelOrder(ctx, db, dialect); err != nil {
				return fmt.Errorf("repair legacy channel_models order: %w", err)
			}
		}

		if tb.Name() == "channel_model_cooldowns" {
			if err := ensureModelCooldownDuration(ctx, db, dialect); err != nil {
				return fmt.Errorf("migrate channel_model_cooldowns cooldown_duration_ms: %w", err)
			}
		}

		// 创建索引
		existingIdx := allIndexes[tb.Name()]
		for _, idx := range buildIndexes(tb, dialect) {
			if existingIdx[idx.Name] {
				continue
			}
			if err := createIndex(ctx, db, idx, dialect); err != nil {
				return err
			}
		}
	}

	// 旧管理员会话不携带身份作用域，不能迁移为新的 Web 会话。
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS admin_sessions"); err != nil {
		return fmt.Errorf("drop obsolete admin_sessions table: %w", err)
	}

	// effective_cost_usd 的历史回填依赖 logs.cost_multiplier，必须等 logs 增量迁移完成后再执行。
	if err := ensureAuthTokensEffectiveCost(ctx, db, dialect); err != nil {
		return fmt.Errorf("migrate auth_tokens effective_cost: %w", err)
	}

	if err := backfillLogsClientProtocol(ctx, db, dialect); err != nil {
		return fmt.Errorf("backfill logs client_protocol: %w", err)
	}

	// 初始化默认配置
	if err := initDefaultSettings(ctx, db, dialect); err != nil {
		return err
	}

	// 清理已移除的配置项（Fail-fast：确保Web管理界面不再暴露危险开关）
	if err := cleanupRemovedSettings(ctx, db, dialect); err != nil {
		return err
	}

	return nil
}

func cleanupRemovedSettings(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// skip_tls_verify 已移除：仅允许通过环境变量 CCLOAD_ALLOW_INSECURE_TLS 控制
	if err := deleteSystemSetting(ctx, db, dialect, "skip_tls_verify"); err != nil {
		return err
	}
	// model_lookup_strip_date_suffix 已移除：不再提供日期后缀回退匹配开关（避免行为分叉）
	if err := deleteSystemSetting(ctx, db, dialect, "model_lookup_strip_date_suffix"); err != nil {
		return err
	}
	return nil
}

func deleteSystemSetting(ctx context.Context, db *sql.DB, dialect Dialect, key string) error {
	query := fmt.Sprintf("DELETE FROM system_settings WHERE %s = ?", quoteKeyIdent(dialect))
	if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, query), key); err != nil {
		return fmt.Errorf("delete system setting %s: %w", key, err)
	}
	return nil
}

// hasSystemSetting 检查系统设置是否存在（用于配置迁移和旧版标记兼容）
func hasSystemSetting(ctx context.Context, db *sql.DB, dialect Dialect, key string) bool {
	query := fmt.Sprintf("SELECT 1 FROM system_settings WHERE %s = ? LIMIT 1", quoteKeyIdent(dialect))
	var exists int
	err := db.QueryRowContext(ctx, rebindIfPostgres(dialect, query), key).Scan(&exists)
	return err == nil
}

// loadAllExistingIndexes 一次性查询整个数据库下所有表的现有索引集合
func loadAllExistingIndexes(ctx context.Context, db *sql.DB, dialect Dialect) (map[string]map[string]bool, error) {
	var query string
	switch dialect {
	case DialectMySQL:
		query = "SELECT DISTINCT TABLE_NAME, INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()"
	case DialectPostgres:
		query = "SELECT tablename, indexname FROM pg_indexes WHERE schemaname = current_schema()"
	default:
		query = "SELECT tbl_name, name FROM sqlite_master WHERE type='index' AND tbl_name IS NOT NULL"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query all indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]map[string]bool)
	for rows.Next() {
		var tbl, idx string
		if err := rows.Scan(&tbl, &idx); err != nil {
			return nil, fmt.Errorf("scan index row: %w", err)
		}
		if result[tbl] == nil {
			result[tbl] = make(map[string]bool)
		}
		result[tbl][idx] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexes: %w", err)
	}
	return result, nil
}

func buildDDL(tb *schema.TableBuilder, dialect Dialect) string {
	switch dialect {
	case DialectMySQL:
		return tb.BuildMySQL()
	case DialectPostgres:
		return tb.BuildPostgres()
	default:
		return tb.BuildSQLite()
	}
}

func buildIndexes(tb *schema.TableBuilder, dialect Dialect) []schema.IndexDef {
	switch dialect {
	case DialectMySQL:
		return tb.GetIndexesMySQL()
	case DialectPostgres:
		return tb.GetIndexesPostgres()
	default:
		return tb.GetIndexesSQLite()
	}
}

func createIndex(ctx context.Context, db *sql.DB, idx schema.IndexDef, dialect Dialect) error {
	_, err := db.ExecContext(ctx, idx.SQL)
	if err == nil {
		return nil
	}

	// MySQL 5.6不支持IF NOT EXISTS，忽略重复索引错误(1061)
	if dialect == DialectMySQL {
		errMsg := err.Error()
		if strings.Contains(errMsg, "1061") ||
			strings.Contains(errMsg, "Duplicate key name") ||
			strings.Contains(errMsg, "already exist") {
			return nil
		}
	}
	if dialect == DialectPostgres {
		errMsg := err.Error()
		if strings.Contains(errMsg, "already exists") {
			return nil
		}
	}

	return fmt.Errorf("create index: %w", err)
}

func initDefaultSettings(ctx context.Context, db *sql.DB, dialect Dialect) error {
	healthDefaults := model.DefaultHealthScoreConfig()
	responsesWSMaxSessionsDescription := fmt.Sprintf(
		"Responses WebSocket execution session limit (process-wide, 0 = use default %d)",
		config.DefaultResponsesWebsocketMaxSessions,
	)
	responsesWSSessionTTLDescription := fmt.Sprintf(
		"Responses WebSocket idle session retention (minutes, 0 = use default %d)",
		config.DefaultResponsesWebsocketSessionTTLMinutes,
	)
	responsesWSMaxTranscriptBytesDescription := fmt.Sprintf(
		"Responses WebSocket transcript payload budget (process-wide bytes, 0 = use default %d MiB)",
		config.DefaultResponsesWebsocketMaxTranscriptBytes/(1024*1024),
	)
	responsesWSMaxConnectionsDescription := fmt.Sprintf(
		"Responses WebSocket downstream connection limit (process-wide, 0 = use default %d)",
		config.DefaultResponsesWebsocketMaxConnections,
	)
	responsesWSMaxConnectionsPerTokenDescription := fmt.Sprintf(
		"Responses WebSocket downstream connection limit per API token (0 = use default %d)",
		config.DefaultResponsesWebsocketMaxConnectionsPerToken,
	)
	settings := []struct {
		key, value, valueType, desc, defaultVal string
	}{
		{config.CodexBaseURLSettingKey, "", "string", "Codex OAuth 完整 Responses URL(留空使用渠道URL；填写后覆盖渠道URL)", ""},
		{config.XAIBaseURLSettingKey, "", "string", "xAI OAuth API根地址(通常以/v1结尾；留空使用渠道URL；填写后覆盖渠道URL)", ""},
		{config.AntigravityURLSettingKey, "", "string", "Antigravity OAuth API根地址(留空使用渠道URL；填写后覆盖渠道URL)", ""},
		{config.AnthropicBaseURLSettingKey, "", "string", "Anthropic OAuth API根地址(留空使用渠道URL；填写后覆盖渠道URL)", ""},
		{"log_retention_days", "7", "int", "日志保留天数(-1永久保留,1-365天)", "7"},
		{"max_key_retries", "3", "int", "单渠道最大Key重试次数", "3"},
		{"max_concurrency", "1000", "int", "最大并发请求数(限制同时处理的代理请求数量)", "1000"},
		{"http_read_timeout_seconds", "0", "duration", "下游请求读取超时(秒,覆盖请求头+请求体的整段读取,0=使用内建默认值120秒)", "0"},
		{"max_body_bytes", "10485760", "int", "请求体最大字节数(默认10MB)", "10485760"},
		{"max_image_body_bytes", "20971520", "int", "Images API 请求体最大字节数(默认20MB)", "20971520"},
		{"cooldown_auth_seconds", "300", "int", "认证错误(401/402/403)初始冷却时间(秒)", "300"},
		{"cooldown_server_seconds", "120", "int", "服务器错误(5xx)初始冷却时间(秒)", "120"},
		{"cooldown_timeout_seconds", "60", "int", "超时错误(597/598)初始冷却时间(秒)", "60"},
		{"cooldown_rate_limit_seconds", "60", "int", "限流错误(429)初始冷却时间(秒)", "60"},
		{"cooldown_max_seconds", "1800", "int", "指数退避冷却上限(秒,>=1且必须>=cooldown_min_seconds)", "1800"},
		{"cooldown_min_seconds", "10", "int", "指数退避冷却下限(秒,>=1且必须<=cooldown_max_seconds)", "10"},
		{config.CodexMap429To503SettingKey, "false", "bool", "所有上游候选均失败时，将返回给官方 Codex 客户端的最终 429 映射为 503，使其按 5xx 重试", "false"},
		{"global_cooldown_detection_rules", "{}", "json", "未配置渠道专属规则时继承的全局冷却探测规则", "{}"},
		{"model_multimodal_fallback", "{}", "json", "多模态请求(含图片/文件等非文本内容)自动改用回退模型，JSON对象格式 {\"文本模型\":\"回退模型\"}", "{}"},
		{"antigravity_sensitive_words", config.DefaultAntigravitySensitiveWordsJSON, "json", "Antigravity systemInstruction 中使用零宽字符替换的敏感词 JSON 字符串数组", config.DefaultAntigravitySensitiveWordsJSON},
		{"upstream_first_byte_timeout", "0", "duration", "流式请求首个有效内容超时(秒,0=禁用)", "0"},
		{"upstream_connection_reuse_limit_seconds", "0", "duration", "上游连接最长复用时间(秒,0=不限制;达到时限后不接收新请求,在途请求完成后关闭)", "0"},
		{"stream_timeout", "0", "duration", "流式请求总超时(秒,0=禁用)", "0"},
		{"non_stream_timeout", "120", "duration", "非流式请求超时(秒,0=禁用)", "120"},
		{"anthropic_first_byte_timeout", "0", "duration", "Anthropic流式请求首个有效内容超时(秒,0=使用全局upstream_first_byte_timeout)", "0"},
		{"anthropic_non_stream_timeout", "0", "duration", "Anthropic非流式请求超时(秒,0=使用全局non_stream_timeout)", "0"},
		{"codex_first_byte_timeout", "0", "duration", "Codex流式请求首个有效内容超时(秒,0=使用全局upstream_first_byte_timeout)", "0"},
		{"codex_non_stream_timeout", "0", "duration", "Codex非流式请求超时(秒,0=使用全局non_stream_timeout)", "0"},
		{"openai_first_byte_timeout", "0", "duration", "OpenAI流式请求首个有效内容超时(秒,0=使用全局upstream_first_byte_timeout)", "0"},
		{"openai_non_stream_timeout", "0", "duration", "OpenAI非流式请求超时(秒,0=使用全局non_stream_timeout)", "0"},
		{"gemini_first_byte_timeout", "0", "duration", "Gemini流式请求首个有效内容超时(秒,0=使用全局upstream_first_byte_timeout)", "0"},
		{"gemini_non_stream_timeout", "0", "duration", "Gemini非流式请求超时(秒,0=使用全局non_stream_timeout)", "0"},
		{"model_fuzzy_match", "false", "bool", "模型匹配失败时，使用子串模糊匹配(多匹配时选最新版本)", "false"},
		{"channel_test_content", config.DefaultChannelTestContent, "string", "渠道测试默认内容（不能为空）", config.DefaultChannelTestContent},
		{"model_catalog_sync_interval_hours", "6", "float", "从 models.dev 同步官方模型定价目录的间隔（小时，支持小数）；0 仅关闭网络同步，继续使用最近缓存或内置定价；不影响渠道模型列表", "6"},
		{"auto_update_interval_hours", "12", "int", "非容器部署的版本检查间隔（整数小时；0=关闭检查；启用时最低1小时）", "12"},
		{"auto_update_channel", "stable", "string", "非容器部署的版本检查和自动更新渠道（stable=稳定版，preview=稳定版和测试版）", "stable"},
		{"log_channel_click_action", "edit", "string", "日志页点击渠道名后的操作", "edit"},
		{"channel_stats_range", "today", "string", "渠道管理页费用统计时间范围（today/yesterday/day_before_yesterday/this_week/last_week/this_month/last_month）", "today"},
		// 健康度排序配置
		{"enable_health_score", "false", "bool", "启用基于健康度的渠道动态排序", "false"},
		{"success_rate_penalty_weight", strconv.Itoa(healthDefaults.SuccessRatePenaltyWeight), "int", "成功率惩罚权重(乘以失败率,>=0)", strconv.Itoa(healthDefaults.SuccessRatePenaltyWeight)},
		{"health_score_window_minutes", strconv.Itoa(healthDefaults.WindowMinutes), "int", "成功率统计时间窗口(分钟,>=1)", strconv.Itoa(healthDefaults.WindowMinutes)},
		{"health_score_update_interval", strconv.Itoa(healthDefaults.UpdateIntervalSeconds), "int", "成功率缓存更新间隔(秒,>=1)", strconv.Itoa(healthDefaults.UpdateIntervalSeconds)},
		{"health_min_confident_sample", strconv.Itoa(healthDefaults.MinConfidentSample), "int", "置信样本量阈值(>=1;样本量达到此值时惩罚全额生效)", strconv.Itoa(healthDefaults.MinConfidentSample)},
		{"enable_ttfb_score", "false", "bool", "启用渠道首字相对延迟惩罚(需同时开启「启用健康度排序」)", "false"},
		{"ttfb_penalty_weight", strconv.FormatFloat(healthDefaults.TTFBPenaltyWeight, 'f', -1, 64), "float", "首字惩罚权重(>=0;相对中位慢1倍时全置信惩罚值)", strconv.FormatFloat(healthDefaults.TTFBPenaltyWeight, 'f', -1, 64)},
		{"ttfb_max_slow_ratio", strconv.FormatFloat(healthDefaults.TTFBMaxSlowRatio, 'f', -1, 64), "float", "首字相对慢速比(s-1)上限(>=0)", strconv.FormatFloat(healthDefaults.TTFBMaxSlowRatio, 'f', -1, 64)},
		{"ttfb_min_confident_sample", strconv.Itoa(healthDefaults.TTFBMinConfidentSample), "int", "首字置信样本量阈值(>=1)", strconv.Itoa(healthDefaults.TTFBMinConfidentSample)},
		// 冷却兜底配置
		{"cooldown_fallback_enabled", "true", "bool", "所有渠道冷却时选最优渠道兜底(关闭则直接拒绝请求)", "true"},
		// Debug日志配置
		{"debug_log_enabled", "false", "bool", "启用Debug日志(记录上游请求/响应原始数据)", "false"},
		{"debug_log_retention_minutes", strconv.Itoa(config.DefaultDebugLogRetentionMinutes), "int", "Debug日志保留时长(分钟,1-1440)", strconv.Itoa(config.DefaultDebugLogRetentionMinutes)},
		// 前端自动刷新
		{"auto_refresh_interval_seconds", "0", "int", "页面自动刷新间隔(秒,>=0;0=禁用,建议≥30;有对话框打开时跳过本次刷新)", "0"},
		{config.ActiveRequestTitleEnabledSettingKey, "false", "bool", "有请求处理时在浏览器标题栏显示请求数量并闪烁", "false"},
		// Responses WebSocket
		{"responses_ws_max_sessions", "0", "int", responsesWSMaxSessionsDescription, "0"},
		{"responses_ws_session_ttl_minutes", "0", "int", responsesWSSessionTTLDescription, "0"},
		{"responses_ws_max_transcript_bytes", "0", "int", responsesWSMaxTranscriptBytesDescription, "0"},
		{"responses_ws_max_connections", "0", "int", responsesWSMaxConnectionsDescription, "0"},
		{"responses_ws_max_connections_per_token", "0", "int", responsesWSMaxConnectionsPerTokenDescription, "0"},
	}

	var query string
	switch dialect {
	case DialectMySQL:
		query = "INSERT IGNORE INTO system_settings (`key`, value, value_type, description, default_value, updated_at) VALUES (?, ?, ?, ?, ?, UNIX_TIMESTAMP())"
	case DialectPostgres:
		query = `INSERT INTO system_settings ("key", value, value_type, description, default_value, updated_at) VALUES (?, ?, ?, ?, ?, EXTRACT(EPOCH FROM NOW())::BIGINT) ON CONFLICT ("key") DO NOTHING`
	default:
		query = "INSERT OR IGNORE INTO system_settings (key, value, value_type, description, default_value, updated_at) VALUES (?, ?, ?, ?, ?, unixepoch())"
	}

	for _, s := range settings {
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, query), s.key, s.value, s.valueType, s.desc, s.defaultVal); err != nil {
			return fmt.Errorf("insert default setting %s: %w", s.key, err)
		}
	}

	// 默认词表在 CLIProxyAPI 示例的 ["API","proxy"] 基础上加入 Claude/Anthropic。
	// 仅迁移仍保持旧默认值的记录；其他值视为用户配置，不覆盖。
	{
		keyCol := quoteKeyIdent(dialect)
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		valueSQL := fmt.Sprintf("UPDATE system_settings SET value = ? WHERE %s = ? AND value = default_value AND default_value IN (?, ?)", keyCol)
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, valueSQL),
			config.DefaultAntigravitySensitiveWordsJSON,
			"antigravity_sensitive_words",
			"[]",
			previousAntigravitySensitiveWords,
		); err != nil {
			return fmt.Errorf("migrate setting value antigravity_sensitive_words: %w", err)
		}
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		metaSQL := fmt.Sprintf("UPDATE system_settings SET default_value = ?, value_type = ? WHERE %s = ?", keyCol)
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, metaSQL), config.DefaultAntigravitySensitiveWordsJSON, "json", "antigravity_sensitive_words"); err != nil {
			return fmt.Errorf("refresh setting metadata antigravity_sensitive_words: %w", err)
		}
	}

	// 刷新部分配置项的元信息（description/default/value_type），避免"代码语义已变但DB描述仍旧"。
	{
		keyCol := quoteKeyIdent(dialect)
		descriptionRefreshKeys := map[string]bool{
			"antigravity_sensitive_words":            true,
			"channel_test_content":                   true,
			"channel_stats_range":                    true,
			"success_rate_penalty_weight":            true,
			"health_score_window_minutes":            true,
			"health_score_update_interval":           true,
			"health_min_confident_sample":            true,
			"ttfb_penalty_weight":                    true,
			"ttfb_max_slow_ratio":                    true,
			"ttfb_min_confident_sample":              true,
			"cooldown_min_seconds":                   true,
			"cooldown_max_seconds":                   true,
			"debug_log_retention_minutes":            true,
			"auto_refresh_interval_seconds":          true,
			"responses_ws_max_sessions":              true,
			"responses_ws_session_ttl_minutes":       true,
			"responses_ws_max_transcript_bytes":      true,
			"responses_ws_max_connections":           true,
			"responses_ws_max_connections_per_token": true,
		}
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		descriptionSQL := fmt.Sprintf("UPDATE system_settings SET description = ?, value_type = ? WHERE %s = ?", keyCol)
		for _, setting := range settings {
			if !descriptionRefreshKeys[setting.key] {
				continue
			}
			if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, descriptionSQL), setting.desc, setting.valueType, setting.key); err != nil {
				return fmt.Errorf("refresh setting description %s: %w", setting.key, err)
			}
		}

		websocketResetDefaultSQL := fmt.Sprintf("UPDATE system_settings SET default_value = '0' WHERE %s = ?", keyCol)
		for _, key := range []string{
			"responses_ws_max_sessions",
			"responses_ws_session_ttl_minutes",
			"responses_ws_max_transcript_bytes",
			"responses_ws_max_connections",
			"responses_ws_max_connections_per_token",
		} {
			if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, websocketResetDefaultSQL), key); err != nil {
				return fmt.Errorf("refresh setting reset default %s: %w", key, err)
			}
		}

		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		metaSQL := fmt.Sprintf("UPDATE system_settings SET description = ?, default_value = ?, value_type = ? WHERE %s = ?", keyCol)
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, metaSQL),
			"流式请求首个有效内容超时(秒,0=禁用)",
			"0",
			"duration",
			"upstream_first_byte_timeout",
		); err != nil {
			return fmt.Errorf("refresh setting metadata upstream_first_byte_timeout: %w", err)
		}
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, metaSQL),
			"Debug日志保留时长(分钟,1-1440)",
			strconv.Itoa(config.DefaultDebugLogRetentionMinutes),
			"int",
			"debug_log_retention_minutes",
		); err != nil {
			return fmt.Errorf("refresh setting metadata debug_log_retention_minutes: %w", err)
		}
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, metaSQL),
			"非容器部署的版本检查间隔（整数小时；0=关闭检查；启用时最低1小时）",
			"12",
			"int",
			"auto_update_interval_hours",
		); err != nil {
			return fmt.Errorf("refresh setting metadata auto_update_interval_hours: %w", err)
		}
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, metaSQL),
			"非容器部署的版本检查和自动更新渠道（stable=稳定版，preview=稳定版和测试版）",
			"stable",
			"string",
			"auto_update_channel",
		); err != nil {
			return fmt.Errorf("refresh setting metadata auto_update_channel: %w", err)
		}
	}

	// 迁移 success_rate_penalty_weight 类型：float → int（2026-01 类型修正）
	{
		keyCol := quoteKeyIdent(dialect)
		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		typeSQL := fmt.Sprintf("UPDATE system_settings SET value_type = 'int' WHERE %s = 'success_rate_penalty_weight' AND value_type = 'float'", keyCol)
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, typeSQL)); err != nil {
			return fmt.Errorf("migrate success_rate_penalty_weight type: %w", err)
		}
	}

	if err := migrateDailyChannelChecks(ctx, db, dialect); err != nil {
		return fmt.Errorf("migrate daily channel checks: %w", err)
	}

	// 迁移旧 migration marker 从 system_settings 到 schema_migrations
	legacyMigrationMarkers := []string{
		"minute_bucket_backfill_done", // 2026-01迁移：迁移标记改存 schema_migrations 表
	}
	for _, marker := range legacyMigrationMarkers {
		if hasSystemSetting(ctx, db, dialect, marker) {
			_ = recordMigration(ctx, db, marker, dialect)
			_ = deleteSystemSetting(ctx, db, dialect, marker)
		}
	}

	// 迁移旧键名 cooldown_fallback_threshold → cooldown_fallback_enabled
	if hasSystemSetting(ctx, db, dialect, "cooldown_fallback_threshold") {
		const oldKey = "cooldown_fallback_threshold"
		const newKey = "cooldown_fallback_enabled"

		keyCol := quoteKeyIdent(dialect)

		//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
		valueMigrateSQL := fmt.Sprintf(`UPDATE system_settings SET value = CASE WHEN value = '0' THEN 'false' ELSE 'true' END WHERE %s = ? AND value_type = 'int'`, keyCol)
		if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, valueMigrateSQL), oldKey); err != nil {
			return fmt.Errorf("migrate setting value %s: %w", oldKey, err)
		}

		if hasSystemSetting(ctx, db, dialect, newKey) {
			if err := deleteSystemSetting(ctx, db, dialect, oldKey); err != nil {
				return err
			}
		} else {
			//nolint:gosec // G201: keyCol 仅为 "key" 或 "`key`"，由内部逻辑控制
			renameSQL := fmt.Sprintf("UPDATE system_settings SET %s = ?, description = ?, default_value = ?, value_type = ? WHERE %s = ?", keyCol, keyCol)
			if _, err := db.ExecContext(ctx, rebindIfPostgres(dialect, renameSQL), newKey, "所有渠道冷却时选最优渠道兜底(关闭则直接拒绝请求)", "true", "bool", oldKey); err != nil {
				return fmt.Errorf("rename setting %s to %s: %w", oldKey, newKey, err)
			}
		}
	}

	return nil
}

// hasMigration 检查迁移是否已执行；查询失败时按未执行处理。
func hasMigration(ctx context.Context, db *sql.DB, version string, dialect Dialect) bool {
	var count int
	err := db.QueryRowContext(ctx,
		rebindIfPostgres(dialect, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?"), version,
	).Scan(&count)
	if err != nil {
		// 表不存在时视为未执行
		return false
	}
	return count > 0
}

// recordMigration 记录迁移已执行
func recordMigration(ctx context.Context, db *sql.DB, version string, dialect Dialect) error {
	insertSQL := insertIgnoreSchemaMigrationSQL(dialect)
	_, err := db.ExecContext(ctx, rebindIfPostgres(dialect, insertSQL), version)
	return err
}

func migrationAppliedAt(ctx context.Context, db *sql.DB, version string, dialect Dialect) (int64, bool, error) {
	var appliedAt int64
	err := db.QueryRowContext(ctx, rebindIfPostgres(dialect, `SELECT applied_at FROM schema_migrations WHERE version = ?`), version).Scan(&appliedAt)
	if err == nil {
		return appliedAt, true, nil
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("query migration %s applied_at: %w", version, err)
}

func recordMigrationTx(ctx context.Context, tx *sql.Tx, version string, dialect Dialect) error {
	insertSQL := insertIgnoreSchemaMigrationSQL(dialect)
	_, err := tx.ExecContext(ctx, rebindIfPostgres(dialect, insertSQL), version)
	return err
}

// ensureSQLiteAutoVacuum 确保新建 SQLite 库开启 auto_vacuum=INCREMENTAL。
// 旧库切换 auto_vacuum 需要完整 VACUUM，会重写整个数据库文件，不能放在普通启动路径。
func ensureSQLiteAutoVacuum(ctx context.Context, db *sql.DB) error {
	empty, err := sqliteHasNoUserTables(ctx, db)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}

	// 读取当前 auto_vacuum 设置
	var currentMode int
	if err := db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&currentMode); err != nil {
		return fmt.Errorf("query auto_vacuum: %w", err)
	}

	// 2 = INCREMENTAL（按需释放空闲页）
	// 0 = NONE（默认值，不自动回收）
	// 1 = FULL（每次提交都整理，性能开销大）
	if currentMode == 2 {
		return nil // 已启用
	}

	// 设置 auto_vacuum = INCREMENTAL
	if _, err := db.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("set auto_vacuum: %w", err)
	}

	// 空库执行 VACUUM 只是把 auto_vacuum 写入数据库头，不会重写业务数据。
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("VACUUM to activate auto_vacuum: %w", err)
	}

	return nil
}

func sqliteHasNoUserTables(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'
	`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query sqlite user tables: %w", err)
	}
	return count == 0, nil
}
