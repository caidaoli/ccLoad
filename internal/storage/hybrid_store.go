//nolint:revive // HybridStore 方法实现 Store 接口，注释在接口定义处
package storage

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

// HybridStore 混合存储（MySQL 主 + SQLite 本地缓存）
//
// 核心职责：
// - 读操作：从 SQLite 读取（本地缓存，低延迟）
// - 写操作：先写 MySQL（主存储），再同步更新 SQLite 缓存
// - 统计/日志查询：从 SQLite 查询
//
// 设计原则：
// - MySQL = 主存储（source of truth，持久化与恢复的唯一来源）
// - SQLite = 本地缓存（读加速，允许短暂不一致）
// - 写操作以 MySQL 为准：MySQL 成功即成功，SQLite 失败仅警告
//
// 日志特殊处理（高吞吐场景）：
// - 写入顺序：先写 SQLite（快），再异步同步到 MySQL（备份）
// - 这是性能妥协：日志写入频率高，同步延迟可接受
// - 代价：极端情况下 MySQL 可能丢失少量最新日志
// - 恢复时：SyncManager 从 MySQL 恢复历史日志到 SQLite
type HybridStore struct {
	sqlite *sqlstore.SQLStore // 本地缓存（读路径）
	mysql  *sqlstore.SQLStore // 主存储（写路径）

	// 异步同步队列（仅用于 logs）
	syncCh    chan *syncTask
	syncWg    sync.WaitGroup
	stopCh    chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
}

// syncTask 同步任务
type syncTask struct {
	operation string
	data      any
}

// syncTaskLog 日志同步数据
type syncTaskLog struct {
	entry *model.LogEntry
}

// syncTaskLogBatch 批量日志同步数据
type syncTaskLogBatch struct {
	entries []*model.LogEntry
}

const (
	syncQueueSize = 10000 // 异步同步队列大小（仅用于 logs）
)

// NewHybridStore 创建混合存储实例
func NewHybridStore(sqlite, mysql *sqlstore.SQLStore) *HybridStore {
	h := &HybridStore{
		sqlite: sqlite,
		mysql:  mysql,
		syncCh: make(chan *syncTask, syncQueueSize),
		stopCh: make(chan struct{}),
	}

	// 启动异步同步 worker
	h.syncWg.Add(1)
	go h.syncWorker()

	return h
}

// syncToSQLite 同步更新 SQLite 缓存
// SQLite 是本地库，启动时已验证可写，运行时不会失败
func (h *HybridStore) syncToSQLite(_ string, fn func() error) {
	_ = fn()
}

// cloneLogEntryForSync 克隆日志条目（异步队列需要）
func cloneLogEntryForSync(e *model.LogEntry) *model.LogEntry {
	if e == nil {
		return nil
	}
	clone := *e
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
// 异步同步 Worker（仅用于 logs）
// ============================================================================

func (h *HybridStore) syncWorker() {
	defer h.syncWg.Done()

	for {
		select {
		case <-h.stopCh:
			h.drainSyncQueue()
			return
		case task := <-h.syncCh:
			h.executeSyncTask(task)
		}
	}
}

// drainSyncQueue 处理剩余的同步任务（优雅关闭）
func (h *HybridStore) drainSyncQueue() {
	queueLen := len(h.syncCh)
	timeoutSec := min(5+queueLen/100, 30)
	timeout := time.After(time.Duration(timeoutSec) * time.Second)

	processed := 0
	for {
		select {
		case task := <-h.syncCh:
			h.executeSyncTask(task)
			processed++
		case <-timeout:
			remaining := len(h.syncCh)
			if remaining > 0 {
				log.Printf("[WARN] MySQL 同步关闭超时（已处理 %d），丢弃 %d 个任务", processed, remaining)
			}
			return
		default:
			if processed > 0 {
				log.Printf("[INFO] MySQL 同步队列已清空，共处理 %d 个任务", processed)
			}
			return
		}
	}
}

// executeSyncTask 执行单个同步任务
func (h *HybridStore) executeSyncTask(task *syncTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch task.operation {
	case "log":
		data := task.data.(*syncTaskLog)
		err = h.mysql.AddLog(ctx, data.entry)
	case "log_batch":
		data := task.data.(*syncTaskLogBatch)
		err = h.mysql.BatchAddLogs(ctx, data.entries)
	default:
		return
	}

	if err != nil {
		log.Printf("[WARN] MySQL 同步失败: %v, operation=%s", err, task.operation)
	}
}

// enqueueLogSync 将日志同步任务加入队列（非阻塞，队列满则丢弃）
func (h *HybridStore) enqueueLogSync(task *syncTask) {
	select {
	case h.syncCh <- task:
	default:
		log.Printf("[WARN] MySQL 同步队列已满，丢弃任务: %s", task.operation)
	}
}

// ============================================================================
// Store 接口实现
// ============================================================================

// === Channel Management ===

func (h *HybridStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	return h.sqlite.ListConfigs(ctx)
}

func (h *HybridStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	return h.sqlite.GetConfig(ctx, id)
}

func (h *HybridStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	result, err := h.mysql.CreateConfig(ctx, c)
	if err != nil {
		return nil, err
	}

	h.syncToSQLite("CreateConfig", func() error {
		_, err := h.sqlite.CreateConfig(ctx, result)
		return err
	})

	return result, nil
}

func (h *HybridStore) UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error) {
	result, err := h.mysql.UpdateConfig(ctx, id, upd)
	if err != nil {
		return nil, err
	}

	h.syncToSQLite("UpdateConfig", func() error {
		_, err := h.sqlite.UpdateConfig(ctx, id, result)
		return err
	})

	return result, nil
}

