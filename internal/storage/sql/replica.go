package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

var replicaTables = map[string]string{
	"channels":                 "channels",
	"auth_tokens":              "auth_tokens",
	"model_fingerprints":       "model_fingerprints",
	"fingerprint_test_results": "fingerprint_test_results",
}

// ReplicaCooldownState preserves both fields used by exponential backoff.
type ReplicaCooldownState struct {
	Until      int64
	DurationMs int64
}

// ListReplicaIDsPage returns a bounded, stable page for in-memory reconciliation.
func (s *SQLStore) ListReplicaIDsPage(ctx context.Context, entity string, afterID int64, limit int) ([]int64, error) {
	table, ok := replicaTables[entity]
	if !ok {
		return nil, fmt.Errorf("unsupported replica entity %q", entity)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("replica page limit must be positive")
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM `+table+` WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReplicaRowExists checks one durable entity without loading its payload.
func (s *SQLStore) ReplicaRowExists(ctx context.Context, entity string, id int64) (bool, error) {
	table, ok := replicaTables[entity]
	if !ok {
		return false, fmt.Errorf("unsupported replica entity %q", entity)
	}
	var one int
	err := s.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// GetModelCooldownReplicaState returns the complete persisted backoff state for one channel.
func (s *SQLStore) GetModelCooldownReplicaState(ctx context.Context, channelID int64) (map[string]ReplicaCooldownState, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT model, cooldown_until, cooldown_duration_ms
		FROM channel_model_cooldowns WHERE channel_id = ?
	`, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]ReplicaCooldownState)
	for rows.Next() {
		var modelName string
		var state ReplicaCooldownState
		if err := rows.Scan(&modelName, &state.Until, &state.DurationMs); err != nil {
			return nil, err
		}
		result[modelName] = state
	}
	return result, rows.Err()
}

// GetDisabledURLsReplica returns one channel aggregate without loading every channel's state.
func (s *SQLStore) GetDisabledURLsReplica(ctx context.Context, channelID int64) ([]string, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT url FROM channel_url_states
		WHERE channel_id = ? AND disabled = 1
		ORDER BY url_hash
	`, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var urls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, err
		}
		urls = append(urls, url)
	}
	return urls, rows.Err()
}

// SyncChannelAggregateReplica atomically converges every scheduling field for
// one channel. It bypasses business CRUD guards because this is replica repair.
func (s *SQLStore) SyncChannelAggregateReplica(
	ctx context.Context,
	cfg *model.Config,
	keys []*model.APIKey,
	disabledURLs []string,
	modelCooldowns map[string]ReplicaCooldownState,
) error {
	if cfg == nil || cfg.ID <= 0 {
		return fmt.Errorf("invalid channel aggregate replica")
	}
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.syncConfigReplicaTx(ctx, tx, cfg); err != nil {
			return err
		}
		if err := s.replaceAPIKeysReplicaTx(ctx, tx, cfg.ID, keys); err != nil {
			return err
		}
		if err := s.replaceDisabledURLsReplicaTx(ctx, tx, cfg.ID, disabledURLs); err != nil {
			return err
		}
		return s.replaceModelCooldownsReplicaTx(ctx, tx, cfg.ID, modelCooldowns)
	})
	if err != nil {
		return fmt.Errorf("sync channel aggregate replica: %w", err)
	}
	s.unmarkChannelDeleted(cfg.ID)
	return nil
}

func (s *SQLStore) replaceAPIKeysReplicaTx(ctx context.Context, tx *sql.Tx, channelID int64, keys []*model.APIKey) error {
	if _, err := s.execTx(ctx, tx, `DELETE FROM api_keys WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear API key replica: %w", err)
	}
	now := timeToUnix(time.Now())
	for _, key := range keys {
		if key == nil {
			return fmt.Errorf("nil API key replica")
		}
		strategy := key.KeyStrategy
		if strategy == "" {
			strategy = model.KeyStrategySequential
		}
		if _, err := s.execTx(ctx, tx, `
			INSERT INTO api_keys(channel_id, key_index, api_key, note, key_strategy,
				cooldown_until, cooldown_duration_ms, disabled, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, channelID, key.KeyIndex, key.APIKey, key.Note, strategy,
			key.CooldownUntil, key.CooldownDurationMs, key.Disabled, now, now); err != nil {
			return fmt.Errorf("insert API key replica: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) replaceDisabledURLsReplicaTx(ctx context.Context, tx *sql.Tx, channelID int64, urls []string) error {
	if _, err := s.execTx(ctx, tx, `DELETE FROM channel_url_states WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear URL state replica: %w", err)
	}
	now := timeToUnix(time.Now())
	for _, url := range urls {
		if _, err := s.execTx(ctx, tx, `
			INSERT INTO channel_url_states(channel_id, url_hash, url, disabled, updated_at)
			VALUES(?, ?, ?, 1, ?)
		`, channelID, urlHash(url), url, now); err != nil {
			return fmt.Errorf("insert URL state replica: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) replaceModelCooldownsReplicaTx(
	ctx context.Context,
	tx *sql.Tx,
	channelID int64,
	cooldowns map[string]ReplicaCooldownState,
) error {
	if _, err := s.execTx(ctx, tx, `DELETE FROM channel_model_cooldowns WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear model cooldown replica: %w", err)
	}
	now := timeToUnix(time.Now())
	for modelName, state := range cooldowns {
		if _, err := s.execTx(ctx, tx, `
			INSERT INTO channel_model_cooldowns(channel_id, model, cooldown_until, cooldown_duration_ms, updated_at)
			VALUES(?, ?, ?, ?, ?)
		`, channelID, modelName, state.Until, state.DurationMs, now); err != nil {
			return fmt.Errorf("insert model cooldown replica: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) deleteChannelReplicaTx(ctx context.Context, tx *sql.Tx, channelID int64) error {
	statements := []string{
		`DELETE FROM api_keys WHERE channel_id = ?`,
		`DELETE FROM channel_models WHERE channel_id = ?`,
		`DELETE FROM channel_model_cooldowns WHERE channel_id = ?`,
		`DELETE FROM channel_url_states WHERE channel_id = ?`,
		`UPDATE model_fingerprints SET channel_id = NULL WHERE channel_id = ?`,
		`DELETE FROM debug_logs WHERE log_id IN (SELECT id FROM logs WHERE channel_id = ?)`,
		`DELETE FROM logs WHERE channel_id = ?`,
		`DELETE FROM channels WHERE id = ?`,
	}
	for _, statement := range statements {
		if _, err := s.execTx(ctx, tx, statement, channelID); err != nil {
			return fmt.Errorf("delete stale channel replica %d: %w", channelID, err)
		}
	}
	return nil
}

// replaceChannelSchedulingReplicaTx removes only the mutable scheduling
// aggregate. Historical logs and fingerprint references keep the same channel ID.
func (s *SQLStore) replaceChannelSchedulingReplicaTx(ctx context.Context, tx *sql.Tx, channelID int64) error {
	statements := []string{
		`DELETE FROM api_keys WHERE channel_id = ?`,
		`DELETE FROM channel_models WHERE channel_id = ?`,
		`DELETE FROM channel_model_cooldowns WHERE channel_id = ?`,
		`DELETE FROM channel_url_states WHERE channel_id = ?`,
		`DELETE FROM channels WHERE id = ?`,
	}
	for _, statement := range statements {
		if _, err := s.execTx(ctx, tx, statement, channelID); err != nil {
			return fmt.Errorf("replace channel scheduling replica %d: %w", channelID, err)
		}
	}
	return nil
}
