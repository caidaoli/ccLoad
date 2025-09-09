package main

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// PaginationParams 通用分页参数结构
type PaginationParams struct {
	Hours  int `form:"hours" binding:"omitempty,min=1"`
	Limit  int `form:"limit" binding:"omitempty,min=1,max=1000"`
	Offset int `form:"offset" binding:"omitempty,min=0"`
}

// SetDefaults 设置默认值
func (p *PaginationParams) SetDefaults() {
	if p.Hours <= 0 {
		p.Hours = 24
	}
	if p.Limit <= 0 {
		p.Limit = 200
	}
}

// GetSinceTime 根据Hours参数计算开始时间
func (p *PaginationParams) GetSinceTime() time.Time {
	return time.Now().Add(-time.Duration(p.Hours) * time.Hour)
}

// ParsePaginationParams 解析通用分页参数
func ParsePaginationParams(c *gin.Context) *PaginationParams {
	var params PaginationParams
	
	// 使用传统方式解析以保持向后兼容
	if hours, err := strconv.Atoi(c.DefaultQuery("hours", "24")); err == nil && hours > 0 {
		params.Hours = hours
	}
	if limit, err := strconv.Atoi(c.DefaultQuery("limit", "200")); err == nil && limit > 0 {
		params.Limit = limit
	}
	if offset, err := strconv.Atoi(c.DefaultQuery("offset", "0")); err == nil && offset >= 0 {
		params.Offset = offset
	}
	
	params.SetDefaults()
	return &params
}

// APIResponse 标准API响应结构
type APIResponse[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// RespondJSON 发送成功的JSON响应
func RespondJSON[T any](c *gin.Context, code int, data T) {
	c.JSON(code, APIResponse[T]{
		Success: code >= 200 && code < 300,
		Data:    data,
	})
}

// RespondJSONWithCount 发送带计数的JSON响应
func RespondJSONWithCount[T any](c *gin.Context, code int, data T, count int) {
	c.JSON(code, APIResponse[T]{
		Success: code >= 200 && code < 300,
		Data:    data,
		Count:   count,
	})
}

// RespondError 发送错误响应
func RespondError(c *gin.Context, code int, err error) {
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	} else {
		errMsg = "unknown error"
	}
	
	c.JSON(code, APIResponse[any]{
		Success: false,
		Error:   errMsg,
	})
}

// RespondErrorMsg 发送错误消息响应
func RespondErrorMsg(c *gin.Context, code int, message string) {
	c.JSON(code, APIResponse[any]{
		Success: false,
		Error:   message,
	})
}

// ParseInt64Param 安全解析int64参数
func ParseInt64Param(c *gin.Context, paramName string) (int64, error) {
	param := c.Param(paramName)
	return strconv.ParseInt(param, 10, 64)
}

// MethodRouter HTTP方法路由器，简化方法分发逻辑
type MethodRouter struct {
	handlers map[string]gin.HandlerFunc
}

// NewMethodRouter 创建新的方法路由器
func NewMethodRouter() *MethodRouter {
	return &MethodRouter{
		handlers: make(map[string]gin.HandlerFunc),
	}
}

// GET 注册GET处理器
func (mr *MethodRouter) GET(handler gin.HandlerFunc) *MethodRouter {
	mr.handlers["GET"] = handler
	return mr
}

// POST 注册POST处理器
func (mr *MethodRouter) POST(handler gin.HandlerFunc) *MethodRouter {
	mr.handlers["POST"] = handler
	return mr
}

// PUT 注册PUT处理器
func (mr *MethodRouter) PUT(handler gin.HandlerFunc) *MethodRouter {
	mr.handlers["PUT"] = handler
	return mr
}

// DELETE 注册DELETE处理器
func (mr *MethodRouter) DELETE(handler gin.HandlerFunc) *MethodRouter {
	mr.handlers["DELETE"] = handler
	return mr
}

// Handle 执行路由分发
func (mr *MethodRouter) Handle(c *gin.Context) {
	method := c.Request.Method
	if handler, exists := mr.handlers[method]; exists {
		handler(c)
	} else {
		RespondErrorMsg(c, 405, "method not allowed")
	}
}

// RequestValidator 请求验证器接口
type RequestValidator interface {
	Validate() error
}

// BindAndValidate 绑定请求数据并验证
func BindAndValidate(c *gin.Context, obj RequestValidator) error {
	if err := c.ShouldBindJSON(obj); err != nil {
		return err
	}
	return obj.Validate()
}