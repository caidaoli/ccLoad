package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

/**
 * @brief 凭证比较和冷却写入在同一事务内完成，旧账号响应不能污染新账号。
 * @param ctx 数据库操作上下文。
 * @param channelID 冷却所属渠道 ID。
 * @param expectedAuthType 发起请求时的 OAuth 认证类型，会先归一化。
 * @param expectedCredential 发起请求时的完整凭证 JSON 快照。
 * @param name 模型冷却键，不得为空。
 * @param until 冷却截止时间。
 * @return 身份匹配且成功写入时为 true；渠道不存在或身份变化时为 false。
 * @return 身份或模型输入非法、事务失败时返回错误；成功或身份不匹配时为 nil。
 * @note 事务内锁定渠道身份后比较凭证并写入冷却，防止重授权竞争。
 */
func (s *SQLStore) SetModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, name string, until time.Time) (bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || model.UsesAPIKeyCredentials(authType) || strings.TrimSpace(expectedCredential) == "" {
		return false, errors.New("invalid expected OAuth identity")
	}
	if _, err := normalizeCooldownModel(name); err != nil {
		return false, err
	}
	matched := false
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched = false
		currentAuthType, currentCredential, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if currentAuthType != authType || currentCredential != expectedCredential {
			return nil
		}
		if err := s.setZAIModelCooldownTx(ctx, tx, channelID, name, until); err != nil {
			return err
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

/**
 * @brief 复用模型冷却 SQL；条件写入使用持有渠道身份锁的事务。
 * @param ctx 数据库操作上下文。
 * @param tx 已持有渠道身份锁的事务。
 * @param channelID 冷却所属渠道 ID。
 * @param model 模型冷却键，不得为空。
 * @param until 冷却截止时间。
 * @return 模型非法或 SQL 执行失败时返回错误；成功时为 nil。
 * @note 不校验身份；调用方负责持锁以及事务提交或回滚。
 */
func (s *SQLStore) setZAIModelCooldownTx(ctx context.Context, tx *sql.Tx, channelID int64, model string, until time.Time) error {
	model, err := normalizeCooldownModel(model)
	if err != nil {
		return err
	}
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	var query string
	if s.supportsONConflict() {
		query = `
			INSERT INTO channel_model_cooldowns
				(channel_id, model, cooldown_until, cooldown_duration_ms, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(channel_id, model) DO UPDATE SET
				cooldown_until = excluded.cooldown_until,
				cooldown_duration_ms = excluded.cooldown_duration_ms,
				updated_at = excluded.updated_at
		`
	} else {
		query = `
			INSERT INTO channel_model_cooldowns
				(channel_id, model, cooldown_until, cooldown_duration_ms, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				cooldown_until = VALUES(cooldown_until),
				cooldown_duration_ms = VALUES(cooldown_duration_ms),
				updated_at = VALUES(updated_at)
		`
	}

	args := []any{channelID, model, timeToUnix(until), durationMs, timeToUnix(now)}
	_, err = s.execTx(ctx, tx, query, args...)
	if err != nil {
		return fmt.Errorf("set model cooldown: %w", err)
	}
	return nil
}
