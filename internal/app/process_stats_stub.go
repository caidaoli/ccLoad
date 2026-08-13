//go:build !linux && !darwin

package app

// Windows 等平台未实现进程资源采集;返回零值,前端按不可用展示。
func readProcessRusage() (userSeconds, systemSeconds float64, maxRSSBytes uint64, ok bool) {
	return 0, 0, 0, false
}

func readCurrentRSSBytes() uint64 { return 0 }
