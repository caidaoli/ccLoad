//nolint:revive // HybridStore 方法实现 Store 接口，注释在接口定义处
package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
	"ccLoad/internal/util"
)

// HybridStore 混合存储（SQLite 权威库 + 主库异步副本）
//
// 核心职责：
// - 权威读写：使用 SQLite，主库延迟不进入请求热路径
// - 后台复制：按实体合并最终状态，失败 10 秒后重试
// - 本地分析：日志与统计默认使用 SQLite；读取失败后可回退主库
// - 本地临时数据：Web session 和 DebugData 仅存 SQLite
//
// 设计原则：
// - SQLite = source of truth
// - 主库 = 最终一致副本；进程退出时允许丢失尚未同步的内存任务
// - 单实例单写者；不支持多个混合实例或外部进程同时修改主库
type HybridStore struct {
	sqlite  *sqlstore.SQLStore // 权威库、本地分析和临时数据
	primary *sqlstore.SQLStore // 主库（MySQL 或 PostgreSQL）

	closeOnce   sync.Once
	primarySync *primaryWriteBehind

	// OAuth credential writes are serialized so the SQLite projection cannot apply them out of order.
	oauthCredentialMu sync.Mutex

	sqliteReadFailCount atomic.Uint64
	analyticsPrimary    atomic.Bool
	primaryReconcileMu  sync.Mutex
	primaryReconcile    primaryReconcileCursor
}

// HybridRuntimeMetrics 是混合存储的进程内健康快照。
type HybridRuntimeMetrics struct {
	SQLiteReadFailures     uint64 `json:"sqlite_read_failures"`
	AnalyticsReadsPrimary  bool   `json:"analytics_reads_primary"`
	PrimarySyncPending     int    `json:"primary_sync_pending"`
	PrimarySyncFailures    uint64 `json:"primary_sync_failures"`
	PrimarySyncDropped     uint64 `json:"primary_sync_dropped"`
	PrimarySyncLastSuccess int64  `json:"primary_sync_last_success_unix_ms"`
}

// HybridRuntimeMetricsProvider 由混合存储实现，避免污染所有 Store 实现。
type HybridRuntimeMetricsProvider interface {
	RuntimeMetrics() HybridRuntimeMetrics
}

// NewHybridStore 创建混合存储实例
func NewHybridStore(sqlite, primary *sqlstore.SQLStore) *HybridStore {
	return newHybridStore(sqlite, primary, nil)
}

func newHybridStore(sqlite, primary *sqlstore.SQLStore, initialize func(context.Context) error) *HybridStore {
	h := &HybridStore{
		sqlite:  sqlite,
		primary: primary,
	}
	h.primarySync = newPrimaryWriteBehindWithInitializer(primarySyncRetryDelay, primarySyncTimeout, initialize)
	h.primarySync.configureReconcile(primarySyncMaxPending, h.reconcilePrimary, h.markPrimaryReconcileDirty)
	return h
}

func (h *HybridStore) markChannelDirty(channelID int64, deleted bool) {
	key := fmt.Sprintf("channel/%d", channelID)
	h.primarySync.enqueue(key, "channel", func(ctx context.Context) error {
		if deleted {
			return h.primary.DeleteConfig(ctx, channelID)
		}
		return h.syncChannelReplica(ctx, channelID)
	})
}

func (h *HybridStore) markAuthTokenDirty(id int64, tokenHash string, deleted bool) {
	key := "auth-token/" + tokenHash
	h.primarySync.enqueue(key, "auth token", func(ctx context.Context) error {
		if deleted {
			return h.primary.DeleteAuthTokenReplica(ctx, id)
		}
		token, err := h.sqlite.GetAuthTokenByValue(ctx, tokenHash)
		if err != nil {
			return err
		}
		return h.primary.UpsertAuthTokenAllFields(ctx, token)
	})
}

func (h *HybridStore) recordSQLiteReadFailure(op string, err error) {
	count := h.sqliteReadFailCount.Add(1)
	h.analyticsPrimary.Store(true)
	if count%10 == 1 {
		log.Printf("[WARN] SQLite 读取失败 (%s): %v (累计失败: %d)", op, err, count)
	}
}

