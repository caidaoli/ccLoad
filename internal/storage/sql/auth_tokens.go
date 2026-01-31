package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

//nolint:gosec // SQL列清单包含“token”字段名，并非硬编码凭据
const authTokenSelectColumns = `
	id, token, description, created_at, expires_at, last_used_at, is_active,
	success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
	prompt_tokens_total, completion_tokens_total, cache_read_tokens_total, cache_creation_tokens_total, total_cost_usd,
	cost_used_microusd, cost_limit_microusd, allowed_models
`

//nolint:gosec // SQL查询模板包含"token"字段名，并非硬编码凭据
const updateTokenStatsQuery = `
	UPDATE auth_tokens
	SET
		success_count = success_count + CASE WHEN ? = 1 THEN 1 ELSE 0 END,
		failure_count = failure_count + CASE WHEN ? = 1 THEN 1 ELSE 0 END,

		-- 只有成功请求才累加 token 与费用（与内存费用缓存语义保持一致）
		prompt_tokens_total = prompt_tokens_total + CASE WHEN ? = 1 THEN ? ELSE 0 END,
		completion_tokens_total = completion_tokens_total + CASE WHEN ? = 1 THEN ? ELSE 0 END,
		cache_read_tokens_total = cache_read_tokens_total + CASE WHEN ? = 1 THEN ? ELSE 0 END,
		cache_creation_tokens_total = cache_creation_tokens_total + CASE WHEN ? = 1 THEN ? ELSE 0 END,
		total_cost_usd = total_cost_usd + CASE WHEN ? = 1 THEN ? ELSE 0 END,
		cost_used_microusd = cost_used_microusd + CASE WHEN ? = 1 THEN ? ELSE 0 END,

		-- 增量更新平均值（new_avg = (old_avg*old_count + v)/(old_count+1)）
		stream_avg_ttfb = CASE
			WHEN ? = 1 THEN ((stream_avg_ttfb * stream_count) + ?) / (stream_count + 1)
			ELSE stream_avg_ttfb
		END,
		stream_count = stream_count + CASE WHEN ? = 1 THEN 1 ELSE 0 END,
		non_stream_avg_rt = CASE
			WHEN ? = 1 THEN ((non_stream_avg_rt * non_stream_count) + ?) / (non_stream_count + 1)
			ELSE non_stream_avg_rt
		END,
		non_stream_count = non_stream_count + CASE WHEN ? = 1 THEN 1 ELSE 0 END,

		last_used_at = ?
	WHERE token = ?
`

func scanAuthToken(scanner interface {
	Scan(...any) error
}) (*model.AuthToken, error) {
	token := &model.AuthToken{}
	var createdAtMs int64
	var expiresAt, lastUsedAt sql.NullInt64
	var isActive int
	var allowedModelsJSON string
	var costUsedMicroUSD int64
	var costLimitMicroUSD int64

	if err := scanner.Scan(
		&token.ID,
		&token.Token,
		&token.Description,
		&createdAtMs,
		&expiresAt,
		&lastUsedAt,
		&isActive,
		&token.SuccessCount,
		&token.FailureCount,
		&token.StreamAvgTTFB,
		&token.NonStreamAvgRT,
		&token.StreamCount,
		&token.NonStreamCount,
		&token.PromptTokensTotal,
		&token.CompletionTokensTotal,
		&token.CacheReadTokensTotal,
		&token.CacheCreationTokensTotal,
		&token.TotalCostUSD,
		&costUsedMicroUSD,
		&costLimitMicroUSD,
		&allowedModelsJSON,
	); err != nil {
		return nil, err
	}

	token.CreatedAt = time.UnixMilli(createdAtMs)
	if expiresAt.Valid {
		// 语义：0 表示永不过期；对外保持 nil（omitempty）更干净
		if expiresAt.Int64 > 0 {
			v := expiresAt.Int64
			token.ExpiresAt = &v
		}
	}
	if lastUsedAt.Valid {
		// 语义：0 表示从未使用过；对外保持 nil（omitempty）
		if lastUsedAt.Int64 > 0 {
			v := lastUsedAt.Int64
			token.LastUsedAt = &v
		}
	}
	token.IsActive = isActive != 0
	token.CostUsedMicroUSD = costUsedMicroUSD
	token.CostLimitMicroUSD = costLimitMicroUSD

	// 解析 allowed_models JSON
	if allowedModelsJSON != "" {
		if err := json.Unmarshal([]byte(allowedModelsJSON), &token.AllowedModels); err != nil {
			// 解析失败则忽略，视为无限制
			token.AllowedModels = nil
		}
	}

	return token, nil
}

