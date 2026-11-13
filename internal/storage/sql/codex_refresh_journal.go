package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

/**
 * @brief 申领刷新授权并绑定 owner，只有首次插入获准发送刷新请求
 * @param ctx 数据库操作上下文
 * @param key 客户端身份与刷新令牌的 SHA-256 十六进制摘要
 * @param owner 本次申领独有的随机标识
 * @return 首次插入返回 true；重复授权返回 false
 * @return 数据库执行、读取影响行数或上下文取消时返回错误
 * @note 并发安全；记录不绑定渠道生命周期
 */
func (s *SQLStore) RefreshClaim(ctx context.Context, key, owner string) (bool, error) {
	query := `INSERT INTO codex_refresh_journal (grant_key, claim_owner) VALUES (?, ?)`
	if s.supportsONConflict() {
		query += ` ON CONFLICT(grant_key) DO NOTHING`
	}
	result, err := s.ExecContext(ctx, query, key, owner)
	if err != nil {
		if s.IsMySQL() && isMySQLDuplicateEntryError(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim Codex refresh grant: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read Codex refresh claim result: %w", err)
	}
	return rows == 1, nil
}

/**
 * @brief 释放确证未发送且仍归原 owner 持有的未完成刷新授权
 * @param ctx 数据库操作上下文
 * @param key 已申领授权的 SHA-256 十六进制摘要
 * @param owner 本次成功申领时提供的随机标识
 * @return 数据库失败、读取影响行数失败、记录不存在、已完成或 owner 不匹配时返回错误
 * @note 并发安全；条件删除防止迟到释放删除后续 owner 的申领。
 */
func (s *SQLStore) RefreshRelease(ctx context.Context, key, owner string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM codex_refresh_journal WHERE grant_key = ? AND claim_owner = ? AND receipt IS NULL`, key, owner)
	if err != nil {
		return fmt.Errorf("release Codex refresh grant: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Codex refresh release result: %w", err)
	}
	if rows != 1 {
		return errors.New("codex refresh grant is not owned or already completed")
	}
	return nil
}

/**
 * @brief 读取已完成的 Codex 刷新回执
 * @param ctx 数据库操作上下文
 * @param key 已申领授权的 SHA-256 十六进制摘要
 * @return 原始回执；未申领或结果未知时返回 nil
 * @return 记录存在时为 true；未申领或查询失败时为 false
 * @return 数据库查询或上下文取消时返回错误
 */
func (s *SQLStore) RefreshRead(ctx context.Context, key string) ([]byte, bool, error) {
	var receipt []byte
	err := s.QueryRowContext(ctx, `SELECT receipt FROM codex_refresh_journal WHERE grant_key = ?`, key).Scan(&receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Codex refresh receipt: %w", err)
	}
	return receipt, true, nil
}

/**
 * @brief 仅为已申领且没有回执的 Codex 刷新授权写入最终结果
 * @param ctx 数据库操作上下文
 * @param key 已申领授权的 SHA-256 十六进制摘要
 * @param receipt 刷新交换结果序列化的私密回执
 * @return 数据库执行、读取影响行数、上下文取消、未申领或已完成时返回错误
 * @note 并发安全；最终回执不可覆盖
 */
func (s *SQLStore) RefreshComplete(ctx context.Context, key string, receipt []byte) error {
	result, err := s.ExecContext(ctx, `UPDATE codex_refresh_journal SET receipt = ? WHERE grant_key = ? AND receipt IS NULL`, string(receipt), key)
	if err != nil {
		return fmt.Errorf("complete Codex refresh grant: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Codex refresh completion result: %w", err)
	}
	if rows != 1 {
		return errors.New("codex refresh grant is unclaimed or already completed")
	}
	return nil
}
