package errors

import (
	"fmt"
)

// ErrorCode 错误代码类型（便于机器识别和监控）
type ErrorCode string

const (
	// Key选择相关错误
	ErrCodeNoKeys        ErrorCode = "NO_KEYS"         // 渠道未配置API Key
	ErrCodeAllCooldown   ErrorCode = "ALL_COOLDOWN"    // 所有Key都在冷却中
	ErrCodeKeyExhausted  ErrorCode = "KEY_EXHAUSTED"   // Key重试次数耗尽

	// 数据库操作错误
	ErrCodeDBQuery       ErrorCode = "DB_QUERY"        // 数据库查询失败
	ErrCodeDBInsert      ErrorCode = "DB_INSERT"       // 数据库插入失败
	ErrCodeDBUpdate      ErrorCode = "DB_UPDATE"       // 数据库更新失败
	ErrCodeDBDelete      ErrorCode = "DB_DELETE"       // 数据库删除失败

	// 渠道相关错误
	ErrCodeChannelNotFound    ErrorCode = "CHANNEL_NOT_FOUND"    // 渠道不存在
	ErrCodeChannelDisabled    ErrorCode = "CHANNEL_DISABLED"     // 渠道已禁用
	ErrCodeChannelCooldown    ErrorCode = "CHANNEL_COOLDOWN"     // 渠道冷却中
	ErrCodeNoAvailableChannel ErrorCode = "NO_AVAILABLE_CHANNEL" // 无可用渠道

	// HTTP请求错误
	ErrCodeHTTPRequest   ErrorCode = "HTTP_REQUEST"    // HTTP请求失败
	ErrCodeHTTPTimeout   ErrorCode = "HTTP_TIMEOUT"    // HTTP请求超时
	ErrCodeHTTPStream    ErrorCode = "HTTP_STREAM"     // 流式响应错误

	// 认证相关错误
	ErrCodeUnauthorized  ErrorCode = "UNAUTHORIZED"    // 未授权
	ErrCodeInvalidToken  ErrorCode = "INVALID_TOKEN"   // Token无效
	ErrCodeTokenExpired  ErrorCode = "TOKEN_EXPIRED"   // Token过期

	// 配置相关错误
	ErrCodeInvalidConfig ErrorCode = "INVALID_CONFIG"  // 配置无效
	ErrCodeMissingConfig ErrorCode = "MISSING_CONFIG"  // 配置缺失
)

// AppError 应用级错误结构（支持错误链和上下文信息）
type AppError struct {
	Code     ErrorCode         // 错误代码（机器可识别）
	Message  string            // 错误消息（人类可读）
	Err      error             // 底层错误（支持错误链）
	Context  map[string]any    // 错误上下文（便于调试和监控）
}

// Error 实现 error 接口
func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// Unwrap 实现错误链（Go 1.13+）
func (e *AppError) Unwrap() error {
	return e.Err
}

// WithContext 添加错误上下文
func (e *AppError) WithContext(key string, value any) *AppError {
	if e.Context == nil {
		e.Context = make(map[string]any)
	}
	e.Context[key] = value
	return e
}

// ============== Key选择错误工厂函数 ==============

// NoKeysConfigured 渠道未配置API Key
func NoKeysConfigured(channelID int64) *AppError {
	return &AppError{
		Code:    ErrCodeNoKeys,
		Message: fmt.Sprintf("channel %d has no API keys configured", channelID),
		Context: map[string]any{"channel_id": channelID},
	}
}

// AllKeysCooldown 所有Key都在冷却中
func AllKeysCooldown(channelID int64, keyCount int) *AppError {
	return &AppError{
		Code:    ErrCodeAllCooldown,
		Message: fmt.Sprintf("all %d keys for channel %d are in cooldown", keyCount, channelID),
		Context: map[string]any{
			"channel_id": channelID,
			"key_count":  keyCount,
		},
	}
}

// KeyRetriesExhausted Key重试次数耗尽
func KeyRetriesExhausted(channelID int64, maxRetries int) *AppError {
	return &AppError{
		Code:    ErrCodeKeyExhausted,
		Message: fmt.Sprintf("key retries exhausted for channel %d (max: %d)", channelID, maxRetries),
		Context: map[string]any{
			"channel_id":  channelID,
			"max_retries": maxRetries,
		},
	}
}

// ============== 数据库错误工厂函数 ==============

// DBQueryError 数据库查询失败
func DBQueryError(operation string, err error) *AppError {
	return &AppError{
		Code:    ErrCodeDBQuery,
		Message: fmt.Sprintf("database query failed: %s", operation),
		Err:     err,
		Context: map[string]any{"operation": operation},
	}
}

// DBInsertError 数据库插入失败
func DBInsertError(table string, err error) *AppError {
	return &AppError{
		Code:    ErrCodeDBInsert,
		Message: fmt.Sprintf("failed to insert into %s", table),
		Err:     err,
		Context: map[string]any{"table": table},
	}
}

