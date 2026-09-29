package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"

	"github.com/tidwall/gjson"
)

const oauthQuotaCostLedgerMigrationVersion = "v7_oauth_quota_cost_ledger"

// ensureOAuthQuotaLedgerBinaryKeys 让 MySQL 账本主键与 SQLite 的区分大小写语义一致。
// DDL 在 v7 回填事务之前执行；仅旧表列排序规则不匹配时改表。
func ensureOAuthQuotaLedgerBinaryKeys(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect != DialectMySQL {
		return nil
	}
	var total, binaryKeys int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN CHARACTER_SET_NAME = 'utf8mb4' AND COLLATION_NAME = 'utf8mb4_bin' THEN 1 ELSE 0 END), 0)
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'oauth_quota_cost_ledger' AND COLUMN_NAME IN ('model', 'window_key')
	`).Scan(&total, &binaryKeys); err != nil {
		return fmt.Errorf("query OAuth quota ledger key collations: %w", err)
	}
	if total != 2 {
		return fmt.Errorf("OAuth quota ledger key columns: got %d, want 2", total)
	}
	if binaryKeys == 2 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE oauth_quota_cost_ledger
		MODIFY COLUMN model VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		MODIFY COLUMN window_key VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("modify OAuth quota ledger key collations: %w", err)
	}
	return nil
}

// 每行 5 个参数；150 行低于旧版 SQLite 的 999 参数上限。
const oauthQuotaLedgerBaselineChunkSize = 150

type oauthQuotaLegacyChannel struct {
	id         int64
	credential string
}

type oauthQuotaBaseline struct {
	channelID int64
	bucketAt  int64
	windowKey string
	cost      int64
}

// backfillOAuthQuotaCostLedger 仅从旧凭据和现有账本计算当前周期的差额基线。
func backfillOAuthQuotaCostLedger(ctx context.Context, db *sql.DB, dialect Dialect) error {
	var options *sql.TxOptions
	if dialect != DialectSQLite {
		// 渠道凭据与账本行必须来自同一快照，避免并发双写时少补基线。
		options = &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}
	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var applied int
	if err := tx.QueryRowContext(ctx, rebindIfPostgres(dialect,
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?"), oauthQuotaCostLedgerMigrationVersion).Scan(&applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}

	channels, err := loadOAuthQuotaLegacyChannels(ctx, tx)
	if err != nil {
		return err
	}
	baselines, err := oauthQuotaLegacyBaselines(ctx, tx, dialect, channels, time.Now())
	if err != nil {
		return err
	}
	if err := insertOAuthQuotaBaselines(ctx, tx, dialect, baselines); err != nil {
		return err
	}
	if err := recordMigrationTx(ctx, tx, oauthQuotaCostLedgerMigrationVersion, dialect); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("[INFO] OAuth quota ledger migrated: %d legacy baselines for %d channels", len(baselines), len(channels))
	return nil
}

func loadOAuthQuotaLegacyChannels(ctx context.Context, tx *sql.Tx) ([]oauthQuotaLegacyChannel, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, auth_type, oauth_credential FROM channels ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("load channels for OAuth quota ledger: %w", err)
	}
	var channels []oauthQuotaLegacyChannel
	for rows.Next() {
		var id int64
		var authType string
		var credential sql.NullString
		if err := rows.Scan(&id, &authType, &credential); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan channel for OAuth quota ledger: %w", err)
		}
		if model.TracksQuotaCost(authType) && credential.Valid {
			channels = append(channels, oauthQuotaLegacyChannel{id: id, credential: credential.String})
		}
	}
	// 事务独占连接，先关闭渠道游标再查询账本。
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read channels for OAuth quota ledger: %w", err)
	}
	return channels, nil
}

func oauthQuotaLegacyBaselines(ctx context.Context, tx *sql.Tx, dialect Dialect, channels []oauthQuotaLegacyChannel, now time.Time) ([]oauthQuotaBaseline, error) {
	var baselines []oauthQuotaBaseline
	for _, channel := range channels {
		if channel.credential != "" && !gjson.Valid(channel.credential) {
			log.Printf("[WARN] OAuth quota ledger migration skips channel %d baseline: invalid credential JSON", channel.id)
			continue
		}
		raw := gjson.Get(channel.credential, "quota_cost_usage")
		if !raw.IsObject() {
			continue
		}
		var usage oauthcost.Usage
		if err := json.Unmarshal([]byte(raw.Raw), &usage); err != nil {
			log.Printf("[WARN] OAuth quota ledger migration skips channel %d baseline: %v", channel.id, err)
			continue
		}
		if err := oauthcost.Validate(&usage); err != nil {
			log.Printf("[WARN] OAuth quota ledger migration skips channel %d baseline: %v", channel.id, err)
			continue
		}
		// WindowsAt 会按 window_seconds 推进；超出 time.Duration 范围会溢出。
		invalidDuration := false
		for _, window := range usage.Windows {
			if window.WindowSeconds > math.MaxInt64/int64(time.Second) {
				invalidDuration = true
				break
			}
		}
		if invalidDuration {
			log.Printf("[WARN] OAuth quota ledger migration skips channel %d baseline: invalid window duration", channel.id)
			continue
		}
		// 已过期窗口的旧成本属于上一周期，无需调用 WindowsAt 推进它。
		active := usage.Windows[:0]
		for _, window := range usage.Windows {
			if window.ResetAt > now.Unix() {
				active = append(active, window)
			}
		}
		usage.Windows = active
		// key 含 gjson 路径保留字符，逐个读取旧窗口原值。
		type legacyWindow struct{ resetAt, cost int64 }
		legacy := make(map[string]legacyWindow)
		raw.Get("windows").ForEach(func(_, window gjson.Result) bool {
			legacy[window.Get("key").String()] = legacyWindow{
				resetAt: window.Get("reset_at").Int(),
				cost:    window.Get("standard_cost_microusd").Int(),
			}
			return true
		})
		for _, window := range oauthcost.WindowsAt(&usage, now) {
			old, ok := legacy[window.Key]
			if !ok || old.cost <= 0 || old.resetAt != window.ResetAt {
				continue
			}
			counted := oauthcost.CountedRange(window)
			existing, err := loadOAuthQuotaLedgerTotalsTx(ctx, tx, dialect, channel.id, counted)
			if err != nil {
				return nil, err
			}
			if diff := old.cost - oauthcost.CostFromLedger(window, existing); diff > 0 {
				baselines = append(baselines, oauthQuotaBaseline{
					channelID: channel.id, bucketAt: counted.From, windowKey: window.Key, cost: diff,
				})
			}
		}
	}
	return baselines, nil
}

func loadOAuthQuotaLedgerTotalsTx(ctx context.Context, tx *sql.Tx, dialect Dialect, channelID int64, counted oauthcost.Range) ([]oauthcost.LedgerTotal, error) {
	rows, err := tx.QueryContext(ctx, rebindIfPostgres(dialect,
		"SELECT model, window_key, cost_microusd FROM oauth_quota_cost_ledger WHERE channel_id = ? AND bucket_at >= ? AND bucket_at < ?"),
		channelID, counted.From, counted.Until)
	if err != nil {
		return nil, fmt.Errorf("load OAuth quota ledger for channel %d: %w", channelID, err)
	}
	var totals []oauthcost.LedgerTotal
	for rows.Next() {
		var total oauthcost.LedgerTotal
		if err := rows.Scan(&total.Model, &total.WindowKey, &total.CostMicroUSD); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan OAuth quota ledger for channel %d: %w", channelID, err)
		}
		totals = append(totals, total)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read OAuth quota ledger for channel %d: %w", channelID, err)
	}
	return totals, nil
}

func insertOAuthQuotaBaselines(ctx context.Context, tx *sql.Tx, dialect Dialect, baselines []oauthQuotaBaseline) error {
	sort.Slice(baselines, func(i, j int) bool {
		a, b := baselines[i], baselines[j]
		if a.channelID != b.channelID {
			return a.channelID < b.channelID
		}
		if a.bucketAt != b.bucketAt {
			return a.bucketAt < b.bucketAt
		}
		return a.windowKey < b.windowKey
	})
	for start := 0; start < len(baselines); start += oauthQuotaLedgerBaselineChunkSize {
		chunk := baselines[start:min(start+oauthQuotaLedgerBaselineChunkSize, len(baselines))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*5)
		for i, baseline := range chunk {
			values[i] = "(?, ?, ?, ?, ?)"
			args = append(args, baseline.channelID, baseline.bucketAt, "", baseline.windowKey, baseline.cost)
		}
		query := "INSERT INTO oauth_quota_cost_ledger (channel_id, bucket_at, model, window_key, cost_microusd) VALUES " + strings.Join(values, ", ")
		if _, err := tx.ExecContext(ctx, rebindIfPostgres(dialect, query), args...); err != nil {
			return fmt.Errorf("insert OAuth quota ledger baselines: %w", err)
		}
	}
	return nil
}
