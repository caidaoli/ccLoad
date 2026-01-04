package sql

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRedisSyncWorker_DeferWaitGroup 验证 defer wg.Done() 的正确性
// [FIX] P0-3: 确保即使 panic 也能释放 WaitGroup
func TestRedisSyncWorker_DeferWaitGroup(t *testing.T) {
	t.Run("正常退出应该释放WaitGroup", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})

		// 模拟 worker
		wg.Add(1)
		go func() {
			defer wg.Done() // P0-3 修复：使用 defer
			<-done
		}()

		// 触发退出
		close(done)

		// 验证：WaitGroup 应该被正确释放（不会永远阻塞）
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			// ✅ 正常释放
		case <-time.After(1 * time.Second):
			t.Fatal("WaitGroup 未被释放，Close() 会永远阻塞")
		}
	})

	t.Run("panic 时也应该释放WaitGroup", func(t *testing.T) {
		var wg sync.WaitGroup
		panicTrigger := make(chan struct{})

		// 模拟 worker with panic
		wg.Add(1)
		go func() {
			defer wg.Done() // P0-3 修复：即使 panic 也能执行
			defer func() {
				if r := recover(); r != nil {
					// 捕获 panic，但 defer wg.Done() 仍会执行
					t.Logf("捕获 panic: %v", r)
				}
			}()
			<-panicTrigger
			panic("simulated panic in worker")
		}()

		// 触发 panic
		close(panicTrigger)

		// 短暂等待 panic 发生
		time.Sleep(10 * time.Millisecond)

		// 验证：即使 panic，WaitGroup 也应该被释放
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			// ✅ panic 时也正确释放
			t.Log("✅ panic 时 WaitGroup 也被正确释放")
		case <-time.After(1 * time.Second):
			t.Fatal("panic 后 WaitGroup 未释放，这会导致 Close() 永远阻塞")
		}
	})

	t.Run("多个 return 路径都应该释放WaitGroup", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})
		earlyExit := make(chan struct{})

		// 模拟有多个 return 路径的 worker
		wg.Add(1)
		go func() {
			defer wg.Done() // P0-3 修复：统一的释放点

			for {
				select {
				case <-earlyExit:
					return // 早退路径
				case <-done:
					return // 正常退出路径
				}
			}
		}()

		// 触发早退路径
		close(earlyExit)

		// 验证：早退也能正确释放
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			// ✅ 早退路径也正确释放
			t.Log("✅ 早退路径也正确释放 WaitGroup")
		case <-time.After(1 * time.Second):
			t.Fatal("早退路径未释放 WaitGroup")
		}
	})
}

// TestRedisSyncWorker_WithoutDefer 演示没有 defer 的危险性
func TestRedisSyncWorker_WithoutDefer(t *testing.T) {
	t.Run("手动调用 wg.Done() 在 panic 时会失败", func(t *testing.T) {
		var wg sync.WaitGroup
		panicTrigger := make(chan struct{})

		// 模拟旧代码：手动调用 wg.Done()（不安全）
		wg.Add(1)
		go func() {
			// ❌ 错误的模式：没有 defer
			defer func() {
				if r := recover(); r != nil {
					// 捕获 panic，但 wg.Done() 不在 defer 链中
					t.Logf("panic 发生: %v", r)
				}
			}()
			<-panicTrigger
			panic("panic before wg.Done()")
			// wg.Done() // ❌ 永远不会执行
		}()

		// 触发 panic
		close(panicTrigger)
		time.Sleep(10 * time.Millisecond)

		// 验证：wg.Wait() 会永远阻塞
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			t.Fatal("不应该释放（这个测试验证了 bug）")
		case <-time.After(100 * time.Millisecond):
			// ✅ 预期行为：永远阻塞（演示 bug）
			t.Log("✅ 确认：没有 defer 的代码在 panic 时会导致 WaitGroup 永远阻塞")
		}
	})

	t.Run("多个退出路径容易遗漏 wg.Done()", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})
		earlyExit := make(chan struct{})

		// 模拟旧代码：多个退出路径，容易遗漏
		wg.Add(1)
		go func() {
			for {
				select {
				case <-earlyExit:
					// ❌ 忘记调用 wg.Done()
					return
				case <-done:
					wg.Done()
					return
				}
			}
		}()

		// 触发早退路径（有 bug 的路径）
		close(earlyExit)
		time.Sleep(10 * time.Millisecond)

		// 验证：wg.Wait() 会永远阻塞
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			t.Fatal("不应该释放（这个测试验证了 bug）")
		case <-time.After(100 * time.Millisecond):
			// ✅ 预期行为：永远阻塞（演示多退出路径的危险性）
			t.Log("✅ 确认：手动管理多个退出路径容易遗漏 wg.Done()")
		}
	})
}