// DBUpdateError 数据库更新失败
func DBUpdateError(table string, id int64, err error) *AppError {
	return &AppError{
		Code:    ErrCodeDBUpdate,
		Message: fmt.Sprintf("failed to update %s (id: %d)", table, id),
		Err:     err,
		Context: map[string]any{"table": table, "id": id},
	}
}

// DBDeleteError 数据库删除失败
func DBDeleteError(table string, id int64, err error) *AppError {
	return &AppError{
		Code:    ErrCodeDBDelete,
		Message: fmt.Sprintf("failed to delete from %s (id: %d)", table, id),
		Err:     err,
		Context: map[string]any{"table": table, "id": id},
	}
}

// ============== 渠道错误工厂函数 ==============

// ChannelNotFound 渠道不存在
func ChannelNotFound(channelID int64) *AppError {
	return &AppError{
		Code:    ErrCodeChannelNotFound,
		Message: fmt.Sprintf("channel %d not found", channelID),
		Context: map[string]any{"channel_id": channelID},
	}
}

// ChannelDisabled 渠道已禁用
func ChannelDisabled(channelID int64, name string) *AppError {
	return &AppError{
		Code:    ErrCodeChannelDisabled,
		Message: fmt.Sprintf("channel %d (%s) is disabled", channelID, name),
		Context: map[string]any{"channel_id": channelID, "name": name},
	}
}

// ChannelInCooldown 渠道冷却中
func ChannelInCooldown(channelID int64, cooldownUntil int64) *AppError {
	return &AppError{
		Code:    ErrCodeChannelCooldown,
		Message: fmt.Sprintf("channel %d is in cooldown until %d", channelID, cooldownUntil),
		Context: map[string]any{
			"channel_id":     channelID,
			"cooldown_until": cooldownUntil,
		},
	}
}

// NoAvailableChannel 无可用渠道
func NoAvailableChannel(model string) *AppError {
	return &AppError{
		Code:    ErrCodeNoAvailableChannel,
		Message: fmt.Sprintf("no available channel supports model: %s", model),
		Context: map[string]any{"model": model},
	}
}

// ============== HTTP错误工厂函数 ==============

// HTTPRequestError HTTP请求失败
func HTTPRequestError(url string, method string, err error) *AppError {
	return &AppError{
		Code:    ErrCodeHTTPRequest,
		Message: fmt.Sprintf("%s request to %s failed", method, url),
		Err:     err,
		Context: map[string]any{"url": url, "method": method},
	}
}

// HTTPTimeoutError HTTP请求超时
func HTTPTimeoutError(url string, timeout int) *AppError {
	return &AppError{
		Code:    ErrCodeHTTPTimeout,
		Message: fmt.Sprintf("request to %s timed out after %ds", url, timeout),
		Context: map[string]any{"url": url, "timeout": timeout},
	}
}

// HTTPStreamError 流式响应错误
func HTTPStreamError(stage string, err error) *AppError {
	return &AppError{
		Code:    ErrCodeHTTPStream,
		Message: fmt.Sprintf("stream error at stage: %s", stage),
		Err:     err,
		Context: map[string]any{"stage": stage},
	}
}

// ============== 认证错误工厂函数 ==============

// UnauthorizedError 未授权
func UnauthorizedError(reason string) *AppError {
	return &AppError{
		Code:    ErrCodeUnauthorized,
		Message: "unauthorized: " + reason,
		Context: map[string]any{"reason": reason},
	}
}

// InvalidTokenError Token无效
func InvalidTokenError() *AppError {
	return &AppError{
		Code:    ErrCodeInvalidToken,
		Message: "invalid or missing authorization token",
	}
}

// TokenExpiredError Token过期
func TokenExpiredError() *AppError {
	return &AppError{
		Code:    ErrCodeTokenExpired,
		Message: "token has expired",
	}
}

// ============== 配置错误工厂函数 ==============

// InvalidConfigError 配置无效
func InvalidConfigError(field string, reason string) *AppError {
	return &AppError{
		Code:    ErrCodeInvalidConfig,
		Message: fmt.Sprintf("invalid config field '%s': %s", field, reason),
		Context: map[string]any{"field": field, "reason": reason},
	}
}

// MissingConfigError 配置缺失
func MissingConfigError(field string) *AppError {
	return &AppError{
		Code:    ErrCodeMissingConfig,
		Message: fmt.Sprintf("missing required config field: %s", field),
		Context: map[string]any{"field": field},
	}
}

// ============== 工具函数 ==============

// IsAppError 判断是否为AppError
func IsAppError(err error) bool {
	_, ok := err.(*AppError)
	return ok
}

// GetErrorCode 获取错误代码（如果是AppError）
func GetErrorCode(err error) ErrorCode {
	if appErr, ok := err.(*AppError); ok {
		return appErr.Code
	}
	return ""
}

// HasErrorCode 判断错误是否为特定错误代码
func HasErrorCode(err error, code ErrorCode) bool {
	return GetErrorCode(err) == code
}