func readAnalytics[T any](h *HybridStore, op string, fn func(*sqlstore.SQLStore) (T, error)) (T, error) {
	store := h.analyticsStore()
	result, err := fn(store)
	if err == nil || store == h.primary {
		return result, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	h.recordSQLiteReadFailure(op, err)
	return fn(h.primary)
}

func execAnalytics(h *HybridStore, op string, fn func(*sqlstore.SQLStore) error) error {
	store := h.analyticsStore()
	err := fn(store)
	if err == nil || store == h.primary {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	h.recordSQLiteReadFailure(op, err)
	return fn(h.primary)
}

// cloneLogEntryForSync 克隆写入主库的日志并剔除只允许本地保存的 DebugData。
func cloneLogEntryForSync(e *model.LogEntry) *model.LogEntry {
	if e == nil {
		return nil
	}
	clone := *e
	// 同步到主库时丢弃 DebugData：调试原始请求/响应体仅保留在 SQLite，
	// 避免膨胀主库；但 logs 表主数据仍需正常同步
	clone.DebugData = nil
	return &clone
}

// cloneLogEntriesForSync 批量克隆日志条目
func cloneLogEntriesForSync(entries []*model.LogEntry) []*model.LogEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]*model.LogEntry, len(entries))
	for i, e := range entries {
		out[i] = cloneLogEntryForSync(e)
	}
	return out
}

// ============================================================================
// Store 接口实现
// ============================================================================

// === Channel Management ===

func (h *HybridStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()
	return h.sqlite.ListConfigs(ctx)
}

func (h *HybridStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()
	return h.sqlite.GetConfig(ctx, id)
}

func (h *HybridStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	if c != nil && c.UsesOAuth() {
		h.oauthCredentialMu.Lock()
		defer h.oauthCredentialMu.Unlock()
	}
	result, err := h.sqlite.CreateConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	h.markChannelDirty(result.ID, false)
	return result, nil
}

func (h *HybridStore) UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error) {
	result, err := h.sqlite.UpdateConfig(ctx, id, upd)
	if err != nil {
		return nil, err
	}

	h.markChannelDirty(id, false)
	return result, nil
}

func (h *HybridStore) CompareAndSwapOAuthCredential(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	updated, err := h.sqlite.CompareAndSwapOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, nextCredential)
	if err != nil {
		return updated, err
	}
	if !updated {
		return false, nil
	}
	h.markChannelDirty(channelID, false)
	return true, nil
}

func (h *HybridStore) ResetOAuthQuotaCostUsage(ctx context.Context, channelID int64, resetAt time.Time) error {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()
	if err := h.sqlite.ResetOAuthQuotaCostUsage(ctx, channelID, resetAt); err != nil {
		return err
	}
	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) DisableOAuthChannelIfCredentialMatches(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential string,
) (bool, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	disabled, err := h.sqlite.DisableOAuthChannelIfCredentialMatches(
		ctx, channelID, expectedAuthType, expectedCredential,
	)
	if err != nil || !disabled {
		return disabled, err
	}
	h.markChannelDirty(channelID, false)
	return true, nil
}

func (h *HybridStore) UpdateOAuthModelStateIfCredentialMatches(
	ctx context.Context,
	channelID int64,
	expectedAuthType, expectedCredential string,
	modelEntries []model.ModelEntry,
	scheduledCheckModel string,
) (bool, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	updated, err := h.sqlite.UpdateOAuthModelStateIfCredentialMatches(
		ctx, channelID, expectedAuthType, expectedCredential, modelEntries, scheduledCheckModel,
	)
	if err != nil {
		return updated, err
	}
	if !updated {
		return false, nil
	}
	h.markChannelDirty(channelID, false)
	return true, nil
}

