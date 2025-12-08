package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

// ============================================================================
// Auth Tokens Management - API访问令牌管理
// ============================================================================

// CreateAuthToken 创建新的API访问令牌
// 注意: token字段存储的是SHA256哈希值，而非明文
func (s *SQLiteStore) CreateAuthToken(ctx context.Context, token *model.AuthToken) error {
	token.CreatedAt = time.Now()

	// 处理可空字段：SQLite NOT NULL DEFAULT 0 需要传入 0 而不是 nil
	var expiresAt int64 = 0
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}

	var lastUsedAt int64 = 0
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (
			token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0.0, 0.0, 0, 0, 0, 0, 0.0)
	`, token.Token, token.Description, token.CreatedAt.UnixMilli(), expiresAt, lastUsedAt, token.IsActive)

	if err != nil {
		return fmt.Errorf("create auth token: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}

	token.ID = id

	// 触发异步Redis同步 (新增 2025-11)
	s.triggerAsyncSync()

	return nil
}

// GetAuthToken 根据ID获取令牌
func (s *SQLiteStore) GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error) {
	token := &model.AuthToken{}
	var createdAtMs int64
	var expiresAt, lastUsedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT
			id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		WHERE id = ?
	`, id).Scan(
		&token.ID,
		&token.Token,
		&token.Description,
		&createdAtMs,
		&expiresAt,
		&lastUsedAt,
		&token.IsActive,
		&token.SuccessCount,
		&token.FailureCount,
		&token.StreamAvgTTFB,
		&token.NonStreamAvgRT,
		&token.StreamCount,
		&token.NonStreamCount,
		&token.PromptTokensTotal,
		&token.CompletionTokensTotal,
		&token.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}

	// 转换时间戳
	token.CreatedAt = time.UnixMilli(createdAtMs)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Int64
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Int64
	}

	return token, nil
}

// GetAuthTokenByValue 根据令牌哈希值获取令牌信息
// 用于认证时快速查找令牌
func (s *SQLiteStore) GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error) {
	token := &model.AuthToken{}
	var createdAtMs int64
	var expiresAt, lastUsedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT
			id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		WHERE token = ?
	`, tokenHash).Scan(
		&token.ID,
		&token.Token,
		&token.Description,
		&createdAtMs,
		&expiresAt,
		&lastUsedAt,
		&token.IsActive,
		&token.SuccessCount,
		&token.FailureCount,
		&token.StreamAvgTTFB,
		&token.NonStreamAvgRT,
		&token.StreamCount,
		&token.NonStreamCount,
		&token.PromptTokensTotal,
		&token.CompletionTokensTotal,
		&token.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token by value: %w", err)
	}

	// 转换时间戳
	token.CreatedAt = time.UnixMilli(createdAtMs)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Int64
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Int64
	}

	return token, nil
}

// ListAuthTokens 列出所有令牌
func (s *SQLiteStore) ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list auth tokens: %w", err)
	}
	defer rows.Close()

	var tokens []*model.AuthToken
	for rows.Next() {
		token := &model.AuthToken{}
		var createdAtMs int64
		var expiresAt, lastUsedAt sql.NullInt64

		if err := rows.Scan(
			&token.ID,
			&token.Token,
			&token.Description,
			&createdAtMs,
			&expiresAt,
			&lastUsedAt,
			&token.IsActive,
			&token.SuccessCount,
			&token.FailureCount,
			&token.StreamAvgTTFB,
			&token.NonStreamAvgRT,
			&token.StreamCount,
			&token.NonStreamCount,
			&token.PromptTokensTotal,
			&token.CompletionTokensTotal,
			&token.TotalCostUSD,
		); err != nil {
			return nil, fmt.Errorf("scan auth token: %w", err)
		}

		token.CreatedAt = time.UnixMilli(createdAtMs)
		if expiresAt.Valid {
			token.ExpiresAt = &expiresAt.Int64
		}
		if lastUsedAt.Valid {
			token.LastUsedAt = &lastUsedAt.Int64
		}

		tokens = append(tokens, token)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auth tokens: %w", err)
	}

	return tokens, nil
}

// ListActiveAuthTokens 列出所有有效的令牌
// 用于热更新AuthService的令牌缓存
func (s *SQLiteStore) ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	now := time.Now().UnixMilli()

	// expires_at = 0 表示永不过期，与 NULL 同等处理
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		WHERE is_active = 1
		  AND (expires_at IS NULL OR expires_at = 0 OR expires_at > ?)
		ORDER BY created_at DESC
	`, now)
	if err != nil {
		return nil, fmt.Errorf("list active auth tokens: %w", err)
	}
	defer rows.Close()

	var tokens []*model.AuthToken
	for rows.Next() {
		token := &model.AuthToken{}
		var createdAtMs int64
		var expiresAt, lastUsedAt sql.NullInt64

		if err := rows.Scan(
			&token.ID,
			&token.Token,
			&token.Description,
			&createdAtMs,
			&expiresAt,
			&lastUsedAt,
			&token.IsActive,
			&token.SuccessCount,
			&token.FailureCount,
			&token.StreamAvgTTFB,
			&token.NonStreamAvgRT,
			&token.StreamCount,
			&token.NonStreamCount,
			&token.PromptTokensTotal,
			&token.CompletionTokensTotal,
			&token.TotalCostUSD,
		); err != nil {
			return nil, fmt.Errorf("scan active auth token: %w", err)
		}

		token.CreatedAt = time.UnixMilli(createdAtMs)
		if expiresAt.Valid {
			token.ExpiresAt = &expiresAt.Int64
		}
		if lastUsedAt.Valid {
			token.LastUsedAt = &lastUsedAt.Int64
		}

		tokens = append(tokens, token)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active auth tokens: %w", err)
	}

	return tokens, nil
}

