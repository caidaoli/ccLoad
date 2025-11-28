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
	Range  string `form:"range" binding:"omitempty"` // 时间范围: today/yesterday/this_week等
	Limit  int    `form:"limit" binding:"omitempty,min=1,max=1000"`
	Offset int    `form:"offset" binding:"omitempty,min=0"`
}

// SetDefaults 设置默认值
func (p *PaginationParams) SetDefaults() {
	if p.Range == "" {
		p.Range = "today"
	}
	if p.Limit <= 0 {
		p.Limit = 200
	}
}


// GetTimeRange 根据Range参数计算时间范围(开始时间和结束时间)（用于统计API）
// 支持的范围: today(本日), yesterday(昨日), day_before_yesterday(前日),
//           this_week(本周), last_week(上周), this_month(本月), last_month(上月)
func (p *PaginationParams) GetTimeRange() (startTime, endTime time.Time) {
	now := time.Now()

	switch p.Range {
	case "today":
		// 本日：今天0:00到现在
		startTime = beginningOfDay(now)
		endTime = now
	case "yesterday":
		// 昨日：昨天0:00到昨天23:59:59
		yesterday := now.AddDate(0, 0, -1)
		startTime = beginningOfDay(yesterday)
		endTime = endOfDay(yesterday)
	case "day_before_yesterday":
		// 前日：前天0:00到前天23:59:59
		dayBefore := now.AddDate(0, 0, -2)
		startTime = beginningOfDay(dayBefore)
		endTime = endOfDay(dayBefore)
	case "this_week":
		// 本周：本周一0:00到现在
		startTime = beginningOfWeek(now)
		endTime = now
	case "last_week":
		// 上周：上周一0:00到上周日23:59:59
		lastWeek := now.AddDate(0, 0, -7)
		startTime = beginningOfWeek(lastWeek)
		endTime = endOfWeek(lastWeek)
	case "this_month":
		// 本月：本月1号0:00到现在
		startTime = beginningOfMonth(now)
		endTime = now
	case "last_month":
		// 上月：上月1号0:00到上月最后一天23:59:59
		lastMonth := now.AddDate(0, -1, 0)
		startTime = beginningOfMonth(lastMonth)
		endTime = endOfMonth(lastMonth)
	default:
		// 未知范围，默认使用today
		startTime = beginningOfDay(now)
		endTime = now
	}

	return
}

// beginningOfDay 返回某一天的0:00:00
func beginningOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// endOfDay 返回某一天的23:59:59.999999999
func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
}

// beginningOfWeek 返回某一周的周一0:00:00
func beginningOfWeek(t time.Time) time.Time {
	weekday := t.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	monday := t.AddDate(0, 0, -int(weekday)+1)
	return beginningOfDay(monday)
}

// endOfWeek 返回某一周的周日23:59:59.999999999
func endOfWeek(t time.Time) time.Time {
	weekday := t.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	sunday := t.AddDate(0, 0, 7-int(weekday))
	return endOfDay(sunday)
}

// beginningOfMonth 返回某个月的1号0:00:00
func beginningOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

// endOfMonth 返回某个月的最后一天23:59:59.999999999
func endOfMonth(t time.Time) time.Time {
	return endOfDay(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location()))
}

// ParsePaginationParams 解析通用分页参数
func ParsePaginationParams(c *gin.Context) *PaginationParams {
	var params PaginationParams

	params.Range = strings.TrimSpace(c.Query("range"))

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
