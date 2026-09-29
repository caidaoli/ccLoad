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
	// LedgerSlices 是本次写入触及的账本分片起点（秒，去重升序），跨渠道共享。
	LedgerSlices []int64
}

// OAuthQuotaLedgerReplicationSlice 是混合模式账本复制的分片宽度（秒）。
const OAuthQuotaLedgerReplicationSlice = int64(60)

// OAuthQuotaLedgerRow 是账本一行除渠道外的字段。
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
	credentialIDs, err := s.updateOAuthQuotaCreditsTx(ctx, tx, logs)
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

func (s *SQLStore) addOAuthQuotaLedgerTx(ctx context.Context, tx *sql.Tx, logs []*model.LogEntry) ([]int64, error) {
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

	seen := make(map[int64]struct{})
	slices := make([]int64, 0, len(keys))
	for _, key := range keys {
		start := key.bucketAt - key.bucketAt%OAuthQuotaLedgerReplicationSlice
		if _, ok := seen[start]; !ok {
			seen[start] = struct{}{}
			slices = append(slices, start)
		}
	}
	sort.Slice(slices, func(i, j int) bool { return slices[i] < slices[j] })
	return slices, nil
}

func (s *SQLStore) oauthQuotaLedgerAccumulateClause() string {
	if s.supportsONConflict() {
		return ` ON CONFLICT (channel_id, bucket_at, model, window_key) DO UPDATE SET cost_microusd = oauth_quota_cost_ledger.cost_microusd + excluded.cost_microusd`
	}
	return ` ON DUPLICATE KEY UPDATE cost_microusd = cost_microusd + VALUES(cost_microusd)`
}

// OAuthQuotaLedgerReplicaRow 是跨渠道复制时的账本行。
type OAuthQuotaLedgerReplicaRow struct {
	ChannelID int64
	OAuthQuotaLedgerRow
}

// ListOAuthQuotaLedgerRangeReplica 读取所有渠道在 [from, until) 秒内的账本行，按主键排序。
func (s *SQLStore) ListOAuthQuotaLedgerRangeReplica(ctx context.Context, from, until int64) ([]OAuthQuotaLedgerReplicaRow, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT channel_id, bucket_at, model, window_key, cost_microusd FROM oauth_quota_cost_ledger
		WHERE bucket_at >= ? AND bucket_at < ? ORDER BY channel_id, bucket_at, model, window_key`), from, until)
	if err != nil {
		return nil, fmt.Errorf("list OAuth quota ledger range: %w", err)
	}
	var result []OAuthQuotaLedgerReplicaRow
	for rows.Next() {
		var row OAuthQuotaLedgerReplicaRow
		if err := rows.Scan(&row.ChannelID, &row.BucketAt, &row.Model, &row.WindowKey, &row.CostMicroUSD); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan OAuth quota ledger range: %w", err)
		}
		result = append(result, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read OAuth quota ledger range: %w", err)
	}
	return result, nil
}

// ReplaceOAuthQuotaLedgerRangeReplica 用 rows 原子替换所有渠道在 [from, until) 秒内的账本行。
// rows 必须来自权威库的同一区间（主键唯一）；重复执行结果相同。
func (s *SQLStore) ReplaceOAuthQuotaLedgerRangeReplica(ctx context.Context, from, until int64, rows []OAuthQuotaLedgerReplicaRow) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := s.execTx(ctx, tx, `DELETE FROM oauth_quota_cost_ledger WHERE bucket_at >= ? AND bucket_at < ?`, from, until); err != nil {
			return fmt.Errorf("clear OAuth quota ledger range: %w", err)
		}
		for start := 0; start < len(rows); start += oauthQuotaLedgerChunkSize {
			chunk := rows[start:min(start+oauthQuotaLedgerChunkSize, len(rows))]
			values := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)*5)
			for i, row := range chunk {
				values[i] = "(?, ?, ?, ?, ?)"
				args = append(args, row.ChannelID, row.BucketAt, row.Model, row.WindowKey, row.CostMicroUSD)
			}
			if _, err := s.execTx(ctx, tx, `INSERT INTO oauth_quota_cost_ledger (channel_id, bucket_at, model, window_key, cost_microusd) VALUES `+
				strings.Join(values, ", "), args...); err != nil {
				return fmt.Errorf("insert OAuth quota ledger range: %w", err)
			}
		}
		return nil
	})
}

// CleanupOAuthQuotaLedgerBefore 删除早于 cutoff 所在秒的账本行。
func (s *SQLStore) CleanupOAuthQuotaLedgerBefore(ctx context.Context, cutoff time.Time) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM oauth_quota_cost_ledger WHERE bucket_at < ?`), cutoff.Unix()); err != nil {
		return fmt.Errorf("cleanup OAuth quota ledger: %w", err)
	}
	return nil
}

