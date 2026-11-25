package sql

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"ccLoad/internal/model"
)

/**
 * @brief 读取同一事务中的渠道、Key 及对应表单版本。
 * @param ctx 数据库操作上下文。
 * @param id 渠道 ID。
 * @return 渠道配置快照。
 * @return 按索引排序的 Key 快照。
 * @return 不带引号的编辑版本。
 * @return 渠道不存在或存储读取失败时的错误。
 */
func (s *SQLStore) GetChannelEditSnapshot(ctx context.Context, id int64) (*model.Config, []*model.APIKey, string, error) {
	var cfg *model.Config
	var keys []*model.APIKey
	var version string
	err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		cfg, keys, err = s.loadChannelEditSnapshotTx(ctx, tx, id)
		if err != nil {
			return err
		}
		version, err = channelEditVersion(cfg, keys)
		return err
	})
	if err != nil {
		return nil, nil, "", err
	}
	return cfg, keys, version, nil
}

/**
 * @brief 保存渠道、Key 和管理配置，可选择拒绝已经过时的表单。
 * @param ctx 数据库操作上下文。
 * @param id 渠道 ID。
 * @param upd 待保存的配置。
 * @param keys 要更新的 Key；原位更新时 nil 表示保留。
 * @param replaceKeys 为 true 时替换完整 Key 集合。
 * @param expectedVersion 编辑版本，空字符串表示无条件。
 * @param mergeManagement 纯管理封套合并函数，nil 时保留，事务重试时可能重新调用。
 * @return 保存后的渠道配置。
 * @return 版本不匹配返回 model.ErrChannelEditConflict，否则返回相应存储错误。
 */
func (s *SQLStore) UpdateChannelIfEditVersion(ctx context.Context, id int64, upd *model.Config, keys []*model.APIKey, replaceKeys bool, expectedVersion string, mergeManagement func(string) (string, error)) (*model.Config, error) {
	return s.updateConfig(ctx, id, upd, keys, replaceKeys, &channelEditUpdate{expectedVersion, mergeManagement})
}

/**
 * @brief 表单保存需要在数据库锁内执行的版本校验与管理设置合并。
 */
type channelEditUpdate struct {
	expectedVersion string                       ///< 空字符串允许无条件保存。
	mergeManagement func(string) (string, error) ///< 纯函数，接收当前持久化封套。
}

/**
 * @brief 锁定并读取渠道、模型和 Key 的一致快照。
 * @param ctx 数据库操作上下文。
 * @param tx 当前存储事务。
 * @param id 渠道 ID。
 * @return 渠道配置快照与最新启用 Key 数量。
 * @return 按索引排序的 Key 快照。
 * @return 渠道不存在或查询失败时的错误。
 */
func (s *SQLStore) loadChannelEditSnapshotTx(ctx context.Context, tx *sql.Tx, id int64) (*model.Config, []*model.APIKey, error) {
	cfg, err := s.loadConfigSnapshotForUpdate(ctx, tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, model.ErrChannelNotFound
		}
		return nil, nil, err
	}
	query := apiKeysByChannelQuery
	if s.supportsRowLock() {
		query += " FOR UPDATE"
	}
	rows, err := s.queryTx(ctx, tx, query, id)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	keys, err := scanAPIKeys(rows)
	if err != nil {
		return nil, nil, err
	}
	for _, key := range keys {
		if !key.Disabled {
			cfg.KeyCount++
		}
	}
	return cfg, keys, nil
}

/**
 * @brief 为渠道表单可编辑字段及实际 Key 内容生成稳定摘要。
 * @param cfg 已归一的持久化渠道快照。
 * @param keys 与渠道在同一事务读取的 Key。
 * @return 编辑版本的 SHA-256 十六进制摘要。
 * @return 管理封套无效或 JSON 编码失败时返回错误。
 */
