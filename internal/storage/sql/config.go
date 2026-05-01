package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// ==================== Config CRUD 实现 ====================

// ListConfigs 获取所有渠道配置列表
func (s *SQLStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	// 添加 key_count 字段，避免 N+1 查询
	// 使用 LEFT JOIN 支持查询有或无API Key的渠道
	// 注意：不再从 channels 表读取 models 和 model_redirects
	query := `
			SELECT c.id, c.name, c.url, c.priority, c.channel_type, c.protocol_transform_mode, c.enabled,
			       c.scheduled_check_enabled, c.scheduled_check_model,
			       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
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
	defer func() { _ = rows.Close() }()

	// 使用统一的扫描器
	scanner := NewConfigScanner()
	configs, err := scanner.ScanConfigs(rows)
	if err != nil {
		return nil, err
	}

	if err := s.loadConfigsAuxConcurrent(ctx, configs); err != nil {
		return nil, err
	}

	return configs, nil
}

// GetConfig 根据ID获取渠道配置
func (s *SQLStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	// 使用 LEFT JOIN 以支持创建渠道时（尚无API Key）仍能获取配置
	// 注意：不再从 channels 表读取 models 和 model_redirects
	query := `
			SELECT c.id, c.name, c.url, c.priority, c.channel_type, c.protocol_transform_mode, c.enabled,
			       c.scheduled_check_enabled, c.scheduled_check_model,
			       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
			       COUNT(k.id) as key_count,
			       c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN api_keys k ON c.id = k.channel_id
			WHERE c.id = ?
			GROUP BY c.id
	`
	row := s.db.QueryRowContext(ctx, query, id)

	// 使用统一的扫描器
	scanner := NewConfigScanner()
	config, err := scanner.ScanConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("not found")
		}
		return nil, err
	}

	if err := s.loadConfigsAuxConcurrent(ctx, []*model.Config{config}); err != nil {
		return nil, err
	}

	return config, nil
}

// GetEnabledChannelsByModel 查询支持指定模型的启用渠道（按优先级排序）
func (s *SQLStore) GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	var query string
	var args []any

	if modelName == "*" {
		// 通配符：返回所有启用的渠道
		// 注意：不再从 channels 表读取 models 和 model_redirects
		query = `
	            SELECT c.id, c.name, c.url, c.priority,
	                   c.channel_type, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
	                   c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
	                   COUNT(k.id) as key_count,
	                   c.created_at, c.updated_at
	            FROM channels c
	            LEFT JOIN api_keys k ON c.id = k.channel_id
	            WHERE c.enabled = 1
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
	} else {
		// 精确匹配：使用 channel_models 索引表
		query = `
	            SELECT c.id, c.name, c.url, c.priority,
	                   c.channel_type, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
	                   c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
	                   COUNT(k.id) as key_count,
	                   c.created_at, c.updated_at
	            FROM channels c
	            INNER JOIN channel_models cm ON c.id = cm.channel_id
	            LEFT JOIN api_keys k ON c.id = k.channel_id
	            WHERE c.enabled = 1
              AND cm.model = ?
            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{modelName}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	scanner := NewConfigScanner()
	configs, err := scanner.ScanConfigs(rows)
	if err != nil {
		return nil, err
	}

	// 批量加载所有渠道的模型数据
	if err := s.loadConfigsAuxConcurrent(ctx, configs); err != nil {
		return nil, err
	}

	return configs, nil
}

// GetEnabledChannelsByType 查询指定类型的启用渠道（按优先级排序）
func (s *SQLStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	// 注意：不再从 channels 表读取 models 和 model_redirects
	query := `
			SELECT c.id, c.name, c.url, c.priority,
			       c.channel_type, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
			       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
			       COUNT(k.id) as key_count,
			       c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN api_keys k ON c.id = k.channel_id
			WHERE c.enabled = 1
			  AND c.channel_type = ?
		GROUP BY c.id
		ORDER BY c.priority DESC, c.id ASC
	`

	rows, err := s.db.QueryContext(ctx, query, channelType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	scanner := NewConfigScanner()
	configs, err := scanner.ScanConfigs(rows)
	if err != nil {
		return nil, err
	}

	// 批量加载所有渠道的模型数据
	if err := s.loadConfigsAuxConcurrent(ctx, configs); err != nil {
		return nil, err
	}

	return configs, nil
}

// GetEnabledChannelsByModelAndProtocol 查询支持指定模型且暴露指定客户端协议的启用渠道（按优先级排序）
func (s *SQLStore) GetEnabledChannelsByModelAndProtocol(ctx context.Context, modelName string, protocol string) ([]*model.Config, error) {
	protocol = strings.TrimSpace(strings.ToLower(protocol))
	if protocol == "" {
		return s.GetEnabledChannelsByModel(ctx, modelName)
	}

	args := []any{protocol, protocol}
	query := `
		SELECT c.id, c.name, c.url, c.priority,
		       c.channel_type, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
		       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules,
		       COUNT(k.id) as key_count,
		       c.created_at, c.updated_at
		FROM channels c
		LEFT JOIN api_keys k ON c.id = k.channel_id
		WHERE c.enabled = 1
		  AND (
		      c.channel_type = ?
		      OR EXISTS (
		          SELECT 1
		          FROM channel_protocol_transforms cpt
		          WHERE cpt.channel_id = c.id AND cpt.protocol = ?
		      )
		  )
	`

	if modelName != "*" {
		query += `
		  AND EXISTS (
		      SELECT 1
		      FROM channel_models cm
		      WHERE cm.channel_id = c.id AND cm.model = ?
		  )
	`
		args = append(args, modelName)
	}

	query += `
		GROUP BY c.id
		ORDER BY c.priority DESC, c.id ASC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	scanner := NewConfigScanner()
	configs, err := scanner.ScanConfigs(rows)
	if err != nil {
		return nil, err
	}

	if err := s.loadConfigsAuxConcurrent(ctx, configs); err != nil {
		return nil, err
	}

	configs = filterConfigsByProtocol(configs, protocol)
	return configs, nil
}

