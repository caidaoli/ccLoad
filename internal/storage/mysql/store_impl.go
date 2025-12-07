package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ==================== Config CRUD 实现 ====================

func (s *MySQLStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	query := `
		SELECT c.id, c.name, c.url, c.priority, c.models, c.model_redirects, c.channel_type, c.enabled,
		       c.cooldown_until, c.cooldown_duration_ms,
		       COUNT(k.id) as key_count,
		       c.rr_key_index, c.created_at, c.updated_at
		FROM channels c
		LEFT JOIN api_keys k ON c.id = k.channel_id
		GROUP BY c.id
		ORDER BY c.priority DESC, c.id ASC
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *MySQLStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	query := `
		SELECT c.id, c.name, c.url, c.priority, c.models, c.model_redirects, c.channel_type, c.enabled,
		       c.cooldown_until, c.cooldown_duration_ms,
		       COUNT(k.id) as key_count,
		       c.rr_key_index, c.created_at, c.updated_at
		FROM channels c
		LEFT JOIN api_keys k ON c.id = k.channel_id
		WHERE c.id = ?
		GROUP BY c.id
	`
	row := s.db.QueryRowContext(ctx, query, id)

	scanner := NewConfigScanner()
	config, err := scanner.ScanConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("not found")
		}
		return nil, err
	}
	return config, nil
}

func (s *MySQLStore) GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	var query string
	var args []any
	nowUnix := time.Now().Unix()

	if modelName == "*" {
		query = `
            SELECT c.id, c.name, c.url, c.priority,
                   c.models, c.model_redirects, c.channel_type, c.enabled,
                   c.cooldown_until, c.cooldown_duration_ms,
                   COUNT(k.id) as key_count,
                   c.rr_key_index, c.created_at, c.updated_at
            FROM channels c
            LEFT JOIN api_keys k ON c.id = k.channel_id
            WHERE c.enabled = 1
              AND (c.cooldown_until = 0 OR c.cooldown_until <= ?)
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{nowUnix}
	} else {
		query = `
            SELECT c.id, c.name, c.url, c.priority,
                   c.models, c.model_redirects, c.channel_type, c.enabled,
                   c.cooldown_until, c.cooldown_duration_ms,
                   COUNT(k.id) as key_count,
                   c.rr_key_index, c.created_at, c.updated_at
            FROM channels c
            INNER JOIN channel_models cm ON c.id = cm.channel_id
            LEFT JOIN api_keys k ON c.id = k.channel_id
            WHERE c.enabled = 1
              AND cm.model = ?
              AND (c.cooldown_until = 0 OR c.cooldown_until <= ?)
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{modelName, nowUnix}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *MySQLStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	nowUnix := time.Now().Unix()
	query := `
		SELECT c.id, c.name, c.url, c.priority,
		       c.models, c.model_redirects, c.channel_type, c.enabled,
		       c.cooldown_until, c.cooldown_duration_ms,
		       COUNT(k.id) as key_count,
		       c.rr_key_index, c.created_at, c.updated_at
		FROM channels c
		LEFT JOIN api_keys k ON c.id = k.channel_id
		WHERE c.enabled = 1
		  AND c.channel_type = ?
		  AND (c.cooldown_until = 0 OR c.cooldown_until <= ?)
		GROUP BY c.id
		ORDER BY c.priority DESC, c.id ASC
	`

	rows, err := s.db.QueryContext(ctx, query, channelType, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *MySQLStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := time.Now().Unix()
	modelsStr, _ := util.SerializeJSON(c.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(c.ModelRedirects, "{}")
	channelType := c.GetChannelType()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels(name, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Name, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)

	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	// 同步模型数据（MySQL: INSERT IGNORE）
	for _, model := range c.Models {
		if _, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO channel_models (channel_id, model, created_at)
			VALUES (?, ?, ?)
		`, id, model, nowUnix); err != nil {
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	s.triggerAsyncSync()
	return config, nil
}

func (s *MySQLStore) UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error) {
	if upd == nil {
		return nil, errors.New("update payload cannot be nil")
	}

	if _, err := s.GetConfig(ctx, id); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(upd.Name)
	url := strings.TrimSpace(upd.URL)
	modelsStr, _ := util.SerializeJSON(upd.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(upd.ModelRedirects, "{}")
	channelType := upd.GetChannelType()
	updatedAtUnix := time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		UPDATE channels
		SET name=?, url=?, priority=?, models=?, model_redirects=?, channel_type=?, enabled=?, updated_at=?
		WHERE id=?
	`, name, url, upd.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(upd.Enabled), updatedAtUnix, id)
	if err != nil {
		return nil, err
	}

	// 同步更新 channel_models
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, id); err != nil {
		log.Printf("Warning: Failed to delete old model indices: %v", err)
	}

	for _, model := range upd.Models {
		if _, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO channel_models (channel_id, model, created_at)
			VALUES (?, ?, ?)
		`, id, model, updatedAtUnix); err != nil {
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	s.triggerAsyncSync()
	return config, nil
}

func (s *MySQLStore) ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := time.Now().Unix()
	modelsStr, _ := util.SerializeJSON(c.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(c.ModelRedirects, "{}")
	channelType := c.GetChannelType()

	// MySQL: ON DUPLICATE KEY UPDATE
	_, err := s.db.ExecContext(ctx, `
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
	`, c.Name, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)
	if err != nil {
		return nil, err
	}

	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, c.Name).Scan(&id)
	if err != nil {
		return nil, err
	}

	// 同步更新 channel_models
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, id); err != nil {
		log.Printf("Warning: Failed to delete old model indices: %v", err)
	}

	for _, model := range c.Models {
		if _, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO channel_models (channel_id, model, created_at)
			VALUES (?, ?, ?)
		`, id, model, nowUnix); err != nil {
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func (s *MySQLStore) DeleteConfig(ctx context.Context, id int64) error {
	if _, err := s.GetConfig(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return err
	}

	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.triggerAsyncSync()
	return nil
}
