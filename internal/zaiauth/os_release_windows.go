package zaiauth

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

/**
 * @brief 使用系统 ICU 获取与 Node Intl 相同格式的本地 IANA 时区。
 */
var defaultTimeZone = windows.NewLazySystemDLL("icu.dll").NewProc("ucal_getDefaultTimeZone")

/**
 * @brief 获取 ZCode 请求头的 IANA 时区，无法识别时返回 unknown。
 * @return 本地 IANA 时区名；ICU 不可用或读取失败时返回 unknown。
 */
func clientTimezone() string {
	if err := defaultTimeZone.Find(); err != nil {
		return "unknown"
	}
	var name [128]uint16
	var status int32
	length, _, _ := defaultTimeZone.Call(uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), uintptr(unsafe.Pointer(&status)))
	// ICU 的负数状态是成功警告；正数才表示错误，包括缓冲区不足。
	if status > 0 || length == 0 || length >= uintptr(len(name)) {
		return "unknown"
	}
	return windows.UTF16ToString(name[:length])
}

/**
 * @brief 获取 Windows 内核版本以匹配 ZCode 的 os.release。
 * @return major.minor.build 格式版本。
 */
func hostRelease() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
