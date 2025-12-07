package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// TxHandler 事务处理器接口
type TxHandler interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

var _ TxHandler = (*sql.Tx)(nil)

// WithTransaction 在事务中执行函数
func (s *MySQLStore) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTransaction(s.db, ctx, fn)
}

// WithLogTransaction 在事务中执行函数（日志操作）
func (s *MySQLStore) WithLogTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTransaction(s.db, ctx, fn)
}

func withTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) error {
	const maxRetries = 5
	const baseDelay = 50 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := executeSingleTransaction(db, ctx, fn)

		if err == nil || !isMySQLRetryableError(err) {
			return err
		}

		if attempt < maxRetries-1 {
			sleepWithBackoff(attempt, baseDelay)
			continue
		}

		return fmt.Errorf("transaction failed after %d retries: %w", maxRetries, err)
	}

	return fmt.Errorf("unexpected: retry loop exited without result")
}

func executeSingleTransaction(db *sql.DB, ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			err = fmt.Errorf("transaction panic: %v", p)
		} else if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// isMySQLRetryableError 检测是否是MySQL可重试错误
func isMySQLRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := strings.ToLower(err.Error())

	// MySQL 可重试错误
	retryablePatterns := []string{
		"deadlock",
		"lock wait timeout",
		"try restarting transaction",
		"error 1213", // Deadlock found
		"error 1205", // Lock wait timeout
	}

	for _, pattern := range retryablePatterns {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}

	return false
}

func sleepWithBackoff(attempt int, baseDelay time.Duration) {
	delay := baseDelay * time.Duration(1<<uint(attempt))
	randomFactor := float64(time.Now().UnixNano()%100) / 100.0
	jitter := time.Duration(float64(delay) * (0.5 + 0.5*randomFactor))
	time.Sleep(jitter)
}
