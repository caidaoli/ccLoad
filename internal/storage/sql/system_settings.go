package sql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

// GetSetting 获取单个配置项
func (s *SQLStore) GetSetting(ctx context.Context, key string) (*model.SystemSetting, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+"`key`"+`, `+"`value`"+`, value_type, description, default_value, updated_at
		FROM system_settings
		WHERE `+"`key`"+` = ?
	`, key)

	var setting model.SystemSetting
	if err := row.Scan(&setting.Key, &setting.Value, &setting.ValueType, &setting.Description, &setting.DefaultValue, &setting.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, model.ErrSettingNotFound
		}
		return nil, fmt.Errorf("query setting: %w", err)
	}

	return &setting, nil
}

// ListAllSettings 获取所有配置项
func (s *SQLStore) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+"`key`"+`, `+"`value`"+`, value_type, description, default_value, updated_at
		FROM system_settings
		ORDER BY `+"`key`"+` ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query all settings: %w", err)
	}
	defer rows.Close()

	var settings []*model.SystemSetting
	for rows.Next() {
		var setting model.SystemSetting
		if err := rows.Scan(&setting.Key, &setting.Value, &setting.ValueType, &setting.Description, &setting.DefaultValue, &setting.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		settings = append(settings, &setting)
	}

	return settings, rows.Err()
}

// UpdateSetting 更新配置项(仅更新value和updated_at)
func (s *SQLStore) UpdateSetting(ctx context.Context, key, value string) error {
	now := timeToUnix(time.Now())

	result, err := s.db.ExecContext(ctx, `
		UPDATE system_settings
		SET `+"`value`"+` = ?, updated_at = ?
		WHERE `+"`key`"+` = ?
	`, value, now, key)
	if err != nil {
		return fmt.Errorf("update setting: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return model.ErrSettingNotFound
	}

	return nil
}

// BatchUpdateSettings 批量更新配置项(事务保护)
func (s *SQLStore) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		now := timeToUnix(time.Now())

		stmt, err := tx.PrepareContext(ctx, `
			UPDATE system_settings
			SET `+"`value`"+` = ?, updated_at = ?
			WHERE `+"`key`"+` = ?
		`)
		if err != nil {
			return fmt.Errorf("prepare statement: %w", err)
		}
		defer stmt.Close()

		for key, value := range updates {
			result, err := stmt.ExecContext(ctx, value, now, key)
			if err != nil {
				return fmt.Errorf("update setting %s: %w", key, err)
			}

			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("check rows affected for %s: %w", key, err)
			}
			if rows == 0 {
				return model.ErrSettingNotFound
			}
		}

		return nil
	})
}