// UpsertAuthTokenAllFields 用于混合存储/恢复场景：按既有 id 写入完整行，保证两端数据一致。
// 注意：这不是常规业务写路径，调用方必须确保 token.Token 已是哈希值而非明文。
func (s *SQLStore) UpsertAuthTokenAllFields(ctx context.Context, token *model.AuthToken) error {
	if token == nil {
		return errors.New("token cannot be nil")
	}
	if token.ID == 0 {
		return errors.New("token id cannot be 0")
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}

	expiresAt := int64(0)
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}
	lastUsedAt := int64(0)
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	var allowedModelsJSON string
	if len(token.AllowedModels) > 0 {
		if data, err := json.Marshal(token.AllowedModels); err == nil {
			allowedModelsJSON = string(data)
		}
	}

	if s.IsSQLite() {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO auth_tokens (
				id, token, description, created_at, expires_at, last_used_at, is_active,
				success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
				prompt_tokens_total, completion_tokens_total, cache_read_tokens_total, cache_creation_tokens_total, total_cost_usd,
				cost_used_microusd, cost_limit_microusd, allowed_models
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				token = excluded.token,
				description = excluded.description,
				created_at = excluded.created_at,
				expires_at = excluded.expires_at,
				last_used_at = excluded.last_used_at,
				is_active = excluded.is_active,
				success_count = excluded.success_count,
				failure_count = excluded.failure_count,
				stream_avg_ttfb = excluded.stream_avg_ttfb,
				non_stream_avg_rt = excluded.non_stream_avg_rt,
				stream_count = excluded.stream_count,
				non_stream_count = excluded.non_stream_count,
				prompt_tokens_total = excluded.prompt_tokens_total,
				completion_tokens_total = excluded.completion_tokens_total,
				cache_read_tokens_total = excluded.cache_read_tokens_total,
				cache_creation_tokens_total = excluded.cache_creation_tokens_total,
				total_cost_usd = excluded.total_cost_usd,
				cost_used_microusd = excluded.cost_used_microusd,
				cost_limit_microusd = excluded.cost_limit_microusd,
				allowed_models = excluded.allowed_models
		`,
			token.ID,
			token.Token,
			token.Description,
			token.CreatedAt.UnixMilli(),
			expiresAt,
			lastUsedAt,
			boolToInt(token.IsActive),
			token.SuccessCount,
			token.FailureCount,
			token.StreamAvgTTFB,
			token.NonStreamAvgRT,
			token.StreamCount,
			token.NonStreamCount,
			token.PromptTokensTotal,
			token.CompletionTokensTotal,
			token.CacheReadTokensTotal,
			token.CacheCreationTokensTotal,
			token.TotalCostUSD,
			token.CostUsedMicroUSD,
			token.CostLimitMicroUSD,
			allowedModelsJSON,
		)
		if err != nil {
			return fmt.Errorf("upsert auth token all fields: %w", err)
		}
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (
			id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, cache_read_tokens_total, cache_creation_tokens_total, total_cost_usd,
			cost_used_microusd, cost_limit_microusd, allowed_models
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			token = VALUES(token),
			description = VALUES(description),
			created_at = VALUES(created_at),
			expires_at = VALUES(expires_at),
			last_used_at = VALUES(last_used_at),
			is_active = VALUES(is_active),
			success_count = VALUES(success_count),
			failure_count = VALUES(failure_count),
			stream_avg_ttfb = VALUES(stream_avg_ttfb),
			non_stream_avg_rt = VALUES(non_stream_avg_rt),
			stream_count = VALUES(stream_count),
			non_stream_count = VALUES(non_stream_count),
			prompt_tokens_total = VALUES(prompt_tokens_total),
			completion_tokens_total = VALUES(completion_tokens_total),
			cache_read_tokens_total = VALUES(cache_read_tokens_total),
			cache_creation_tokens_total = VALUES(cache_creation_tokens_total),
			total_cost_usd = VALUES(total_cost_usd),
			cost_used_microusd = VALUES(cost_used_microusd),
			cost_limit_microusd = VALUES(cost_limit_microusd),
			allowed_models = VALUES(allowed_models)
	`,
		token.ID,
		token.Token,
		token.Description,
		token.CreatedAt.UnixMilli(),
		expiresAt,
		lastUsedAt,
		boolToInt(token.IsActive),
		token.SuccessCount,
		token.FailureCount,
		token.StreamAvgTTFB,
		token.NonStreamAvgRT,
		token.StreamCount,
		token.NonStreamCount,
		token.PromptTokensTotal,
		token.CompletionTokensTotal,
		token.CacheReadTokensTotal,
		token.CacheCreationTokensTotal,
		token.TotalCostUSD,
		token.CostUsedMicroUSD,
		token.CostLimitMicroUSD,
		allowedModelsJSON,
	)
	if err != nil {
		return fmt.Errorf("upsert auth token all fields: %w", err)
	}
	return nil
}

