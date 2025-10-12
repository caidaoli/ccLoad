package testutil

import "fmt"

// TestChannelRequest 渠道测试请求结构
type TestChannelRequest struct {
	Model       string            `json:"model" binding:"required"`
	MaxTokens   int               `json:"max_tokens,omitempty"`   // 可选，默认512
	Stream      bool              `json:"stream,omitempty"`       // 可选，流式响应
	Content     string            `json:"content,omitempty"`      // 可选，测试内容，默认"test"
	Headers     map[string]string `json:"headers,omitempty"`      // 可选，自定义请求头
	ChannelType string            `json:"channel_type,omitempty"` // 可选，渠道类型：anthropic(默认)、codex、gemini
	KeyIndex    int               `json:"key_index,omitempty"`    // 可选，指定测试的Key索引，默认0（第一个）
}

// Validate 实现RequestValidator接口
func (tr *TestChannelRequest) Validate() error {
	if tr.Model == "" {
		return fmt.Errorf("model cannot be empty")
	}
	return nil
}