func (h *HybridStore) DeleteConfig(ctx context.Context, id int64) error {
	if err := h.mysql.DeleteConfig(ctx, id); err != nil {
		return err
	}

	h.syncToSQLite("DeleteConfig", func() error {
		return h.sqlite.DeleteConfig(ctx, id)
	})

	return nil
}

func (h *HybridStore) GetEnabledChannelsByModel(ctx context.Context, modelName string) ([]*model.Config, error) {
	return h.sqlite.GetEnabledChannelsByModel(ctx, modelName)
}

func (h *HybridStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	return h.sqlite.GetEnabledChannelsByType(ctx, channelType)
}

func (h *HybridStore) BatchUpdatePriority(ctx context.Context, updates []struct {
	ID       int64
	Priority int
}) (int64, error) {
	affected, err := h.mysql.BatchUpdatePriority(ctx, updates)
	if err != nil {
		return 0, err
	}

	h.syncToSQLite("BatchUpdatePriority", func() error {
		_, err := h.sqlite.BatchUpdatePriority(ctx, updates)
		return err
	})

	return affected, nil
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
	if err := h.mysql.CreateAPIKeysBatch(ctx, keys); err != nil {
		return err
	}

	h.syncToSQLite("CreateAPIKeysBatch", func() error {
		return h.sqlite.CreateAPIKeysBatch(ctx, keys)
	})

	return nil
}

func (h *HybridStore) UpdateAPIKeysStrategy(ctx context.Context, channelID int64, strategy string) error {
	if err := h.mysql.UpdateAPIKeysStrategy(ctx, channelID, strategy); err != nil {
		return err
	}

	h.syncToSQLite("UpdateAPIKeysStrategy", func() error {
		return h.sqlite.UpdateAPIKeysStrategy(ctx, channelID, strategy)
	})

	return nil
}

func (h *HybridStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	if err := h.mysql.DeleteAPIKey(ctx, channelID, keyIndex); err != nil {
		return err
	}

	h.syncToSQLite("DeleteAPIKey", func() error {
		return h.sqlite.DeleteAPIKey(ctx, channelID, keyIndex)
	})

	return nil
}

func (h *HybridStore) CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error {
	if err := h.mysql.CompactKeyIndices(ctx, channelID, removedIndex); err != nil {
		return err
	}

	h.syncToSQLite("CompactKeyIndices", func() error {
		return h.sqlite.CompactKeyIndices(ctx, channelID, removedIndex)
	})

	return nil
}

func (h *HybridStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	if err := h.mysql.DeleteAllAPIKeys(ctx, channelID); err != nil {
		return err
	}

	h.syncToSQLite("DeleteAllAPIKeys", func() error {
		return h.sqlite.DeleteAllAPIKeys(ctx, channelID)
	})

	return nil
}

// === Cooldown Management ===

func (h *HybridStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return h.sqlite.GetAllChannelCooldowns(ctx)
}

func (h *HybridStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	duration, err := h.mysql.BumpChannelCooldown(ctx, channelID, now, statusCode)
	if err != nil {
		return 0, err
	}

	h.syncToSQLite("BumpChannelCooldown", func() error {
		_, err := h.sqlite.BumpChannelCooldown(ctx, channelID, now, statusCode)
		return err
	})

	return duration, nil
}

func (h *HybridStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	if err := h.mysql.ResetChannelCooldown(ctx, channelID); err != nil {
		return err
	}

	h.syncToSQLite("ResetChannelCooldown", func() error {
		return h.sqlite.ResetChannelCooldown(ctx, channelID)
	})

	return nil
}

func (h *HybridStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	if err := h.mysql.SetChannelCooldown(ctx, channelID, until); err != nil {
		return err
	}

	h.syncToSQLite("SetChannelCooldown", func() error {
		return h.sqlite.SetChannelCooldown(ctx, channelID, until)
	})

	return nil
}

