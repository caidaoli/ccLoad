package storage

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"ccLoad/internal/oauthcost"
	sqlstore "ccLoad/internal/storage/sql"
)

// primaryReconcileLedgerSlice 是全量对账复制账本的时间片（秒）。
const primaryReconcileLedgerSlice = int64(6 * 60 * 60)

// enqueueOAuthQuotaLedgerSlices 为每个分钟分片入队一次跨渠道复制。
func (h *HybridStore) enqueueOAuthQuotaLedgerSlices(sliceStarts []int64) {
	for _, from := range sliceStarts {
		h.primarySync.enqueue(fmt.Sprintf("oauth-quota-ledger/%d", from), "OAuth quota ledger", func(ctx context.Context) error {
			return h.copyOAuthQuotaLedgerRange(ctx, from, from+sqlstore.OAuthQuotaLedgerReplicationSlice)
		})
	}
}

// copyOAuthQuotaLedgerRange 让主库同一区间与 SQLite 权威切片一致；相同则不写主库。
func (h *HybridStore) copyOAuthQuotaLedgerRange(ctx context.Context, from, until int64) error {
	want, err := h.sqlite.ListOAuthQuotaLedgerRangeReplica(ctx, from, until)
	if err != nil {
		return err
	}
	got, err := h.primary.ListOAuthQuotaLedgerRangeReplica(ctx, from, until)
	if err != nil {
		return err
	}
	// SQL 的 ORDER BY 受列 collation 影响；比较前按 Go 字符串字节序统一主键顺序。
	less := func(a, b sqlstore.OAuthQuotaLedgerReplicaRow) int {
		if n := cmp.Compare(a.ChannelID, b.ChannelID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.BucketAt, b.BucketAt); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Model, b.Model); n != 0 {
			return n
		}
		return cmp.Compare(a.WindowKey, b.WindowKey)
	}
	slices.SortFunc(want, less)
	slices.SortFunc(got, less)
	if slices.Equal(want, got) {
		return nil
	}
	return h.primary.ReplaceOAuthQuotaLedgerRangeReplica(ctx, from, until, want)
}

// reconcileOAuthQuotaLedgerPage 每次复制六小时；末页覆盖到正无穷，避免未来桶漏复制。
func (h *HybridStore) reconcileOAuthQuotaLedgerPage(ctx context.Context, afterID int64) (int64, bool, error) {
	now := time.Now().Unix()
	from := afterID
	if from == 0 {
		from = now - int64(oauthcost.LedgerRetention/time.Second)
	}
	until := from + primaryReconcileLedgerSlice
	done := until > now
	if done {
		until = math.MaxInt64
	}
	if err := h.copyOAuthQuotaLedgerRange(ctx, from, until); err != nil {
		return afterID, false, fmt.Errorf("sync OAuth quota ledger [%d, %d): %w", from, until, err)
	}
	return until, done, nil
}
