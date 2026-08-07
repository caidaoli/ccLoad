package sql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

// AddDebugLog 插入一条调试日志
func (s *SQLStore) AddDebugLog(ctx context.Context, e *model.DebugLogEntry) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	_, err := s.ExecContext(ctx, `
			INSERT INTO debug_logs (log_id, created_at, req_method, req_url, req_headers, req_body, resp_status, resp_headers, resp_body,
				protocol_transformed, original_req_url, original_req_headers, original_req_body,
				translated_resp_status, translated_resp_headers, translated_resp_body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.LogID, e.CreatedAt, e.ReqMethod, e.ReqURL, e.ReqHeaders, e.ReqBody, e.RespStatus, e.RespHeaders, e.RespBody,
		e.ProtocolTransformed, e.OriginalReqURL, e.OriginalReqHeaders, e.OriginalReqBody,
		e.TranslatedRespStatus, e.TranslatedRespHeaders, e.TranslatedRespBody,
	)
	return err
}

// GetDebugLogByLogID 根据 log_id 查询调试日志
func (s *SQLStore) GetDebugLogByLogID(ctx context.Context, logID int64) (*model.DebugLogEntry, error) {
	row := s.QueryRowContext(ctx, `
			SELECT log_id, created_at, req_method, req_url, req_headers, req_body, resp_status, resp_headers, resp_body,
				protocol_transformed, COALESCE(original_req_url, ''), COALESCE(original_req_headers, ''), original_req_body,
				translated_resp_status, COALESCE(translated_resp_headers, ''), translated_resp_body
			FROM debug_logs WHERE log_id = ? LIMIT 1`, logID)

	var e model.DebugLogEntry
	err := row.Scan(
		&e.LogID, &e.CreatedAt, &e.ReqMethod, &e.ReqURL, &e.ReqHeaders, &e.ReqBody,
		&e.RespStatus, &e.RespHeaders, &e.RespBody, &e.ProtocolTransformed,
		&e.OriginalReqURL, &e.OriginalReqHeaders, &e.OriginalReqBody,
		&e.TranslatedRespStatus, &e.TranslatedRespHeaders, &e.TranslatedRespBody,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// CleanupDebugLogsBatch 按创建时间删除有限数量的过期调试日志。
func (s *SQLStore) CleanupDebugLogsBatch(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, fmt.Errorf("debug log cleanup limit must be positive: %d", limit)
	}

	query := `DELETE FROM debug_logs
		WHERE log_id IN (
			SELECT log_id FROM debug_logs
			WHERE created_at < ?
			ORDER BY created_at
			LIMIT ?
		)`
	if s.IsMySQL() {
		query = `DELETE FROM debug_logs WHERE created_at < ? ORDER BY created_at LIMIT ?`
	}

	// 单条 DELETE 不包外层事务；ExecContext 成功返回时该批次已经自动提交。
	result, err := s.ExecContext(ctx, query, cutoff.Unix(), limit)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// TruncateDebugLogs 清空所有调试日志
func (s *SQLStore) TruncateDebugLogs(ctx context.Context) error {
	query := `TRUNCATE TABLE debug_logs`
	if s.IsSQLite() {
		// SQLite 没有 TRUNCATE TABLE；无条件 DELETE 会触发 truncate optimization。
		query = `DELETE FROM debug_logs`
	}
	result, err := s.ExecContext(ctx, query)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	s.runSQLiteIncrementalVacuum(ctx, affected)
	return nil
}
