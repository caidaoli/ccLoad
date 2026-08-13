package app

// readCurrentRSSBytes 在 macOS 上没有免 cgo 的当前 RSS 获取途径,返回 0 表示不可用。
func readCurrentRSSBytes() uint64 { return 0 }
