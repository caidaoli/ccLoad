package mysql

import (
	"context"
	"time"
)

// CreateAdminSession 创建管理员会话（MySQL: REPLACE INTO）
func (s *MySQLStore) CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		REPLACE INTO admin_sessions (token, expires_at, created_at)
		VALUES (?, ?, ?)
	`, token, expiresAt.Unix(), now)
	return err
}

func (s *MySQLStore) GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error) {
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

	return time.Unix(expiresUnix, 0), true, nil
}

func (s *MySQLStore) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

func (s *MySQLStore) CleanExpiredSessions(ctx context.Context) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, now)
	return err
}

func (s *MySQLStore) LoadAllSessions(ctx context.Context) (map[string]time.Time, error) {
	now := time.Now().Unix()
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
		sessions[token] = time.Unix(expiresUnix, 0)
	}

	return sessions, rows.Err()
}
