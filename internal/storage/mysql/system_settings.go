package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

func (s *MySQLStore) GetSetting(ctx context.Context, key string) (*model.SystemSetting, error) {
	row := s.db.QueryRowContext(ctx, "SELECT `key`, value, value_type, description, default_value, updated_at FROM system_settings WHERE `key` = ?", key)

	var setting model.SystemSetting
	if err := row.Scan(&setting.Key, &setting.Value, &setting.ValueType, &setting.Description, &setting.DefaultValue, &setting.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrSettingNotFound
		}
		return nil, fmt.Errorf("query setting: %w", err)
	}

	return &setting, nil
}

func (s *MySQLStore) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT `key`, value, value_type, description, default_value, updated_at FROM system_settings ORDER BY `key` ASC")
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

func (s *MySQLStore) UpdateSetting(ctx context.Context, key, value string) error {
	now := time.Now().Unix()

	result, err := s.db.ExecContext(ctx, "UPDATE system_settings SET value = ?, updated_at = ? WHERE `key` = ?", value, now, key)
	if err != nil {
		return fmt.Errorf("update setting: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return ErrSettingNotFound
	}

	return nil
}

func (s *MySQLStore) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()

		stmt, err := tx.PrepareContext(ctx, "UPDATE system_settings SET value = ?, updated_at = ? WHERE `key` = ?")
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
				return ErrSettingNotFound
			}
		}

		return nil
	})
}