func (h *HybridStore) UpdateChannelEnabled(ctx context.Context, id int64, enabled bool) (*model.Config, error) {
	result, err := h.sqlite.UpdateChannelEnabled(ctx, id, enabled)
	if err != nil {
		return nil, err
	}

	h.markChannelDirty(id, false)
	return result, nil
}

func (h *HybridStore) BatchPatchConfigs(ctx context.Context, channelIDs []int64, patch model.BatchConfigPatch) (model.BatchConfigPatchResult, error) {
	if patch.ModelImportMode != "" {
		h.oauthCredentialMu.Lock()
		defer h.oauthCredentialMu.Unlock()
	}
	result, err := h.sqlite.BatchPatchConfigs(ctx, channelIDs, patch)
	if err != nil {
		return model.BatchConfigPatchResult{}, err
	}

	for _, id := range channelIDs {
		h.markChannelDirty(id, false)
	}
	return result, nil
}

func (h *HybridStore) DeleteConfig(ctx context.Context, id int64) error {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	if err := h.sqlite.DeleteConfig(ctx, id); err != nil {
		return err
	}
	h.markChannelDirty(id, true)
	return nil
}

func (h *HybridStore) DeleteConfigIfOAuthSnapshotMatches(
	ctx context.Context,
	expected *model.Config,
) (bool, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	deleted, err := h.sqlite.DeleteConfigIfOAuthSnapshotMatches(ctx, expected)
	if err != nil || !deleted {
		return deleted, err
	}
	h.markChannelDirty(expected.ID, true)
	return true, nil
}

func (h *HybridStore) DisableConfigIfOAuthSnapshotMatches(
	ctx context.Context,
	expected *model.Config,
) (bool, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()

	disabled, err := h.sqlite.DisableConfigIfOAuthSnapshotMatches(ctx, expected)
	if err != nil || !disabled {
		return disabled, err
	}
	h.markChannelDirty(expected.ID, false)
	return true, nil
}

func (h *HybridStore) GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	h.oauthCredentialMu.Lock()
	defer h.oauthCredentialMu.Unlock()
	return h.sqlite.GetEnabledChannelsByModel(ctx, modelName)
}

func (h *HybridStore) BatchUpdatePriority(ctx context.Context, updates []struct {
	ID       int64
	Priority int
}) (int64, error) {
	affected, err := h.sqlite.BatchUpdatePriority(ctx, updates)
	if err != nil {
		return 0, err
	}

	for _, update := range updates {
		h.markChannelDirty(update.ID, false)
	}
	return affected, nil
}

// === Channel URL Runtime State ===

func (h *HybridStore) LoadDisabledURLs(ctx context.Context) (map[int64][]string, error) {
	return h.sqlite.LoadDisabledURLs(ctx)
}

func (h *HybridStore) SetURLDisabled(ctx context.Context, channelID int64, url string, disabled bool) error {
	if err := h.sqlite.SetURLDisabled(ctx, channelID, url, disabled); err != nil {
		return err
	}
	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) SetAPIKeyDisabled(ctx context.Context, channelID int64, keyIndex int, disabled bool) error {
	if err := h.sqlite.SetAPIKeyDisabled(ctx, channelID, keyIndex, disabled); err != nil {
		return err
	}
	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) CleanupOrphanedURLStates(ctx context.Context, channelID int64, keepURLs []string) error {
	if err := h.sqlite.CleanupOrphanedURLStates(ctx, channelID, keepURLs); err != nil {
		return err
	}
	h.markChannelDirty(channelID, false)
	return nil
}

// === API Key Management ===

func (h *HybridStore) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	return h.sqlite.GetAPIKeys(ctx, channelID)
}

func (h *HybridStore) GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error) {
	return h.sqlite.GetAPIKey(ctx, channelID, keyIndex)
}

func (h *HybridStore) GetAllAPIKeys(ctx context.Context) (map[int64][]*model.APIKey, error) {
	return h.sqlite.GetAllAPIKeys(ctx)
}

func (h *HybridStore) CreateAPIKeysBatch(ctx context.Context, keys []*model.APIKey) error {
	if err := h.sqlite.CreateAPIKeysBatch(ctx, keys); err != nil {
		return err
	}
	for _, key := range keys {
		if key != nil {
			h.markChannelDirty(key.ChannelID, false)
		}
	}
	return nil
}

