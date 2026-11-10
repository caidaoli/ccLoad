package cursorauth

import (
	"context"
	"errors"
)

/**
 * @brief Cursor 上游报告的 token 用量；输入、缓存读写互不重叠，思考量包含在输出中。
 */
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	TotalTokens      int
	ReasoningTokens  int
}

/**
 * @brief 累加另一份已报告用量。
 * @param other 本次新增的用量，必须非 nil。
 * @note 修改接收者；调用方负责同步，不重复传入同一份累计用量。
 */
func (u *Usage) Add(other *Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.TotalTokens += other.TotalTokens
	u.ReasoningTokens += other.ReasoningTokens
}

/**
 * @brief Cursor 推理事件；Text 是当前正文，Delta 是新增正文，ThinkingDelta 是独立思考增量。
 * @details 工具边界可携带上下文估算；真实逐轮用量立即发出，终态只补齐累计用量的差额。
 * RawResponse 在启用捕获时携带单个 RunStreamMessage 的标准 protobuf JSON。
 */
type Event struct {
	Delta         string
	ThinkingDelta string ///< 上游思考文本增量，不并入正文或生成签名。
	Text          string
	ToolCall      *ToolCall
	Usage         *Usage
	// UsageEstimated marks a local context estimate. Estimated usage is exposed
	// to the client for context management and logged separately from real usage.
	// It must not be billed or included in actual token totals.
	UsageEstimated bool
	// Replayed marks a completed native run returned for a duplicate tool-result
	// request. The wire layer must not charge or log that run a second time.
	Replayed bool
	// Ping marks a live run heartbeat (SDK status RUNNING). It is not assistant text.
	Ping        bool
	Done        bool
	Err         error
	RawResponse []byte
}

type rawResponseCaptureContextKey struct{}

// WithRawResponseCapture asks the SDK runner to emit every RunStreamMessage
// as standard protobuf JSON before emitting its parsed inference events.
func WithRawResponseCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, rawResponseCaptureContextKey{}, true)
}

func rawResponseCaptureEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(rawResponseCaptureContextKey{}).(bool)
	return enabled
}

// Runner runs one Cursor inference.
type Runner interface {
	Run(ctx context.Context, credential *Credential, request Request) (<-chan Event, error)
}

// ModelLister returns the model IDs accepted by the Cursor SDK.
type ModelLister interface {
	ListModels(ctx context.Context, apiKey string) ([]string, error)
}

var (
	// ErrAgentMissing reports that the pinned bridge companion is unavailable.
	ErrAgentMissing = errors.New("cursor-sdk-bridge is not installed")
	// ErrMissingAPIKey reports a channel imported without its User API Key.
	ErrMissingAPIKey = errors.New("cursor credential is missing api_key")
	// ErrBridgeClosed reports use after the process owner began shutting down.
	ErrBridgeClosed = errors.New("cursor-sdk-bridge is closed")
	// ErrToolSessionNotFound reports a result for a suspended run that is no
	// longer owned by this ccLoad process.
	ErrToolSessionNotFound = errors.New("cursor tool session was not found")
)