// ============================================================================
// Auth Tokens Management - API访问令牌管理
// ============================================================================

// CreateAuthToken 创建新的API访问令牌
// 注意: token字段存储的是SHA256哈希值，而非明文
func (s *SQLStore) CreateAuthToken(ctx context.Context, token *model.AuthToken) error {
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}

	// 处理可空字段：SQLite NOT NULL DEFAULT 0 需要传入 0 而不是 nil
	var expiresAt int64
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}

	var lastUsedAt int64
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	// 序列化 allowed_models 为 JSON
	var allowedModelsJSON string
	if len(token.AllowedModels) > 0 {
		if data, err := json.Marshal(token.AllowedModels); err == nil {
			allowedModelsJSON = string(data)
		}
	}

	if token.ID != 0 {
		if s.IsSQLite() {
			_, err := s.db.ExecContext(ctx, `
				INSERT INTO auth_tokens (
					id,
					token, description, created_at, expires_at, last_used_at, is_active,
					success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
					prompt_tokens_total, completion_tokens_total, total_cost_usd, allowed_models,
					cost_used_microusd, cost_limit_microusd
				)
				VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0.0, 0.0, 0, 0, 0, 0, 0.0, ?, 0, ?)
			`, token.ID, token.Token, token.Description, token.CreatedAt.UnixMilli(), expiresAt, lastUsedAt, boolToInt(token.IsActive), allowedModelsJSON, token.CostLimitMicroUSD)
			if err != nil {
				return fmt.Errorf("create auth token: %w", err)
			}
			return nil
		}

		_, err := s.db.ExecContext(ctx, `
			INSERT INTO auth_tokens (
				id,
				token, description, created_at, expires_at, last_used_at, is_active,
				success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
				prompt_tokens_total, completion_tokens_total, total_cost_usd, allowed_models,
				cost_used_microusd, cost_limit_microusd
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0.0, 0.0, 0, 0, 0, 0, 0.0, ?, 0, ?)
			ON DUPLICATE KEY UPDATE id = id
		`, token.ID, token.Token, token.Description, token.CreatedAt.UnixMilli(), expiresAt, lastUsedAt, boolToInt(token.IsActive), allowedModelsJSON, token.CostLimitMicroUSD)
		if err != nil {
			return fmt.Errorf("create auth token: %w", err)
		}
		return nil
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (
			token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd, allowed_models,
			cost_used_microusd, cost_limit_microusd
		)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0.0, 0.0, 0, 0, 0, 0, 0.0, ?, 0, ?)
	`, token.Token, token.Description, token.CreatedAt.UnixMilli(), expiresAt, lastUsedAt, boolToInt(token.IsActive), allowedModelsJSON, token.CostLimitMicroUSD)

	if err != nil {
		return fmt.Errorf("create auth token: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}

	token.ID = id

	return nil
}

// GetAuthToken 根据ID获取令牌
func (s *SQLStore) GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error) {
	token, err := scanAuthToken(s.db.QueryRowContext(
		ctx,
		fmt.Sprintf("SELECT %s FROM auth_tokens WHERE id = ?", authTokenSelectColumns),
		id,
	))

	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}

	return token, nil
}

// GetAuthTokenByValue 根据令牌哈希值获取令牌信息
// 用于认证时快速查找令牌
func (s *SQLStore) GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error) {
	token, err := scanAuthToken(s.db.QueryRowContext(
		ctx,
		fmt.Sprintf("SELECT %s FROM auth_tokens WHERE token = ?", authTokenSelectColumns),
		tokenHash,
	))

	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token by value: %w", err)
	}

	return token, nil
}

// ListAuthTokens 列出所有令牌
func (s *SQLStore) ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT %s FROM auth_tokens ORDER BY created_at DESC",
		authTokenSelectColumns,
	))
	if err != nil {
		return nil, fmt.Errorf("list auth tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []*model.AuthToken
	for rows.Next() {
		token, err := scanAuthToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan auth token: %w", err)
		}

		tokens = append(tokens, token)
	}

	return tokens, rows.Err()
}

// ListActiveAuthTokens 列出所有有效的令牌
// 用于热更新AuthService的令牌缓存
func (s *SQLStore) ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	now := time.Now().UnixMilli()

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT %s FROM auth_tokens WHERE is_active = 1 AND (expires_at = 0 OR expires_at > ?) ORDER BY created_at DESC",
		authTokenSelectColumns,
	), now)
	if err != nil {
		return nil, fmt.Errorf("list active auth tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []*model.AuthToken
	for rows.Next() {
		token, err := scanAuthToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan auth token: %w", err)
		}

		tokens = append(tokens, token)
	}

	return tokens, rows.Err()
}

// UpdateAuthToken 更新令牌信息
func (s *SQLStore) UpdateAuthToken(ctx context.Context, token *model.AuthToken) error {
	var expiresAt any = int64(0)
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}

	var lastUsedAt any = int64(0)
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	// 序列化 allowed_models 为 JSON
	var allowedModelsJSON string
	if len(token.AllowedModels) > 0 {
		if data, err := json.Marshal(token.AllowedModels); err == nil {
			allowedModelsJSON = string(data)
		}
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens
		SET description = ?,
		    expires_at = ?,
		    last_used_at = ?,
		    is_active = ?,
		    cost_limit_microusd = ?,
		    allowed_models = ?
		WHERE id = ?
	`, token.Description, expiresAt, lastUsedAt, boolToInt(token.IsActive), token.CostLimitMicroUSD, allowedModelsJSON, token.ID)

	if err != nil {
		return fmt.Errorf("update auth token: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("auth token not found")
	}

	return nil
}

