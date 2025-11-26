package app

import (
	"ccLoad/internal/model"
	"strconv"
	"strings"
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

// BuildLogFilter 从查询参数构建LogFilter（DRY原则：消除重复的过滤逻辑）
// 支持的查询参数：
// - channel_id: 精确匹配渠道ID
// - channel_name: 精确匹配渠道名称
// - channel_name_like: 模糊匹配渠道名称
// - model: 精确匹配模型名称
// - model_like: 模糊匹配模型名称
func BuildLogFilter(c *gin.Context) model.LogFilter {
	var lf model.LogFilter

	// 渠道ID过滤
	if cidStr := strings.TrimSpace(c.Query("channel_id")); cidStr != "" {
		if id, err := strconv.ParseInt(cidStr, 10, 64); err == nil && id > 0 {
			lf.ChannelID = &id
		}
	}

	// 渠道名称精确匹配
	if cn := strings.TrimSpace(c.Query("channel_name")); cn != "" {
		lf.ChannelName = cn
	}

	// 渠道名称模糊匹配
	if cnl := strings.TrimSpace(c.Query("channel_name_like")); cnl != "" {
		lf.ChannelNameLike = cnl
	}

	// 模型名称精确匹配
	if m := strings.TrimSpace(c.Query("model")); m != "" {
		lf.Model = m
	}

	// 模型名称模糊匹配
	if ml := strings.TrimSpace(c.Query("model_like")); ml != "" {
		lf.ModelLike = ml
	}

	// 状态码精确匹配
	if scStr := strings.TrimSpace(c.Query("status_code")); scStr != "" {
		if code, err := strconv.Atoi(scStr); err == nil && code > 0 {
			lf.StatusCode = &code
		}
	}

	return lf
}
