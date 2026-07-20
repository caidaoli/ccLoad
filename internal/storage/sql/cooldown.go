package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/util"
)

// ==================== 渠道级冷却方法（操作 channels 表内联字段）====================

func (s *SQLStore) cooldownSelectLockClause() string {
	if s.supportsRowLock() {
		return " FOR UPDATE"
	}
	return ""
}

// BumpChannelCooldown 渠道级冷却：指数退避策略（认证错误5分钟起，其他1秒起，最大30分钟）
func (s *SQLStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	// 使用事务保护Read-Modify-Write操作,防止并发竞态
	// 问题场景同BumpKeyCooldown,多个并发请求可能导致指数退避计算错误

	var nextDuration time.Duration

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 1. 读取当前冷却状态。MySQL 必须显式 FOR UPDATE 锁行，否则两个事务可读到同一旧值。
		var cooldownUntil, cooldownDurationMs int64
		err := s.queryRowTx(ctx, tx, `
			SELECT cooldown_until, cooldown_duration_ms
			FROM channels
			WHERE id = ?
		`+s.cooldownSelectLockClause(), channelID).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("channel not found")
			}
			return fmt.Errorf("query channel cooldown: %w", err)
		}

		// 2. 计算新的冷却时间(指数退避)
		until := unixToTime(cooldownUntil)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		// 3. 更新 channels 表(事务内)
		_, err = s.execTx(ctx, tx, `
			UPDATE channels
			SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
			WHERE id = ?
		`, timeToUnix(newUntil), int64(nextDuration/time.Millisecond), timeToUnix(now), channelID)

		if err != nil {
			return fmt.Errorf("update channel cooldown: %w", err)
		}

		return nil
	})

	return nextDuration, err
}

// ResetChannelCooldown 重置渠道冷却状态
// 优化：仅更新实际处于冷却中的记录，避免无谓的写入
func (s *SQLStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	_, err := s.ExecContext(ctx, `
		UPDATE channels
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE id = ? AND cooldown_until > 0
	`, timeToUnix(time.Now()), channelID)

	if err != nil {
		return fmt.Errorf("reset channel cooldown: %w", err)
	}

	return nil
}

// SetChannelCooldown 设置渠道冷却（手动设置冷却时间）
func (s *SQLStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.ExecContext(ctx, `
		UPDATE channels
		SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
		WHERE id = ?
	`, timeToUnix(until), durationMs, timeToUnix(now), channelID)

	if err != nil {
		return fmt.Errorf("set channel cooldown: %w", err)
	}

	return nil
}

// GetAllChannelCooldowns 批量查询所有渠道冷却状态（从 channels 表读取）
func (s *SQLStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	now := timeToUnix(time.Now())
	query := `SELECT id, cooldown_until FROM channels WHERE cooldown_until > ?`

	rows, err := s.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all channel cooldowns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]time.Time)
	for rows.Next() {
		var channelID int64
		var until int64

		if err := rows.Scan(&channelID, &until); err != nil {
			return nil, fmt.Errorf("scan channel cooldown: %w", err)
		}

		result[channelID] = unixToTime(until)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel cooldowns: %w", err)
	}

	return result, nil
}

// ==================== 模型级冷却机制（channel_id + 实际上游模型）====================

func normalizeCooldownModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("model is empty")
	}
	if strings.ContainsAny(model, "\x00\r\n") {
		return "", errors.New("model contains illegal characters")
	}
	return model, nil
}

