package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/util"
)

// ==================== 渠道级冷却方法（操作 channels 表内联字段）====================

// BumpChannelCooldown 渠道级冷却：指数退避策略（认证错误5分钟起，其他1秒起，最大30分钟）
func (s *SQLiteStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	// 使用事务保护Read-Modify-Write操作,防止并发竞态
	// 问题场景同BumpKeyCooldown,多个并发请求可能导致指数退避计算错误

	var nextDuration time.Duration

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 1. 读取当前冷却状态(事务内,隐式锁定行)
		var cooldownUntil, cooldownDurationMs int64
		err := tx.QueryRowContext(ctx, `
			SELECT cooldown_until, cooldown_duration_ms
			FROM channels
			WHERE id = ?
		`, channelID).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("channel not found")
			}
			return fmt.Errorf("query channel cooldown: %w", err)
		}

		// 2. 计算新的冷却时间(指数退避)
		until := time.Unix(cooldownUntil, 0)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		// 3. 更新 channels 表(事务内)
		_, err = tx.ExecContext(ctx, `
			UPDATE channels
			SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
			WHERE id = ?
		`, newUntil.Unix(), int64(nextDuration/time.Millisecond), now.Unix(), channelID)

		if err != nil {
			return fmt.Errorf("update channel cooldown: %w", err)
		}

		return nil
	})

	return nextDuration, err
}

// ResetChannelCooldown 重置渠道冷却状态
func (s *SQLiteStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE channels
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE id = ?
	`, time.Now().Unix(), channelID)

	if err != nil {
		return fmt.Errorf("reset channel cooldown: %w", err)
	}

	return nil
}

// SetChannelCooldown 设置渠道冷却（手动设置冷却时间）
func (s *SQLiteStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.db.ExecContext(ctx, `
		UPDATE channels
		SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
		WHERE id = ?
	`, until.Unix(), durationMs, now.Unix(), channelID)

	if err != nil {
		return fmt.Errorf("set channel cooldown: %w", err)
	}

	return nil
}

// GetAllChannelCooldowns 批量查询所有渠道冷却状态（从 channels 表读取）
func (s *SQLiteStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	now := time.Now().Unix()
	query := `SELECT id, cooldown_until FROM channels WHERE cooldown_until > ?`

	rows, err := s.db.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all channel cooldowns: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]time.Time)
	for rows.Next() {
		var channelID int64
		var until int64

		if err := rows.Scan(&channelID, &until); err != nil {
			return nil, fmt.Errorf("scan channel cooldown: %w", err)
		}

		result[channelID] = time.Unix(until, 0)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel cooldowns: %w", err)
	}

	return result, nil
}

// ==================== Key级别冷却机制（操作 api_keys 表内联字段）====================

// GetKeyCooldownUntil 查询指定Key的冷却截止时间（从 api_keys 表读取）
func (s *SQLiteStore) GetKeyCooldownUntil(ctx context.Context, configID int64, keyIndex int) (time.Time, bool) {
	var cooldownUntil int64
	err := s.db.QueryRowContext(ctx, `
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

	return time.Unix(cooldownUntil, 0), true
}

// GetAllKeyCooldowns 批量查询所有Key冷却状态（从 api_keys 表读取）
// 返回: map[channelID]map[keyIndex]cooldownUntil
func (s *SQLiteStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	now := time.Now().Unix()
	query := `SELECT channel_id, key_index, cooldown_until FROM api_keys WHERE cooldown_until > ?`

	rows, err := s.db.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all key cooldowns: %w", err)
	}
	defer rows.Close()

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
		result[channelID][keyIndex] = time.Unix(until, 0)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return result, nil
}

// BumpKeyCooldown Key级别冷却：指数退避策略（认证错误5分钟起，其他1秒起，最大30分钟）
func (s *SQLiteStore) BumpKeyCooldown(ctx context.Context, configID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
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
		// 1. 读取当前冷却状态(事务内,隐式锁定行)
		var cooldownUntil, cooldownDurationMs int64
		err := tx.QueryRowContext(ctx, `
			SELECT cooldown_until, cooldown_duration_ms
			FROM api_keys
			WHERE channel_id = ? AND key_index = ?
		`, configID, keyIndex).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("api key not found")
			}
			return fmt.Errorf("query key cooldown: %w", err)
		}

		// 2. 计算新的冷却时间(指数退避)
		until := time.Unix(cooldownUntil, 0)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		// 3. 更新 api_keys 表(事务内)
		_, err = tx.ExecContext(ctx, `
			UPDATE api_keys
			SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
			WHERE channel_id = ? AND key_index = ?
		`, newUntil.Unix(), int64(nextDuration/time.Millisecond), now.Unix(), configID, keyIndex)

		if err != nil {
			return fmt.Errorf("update key cooldown: %w", err)
		}

		return nil
	})

	return nextDuration, err
}

// SetKeyCooldown 设置指定Key的冷却截止时间（操作 api_keys 表）
func (s *SQLiteStore) SetKeyCooldown(ctx context.Context, configID int64, keyIndex int, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ?
		WHERE channel_id = ? AND key_index = ?
	`, until.Unix(), durationMs, now.Unix(), configID, keyIndex)

	return err
}

// ResetKeyCooldown 重置指定Key的冷却状态（操作 api_keys 表）
func (s *SQLiteStore) ResetKeyCooldown(ctx context.Context, configID int64, keyIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE channel_id = ? AND key_index = ?
	`, time.Now().Unix(), configID, keyIndex)

	return err
}

// ClearAllKeyCooldowns 清理渠道的所有Key冷却数据（操作 api_keys 表）
func (s *SQLiteStore) ClearAllKeyCooldowns(ctx context.Context, configID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ?
		WHERE channel_id = ?
	`, time.Now().Unix(), configID)

	return err
}
