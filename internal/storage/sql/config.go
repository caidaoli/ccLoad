package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
			SELECT c.id, c.name, c.url, c.priority, c.rpm_limit, c.max_concurrency, c.auth_type, COALESCE(c.oauth_credential, ''), c.websockets, c.protocol_transform_mode, c.enabled,
			       c.scheduled_check_enabled, c.scheduled_check_model,
			       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules, c.cooldown_detection_rules, c.proxy_url, c.retry_other_keys_on_failure,
			       SUM(CASE WHEN k.id IS NOT NULL AND k.disabled = 0 THEN 1 ELSE 0 END) as key_count,
			       c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN api_keys k ON c.id = k.channel_id
			GROUP BY c.id
			ORDER BY c.priority DESC, c.id ASC
	`
	rows, err := s.QueryContext(ctx, query)
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
			SELECT c.id, c.name, c.url, c.priority, c.rpm_limit, c.max_concurrency, c.auth_type, COALESCE(c.oauth_credential, ''), c.websockets, c.protocol_transform_mode, c.enabled,
			       c.scheduled_check_enabled, c.scheduled_check_model,
			       c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules, c.cooldown_detection_rules, c.proxy_url, c.retry_other_keys_on_failure,
			       SUM(CASE WHEN k.id IS NOT NULL AND k.disabled = 0 THEN 1 ELSE 0 END) as key_count,
			       c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN api_keys k ON c.id = k.channel_id
			WHERE c.id = ?
			GROUP BY c.id
	`
	row := s.QueryRowContext(ctx, query, id)

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
	            SELECT c.id, c.name, c.url, c.priority, c.rpm_limit, c.max_concurrency,
		                   c.auth_type, COALESCE(c.oauth_credential, ''), c.websockets, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
	                   c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules, c.cooldown_detection_rules, c.proxy_url, c.retry_other_keys_on_failure,
	                   SUM(CASE WHEN k.id IS NOT NULL AND k.disabled = 0 THEN 1 ELSE 0 END) as key_count,
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
	            SELECT c.id, c.name, c.url, c.priority, c.rpm_limit, c.max_concurrency,
		                   c.auth_type, COALESCE(c.oauth_credential, ''), c.websockets, c.protocol_transform_mode, c.enabled, c.scheduled_check_enabled, c.scheduled_check_model,
	                   c.cooldown_until, c.cooldown_duration_ms, c.daily_cost_limit, c.cost_multiplier, c.custom_request_rules, c.cooldown_detection_rules, c.proxy_url, c.retry_other_keys_on_failure,
	                   SUM(CASE WHEN k.id IS NOT NULL AND k.disabled = 0 THEN 1 ELSE 0 END) as key_count,
	                   c.created_at, c.updated_at
	            FROM channels c
	            INNER JOIN channel_models cm ON c.id = cm.channel_id
	            LEFT JOIN api_keys k ON c.id = k.channel_id
	            WHERE c.enabled = 1
              AND cm.model = ?
	              AND cm.disabled = 0
	            GROUP BY c.id
            ORDER BY c.priority DESC, c.id ASC
        `
		args = []any{modelName}
	}

	rows, err := s.QueryContext(ctx, query, args...)
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

// CreateConfig 创建新的渠道配置
func (s *SQLStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	nowUnix := timeToUnix(time.Now())
	authType := model.NormalizeAuthType(c.AuthType)
	if authType == "" {
		return nil, fmt.Errorf("invalid auth_type %q", c.AuthType)
	}
	if authType != model.AuthTypeAPIKey && strings.TrimSpace(c.OAuthCredential) == "" {
		return nil, fmt.Errorf("%s channel requires a credential", authType)
	}
	if authType == model.AuthTypeAPIKey && strings.TrimSpace(c.OAuthCredential) != "" {
		return nil, errors.New("api_key channel cannot contain an OAuth credential")
	}

	protocolTransformMode := c.GetProtocolTransformMode()
	customRules, err := marshalCustomRequestRules(c.CustomRequestRules)
	if err != nil {
		return nil, err
	}
	cooldownDetectionRules, err := marshalCooldownDetectionRules(c.CooldownDetectionRules)
	if err != nil {
		return nil, err
	}

	id := c.ID
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if id != 0 {
			if err := s.lockPostgresExplicitIDTable(ctx, tx, "channels"); err != nil {
				return err
			}
			existingAuthType, _, loadErr := s.loadOAuthCredentialForUpdate(ctx, tx, id)
			if loadErr == nil && (model.NormalizeAuthType(existingAuthType) != model.AuthTypeAPIKey || authType != model.AuthTypeAPIKey) {
				return errors.New("OAuth channel cannot replace or be replaced through CreateConfig")
			}
			if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
				return loadErr
			}
		}
		if id == 0 {
			// 插入渠道记录（数据库生成自增 id）
			if s.IsPostgres() {
				err := s.queryRowTx(ctx, tx, `
					INSERT INTO channels(name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					RETURNING id
					`, c.Name, c.URLs, c.Priority, c.RPMLimit, c.MaxConcurrency, authType, c.OAuthCredential, c.Websockets,
					protocolTransformMode, c.Enabled, c.ScheduledCheckEnabled, c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, cooldownDetectionRules, c.ProxyURL, c.RetryOtherKeysOnFailure, nowUnix, nowUnix).Scan(&id)
				if err != nil {
					return err
				}
			} else {
				res, err := s.execTx(ctx, tx, `
					INSERT INTO channels(name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, c.Name, c.URLs, c.Priority, c.RPMLimit, c.MaxConcurrency, authType, c.OAuthCredential, c.Websockets,
					protocolTransformMode, c.Enabled, c.ScheduledCheckEnabled, c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, cooldownDetectionRules, c.ProxyURL, c.RetryOtherKeysOnFailure, nowUnix, nowUnix)
				if err != nil {
					return err
				}
				id, err = res.LastInsertId()
				if err != nil {
					return fmt.Errorf("get last insert id: %w", err)
				}
			}
		} else {
			// 显式主键：用于混合存储同步/恢复，保证两端主键一致
			if s.supportsONConflict() {
				_, err := s.execTx(ctx, tx, `
					INSERT INTO channels(id, name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					`, id, c.Name, c.URLs, c.Priority, c.RPMLimit, c.MaxConcurrency, authType, c.OAuthCredential, c.Websockets,
					protocolTransformMode, c.Enabled, c.ScheduledCheckEnabled, c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, cooldownDetectionRules, c.ProxyURL, c.RetryOtherKeysOnFailure, nowUnix, nowUnix)
				if err != nil {
					return err
				}
			} else {
				_, err := s.execTx(ctx, tx, `
					INSERT INTO channels(id, name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					ON DUPLICATE KEY UPDATE
						name = VALUES(name),
						url = VALUES(url),
						priority = VALUES(priority),
						rpm_limit = VALUES(rpm_limit),
						max_concurrency = VALUES(max_concurrency),
						auth_type = VALUES(auth_type),
						oauth_credential = VALUES(oauth_credential),
						websockets = VALUES(websockets),
						protocol_transform_mode = VALUES(protocol_transform_mode),
						enabled = VALUES(enabled),
						scheduled_check_enabled = VALUES(scheduled_check_enabled),
						scheduled_check_model = VALUES(scheduled_check_model),
						daily_cost_limit = VALUES(daily_cost_limit),
						cost_multiplier = VALUES(cost_multiplier),
						custom_request_rules = VALUES(custom_request_rules),
						cooldown_detection_rules = VALUES(cooldown_detection_rules),
						proxy_url = VALUES(proxy_url),
						retry_other_keys_on_failure = VALUES(retry_other_keys_on_failure),
						updated_at = VALUES(updated_at)
					`, id, c.Name, c.URLs, c.Priority, c.RPMLimit, c.MaxConcurrency, authType, c.OAuthCredential, c.Websockets,
					protocolTransformMode, c.Enabled, c.ScheduledCheckEnabled, c.ScheduledCheckModel, c.DailyCostLimit, normalizeCostMultiplier(c.CostMultiplier), customRules, cooldownDetectionRules, c.ProxyURL, c.RetryOtherKeysOnFailure, nowUnix, nowUnix)
				if err != nil {
					return err
				}
			}
		}

		// 保存模型数据到 channel_models 表
		if err := s.saveModelEntriesTx(ctx, tx, id, c.ModelEntries); err != nil {
			return fmt.Errorf("save model entries: %w", err)
		}
		if id != 0 {
			if err := s.syncPostgresIDSequence(ctx, tx, "channels"); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	s.unmarkChannelDeleted(id)

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

	// 确认目标存在，并禁止普通配置更新改变认证机制或私有凭证。
	existing, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(upd.AuthType) != "" {
		authType := model.NormalizeAuthType(upd.AuthType)
		if authType == "" {
			return nil, fmt.Errorf("invalid auth_type %q", upd.AuthType)
		}
		if authType != existing.GetAuthType() {
			return nil, errors.New("auth_type cannot be changed")
		}
	}

	name := strings.TrimSpace(upd.Name)
	urls := upd.URLs.Clone()
	if err := urls.Normalize(); err != nil {
		return nil, err
	}

	protocolTransformMode := upd.GetProtocolTransformMode()
	customRules, err := marshalCustomRequestRules(upd.CustomRequestRules)
	if err != nil {
		return nil, err
	}
	cooldownDetectionRules, err := marshalCooldownDetectionRules(upd.CooldownDetectionRules)
	if err != nil {
		return nil, err
	}
	updatedAtUnix := timeToUnix(time.Now())

	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 更新渠道记录
		_, err := s.execTx(ctx, tx, `
			UPDATE channels
			SET name=?, url=?, priority=?, rpm_limit=?, max_concurrency=?, websockets=?, protocol_transform_mode=?, enabled=?, scheduled_check_enabled=?, scheduled_check_model=?, daily_cost_limit=?, cost_multiplier=?, custom_request_rules=?, cooldown_detection_rules=?, proxy_url=?, retry_other_keys_on_failure=?, updated_at=?
			WHERE id=?
			`, name, urls, upd.Priority, upd.RPMLimit, upd.MaxConcurrency, upd.Websockets,
			protocolTransformMode, upd.Enabled, upd.ScheduledCheckEnabled, upd.ScheduledCheckModel, upd.DailyCostLimit, normalizeCostMultiplier(upd.CostMultiplier), customRules, cooldownDetectionRules, upd.ProxyURL, upd.RetryOtherKeysOnFailure, updatedAtUnix, id)
		if err != nil {
			return err
		}

		// 更新 channel_models 表（先删后插）
		if err := s.saveModelEntriesTx(ctx, tx, id, upd.ModelEntries); err != nil {
			return fmt.Errorf("save model entries: %w", err)
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

// CompareAndSwapOAuthCredential replaces the complete private credential only
// when both its provider and previous payload still match.
func (s *SQLStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || authType == model.AuthTypeAPIKey {
		return false, errors.New("OAuth auth type is invalid")
	}
	if strings.TrimSpace(expectedCredential) == "" {
		return false, errors.New("expected OAuth credential cannot be empty")
	}
	if strings.TrimSpace(nextCredential) == "" {
		return false, errors.New("next OAuth credential cannot be empty")
	}
	matched := false
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched = false
		currentAuthType, currentCredential, loadErr := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(loadErr, sql.ErrNoRows) {
			return nil
		}
		if loadErr != nil {
			return loadErr
		}
		if currentAuthType != authType || currentCredential != expectedCredential {
			return nil
		}
		if _, updateErr := s.execTx(ctx, tx, `
			UPDATE channels SET oauth_credential = ?, updated_at = ? WHERE id = ?
		`, nextCredential, timeToUnix(time.Now()), channelID); updateErr != nil {
			return updateErr
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("compare and swap OAuth credential: %w", err)
	}
	return matched, nil
}

func (s *SQLStore) loadOAuthCredentialForUpdate(ctx context.Context, tx *sql.Tx, channelID int64) (string, string, error) {
	query := `SELECT auth_type, COALESCE(oauth_credential, '') FROM channels WHERE id = ?`
	if s.supportsRowLock() {
		query += ` FOR UPDATE`
	}
	var authType, credential string
	err := s.queryRowTx(ctx, tx, query, channelID).Scan(&authType, &credential)
	return authType, credential, err
}

// SyncConfigReplica idempotently mirrors a complete channel snapshot while
// preserving the SQLite-assigned ID. It is intentionally absent from Store.
func (s *SQLStore) SyncConfigReplica(ctx context.Context, cfg *model.Config) error {
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		return s.syncConfigReplicaTx(ctx, tx, cfg)
	})
	if err != nil {
		return fmt.Errorf("sync config replica: %w", err)
	}
	s.unmarkChannelDeleted(cfg.ID)
	return nil
}