// GetAllModelCooldowns 批量查询未过期的模型冷却状态。
func (s *SQLStore) GetAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT channel_id, model, cooldown_until
		FROM channel_model_cooldowns
		WHERE cooldown_until > ?
	`, timeToUnix(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("query all model cooldowns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]map[string]time.Time)
	for rows.Next() {
		var channelID int64
		var model string
		var until int64
		if err := rows.Scan(&channelID, &model, &until); err != nil {
			return nil, fmt.Errorf("scan model cooldown: %w", err)
		}
		if result[channelID] == nil {
			result[channelID] = make(map[string]time.Time)
		}
		result[channelID][model] = unixToTime(until)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model cooldowns: %w", err)
	}
	return result, nil
}

// SetModelCooldown 设置指定渠道的实际上游模型冷却截止时间。
func (s *SQLStore) SetModelCooldown(ctx context.Context, channelID int64, model string, until time.Time) error {
	model, err := normalizeCooldownModel(model)
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := s.ExecContext(ctx, `
		DELETE FROM channel_model_cooldowns
		WHERE channel_id = ? AND model = ? AND cooldown_until <= ?
	`, channelID, model, timeToUnix(now)); err != nil {
		return fmt.Errorf("cleanup expired model cooldown: %w", err)
	}

	var query string
	if s.supportsONConflict() {
		query = `
			INSERT INTO channel_model_cooldowns (channel_id, model, cooldown_until, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(channel_id, model) DO UPDATE SET
				cooldown_until = excluded.cooldown_until,
				updated_at = excluded.updated_at
		`
	} else {
		query = `
			INSERT INTO channel_model_cooldowns (channel_id, model, cooldown_until, updated_at)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				cooldown_until = VALUES(cooldown_until),
				updated_at = VALUES(updated_at)
		`
	}

	if _, err := s.ExecContext(ctx, query, channelID, model, timeToUnix(until), timeToUnix(now)); err != nil {
		return fmt.Errorf("set model cooldown: %w", err)
	}
	return nil
}

// ResetModelCooldown 清除指定渠道模型的冷却状态。
func (s *SQLStore) ResetModelCooldown(ctx context.Context, channelID int64, model string) error {
	model, err := normalizeCooldownModel(model)
	if err != nil {
		return err
	}
	if _, err := s.ExecContext(ctx, `
		DELETE FROM channel_model_cooldowns
		WHERE channel_id = ? AND model = ?
	`, channelID, model); err != nil {
		return fmt.Errorf("reset model cooldown: %w", err)
	}
	return nil
}

// ==================== Key级别冷却机制（操作 api_keys 表内联字段）====================

// GetKeyCooldownUntil 查询指定Key的冷却截止时间（从 api_keys 表读取）
func (s *SQLStore) GetKeyCooldownUntil(ctx context.Context, configID int64, keyIndex int) (time.Time, bool) {
	var cooldownUntil int64
	err := s.QueryRowContext(ctx, `
		SELECT cooldown_until
		FROM api_keys
		WHERE channel_id = ? AND key_index = ?
	`, configID, keyIndex).Scan(&cooldownUntil)

	if err != nil {
		return time.Time{}, false
	}

	if cooldownUntil == 0 {
		return time.Time{}, false
	}

	return unixToTime(cooldownUntil), true
}

// GetAllKeyCooldowns 批量查询所有Key冷却状态（从 api_keys 表读取）
// 返回: map[channelID]map[keyIndex]cooldownUntil
func (s *SQLStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	now := timeToUnix(time.Now())
	query := `SELECT channel_id, key_index, cooldown_until FROM api_keys WHERE cooldown_until > ? AND disabled = 0`

	rows, err := s.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all key cooldowns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]map[int]time.Time)
	for rows.Next() {
		var channelID int64
		var keyIndex int
		var until int64

		if err := rows.Scan(&channelID, &keyIndex, &until); err != nil {
			return nil, fmt.Errorf("scan key cooldown: %w", err)
		}

		// 初始化渠道级map
		if result[channelID] == nil {
			result[channelID] = make(map[int]time.Time)
		}
		result[channelID][keyIndex] = unixToTime(until)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return result, nil
}

// BumpKeyCooldown Key级别冷却：指数退避策略（认证错误5分钟起，其他1秒起，最大30分钟）
func (s *SQLStore) BumpKeyCooldown(ctx context.Context, configID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	// 使用事务保护Read-Modify-Write操作,防止并发竞态
	// 问题场景:
	//   请求A: 读取duration=1000 → 计算新值=2000
	//   请求B: 读取duration=1000 → 计算新值=2000 (应该是4000!)
	//   请求A: 写入2000
	//   请求B: 写入2000 (覆盖A的更新,指数退避失效!)
	//
	// 修复后: 整个操作在事务中原子执行,避免Lost Update问题

	var nextDuration time.Duration

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 1. 读取当前冷却状态。MySQL 必须显式 FOR UPDATE 锁行，否则两个事务可读到同一旧值。
		var cooldownUntil, cooldownDurationMs int64
		err := s.queryRowTx(ctx, tx, `
			SELECT cooldown_until, cooldown_duration_ms
			FROM api_keys
			WHERE channel_id = ? AND key_index = ?
		`+s.cooldownSelectLockClause(), configID, keyIndex).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("api key not found")
			}
			return fmt.Errorf("query key cooldown: %w", err)
		}

		// 2. 计算新的冷却时间(指数退避)
		until := unixToTime(cooldownUntil)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		// 3. 更新 api_keys 表(事务内)
		_, err = s.execTx(ctx, tx, `
			UPDATE api_keys
			SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
			WHERE channel_id = ? AND key_index = ?
		`, timeToUnix(newUntil), int64(nextDuration/time.Millisecond), timeToUnix(now), configID, keyIndex)

		if err != nil {
			return fmt.Errorf("update key cooldown: %w", err)
		}

		return nil
	})

	return nextDuration, err
}

// SetKeyCooldown 设置指定Key的冷却截止时间（操作 api_keys 表）
func (s *SQLStore) SetKeyCooldown(ctx context.Context, configID int64, keyIndex int, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
		WHERE channel_id = ? AND key_index = ?
	`, timeToUnix(until), durationMs, timeToUnix(now), configID, keyIndex)

	return err
}

// ResetKeyCooldown 重置指定Key的冷却状态（操作 api_keys 表）
// 优化：仅更新实际处于冷却中的记录，避免无谓的写入
func (s *SQLStore) ResetKeyCooldown(ctx context.Context, configID int64, keyIndex int) error {
	_, err := s.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE channel_id = ? AND key_index = ? AND cooldown_until > 0
	`, timeToUnix(time.Now()), configID, keyIndex)

	return err
}

// ClearAllKeyCooldowns 清理渠道的所有Key冷却数据（操作 api_keys 表）
func (s *SQLStore) ClearAllKeyCooldowns(ctx context.Context, configID int64) error {
	_, err := s.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE channel_id = ?
	`, timeToUnix(time.Now()), configID)

	return err
}