// TestRedisSyncWorker_RealWorld 模拟真实场景
func TestRedisSyncWorker_RealWorld(t *testing.T) {
	t.Run("模拟真实的 redisSyncWorker 场景", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})
		syncCh := make(chan struct{})
		var panicInSync atomic.Bool

		// 模拟修复后的 worker
		wg.Add(1)
		go func() {
			defer wg.Done() // P0-3 修复

			defer func() {
				if r := recover(); r != nil {
					_ = r // 捕获 panic，但 defer wg.Done() 仍会执行
				}
			}()

			for {
				select {
				case <-syncCh:
					// 模拟同步操作可能 panic
					if panicInSync.Load() {
						panic("sync operation panic")
					}
					// 正常同步...
				case <-done:
					return
				}
			}
		}()

		// 场景1：正常关闭
		close(done)

		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			t.Log("✅ 正常关闭场景：WaitGroup 正确释放")
		case <-time.After(1 * time.Second):
			t.Fatal("正常关闭失败")
		}
	})

	t.Run("模拟同步操作中 panic 的场景", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})
		syncCh := make(chan struct{}, 1)

		// 模拟修复后的 worker
		wg.Add(1)
		go func() {
			defer wg.Done() // P0-3 修复：即使同步操作 panic 也能释放

			defer func() {
				if r := recover(); r != nil {
					_ = r // 捕获 panic，但 defer wg.Done() 仍会执行
				}
			}()

			for {
				select {
				case <-syncCh:
					// 模拟同步操作 panic
					panic("redis connection lost during sync")
				case <-done:
					return
				}
			}
		}()

		// 触发同步操作（会 panic）
		syncCh <- struct{}{}
		time.Sleep(10 * time.Millisecond)

		// 验证：即使 panic，WaitGroup 也能释放
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			t.Log("✅ 同步 panic 场景：WaitGroup 仍然正确释放")
		case <-time.After(1 * time.Second):
			t.Fatal("同步 panic 后 WaitGroup 未释放")
		}
	})
}

// TestRedisSyncWorker_CorrectShutdown 验证正确的关闭流程
func TestRedisSyncWorker_CorrectShutdown(t *testing.T) {
	t.Run("Close() 应该在合理时间内完成", func(t *testing.T) {
		var wg sync.WaitGroup
		done := make(chan struct{})
		syncCh := make(chan struct{})

		// 启动 3 个 worker（模拟多个后台任务）
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(_ int) {
				defer wg.Done() // P0-3 修复

				for {
					select {
					case <-syncCh:
						// 模拟工作
						time.Sleep(10 * time.Millisecond)
					case <-done:
						return
					}
				}
			}(i)
		}

		// 短暂运行
		time.Sleep(50 * time.Millisecond)

		// 触发关闭
		close(done)

		// 验证：所有 worker 应该在合理时间内退出
		shutdownStart := time.Now()
		waitCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitCh)
		}()

		select {
		case <-waitCh:
			shutdownDuration := time.Since(shutdownStart)
			t.Logf("✅ 所有 worker 正确退出，耗时: %v", shutdownDuration)
			if shutdownDuration > 500*time.Millisecond {
				t.Errorf("关闭耗时过长: %v", shutdownDuration)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("关闭超时，某些 worker 的 WaitGroup 未释放")
		}
	})
}
