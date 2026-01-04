// Package sql 提供基于 SQL 的数据存储实现。
// 支持 SQLite 和 MySQL 两种后端，实现统一的 storage.Store 接口。
package sql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ccLoad/internal/model"
)

// CreateAdminSession 创建管理员会话
// [INFO] 安全修复：存储token的SHA256哈希而非明文(2025-12)
func (s *SQLStore) CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error {
	tokenHash := model.HashToken(token)
	now := timeToUnix(time.Now())
	_, err := s.db.ExecContext(ctx, `
		REPLACE INTO admin_sessions (token, expires_at, created_at)
		VALUES (?, ?, ?)
	`, tokenHash, timeToUnix(expiresAt), now)
	return err
}

// GetAdminSession 获取管理员会话
// [INFO] 安全修复：通过token哈希查询(2025-12)
func (s *SQLStore) GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error) {
	tokenHash := model.HashToken(token)
	var expiresUnix int64
	err = s.db.QueryRowContext(ctx, `
		SELECT expires_at FROM admin_sessions WHERE token = ?
	`, tokenHash).Scan(&expiresUnix)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}

	return unixToTime(expiresUnix), true, nil
}

// DeleteAdminSession 删除管理员会话
// [INFO] 安全修复：通过token哈希删除(2025-12)
func (s *SQLStore) DeleteAdminSession(ctx context.Context, token string) error {
	tokenHash := model.HashToken(token)
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, tokenHash)
	return err
}

// CleanExpiredSessions 清理过期的会话
func (s *SQLStore) CleanExpiredSessions(ctx context.Context) error {
	now := timeToUnix(time.Now())
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, now)
	return err
}

// LoadAllSessions 加载所有未过期的会话（启动时调用）
// [INFO] 安全修复：返回tokenHash→expiry映射(2025-12)
func (s *SQLStore) LoadAllSessions(ctx context.Context) (map[string]time.Time, error) {
	now := timeToUnix(time.Now())
	rows, err := s.db.QueryContext(ctx, `
		SELECT token, expires_at FROM admin_sessions WHERE expires_at > ?
	`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	sessions := make(map[string]time.Time)
	for rows.Next() {
		var tokenHash string
		var expiresUnix int64
		if err := rows.Scan(&tokenHash, &expiresUnix); err != nil {
			return nil, err
		}
		sessions[tokenHash] = unixToTime(expiresUnix)
	}

	return sessions, rows.Err()
}
