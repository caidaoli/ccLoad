package sql

import (
	"context"
	"time"
)

// CreateAdminSession 创建管理员会话
func (s *SQLStore) CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error {
	now := timeToUnix(time.Now())
	_, err := s.db.ExecContext(ctx, `
		REPLACE INTO admin_sessions (token, expires_at, created_at)
		VALUES (?, ?, ?)
	`, token, timeToUnix(expiresAt), now)
	return err
}

// GetAdminSession 获取管理员会话
func (s *SQLStore) GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error) {
	var expiresUnix int64
	err = s.db.QueryRowContext(ctx, `
		SELECT expires_at FROM admin_sessions WHERE token = ?
	`, token).Scan(&expiresUnix)

	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}

	return unixToTime(expiresUnix), true, nil
}

// DeleteAdminSession 删除管理员会话
func (s *SQLStore) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

// CleanExpiredSessions 清理过期的会话
func (s *SQLStore) CleanExpiredSessions(ctx context.Context) error {
	now := timeToUnix(time.Now())
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, now)
	return err
}

// LoadAllSessions 加载所有未过期的会话（启动时调用）
func (s *SQLStore) LoadAllSessions(ctx context.Context) (map[string]time.Time, error) {
	now := timeToUnix(time.Now())
	rows, err := s.db.QueryContext(ctx, `
		SELECT token, expires_at FROM admin_sessions WHERE expires_at > ?
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make(map[string]time.Time)
	for rows.Next() {
		var token string
		var expiresUnix int64
		if err := rows.Scan(&token, &expiresUnix); err != nil {
			return nil, err
		}
		sessions[token] = unixToTime(expiresUnix)
	}

	return sessions, rows.Err()
}
