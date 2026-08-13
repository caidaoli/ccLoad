package app

import "os"

// readCurrentRSSBytes 读取当前物理内存占用(/proc/self/statm)。
func readCurrentRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	return parseStatmResidentBytes(string(data), os.Getpagesize())
}
