package util

import (
	"log"
	"strings"
	"unicode"

	"ccLoad/internal/config"
)

// SanitizeLogMessage 消毒日志消息，防止日志注入攻击
// 设计原则：
// 1. 移除控制字符（换行符、回车符、制表符等）
// 2. 限制消息长度，防止日志爆炸
// 3. 保留可读性，不过度转义
func SanitizeLogMessage(msg string) string {
	if msg == "" {
		return ""
	}

	// 移除危险的控制字符
	msg = strings.ReplaceAll(msg, "\n", "\\n")
	msg = strings.ReplaceAll(msg, "\r", "\\r")
	msg = strings.ReplaceAll(msg, "\t", "\\t")

	// 移除其他控制字符（ASCII 0-31，除了常见的空格、换行等）
	var builder strings.Builder
	builder.Grow(len(msg))

	for _, r := range msg {
		// 保留可打印字符和常见空白字符
		if unicode.IsPrint(r) || r == ' ' {
			builder.WriteRune(r)
		} else if r < 32 {
			// 将其他控制字符转义为可见形式
			builder.WriteString("\\x")
			builder.WriteString(string(rune('0' + (r/16)%16)))
			builder.WriteString(string(rune('0' + r%16)))
		}
	}

	msg = builder.String()

	// 限制长度，防止单条日志过长
	if len(msg) > config.LogMaxMessageLength {
		msg = msg[:config.LogMaxMessageLength] + "...[truncated]"
	}

	return msg
}

// SanitizeError 消毒error对象的Error()输出
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return SanitizeLogMessage(err.Error())
}

// SafePrintf 安全的日志打印函数（自动消毒所有参数）
// 用于替代标准库的 log.Printf，防止日志注入攻击
func SafePrintf(format string, args ...any) {
	// 消毒所有参数
	sanitizedArgs := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			sanitizedArgs[i] = SanitizeLogMessage(v)
		case error:
			sanitizedArgs[i] = SanitizeError(v)
		default:
			sanitizedArgs[i] = v
		}
	}
	log.Printf(format, sanitizedArgs...)
}

// SafePrint 安全的日志打印函数（自动消毒所有参数）
// 用于替代标准库的 log.Print
func SafePrint(args ...any) {
	sanitizedArgs := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			sanitizedArgs[i] = SanitizeLogMessage(v)
		case error:
			sanitizedArgs[i] = SanitizeError(v)
		default:
			sanitizedArgs[i] = v
		}
	}
	log.Print(sanitizedArgs...)
}
