//go:build unix

package zaiauth

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

/**
 * @brief 获取 ZCode 请求头的 IANA 时区，无法识别时返回 unknown。
 * @return Go 的命名时区或系统 zoneinfo 标识；无法识别时返回 unknown。
 */
func clientTimezone() string {
	if name := time.Now().Location().String(); name != "Local" {
		return name
	}
	// Go 从 /etc/localtime 加载时保留 Local，需从系统配置恢复 IANA 名称。
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
			return name
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	return "unknown"
}

/**
 * @brief 获取 Unix 内核版本以匹配 ZCode 的 os.release。
 * @return 内核 release，系统调用失败时为空。
 */
func hostRelease() string {
	var info unix.Utsname
	if unix.Uname(&info) != nil {
		return ""
	}
	var value []byte
	for _, c := range info.Release {
		if c == 0 {
			break
		}
		value = append(value, byte(c))
	}
	return string(value)
}