func (s *SQLStore) syncConfigReplicaTx(ctx context.Context, tx *sql.Tx, cfg *model.Config) error {
	if cfg == nil || cfg.ID <= 0 {
		return errors.New("replica config is invalid")
	}
	authType := cfg.GetAuthType()
	if authType != model.AuthTypeAPIKey && strings.TrimSpace(cfg.OAuthCredential) == "" {
		return errors.New("replica OAuth config is invalid")
	}
	name := cfg.Name
	urls := cfg.URLs.Clone()
	protocolTransformMode := cfg.ProtocolTransformMode
	customRules, err := marshalCustomRequestRules(cfg.CustomRequestRules)
	if err != nil {
		return err
	}
	cooldownDetectionRules, err := marshalCooldownDetectionRules(cfg.CooldownDetectionRules)
	if err != nil {
		return err
	}
	nowUnix := timeToUnix(time.Now())
	var conflictingID int64
	conflictErr := s.queryRowTx(ctx, tx, `SELECT id FROM channels WHERE name = ? AND id <> ?`, name, cfg.ID).Scan(&conflictingID)
	if conflictErr == nil {
		if err := s.deleteChannelReplicaTx(ctx, tx, conflictingID); err != nil {
			return err
		}
	} else if !errors.Is(conflictErr, sql.ErrNoRows) {
		return conflictErr
	}

	currentAuthType, _, loadErr := s.loadOAuthCredentialForUpdate(ctx, tx, cfg.ID)
	switch {
	case errors.Is(loadErr, sql.ErrNoRows):
		if _, insertErr := s.execTx(ctx, tx, `
					INSERT INTO channels(id, name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, cooldown_until, cooldown_duration_ms, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, cfg.ID, name, urls, cfg.Priority, cfg.RPMLimit, cfg.MaxConcurrency, authType, cfg.OAuthCredential,
			cfg.Websockets, protocolTransformMode, cfg.Enabled, cfg.ScheduledCheckEnabled, cfg.ScheduledCheckModel,
			cfg.CooldownUntil, cfg.CooldownDurationMs, cfg.DailyCostLimit, normalizeCostMultiplier(cfg.CostMultiplier),
			customRules, cooldownDetectionRules, cfg.ProxyURL, cfg.RetryOtherKeysOnFailure, nowUnix, nowUnix); insertErr != nil {
			return insertErr
		}
	case loadErr != nil:
		return loadErr
	case model.NormalizeAuthType(currentAuthType) != authType:
		if err := s.replaceChannelSchedulingReplicaTx(ctx, tx, cfg.ID); err != nil {
			return err
		}
		if _, insertErr := s.execTx(ctx, tx, `
					INSERT INTO channels(id, name, url, priority, rpm_limit, max_concurrency, auth_type, oauth_credential, websockets, protocol_transform_mode, enabled, scheduled_check_enabled, scheduled_check_model, cooldown_until, cooldown_duration_ms, daily_cost_limit, cost_multiplier, custom_request_rules, cooldown_detection_rules, proxy_url, retry_other_keys_on_failure, created_at, updated_at)
					VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, cfg.ID, name, urls, cfg.Priority, cfg.RPMLimit, cfg.MaxConcurrency, authType, cfg.OAuthCredential,
			cfg.Websockets, protocolTransformMode, cfg.Enabled, cfg.ScheduledCheckEnabled, cfg.ScheduledCheckModel,
			cfg.CooldownUntil, cfg.CooldownDurationMs, cfg.DailyCostLimit, normalizeCostMultiplier(cfg.CostMultiplier),
			customRules, cooldownDetectionRules, cfg.ProxyURL, cfg.RetryOtherKeysOnFailure, nowUnix, nowUnix); insertErr != nil {
			return insertErr
		}
	default:
		if _, updateErr := s.execTx(ctx, tx, `
					UPDATE channels SET
					name = ?, url = ?, priority = ?, rpm_limit = ?, max_concurrency = ?, oauth_credential = ?,
					websockets = ?, protocol_transform_mode = ?, enabled = ?, scheduled_check_enabled = ?,
					scheduled_check_model = ?, cooldown_until = ?, cooldown_duration_ms = ?, daily_cost_limit = ?,
					cost_multiplier = ?, custom_request_rules = ?, cooldown_detection_rules = ?, proxy_url = ?,
					retry_other_keys_on_failure = ?, updated_at = ?
				WHERE id = ?
			`, name, urls, cfg.Priority, cfg.RPMLimit, cfg.MaxConcurrency, cfg.OAuthCredential,
			cfg.Websockets, protocolTransformMode, cfg.Enabled, cfg.ScheduledCheckEnabled,
			cfg.ScheduledCheckModel, cfg.CooldownUntil, cfg.CooldownDurationMs, cfg.DailyCostLimit,
			normalizeCostMultiplier(cfg.CostMultiplier), customRules, cooldownDetectionRules, cfg.ProxyURL,
			cfg.RetryOtherKeysOnFailure, nowUnix, cfg.ID); updateErr != nil {
			return updateErr
		}
	}
	if err := s.saveModelEntriesTx(ctx, tx, cfg.ID, cfg.ModelEntries); err != nil {
		return fmt.Errorf("sync replica models: %w", err)
	}
	return s.syncPostgresIDSequence(ctx, tx, "channels")
}

