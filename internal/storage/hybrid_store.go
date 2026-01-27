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

// HybridStore 混合存储（SQLite 主 + MySQL 异步备份）
//
// 核心职责：
// - 读操作：仅从 SQLite 读取（MySQL 不参与查询）
// - 写操作：写入 SQLite + 异步同步到 MySQL（备份）
// - 统计查询：仅从 SQLite 查询
// - 日志查询：仅从 SQLite 查询
//
// 设计原则：
// - SQLite = 主存储（所有读写操作）
// - MySQL = 备份存储（仅接收写入同步，**不参与查询**）
// - 异步同步：使用 channel 队列，不阻塞主业务
// - 透明降级：MySQL 同步失败仅记录警告，不影响业务
type HybridStore struct {
	sqlite *sqlstore.SQLStore // 主存储（所有读写）
	mysql  *sqlstore.SQLStore // 备份存储（仅接收写入同步）

	// 异步同步队列
	syncCh    chan *syncTask
	syncWg    sync.WaitGroup
	stopCh    chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
}

// syncTask 同步任务
type syncTask struct {
	operation string // "log", "config_create", "config_update", "config_delete", ...
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

// syncTaskConfig 配置同步数据
type syncTaskConfig struct {
	id     int64
	config *model.Config
}

// syncTaskAPIKeys API Keys 同步数据
type syncTaskAPIKeys struct {
	channelID int64
	keys      []*model.APIKey
	strategy  string
	keyIndex  int
}

// syncTaskAuthToken Auth Token 同步数据
type syncTaskAuthToken struct {
	id    int64
	token *model.AuthToken
}

// syncTaskSetting 系统设置同步数据
type syncTaskSetting struct {
	key     string
	value   string
	updates map[string]string
}

// syncTaskImport 批量导入同步数据
type syncTaskImport struct {
	channels []*model.ChannelWithKeys
}

const (
	syncQueueSize = 10000 // 异步同步队列大小
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

// ============================================================================
// 异步同步 Worker
// ============================================================================

func (h *HybridStore) syncWorker() {
	defer h.syncWg.Done()

	for {
		select {
		case <-h.stopCh:
			// 收到停止信号，尝试处理剩余任务
			h.drainSyncQueue()
			return
		case task := <-h.syncCh:
			h.executeSyncTask(task)
		}
	}
}

// drainSyncQueue 处理剩余的同步任务（优雅关闭）
func (h *HybridStore) drainSyncQueue() {
	// 动态超时：基础 5 秒 + 每 100 个任务额外 1 秒，上限 30 秒
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
			return // 队列为空
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

	case "config_create":
		data := task.data.(*syncTaskConfig)
		_, err = h.mysql.CreateConfig(ctx, data.config)

	case "config_update":
		data := task.data.(*syncTaskConfig)
		_, err = h.mysql.UpdateConfig(ctx, data.id, data.config)

	case "config_delete":
		data := task.data.(*syncTaskConfig)
		err = h.mysql.DeleteConfig(ctx, data.id)

	case "apikeys_create":
		data := task.data.(*syncTaskAPIKeys)
		err = h.mysql.CreateAPIKeysBatch(ctx, data.keys)

	case "apikeys_strategy":
		data := task.data.(*syncTaskAPIKeys)
		err = h.mysql.UpdateAPIKeysStrategy(ctx, data.channelID, data.strategy)

	case "apikey_delete":
		data := task.data.(*syncTaskAPIKeys)
		err = h.mysql.DeleteAPIKey(ctx, data.channelID, data.keyIndex)

	case "apikeys_delete_all":
		data := task.data.(*syncTaskAPIKeys)
		err = h.mysql.DeleteAllAPIKeys(ctx, data.channelID)

	case "apikeys_compact":
		data := task.data.(*syncTaskAPIKeys)
		err = h.mysql.CompactKeyIndices(ctx, data.channelID, data.keyIndex)

	case "authtoken_create":
		data := task.data.(*syncTaskAuthToken)
		err = h.mysql.CreateAuthToken(ctx, data.token)

	case "authtoken_update":
		data := task.data.(*syncTaskAuthToken)
		err = h.mysql.UpdateAuthToken(ctx, data.token)

	case "authtoken_delete":
		data := task.data.(*syncTaskAuthToken)
		err = h.mysql.DeleteAuthToken(ctx, data.id)

	case "setting_update":
		data := task.data.(*syncTaskSetting)
		err = h.mysql.UpdateSetting(ctx, data.key, data.value)

	case "settings_batch":
		data := task.data.(*syncTaskSetting)
		err = h.mysql.BatchUpdateSettings(ctx, data.updates)

	case "import_batch":
		data := task.data.(*syncTaskImport)
		_, _, err = h.mysql.ImportChannelBatch(ctx, data.channels)
	}

	if err != nil {
		log.Printf("[WARN] MySQL 同步失败: %v, operation=%s", err, task.operation)
	}
}

// enqueueSyncTask 将任务加入同步队列（非阻塞）
func (h *HybridStore) enqueueSyncTask(task *syncTask) {
	select {
	case h.syncCh <- task:
		// 成功入队
	default:
		// 队列已满，丢弃任务（记录警告）
		log.Printf("[WARN] MySQL 同步队列已满，丢弃任务: %s", task.operation)
	}
}

// ============================================================================
// Store 接口实现 - 所有读操作都走 SQLite
// 以下方法实现 storage.Store 接口，方法签名和行为见接口定义
// ============================================================================

// === Channel Management ===

func (h *HybridStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	return h.sqlite.ListConfigs(ctx)
}

func (h *HybridStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	return h.sqlite.GetConfig(ctx, id)
}

func (h *HybridStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	result, err := h.sqlite.CreateConfig(ctx, c)
	if err != nil {
		return nil, err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "config_create",
		data:      &syncTaskConfig{config: result},
	})

	return result, nil
}