func (h *HybridStore) UpdateAPIKeysStrategy(ctx context.Context, channelID int64, strategy string) error {
	if err := h.sqlite.UpdateAPIKeysStrategy(ctx, channelID, strategy); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) UpdateAPIKeyNotes(ctx context.Context, channelID int64, notesByIndex map[int]string) error {
	if err := h.sqlite.UpdateAPIKeyNotes(ctx, channelID, notesByIndex); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) UpdateAPIKeyModelScopes(
	ctx context.Context,
	channelID int64,
	scopesByIndex map[int]model.APIKeyModelScope,
) error {
	if err := h.sqlite.UpdateAPIKeyModelScopes(ctx, channelID, scopesByIndex); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	if err := h.sqlite.DeleteAPIKey(ctx, channelID, keyIndex); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error {
	if err := h.sqlite.CompactKeyIndices(ctx, channelID, removedIndex); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	if err := h.sqlite.DeleteAllAPIKeys(ctx, channelID); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

// === Cooldown Management ===

func (h *HybridStore) ConfigureCooldown(settings util.CooldownSettings) {
	h.primary.ConfigureCooldown(settings)
	h.sqlite.ConfigureCooldown(settings)
}

func (h *HybridStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return h.sqlite.GetAllChannelCooldowns(ctx)
}

func (h *HybridStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	duration, err := h.sqlite.BumpChannelCooldown(ctx, channelID, now, statusCode)
	if err != nil {
		return 0, err
	}

	h.markChannelDirty(channelID, false)
	return duration, nil
}

func (h *HybridStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	if err := h.sqlite.ResetChannelCooldown(ctx, channelID); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) ResetAllCooldowns(ctx context.Context, channelID int64) error {
	if err := h.sqlite.ResetAllCooldowns(ctx, channelID); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	if err := h.sqlite.SetChannelCooldown(ctx, channelID, until); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return h.sqlite.GetAllKeyCooldowns(ctx)
}

func (h *HybridStore) BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	duration, err := h.sqlite.BumpKeyCooldown(ctx, channelID, keyIndex, now, statusCode)
	if err != nil {
		return 0, err
	}

	h.markChannelDirty(channelID, false)

	return duration, nil
}

func (h *HybridStore) ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	if err := h.sqlite.ResetKeyCooldown(ctx, channelID, keyIndex); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error {
	if err := h.sqlite.SetKeyCooldown(ctx, channelID, keyIndex, until); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) GetAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	return h.sqlite.GetAllModelCooldowns(ctx)
}

func (h *HybridStore) BumpModelCooldown(
	ctx context.Context,
	channelID int64,
	model string,
	now time.Time,
	statusCode int,
) (time.Duration, error) {
	duration, err := h.sqlite.BumpModelCooldown(ctx, channelID, model, now, statusCode)
	if err != nil {
		return 0, err
	}

	h.markChannelDirty(channelID, false)
	return duration, nil
}

func (h *HybridStore) SetModelCooldown(ctx context.Context, channelID int64, model string, until time.Time) error {
	if err := h.sqlite.SetModelCooldown(ctx, channelID, model, until); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

func (h *HybridStore) ResetModelCooldown(ctx context.Context, channelID int64, model string) error {
	if err := h.sqlite.ResetModelCooldown(ctx, channelID, model); err != nil {
		return err
	}

	h.markChannelDirty(channelID, false)
	return nil
}

// === Log Management ===
// SQLite 写入成功即完成；主库只保留最后一个待同步日志批次。日志允许少量丢失。

func (h *HybridStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	if e == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = model.JSONTime{Time: time.Now()}
	}
	h.oauthCredentialMu.Lock()
	updatedChannelIDs, err := h.sqlite.AddLogWithOAuthQuotaCost(ctx, e)
	if err != nil {
		h.oauthCredentialMu.Unlock()
		return err
	}
	for _, channelID := range updatedChannelIDs {
		h.markChannelDirty(channelID, false)
	}
	h.oauthCredentialMu.Unlock()
	entry := cloneLogEntryForSync(e)
	h.primarySync.enqueueBestEffort("logs/latest", "logs", func(syncCtx context.Context) error {
		return h.primary.AddLogReplica(syncCtx, entry)
	})
	return nil
}

