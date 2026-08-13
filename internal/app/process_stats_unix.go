//go:build linux || darwin

package app

import (
	"runtime"
	"syscall"
)

// readProcessRusage 一次 getrusage 同时取进程累计 CPU 时间与峰值 RSS。
// Linux 的 Maxrss 单位是 KiB,macOS 是字节,此处统一换算为字节。
func readProcessRusage() (userSeconds, systemSeconds float64, maxRSSBytes uint64, ok bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, 0, false
	}
	maxRSS := usage.Maxrss
	if maxRSS > 0 {
		maxRSSBytes = uint64(maxRSS)
		if runtime.GOOS == "linux" {
			maxRSSBytes *= 1024
		}
	}
	return timevalSeconds(usage.Utime), timevalSeconds(usage.Stime), maxRSSBytes, true
}

func timevalSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}
