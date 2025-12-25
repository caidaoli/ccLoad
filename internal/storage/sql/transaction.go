package sql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// 事务接口与高阶函数
// ============================================================================

// WithTransaction 在主数据库事务中执行函数（用于channels、api_keys、key_rr操作）
// [INFO] DRY原则：统一事务管理逻辑，消除重复代码
// [INFO] 错误处理：自动回滚，优雅处理panic
//
// 使用示例:
//
//	err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
//	    _, err := tx.ExecContext(ctx, "INSERT INTO channels ...")
//	    if err != nil {
//	        return err // 自动回滚
//	    }
//	    _, err = tx.ExecContext(ctx, "INSERT INTO api_keys ...")
//	    return err // 成功则自动提交
//	})
func (s *SQLStore) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTransaction(s.db, ctx, fn)
}

// withTransaction 核心事务执行逻辑（私有函数，遵循DRY原则）
// [INFO] KISS原则：简单的事务模板，自动处理提交/回滚
// [INFO] 安全性：panic恢复 + defer回滚双重保障
// [FIX] P1-5: 对齐注释和实现，说明实际重试次数
// [FIX] 后续优化: 支持 context.Deadline 限制总重试时间
func withTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) error {
	// 增加死锁重试机制
	// 问题: SQLite在高并发事务下可能返回"database is deadlocked"错误
	// 解决: 自动重试带指数退避，最多12次重试（attempt 0-11）
	//
	// 重试时间轴：
	//   attempt 0:  25ms (初次失败后第一次重试)
	//   attempt 1:  50ms
	//   attempt 2: 100ms
	//   attempt 3: 200ms
	//   ...
	//   attempt 11: 51.2s (最大单次等待)
	//
	// 注意：实际等待时间有 50%-99.5% 的随机抖动，避免惊群效应
	// 注意：如果 context 有 deadline，会在到达 deadline 时提前退出

	const maxRetries = 12
	const baseDelay = 25 * time.Millisecond

	// 检查 context 是否有 deadline（用于限制总重试时间）
	deadline, hasDeadline := ctx.Deadline()

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := executeSingleTransaction(db, ctx, fn)

		// 成功或非BUSY错误,立即返回
		if err == nil || !isSQLiteBusyError(err) {
			return err
		}

		// BUSY错误且还有重试机会
		if attempt < maxRetries-1 {
			// 计算下次重试的等待时间
			nextDelay := calculateBackoffDelay(attempt, baseDelay)

			// 如果有 deadline，检查是否会超时
			if hasDeadline {
				// 预估下次重试后是否会超过 deadline
				if time.Now().Add(nextDelay).After(deadline) {
					return fmt.Errorf("transaction aborted: context deadline would be exceeded (attempted %d retries): %w", attempt+1, err)
				}
			}

			// 检查 context 是否已取消
			select {
			case <-ctx.Done():
				return fmt.Errorf("transaction cancelled after %d retries: %w", attempt+1, ctx.Err())
			case <-time.After(nextDelay):
				// 等待完成，继续重试
			}
			continue
		}

		// 所有重试都失败
		return fmt.Errorf("transaction failed after %d retries: %w", maxRetries, err)
	}

	return fmt.Errorf("unexpected: retry loop exited without result")
}

// executeSingleTransaction 执行单次事务(无重试)
func executeSingleTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) (err error) {
	// 1. 开启事务
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// 2. 延迟回滚(幂等操作,提交后回滚无效)
	// 设计原则:Fail-Fast,panic回滚后继续炸掉(不隐藏编程错误)
	defer func() {
		if p := recover(); p != nil {
			// panic恢复:强制回滚后继续panic(不吞掉编程错误)
			_ = tx.Rollback()
			panic(p)
		} else if err != nil {
			// 函数返回错误:回滚事务
			_ = tx.Rollback()
		}
	}()

	// 3. 执行用户函数
	if err = fn(tx); err != nil {
		return err // defer会自动回滚
	}

	// 4. 提交事务
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// isSQLiteBusyError 检测是否是SQLite的BUSY/LOCKED错误
// 这些错误表示数据库暂时不可用,可以通过重试解决
func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := strings.ToLower(err.Error())

	// SQLite BUSY/LOCKED错误的特征字符串
	busyPatterns := []string{
		"database is locked",
		"database is deadlocked",
		"database table is locked",
		"sqlite_busy",
		"sqlite_locked",
	}

	for _, pattern := range busyPatterns {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}

	return false
}

// calculateBackoffDelay 计算指数退避延迟（带随机抖动）
// [FIX] 后续优化: 提取计算逻辑，支持 deadline 检查前预估等待时间
//
// 公式: delay = baseDelay * 2^attempt * jitter
// jitter 范围: [0.5, 0.995] (即 50% 到 99.5%)
//
// 示例（baseDelay = 25ms）：
//
//	attempt 0: 25ms * [0.5, 0.995] = 12.5ms ~ 24.9ms
//	attempt 1: 50ms * [0.5, 0.995] = 25ms ~ 49.8ms
//	attempt 2: 100ms * [0.5, 0.995] = 50ms ~ 99.5ms
func calculateBackoffDelay(attempt int, baseDelay time.Duration) time.Duration {
	// 计算基础延迟：指数增长
	delay := baseDelay * time.Duration(1<<uint(attempt))

	// 添加随机抖动，避免多个 goroutine 同时重试（惊群效应）
	// 使用纳秒时间戳的后两位作为随机因子 (0-99)
	randomFactor := float64(time.Now().UnixNano()%100) / 100.0         // 0.00 到 0.99
	jitter := time.Duration(float64(delay) * (0.5 + 0.5*randomFactor)) // [50%, 99.5%]

	return jitter
}
