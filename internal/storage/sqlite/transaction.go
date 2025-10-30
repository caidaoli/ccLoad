package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// 事务高阶函数 (P3性能优化)
// ============================================================================

// WithTransaction 在主数据库事务中执行函数（用于channels、api_keys、key_rr操作）
// ✅ DRY原则：统一事务管理逻辑，消除重复代码
// ✅ 错误处理：自动回滚，优雅处理panic
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
func (s *SQLiteStore) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTransaction(s.db, ctx, fn)
}

// WithLogTransaction 在日志数据库事务中执行函数（用于logs操作）
// ✅ 拆分日志库：减少主数据库锁竞争，提升并发性能
func (s *SQLiteStore) WithLogTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTransaction(s.logDB, ctx, fn)
}

// withTransaction 核心事务执行逻辑（私有函数，遵循DRY原则）
// ✅ KISS原则：简单的事务模板，自动处理提交/回滚
// ✅ 安全性：panic恢复 + defer回滚双重保障
func withTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) error {
	// 增加死锁重试机制
	// 问题: SQLite在高并发事务下可能返回"database is deadlocked"错误
	// 解决: 自动重试带指数退避,最多重试5次

	const maxRetries = 5
	const baseDelay = 10 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := executeSingleTransaction(db, ctx, fn)

		// 成功或非BUSY错误,立即返回
		if err == nil || !isSQLiteBusyError(err) {
			return err
		}

		// BUSY错误且还有重试机会,执行退避后重试
		if attempt < maxRetries-1 {
			sleepWithBackoff(attempt, baseDelay)
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
	// 设计原则:防御性编程,即使panic也能回滚
	defer func() {
		if p := recover(); p != nil {
			// panic恢复:强制回滚并转换为error
			_ = tx.Rollback()
			err = fmt.Errorf("transaction panic: %v", p)
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

// sleepWithBackoff 执行指数退避sleep
// 公式: delay = baseDelay * 2^attempt + jitter
func sleepWithBackoff(attempt int, baseDelay time.Duration) {
	// 计算延迟:10ms, 20ms, 40ms, 80ms, 160ms
	delay := baseDelay * time.Duration(1<<uint(attempt))

	// 添加随机抖动(±25%),避免多个goroutine同时重试
	// 使用纳秒时间戳的后两位作为随机因子(0-99)
	randomFactor := float64(time.Now().UnixNano()%100) / 100.0 // 0.00 到 0.99
	jitter := time.Duration(float64(delay) * (0.5 + 0.5*randomFactor))

	time.Sleep(jitter)
}
