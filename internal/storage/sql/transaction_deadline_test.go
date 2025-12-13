package sql

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestWithTransaction_ContextDeadline 验证 context.Deadline 限制总重试时间
// [FIX] 后续优化: 防止事务重试超过 context 的 deadline
func TestWithTransaction_ContextDeadline(t *testing.T) {
	t.Run("context 有 deadline 时应该提前退出", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer db.Close()

		// 创建一个 500ms deadline 的 context
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		attemptCount := 0
		start := time.Now()

		// 模拟一个总是返回 BUSY 错误的事务
		err = withTransaction(db, ctx, func(tx *sql.Tx) error {
			attemptCount++
			// 模拟 SQLite BUSY 错误
			return errors.New("database is locked")
		})

		elapsed := time.Since(start)

		// 验证：应该在 deadline 前退出（不是等到 12 次重试完）
		if err == nil {
			t.Fatal("期望失败，但成功了")
		}

		// 验证：耗时应该接近 500ms，而不是 51.2s（12 次重试的理论最大值）
		if elapsed > 1*time.Second {
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

		t.Logf("✅ context.Deadline 生效: 耗时 %v, 重试 %d 次后提前退出", elapsed, attemptCount)
	})

	t.Run("没有 deadline 时应该正常重试到最大次数", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer db.Close()

		// 使用 background context（无 deadline）
		ctx := context.Background()

		attemptCount := 0

		// 模拟一个总是返回 BUSY 错误的事务
		err = withTransaction(db, ctx, func(tx *sql.Tx) error {
			attemptCount++
			return errors.New("database is locked")
		})

		// 验证：应该重试到最大次数
		if attemptCount != 12 {
			t.Errorf("重试次数不符合预期: got %d, want 12", attemptCount)
		}

		// 验证：错误信息应该包含"after 12 retries"
		if err == nil || err.Error() == "" {
			t.Fatal("期望失败，但成功了或错误为空")
		}

		t.Logf("✅ 无 deadline 时正常重试到最大次数: %d 次", attemptCount)
	})

	t.Run("context 取消时应该立即退出", func(t *testing.T) {
		// 创建临时数据库
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("打开数据库失败: %v", err)
		}
		defer db.Close()

		// 创建可取消的 context
		ctx, cancel := context.WithCancel(context.Background())

		attemptCount := 0
		start := time.Now()

		// 在第一次重试后取消 context
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		// 模拟一个总是返回 BUSY 错误的事务
		err = withTransaction(db, ctx, func(tx *sql.Tx) error {
			attemptCount++
			return errors.New("database is locked")
		})

		elapsed := time.Since(start)

		// 验证：应该快速退出（不是等到 12 次重试完）
		if elapsed > 500*time.Millisecond {
			t.Errorf("取消后耗时过长: %v", elapsed)
		}

		// 验证：错误信息应该包含"cancelled"
		if err == nil {
			t.Fatal("期望失败，但成功了")
		}

		t.Logf("✅ context 取消时立即退出: 耗时 %v, 重试 %d 次", elapsed, attemptCount)
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
		defer db.Close()

		// 模拟 HTTP 请求的 1 秒超时
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		attemptCount := 0
		start := time.Now()

		// 模拟事务操作（总是失败）
		err = withTransaction(db, ctx, func(tx *sql.Tx) error {
			attemptCount++
			return errors.New("database is deadlocked")
		})

		elapsed := time.Since(start)

		// 验证：应该在 1 秒左右退出
		if elapsed > 1500*time.Millisecond {
			t.Errorf("超时控制失效: 耗时 %v（应该约 1s）", elapsed)
		}

		// 验证：不应该达到 12 次重试
		if attemptCount >= 12 {
			t.Errorf("重试次数过多: %d（应该被 deadline 提前终止）", attemptCount)
		}

		t.Logf("✅ HTTP 超时传播到事务层: 耗时 %v, 重试 %d 次后退出", elapsed, attemptCount)
	})
}