// UpdateAuthToken 更新令牌信息
func (s *SQLiteStore) UpdateAuthToken(ctx context.Context, token *model.AuthToken) error {
	var expiresAt any
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}

	var lastUsedAt any
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens
		SET description = ?,
		    expires_at = ?,
		    last_used_at = ?,
		    is_active = ?
		WHERE id = ?
	`, token.Description, expiresAt, lastUsedAt, token.IsActive, token.ID)

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

	// 触发异步Redis同步 (新增 2025-11)
	s.triggerAsyncSync()

	return nil
}

// DeleteAuthToken 删除令牌
func (s *SQLiteStore) DeleteAuthToken(ctx context.Context, id int64) error {
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

	// 触发异步Redis同步 (新增 2025-11)
	s.triggerAsyncSync()

	return nil
}

// UpdateTokenLastUsed 更新令牌最后使用时间
// 异步调用，性能优化
func (s *SQLiteStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
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
func (s *SQLiteStore) UpdateTokenStats(
	ctx context.Context,
	tokenHash string,
	isSuccess bool,
	duration float64,
	isStreaming bool,
	firstByteTime float64,
	promptTokens int64,
	completionTokens int64,
	costUSD float64,
) error {
	// 使用事务保证原子性（读-计算-写）
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() // 失败时自动回滚

	// 1. 查询当前统计数据
	var stats struct {
		SuccessCount          int64
		FailureCount          int64
		StreamAvgTTFB         float64
		NonStreamAvgRT        float64
		StreamCount           int64
		NonStreamCount        int64
		PromptTokensTotal     int64
		CompletionTokensTotal int64
		TotalCostUSD          float64
	}

	err = tx.QueryRowContext(ctx, `
		SELECT
			success_count, failure_count,
			stream_avg_ttfb, non_stream_avg_rt,
			stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		WHERE token = ?
	`, tokenHash).Scan(
		&stats.SuccessCount,
		&stats.FailureCount,
		&stats.StreamAvgTTFB,
		&stats.NonStreamAvgRT,
		&stats.StreamCount,
		&stats.NonStreamCount,
		&stats.PromptTokensTotal,
		&stats.CompletionTokensTotal,
		&stats.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return fmt.Errorf("token not found: %s", tokenHash)
	}
	if err != nil {
		return fmt.Errorf("query current stats: %w", err)
	}

	// 2. 增量更新计数器
	if isSuccess {
		stats.SuccessCount++
		// 只有成功请求才累加token和费用
		stats.PromptTokensTotal += promptTokens
		stats.CompletionTokensTotal += completionTokens
		stats.TotalCostUSD += costUSD
	} else {
		stats.FailureCount++
	}

	// 3. 增量更新平均值（使用累加公式避免扫描历史数据）
	// 公式: new_avg = (old_avg * old_count + new_value) / (old_count + 1)
	if isStreaming && firstByteTime > 0 {
		// 流式请求：更新平均首字节时间
		stats.StreamAvgTTFB = ((stats.StreamAvgTTFB * float64(stats.StreamCount)) + firstByteTime) / float64(stats.StreamCount+1)
		stats.StreamCount++
	} else if !isStreaming {
		// 非流式请求：更新平均响应时间
		stats.NonStreamAvgRT = ((stats.NonStreamAvgRT * float64(stats.NonStreamCount)) + duration) / float64(stats.NonStreamCount+1)
		stats.NonStreamCount++
	}

	// 4. 写回数据库
	_, err = tx.ExecContext(ctx, `
		UPDATE auth_tokens
		SET
			success_count = ?,
			failure_count = ?,
			stream_avg_ttfb = ?,
			non_stream_avg_rt = ?,
			stream_count = ?,
			non_stream_count = ?,
			prompt_tokens_total = ?,
			completion_tokens_total = ?,
			total_cost_usd = ?,
			last_used_at = ?
		WHERE token = ?
	`,
		stats.SuccessCount,
		stats.FailureCount,
		stats.StreamAvgTTFB,
		stats.NonStreamAvgRT,
		stats.StreamCount,
		stats.NonStreamCount,
		stats.PromptTokensTotal,
		stats.CompletionTokensTotal,
		stats.TotalCostUSD,
		time.Now().UnixMilli(),
		tokenHash,
	)

	if err != nil {
		return fmt.Errorf("update stats: %w", err)
	}

	// 5. 提交事务
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}
