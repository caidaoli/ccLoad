package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

/**
 * @brief Accio 额度冷却在模型表中的临时窗口时长和持续时间保留值。
 * @details 普通时长非负；确认行使用 -1，临时行以负的纳秒截止时间区分轮次。
 *          到期后两类额度冷却均不再参与普通模型的指数退避。
 */
const (
	accioProvisionalQuotaDuration = 10 * time.Minute ///< 上游额度查询无法确认时保留的临时时长。
	accioConfirmedQuotaDurationMs = -1               ///< 已确认的账号和模型额度冷却。
)

/**
 * @brief 为 Accio 模型登记临时额度冷却。同一账号的一轮冷却只有首个调用方负责查询。
 * @param ctx 数据库操作上下文。
 * @param channelID 冷却所属渠道 ID。
 * @param expectedCredential 查询前读取的完整凭证原文。
 * @param actualModel 上游报出额度错误的实际模型。
 * @param now 本轮错误的时间基准。
 * @return 本轮临时截止时间；凭证变化、已有确认窗口或模型已有更长冷却时为零值。
 * @return 创建临时窗口并应查询额度时为 true。
 * @return 模型非法、取消或数据库失败时返回错误。
 */
func (s *SQLStore) BeginAccioQuotaCooldown(ctx context.Context, channelID int64, expectedCredential, actualModel string, now time.Time) (time.Time, bool, error) {
	name, err := normalizeCooldownModel(actualModel)
	if err != nil {
		return time.Time{}, false, err
	}
	var until time.Time
	var probe bool
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		until, probe = time.Time{}, false
		_, matches, err := s.lockAccioQuotaChannel(ctx, tx, channelID, expectedCredential)
		if err != nil || !matches {
			return err
		}
		nowUnix := timeToUnix(now)
		var confirmedUntil, generation int64
		if err := s.queryRowTx(ctx, tx, `
			SELECT COALESCE(MAX(CASE WHEN model = ? THEN cooldown_until END), 0),
			       COALESCE(MIN(CASE WHEN cooldown_duration_ms < ? THEN cooldown_duration_ms END), 0)
			FROM channel_model_cooldowns WHERE channel_id = ? AND cooldown_until > ?
		`, model.AccioQuotaWindowModel, accioConfirmedQuotaDurationMs, channelID, nowUnix).Scan(&confirmedUntil, &generation); err != nil {
			return fmt.Errorf("load Accio quota cooldown window: %w", err)
		}
		if confirmedUntil > nowUnix {
			return nil
		}

		candidate := now.Add(accioProvisionalQuotaDuration)
		newWindow := generation == 0
		if newWindow {
			generation = -candidate.UnixNano()
		} else {
			candidate = time.Unix(0, -generation)
		}
		var currentUntil, currentDuration int64
		err = s.queryRowTx(ctx, tx, `
			SELECT cooldown_until, cooldown_duration_ms FROM channel_model_cooldowns
			WHERE channel_id = ? AND model = ?
		`+s.cooldownSelectLockClause(), channelID, name).Scan(&currentUntil, &currentDuration)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load Accio quota model cooldown: %w", err)
		}
		untilUnix := timeToUnix(candidate)
		if currentUntil >= untilUnix {
			if currentDuration == generation {
				until = candidate
			}
			return nil
		}
		if _, err := s.execTx(ctx, tx, s.modelCooldownUpsertSQL(), channelID, name, untilUnix, generation, nowUnix); err != nil {
			return fmt.Errorf("begin Accio quota model cooldown: %w", err)
		}
		until, probe = candidate, newWindow
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return until, probe, nil
}