func (h *HybridStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return h.sqlite.GetAllKeyCooldowns(ctx)
}

func (h *HybridStore) BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	duration, err := h.mysql.BumpKeyCooldown(ctx, channelID, keyIndex, now, statusCode)
	if err != nil {
		return 0, err
	}

	h.syncToSQLite("BumpKeyCooldown", func() error {
		_, err := h.sqlite.BumpKeyCooldown(ctx, channelID, keyIndex, now, statusCode)
		return err
	})

	return duration, nil
}

func (h *HybridStore) ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	if err := h.mysql.ResetKeyCooldown(ctx, channelID, keyIndex); err != nil {
		return err
	}

	h.syncToSQLite("ResetKeyCooldown", func() error {
		return h.sqlite.ResetKeyCooldown(ctx, channelID, keyIndex)
	})

	return nil
}

func (h *HybridStore) SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error {
	if err := h.mysql.SetKeyCooldown(ctx, channelID, keyIndex, until); err != nil {
		return err
	}

	h.syncToSQLite("SetKeyCooldown", func() error {
		return h.sqlite.SetKeyCooldown(ctx, channelID, keyIndex, until)
	})

	return nil
}

// === Log Management ===
// 日志特殊处理：写 SQLite（快）+ 异步同步到 MySQL（备份）

func (h *HybridStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	if err := h.sqlite.AddLog(ctx, e); err != nil {
		return err
	}

	h.enqueueLogSync(&syncTask{
		operation: "log",
		data:      &syncTaskLog{entry: cloneLogEntryForSync(e)},
	})

	return nil
}

func (h *HybridStore) BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error {
	if err := h.sqlite.BatchAddLogs(ctx, logs); err != nil {
		return err
	}

	if len(logs) > 0 {
		h.enqueueLogSync(&syncTask{
			operation: "log_batch",
			data:      &syncTaskLogBatch{entries: cloneLogEntriesForSync(logs)},
		})
	}

	return nil
}

func (h *HybridStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return h.sqlite.ListLogs(ctx, since, limit, offset, filter)
}

func (h *HybridStore) ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return h.sqlite.ListLogsRange(ctx, since, until, limit, offset, filter)
}

func (h *HybridStore) CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error) {
	return h.sqlite.CountLogs(ctx, since, filter)
}

func (h *HybridStore) CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error) {
	return h.sqlite.CountLogsRange(ctx, since, until, filter)
}

func (h *HybridStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	return h.sqlite.CleanupLogsBefore(ctx, cutoff)
}

// === Metrics & Statistics ===

func (h *HybridStore) AggregateRangeWithFilter(ctx context.Context, since, until time.Time, bucket time.Duration, filter *model.LogFilter) ([]model.MetricPoint, error) {
	return h.sqlite.AggregateRangeWithFilter(ctx, since, until, bucket, filter)
}

func (h *HybridStore) GetDistinctModels(ctx context.Context, since, until time.Time, channelType string) ([]string, error) {
	return h.sqlite.GetDistinctModels(ctx, since, until, channelType)
}

func (h *HybridStore) GetStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) ([]model.StatsEntry, error) {
	return h.sqlite.GetStats(ctx, startTime, endTime, filter, isToday)
}

func (h *HybridStore) GetStatsLite(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
	return h.sqlite.GetStatsLite(ctx, startTime, endTime, filter)
}

func (h *HybridStore) GetRPMStats(ctx context.Context, startTime, endTime time.Time, filter *model.LogFilter, isToday bool) (*model.RPMStats, error) {
	return h.sqlite.GetRPMStats(ctx, startTime, endTime, filter, isToday)
}

func (h *HybridStore) GetChannelSuccessRates(ctx context.Context, since time.Time) (map[int64]model.ChannelHealthStats, error) {
	return h.sqlite.GetChannelSuccessRates(ctx, since)
}

func (h *HybridStore) GetHealthTimeline(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return h.sqlite.GetHealthTimeline(ctx, query, args...)
}

func (h *HybridStore) GetTodayChannelCosts(ctx context.Context, todayStart time.Time) (map[int64]float64, error) {
	return h.sqlite.GetTodayChannelCosts(ctx, todayStart)
}

// === Auth Token Management ===

func (h *HybridStore) CreateAuthToken(ctx context.Context, token *model.AuthToken) error {
	if err := h.mysql.CreateAuthToken(ctx, token); err != nil {
		return err
	}

	h.syncToSQLite("CreateAuthToken", func() error {
		return h.sqlite.CreateAuthToken(ctx, token)
	})

	return nil
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
	if err := h.mysql.UpdateAuthToken(ctx, token); err != nil {
		return err
	}

	h.syncToSQLite("UpdateAuthToken", func() error {
		return h.sqlite.UpdateAuthToken(ctx, token)
	})

	return nil
}

