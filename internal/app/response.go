package app

import (
	"ccLoad/internal/errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// StandardResponse 统一API响应结构
// ✅ 遵循接口隔离原则(ISP): 所有API使用统一的响应格式
type StandardResponse[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"` // 机器可读错误码
}

// ResponseHelper 响应辅助函数集合
// ✅ 遵循单一职责原则(SRP): 专注于HTTP响应的构建和返回
type ResponseHelper struct{}

// NewResponseHelper 创建响应助手实例
func NewResponseHelper() *ResponseHelper {
	return &ResponseHelper{}
}

// Success 返回成功响应（泛型版本）
// ✅ 遵循DRY原则: 消除重复的JSON响应代码
func (h *ResponseHelper) Success(c *gin.Context, data any) {
	c.JSON(http.StatusOK, StandardResponse[any]{
		Success: true,
		Data:    data,
	})
}

// SuccessWithCount 返回成功响应（带总数，用于分页）
func (h *ResponseHelper) SuccessWithCount(c *gin.Context, data any, count int64) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    data,
		"count":   count,
	})
}

// Error 返回错误响应（自动识别错误类型）
// ✅ 智能错误处理: 自动提取应用级错误码
func (h *ResponseHelper) Error(c *gin.Context, httpCode int, err error) {
	resp := StandardResponse[any]{
		Success: false,
		Error:   err.Error(),
	}

	// 如果是应用级错误,提取错误码
	if appErr, ok := err.(*errors.AppError); ok {
		resp.Code = string(appErr.Code) // 转换 ErrorCode 类型为 string
	}

	c.JSON(httpCode, resp)
}

// ErrorMsg 返回错误响应（仅消息）
func (h *ResponseHelper) ErrorMsg(c *gin.Context, httpCode int, message string) {
	c.JSON(httpCode, StandardResponse[any]{
		Success: false,
		Error:   message,
	})
}

// BadRequest 快捷方法 - 400 错误
func (h *ResponseHelper) BadRequest(c *gin.Context, message string) {
	h.ErrorMsg(c, http.StatusBadRequest, message)
}

// NotFound 快捷方法 - 404 错误
func (h *ResponseHelper) NotFound(c *gin.Context, resource string) {
	h.ErrorMsg(c, http.StatusNotFound, resource+" not found")
}

// InternalError 快捷方法 - 500 错误
func (h *ResponseHelper) InternalError(c *gin.Context, err error) {
	h.Error(c, http.StatusInternalServerError, err)
}

// Unauthorized 快捷方法 - 401 错误
func (h *ResponseHelper) Unauthorized(c *gin.Context, message string) {
	h.ErrorMsg(c, http.StatusUnauthorized, message)
}

// ServiceUnavailable 快捷方法 - 503 错误
func (h *ResponseHelper) ServiceUnavailable(c *gin.Context, message string) {
	h.ErrorMsg(c, http.StatusServiceUnavailable, message)
}
