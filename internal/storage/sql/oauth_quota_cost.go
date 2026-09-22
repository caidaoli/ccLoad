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
	return oauthcost.BootstrapFromSnapshot(envelope.QuotaCostUsage, envelope.OAuthUsage)
}

// The channel row is already locked by the credential CAS. Logs committed
// before that lock are included here; in-flight log transactions accumulate
// against the resulting window after acquiring the same lock.
func (s *SQLStore) reconcileOAuthQuotaCostsTx(
	ctx context.Context, tx *sql.Tx, channelID int64, nextJSON string,
) (string, *oauthcost.Usage, error) {
	var next oauthQuotaCostCredentialEnvelope
	if err := json.Unmarshal([]byte(nextJSON), &next); err != nil {
		return "", nil, fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	usage := next.QuotaCostUsage
	if usage == nil {
		return nextJSON, nil, nil
	}
	if err := oauthcost.Validate(usage); err != nil {
		return "", nil, fmt.Errorf("validate OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	for _, window := range usage.Windows {
		if err := s.reconcileOAuthQuotaWindow(ctx, tx, channelID, window); err != nil {
			return "", nil, fmt.Errorf("reconcile OAuth quota cost for channel %d: %w", channelID, err)
		}
	}
	payload, err := replaceOAuthQuotaCostUsage(nextJSON, usage)
	if err != nil {
		return "", nil, fmt.Errorf("encode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	return payload, usage, nil
}

// reconcileOAuthQuotaWindow 把累计成本从「已计入区间」搬到当前边界。
// 只查询两个区间的对称差：日志保留期可以短于周/月额度周期，按新边界全量
// 重算会把保留期外已累计的历史成本直接抹掉。
func (s *SQLStore) reconcileOAuthQuotaWindow(
	ctx context.Context, tx *sql.Tx, channelID int64, window *oauthcost.Window,
) error {
	from, until := oauthcost.CountFrom(window), window.ResetAt
	if !oauthcost.Accounted(window) {
		// 已计入区间未知：按当前边界重算一次。重算只能看到保留期内的日志，
		// 与累计值同为下界，取较大者，绝不会因日志过期抹掉真实成本。
		cost, err := s.sumOAuthQuotaWindowCost(ctx, tx, channelID, window, from, until, 0, 0)
		if err != nil {
			return err
		}
		window.StandardCostMicroUSD = max(window.StandardCostMicroUSD, cost)
		oauthcost.MarkAccounted(window, from, until)
		return nil
	}
	accountedFrom, accountedUntil := window.AccountedFrom, window.AccountedUntil
	if accountedFrom == from && accountedUntil == until {
		return nil
	}
	added, err := s.sumOAuthQuotaWindowCost(ctx, tx, channelID, window, from, until, accountedFrom, accountedUntil)
	if err != nil {
		return err
	}
	removed, err := s.sumOAuthQuotaWindowCost(ctx, tx, channelID, window, accountedFrom, accountedUntil, from, until)
	if err != nil {
		return err
	}
	if added > math.MaxInt64-window.StandardCostMicroUSD {
		return errOAuthQuotaCostOverflow
	}
	// 移出区间的日志可能已被保留期清理，减不干净只会少减；宁可少减也不能多减。
	window.StandardCostMicroUSD = max(0, window.StandardCostMicroUSD+added-removed)
	oauthcost.MarkAccounted(window, from, until)
	return nil
}

// sumOAuthQuotaWindowCost 汇总 [from, until) 内本窗口模型族的成本，
// 排除 [excludeFrom, excludeUntil)；排除区间为空时汇总整个区间。
func (s *SQLStore) sumOAuthQuotaWindowCost(
	ctx context.Context, tx *sql.Tx, channelID int64, window *oauthcost.Window,
	from, until, excludeFrom, excludeUntil int64,
) (int64, error) {
	if until <= from {
		return 0, nil
	}
	if excludeUntil <= excludeFrom || excludeUntil <= from || excludeFrom >= until {
		return s.sumOAuthQuotaLogCost(ctx, tx, channelID, window, from, until)
	}
	head, err := s.sumOAuthQuotaLogCost(ctx, tx, channelID, window, from, excludeFrom)
	if err != nil {
		return 0, err
	}
	tail, err := s.sumOAuthQuotaLogCost(ctx, tx, channelID, window, excludeUntil, until)
	if err != nil {
		return 0, err
	}
	if head > math.MaxInt64-tail {
		return 0, errOAuthQuotaCostOverflow
	}
	return head + tail, nil
}

// microUSDSumExpr 返回「逐条取整后求和」的方言表达式。
//
// 取整必须与增量记账的 util.USDToMicroUSDSafe 同源，后者是
// int64(math.Round(usd * 1e6))：双精度乘法 + 半数远离零。
// Postgres 与 MySQL 的 ROUND 作用于双精度时走 C 的 rint 语义（半数取偶），
// 实测 2.5 落到 2 而 Go 给 3。对正成本这个偏差恒为 0 或 -1 微美元、方向单一，
// 不会正负抵消，只会随日志量单调累积；且对称差分支的 added 与手动重置基线
// 都不经过 max 保护，少算会直接落进持久化累计。
// 先把乘积转成十进制再取整即可复刻 Go 的语义：乘法仍在双精度下完成，
// 与 Go 的运算顺序一致，只有 tie 的方向被纠正。SQLite 的 ROUND 本就是
// 半数远离零，保持原样。
// 外层再转回双精度是为了让 Scan 目标类型在三种驱动下保持一致；求和结果是
// 整数微美元，远小于 2^53，转换无损。
func (s *SQLStore) microUSDSumExpr() string {
	switch {
	case s.IsPostgres():
		return "SUM(ROUND((cost * 1000000)::numeric))::double precision"
	case s.IsMySQL():
		return "CAST(SUM(ROUND(CAST(cost * 1000000 AS DECIMAL(65, 30)))) AS DOUBLE)"
	default:
		return "SUM(ROUND(cost * 1000000))"
	}
}

// sumOAuthQuotaLogCost 汇总 [from, until) 内本窗口模型族的成本。
//
// 取整必须逐条进行，与增量累计同源（AddStandardCost 对每条日志单独取微美元），
// 所以 SQL 侧只做「按模型分组求和取整后的条数与总额」是不够的——SUM(cost) 再
// 取整会和逐条取整产生偏差。这里用 SUM(ROUND(cost * 1e6)) 把逐条取整下推到
// SQL：ROUND 作用于每一行，与 util.USDToMicroUSDSafe 的 math.Round 语义一致，
// 分组后返回的行数等于模型基数（通常 <20）而非日志条数，避免在渠道行锁内做
// 与日志量成正比的回表扫描。
func (s *SQLStore) sumOAuthQuotaLogCost(
	ctx context.Context, tx *sql.Tx, channelID int64, window *oauthcost.Window, from, until int64,
) (int64, error) {
	if until <= from {
		return 0, nil
	}
	rows, err := s.queryTx(ctx, tx, fmt.Sprintf(`SELECT model, actual_model, %s FROM logs
		WHERE channel_id = ? AND time >= ? AND time < ? AND cost > 0 AND codex_has_credits = 0 AND log_source <> 'jev'
		GROUP BY model, actual_model`, s.microUSDSumExpr()),
		channelID, time.Unix(from, 0).UnixMilli(), time.Unix(until, 0).UnixMilli())
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var total int64
	for rows.Next() {
		var entry model.LogEntry
		var costMicroUSD float64
		if err := rows.Scan(&entry.Model, &entry.ActualModel, &costMicroUSD); err != nil {
			return 0, err
		}
		if !oauthcost.WindowMatchesModel(window, quotaCostModel(&entry)) {
			continue
		}
		cost, err := microUSDFromSQLSum(costMicroUSD)
		if err != nil {
			return 0, err
		}
		if cost > math.MaxInt64-total {
			return 0, errOAuthQuotaCostOverflow
		}
		total += cost
	}
	return total, rows.Err()
}

// microUSDFromSQLSum 把 SUM(ROUND(cost * 1e6)) 的结果转成微美元整数。
// 各方言对 SUM 的返回类型不一致（SQLite 可能给 float，MySQL 给 DECIMAL），
// 统一按 float64 扫描后校验范围；负数与非有限值视为数据损坏。
func microUSDFromSQLSum(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("OAuth quota standard cost sum is not finite")
	}
	if value < 0 {
		return 0, errors.New("OAuth quota standard cost cannot be negative")
	}
	if value > math.MaxInt64 {
		return 0, errOAuthQuotaCostOverflow
	}
	return int64(math.Round(value)), nil
}

func (s *SQLStore) updateOAuthQuotaCostsTx(
	ctx context.Context,
	tx *sql.Tx,
	logs []*model.LogEntry,
) ([]int64, error) {
	byChannel := make(map[int64][]*model.LogEntry)
	for _, entry := range logs {
		if entry == nil || entry.ChannelID <= 0 || entry.Cost <= 0 || entry.LogSource == model.LogSourceJev {
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
		if !model.TracksQuotaCost(authType) || strings.TrimSpace(credentialJSON) == "" {
			continue
		}
		var envelope oauthQuotaCostCredentialEnvelope
		if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
			return nil, fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
		}
		next := quotaCostWindows(&envelope)
		if next == nil {
			next = &oauthcost.Usage{}
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
			if entry.CodexHasCredits && authType == model.AuthTypeCodexOAuth {
				if next.CreditStandardCostMicroUSD > math.MaxInt64-costMicroUSD {
					return nil, errOAuthQuotaCostOverflow
				}
				next.CreditStandardCostMicroUSD += costMicroUSD
				changed = true
				continue
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
	if !model.TracksQuotaCost(authType) || strings.TrimSpace(credentialJSON) == "" {
		return errors.New("OAuth credential is unavailable")
	}
	var envelope oauthQuotaCostCredentialEnvelope
	if err := json.Unmarshal([]byte(credentialJSON), &envelope); err != nil {
		return fmt.Errorf("decode OAuth quota cost credential for channel %d: %w", channelID, err)
	}
	usage := quotaCostWindows(&envelope)
	costByFamily, err := s.sumOAuthQuotaCostByFamily(ctx, tx, channelID, resetAt, usage)
	if err != nil {
		return err
	}
	next := oauthcost.Reset(usage, resetAt, costByFamily)
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
	rows, err := s.queryTx(ctx, tx, fmt.Sprintf(`
		SELECT model, actual_model, %s FROM logs
		WHERE channel_id = ? AND time >= ? AND cost > 0 AND codex_has_credits = 0 AND log_source <> 'jev'
		GROUP BY model, actual_model
	`, s.microUSDSumExpr()), channelID, resetAt.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	costByFamily := make(map[string]int64, len(families))
	for rows.Next() {
		var modelName, actualModel string
		var costSum float64
		if err := rows.Scan(&modelName, &actualModel, &costSum); err != nil {
			return nil, err
		}
		// Match incremental accounting: rounding happens per log row in SQL.
		costMicroUSD, err := microUSDFromSQLSum(costSum)
		if err != nil {
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
			family := oauthcost.WindowModelFamily(window)
			if _, matched := matchedFamilies[family]; matched {
				continue
			}
			if oauthcost.WindowMatchesModel(window, modelName) {
				if costByFamily[family] > math.MaxInt64-costMicroUSD {
					return nil, errOAuthQuotaCostOverflow
				}
				costByFamily[family] += costMicroUSD
				matchedFamilies[family] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return costByFamily, nil
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
