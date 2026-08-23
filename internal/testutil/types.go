package testutil

import (
	"fmt"
	"strings"

	"ccLoad/internal/util"
)

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

// ImageGenerationOptions marks an admin channel test as a non-streaming
// Chat Completions image request. It is internal test plumbing, not part of
// the public /admin/channels/:id/test request contract.
type ImageGenerationOptions struct {
	AspectRatio string
	ImageSize   string
}

// TestChannelRequest 渠道测试请求结构
type TestChannelRequest struct {
	Model             string                  `json:"model" binding:"required"`
	MaxTokens         int                     `json:"max_tokens,omitempty"`      // 可选，默认512
	Temperature       *float64                `json:"temperature,omitempty"`     // 可选，采样温度
	TopP              *float64                `json:"top_p,omitempty"`           // 可选，核采样阈值
	Stream            bool                    `json:"stream,omitempty"`          // 可选，流式响应
	Content           string                  `json:"content,omitempty"`         // 可选，测试内容，默认"test"；Messages 非空时忽略
	Messages          []ChatMessage           `json:"messages,omitempty"`        // 可选，多轮对话消息；非空时覆盖 Content
	SystemPrompt      string                  `json:"system_prompt,omitempty"`   // 可选，按协议注入的系统提示词
	ThinkingEffort    string                  `json:"thinking_effort,omitempty"` // 可选，思考等级：none/minimal/low/medium/high/xhigh(max)
	BuiltinSearch     bool                    `json:"builtin_search,omitempty"`  // 可选，启用模型内置搜索工具
	Headers           map[string]string       `json:"headers,omitempty"`         // 可选，自定义请求头
	ClientProtocol    string                  `json:"client_protocol,omitempty"` // 客户端请求协议
	SessionID         string                  `json:"session_id,omitempty"`      // 管理对话身份；同一对话跨轮次保持稳定
	KeyIndex          *int                    `json:"key_index,omitempty"`       // 可选；省略时按模型范围自动选择可用 Key
	APIKey            string                  `json:"api_key,omitempty"`         // 可选，测试当前编辑器中的未保存Key
	BaseURL           string                  `json:"base_url,omitempty"`        // 可选，仅 /test-url 使用，强制指定测试URL（必须属于该渠道）
	WaitForCapacity   bool                    `json:"-"`                         // 后台批任务等待渠道配额；交互式测试仍快速失败
	ImageGeneration   *ImageGenerationOptions `json:"-"`                         // 生图 Tab 的 Chat Completions 请求选项
	resolvedSessionID string
}

// ResolveSessionID 返回当前测试请求的安全 UUID 会话身份。
// 客户端值只作为稳定种子，不直接进入上游请求头或请求体。
func (tr *TestChannelRequest) ResolveSessionID() string {
	if tr == nil {
		return util.NewUUIDv4()
	}
	if tr.resolvedSessionID != "" {
		return tr.resolvedSessionID
	}
	seed := strings.TrimSpace(tr.SessionID)
	if seed == "" {
		tr.resolvedSessionID = util.NewUUIDv4()
	} else {
		tr.resolvedSessionID = util.NewUUIDv5(util.NameSpaceOID, "ccload:admin-test-session:"+seed)
	}
	return tr.resolvedSessionID
}

// Validate 实现RequestValidator接口
func (tr *TestChannelRequest) Validate() error {
	if tr.Model == "" {
		return fmt.Errorf("model cannot be empty")
	}
	tr.ClientProtocol = strings.ToLower(strings.TrimSpace(tr.ClientProtocol))
	switch tr.ClientProtocol {
	case "anthropic", "codex", "openai", "gemini":
		return nil
	case "":
		return fmt.Errorf("client_protocol cannot be empty")
	default:
		return fmt.Errorf("unsupported client_protocol %q", tr.ClientProtocol)
	}
}
