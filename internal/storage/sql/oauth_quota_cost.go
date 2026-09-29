package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errOAuthQuotaCostOverflow = errors.New("OAuth quota standard cost overflow")

type oauthQuotaCostCredentialEnvelope struct {
	OAuthUsage     json.RawMessage  `json:"oauth_usage"`
	QuotaCostUsage *oauthcost.Usage `json:"quota_cost_usage"`
}

// validateOAuthQuotaCostCredential 拒绝持久化非法的额度窗口边界。
func validateOAuthQuotaCostCredential(channelID int64, nextJSON string) error {
	var next oauthQuotaCostCredentialEnvelope
	if err := json.Unmarshal([]byte(nextJSON), &next); err != nil {
		return fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	if next.QuotaCostUsage == nil {
		return nil
	}
	if err := oauthcost.Validate(next.QuotaCostUsage); err != nil {
		return fmt.Errorf("validate OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	return nil
}

func isOAuthQuotaCreditLog(entry *model.LogEntry) bool {
	return entry != nil && entry.ChannelID > 0 && entry.Cost > 0 &&
		entry.LogSource != model.LogSourceJev && entry.CodexHasCredits
}

// LogsChargeOAuthCredits 报告这批日志是否会改写凭据（仅 Codex 已购额度）。
func LogsChargeOAuthCredits(logs []*model.LogEntry) bool {
	for _, entry := range logs {
		if isOAuthQuotaCreditLog(entry) {
			return true
		}
	}
	return false
}

// updateOAuthQuotaCreditsTx 仅将 Codex 已购额度成本累计进凭据；周期成本由账本承担。
func (s *SQLStore) updateOAuthQuotaCreditsTx(ctx context.Context, tx *sql.Tx, logs []*model.LogEntry) ([]int64, error) {
	credits := make(map[int64]int64)
	for _, entry := range logs {
		if !isOAuthQuotaCreditLog(entry) {
			continue
		}
		costMicroUSD, err := util.USDToMicroUSDSafe(entry.Cost)
		if err != nil {
			return nil, fmt.Errorf("convert OAuth quota credit cost for channel %d: %w", entry.ChannelID, err)
		}
		if credits[entry.ChannelID] > math.MaxInt64-costMicroUSD {
			return nil, errOAuthQuotaCostOverflow
		}
		credits[entry.ChannelID] += costMicroUSD
	}
	channelIDs := make([]int64, 0, len(credits))
	for channelID, cost := range credits {
		if cost > 0 {
			channelIDs = append(channelIDs, channelID)
		}
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
	updatedChannelIDs := make([]int64, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		authType, credentialJSON, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if model.NormalizeAuthType(authType) != model.AuthTypeCodexOAuth || strings.TrimSpace(credentialJSON) == "" {
			continue
		}
		var envelope oauthQuotaCostCredentialEnvelope
		if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
			return nil, fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		next := oauthcost.Clone(envelope.QuotaCostUsage)
		if next == nil {
			next = &oauthcost.Usage{}
		}
		if err := oauthcost.Validate(next); err != nil {
			return nil, fmt.Errorf("validate OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		if next.CreditStandardCostMicroUSD > math.MaxInt64-credits[channelID] {
			return nil, errOAuthQuotaCostOverflow
		}
		next.CreditStandardCostMicroUSD += credits[channelID]
		updatedCredential, err := replaceOAuthQuotaCostUsage(credentialJSON, next)
		if err != nil {
			return nil, fmt.Errorf("encode OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		if _, err := s.execTx(ctx, tx, `UPDATE channels SET oauth_credential = ?, updated_at = ? WHERE id = ?`,
			updatedCredential, timeToUnix(time.Now()), channelID); err != nil {
			return nil, err
		}
		updatedChannelIDs = append(updatedChannelIDs, channelID)
	}
	return updatedChannelIDs, nil
}

// ResetOAuthQuotaCostUsage 设置窗口计数起点；成本在读取时对账本求和。
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
	if !model.TracksQuotaCost(authType) || strings.TrimSpace(credentialJSON) == "" {
		return errors.New("OAuth credential is unavailable")
	}
	var envelope oauthQuotaCostCredentialEnvelope
	if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
		return fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	next := oauthcost.Reset(oauthcost.EffectiveUsage(envelope.QuotaCostUsage, envelope.OAuthUsage), resetAt)
	if err := oauthcost.Validate(next); err != nil {
		return err
	}
	updatedCredential, err := replaceOAuthQuotaCostUsage(credentialJSON, next)
	if err != nil {
		return err
	}
	if _, err := s.execTx(ctx, tx, `UPDATE channels SET oauth_credential = ?, updated_at = ? WHERE id = ?`,
		updatedCredential, timeToUnix(time.Now()), channelID); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceOAuthQuotaCostUsage(credentialJSON string, usage *oauthcost.Usage) (string, error) {
	raw := []byte(credentialJSON)
	if !sonic.Valid(raw) || !gjson.ParseBytes(raw).IsObject() {
		return "", errors.New("oauth credential must be a JSON object")
	}
	costJSON, err := json.Marshal(usage)
	if err != nil {
		return "", err
	}
	updatedCredential, err := sjson.SetRawBytes(raw, "quota_cost_usage", costJSON)
	if err != nil {
		return "", err
	}
	return string(updatedCredential), nil
}