// SyncOAuthConfigReplica keeps the stricter OAuth-only contract used by
// startup restore and older callers.
func (s *SQLStore) SyncOAuthConfigReplica(ctx context.Context, cfg *model.Config) error {
	if cfg == nil || !cfg.UsesOAuth() {
		return errors.New("OAuth replica config is invalid")
	}
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		currentAuthType, _, loadErr := s.loadOAuthCredentialForUpdate(ctx, tx, cfg.ID)
		if loadErr == nil && model.NormalizeAuthType(currentAuthType) != cfg.GetAuthType() {
			return errors.New("OAuth replica provider does not match existing channel")
		}
		if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
			return loadErr
		}
		return s.syncConfigReplicaTx(ctx, tx, cfg)
	})
	if err != nil {
		return fmt.Errorf("sync OAuth config replica: %w", err)
	}
	s.unmarkChannelDeleted(cfg.ID)
	return nil
}

// UpdateOAuthModelStateIfCredentialMatches conditionally commits model state
// derived from one exact OAuth credential snapshot.
func (s *SQLStore) UpdateOAuthModelStateIfCredentialMatches(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential string,
	modelEntries []model.ModelEntry,
	scheduledCheckModel string,
) (bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || authType == model.AuthTypeAPIKey {
		return false, errors.New("OAuth auth type is invalid")
	}
	if strings.TrimSpace(expectedCredential) == "" {
		return false, errors.New("expected OAuth credential cannot be empty")
	}
	matched := false
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched = false
		currentAuthType, currentCredential, loadErr := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(loadErr, sql.ErrNoRows) {
			return nil
		}
		if loadErr != nil {
			return loadErr
		}
		if currentAuthType != authType || currentCredential != expectedCredential {
			return nil
		}
		if err := s.saveModelEntriesTx(ctx, tx, channelID, modelEntries); err != nil {
			return fmt.Errorf("save OAuth model state: %w", err)
		}
		if _, updateErr := s.execTx(ctx, tx, `
			UPDATE channels SET scheduled_check_model = ?, updated_at = ? WHERE id = ?
		`, scheduledCheckModel, timeToUnix(time.Now()), channelID); updateErr != nil {
			return updateErr
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("update OAuth model state: %w", err)
	}
	return matched, nil
}

