package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/util"
)

type oauthQuotaCostCredentialEnvelope struct {
	// OAuthUsage 是上一次额度采样的持久化快照，窗口边界的唯一来源。
	OAuthUsage     json.RawMessage  `json:"oauth_usage"`
	QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage"`
}

// quotaCostWindows 返回可用于累加的窗口集合：持久化计数器为空时，
// 从同一份凭证的额度采样快照 bootstrap。采样落盘即可累加，不必等人工刷新——
// 否则采样与首次刷新之间的全部消耗会被静默丢弃。
func quotaCostWindows(envelope *oauthQuotaCostCredentialEnvelope) *oauthcost.Usage {
	if envelope.QuotaCostUsage != nil && len(envelope.QuotaCostUsage.Windows) > 0 {
		return oauthcost.Clone(envelope.QuotaCostUsage)
	}
	return oauthcost.BootstrapFromSnapshot(envelope.OAuthUsage)
}

func (s *SQLStore) updateOAuthQuotaCostsTx(
	ctx context.Context,
	tx *sql.Tx,
	logs []*model.LogEntry,
) ([]int64, error) {
	byChannel := make(map[int64][]*model.LogEntry)
	for _, entry := range logs {
		if entry == nil || entry.ChannelID <= 0 || entry.Cost <= 0 {
			continue
		}
		byChannel[entry.ChannelID] = append(byChannel[entry.ChannelID], entry)
	}
	channelIDs := make([]int64, 0, len(byChannel))
	for channelID := range byChannel {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })

	updatedChannelIDs := make([]int64, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		authType, credentialJSON, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return nil, err
		}
		if !isOAuthAuthType(authType) || strings.TrimSpace(credentialJSON) == "" {
			continue
		}
		var envelope oauthQuotaCostCredentialEnvelope
		if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
			return nil, fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		next := quotaCostWindows(&envelope)
		if next == nil {
			continue
		}
		if err := oauthcost.Validate(next); err != nil {
			return nil, fmt.Errorf("validate OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		entries := byChannel[channelID]
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].Time.Before(entries[j].Time.Time)
		})
		changed := false
		for _, entry := range entries {
			costMicroUSD, err := util.USDToMicroUSDSafe(entry.Cost)
			if err != nil {
				return nil, fmt.Errorf("convert OAuth quota standard cost for channel %d: %w", channelID, err)
			}
			entryChanged, err := oauthcost.AddStandardCost(next, entry.Time.Time, quotaCostModel(entry), costMicroUSD)
			if err != nil {
				return nil, fmt.Errorf("accumulate OAuth quota standard cost for channel %d: %w", channelID, err)
			}
			changed = changed || entryChanged
		}
		if !changed {
			continue
		}
		updatedCredential, err := replaceOAuthQuotaCostUsage(credentialJSON, next)
		if err != nil {
			return nil, fmt.Errorf("encode OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		if _, err := s.execTx(ctx, tx, `
			UPDATE channels SET oauth_credential = ?, updated_at = ? WHERE id = ?
		`, string(updatedCredential), timeToUnix(time.Now()), channelID); err != nil {
			return nil, err
		}
		updatedChannelIDs = append(updatedChannelIDs, channelID)
	}
	return updatedChannelIDs, nil
}

// ResetOAuthQuotaCostUsage atomically starts local counters at resetAt and
// includes logs that reached durable storage after that cutoff but before this
// transaction acquired the channel row lock.
func (s *SQLStore) ResetOAuthQuotaCostUsage(ctx context.Context, channelID int64, resetAt time.Time) error {
	if channelID <= 0 || resetAt.IsZero() {
		return errors.New("OAuth quota cost reset is invalid")
	}
	resetAt = resetAt.UTC()
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	authType, credentialJSON, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
	if err != nil {
		return err
	}
	if !isOAuthAuthType(authType) || strings.TrimSpace(credentialJSON) == "" {
		return errors.New("OAuth credential is unavailable")
	}
	var envelope oauthQuotaCostCredentialEnvelope
	if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
		return fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	if envelope.QuotaCostUsage == nil {
		return tx.Commit()
	}
	costByFamily, err := s.sumOAuthQuotaCostByFamily(ctx, tx, channelID, resetAt, envelope.QuotaCostUsage)
	if err != nil {
		return err
	}
	next := oauthcost.Reset(envelope.QuotaCostUsage, resetAt, costByFamily)
	if err := oauthcost.Validate(next); err != nil {
		return err
	}
	updatedCredential, err := replaceOAuthQuotaCostUsage(credentialJSON, next)
	if err != nil {
		return err
	}
	if _, err := s.execTx(ctx, tx, `
		UPDATE channels SET oauth_credential = ?, updated_at = ? WHERE id = ?
	`, updatedCredential, timeToUnix(time.Now()), channelID); err != nil {
		return err
	}
	return tx.Commit()
}

// quotaCostModel 返回该条日志实际消耗的上游模型；额度窗口按实际上游模型分族。
func quotaCostModel(entry *model.LogEntry) string {
	if actual := strings.TrimSpace(entry.ActualModel); actual != "" {
		return actual
	}
	return entry.Model
}

// sumOAuthQuotaCostByFamily 按模型族汇总 resetAt 之后已落盘的标准成本——
// 只覆盖部分模型的窗口不能吃下其他模型的消耗。
func (s *SQLStore) sumOAuthQuotaCostByFamily(
	ctx context.Context,
	tx *sql.Tx,
	channelID int64,
	resetAt time.Time,
	usage *oauthcost.Usage,
) (map[string]int64, error) {
	families := oauthcost.Families(usage)
	if len(families) == 0 {
		return nil, nil
	}
	rows, err := s.queryTx(ctx, tx, `
		SELECT model, actual_model, COALESCE(SUM(cost), 0) FROM logs
		WHERE channel_id = ? AND time >= ? AND cost > 0
		GROUP BY model, actual_model
	`, channelID, resetAt.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	costUSDByFamily := make(map[string]float64, len(families))
	for rows.Next() {
		var modelName, actualModel string
		var costUSD float64
		if err := rows.Scan(&modelName, &actualModel, &costUSD); err != nil {
			return nil, err
		}
		if actual := strings.TrimSpace(actualModel); actual != "" {
			modelName = actual
		}
		matchedFamilies := make(map[string]struct{}, len(families))
		for _, window := range usage.Windows {
			if window == nil {
				continue
			}
			if _, matched := matchedFamilies[window.Family]; matched {
				continue
			}
			if oauthcost.WindowMatchesModel(window, modelName) {
				costUSDByFamily[window.Family] += costUSD
				matchedFamilies[window.Family] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	costByFamily := make(map[string]int64, len(costUSDByFamily))
	for family, costUSD := range costUSDByFamily {
		costMicroUSD, err := util.USDToMicroUSDSafe(costUSD)
		if err != nil {
			return nil, err
		}
		costByFamily[family] = costMicroUSD
	}
	return costByFamily, nil
}

func replaceOAuthQuotaCostUsage(credentialJSON string, usage *oauthcost.Usage) (string, error) {
	var credentialFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(credentialJSON), &credentialFields); err != nil {
		return "", err
	}
	costJSON, err := json.Marshal(usage)
	if err != nil {
		return "", err
	}
	credentialFields["quota_cost_usage"] = costJSON
	updatedCredential, err := json.Marshal(credentialFields)
	return string(updatedCredential), err
}

func isOAuthAuthType(authType string) bool {
	switch model.NormalizeAuthType(authType) {
	case model.AuthTypeCodexOAuth, model.AuthTypeAnthropicOAuth,
		model.AuthTypeAntigravityOAuth, model.AuthTypeXAIOAuth:
		return true
	default:
		return false
	}
}