func (h *HybridStore) UpdateConfig(ctx context.Context, id int64, upd *model.Config) (*model.Config, error) {
	result, err := h.sqlite.UpdateConfig(ctx, id, upd)
	if err != nil {
		return nil, err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "config_update",
		data:      &syncTaskConfig{id: id, config: result},
	})

	return result, nil
}

func (h *HybridStore) DeleteConfig(ctx context.Context, id int64) error {
	if err := h.sqlite.DeleteConfig(ctx, id); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "config_delete",
		data:      &syncTaskConfig{id: id},
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
	return h.sqlite.BatchUpdatePriority(ctx, updates)
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

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "apikeys_create",
		data:      &syncTaskAPIKeys{keys: keys},
	})

	return nil
}

func (h *HybridStore) UpdateAPIKeysStrategy(ctx context.Context, channelID int64, strategy string) error {
	if err := h.sqlite.UpdateAPIKeysStrategy(ctx, channelID, strategy); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "apikeys_strategy",
		data:      &syncTaskAPIKeys{channelID: channelID, strategy: strategy},
	})

	return nil
}

func (h *HybridStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	if err := h.sqlite.DeleteAPIKey(ctx, channelID, keyIndex); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "apikey_delete",
		data:      &syncTaskAPIKeys{channelID: channelID, keyIndex: keyIndex},
	})

	return nil
}

func (h *HybridStore) CompactKeyIndices(ctx context.Context, channelID int64, removedIndex int) error {
	if err := h.sqlite.CompactKeyIndices(ctx, channelID, removedIndex); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "apikeys_compact",
		data:      &syncTaskAPIKeys{channelID: channelID, keyIndex: removedIndex},
	})

	return nil
}

func (h *HybridStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	if err := h.sqlite.DeleteAllAPIKeys(ctx, channelID); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "apikeys_delete_all",
		data:      &syncTaskAPIKeys{channelID: channelID},
	})

	return nil
}

// === Cooldown Management ===

