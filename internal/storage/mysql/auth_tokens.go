package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

func (s *MySQLStore) CreateAuthToken(ctx context.Context, token *model.AuthToken) error {
	token.CreatedAt = time.Now()

	// 处理可空字段：MySQL NOT NULL 需要传入 0 而不是 nil
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

	id, _ := result.LastInsertId()
	token.ID = id

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error) {
	token := &model.AuthToken{}
	var createdAtMs int64
	var expiresAt, lastUsedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens WHERE id = ?
	`, id).Scan(
		&token.ID, &token.Token, &token.Description, &createdAtMs, &expiresAt, &lastUsedAt, &token.IsActive,
		&token.SuccessCount, &token.FailureCount, &token.StreamAvgTTFB, &token.NonStreamAvgRT,
		&token.StreamCount, &token.NonStreamCount, &token.PromptTokensTotal, &token.CompletionTokensTotal, &token.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}

	token.CreatedAt = time.UnixMilli(createdAtMs)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Int64
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Int64
	}

	return token, nil
}

func (s *MySQLStore) GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error) {
	token := &model.AuthToken{}
	var createdAtMs int64
	var expiresAt, lastUsedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens WHERE token = ?
	`, tokenHash).Scan(
		&token.ID, &token.Token, &token.Description, &createdAtMs, &expiresAt, &lastUsedAt, &token.IsActive,
		&token.SuccessCount, &token.FailureCount, &token.StreamAvgTTFB, &token.NonStreamAvgRT,
		&token.StreamCount, &token.NonStreamCount, &token.PromptTokensTotal, &token.CompletionTokensTotal, &token.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("auth token not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get auth token by value: %w", err)
	}

	token.CreatedAt = time.UnixMilli(createdAtMs)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Int64
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Int64
	}

	return token, nil
}

func (s *MySQLStore) ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list auth tokens: %w", err)
	}
	defer rows.Close()

	return scanAuthTokens(rows)
}

func (s *MySQLStore) ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	now := time.Now().UnixMilli()

	// expires_at = 0 表示永不过期，与 NULL 同等处理
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active,
			success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens
		WHERE is_active = 1 AND (expires_at IS NULL OR expires_at = 0 OR expires_at > ?)
		ORDER BY created_at DESC
	`, now)
	if err != nil {
		return nil, fmt.Errorf("list active auth tokens: %w", err)
	}
	defer rows.Close()

	return scanAuthTokens(rows)
}

func scanAuthTokens(rows *sql.Rows) ([]*model.AuthToken, error) {
	var tokens []*model.AuthToken
	for rows.Next() {
		token := &model.AuthToken{}
		var createdAtMs int64
		var expiresAt, lastUsedAt sql.NullInt64

		if err := rows.Scan(
			&token.ID, &token.Token, &token.Description, &createdAtMs, &expiresAt, &lastUsedAt, &token.IsActive,
			&token.SuccessCount, &token.FailureCount, &token.StreamAvgTTFB, &token.NonStreamAvgRT,
			&token.StreamCount, &token.NonStreamCount, &token.PromptTokensTotal, &token.CompletionTokensTotal, &token.TotalCostUSD,
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

	return tokens, rows.Err()
}

func (s *MySQLStore) UpdateAuthToken(ctx context.Context, token *model.AuthToken) error {
	var expiresAt, lastUsedAt any
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens SET description = ?, expires_at = ?, last_used_at = ?, is_active = ? WHERE id = ?
	`, token.Description, expiresAt, lastUsedAt, token.IsActive, token.ID)

	if err != nil {
		return fmt.Errorf("update auth token: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("auth token not found")
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) DeleteAuthToken(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete auth token: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("auth token not found")
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET last_used_at = ? WHERE token = ?`, now.UnixMilli(), tokenHash)
	return err
}

func (s *MySQLStore) UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, costUSD float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var stats struct {
		SuccessCount, FailureCount, StreamCount, NonStreamCount, PromptTokensTotal, CompletionTokensTotal int64
		StreamAvgTTFB, NonStreamAvgRT, TotalCostUSD                                                       float64
	}

	err = tx.QueryRowContext(ctx, `
		SELECT success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt, stream_count, non_stream_count,
			prompt_tokens_total, completion_tokens_total, total_cost_usd
		FROM auth_tokens WHERE token = ?
	`, tokenHash).Scan(
		&stats.SuccessCount, &stats.FailureCount, &stats.StreamAvgTTFB, &stats.NonStreamAvgRT,
		&stats.StreamCount, &stats.NonStreamCount, &stats.PromptTokensTotal, &stats.CompletionTokensTotal, &stats.TotalCostUSD,
	)

	if err == sql.ErrNoRows {
		return fmt.Errorf("token not found: %s", tokenHash)
	}
	if err != nil {
		return fmt.Errorf("query current stats: %w", err)
	}

	if isSuccess {
		stats.SuccessCount++
		stats.PromptTokensTotal += promptTokens
		stats.CompletionTokensTotal += completionTokens
		stats.TotalCostUSD += costUSD
	} else {
		stats.FailureCount++
	}

	if isStreaming && firstByteTime > 0 {
		stats.StreamAvgTTFB = ((stats.StreamAvgTTFB * float64(stats.StreamCount)) + firstByteTime) / float64(stats.StreamCount+1)
		stats.StreamCount++
	} else if !isStreaming {
		stats.NonStreamAvgRT = ((stats.NonStreamAvgRT * float64(stats.NonStreamCount)) + duration) / float64(stats.NonStreamCount+1)
		stats.NonStreamCount++
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE auth_tokens SET
			success_count = ?, failure_count = ?, stream_avg_ttfb = ?, non_stream_avg_rt = ?,
			stream_count = ?, non_stream_count = ?, prompt_tokens_total = ?, completion_tokens_total = ?,
			total_cost_usd = ?, last_used_at = ?
		WHERE token = ?
	`, stats.SuccessCount, stats.FailureCount, stats.StreamAvgTTFB, stats.NonStreamAvgRT,
		stats.StreamCount, stats.NonStreamCount, stats.PromptTokensTotal, stats.CompletionTokensTotal,
		stats.TotalCostUSD, time.Now().UnixMilli(), tokenHash)

	if err != nil {
		return fmt.Errorf("update stats: %w", err)
	}

	return tx.Commit()
}