// UpdateChannelEnabled updates only the enabled flag.
// The full UpdateConfig path rewrites models and reloads the
// config before writing. A switch click must not pay that cost.
func (s *SQLStore) UpdateChannelEnabled(ctx context.Context, id int64, enabled bool) (*model.Config, error) {
	updatedAtUnix := timeToUnix(time.Now())
	result, err := s.ExecContext(ctx, `
		UPDATE channels
		SET enabled = ?, updated_at = ?
		WHERE id = ?
	`, enabled, updatedAtUnix, id)
	if err != nil {
		return nil, fmt.Errorf("update channel enabled: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected == 0 {
		cfg, getErr := s.GetConfig(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		return cfg, nil
	}

	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	return config, nil
}

// BatchPatchConfigs atomically changes only the explicitly requested channel fields.
func (s *SQLStore) BatchPatchConfigs(ctx context.Context, channelIDs []int64, patch model.BatchConfigPatch) (model.BatchConfigPatchResult, error) {
	channelIDs = normalizeBatchPatchChannelIDs(channelIDs)
	if len(channelIDs) == 0 {
		return model.BatchConfigPatchResult{}, nil
	}

	patch, err := patch.Normalize()
	if err != nil {
		return model.BatchConfigPatchResult{}, err
	}

	result := model.BatchConfigPatchResult{}
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		states, err := s.loadBatchConfigPatchStates(ctx, tx, channelIDs, patch.ModelImportMode != "")
		if err != nil {
			return err
		}

		for _, channelID := range channelIDs {
			state, ok := states[channelID]
			if !ok {
				result.NotFound = append(result.NotFound, channelID)
				continue
			}
			// Model imports are independent of the channel authentication type.
			nextCostMultiplier := state.costMultiplier
			if patch.CostMultiplier != nil {
				nextCostMultiplier = *patch.CostMultiplier
			}
			nextDailyCostLimit := state.dailyCostLimit
			if patch.DailyCostLimit != nil {
				nextDailyCostLimit = *patch.DailyCostLimit
			}
			nextRPMLimit := state.rpmLimit
			if patch.RPMLimit != nil {
				nextRPMLimit = *patch.RPMLimit
			}
			nextMaxConcurrency := state.maxConcurrency
			if patch.MaxConcurrency != nil {
				nextMaxConcurrency = *patch.MaxConcurrency
			}
			nextProtocolMode := state.protocolTransformMode
			if patch.ProtocolTransformMode != nil {
				nextProtocolMode = *patch.ProtocolTransformMode
			}
			nextScheduledCheckModel := state.scheduledCheckModel
			nextModels := state.modelEntries
			modelsChanged := false
			if patch.ModelImportMode != "" {
				nextModels = importedModelEntries(state.modelEntries, patch.ModelEntries, patch.ModelImportMode)
				modelsChanged = !modelEntrySlicesEqual(state.modelEntries, nextModels)
				nextScheduledCheckModel = reconciledScheduledCheckModel(nextScheduledCheckModel, nextModels)
			}

			changed := state.costMultiplier != nextCostMultiplier ||
				state.dailyCostLimit != nextDailyCostLimit ||
				state.rpmLimit != nextRPMLimit ||
				state.maxConcurrency != nextMaxConcurrency ||
				state.protocolTransformMode != nextProtocolMode ||
				state.scheduledCheckModel != nextScheduledCheckModel ||
				modelsChanged
			if !changed {
				result.Unchanged++
				continue
			}

			if _, err := s.execTx(ctx, tx, `
				UPDATE channels
				SET cost_multiplier = ?, daily_cost_limit = ?, rpm_limit = ?, max_concurrency = ?,
					protocol_transform_mode = ?, scheduled_check_model = ?, updated_at = ?
				WHERE id = ?
			`, nextCostMultiplier, nextDailyCostLimit, nextRPMLimit, nextMaxConcurrency,
				nextProtocolMode, nextScheduledCheckModel, timeToUnix(time.Now()), channelID); err != nil {
				return fmt.Errorf("patch channel %d: %w", channelID, err)
			}
			if modelsChanged {
				if err := s.saveModelEntriesTx(ctx, tx, channelID, nextModels); err != nil {
					return fmt.Errorf("patch channel %d models: %w", channelID, err)
				}
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return model.BatchConfigPatchResult{}, err
	}
	return result, nil
}

type batchConfigPatchState struct {
	costMultiplier        float64
	dailyCostLimit        float64
	rpmLimit              int
	maxConcurrency        int
	protocolTransformMode string
	scheduledCheckModel   string
	modelEntries          []model.ModelEntry
}

func normalizeBatchPatchChannelIDs(channelIDs []int64) []int64 {
	seen := make(map[int64]struct{}, len(channelIDs))
	result := make([]int64, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		if channelID <= 0 {
			continue
		}
		if _, ok := seen[channelID]; ok {
			continue
		}
		seen[channelID] = struct{}{}
		result = append(result, channelID)
	}
	return result
}

func (s *SQLStore) loadBatchConfigPatchStates(ctx context.Context, tx *sql.Tx, channelIDs []int64, withModels bool) (map[int64]*batchConfigPatchState, error) {
	placeholders := make([]string, len(channelIDs))
	args := make([]any, len(channelIDs))
	for i, channelID := range channelIDs {
		placeholders[i] = "?"
		args[i] = channelID
	}

	//nolint:gosec // placeholders are generated internally and contain only "?".
	query := `SELECT id, cost_multiplier, daily_cost_limit, rpm_limit, max_concurrency,
		protocol_transform_mode, scheduled_check_model
		FROM channels WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id`
	if s.supportsRowLock() {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, s.q(query), normalizeSQLArgs(args)...)
	if err != nil {
		return nil, fmt.Errorf("query channels for batch patch: %w", err)
	}
	states := make(map[int64]*batchConfigPatchState, len(channelIDs))
	for rows.Next() {
		var channelID int64
		state := &batchConfigPatchState{}
		if err := rows.Scan(
			&channelID, &state.costMultiplier, &state.dailyCostLimit, &state.rpmLimit, &state.maxConcurrency,
			&state.protocolTransformMode, &state.scheduledCheckModel,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan channel for batch patch: %w", err)
		}
		states[channelID] = state
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate channels for batch patch: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close channels for batch patch: %w", err)
	}
	if !withModels || len(states) == 0 {
		return states, nil
	}

	modelRows, err := tx.QueryContext(ctx, s.q(`SELECT channel_id, model, redirect_model, disabled
		FROM channel_models WHERE channel_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY channel_id, created_at ASC, model ASC`), normalizeSQLArgs(args)...)
	if err != nil {
		return nil, fmt.Errorf("query models for batch patch: %w", err)
	}
	defer func() { _ = modelRows.Close() }()
	for modelRows.Next() {
		var channelID int64
		var entry model.ModelEntry
		if err := modelRows.Scan(&channelID, &entry.Model, &entry.RedirectModel, &entry.Disabled); err != nil {
			return nil, fmt.Errorf("scan model for batch patch: %w", err)
		}
		if state := states[channelID]; state != nil {
			state.modelEntries = append(state.modelEntries, entry)
		}
	}
	if err := modelRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate models for batch patch: %w", err)
	}
	return states, nil
}

func importedModelEntries(existing, imported []model.ModelEntry, mode string) []model.ModelEntry {
	if mode == model.ModelImportModeReplace {
		return append([]model.ModelEntry(nil), imported...)
	}
	result := append([]model.ModelEntry(nil), existing...)
	seen := make(map[string]struct{}, len(existing)+len(imported))
	for _, entry := range existing {
		seen[strings.ToLower(entry.Model)] = struct{}{}
	}
	for _, entry := range imported {
		key := strings.ToLower(entry.Model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, entry)
	}
	return result
}

func modelEntrySlicesEqual(left, right []model.ModelEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func reconciledScheduledCheckModel(current string, entries []model.ModelEntry) string {
	if current == "" {
		return ""
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Model, current) || strings.EqualFold(entry.RedirectModel, current) {
			return entry.Model
		}
	}
	return ""
}

// DeleteConfig 删除渠道配置
func (s *SQLStore) DeleteConfig(ctx context.Context, id int64) error {
	// 检查记录是否存在，但不存在也继续清理残留子数据。
	if _, err := s.GetConfig(ctx, id); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return err
		}
	}

	s.markChannelDeleted(id)

	// 显式删除关联数据，不依赖驱动或 DSN 是否正确启用外键级联。
	var deletedRowsForVacuum int64
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := s.execTx(ctx, tx, `DELETE FROM api_keys WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel api keys: %w", err)
		}
		if _, err := s.execTx(ctx, tx, `DELETE FROM channel_models WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel models: %w", err)
		}
		if _, err := s.execTx(ctx, tx, `DELETE FROM channel_model_cooldowns WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel model cooldowns: %w", err)
		}
		if _, err := s.execTx(ctx, tx, `DELETE FROM channel_url_states WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel url states: %w", err)
		}
		if _, err := s.execTx(ctx, tx, `UPDATE model_fingerprints SET channel_id = NULL WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("clear fingerprint channel_id: %w", err)
		}
		if result, err := s.execTx(ctx, tx, `DELETE FROM debug_logs WHERE log_id IN (SELECT id FROM logs WHERE channel_id = ?)`, id); err != nil {
			return fmt.Errorf("delete channel debug logs: %w", err)
		} else if affected, rowsErr := result.RowsAffected(); rowsErr == nil {
			deletedRowsForVacuum += affected
		}
		if result, err := s.execTx(ctx, tx, `DELETE FROM logs WHERE channel_id = ?`, id); err != nil {
			return fmt.Errorf("delete channel logs: %w", err)
		} else if affected, rowsErr := result.RowsAffected(); rowsErr == nil {
			deletedRowsForVacuum += affected
		}
		if _, err := s.execTx(ctx, tx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete channel: %w", err)
		}
		return nil
	})
	if err != nil {
		s.unmarkChannelDeleted(id)
		return err
	}

	s.runSQLiteIncrementalVacuum(ctx, deletedRowsForVacuum)
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
	priorityPlaceholder := "?"
	if s.IsPostgres() {
		priorityPlaceholder = "CAST(? AS INTEGER)"
	}
	for _, update := range updates {
		caseBuilder.WriteString("WHEN ? THEN ")
		caseBuilder.WriteString(priorityPlaceholder)
		caseBuilder.WriteByte(' ')
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
	result, err := s.ExecContext(ctx, caseBuilder.String(), args...)
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
		`SELECT channel_id, model, redirect_model, disabled FROM channel_models WHERE channel_id IN (%s) ORDER BY channel_id, created_at ASC, model ASC`,
		strings.Join(placeholders, ","),
	)

	rows, err := s.QueryContext(ctx, query, channelIDs...)
	if err != nil {
		return fmt.Errorf("query model entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID int64
		var entry model.ModelEntry
		if err := rows.Scan(&channelID, &entry.Model, &entry.RedirectModel, &entry.Disabled); err != nil {
			return fmt.Errorf("scan model entry: %w", err)
		}
		if cfg, ok := idToConfig[channelID]; ok {
			cfg.ModelEntries = append(cfg.ModelEntries, entry)
		}
	}

	return rows.Err()
}

// loadConfigsAuxConcurrent 加载渠道模型附属数据。
func (s *SQLStore) loadConfigsAuxConcurrent(ctx context.Context, configs []*model.Config) error {
	return s.loadModelEntriesForConfigs(ctx, configs)
}

// saveModelEntriesTx 保存渠道的模型数据（事务版本，用于 Create/Update/Replace）
func (s *SQLStore) saveModelEntriesTx(ctx context.Context, tx *sql.Tx, channelID int64, entries []model.ModelEntry) error {
	return s.saveModelEntriesImpl(ctx, tx, channelID, entries)
}

// saveModelEntriesImpl 保存渠道模型数据的统一实现
// 注意：调用方必须保证 entries 中没有重复的模型名，否则会因 PRIMARY KEY 冲突而失败（Fail-Fast）
func (s *SQLStore) saveModelEntriesImpl(ctx context.Context, exec sqlExecutor, channelID int64, entries []model.ModelEntry) error {
	// 先删除旧的记录（Postgres 需 rebind 占位符）
	if _, err := s.execWith(ctx, exec, `DELETE FROM channel_models WHERE channel_id = ?`, channelID); err != nil {
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
		b.WriteString(`INSERT INTO channel_models (channel_id, model, redirect_model, disabled, created_at) VALUES `)
		args := make([]any, 0, len(chunk)*5)
		for i, entry := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?, ?)")
			args = append(args, channelID, entry.Model, entry.RedirectModel, entry.Disabled, baseCreatedAt+int64(offset+i))
		}
		if _, err := s.execWith(ctx, exec, b.String(), args...); err != nil {
			return fmt.Errorf("save model entries (offset %d): %w", offset, err)
		}
	}

	return nil
}
