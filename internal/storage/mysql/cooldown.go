package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/util"
)

// ==================== 渠道级冷却 ====================

func (s *MySQLStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	var nextDuration time.Duration

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var cooldownUntil, cooldownDurationMs int64
		err := tx.QueryRowContext(ctx, `
			SELECT cooldown_until, cooldown_duration_ms FROM channels WHERE id = ?
		`, channelID).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("channel not found")
			}
			return fmt.Errorf("query channel cooldown: %w", err)
		}

		until := time.Unix(cooldownUntil, 0)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		_, err = tx.ExecContext(ctx, `
			UPDATE channels SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ? WHERE id = ?
		`, newUntil.Unix(), int64(nextDuration/time.Millisecond), now.Unix(), channelID)

		return err
	})

	return nextDuration, err
}

func (s *MySQLStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE channels SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ? WHERE id = ?
	`, time.Now().Unix(), channelID)
	return err
}

func (s *MySQLStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.db.ExecContext(ctx, `
		UPDATE channels SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ? WHERE id = ?
	`, until.Unix(), durationMs, now.Unix(), channelID)
	return err
}

func (s *MySQLStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT id, cooldown_until FROM channels WHERE cooldown_until > ?`, now)
	if err != nil {
		return nil, fmt.Errorf("query all channel cooldowns: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]time.Time)
	for rows.Next() {
		var channelID, until int64
		if err := rows.Scan(&channelID, &until); err != nil {
			return nil, fmt.Errorf("scan channel cooldown: %w", err)
		}
		result[channelID] = time.Unix(until, 0)
	}

	return result, rows.Err()
}

// ==================== Key级冷却 ====================

func (s *MySQLStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, key_index, cooldown_until FROM api_keys WHERE cooldown_until > ?`, now)
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

		if result[channelID] == nil {
			result[channelID] = make(map[int]time.Time)
		}
		result[channelID][keyIndex] = time.Unix(until, 0)
	}

	return result, rows.Err()
}

func (s *MySQLStore) BumpKeyCooldown(ctx context.Context, configID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	var nextDuration time.Duration

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var cooldownUntil, cooldownDurationMs int64
		err := tx.QueryRowContext(ctx, `
			SELECT cooldown_until, cooldown_duration_ms FROM api_keys WHERE channel_id = ? AND key_index = ?
		`, configID, keyIndex).Scan(&cooldownUntil, &cooldownDurationMs)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("api key not found")
			}
			return fmt.Errorf("query key cooldown: %w", err)
		}

		until := time.Unix(cooldownUntil, 0)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)

		_, err = tx.ExecContext(ctx, `
			UPDATE api_keys SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ? WHERE channel_id = ? AND key_index = ?
		`, newUntil.Unix(), int64(nextDuration/time.Millisecond), now.Unix(), configID, keyIndex)

		return err
	})

	return nextDuration, err
}

func (s *MySQLStore) SetKeyCooldown(ctx context.Context, configID int64, keyIndex int, until time.Time) error {
	now := time.Now()
	durationMs := util.CalculateCooldownDuration(until, now)

	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET cooldown_until = ?, cooldown_duration_ms = ?, updated_at = ? WHERE channel_id = ? AND key_index = ?
	`, until.Unix(), durationMs, now.Unix(), configID, keyIndex)
	return err
}

func (s *MySQLStore) ResetKeyCooldown(ctx context.Context, configID int64, keyIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET cooldown_until = 0, cooldown_duration_ms = 0, updated_at = ? WHERE channel_id = ? AND key_index = ?
	`, time.Now().Unix(), configID, keyIndex)
	return err
}
