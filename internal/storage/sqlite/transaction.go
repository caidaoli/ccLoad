package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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
func withTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) (err error) {
	// 1. 开启事务
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// 2. 延迟回滚（幂等操作，提交后回滚无效）
	// 设计原则：防御性编程，即使panic也能回滚
	defer func() {
		if p := recover(); p != nil {
			// panic恢复：强制回滚并转换为error
			_ = tx.Rollback()
			err = fmt.Errorf("transaction panic: %v", p)
		} else if err != nil {
			// 函数返回错误：回滚事务
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
