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
	QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage"`
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
		if envelope.QuotaCostUsage == nil {
			continue
		}
		next := oauthcost.Clone(envelope.QuotaCostUsage)
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
			entryChanged, err := oauthcost.AddStandardCost(next, entry.Time.Time, costMicroUSD)
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
	var costUSD float64
	if err := s.queryRowTx(ctx, tx, `
		SELECT COALESCE(SUM(cost), 0) FROM logs
		WHERE channel_id = ? AND time >= ? AND cost > 0
	`, channelID, resetAt.UnixMilli()).Scan(&costUSD); err != nil {
		return err
	}
	costMicroUSD, err := util.USDToMicroUSDSafe(costUSD)
	if err != nil {
		return err
	}
	next := oauthcost.Reset(envelope.QuotaCostUsage, resetAt, costMicroUSD)
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
