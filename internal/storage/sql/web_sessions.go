package sql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ccLoad/internal/model"
)

// CreateWebSession persists a hashed role-aware browser session.
func (s *SQLStore) CreateWebSession(ctx context.Context, token string, session model.WebSession) error {
	tokenHash := model.HashToken(token)
	_, err := s.db.ExecContext(ctx, `
		REPLACE INTO web_sessions (token_hash, role, auth_token_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, tokenHash, session.Role, session.AuthTokenID, timeToUnix(session.ExpiresAt), timeToUnix(time.Now()))
	return err
}

// GetWebSession retrieves a browser session by its plaintext bearer value.
func (s *SQLStore) GetWebSession(ctx context.Context, token string) (model.WebSession, bool, error) {
	tokenHash := model.HashToken(token)
	var session model.WebSession
	var expiresUnix int64
	err := s.db.QueryRowContext(ctx, `
		SELECT token_hash, role, auth_token_id, expires_at
		FROM web_sessions
		WHERE token_hash = ?
	`, tokenHash).Scan(&session.TokenHash, &session.Role, &session.AuthTokenID, &expiresUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return model.WebSession{}, false, nil
	}
	if err != nil {
		return model.WebSession{}, false, err
	}
	session.ExpiresAt = unixToTime(expiresUnix)
	return session, true, nil
}

// DeleteWebSession deletes a browser session by its plaintext bearer value.
func (s *SQLStore) DeleteWebSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash = ?`, model.HashToken(token))
	return err
}

// DeleteWebSessionsByAuthTokenID irreversibly revokes browser sessions for an API token.
func (s *SQLStore) DeleteWebSessionsByAuthTokenID(ctx context.Context, authTokenID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE auth_token_id = ?`, authTokenID)
	return err
}

// CleanExpiredWebSessions deletes expired browser sessions.
func (s *SQLStore) CleanExpiredWebSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at < ?`, timeToUnix(time.Now()))
	return err
}

// LoadWebSessions loads all unexpired browser sessions keyed by token hash.
func (s *SQLStore) LoadWebSessions(ctx context.Context) (map[string]model.WebSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT token_hash, role, auth_token_id, expires_at
		FROM web_sessions
		WHERE expires_at > ?
	`, timeToUnix(time.Now()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	sessions := make(map[string]model.WebSession)
	for rows.Next() {
		var session model.WebSession
		var expiresUnix int64
		if err := rows.Scan(&session.TokenHash, &session.Role, &session.AuthTokenID, &expiresUnix); err != nil {
			return nil, err
		}
		session.ExpiresAt = unixToTime(expiresUnix)
		sessions[session.TokenHash] = session
	}
	return sessions, rows.Err()
}