func (h *HybridStore) BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error {
	now := time.Now()
	for _, entry := range logs {
		if entry != nil && entry.Time.IsZero() {
			entry.Time = model.JSONTime{Time: now}
		}
	}
	h.oauthCredentialMu.Lock()
	updatedChannelIDs, err := h.sqlite.BatchAddLogsWithOAuthQuotaCost(ctx, logs)
	if err != nil {
		h.oauthCredentialMu.Unlock()
		return err
	}
	for _, channelID := range updatedChannelIDs {
		h.markChannelDirty(channelID, false)
	}
	h.oauthCredentialMu.Unlock()
	entries := cloneLogEntriesForSync(logs)
	h.primarySync.enqueueBestEffort("logs/latest", "logs", func(syncCtx context.Context) error {
		return h.primary.BatchAddLogsReplica(syncCtx, entries)
	})
	return nil
}

func (h *HybridStore) analyticsStore() *sqlstore.SQLStore {
	if h.analyticsPrimary.Load() {
		return h.primary
	}
	return h.sqlite
}

func (h *HybridStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return readAnalytics(h, "ListLogs", func(store *sqlstore.SQLStore) ([]*model.LogEntry, error) {
		return store.ListLogs(ctx, since, limit, offset, filter)
	})
}

func (h *HybridStore) ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return readAnalytics(h, "ListLogsRange", func(store *sqlstore.SQLStore) ([]*model.LogEntry, error) {
		return store.ListLogsRange(ctx, since, until, limit, offset, filter)
	})
}

func (h *HybridStore) ListLogsRangeWithCount(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, int, error) {
	type result struct {
		entries []*model.LogEntry
		count   int
	}
	page, err := readAnalytics(h, "ListLogsRangeWithCount", func(store *sqlstore.SQLStore) (result, error) {
		entries, count, queryErr := store.ListLogsRangeWithCount(ctx, since, until, limit, offset, filter)
		return result{entries: entries, count: count}, queryErr
	})
	return page.entries, page.count, err
}

func (h *HybridStore) CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error) {
	return readAnalytics(h, "CountLogs", func(store *sqlstore.SQLStore) (int, error) {
		return store.CountLogs(ctx, since, filter)
	})
}

func (h *HybridStore) CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error) {
	return readAnalytics(h, "CountLogsRange", func(store *sqlstore.SQLStore) (int, error) {
		return store.CountLogsRange(ctx, since, until, filter)
	})
}

func (h *HybridStore) GetTodayChannelURLStats(ctx context.Context, dayStart time.Time) ([]model.ChannelURLLogStat, error) {
	return readAnalytics(h, "GetTodayChannelURLStats", func(store *sqlstore.SQLStore) ([]model.ChannelURLLogStat, error) {
		return store.GetTodayChannelURLStats(ctx, dayStart)
	})
}

func (h *HybridStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	if err := h.sqlite.CleanupLogsBefore(ctx, cutoff); err != nil {
		return err
	}
	h.primarySync.enqueueBestEffort("logs/cleanup", "log cleanup", func(syncCtx context.Context) error {
		return h.primary.CleanupLogsBefore(syncCtx, cutoff)
	})
	return nil
}

// === Metrics & Statistics ===

func (h *HybridStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, filter *model.LogFilter) ([]model.MetricPoint, error) {
	return readAnalytics(h, "AggregateRangeWithFilter", func(store *sqlstore.SQLStore) ([]model.MetricPoint, error) {
		return store.AggregateRangeWithFilter(ctx, since, until, bucket, filter)
	})
}

