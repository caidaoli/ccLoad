package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// CheckGorutineLeak 检查测试执行期间是否有goroutine泄漏
// 使用方式：
//
//	defer testutil.CheckGorutineLeak(t)()
//
// 原理：比较测试前后的goroutine数量和堆栈
func CheckGorutineLeak(t *testing.T) func() {
	t.Helper()

	// 记录测试开始前的goroutine快照
	before := countRelevantGoroutines()

	return func() {
		t.Helper()

		// 等待异步goroutine完成
		time.Sleep(100 * time.Millisecond)
		runtime.GC() // 触发GC，清理已完成的goroutine

		// 记录测试结束后的goroutine快照
		after := countRelevantGoroutines()

		leaked := after - before
		if leaked > 0 {
			// 打印当前所有goroutine的堆栈
			buf := make([]byte, 1<<20) // 1MB buffer
			stackLen := runtime.Stack(buf, true)
			t.Errorf("❌ Goroutine泄漏检测到 %d 个泄漏\n\n当前goroutine堆栈:\n%s",
				leaked, string(buf[:stackLen]))
		} else if leaked < 0 {
			t.Logf("✅ Goroutine数量减少 %d 个（正常）", -leaked)
		} else {
			t.Logf("✅ 无Goroutine泄漏")
		}
	}
}

// countRelevantGoroutines 计算相关goroutine数量
// 过滤掉testing框架自己的goroutine
func countRelevantGoroutines() int {
	buf := make([]byte, 1<<20)
	stackLen := runtime.Stack(buf, true)
	stackStr := string(buf[:stackLen])

	// 按 goroutine 分组
	stacks := strings.Split(stackStr, "\n\n")
	count := 0

	for _, stack := range stacks {
		// 跳过空堆栈
		if strings.TrimSpace(stack) == "" {
			continue
		}

		// 过滤掉testing框架的goroutine
		if isTestingGoroutine(stack) {
			continue
		}

		count++
	}

	return count
}

// isTestingGoroutine 判断是否是testing框架或数据库连接池的goroutine
func isTestingGoroutine(stack string) bool {
	testingPatterns := []string{
		"testing.(*T).Run",
		"testing.tRunner",
		"testing.Main",
		"runtime.goexit",
		// 过滤database/sql的后台goroutine
		// 这些goroutine由Go标准库管理，Close()后会自动清理但需要时间
		"database/sql.(*DB).connectionOpener",
		"database/sql.(*DB).connectionCleaner",
	}

	for _, pattern := range testingPatterns {
		if strings.Contains(stack, pattern) {
			return true
		}
	}

	return false
}
