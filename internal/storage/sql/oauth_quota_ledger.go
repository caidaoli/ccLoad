package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/util"
)

// OAuthQuotaLogEffects 描述一次日志事务造成的 OAuth 额度副作用。
type OAuthQuotaLogEffects struct {
	CredentialChannelIDs []int64
	LedgerSlices         []OAuthQuotaLedgerSlice
}

// OAuthQuotaLedgerReplicationSlice 是混合模式账本复制的分片宽度（秒）。
const OAuthQuotaLedgerReplicationSlice = int64(60)

// OAuthQuotaLedgerSlice 标识一个待复制的账本时间分片。
type OAuthQuotaLedgerSlice struct {
	ChannelID  int64
	SliceStart int64
}

// OAuthQuotaLedgerRow 是账本的一行，用于副本复制与查询。
type OAuthQuotaLedgerRow struct {
	BucketAt     int64
	Model        string
	WindowKey    string
	CostMicroUSD int64
}

type oauthQuotaLedgerKey struct {
	channelID int64
	bucketAt  int64
	model     string
	windowKey string
}

const (
	oauthQuotaLedgerChunkSize        = 150
	oauthQuotaChannelLookupChunkSize = 500
)

func sqlPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}

func (s *SQLStore) applyOAuthQuotaLogEffectsTx(ctx context.Context, tx *sql.Tx, logs []*model.LogEntry) (OAuthQuotaLogEffects, error) {
	credentialIDs, err := s.updateOAuthQuotaCostsTx(ctx, tx, logs)
	if err != nil {
		return OAuthQuotaLogEffects{}, err
	}
	slices, err := s.addOAuthQuotaLedgerTx(ctx, tx, logs)
	if err != nil {
		return OAuthQuotaLogEffects{}, err
	}
	return OAuthQuotaLogEffects{CredentialChannelIDs: credentialIDs, LedgerSlices: slices}, nil
}