func (h *HybridStore) GetDistinctModels(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]string, error) {
	return readAnalytics(h, "GetDistinctModels", func(store *sqlstore.SQLStore) ([]string, error) {
		return store.GetDistinctModels(ctx, since, until, filter)
	})
}

func (h *HybridStore) GetDistinctStatusCodes(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]int, error) {
	return readAnalytics(h, "GetDistinctStatusCodes", func(store *sqlstore.SQLStore) ([]int, error) {
		return store.GetDistinctStatusCodes(ctx, since, until, filter)
	})
}

func (h *HybridStore) GetDistinctChannels(ctx context.Context, since, until time.Time, filter *model.LogFilter) ([]model.ChannelNameID, error) {
	return readAnalytics(h, "GetDistinctChannels", func(store *sqlstore.SQLStore) ([]model.ChannelNameID, error) {
		return store.GetDistinctChannels(ctx, since, until, filter)
	})
}

func (h *HybridStore) GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) ([]model.StatsEntry, error) {
	return readAnalytics(h, "GetStats", func(store *sqlstore.SQLStore) ([]model.StatsEntry, error) {
		return store.GetStats(ctx, startTime, endTime, filter, isToday)
	})
}

func (h *HybridStore) GetStatsLite(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
	return readAnalytics(h, "GetStatsLite", func(store *sqlstore.SQLStore) ([]model.StatsEntry, error) {
		return store.GetStatsLite(ctx, startTime, endTime, filter)
	})
}

func (h *HybridStore) GetClientProtocolStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.ClientProtocolStats, error) {
	return readAnalytics(h, "GetClientProtocolStats", func(store *sqlstore.SQLStore) ([]model.ClientProtocolStats, error) {
		return store.GetClientProtocolStats(ctx, startTime, endTime, filter)
	})
}

func (h *HybridStore) GetAuthTypeStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.AuthTypeStats, error) {
	return readAnalytics(h, "GetAuthTypeStats", func(store *sqlstore.SQLStore) ([]model.AuthTypeStats, error) {
		return store.GetAuthTypeStats(ctx, startTime, endTime, filter)
	})
}

func (h *HybridStore) GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error) {
	return readAnalytics(h, "GetRPMStats", func(store *sqlstore.SQLStore) (*model.RPMStats, error) {
		return store.GetRPMStats(ctx, startTime, endTime, filter, isToday)
	})
}

func (h *HybridStore) GetChannelSuccessRates(ctx context.Context, since time.Time) (map[int64]model.ChannelHealthStats, error) {
	return readAnalytics(h, "GetChannelSuccessRates", func(store *sqlstore.SQLStore) (map[int64]model.ChannelHealthStats, error) {
		return store.GetChannelSuccessRates(ctx, since)
	})
}

func (h *HybridStore) GetHealthTimeline(ctx context.Context, params model.HealthTimelineParams) ([]model.HealthTimelineRow, error) {
	return readAnalytics(h, "GetHealthTimeline", func(store *sqlstore.SQLStore) ([]model.HealthTimelineRow, error) {
		return store.GetHealthTimeline(ctx, params)
	})
}

func (h *HybridStore) GetTodayChannelCosts(ctx context.Context, todayStart time.Time) (map[int64]float64, error) {
	return readAnalytics(h, "GetTodayChannelCosts", func(store *sqlstore.SQLStore) (map[int64]float64, error) {
		return store.GetTodayChannelCosts(ctx, todayStart)
	})
}

// === Auth Token Management ===

func (h *HybridStore) CreateAuthToken(ctx context.Context, token *model.AuthToken) error {
	if err := h.sqlite.CreateAuthToken(ctx, token); err != nil {
		return err
	}
	current, err := h.sqlite.GetAuthToken(ctx, token.ID)
	if err != nil {
		return err
	}
	h.markAuthTokenDirty(current.ID, current.Token, false)
	return nil
}

// EnsureAuthToken creates a missing auth token in SQLite and schedules its final state.
func (h *HybridStore) EnsureAuthToken(ctx context.Context, token *model.AuthToken) (bool, error) {
	created, err := h.sqlite.EnsureAuthToken(ctx, token)
	if err != nil {
		return false, err
	}

	h.markAuthTokenDirty(token.ID, token.Token, false)
	return created, nil
}