func (h *HybridStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return h.sqlite.GetAllChannelCooldowns(ctx)
}

func (h *HybridStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	return h.sqlite.BumpChannelCooldown(ctx, channelID, now, statusCode)
}

func (h *HybridStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	return h.sqlite.ResetChannelCooldown(ctx, channelID)
}

func (h *HybridStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	return h.sqlite.SetChannelCooldown(ctx, channelID, until)
}

func (h *HybridStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return h.sqlite.GetAllKeyCooldowns(ctx)
}

func (h *HybridStore) BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	return h.sqlite.BumpKeyCooldown(ctx, channelID, keyIndex, now, statusCode)
}

func (h *HybridStore) ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	return h.sqlite.ResetKeyCooldown(ctx, channelID, keyIndex)
}

func (h *HybridStore) SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error {
	return h.sqlite.SetKeyCooldown(ctx, channelID, keyIndex, until)
}

// === Log Management ===

func (h *HybridStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	// 写入 SQLite（主存储）
	if err := h.sqlite.AddLog(ctx, e); err != nil {
		return err
	}

	// 异步同步到 MySQL（非阻塞）
	h.enqueueSyncTask(&syncTask{
		operation: "log",
		data:      &syncTaskLog{entry: e},
	})

	return nil
}

func (h *HybridStore) BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error {
	// 写入 SQLite（主存储）
	if err := h.sqlite.BatchAddLogs(ctx, logs); err != nil {
		return err
	}

	// 异步批量同步到 MySQL（单个任务，避免队列风暴）
	if len(logs) > 0 {
		h.enqueueSyncTask(&syncTask{
			operation: "log_batch",
			data:      &syncTaskLogBatch{entries: logs},
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
	if err := h.sqlite.CreateAuthToken(ctx, token); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "authtoken_create",
		data:      &syncTaskAuthToken{token: token},
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
	if err := h.sqlite.UpdateAuthToken(ctx, token); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "authtoken_update",
		data:      &syncTaskAuthToken{token: token},
	})

	return nil
}

func (h *HybridStore) DeleteAuthToken(ctx context.Context, id int64) error {
	if err := h.sqlite.DeleteAuthToken(ctx, id); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "authtoken_delete",
		data:      &syncTaskAuthToken{id: id},
	})

	return nil
}

func (h *HybridStore) UpdateTokenLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	return h.sqlite.UpdateTokenLastUsed(ctx, tokenHash, now)
}

func (h *HybridStore) UpdateTokenStats(ctx context.Context, tokenHash string, isSuccess bool, duration float64, isStreaming bool, firstByteTime float64, promptTokens int64, completionTokens int64, cacheReadTokens int64, cacheCreationTokens int64, costUSD float64) error {
	return h.sqlite.UpdateTokenStats(ctx, tokenHash, isSuccess, duration, isStreaming, firstByteTime, promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens, costUSD)
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
	if err := h.sqlite.UpdateSetting(ctx, key, value); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "setting_update",
		data:      &syncTaskSetting{key: key, value: value},
	})

	return nil
}

func (h *HybridStore) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	if err := h.sqlite.BatchUpdateSettings(ctx, updates); err != nil {
		return err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "settings_batch",
		data:      &syncTaskSetting{updates: updates},
	})

	return nil
}

// === Admin Session Management ===

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
	created, updated, err = h.sqlite.ImportChannelBatch(ctx, channels)
	if err != nil {
		return 0, 0, err
	}

	// 异步同步到 MySQL
	h.enqueueSyncTask(&syncTask{
		operation: "import_batch",
		data:      &syncTaskImport{channels: channels},
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
		// 1. 停止同步 worker
		h.stopOnce.Do(func() {
			close(h.stopCh)
		})
		h.syncWg.Wait()

		// 2. 关闭 SQLite
		if closeErr := h.sqlite.Close(); closeErr != nil {
			err = closeErr
		}

		// 3. 关闭 MySQL
		if closeErr := h.mysql.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