const oauthQuotaLedgerSumChunkSize = 200

func (s *SQLStore) ledgerSumType() string {
	switch {
	case s.IsMySQL():
		return "SIGNED"
	case s.IsPostgres():
		return "BIGINT"
	default:
		return "INTEGER"
	}
}

// OAuthQuotaCostViews 返回各渠道在 at 时刻按账本计费的窗口视图。
// 每个渠道只扫描一次所有窗口计数区间的并集：按区间端点切成基本分段在 SQL 端聚合，
// 每个窗口恰好覆盖若干连续分段，返回行数只取决于分段与模型数，与流量无关。
func (s *SQLStore) OAuthQuotaCostViews(ctx context.Context, usages map[int64]*oauthcost.Usage, at time.Time) (map[int64]*oauthcost.CostView, error) {
	type channelSegments struct {
		id      int64
		windows []*oauthcost.Window
		bounds  []int64 // 非空计数区间的端点，去重升序；分段 i 为 [bounds[i], bounds[i+1])
	}
	views := make(map[int64]*oauthcost.CostView, len(usages))
	channels := make([]channelSegments, 0, len(usages))
	for id, usage := range usages {
		if usage == nil {
			continue
		}
		windows := oauthcost.WindowsAt(usage, at)
		bounds := oauthQuotaLedgerSegmentBounds(windows)
		if len(bounds) < 2 {
			views[id] = oauthcost.NewCostView(usage, windows, nil)
			continue
		}
		channels = append(channels, channelSegments{id: id, windows: windows, bounds: bounds})
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].id < channels[j].id })

	sumType := s.ledgerSumType()
	for start := 0; start < len(channels); start += oauthQuotaLedgerSumChunkSize {
		chunk := channels[start:min(start+oauthQuotaLedgerSumChunkSize, len(channels))]
		parts := make([]string, len(chunk))
		var args []any
		for i, ch := range chunk {
			// WHERE 保证 bucket_at < 末端点，CASE 必然命中某个分段。
			var seg strings.Builder
			seg.WriteString("CASE")
			for j, bound := range ch.bounds[1:] {
				fmt.Fprintf(&seg, " WHEN bucket_at < ? THEN %d", j)
				args = append(args, bound)
			}
			seg.WriteString(" END")
			parts[i] = fmt.Sprintf(`SELECT %d AS idx, %s AS seg, model, window_key, CAST(SUM(cost_microusd) AS %s) AS cost
				FROM oauth_quota_cost_ledger WHERE channel_id = ? AND bucket_at >= ? AND bucket_at < ?
				GROUP BY seg, model, window_key`, i, seg.String(), sumType)
			args = append(args, ch.id, ch.bounds[0], ch.bounds[len(ch.bounds)-1])
		}
		rows, err := s.queryWith(ctx, s.db, strings.Join(parts, " UNION ALL "), args...)
		if err != nil {
			return nil, fmt.Errorf("sum OAuth quota ledger: %w", err)
		}
		totals := make([][][]oauthcost.LedgerTotal, len(chunk))
		for i, ch := range chunk {
			totals[i] = make([][]oauthcost.LedgerTotal, len(ch.windows))
		}
		for rows.Next() {
			var idx, seg int
			var total oauthcost.LedgerTotal
			if err := rows.Scan(&idx, &seg, &total.Model, &total.WindowKey, &total.CostMicroUSD); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan OAuth quota ledger sum: %w", err)
			}
			if idx < 0 || idx >= len(chunk) || seg < 0 || seg >= len(chunk[idx].bounds)-1 {
				_ = rows.Close()
				return nil, fmt.Errorf("scan OAuth quota ledger sum: segment %d/%d out of bounds", idx, seg)
			}
			ch := chunk[idx]
			for w, window := range ch.windows {
				counted := oauthcost.CountedRange(window)
				if ch.bounds[seg] >= counted.From && ch.bounds[seg+1] <= counted.Until {
					totals[idx][w] = append(totals[idx][w], total)
				}
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("read OAuth quota ledger sum: %w", err)
		}
		for i, ch := range chunk {
			views[ch.id] = oauthcost.NewCostView(usages[ch.id], ch.windows, totals[i])
		}
	}
	return views, nil
}

// oauthQuotaLedgerSegmentBounds 返回所有非空计数区间的端点（去重升序）。
func oauthQuotaLedgerSegmentBounds(windows []*oauthcost.Window) []int64 {
	var bounds []int64
	for _, window := range windows {
		if counted := oauthcost.CountedRange(window); counted.From < counted.Until {
			bounds = append(bounds, counted.From, counted.Until)
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	unique := bounds[:0]
	for _, bound := range bounds {
		if len(unique) == 0 || unique[len(unique)-1] != bound {
			unique = append(unique, bound)
		}
	}
	return unique
}
