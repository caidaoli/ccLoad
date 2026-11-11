package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

/**
 * @brief 只读取有待恢复模型历史的 OAuth 渠道身份，避免加载全部渠道配置。
 * @param ctx 数据库上下文，取消时终止查询。
 * @param authType OAuth 类型，不接受 API Key 类型。
 * @param name 模型冷却键，不得为空。
 * @param cutoff 历史截止时间，不得为零。
 * @return 仅含 ID、AuthType、OAuthCredential 的渠道快照，无历史时为空。
 * @return 输入非法、取消或数据库失败时返回错误，否则 nil。
 * @note 包含停用渠道且不修改历史；调用方删除时仍须原子校验身份和窗口。
 */
func (s *SQLStore) ListExpiredOAuthModelCooldowns(ctx context.Context, authType, name string, cutoff time.Time) ([]*model.Config, error) {
	authType = model.NormalizeAuthType(authType)
	if authType == "" || model.UsesAPIKeyCredentials(authType) || cutoff.IsZero() {
		return nil, errors.New("invalid OAuth type or cutoff")
	}
	name, err := normalizeCooldownModel(name)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(ctx, `
		SELECT c.id, c.auth_type, COALESCE(c.oauth_credential, '')
		FROM channel_model_cooldowns mc
		JOIN channels c ON c.id = mc.channel_id
		WHERE mc.model = ? AND mc.cooldown_until <= ? AND c.auth_type = ?
		ORDER BY c.id
	`, name, timeToUnix(cutoff), authType)
	if err != nil {
		return nil, fmt.Errorf("query expired OAuth model cooldowns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var configs []*model.Config
	for rows.Next() {
		cfg := &model.Config{}
		if err := rows.Scan(&cfg.ID, &cfg.AuthType, &cfg.OAuthCredential); err != nil {
			return nil, fmt.Errorf("scan expired OAuth model cooldown: %w", err)
		}
		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired OAuth model cooldowns: %w", err)
	}
	return configs, nil
}

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
	query, args, err := s.modelCooldownUpsert(channelID, name, until)
	if err != nil {
		return false, err
	}
	matched := false
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
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
		if _, err := s.execTx(ctx, tx, query, args...); err != nil {
			return fmt.Errorf("set model cooldown: %w", err)
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
 * @brief 将 OAuth 身份校验与模型指数冷却写入放在同一事务中。
 * @param ctx 存储操作上下文。
 * @param channelID 目标渠道 ID。
 * @param expectedAuthType 预期 OAuth 认证类型。
 * @param expectedCredential 完整凭证 JSON 快照。
 * @param name 模型冷却键，不得为空。
 * @param now 本次错误时间基准。
 * @param statusCode 已归一的上游错误状态。
 * @return 写入后的绝对截止时间；身份不匹配或失败时为零值。
 * @return 身份匹配且写入成功为 true，否则为 false。
 * @return 输入非法、事务或写入失败时返回错误。
 */
func (s *SQLStore) BumpModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, name string, now time.Time, statusCode int) (time.Time, bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || model.UsesAPIKeyCredentials(authType) || strings.TrimSpace(expectedCredential) == "" {
		return time.Time{}, false, errors.New("invalid expected OAuth identity")
	}
	name, err := normalizeCooldownModel(name)
	if err != nil {
		return time.Time{}, false, err
	}
	var until time.Time
	matched := false
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched, until = false, time.Time{}
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
		duration, err := s.bumpModelCooldownTx(ctx, tx, channelID, name, now, statusCode)
		if err != nil {
			return err
		}
		matched, until = true, now.Add(duration)
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return until, matched, nil
}

/**
 * @brief 只清除身份未变化且在成功请求开始前已过期的模型退避历史。
 * @param ctx 数据库操作上下文。
 * @param channelID 目标渠道。
 * @param expectedAuthType 预期 OAuth 类型。
 * @param expectedCredential 发送时完整凭证。
 * @param name 模型冷却键。
 * @param cutoff 成功请求发送时间；晚于此时的冷却必须保留。
 * @return 实际删除历史返回 true；身份变化或无符合历史返回 false。
 * @return 输入非法、取消或事务失败返回错误，否则 nil。
 * @note 凭证比较和条件删除在同一事务完成，保护重新授权及并发失败。
 */
func (s *SQLStore) ResetExpiredModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, name string, cutoff time.Time) (bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || model.UsesAPIKeyCredentials(authType) || strings.TrimSpace(expectedCredential) == "" || cutoff.IsZero() {
		return false, errors.New("invalid expected OAuth identity or cutoff")
	}
	name, err := normalizeCooldownModel(name)
	if err != nil {
		return false, err
	}
	changed := false
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		changed = false
		auth, credential, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if auth != authType || credential != expectedCredential {
			return nil
		}
		result, err := s.execTx(ctx, tx, `DELETE FROM channel_model_cooldowns WHERE channel_id = ? AND model = ? AND cooldown_until <= ?`, channelID, name, timeToUnix(cutoff))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		changed = count > 0
		return err
	})
	return changed && err == nil, err
}
