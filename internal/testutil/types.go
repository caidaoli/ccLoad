package testutil

import "fmt"

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type chatContentBlock struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// ChatMessage 多轮对话消息
type ChatMessage struct {
	Role          string             `json:"role"`
	Content       any                `json:"content"`
	ContentBlocks []chatContentBlock `json:"-"`
}

// TestChannelRequest 渠道测试请求结构
type TestChannelRequest struct {
	Model             string            `json:"model" binding:"required"`
	MaxTokens         int               `json:"max_tokens,omitempty"`         // 可选，默认512
	Temperature       *float64          `json:"temperature,omitempty"`        // 可选，采样温度
	TopP              *float64          `json:"top_p,omitempty"`              // 可选，核采样阈值
	Stream            bool              `json:"stream,omitempty"`             // 可选，流式响应
	Content           string            `json:"content,omitempty"`            // 可选，测试内容，默认"test"；Messages 非空时忽略
	Messages          []ChatMessage     `json:"messages,omitempty"`           // 可选，多轮对话消息；非空时覆盖 Content
	SystemPrompt      string            `json:"system_prompt,omitempty"`      // 可选，按协议注入的系统提示词
	ThinkingEffort    string            `json:"thinking_effort,omitempty"`    // 可选，思考等级：none/minimal/low/medium/high
	BuiltinSearch     bool              `json:"builtin_search,omitempty"`     // 可选，启用模型内置搜索工具
	Headers           map[string]string `json:"headers,omitempty"`            // 可选，自定义请求头
	ChannelType       string            `json:"channel_type,omitempty"`       // 可选，旧调用方兼容字段
	ProtocolTransform string            `json:"protocol_transform,omitempty"` // 可选，客户端协议；默认等于渠道原生协议
	KeyIndex          int               `json:"key_index,omitempty"`          // 可选，指定测试的Key索引，默认0（第一个）
	APIKey            string            `json:"api_key,omitempty"`            // 可选，测试当前编辑器中的未保存Key
	BaseURL           string            `json:"base_url,omitempty"`           // 可选，仅 /test-url 使用，强制指定测试URL（必须属于该渠道）
}

// Validate 实现RequestValidator接口
func (tr *TestChannelRequest) Validate() error {
	if tr.Model == "" {
		return fmt.Errorf("model cannot be empty")
	}
	return nil
}