func channelEditVersion(cfg *model.Config, keys []*model.APIKey) (string, error) {
	config := cfg.Clone()
	config.CooldownUntil, config.CooldownDurationMs, config.KeyCount = 0, 0, 0
	config.CreatedAt, config.UpdatedAt = model.JSONTime{}, model.JSONTime{}
	editKeys := make([]model.APIKey, 0, len(keys))
	for _, key := range keys {
		entry := *key
		entry.ID, entry.ChannelID, entry.Disabled = 0, 0, false
		entry.CooldownUntil, entry.CooldownDurationMs = 0, 0
		entry.CreatedAt, entry.UpdatedAt = model.JSONTime{}, model.JSONTime{}
		editKeys = append(editKeys, entry)
	}
	var management *model.ChannelManagementEnvelope
	if cfg.GetAuthType() == model.AuthTypeAPIKey && cfg.OAuthCredential != "" {
		var err error
		management, err = model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
		if err != nil {
			return "", fmt.Errorf("parse channel edit management settings: %w", err)
		}
		management.State = model.ChannelManagementState{}
		if management.Profile == model.ChannelManagementProfileSub2API || management.Profile == model.ChannelManagementProfileSub2APIPro {
			management.Settings.AccessToken = ""
			management.Settings.RefreshToken = ""
			management.Settings.ExpiresAt = nil
			management.Settings.AccountID = nil
		}
	}
	payload, err := json.Marshal(struct {
		Channel    *model.Config
		Keys       []model.APIKey
		Management *model.ChannelManagementEnvelope
	}{config, editKeys, management})
	if err != nil {
		return "", fmt.Errorf("encode channel edit version: %w", err)
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

/**
 * @brief 在事务内校验编辑版本，并将管理设置与最新运行态合并。
 * @param ctx 数据库操作上下文。
 * @param tx 当前写事务。
 * @param id 渠道 ID。
 * @param edit 版本及管理设置合并参数。
 * @return 当前 Key 的锁定快照。
 * @return 版本不符、管理设置不合规或存储失败时返回错误。
 */
func (s *SQLStore) checkChannelEditTx(ctx context.Context, tx *sql.Tx, id int64, edit *channelEditUpdate) ([]*model.APIKey, error) {
	cfg, keys, err := s.loadChannelEditSnapshotTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if edit.expectedVersion != "" {
		version, err := channelEditVersion(cfg, keys)
		if err != nil {
			return nil, err
		}
		if version != edit.expectedVersion {
			return nil, model.ErrChannelEditConflict
		}
	}
	if edit.mergeManagement == nil {
		return keys, nil
	}
	if cfg.GetAuthType() != model.AuthTypeAPIKey {
		return nil, errors.New("channel management requires an API key channel")
	}
	next, err := edit.mergeManagement(cfg.OAuthCredential)
	if err != nil {
		return nil, err
	}
	if next != cfg.OAuthCredential {
		if _, err := s.execTx(ctx, tx, `UPDATE channels SET oauth_credential = ? WHERE id = ?`, next, id); err != nil {
			return nil, fmt.Errorf("update channel management settings: %w", err)
		}
	}
	return keys, nil
}

/**
 * @brief 更新已锁定 Key 的编辑字段，保留身份、冷却及手工禁用。
 * @param ctx 数据库操作上下文。
 * @param tx 当前写事务。
 * @param current 按索引排序的 Key 快照。
 * @param edited 索引和密钥值仍匹配的待更新 Key。
 * @param updatedAt 本次写入的 Unix 秒。
 * @return Key 身份不符或持久化失败时返回错误。
 */
func (s *SQLStore) updateChannelEditKeysTx(ctx context.Context, tx *sql.Tx, current, edited []*model.APIKey, updatedAt int64) error {
	for _, key := range edited {
		i, found := slices.BinarySearchFunc(current, key.KeyIndex, func(old *model.APIKey, index int) int {
			return cmp.Compare(old.KeyIndex, index)
		})
		if !found || current[i].APIKey != key.APIKey {
			return model.ErrChannelEditConflict
		}
		old := current[i]
		sameScope := old.ModelScopeEmpty == key.ModelScopeEmpty && old.ModelScopeAuto == key.ModelScopeAuto &&
			slices.Equal(old.AllowedModels, key.AllowedModels) && slices.Equal(old.DetectedModels, key.DetectedModels)
		if old.KeyStrategy == key.KeyStrategy && old.Note == key.Note && old.Priority == key.Priority && old.Weight == key.Weight &&
			old.CostMultiplier == key.CostMultiplier && sameScope {
			continue
		}
		allowed, err := marshalAllowedModels(key.AllowedModels)
		if err != nil {
			return err
		}
		detected, err := marshalAllowedModels(key.DetectedModels)
		if err != nil {
			return err
		}
		disabled := old.Disabled
		if !sameScope {
			disabled = (old.Disabled && !old.ModelScopeEmpty) || key.ModelScopeEmpty
		}
		if _, err := s.execTx(ctx, tx, `
			UPDATE api_keys SET key_strategy = ?, note = ?, priority = ?, weight = ?, cost_multiplier = ?,
				allowed_models = ?, detected_models = ?, model_scope_empty = ?, model_scope_auto = ?, disabled = ?, updated_at = ?
			WHERE id = ?
		`, key.KeyStrategy, key.Note, key.Priority, key.Weight, normalizeCostMultiplier(key.CostMultiplier),
			allowed, detected, key.ModelScopeEmpty, key.ModelScopeAuto, disabled, updatedAt, old.ID); err != nil {
			return fmt.Errorf("update channel edit key index %d: %w", key.KeyIndex, err)
		}
	}
	return nil
}

/**
 * @brief 保留替换操作中同值 Key 的手动禁用状态，重复值按出现次序对应。
 * @param current 事务中锁定的现有 Key。
 * @param edited 编辑器提交的替换 Key。
 * @return 带当前禁用状态的替换行，不修改调用方数据。
 */
func channelEditReplacementKeys(current, edited []*model.APIKey) []*model.APIKey {
	disabledByValue := make(map[string][]bool, len(current))
	for _, key := range current {
		disabledByValue[key.APIKey] = append(disabledByValue[key.APIKey], key.Disabled && !key.ModelScopeEmpty)
	}
	replacement := make([]*model.APIKey, 0, len(edited))
	for _, key := range edited {
		entry := *key
		entry.Disabled = entry.ModelScopeEmpty
		if states := disabledByValue[key.APIKey]; len(states) > 0 {
			entry.Disabled = entry.Disabled || states[0]
			disabledByValue[key.APIKey] = states[1:]
		}
		replacement = append(replacement, &entry)
	}
	return replacement
}
