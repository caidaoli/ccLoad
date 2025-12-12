package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ==================== API Keys CRUD 实现 ====================
// [INFO] Linus风格：删除轮询指针数据库代码，已改用内存atomic计数器

// GetAPIKeys 获取指定渠道的所有 API Key（按 key_index 升序）
func (s *SQLStore) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
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
			&key.ID,
			&key.ChannelID,
			&key.KeyIndex,
			&key.APIKey,
			&key.KeyStrategy,
			&key.CooldownUntil,
			&key.CooldownDurationMs,
			&createdAt,
			&updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}

		key.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
		key.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}
		keys = append(keys, key)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}

	return keys, nil
}

// GetAPIKey 获取指定渠道的特定 API Key
func (s *SQLStore) GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error) {
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
		&key.ID,
		&key.ChannelID,
		&key.KeyIndex,
		&key.APIKey,
		&key.KeyStrategy,
		&key.CooldownUntil,
		&key.CooldownDurationMs,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("api key not found")
		}
		return nil, fmt.Errorf("query api key: %w", err)
	}

	key.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
	key.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}

	return key, nil
}

// CreateAPIKey 创建新的 API Key
func (s *SQLStore) CreateAPIKey(ctx context.Context, key *model.APIKey) error {
	if key == nil {
		return errors.New("api key cannot be nil")
	}

	nowUnix := timeToUnix(time.Now())

	// 确保默认值
	if key.KeyStrategy == "" {
		key.KeyStrategy = model.KeyStrategySequential
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

	// 触发异步Redis同步(确保新增操作同步到Redis)
	s.triggerAsyncSync(syncChannels)

	return nil
}

// UpdateAPIKey 更新 API Key 信息
func (s *SQLStore) UpdateAPIKey(ctx context.Context, key *model.APIKey) error {
	if key == nil {
		return errors.New("api key cannot be nil")
	}

	updatedAtUnix := timeToUnix(time.Now())

	// 确保默认值
	if key.KeyStrategy == "" {
		key.KeyStrategy = model.KeyStrategySequential
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

	// 触发异步Redis同步(确保更新操作同步到Redis)
	s.triggerAsyncSync(syncChannels)

	return nil
}

// DeleteAPIKey 删除指定的 API Key
func (s *SQLStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM api_keys
		WHERE channel_id = ? AND key_index = ?
	`, channelID, keyIndex)

	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}

	// 触发异步Redis同步(确保删除操作同步到Redis)
	s.triggerAsyncSync(syncChannels)

	return nil
}

// CompactKeyIndices 将指定渠道中 key_index > removedIndex 的记录整体前移，保持索引连续
// 设计原因：KeySelector 使用 key_index 作为逻辑下标；存在间隙会导致轮询和索引匹配异常
func (s *SQLStore) CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET key_index = key_index - 1
		WHERE channel_id = ? AND key_index > ?
	`, channelID, removedIndex)
	if err != nil {
		return fmt.Errorf("compact key indices: %w", err)
	}

	// 触发异步Redis同步，确保索引更新同步到缓存
	s.triggerAsyncSync(syncChannels)
	return nil
}

// DeleteAllAPIKeys 删除渠道的所有 API Key（用于渠道删除时级联清理）
func (s *SQLStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM api_keys
		WHERE channel_id = ?
	`, channelID)

	if err != nil {
		return fmt.Errorf("delete all api keys: %w", err)
	}

	return nil
}

// ==================== 批量导入优化 (P3性能优化) ====================

// ImportChannelBatch 批量导入渠道配置（原子性+性能优化）
// 单事务+预编译语句，提升CSV导入性能
// [INFO] ACID原则：确保批量导入的原子性（要么全部成功，要么全部回滚）
//
// 参数:
//   - channels: 渠道配置和API Keys的批量数据
//
// 返回:
//   - created: 新创建的渠道数量
//   - updated: 更新的渠道数量
//   - error: 导入失败时的错误信息
func (s *SQLStore) ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error) {
	if len(channels) == 0 {
		return 0, 0, nil
	}

	// 预加载现有渠道名称集合（用于区分创建/更新）
	existingConfigs, err := s.ListConfigs(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("query existing channels: %w", err)
	}
	existingNames := make(map[string]struct{}, len(existingConfigs))
	for _, ec := range existingConfigs {
		existingNames[ec.Name] = struct{}{}
	}

	// 使用事务确保原子性
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		nowUnix := timeToUnix(time.Now())

		// 预编译渠道插入语句（复用，减少解析开销）
		var channelUpsertSQL string
		if s.IsSQLite() {
			channelUpsertSQL = `
				INSERT INTO channels(name, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(name) DO UPDATE SET
					url = excluded.url,
					priority = excluded.priority,
					models = excluded.models,
					model_redirects = excluded.model_redirects,
					channel_type = excluded.channel_type,
					enabled = excluded.enabled,
					updated_at = excluded.updated_at`
		} else {
			channelUpsertSQL = `
				INSERT INTO channels(name, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
					url = VALUES(url),
					priority = VALUES(priority),
					models = VALUES(models),
					model_redirects = VALUES(model_redirects),
					channel_type = VALUES(channel_type),
					enabled = VALUES(enabled),
					updated_at = VALUES(updated_at)`
		}
		channelStmt, err := tx.PrepareContext(ctx, channelUpsertSQL)
		if err != nil {
			return fmt.Errorf("prepare channel statement: %w", err)
		}
		defer channelStmt.Close()

		// 预编译API Key插入语句
		keyStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
			                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			return fmt.Errorf("prepare api key statement: %w", err)
		}
		defer keyStmt.Close()

		// 批量导入渠道
		for _, cwk := range channels {
			config := cwk.Config

			// 标准化数据
			modelsStr, _ := util.SerializeJSON(config.Models, "[]")
			modelRedirectsStr, _ := util.SerializeJSON(config.ModelRedirects, "{}")
			channelType := config.GetChannelType()

			// 检查是否为更新操作
			_, isUpdate := existingNames[config.Name]

			// 插入或更新渠道配置
			_, err := channelStmt.ExecContext(ctx,
				config.Name, config.URL, config.Priority,
				modelsStr, modelRedirectsStr, channelType,
				boolToInt(config.Enabled), nowUnix, nowUnix)
			if err != nil {
				return fmt.Errorf("import channel %s: %w", config.Name, err)
			}

			// 获取渠道ID
			var channelID int64
			err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, config.Name).Scan(&channelID)
			if err != nil {
				return fmt.Errorf("get channel id for %s: %w", config.Name, err)
			}

			// 删除旧的API Keys和模型索引（如果是更新）
			if isUpdate {
				if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE channel_id = ?`, channelID); err != nil {
					return fmt.Errorf("delete old api keys for channel %d: %w", channelID, err)
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, channelID); err != nil {
					return fmt.Errorf("delete old model indices for channel %d: %w", channelID, err)
				}
			}

			// 同步模型索引到 channel_models 表
			var modelInsertSQL string
			if s.IsSQLite() {
				modelInsertSQL = `INSERT OR IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
			} else {
				modelInsertSQL = `INSERT IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
			}
			for _, model := range config.Models {
				if _, err := tx.ExecContext(ctx, modelInsertSQL, channelID, model); err != nil {
					return fmt.Errorf("insert model index %s for channel %d: %w", model, channelID, err)
				}
			}

			// 批量插入API Keys（使用预编译语句）
			for _, key := range cwk.APIKeys {
				_, err := keyStmt.ExecContext(ctx,
					channelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
					key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)
				if err != nil {
					return fmt.Errorf("insert api key %d for channel %d: %w", key.KeyIndex, channelID, err)
				}
			}

			// 统计
			if isUpdate {
				updated++
			} else {
				created++
				existingNames[config.Name] = struct{}{} // 加入集合，避免后续重复计算
			}
		}

		return nil
	})

	if err != nil {
		return 0, 0, err
	}

	// 异步同步到Redis（非阻塞）
	s.triggerAsyncSync(syncChannels)

	return created, updated, nil
}

// GetAllAPIKeys 批量查询所有API Keys
// [INFO] 消除N+1问题：一次查询获取所有渠道的Keys，避免逐个查询
// 返回: map[channelID][]*APIKey
func (s *SQLStore) GetAllAPIKeys(ctx context.Context) (map[int64][]*model.APIKey, error) {
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
			&key.ID,
			&key.ChannelID,
			&key.KeyIndex,
			&key.APIKey,
			&key.KeyStrategy,
			&key.CooldownUntil,
			&key.CooldownDurationMs,
			&createdAt,
			&updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}

		key.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
		key.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}

		result[key.ChannelID] = append(result[key.ChannelID], key)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}

	return result, nil
}
