package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/storage/sqlite"
)

// LogService 日志管理服务
//
// 职责：处理所有日志相关的业务逻辑
// - 异步日志记录（批量写入）
// - 日志 Worker 管理
// - 日志清理（定时任务）
// - 优雅关闭
//
// 遵循 SRP 原则：仅负责日志管理，不涉及代理、认证、管理 API
type LogService struct {
	store storage.Store

	// 日志队列和 Worker
	logChan      chan *model.LogEntry
	logWorkers   int
	logDropCount atomic.Uint64

	// 日志保留天数（启动时确定，修改后重启生效）
	retentionDays int

	// 优雅关闭
	shutdownCh     chan struct{}
	isShuttingDown *atomic.Bool
	wg             *sync.WaitGroup
}

// NewLogService 创建日志服务实例
func NewLogService(
	store storage.Store,
	logBufferSize int,
	logWorkers int,
	retentionDays int, // 启动时确定，修改后重启生效
	shutdownCh chan struct{},
	isShuttingDown *atomic.Bool,
	wg *sync.WaitGroup,
) *LogService {
	return &LogService{
		store:          store,
		logChan:        make(chan *model.LogEntry, logBufferSize),
		logWorkers:     logWorkers,
		retentionDays:  retentionDays,
		shutdownCh:     shutdownCh,
		isShuttingDown: isShuttingDown,
		wg:             wg,
	}
}

// ============================================================================
// Worker 管理
// ============================================================================

// StartWorkers 启动日志 Worker
func (s *LogService) StartWorkers() {
	for i := 0; i < s.logWorkers; i++ {
		s.wg.Add(1)
		go s.logWorker()
	}
}

// logWorker 日志 Worker（后台协程）
// 简化shutdown逻辑，利用channel关闭特性
func (s *LogService) logWorker() {
	defer s.wg.Done()

	batch := make([]*model.LogEntry, 0, config.LogBatchSize)
	ticker := time.NewTicker(config.LogBatchTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			// 优先检查shutdown信号，快速响应关闭
			s.flushIfNeeded(batch)
			return

		case entry, ok := <-s.logChan:
			if !ok {
				// logChan已关闭，flush剩余日志并退出
				s.flushIfNeeded(batch)
				return
			}

			batch = append(batch, entry)
			if len(batch) >= config.LogBatchSize {
				s.flushLogs(batch)
				batch = batch[:0]
				ticker.Reset(config.LogBatchTimeout)
			}

		case <-ticker.C:
			// 移除嵌套select，简化定时flush逻辑
			// 设计原则：
			// - ticker触发时直接flush当前batch
			// - 如果logChan关闭，下次循环会在entry <- logChan中捕获
			// - shutdown信号在select中优先级最高，保证快速响应
			s.flushIfNeeded(batch)
			batch = batch[:0]
		}
	}
}

// flushLogs 批量写入日志
func (s *LogService) flushLogs(logs []*model.LogEntry) {
	// 为日志持久化增加超时控制，避免阻塞关闭或积压
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LogFlushTimeoutMs)*time.Millisecond)
	defer cancel()

	// 优先使用SQLite批量写入，加速刷盘
	if ss, ok := s.store.(*sqlite.SQLiteStore); ok {
		_ = ss.BatchAddLogs(ctx, logs)
		return
	}
	// 回退逐条写入
	for _, e := range logs {
		_ = s.store.AddLog(ctx, e)
	}
}

// flushIfNeeded 辅助函数：当batch非空时执行flush
func (s *LogService) flushIfNeeded(batch []*model.LogEntry) {
	if len(batch) > 0 {
		s.flushLogs(batch)
	}
}

// ============================================================================
// 日志记录方法
// ============================================================================

// AddLogAsync 异步添加日志
func (s *LogService) AddLogAsync(entry *model.LogEntry) {
	// shutdown时不再写入日志
	if s.isShuttingDown.Load() {
		return
	}

	select {
	case s.logChan <- entry:
		// 成功放入队列
	default:
		// 队列满，丢弃日志（计数用于监控）
		s.logDropCount.Add(1)
	}
}

// ============================================================================
// 日志清理
// ============================================================================

// StartCleanupLoop 启动日志清理后台协程
// 每小时检查一次，删除3天前的日志
// 支持优雅关闭
func (s *LogService) StartCleanupLoop() {
	s.wg.Add(1)
	go s.cleanupOldLogsLoop()
}

// cleanupOldLogsLoop 日志清理后台协程（私有方法）
func (s *LogService) cleanupOldLogsLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(config.LogCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 使用带超时的context，避免日志清理阻塞关闭流程
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cutoff := time.Now().AddDate(0, 0, -s.retentionDays)

			// 通过Store接口清理旧日志，忽略错误（非关键操作）
			_ = s.store.CleanupLogsBefore(ctx, cutoff)
			cancel() // 立即释放资源

		case <-s.shutdownCh:
			// 收到关闭信号，直接退出（不执行最后一次清理）
			return
		}
	}
}

// ============================================================================
// 优雅关闭
// ============================================================================

// Shutdown 优雅关闭日志服务
// 注意：不需要等待 Workers，因为 Server 会通过 wg.Wait() 等待
func (s *LogService) Shutdown(ctx context.Context) error {
	// 关闭日志通道，通知所有 Worker 退出
	close(s.logChan)
	return nil
}
