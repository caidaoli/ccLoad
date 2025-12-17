package sql

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

func (s *SQLStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	// 添加 key_count 字段，避免 N+1 查询
	// 使用 LEFT JOIN 支持查询有或无API Key的渠道
	query := `
			SELECT c.id, c.name, c.url, c.priority, c.models, c.model_redirects, c.channel_type, c.enabled,
			       c.cooldown_until, c.cooldown_duration_ms,
			       COUNT(k.id) as key_count,
			       c.created_at, c.updated_at
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

	// 使用统一的扫描器(注入Dialect)
	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *SQLStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	// 新架构：包含内联的轮询索引字段
	// 使用 LEFT JOIN 以支持创建渠道时（尚无API Key）仍能获取配置
	query := `
			SELECT c.id, c.name, c.url, c.priority, c.models, c.model_redirects, c.channel_type, c.enabled,
			       c.cooldown_until, c.cooldown_duration_ms,
			       COUNT(k.id) as key_count,
			       c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN api_keys k ON c.id = k.channel_id
			WHERE c.id = ?
			GROUP BY c.id
	`
	row := s.db.QueryRowContext(ctx, query, id)

	// 使用统一的扫描器(注入Dialect)
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

// GetEnabledChannelsByModel 查询支持指定模型的启用渠道（按优先级排序）
func (s *SQLStore) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*model.Config, error) {
	var query string
	var args []any
	nowUnix := timeToUnix(time.Now())

	if model == "*" {
		// 通配符：返回所有启用的渠道（新架构：从 channels 表读取内联冷却字段）
		// 使用 LEFT JOIN 支持查询有或无API Key的渠道
		query = `
	            SELECT c.id, c.name, c.url, c.priority,
	                   c.models, c.model_redirects, c.channel_type, c.enabled,
	                   c.cooldown_until, c.cooldown_duration_ms,
	                   COUNT(k.id) as key_count,
	                   c.created_at, c.updated_at
	            FROM channels c
	            LEFT JOIN api_keys k ON c.id = k.channel_id
	            WHERE c.enabled = 1
	              AND (c.cooldown_until = 0 OR c.cooldown_until <= ?)
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{nowUnix}
	} else {
		// 精确匹配：使用去规范化的 channel_models 索引表（性能优化：消除JSON查询）
		// 使用 LEFT JOIN 支持查询有或无API Key的渠道
		query = `
	            SELECT c.id, c.name, c.url, c.priority,
	                   c.models, c.model_redirects, c.channel_type, c.enabled,
	                   c.cooldown_until, c.cooldown_duration_ms,
	                   COUNT(k.id) as key_count,
	                   c.created_at, c.updated_at
	            FROM channels c
	            INNER JOIN channel_models cm ON c.id = cm.channel_id
	            LEFT JOIN api_keys k ON c.id = k.channel_id
	            WHERE c.enabled = 1
              AND cm.model = ?
              AND (c.cooldown_until = 0 OR c.cooldown_until <= ?)
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{model, nowUnix}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

// GetEnabledChannelsByType 查询指定类型的启用渠道（按优先级排序）
// 新架构：从 channels 表读取内联冷却字段，不再 JOIN cooldowns 表
// 使用 LEFT JOIN 支持查询有或无API Key的渠道
func (s *SQLStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	nowUnix := timeToUnix(time.Now())
	query := `
			SELECT c.id, c.name, c.url, c.priority,
			       c.models, c.model_redirects, c.channel_type, c.enabled,
			       c.cooldown_until, c.cooldown_duration_ms,
			       COUNT(k.id) as key_count,
			       c.created_at, c.updated_at
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

func (s *SQLStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := timeToUnix(time.Now())
	modelsStr, _ := util.SerializeJSON(c.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(c.ModelRedirects, "{}")

	// 使用GetChannelType确保默认值
	channelType := c.GetChannelType()

	// 新架构：API Keys 不再存储在 channels 表中
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels(name, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Name, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)

	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}

	// 同步模型数据到 channel_models 索引表（性能优化：去规范化）
	for _, model := range c.Models {
		var insertSQL string
		if s.IsSQLite() {
			insertSQL = `INSERT OR IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		} else {
			insertSQL = `INSERT IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		}
		if _, err := s.db.ExecContext(ctx, insertSQL, id, model); err != nil {
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	// 获取完整的配置信息
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 异步同步渠道配置到Redis（非阻塞，立即返回）
	s.triggerAsyncSync(syncChannels)

	return config, nil
}

func (s *SQLStore) UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error) {
	if upd == nil {
		return nil, errors.New("update payload cannot be nil")
	}

	// 确认目标存在，保持与之前逻辑一致
	if _, err := s.GetConfig(ctx, id); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(upd.Name)
	url := strings.TrimSpace(upd.URL)
	modelsStr, _ := util.SerializeJSON(upd.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(upd.ModelRedirects, "{}")

	// 使用GetChannelType确保默认值
	channelType := upd.GetChannelType()
	updatedAtUnix := timeToUnix(time.Now())

	// 新架构：API Keys 不再存储在 channels 表中，通过单独的 CreateAPIKey/UpdateAPIKey/DeleteAPIKey 管理
	_, err := s.db.ExecContext(ctx, `
		UPDATE channels
		SET name=?, url=?, priority=?, models=?, model_redirects=?, channel_type=?, enabled=?, updated_at=?
		WHERE id=?
	`, name, url, upd.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(upd.Enabled), updatedAtUnix, id)
	if err != nil {
		return nil, err
	}

	// 同步更新 channel_models 索引表（性能优化：去规范化）
	// 先删除旧的模型索引
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM channel_models WHERE channel_id = ?
	`, id); err != nil {
		// 索引同步失败不影响主要功能，记录警告
		log.Printf("Warning: Failed to delete old model indices: %v", err)
	}

	// 再插入新的模型索引
	for _, model := range upd.Models {
		var insertSQL string
		if s.IsSQLite() {
			insertSQL = `INSERT OR IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		} else {
			insertSQL = `INSERT IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		}
		if _, err := s.db.ExecContext(ctx, insertSQL, id, model); err != nil {
			// 索引同步失败不影响主要功能，记录警告
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	// 获取更新后的配置
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 异步同步渠道配置到Redis（非阻塞，立即返回）
	s.triggerAsyncSync(syncChannels)

	return config, nil
}

func (s *SQLStore) ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := timeToUnix(time.Now())
	modelsStr, _ := util.SerializeJSON(c.Models, "[]")
	modelRedirectsStr, _ := util.SerializeJSON(c.ModelRedirects, "{}")

	// 使用GetChannelType确保默认值
	channelType := c.GetChannelType()

	// 新架构：API Keys 不再存储在 channels 表中，通过单独的 CreateAPIKey 管理
	var upsertSQL string
	if s.IsSQLite() {
		upsertSQL = `
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
		upsertSQL = `
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
	_, err := s.db.ExecContext(ctx, upsertSQL, c.Name, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)
	if err != nil {
		return nil, err
	}

	// 获取实际的记录ID（可能是新创建的或已存在的）
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, c.Name).Scan(&id)
	if err != nil {
		return nil, err
	}

	// 同步更新 channel_models 索引表（性能优化：去规范化）
	// 先删除旧的模型索引
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM channel_models WHERE channel_id = ?
	`, id); err != nil {
		// 索引同步失败不影响主要功能，记录警告
		log.Printf("Warning: Failed to delete old model indices: %v", err)
	}

	// 再插入新的模型索引
	for _, model := range c.Models {
		var insertSQL string
		if s.IsSQLite() {
			insertSQL = `INSERT OR IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		} else {
			insertSQL = `INSERT IGNORE INTO channel_models (channel_id, model) VALUES (?, ?)`
		}
		if _, err := s.db.ExecContext(ctx, insertSQL, id, model); err != nil {
			log.Printf("Warning: Failed to sync model %s to channel_models: %v", model, err)
		}
	}

	// 获取完整的配置信息
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 注意: ReplaceConfig通常在批量导入时使用，最后会统一触发Redis同步
	// 这里不做单独同步，避免CSV导入时的N次Redis操作

	return config, nil
}

func (s *SQLStore) DeleteConfig(ctx context.Context, id int64) error {
	// 检查记录是否存在（幂等性）
	if _, err := s.GetConfig(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // 记录不存在，直接返回
		}
		return err
	}

	// 删除渠道配置（FOREIGN KEY CASCADE 自动级联删除 api_keys 和 key_rr）
	// 使用事务高阶函数，消除重复代码（DRY原则）
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 异步同步渠道配置到Redis（非阻塞，立即返回）
	s.triggerAsyncSync(syncChannels)

	return nil
}

// BatchUpdatePriority 批量更新渠道优先级
// 性能优化：使用单条批量UPDATE + CASE WHEN语句，性能提升90倍（45渠道：90次→1次）
func (s *SQLStore) BatchUpdatePriority(ctx context.Context, updates []struct{ ID int64; Priority int }) (int64, error) {
	if len(updates) == 0 {
		return 0, nil
	}

	updatedAtUnix := timeToUnix(time.Now())

	// 构建批量UPDATE语句
	var caseBuilder strings.Builder
	var ids []int64

	caseBuilder.WriteString("UPDATE channels SET priority = CASE id ")
	for _, update := range updates {
		caseBuilder.WriteString(fmt.Sprintf("WHEN %d THEN %d ", update.ID, update.Priority))
		ids = append(ids, update.ID)
	}
	caseBuilder.WriteString("END, updated_at = ? WHERE id IN (")

	for i := range ids {
		if i > 0 {
			caseBuilder.WriteString(",")
		}
		caseBuilder.WriteString("?")
	}
	caseBuilder.WriteString(")")

	// 构建参数列表：updated_at + ids
	args := []any{updatedAtUnix}
	for _, id := range ids {
		args = append(args, id)
	}

	// 执行批量更新
	result, err := s.db.ExecContext(ctx, caseBuilder.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("batch update priority: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()

	// 异步同步到Redis
	s.triggerAsyncSync(syncChannels)

	return rowsAffected, nil
}
