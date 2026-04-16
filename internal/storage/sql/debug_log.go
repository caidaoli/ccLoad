package sql

import (
	"context"
	"database/sql"
	"time"

	"ccLoad/internal/model"
)

// AddDebugLog 插入一条调试日志
func (s *SQLStore) AddDebugLog(ctx context.Context, e *model.DebugLogEntry) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO debug_logs (log_id, created_at, req_method, req_url, req_headers, req_body, resp_status, resp_headers, resp_body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.LogID, e.CreatedAt, e.ReqMethod, e.ReqURL, e.ReqHeaders, e.ReqBody, e.RespStatus, e.RespHeaders, e.RespBody,
	)
	return err
}

// GetDebugLogByLogID 根据 log_id 查询调试日志
func (s *SQLStore) GetDebugLogByLogID(ctx context.Context, logID int64) (*model.DebugLogEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, log_id, created_at, req_method, req_url, req_headers, req_body, resp_status, resp_headers, resp_body
		FROM debug_logs WHERE log_id = ? LIMIT 1`, logID)

	var e model.DebugLogEntry
	err := row.Scan(&e.ID, &e.LogID, &e.CreatedAt, &e.ReqMethod, &e.ReqURL, &e.ReqHeaders, &e.ReqBody, &e.RespStatus, &e.RespHeaders, &e.RespBody)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// CleanupDebugLogsBefore 清理过期的调试日志
func (s *SQLStore) CleanupDebugLogsBefore(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM debug_logs WHERE created_at < ?`, cutoff.Unix())
	return err
}

// TruncateDebugLogs 清空所有调试日志
func (s *SQLStore) TruncateDebugLogs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM debug_logs`)
	return err
}