/**
 * @brief 仍匹配的 Accio 临时额度轮次以官方恢复时间确认，保留更久的独立渠道冷却。
 * @param ctx 数据库操作上下文。
 * @param channelID 冷却所属渠道 ID。
 * @param expectedCredential 发起查询前读取的完整凭证原文。
 * @param actualModel 发起本轮查询的实际模型。
 * @param provisionalUntil 创建本轮时返回的原始时间，含纳秒级轮次信息。
 * @param resetAt 官方额度恢复时间。
 * @return 事务成功确认窗口时为 true，身份或原冷却变化时为 false。
 * @return 模型非法、取消或数据库失败时返回错误。
 */
func (s *SQLStore) ConfirmAccioQuotaWindow(ctx context.Context, channelID int64, expectedCredential, actualModel string, provisionalUntil, resetAt time.Time) (bool, error) {
	name, err := normalizeCooldownModel(actualModel)
	if err != nil {
		return false, err
	}
	var matched bool
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched = false
		channelUntil, sameCredential, err := s.lockAccioQuotaChannel(ctx, tx, channelID, expectedCredential)
		if err != nil || !sameCredential {
			return err
		}
		now := time.Now()
		nowUnix, provisionalUnix, resetUnix := timeToUnix(now), timeToUnix(provisionalUntil), timeToUnix(resetAt)
		if provisionalUnix <= nowUnix || resetUnix <= nowUnix {
			return nil
		}

		generation := -provisionalUntil.UnixNano()
		const updateModels = `UPDATE channel_model_cooldowns
			SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
			WHERE channel_id = ? AND cooldown_until = ? AND cooldown_duration_ms = ?`
		result, err := s.execTx(ctx, tx, updateModels+` AND model = ?`, resetUnix, accioConfirmedQuotaDurationMs, nowUnix, channelID, provisionalUnix, generation, name)
		if err != nil {
			return fmt.Errorf("confirm initiating Accio quota model cooldown: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if _, err := s.execTx(ctx, tx, updateModels, resetUnix, accioConfirmedQuotaDurationMs, nowUnix, channelID, provisionalUnix, generation); err != nil {
			return fmt.Errorf("confirm other Accio quota model cooldowns: %w", err)
		}
		if _, err := s.execTx(ctx, tx, s.modelCooldownUpsertSQL(), channelID, model.AccioQuotaWindowModel, resetUnix, accioConfirmedQuotaDurationMs, nowUnix); err != nil {
			return fmt.Errorf("confirm Accio quota account cooldown: %w", err)
		}
		if channelUntil < resetUnix {
			if _, err := s.execTx(ctx, tx, `
				UPDATE channels SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ? WHERE id = ?
			`, resetUnix, util.CalculateCooldownDuration(resetAt, now), nowUnix, channelID); err != nil {
				return fmt.Errorf("confirm Accio quota channel cooldown: %w", err)
			}
		}
		matched = true
		return nil
	})
	return matched && err == nil, err
}

/**
 * @brief 锁定启用的 Accio 渠道并核对完整凭证，串行化同账号的额度状态更改。
 * @param ctx 数据库操作上下文。
 * @param tx 调用方持有的事务。
 * @param channelID 目标渠道 ID。
 * @param expectedCredential 与存储值比较账号和令牌的凭证 JSON。
 * @return 渠道冷却的 Unix 截止时间。
 * @return 渠道启用且凭证匹配时为 true。
 * @return 数据库读取失败时返回错误，渠道不存在时为 nil。
 */
func (s *SQLStore) lockAccioQuotaChannel(ctx context.Context, tx *sql.Tx, channelID int64, expectedCredential string) (int64, bool, error) {
	var currentCredential string
	var channelUntil int64
	err := s.queryRowTx(ctx, tx, `
		SELECT COALESCE(oauth_credential, ''), cooldown_until FROM channels
		WHERE id = ? AND enabled = 1 AND auth_type = ?
	`+s.cooldownSelectLockClause(), channelID, model.AuthTypeAccioOAuth).Scan(&currentCredential, &channelUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load Accio quota channel: %w", err)
	}
	return channelUntil, accioauth.SameCredentialAuthentication(currentCredential, expectedCredential), nil
}