// GetEnabledChannelsByExposedProtocol 查询暴露指定客户端协议的启用渠道（按优先级排序）
func (s *SQLStore) GetEnabledChannelsByExposedProtocol(ctx context.Context, protocol string) ([]*model.Config, error) {
	protocol = strings.TrimSpace(strings.ToLower(protocol))
	if protocol == "" {
		return []*model.Config{}, nil
	}
	return s.GetEnabledChannelsByModelAndProtocol(ctx, "*", protocol)
}

// CreateConfig 创建新的渠道配置
func (s *SQLStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := timeToUnix(time.Now())

	// 使用GetChannelType确保默认值
	channelType := c.GetChannelType()
	protocolTransformMode := c.GetProtocolTransformMode()
	customRules, err := marshalCustomRequestRules(c.CustomRequestRules)
	if err != nil {
		return nil, err
	}

	id := c.ID
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			// 插入渠道记录（数据库生成自增 id）
			res, err := tx.ExecContext(ctx, `
				INSERT INTO channels(name, url, priority, channel_type, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, c.Name, c.URL, c.Priority, channelType, protocolTransformMode,
				boolToInt(c.Enabled), boolToInt(c.ScheduledCheckEnabled), c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, nowUnix, nowUnix)
			if err != nil {
				return err
			}

			id, err = res.LastInsertId()
			if err != nil {
				return fmt.Errorf("get last insert id: %w", err)
			}
		} else {
			// 显式主键：用于混合存储同步/恢复，保证两端主键一致
			if s.IsSQLite() {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO channels(id, name, url, priority, channel_type, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, id, c.Name, c.URL, c.Priority, channelType, protocolTransformMode,
					boolToInt(c.Enabled), boolToInt(c.ScheduledCheckEnabled), c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, nowUnix, nowUnix)
				if err != nil {
					return err
				}
			} else {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO channels(id, name, url, priority, channel_type, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					ON DUPLICATE KEY UPDATE
						name = VALUES(name),
						url = VALUES(url),
						priority = VALUES(priority),
						channel_type = VALUES(channel_type),
						protocol_transform_mode = VALUES(protocol_transform_mode),
						enabled = VALUES(enabled),
						scheduled_check_enabled = VALUES(scheduled_check_enabled),
						scheduled_check_model = VALUES(scheduled_check_model),
						daily_cost_limit = VALUES(daily_cost_limit),
						cost_multiplier = VALUES(cost_multiplier),
						custom_request_rules = VALUES(custom_request_rules),
						updated_at = VALUES(updated_at)
				`, id, c.Name, c.URL, c.Priority, channelType, protocolTransformMode,
					boolToInt(c.Enabled), boolToInt(c.ScheduledCheckEnabled), c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, nowUnix, nowUnix)
				if err != nil {
					return err
				}
			}
		}

		// 保存模型数据到 channel_models 表
		if err := s.saveModelEntriesTx(ctx, tx, id, c.ModelEntries); err != nil {
			return fmt.Errorf("save model entries: %w", err)
		}
		if err := s.saveProtocolTransformsTx(ctx, tx, id, c.GetProtocolTransforms()); err != nil {
			return fmt.Errorf("save protocol transforms: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 获取完整的配置信息
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// UpdateConfig 更新渠道配置
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

	// 使用GetChannelType确保默认值
	channelType := upd.GetChannelType()
	protocolTransformMode := upd.GetProtocolTransformMode()
	customRules, err := marshalCustomRequestRules(upd.CustomRequestRules)
	if err != nil {
		return nil, err
	}
	updatedAtUnix := timeToUnix(time.Now())

	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 更新渠道记录
		_, err := tx.ExecContext(ctx, `
			UPDATE channels
			SET name=?, url=?, priority=?, channel_type=?, protocol_transform_mode=?, enabled=?, scheduled_check_enabled=?, scheduled_check_model=?, daily_cost_limit=?, cost_multiplier=?, custom_request_rules=?, updated_at=?
			WHERE id=?
		`, name, url, upd.Priority, channelType, protocolTransformMode,
			boolToInt(upd.Enabled), boolToInt(upd.ScheduledCheckEnabled), upd.ScheduledCheckModel, upd.DailyCostLimit, normalizeCostMultiplier(upd.CostMultiplier), customRules, updatedAtUnix, id)
		if err != nil {
			return err
		}

		// 更新 channel_models 表（先删后插）
		if err := s.saveModelEntriesTx(ctx, tx, id, upd.ModelEntries); err != nil {
			return fmt.Errorf("save model entries: %w", err)
		}
		if err := s.saveProtocolTransformsTx(ctx, tx, id, upd.GetProtocolTransforms()); err != nil {
			return fmt.Errorf("save protocol transforms: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 获取更新后的配置
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// DeleteConfig 删除渠道配置
func (s *SQLStore) DeleteConfig(ctx context.Context, id int64) error {
	// 检查记录是否存在（幂等性）
	if _, err := s.GetConfig(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // 记录不存在，直接返回
		}
		return err
	}

	// 显式删除关联数据，不依赖驱动或 DSN 是否正确启用外键级联。
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel api keys: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel models: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel logs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// BatchUpdatePriority 批量更新渠道优先级
// 使用单条批量 UPDATE + CASE WHEN 语句更新优先级（全参数化）
func (s *SQLStore) BatchUpdatePriority(ctx context.Context, updates []struct {
	ID       int64
	Priority int
}) (int64, error) {
	if len(updates) == 0 {
		return 0, nil
	}

	updatedAtUnix := timeToUnix(time.Now())

	// 构建批量UPDATE语句（CASE WHEN 使用参数化占位符）
	var caseBuilder strings.Builder
	// args 顺序：CASE WHEN 的 (id, priority) 对 + updated_at + WHERE IN 的 ids
	args := make([]any, 0, len(updates)*2+1+len(updates))

	caseBuilder.WriteString("UPDATE channels SET priority = CASE id ")
	for _, update := range updates {
		caseBuilder.WriteString("WHEN ? THEN ? ")
		args = append(args, update.ID, update.Priority)
	}
	caseBuilder.WriteString("END, updated_at = ? WHERE id IN (")
	args = append(args, updatedAtUnix)

	for i, update := range updates {
		if i > 0 {
			caseBuilder.WriteString(",")
		}
		caseBuilder.WriteString("?")
		args = append(args, update.ID)
	}
	caseBuilder.WriteString(")")

	// 执行批量更新
	result, err := s.db.ExecContext(ctx, caseBuilder.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("batch update priority: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()

	return rowsAffected, nil
}

// ==================== ModelEntries 辅助方法 ====================

// loadModelEntriesForConfigs 批量加载多个渠道的模型数据
// 设计说明：使用 IN 子句批量查询而非 JOIN，原因：
// 1. JOIN 会导致结果集膨胀（每个渠道有 N 个模型时重复 N 次渠道数据）
// 2. 当前方案：2 次查询，但总数据传输量更小
// 3. 热路径已由 ChannelCache 缓存，首次加载后不再查询数据库
func (s *SQLStore) loadModelEntriesForConfigs(ctx context.Context, configs []*model.Config) error {
	if len(configs) == 0 {
		return nil
	}

	// 构建 channel_id IN (...) 查询
	channelIDs := make([]any, len(configs))
	placeholders := make([]string, len(configs))
	idToConfig := make(map[int64]*model.Config)
	for i, cfg := range configs {
		channelIDs[i] = cfg.ID
		placeholders[i] = "?"
		idToConfig[cfg.ID] = cfg
		cfg.ModelEntries = nil // 初始化为空
	}

	//nolint:gosec // G201: placeholders 由内部构建的 "?" 占位符组成，安全可控
	query := fmt.Sprintf(
		`SELECT channel_id, model, redirect_model FROM channel_models WHERE channel_id IN (%s) ORDER BY channel_id, created_at ASC, model ASC`,
		strings.Join(placeholders, ","),
	)

	rows, err := s.db.QueryContext(ctx, query, channelIDs...)
	if err != nil {
		return fmt.Errorf("query model entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID int64
		var entry model.ModelEntry
		if err := rows.Scan(&channelID, &entry.Model, &entry.RedirectModel); err != nil {
			return fmt.Errorf("scan model entry: %w", err)
		}
		if cfg, ok := idToConfig[channelID]; ok {
			cfg.ModelEntries = append(cfg.ModelEntries, entry)
		}
	}

	return rows.Err()
}

func (s *SQLStore) loadProtocolTransformsForConfigs(ctx context.Context, configs []*model.Config) error {
	if len(configs) == 0 {
		return nil
	}

	channelIDs := make([]any, len(configs))
	placeholders := make([]string, len(configs))
	idToConfig := make(map[int64]*model.Config, len(configs))
	for i, cfg := range configs {
		channelIDs[i] = cfg.ID
		placeholders[i] = "?"
		idToConfig[cfg.ID] = cfg
		cfg.ProtocolTransforms = nil
	}

	query := fmt.Sprintf(
		`SELECT channel_id, protocol FROM channel_protocol_transforms WHERE channel_id IN (%s) ORDER BY channel_id, protocol ASC`,
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.QueryContext(ctx, query, channelIDs...)
	if err != nil {
		return fmt.Errorf("query protocol transforms: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID int64
		var protocol string
		if err := rows.Scan(&channelID, &protocol); err != nil {
			return fmt.Errorf("scan protocol transform: %w", err)
		}
		if cfg, ok := idToConfig[channelID]; ok {
			cfg.ProtocolTransforms = append(cfg.ProtocolTransforms, protocol)
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}
	for _, cfg := range configs {
		normalizeLoadedProtocolTransforms(cfg)
	}
	return nil
}

// loadConfigsAuxConcurrent 并发加载多渠道的模型与协议转换附属数据。
// 两次 IN 查询互不依赖，并行可省去一次 RTT；DB 资源池足够时无额外开销。
func (s *SQLStore) loadConfigsAuxConcurrent(ctx context.Context, configs []*model.Config) error {
	if len(configs) == 0 {
		return nil
	}
	var (
		wg          sync.WaitGroup
		modelErr    error
		protocolErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		modelErr = s.loadModelEntriesForConfigs(ctx, configs)
	}()
	go func() {
		defer wg.Done()
		protocolErr = s.loadProtocolTransformsForConfigs(ctx, configs)
	}()
	wg.Wait()
	if modelErr != nil {
		return modelErr
	}
	return protocolErr
}

func normalizeLoadedProtocolTransforms(cfg *model.Config) {
	if cfg == nil {
		return
	}
	cfg.ProtocolTransforms = cfg.GetProtocolTransforms()
}

func filterConfigsByProtocol(configs []*model.Config, protocol string) []*model.Config {
	if protocol == "" {
		return configs
	}
	filtered := make([]*model.Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg != nil && cfg.SupportsProtocol(protocol) {
			filtered = append(filtered, cfg)
		}
	}
	return filtered
}

// saveModelEntriesTx 保存渠道的模型数据（事务版本，用于 Create/Update/Replace）
func (s *SQLStore) saveModelEntriesTx(ctx context.Context, tx *sql.Tx, channelID int64, entries []model.ModelEntry) error {
	return s.saveModelEntriesImpl(ctx, tx, channelID, entries)
}

func (s *SQLStore) saveProtocolTransformsTx(ctx context.Context, tx *sql.Tx, channelID int64, transforms []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_protocol_transforms WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("delete old protocol transforms: %w", err)
	}
	if len(transforms) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString(`INSERT INTO channel_protocol_transforms (channel_id, protocol) VALUES `)
	args := make([]any, 0, len(transforms)*2)
	for i, protocol := range transforms {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?, ?)")
		args = append(args, channelID, protocol)
	}
	if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
		return fmt.Errorf("save protocol transforms: %w", err)
	}
	return nil
}

// dbExecutor 数据库执行器接口，统一 *sql.DB 和 *sql.Tx
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// saveModelEntriesImpl 保存渠道模型数据的统一实现
// 注意：调用方必须保证 entries 中没有重复的模型名，否则会因 PRIMARY KEY 冲突而失败（Fail-Fast）
func (s *SQLStore) saveModelEntriesImpl(ctx context.Context, exec dbExecutor, channelID int64, entries []model.ModelEntry) error {
	// 先删除旧的记录
	if _, err := exec.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("delete old model entries: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	// 多值 INSERT 分块提交：单批最多 200 行（800 占位符），兼容 SQLite 默认上限。
	// created_at 使用递增值保留用户输入顺序，避免同秒写入时被 model 字典序打乱。
	const batchSize = 200
	baseCreatedAt := time.Now().UnixMilli()

	for offset := 0; offset < len(entries); offset += batchSize {
		end := min(offset+batchSize, len(entries))
		chunk := entries[offset:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO channel_models (channel_id, model, redirect_model, created_at) VALUES `)
		args := make([]any, 0, len(chunk)*4)
		for i, entry := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?)")
			args = append(args, channelID, entry.Model, entry.RedirectModel, baseCreatedAt+int64(offset+i))
		}
		if _, err := exec.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("save model entries (offset %d): %w", offset, err)
		}
	}

	return nil
}