func (s *SQLStore) loadChannelAuthTypesTx(ctx context.Context, tx *sql.Tx, channelIDs []int64) (map[int64]string, error) {
	authTypes := make(map[int64]string, len(channelIDs))
	for start := 0; start < len(channelIDs); start += oauthQuotaChannelLookupChunkSize {
		chunk := channelIDs[start:min(start+oauthQuotaChannelLookupChunkSize, len(channelIDs))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.queryTx(ctx, tx, `SELECT id, auth_type FROM channels WHERE id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("load channel auth types: %w", err)
		}
		for rows.Next() {
			var id int64
			var authType string
			if err := rows.Scan(&id, &authType); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan channel auth type: %w", err)
			}
			authTypes[id] = authType
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("read channel auth types: %w", err)
		}
	}
	return authTypes, nil
}

func (s *SQLStore) addOAuthQuotaLedgerTx(ctx context.Context, tx *sql.Tx, logs []*model.LogEntry) ([]OAuthQuotaLedgerSlice, error) {
	candidates := make(map[int64]struct{})
	for _, entry := range logs {
		if entry != nil && entry.ChannelID > 0 && entry.Cost > 0 && entry.LogSource != model.LogSourceJev {
			candidates[entry.ChannelID] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	channelIDs := make([]int64, 0, len(candidates))
	for id := range candidates {
		channelIDs = append(channelIDs, id)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
	authTypes, err := s.loadChannelAuthTypesTx(ctx, tx, channelIDs)
	if err != nil {
		return nil, err
	}

	totals := make(map[oauthQuotaLedgerKey]int64)
	for _, entry := range logs {
		if entry == nil {
			continue
		}
		authType, ok := authTypes[entry.ChannelID]
		if !ok || !model.CountsTowardQuotaWindows(authType, entry.LogSource, entry.Cost, entry.CodexHasCredits) {
			continue
		}
		costMicroUSD, err := util.USDToMicroUSDSafe(entry.Cost)
		if err != nil {
			return nil, fmt.Errorf("convert OAuth quota ledger cost for channel %d: %w", entry.ChannelID, err)
		}
		if costMicroUSD <= 0 {
			continue
		}
		key := oauthQuotaLedgerKey{channelID: entry.ChannelID, bucketAt: entry.Time.Unix(),
			model: oauthcost.LedgerModel(entry.Model, entry.ActualModel)}
		if totals[key] > math.MaxInt64-costMicroUSD {
			return nil, errOAuthQuotaCostOverflow
		}
		totals[key] += costMicroUSD
	}
	if len(totals) == 0 {
		return nil, nil
	}

	keys := make([]oauthQuotaLedgerKey, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.channelID != b.channelID {
			return a.channelID < b.channelID
		}
		if a.bucketAt != b.bucketAt {
			return a.bucketAt < b.bucketAt
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.windowKey < b.windowKey
	})
	for start := 0; start < len(keys); start += oauthQuotaLedgerChunkSize {
		chunk := keys[start:min(start+oauthQuotaLedgerChunkSize, len(keys))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*5)
		for i, key := range chunk {
			values[i] = "(?, ?, ?, ?, ?)"
			args = append(args, key.channelID, key.bucketAt, key.model, key.windowKey, totals[key])
		}
		query := `INSERT INTO oauth_quota_cost_ledger (channel_id, bucket_at, model, window_key, cost_microusd) VALUES ` +
			strings.Join(values, ", ") + s.oauthQuotaLedgerAccumulateClause()
		if _, err := s.execTx(ctx, tx, query, args...); err != nil {
			return nil, fmt.Errorf("upsert OAuth quota ledger: %w", err)
		}
	}

	slices := make([]OAuthQuotaLedgerSlice, 0, len(keys))
	for _, key := range keys {
		slice := OAuthQuotaLedgerSlice{ChannelID: key.channelID,
			SliceStart: key.bucketAt - key.bucketAt%OAuthQuotaLedgerReplicationSlice}
		if len(slices) == 0 || slices[len(slices)-1] != slice {
			slices = append(slices, slice)
		}
	}
	return slices, nil
}

func (s *SQLStore) oauthQuotaLedgerAccumulateClause() string {
	if s.supportsONConflict() {
		return ` ON CONFLICT (channel_id, bucket_at, model, window_key) DO UPDATE SET cost_microusd = oauth_quota_cost_ledger.cost_microusd + excluded.cost_microusd`
	}
	return ` ON DUPLICATE KEY UPDATE cost_microusd = cost_microusd + VALUES(cost_microusd)`
}

// ListOAuthQuotaLedgerReplica 返回渠道在 [from, until) 秒内的账本行。
func (s *SQLStore) ListOAuthQuotaLedgerReplica(ctx context.Context, channelID, from, until int64) ([]OAuthQuotaLedgerRow, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT bucket_at, model, window_key, cost_microusd FROM oauth_quota_cost_ledger
		WHERE channel_id = ? AND bucket_at >= ? AND bucket_at < ? ORDER BY bucket_at, model, window_key`), channelID, from, until)
	if err != nil {
		return nil, fmt.Errorf("list OAuth quota ledger: %w", err)
	}
	var result []OAuthQuotaLedgerRow
	for rows.Next() {
		var row OAuthQuotaLedgerRow
		if err := rows.Scan(&row.BucketAt, &row.Model, &row.WindowKey, &row.CostMicroUSD); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan OAuth quota ledger: %w", err)
		}
		result = append(result, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read OAuth quota ledger: %w", err)
	}
	return result, nil
}

// CleanupOAuthQuotaLedgerBefore 删除早于 cutoff 所在秒的账本行。
func (s *SQLStore) CleanupOAuthQuotaLedgerBefore(ctx context.Context, cutoff time.Time) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM oauth_quota_cost_ledger WHERE bucket_at < ?`), cutoff.Unix()); err != nil {
		return fmt.Errorf("cleanup OAuth quota ledger: %w", err)
	}
	return nil
}