func (h *HybridStore) GetAuthToken(ctx context.Context, id int64) (*model.AuthToken, error) {
	return h.sqlite.GetAuthToken(ctx, id)
}

func (h *HybridStore) GetAuthTokenByValue(ctx context.Context, tokenHash string) (*model.AuthToken, error) {
	return h.sqlite.GetAuthTokenByValue(ctx, tokenHash)
}

func (h *HybridStore) ListAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	return h.sqlite.ListAuthTokens(ctx)
}

func (h *HybridStore) ListActiveAuthTokens(ctx context.Context) ([]*model.AuthToken, error) {
	return h.sqlite.ListActiveAuthTokens(ctx)
}

func (h *HybridStore) UpdateAuthToken(ctx context.Context, token *model.AuthToken) error {
	if err := h.sqlite.UpdateAuthToken(ctx, token); err != nil {
		return err
	}

	current, err := h.sqlite.GetAuthToken(ctx, token.ID)
	if err != nil {
		return err
	}
	h.markAuthTokenDirty(current.ID, current.Token, false)
	return nil
}

func (h *HybridStore) DeleteAuthToken(ctx context.Context, id int64) error {
	token, err := h.sqlite.GetAuthToken(ctx, id)
	if err != nil {
		return err
	}
	if err := h.sqlite.DeleteAuthToken(ctx, id); err != nil {
		return err
	}
	h.markAuthTokenDirty(id, token.Token, true)
	return nil
}

func (h *HybridStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	if err := h.sqlite.UpdateTokenLastUsed(ctx, tokenHash, now); err != nil {
		return err
	}
	token, err := h.sqlite.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		return err
	}
	h.markAuthTokenDirty(token.ID, tokenHash, false)
	return nil
}

func (h *HybridStore) UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, cacheReadTokens int64, cacheCreationTokens int64, costUSD float64, effectiveCostUSD float64, completedAt time.Time) error {
	if err := h.sqlite.UpdateTokenStats(ctx, tokenHash, isSuccess, duration, isStreaming, firstByteTime, promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens, costUSD, effectiveCostUSD, completedAt); err != nil {
		return err
	}
	token, err := h.sqlite.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		return err
	}
	h.markAuthTokenDirty(token.ID, tokenHash, false)
	return nil
}

func (h *HybridStore) GetAuthTokenStatsInRange(ctx context.Context, startTime, endTime time.Time) (map[int64]*model.AuthTokenRangeStats, error) {
	return readAnalytics(h, "GetAuthTokenStatsInRange", func(store *sqlstore.SQLStore) (map[int64]*model.AuthTokenRangeStats, error) {
		return store.GetAuthTokenStatsInRange(ctx, startTime, endTime)
	})
}

func (h *HybridStore) FillAuthTokenRPMStats(ctx context.Context, stats map[int64]*model.AuthTokenRangeStats, startTime, endTime time.Time, isToday bool) error {
	return execAnalytics(h, "FillAuthTokenRPMStats", func(store *sqlstore.SQLStore) error {
		return store.FillAuthTokenRPMStats(ctx, stats, startTime, endTime, isToday)
	})
}

// === System Settings ===

func (h *HybridStore) GetSetting(ctx context.Context, key string) (*model.SystemSetting, error) {
	return h.sqlite.GetSetting(ctx, key)
}

func (h *HybridStore) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	return h.sqlite.ListAllSettings(ctx)
}

func (h *HybridStore) UpdateSetting(ctx context.Context, key, value string) error {
	if err := h.sqlite.UpdateSetting(ctx, key, value); err != nil {
		return err
	}
	h.primarySync.enqueue("setting/"+key, "setting", func(syncCtx context.Context) error {
		return h.primary.UpdateSetting(syncCtx, key, value)
	})

	return nil
}

