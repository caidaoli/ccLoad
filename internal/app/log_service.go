package app

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
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
func (s *LogService) logWorker() {
	defer s.wg.Done()

	batch := make([]*model.LogEntry, 0, config.LogBatchSize)
	ticker := time.NewTicker(config.LogBatchTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			// shutdown时尽量flush掉已排队的日志，避免“退出即丢日志”
			for {
				select {
				case entry, ok := <-s.logChan:
					if !ok {
						s.flushIfNeeded(batch)
						return
					}
					batch = append(batch, entry)
					if len(batch) >= config.LogBatchSize {
						s.flushLogs(batch)
						batch = batch[:0]
					}
				default:
					s.flushIfNeeded(batch)
					return
				}
			}

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
	if len(logs) == 0 {
		return
	}

	timeout := time.Duration(config.LogFlushTimeoutMs) * time.Millisecond
	maxRetries := config.LogFlushMaxRetries
	if s.isShutdownInProgress() {
		// 关停阶段不做重试，避免单批刷盘耗时放大拖垮优雅关闭预算。
		maxRetries = 1
	}

	var lastErr error
	attempts := 0
retryLoop:
	for attempt := 1; attempt <= maxRetries; attempt++ {
		attempts = attempt
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := s.store.BatchAddLogs(ctx, logs)
		cancel()
		if err == nil {
			if attempt > 1 {
				log.Printf("[WARN] 日志批量写入重试成功 (attempt=%d/%d, batch_size=%d)", attempt, maxRetries, len(logs))
			}
			return
		}

		lastErr = err
		if attempt < maxRetries {
			// 运行中可能刚进入关停流程，此时停止重试，避免拖慢 drain。
			if s.isShutdownInProgress() {
				break
			}

			log.Printf("[WARN] 日志批量写入失败，准备重试 (attempt=%d/%d, batch_size=%d): %v", attempt, maxRetries, len(logs), err)
			backoff := time.Duration(attempt) * config.LogFlushRetryBackoff
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-s.shutdownCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				break retryLoop
			}
		}
	}

	log.Printf("[ERROR] 日志批量写入最终失败 (attempts=%d, batch_size=%d): %v", attempts, len(logs), lastErr)
}

func (s *LogService) isShutdownInProgress() bool {
	return s.isShuttingDown != nil && s.isShuttingDown.Load()
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
		count := s.logDropCount.Add(1)
		// [FIX] 降低采样频率，每10次丢弃打印一次（原来是100次）
		// 设计原则：及早暴露问题，避免用户在黑暗中调试
		if count%10 == 1 {
			log.Printf("[ERROR] 日志队列已满，日志被丢弃 (累计丢弃: %d) - 考虑增大 LOG_BUFFER_SIZE 或 LOG_WORKERS", count)
		}
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
			// 使用带超时的context，避免日志清理阻塞关闭流程。
			// [FIX] P0-4: WithTimeout 的 cancel 必须在每次循环内执行，不能在循环里 defer 到 goroutine 退出。
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				cutoff := time.Now().AddDate(0, 0, -s.retentionDays)

				// 通过Store接口清理旧日志，忽略错误（非关键操作）
				_ = s.store.CleanupLogsBefore(ctx, cutoff)
			}()

		case <-s.shutdownCh:
			// 收到关闭信号，直接退出（不执行最后一次清理）
			return
		}
	}
}
