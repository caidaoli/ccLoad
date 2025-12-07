package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

func (s *MySQLStore) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	query := `
		SELECT id, channel_id, key_index, api_key, key_strategy,
		       cooldown_until, cooldown_duration_ms, created_at, updated_at
		FROM api_keys
		WHERE channel_id = ?
		ORDER BY key_index ASC
	`
	rows, err := s.db.QueryContext(ctx, query, channelID)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()

	var keys []*model.APIKey
	for rows.Next() {
		key := &model.APIKey{}
		var createdAt, updatedAt int64

		err := rows.Scan(
			&key.ID, &key.ChannelID, &key.KeyIndex, &key.APIKey, &key.KeyStrategy,
			&key.CooldownUntil, &key.CooldownDurationMs, &createdAt, &updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}

		key.CreatedAt = model.JSONTime{Time: time.Unix(createdAt, 0)}
		key.UpdatedAt = model.JSONTime{Time: time.Unix(updatedAt, 0)}
		keys = append(keys, key)
	}

	return keys, rows.Err()
}

func (s *MySQLStore) GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error) {
	query := `
		SELECT id, channel_id, key_index, api_key, key_strategy,
		       cooldown_until, cooldown_duration_ms, created_at, updated_at
		FROM api_keys
		WHERE channel_id = ? AND key_index = ?
	`
	row := s.db.QueryRowContext(ctx, query, channelID, keyIndex)

	key := &model.APIKey{}
	var createdAt, updatedAt int64

	err := row.Scan(
		&key.ID, &key.ChannelID, &key.KeyIndex, &key.APIKey, &key.KeyStrategy,
		&key.CooldownUntil, &key.CooldownDurationMs, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("api key not found")
		}
		return nil, fmt.Errorf("query api key: %w", err)
	}

	key.CreatedAt = model.JSONTime{Time: time.Unix(createdAt, 0)}
	key.UpdatedAt = model.JSONTime{Time: time.Unix(updatedAt, 0)}

	return key, nil
}

func (s *MySQLStore) CreateAPIKey(ctx context.Context, key *model.APIKey) error {
	if key == nil {
		return errors.New("api key cannot be nil")
	}

	nowUnix := time.Now().Unix()
	if key.KeyStrategy == "" {
		key.KeyStrategy = "sequential"
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
		                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, key.ChannelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
		key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)

	if err != nil {
		return fmt.Errorf("insert api key: %w", err)
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) UpdateAPIKey(ctx context.Context, key *model.APIKey) error {
	if key == nil {
		return errors.New("api key cannot be nil")
	}

	updatedAtUnix := time.Now().Unix()
	if key.KeyStrategy == "" {
		key.KeyStrategy = "sequential"
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET api_key = ?, key_strategy = ?,
		    cooldown_until = ?, cooldown_duration_ms = ?,
		    updated_at = ?
		WHERE channel_id = ? AND key_index = ?
	`, key.APIKey, key.KeyStrategy,
		key.CooldownUntil, key.CooldownDurationMs,
		updatedAtUnix, key.ChannelID, key.KeyIndex)

	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM api_keys WHERE channel_id = ? AND key_index = ?
	`, channelID, keyIndex)

	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET key_index = key_index - 1
		WHERE channel_id = ? AND key_index > ?
	`, channelID, removedIndex)
	if err != nil {
		return fmt.Errorf("compact key indices: %w", err)
	}

	s.triggerAsyncSync()
	return nil
}

func (s *MySQLStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE channel_id = ?`, channelID)
	if err != nil {
		return fmt.Errorf("delete all api keys: %w", err)
	}
	return nil
}

// ImportChannelBatch 批量导入（MySQL: ON DUPLICATE KEY UPDATE）
func (s *MySQLStore) ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error) {
	if len(channels) == 0 {
		return 0, 0, nil
	}

	existingConfigs, err := s.ListConfigs(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("query existing channels: %w", err)
	}
	existingNames := make(map[string]struct{}, len(existingConfigs))
	for _, ec := range existingConfigs {
		existingNames[ec.Name] = struct{}{}
	}

	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		nowUnix := time.Now().Unix()

		channelStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO channels(name, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				url = VALUES(url),
				priority = VALUES(priority),
				models = VALUES(models),
				model_redirects = VALUES(model_redirects),
				channel_type = VALUES(channel_type),
				enabled = VALUES(enabled),
				updated_at = VALUES(updated_at)
		`)
		if err != nil {
			return fmt.Errorf("prepare channel statement: %w", err)
		}
		defer channelStmt.Close()

		keyStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
			                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			return fmt.Errorf("prepare api key statement: %w", err)
		}
		defer keyStmt.Close()

		for _, cwk := range channels {
			config := cwk.Config

			modelsStr, _ := util.SerializeJSON(config.Models, "[]")
			modelRedirectsStr, _ := util.SerializeJSON(config.ModelRedirects, "{}")
			channelType := config.GetChannelType()

			_, isUpdate := existingNames[config.Name]

			_, err := channelStmt.ExecContext(ctx,
				config.Name, config.URL, config.Priority,
				modelsStr, modelRedirectsStr, channelType,
				boolToInt(config.Enabled), nowUnix, nowUnix)
			if err != nil {
				return fmt.Errorf("import channel %s: %w", config.Name, err)
			}

			var channelID int64
			err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, config.Name).Scan(&channelID)
			if err != nil {
				return fmt.Errorf("get channel id for %s: %w", config.Name, err)
			}

			if isUpdate {
				_, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE channel_id = ?`, channelID)
				if err != nil {
					return fmt.Errorf("delete old api keys for channel %d: %w", channelID, err)
				}
			}

			for _, key := range cwk.APIKeys {
				_, err := keyStmt.ExecContext(ctx,
					channelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
					key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)
				if err != nil {
					return fmt.Errorf("insert api key %d for channel %d: %w", key.KeyIndex, channelID, err)
				}
			}

			if isUpdate {
				updated++
			} else {
				created++
				existingNames[config.Name] = struct{}{}
			}
		}

		return nil
	})

	if err != nil {
		return 0, 0, err
	}

	s.triggerAsyncSync()
	return created, updated, nil
}

func (s *MySQLStore) GetAllAPIKeys(ctx context.Context) (map[int64][]*model.APIKey, error) {
	query := `
		SELECT id, channel_id, key_index, api_key, key_strategy,
		       cooldown_until, cooldown_duration_ms, created_at, updated_at
		FROM api_keys
		ORDER BY channel_id ASC, key_index ASC
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query all api keys: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]*model.APIKey)
	for rows.Next() {
		key := &model.APIKey{}
		var createdAt, updatedAt int64

		err := rows.Scan(
			&key.ID, &key.ChannelID, &key.KeyIndex, &key.APIKey, &key.KeyStrategy,
			&key.CooldownUntil, &key.CooldownDurationMs, &createdAt, &updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}

		key.CreatedAt = model.JSONTime{Time: time.Unix(createdAt, 0)}
		key.UpdatedAt = model.JSONTime{Time: time.Unix(updatedAt, 0)}

		result[key.ChannelID] = append(result[key.ChannelID], key)
	}

	return result, rows.Err()
}
