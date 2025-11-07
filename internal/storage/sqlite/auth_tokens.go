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

	var expiresAt any
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}

	var lastUsedAt any
	if token.LastUsedAt != nil {
		lastUsedAt = *token.LastUsedAt
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (token, description, created_at, expires_at, last_used_at, is_active)
		VALUES (?, ?, ?, ?, ?, ?)
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
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active
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
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active
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
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active
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

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token, description, created_at, expires_at, last_used_at, is_active
		FROM auth_tokens
		WHERE is_active = 1
		  AND (expires_at IS NULL OR expires_at > ?)
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