func (h *HybridStore) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	if err := h.sqlite.BatchUpdateSettings(ctx, updates); err != nil {
		return err
	}
	for key, value := range updates {
		settingKey, settingValue := key, value
		h.primarySync.enqueue("setting/"+settingKey, "setting", func(syncCtx context.Context) error {
			return h.primary.UpdateSetting(syncCtx, settingKey, settingValue)
		})
	}
	return nil
}

func (h *HybridStore) CreateWebSession(ctx context.Context, token string, session model.WebSession) error {
	return h.sqlite.CreateWebSession(ctx, token, session)
}

func (h *HybridStore) GetWebSession(ctx context.Context, token string) (model.WebSession, bool, error) {
	return h.sqlite.GetWebSession(ctx, token)
}

func (h *HybridStore) DeleteWebSession(ctx context.Context, token string) error {
	return h.sqlite.DeleteWebSession(ctx, token)
}

func (h *HybridStore) DeleteWebSessionsByAuthTokenID(ctx context.Context, authTokenID int64) error {
	return h.sqlite.DeleteWebSessionsByAuthTokenID(ctx, authTokenID)
}

func (h *HybridStore) CleanExpiredWebSessions(ctx context.Context) error {
	return h.sqlite.CleanExpiredWebSessions(ctx)
}

func (h *HybridStore) LoadWebSessions(ctx context.Context) (map[string]model.WebSession, error) {
	return h.sqlite.LoadWebSessions(ctx)
}

// === Batch Operations ===

func (h *HybridStore) ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error) {
	hasOAuth := false
	for _, channel := range channels {
		if channel != nil && channel.Config != nil && channel.Config.UsesOAuth() {
			hasOAuth = true
			break
		}
	}
	if hasOAuth {
		h.oauthCredentialMu.Lock()
		defer h.oauthCredentialMu.Unlock()
	}
	created, updated, err = h.sqlite.ImportChannelBatch(ctx, channels)
	if err != nil {
		return 0, 0, err
	}
	for _, channel := range channels {
		if channel != nil && channel.Config != nil {
			h.markChannelDirty(channel.Config.ID, false)
		}
	}

	return created, updated, nil
}

// === Lifecycle ===

func (h *HybridStore) Ping(ctx context.Context) error {
	return h.sqlite.Ping(ctx)
}

func (h *HybridStore) RuntimeMetrics() HybridRuntimeMetrics {
	return HybridRuntimeMetrics{
		SQLiteReadFailures:     h.sqliteReadFailCount.Load(),
		AnalyticsReadsPrimary:  h.analyticsPrimary.Load(),
		PrimarySyncPending:     h.primarySync.pending(),
		PrimarySyncFailures:    h.primarySync.failures.Load(),
		PrimarySyncDropped:     h.primarySync.dropped.Load(),
		PrimarySyncLastSuccess: h.primarySync.success.Load(),
	}
}

// === Debug Log Management (SQLite only, no primary sync) ===

func (h *HybridStore) AddDebugLog(ctx context.Context, e *model.DebugLogEntry) error {
	return h.sqlite.AddDebugLog(ctx, e)
}

func (h *HybridStore) GetDebugLogByLogID(ctx context.Context, logID int64) (*model.DebugLogEntry, error) {
	// 主库日志 ID 与本地 SQLite 日志 ID 不是同一个命名空间。
	// 分析读已切主库时按数值 ID 查询本地 DebugData 可能泄露另一条请求，必须拒绝。
	if h.analyticsPrimary.Load() {
		return nil, nil
	}
	return h.sqlite.GetDebugLogByLogID(ctx, logID)
}

func (h *HybridStore) CleanupDebugLogsBatch(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return h.sqlite.CleanupDebugLogsBatch(ctx, cutoff, limit)
}

func (h *HybridStore) TruncateDebugLogs(ctx context.Context) error {
	return h.sqlite.TruncateDebugLogs(ctx)
}

func (h *HybridStore) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.primarySync.close()
		if closeErr := h.sqlite.Close(); closeErr != nil {
			err = closeErr
		}
		if closeErr := h.primary.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
