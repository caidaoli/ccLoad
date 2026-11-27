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
	Delta           string
	ThinkingDelta   string ///< 上游思考文本增量，不并入正文或生成签名。
	Text            string
	ToolCall        *ToolCall
	Usage           *Usage  ///< 上游报告的账单增量；上游未报告时为 nil。
	UsageIncrements []Usage ///< 终态合并多份增量时保留已知轮次，合计等于 Usage。
	BillingModel    string  ///< 本次 SDK 模型选择对应的 Cursor 计费速度；由真实用量事件携带。
	SentEffort      *string ///< 新 Send 首次确认模型执行时的档位；空字符串表示未指定档位。
	Replayed        bool    ///< 重复工具请求重放的响应，代理层不重复计费或记日志。
	// Ping marks a live run heartbeat (SDK status RUNNING). It is not assistant text.
	Ping         bool
	Done         bool
	Err          error
	RawResponse  []byte
	terminalText bool ///< 终态补发的正文，须等成功 Done 才能确认模型执行。
}

/**
 * @brief 合并未交付事件的用量，同时保留每份已知增量的计费边界。
 * @param other 尚未交付的事件，终态可能已包含多份已知增量。
 * @note 仅用于合成终态；单份终态补差仍作为一份用量。
 */
func (e *Event) mergeUsage(other Event) {
	if !hasTokenUsage(other.Usage) {
		return
	}
	if e.BillingModel == "" {
		e.BillingModel = other.BillingModel
	}
	if len(e.UsageIncrements) == 0 && hasTokenUsage(e.Usage) {
		e.UsageIncrements = append(e.UsageIncrements, *e.Usage)
	}
	if len(other.UsageIncrements) > 0 {
		e.UsageIncrements = append(e.UsageIncrements, other.UsageIncrements...)
	} else {
		e.UsageIncrements = append(e.UsageIncrements, *other.Usage)
	}
	if e.Usage == nil {
		e.Usage = &Usage{}
	}
	e.Usage.Add(other.Usage)
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

/**
 * @brief 当前 Cursor 账号的模型目录、标称上下文窗口和思考档位。
 */
type ModelCatalog struct {
	Models         []string            ///< 可发现的原生模型与生成的别名，按目录顺序。
	ContextWindows map[string]int      ///< 按实际模型 ID 记录 token 数；缺失时未知，可包含已保存的默认窗口别名。
	DefaultEfforts map[string]string   ///< 目录明确标记且在该选择下可用的默认思考档位。
	EffortLevels   map[string][]string ///< 按实际模型 ID 记录有序的规范思考档位；空列表为无档位，缺失为未知。
}

/**
 * @brief 通过 Cursor SDK 拉取模型目录。
 */
type ModelLister interface {
	/**
	 * @brief 查询 User API Key 对应账号的模型目录。
	 * @param ctx 请求上下文。
	 * @param apiKey Cursor User API Key。
	 * @return 当前账号的模型目录；请求失败时为零值。
	 * @return 无法拉取有效目录时返回错误。
	 */
	ListModels(ctx context.Context, apiKey string) (ModelCatalog, error)
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
 * @brief Cursor 工具续轮仍在执行、已推进或与已保存的回复冲突。
 */
var ErrToolTurnInProgress = errors.New("cursor tool continuation is still running")