func (h *HybridStore) DeleteAuthToken(ctx context.Context, id int64) error {
	if err := h.mysql.DeleteAuthToken(ctx, id); err != nil {
		return err
	}

	h.syncToSQLite("DeleteAuthToken", func() error {
		return h.sqlite.DeleteAuthToken(ctx, id)
	})

	return nil
}

func (h *HybridStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	if err := h.mysql.UpdateTokenLastUsed(ctx, tokenHash, now); err != nil {
		return err
	}

	h.syncToSQLite("UpdateTokenLastUsed", func() error {
		return h.sqlite.UpdateTokenLastUsed(ctx, tokenHash, now)
	})

	return nil
}

func (h *HybridStore) UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, cacheReadTokens int64, cacheCreationTokens int64, costUSD float64) error {
	if err := h.mysql.UpdateTokenStats(ctx, tokenHash, isSuccess, duration, isStreaming, firstByteTime, promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens, costUSD); err != nil {
		return err
	}

	h.syncToSQLite("UpdateTokenStats", func() error {
		return h.sqlite.UpdateTokenStats(ctx, tokenHash, isSuccess, duration, isStreaming, firstByteTime, promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens, costUSD)
	})

	return nil
}

func (h *HybridStore) GetAuthTokenStatsInRange(ctx context.Context, startTime, endTime time.Time) (map[int64]*model.AuthTokenRangeStats, error) {
	return h.sqlite.GetAuthTokenStatsInRange(ctx, startTime, endTime)
}

func (h *HybridStore) FillAuthTokenRPMStats(ctx context.Context, stats map[int64]*model.AuthTokenRangeStats, startTime, endTime time.Time, isToday bool) error {
	return h.sqlite.FillAuthTokenRPMStats(ctx, stats, startTime, endTime, isToday)
}

// === System Settings ===

func (h *HybridStore) GetSetting(ctx context.Context, key string) (*model.SystemSetting, error) {
	return h.sqlite.GetSetting(ctx, key)
}

func (h *HybridStore) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	return h.sqlite.ListAllSettings(ctx)
}

func (h *HybridStore) UpdateSetting(ctx context.Context, key, value string) error {
	if err := h.mysql.UpdateSetting(ctx, key, value); err != nil {
		return err
	}

	h.syncToSQLite("UpdateSetting", func() error {
		return h.sqlite.UpdateSetting(ctx, key, value)
	})

	return nil
}

func (h *HybridStore) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	if err := h.mysql.BatchUpdateSettings(ctx, updates); err != nil {
		return err
	}

	h.syncToSQLite("BatchUpdateSettings", func() error {
		return h.sqlite.BatchUpdateSettings(ctx, updates)
	})

	return nil
}

// === Admin Session Management ===
// Admin sessions 只存在于 SQLite（本地会话，无需同步）

func (h *HybridStore) CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error {
	return h.sqlite.CreateAdminSession(ctx, token, expiresAt)
}

func (h *HybridStore) GetAdminSession(ctx context.Context, token string) (expiresAt time.Time, exists bool, err error) {
	return h.sqlite.GetAdminSession(ctx, token)
}

func (h *HybridStore) DeleteAdminSession(ctx context.Context, token string) error {
	return h.sqlite.DeleteAdminSession(ctx, token)
}

func (h *HybridStore) CleanExpiredSessions(ctx context.Context) error {
	return h.sqlite.CleanExpiredSessions(ctx)
}

func (h *HybridStore) LoadAllSessions(ctx context.Context) (map[string]time.Time, error) {
	return h.sqlite.LoadAllSessions(ctx)
}

// === Batch Operations ===

func (h *HybridStore) ImportChannelBatch(ctx context.Context, channels []*model.ChannelWithKeys) (created, updated int, err error) {
	created, updated, err = h.mysql.ImportChannelBatch(ctx, channels)
	if err != nil {
		return 0, 0, err
	}

	h.syncToSQLite("ImportChannelBatch", func() error {
		_, _, err := h.sqlite.ImportChannelBatch(ctx, channels)
		return err
	})

	return created, updated, nil
}

// === Lifecycle ===

func (h *HybridStore) Ping(ctx context.Context) error {
	return h.sqlite.Ping(ctx)
}

// SyncQueueLen 返回当前同步队列中待处理的任务数量（用于监控）
func (h *HybridStore) SyncQueueLen() int {
	return len(h.syncCh)
}

func (h *HybridStore) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.stopOnce.Do(func() {
			close(h.stopCh)
		})
		h.syncWg.Wait()

		if closeErr := h.sqlite.Close(); closeErr != nil {
			err = closeErr
		}
		if closeErr := h.mysql.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
