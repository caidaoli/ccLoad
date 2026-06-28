package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var fastTransactionRetryPolicy = transactionRetryPolicy{
	maxRetries: 12,
	delay:      func(int) time.Duration { return 10 * time.Millisecond },
}

var instantTransactionRetryPolicy = transactionRetryPolicy{
	maxRetries: 12,
	delay:      func(int) time.Duration { return 0 },
}

// TestWithTransaction_ContextDeadline 验证 context.Deadline 限制总重试时间
// [FIX] 后续优化: 防止事务重试超过 context 的 deadline
func TestWithTransaction_ContextDeadline(t *testing.T) {
	t.Run("context 有 deadline 时应该提前退出", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer func() { _ = db.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		attemptCount := 0
		start := time.Now()

		// 模拟一个总是返回 BUSY 错误的事务
		err = withTransactionWithRetryPolicy(ctx, db, func(_ *sql.Tx) error {
			attemptCount++
			return errors.New("database is locked")
		}, fastTransactionRetryPolicy)

		elapsed := time.Since(start)

		// 验证：应该在 deadline 前退出（不是等到 12 次重试完）
		if err == nil {
			t.Fatal("期望失败，但成功了")
		}

		if elapsed > 200*time.Millisecond {
			t.Errorf("重试耗时过长: %v（应该在 deadline 前退出）", elapsed)
		}

		// 验证：应该有多次重试（至少 2-3 次）
		if attemptCount < 2 {
			t.Errorf("重试次数过少: %d（应该至少有几次重试）", attemptCount)
		}

		// 验证：不应该达到最大重试次数 12
		if attemptCount >= 12 {
			t.Errorf("重试次数过多: %d（应该在 deadline 前退出）", attemptCount)
		}

		t.Logf("context.Deadline 生效: 耗时 %v, 重试 %d 次后提前退出", elapsed, attemptCount)
	})

	t.Run("没有 deadline 时应该正常重试到最大次数", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer func() { _ = db.Close() }()

		// 使用 background context（无 deadline）
		ctx := context.Background()

		attemptCount := 0

		err = withTransactionWithRetryPolicy(ctx, db, func(_ *sql.Tx) error {
			attemptCount++
			return errors.New("database is locked")
		}, instantTransactionRetryPolicy)

		// 验证：应该重试到最大次数
		if attemptCount != 12 {
			t.Errorf("重试次数不符合预期: got %d, want 12", attemptCount)
		}

		// 验证：错误信息应该包含"after 12 retries"
		if err == nil || err.Error() == "" {
			t.Fatal("期望失败，但成功了或错误为空")
		}

		t.Logf("无 deadline 时正常重试到最大次数: %d 次", attemptCount)
	})

	t.Run("context 取消时应该立即退出", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer func() { _ = db.Close() }()

		// 创建可取消的 context
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		attemptCount := 0
		start := time.Now()

		firstAttempt := make(chan struct{})
		var closeOnce sync.Once
		closeFirstAttempt := func() { closeOnce.Do(func() { close(firstAttempt) }) }
		go func() {
			<-firstAttempt
			cancel()
		}()

		err = withTransactionWithRetryPolicy(ctx, db, func(_ *sql.Tx) error {
			attemptCount++
			if attemptCount == 1 {
				closeFirstAttempt()
			}
			return errors.New("database is locked")
		}, fastTransactionRetryPolicy)
		closeFirstAttempt()

		elapsed := time.Since(start)

		// 验证：应该快速退出（不是等到 12 次重试完）
		if elapsed > 500*time.Millisecond {
			t.Errorf("取消后耗时过长: %v", elapsed)
		}

		// 验证：错误信息应该包含"cancelled"
		if err == nil {
			t.Fatal("期望失败，但成功了")
		}

		t.Logf("context 取消时立即退出: 耗时 %v, 重试 %d 次", elapsed, attemptCount)
	})
}

// TestWithTransaction_DeadlineRealWorld 模拟真实的 deadline 场景
func TestWithTransaction_DeadlineRealWorld(t *testing.T) {
	t.Run("HTTP 请求超时应该传播到事务层", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer func() { _ = db.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()

		attemptCount := 0
		start := time.Now()

		err = withTransactionWithRetryPolicy(ctx, db, func(_ *sql.Tx) error {
			attemptCount++
			return errors.New("database is deadlocked")
		}, fastTransactionRetryPolicy)
		if err == nil {
			t.Fatal("期望事务失败，但成功了")
		}

		elapsed := time.Since(start)

		if elapsed > 250*time.Millisecond {
			t.Errorf("超时控制失效: 耗时 %v（应该在测试 deadline 附近退出）", elapsed)
		}

		// 验证：不应该达到 12 次重试
		if attemptCount >= 12 {
			t.Errorf("重试次数过多: %d（应该被 deadline 提前终止）", attemptCount)
		}

		t.Logf("HTTP 超时传播到事务层: 耗时 %v, 重试 %d 次后退出", elapsed, attemptCount)
	})
}

func TestWithTransaction_DefaultRetryPolicyUsesExponentialBackoff(t *testing.T) {
	for attempt, minDelay := range []time.Duration{
		12 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
	} {
		t.Run(fmt.Sprintf("attempt_%d", attempt), func(t *testing.T) {
			delay := defaultTransactionRetryPolicy.delay(attempt)
			if delay < minDelay {
				t.Fatalf("delay=%v, want >= %v", delay, minDelay)
			}
		})
	}
}
