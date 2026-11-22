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
 * @details 真实逐轮用量立即发出，终态只补齐累计用量的差额。
 * RawResponse 在启用捕获时携带单个 RunStreamMessage 的标准 protobuf JSON。
 */
type Event struct {
	Delta         string
	ThinkingDelta string ///< 上游思考文本增量，不并入正文或生成签名。
	Text          string
	ToolCall      *ToolCall
	Usage         *Usage  ///< 上游报告的账单增量；上游未报告时为 nil。
	SentEffort    *string ///< 新 Send 首次确认模型执行时的档位；空字符串表示未指定档位。
	// Replayed marks a completed native run returned for a duplicate tool-result
	// request. The wire layer must not charge or log that run a second time.
	Replayed bool
	// Ping marks a live run heartbeat (SDK status RUNNING). It is not assistant text.
	Ping         bool
	Done         bool
	Err          error
	RawResponse  []byte
	terminalText bool ///< 终态补发的正文，须等成功 Done 才能确认模型执行。
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

/**
 * @brief Local Agent 的待发消息含 URL 图片，需要客户端改用内联图片或切换渠道。
 */
var ErrURLImageUnsupported = errors.New("use inline base64 images with Cursor Local Agent")

/**
 * @brief 同一工具调用的混合续轮仍在执行，不能再次向上游发送。
 */
var ErrToolTurnInProgress = errors.New("cursor tool continuation is still running")
