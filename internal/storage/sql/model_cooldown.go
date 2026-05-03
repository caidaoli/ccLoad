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

func (s *SQLStore) GetAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	nowUnix := timeToUnix(time.Now())
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id, model_name, cooldown_until
		FROM model_cooldowns
		WHERE cooldown_until > ?
	`, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("query all model cooldowns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]map[string]time.Time)
	for rows.Next() {
		var channelID int64
		var modelName string
		var cooldownUntil int64
		if err := rows.Scan(&channelID, &modelName, &cooldownUntil); err != nil {
			return nil, fmt.Errorf("scan model cooldown: %w", err)
		}
		if result[channelID] == nil {
			result[channelID] = make(map[string]time.Time)
		}
		result[channelID][modelName] = unixToTime(cooldownUntil)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model cooldowns: %w", err)
	}

	return result, nil
}

func (s *SQLStore) BumpModelCooldown(ctx context.Context, channelID int64, modelName string, now time.Time, statusCode int) (time.Duration, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return 0, errors.New("model name cannot be empty")
	}

	var nextDuration time.Duration
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var cooldownUntil int64
		var cooldownDurationMs int64

		err := tx.QueryRowContext(ctx, `
			SELECT cooldown_until, cooldown_duration_ms
			FROM model_cooldowns
			WHERE channel_id = ? AND model_name = ?
		`, channelID, modelName).Scan(&cooldownUntil, &cooldownDurationMs)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("query model cooldown: %w", err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			cooldownUntil = 0
			cooldownDurationMs = 0
		}

		until := unixToTime(cooldownUntil)
		nextDuration = util.CalculateBackoffDuration(cooldownDurationMs, until, now, &statusCode)
		newUntil := now.Add(nextDuration)
		if err := upsertModelCooldownTx(ctx, tx, s.IsSQLite(), channelID, modelName, newUntil, nextDuration, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	return nextDuration, nil
}

func (s *SQLStore) ResetModelCooldown(ctx context.Context, channelID int64, modelName string) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return errors.New("model name cannot be empty")
	}

	_, err := s.db.ExecContext(ctx, `
		DELETE FROM model_cooldowns
		WHERE channel_id = ? AND model_name = ?
	`, channelID, modelName)
	if err != nil {
		return fmt.Errorf("reset model cooldown: %w", err)
	}
	return nil
}

func (s *SQLStore) SetModelCooldown(ctx context.Context, channelID int64, modelName string, until time.Time) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return errors.New("model name cannot be empty")
	}
	now := time.Now()
	if !until.After(now) {
		return s.ResetModelCooldown(ctx, channelID, modelName)
	}

	durationMs := util.CalculateCooldownDuration(until, now)
	duration := time.Duration(durationMs) * time.Millisecond
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		return upsertModelCooldownTx(ctx, tx, s.IsSQLite(), channelID, modelName, until, duration, now)
	})
}

func upsertModelCooldownTx(ctx context.Context, tx *sql.Tx, sqliteDialect bool, channelID int64, modelName string, until time.Time, duration time.Duration, now time.Time) error {
	untilUnix := timeToUnix(until)
	durationMs := int64(duration / time.Millisecond)
	nowUnix := timeToUnix(now)

	if sqliteDialect {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO model_cooldowns(channel_id, model_name, cooldown_until, cooldown_duration_ms, updated_at)
			VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(channel_id, model_name) DO UPDATE SET
				cooldown_until = excluded.cooldown_until,
				cooldown_duration_ms = excluded.cooldown_duration_ms,
				updated_at = excluded.updated_at
		`, channelID, modelName, untilUnix, durationMs, nowUnix)
		if err != nil {
			return fmt.Errorf("upsert model cooldown: %w", err)
		}
		return nil
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO model_cooldowns(channel_id, model_name, cooldown_until, cooldown_duration_ms, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			cooldown_until = VALUES(cooldown_until),
			cooldown_duration_ms = VALUES(cooldown_duration_ms),
			updated_at = VALUES(updated_at)
	`, channelID, modelName, untilUnix, durationMs, nowUnix)
	if err != nil {
		return fmt.Errorf("upsert model cooldown: %w", err)
	}
	return nil
}