// DeleteAuthToken 删除令牌
func (s *SQLStore) DeleteAuthToken(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM auth_tokens WHERE id = ?
	`, id)

	if err != nil {
		return fmt.Errorf("delete auth token: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("auth token not found")
	}

	return nil
}

// UpdateTokenLastUsed 更新令牌最后使用时间
// 异步调用，性能优化
func (s *SQLStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens
		SET last_used_at = ?
		WHERE token = ?
	`, now.UnixMilli(), tokenHash)

	if err != nil {
		return fmt.Errorf("update token last used: %w", err)
	}

	return nil
}

// UpdateTokenStats 增量更新Token统计信息
// 使用事务保证原子性，采用增量计算公式避免扫描历史数据
// 参数:
//   - tokenHash: Token的SHA256哈希值
//   - isSuccess: 本次请求是否成功(2xx状态码)
//   - duration: 总响应时间(秒)
//   - isStreaming: 是否为流式请求
//   - firstByteTime: 流式请求的首字节时间(秒)，非流式时为0
//   - promptTokens: 输入token数量
//   - completionTokens: 输出token数量
//   - costUSD: 本次请求费用(美元)
func (s *SQLStore) UpdateTokenStats(
	ctx context.Context,
	tokenHash string,
	isSuccess bool,
	duration float64,
	isStreaming bool,
	firstByteTime float64,
	promptTokens int64,
	completionTokens int64,
	cacheReadTokens int64,
	cacheCreationTokens int64,
	costUSD float64,
) error {
	// 单条 UPDATE 保证原子性：避免每次请求都做 BEGIN+SELECT+UPDATE+COMMIT
	// 这对 SQLite（减少写锁持有时间/往返）和 MySQL（减少往返/行锁竞争）都更友好。
	successFlag := boolToInt(isSuccess)
	failureFlag := boolToInt(!isSuccess)
	streamUpdateFlag := boolToInt(isStreaming && firstByteTime > 0)
	nonStreamUpdateFlag := boolToInt(!isStreaming)
	nowMs := time.Now().UnixMilli()
	costMicroUSD := util.USDToMicroUSD(costUSD)

	result, err := s.db.ExecContext(ctx, updateTokenStatsQuery,
		successFlag,
		failureFlag,
		successFlag, promptTokens,
		successFlag, completionTokens,
		successFlag, cacheReadTokens,
		successFlag, cacheCreationTokens,
		successFlag, costUSD,
		successFlag, costMicroUSD,
		streamUpdateFlag, firstByteTime,
		streamUpdateFlag,
		nonStreamUpdateFlag, duration,
		nonStreamUpdateFlag,
		nowMs,
		tokenHash,
	)
	if err != nil {
		return fmt.Errorf("update stats: %w", err)
	}

	// 兼容性：少数驱动可能不支持 RowsAffected，这里尽力检查
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("token not found: %s", tokenHash)
	}

	return nil
}
