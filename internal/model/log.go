package model

import (
	"strconv"
	"time"
)

// JSONTime 自定义时间类型，使用Unix时间戳进行JSON序列化
// 设计原则：与数据库格式统一，减少转换复杂度（KISS原则）
type JSONTime struct {
	time.Time
}

// MarshalJSON 实现JSON序列化
func (jt JSONTime) MarshalJSON() ([]byte, error) {
	if jt.Time.IsZero() {
		return []byte("0"), nil
	}
	return []byte(strconv.FormatInt(jt.Time.Unix(), 10)), nil
}

// UnmarshalJSON 实现JSON反序列化
func (jt *JSONTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == "0" {
		jt.Time = time.Time{}
		return nil
	}
	ts, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}
	jt.Time = time.Unix(ts, 0)
	return nil
}

// LogEntry 请求日志条目
type LogEntry struct {
	ID            int64    `json:"id"`
	Time          JSONTime `json:"time"`
	Model         string   `json:"model"`
	ChannelID     *int64   `json:"channel_id,omitempty"`
	ChannelName   string   `json:"channel_name,omitempty"`
	StatusCode    int      `json:"status_code"`
	Message       string   `json:"message"`
	Duration      float64  `json:"duration"`                  // 总耗时（秒）
	IsStreaming   bool     `json:"is_streaming"`              // 是否为流式请求
	FirstByteTime *float64 `json:"first_byte_time,omitempty"` // 首字节响应时间（秒）
	APIKeyUsed    string   `json:"api_key_used,omitempty"`    // 使用的API Key（查询时自动脱敏为 abcd...klmn 格式）

	// Token统计（2025-11新增，支持Claude API usage字段）
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

// LogFilter 日志查询过滤条件
type LogFilter struct {
	ChannelID       *int64
	ChannelName     string
	ChannelNameLike string
	Model           string
	ModelLike       string
}
