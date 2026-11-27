package cursorauth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
	"ccLoad/internal/util"
)

/**
 * @brief 通过共享的 Cursor SDK Bridge 执行请求并管理跨 HTTP 工具会话。
 */
type SDKRunner struct {
	bridge      *bridge
	idleTimeout time.Duration ///< 无附着请求的工具会话回收间隔；零值使用默认值。

	resumeMu       sync.Mutex
	mu             sync.Mutex
	closed         bool
	active         sync.WaitGroup
	sessions       map[string]*sdkSession
	calls          map[string]*pendingToolCall
	completedCalls map[string]*sdkSession
	catalogs       map[[sha256.Size]byte]cursorModelCatalog ///< API Key 指纹 → 模型目录；受 mu 保护
	parked         map[string]*parkedAgent                  ///< 停放键 → 已完成普通轮、可续发的 Agent；受 mu 保护

	callbackMu       sync.Mutex
	callbackServer   *http.Server
	callbackListener net.Listener
	callbackURL      string
	callbackToken    string
	callbackClient   *bridgeClient
}

/**
 * @brief 保留一个 Cursor Agent 的事件流、工具状态与当前请求的附着状态。
 */
type sdkSession struct {
	agentID            string
	apiKey             string
	scope              SessionScope ///< 创建 Agent 时确定的渠道、调用方与客户端会话归属。
	instructionsDigest string       ///< 创建 Run 时的显式指令摘要。
	model              string       ///< 启动本次 Run 的实际路由模型，保留 Cursor 模型名后缀。
	workdir            string
	client             sdkv1connect.SdkAgentServiceClient
	ctx                context.Context
	cancel             context.CancelCauseFunc
	events             chan Event
	done               chan struct{}
	tools              map[string]*sdkv1.CustomToolDefinition ///< 只包含创建 Agent 时实际注册的工具。
	toolPolicy         map[string]*sdkv1.CustomToolDefinition ///< Run 已收紧的权限；nil 表示沿用 tools。写入须持 turnMu 和 runner.mu。
	toolsDigest        string                                 ///< 自定义工具定义摘要，参与停放键。
	captureRaw         bool
	emitMu             sync.Mutex ///< 串行发布事件和终态，不阻塞附着时的会话元数据更新。
	eventMu            sync.Mutex
	transcriptDigest   string          ///< 最近附着请求的完整消息摘要；受 eventMu 保护。
	turnText           strings.Builder ///< 当前 HTTP 轮正文，含已排队的增量；受 eventMu 保护。
	terminal           *Event
	terminalAt         time.Time            ///< 终态发布时间；受 eventMu 保护。
	mixedToolTurn      *mixedCursorToolTurn ///< 替换本 Run 的混合续轮；受 runner.resumeMu 保护。
	parkedStart        chan error           ///< 续用时等待首个 SDK 帧、工具回调或启动错误；新 Agent 为 nil。
	parkedStartOnce    sync.Once            ///< 保证流读取与工具回调只报告一次续用结果。

	turnMu             sync.Mutex
	attached           bool
	serialTools        bool             ///< 每轮只向客户端公开一个工具调用；受 turnMu 保护。
	deliveredToolCalls []string         ///< 已交给客户端并等待整批结果的调用 ID；受 turnMu 保护。
	idleTimeout        time.Duration    ///< 工具轮之间等待下一请求的最长间隔。
	idleTimer          *time.Timer      ///< 仅在无附着请求时运行，受 turnMu 保护。
	onDetachedUsage    func(Usage)      ///< 工具轮解除附着时登记的用量接收方，受 turnMu 保护。
	turnReply          []Event          ///< 最近可重放 HTTP 轮的有序输出，受 turnMu 保护。
	nativeToolReply    *nativeToolReply ///< 生成当前响应的原生工具结果批次，受 turnMu 保护。
}

/**
 * @brief Cursor 会话的空闲回收间隔与已完成工具调用的重放保留期。
 */
const (
	sdkSessionIdleTimeout = 30 * time.Minute ///< 仅限制工具轮之间的等待，不限制活动请求。
	sdkCompletedCallsTTL  = 3 * time.Minute  ///< 完成后短期保留调用标识，支持重复结果重放。
)

/**
 * @brief 创建进程级 Cursor 推理执行器。
 * @param binaryPath 可选的、已在启动时校验的 Bridge 路径。
 * @return 管理共享 Bridge 和跨请求工具会话的执行器。
 */
func NewSDKRunner(binaryPath ...string) *SDKRunner {
	return &SDKRunner{
		bridge: newBridge(binaryPath...), idleTimeout: sdkSessionIdleTimeout,
		sessions: make(map[string]*sdkSession), calls: make(map[string]*pendingToolCall),
		completedCalls: make(map[string]*sdkSession),
	}
}

// Start eagerly launches and validates the managed bridge without requiring a
// model request. It is safe to call concurrently with Run or ListModels.
func (r *SDKRunner) Start(ctx context.Context) error {
	if r == nil || r.bridge == nil {
		return errors.New("cursor SDK runner is unavailable")
	}
	if err := r.begin(); err != nil {
		return err
	}
	defer r.active.Done()
	_, err := r.bridge.client(ctx)
	return err
}

/**
 * @brief 获取 Cursor 的模型 ID、变体和目录可确定的标称上下文窗口。
 * @param ctx 当前请求上下文；拉取受 RequestTimeout 限制。
 * @param apiKey 用于拉取目录的 User API Key，允许首尾空白。
 * @return 模型目录；请求失败时为零值。
 * @return 缺少凭证时返回 ErrMissingAPIKey；目录无有效模型或 Bridge 操作失败时返回对应错误。
 * @note 仅有效的目录会更新缓存。
 */
func (r *SDKRunner) ListModels(ctx context.Context, apiKey string) (ModelCatalog, error) {
	if r == nil || r.bridge == nil {
		return ModelCatalog{}, errors.New("cursor SDK runner is unavailable")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ModelCatalog{}, ErrMissingAPIKey
	}
	if err := r.begin(); err != nil {
		return ModelCatalog{}, err
	}
	defer r.active.Done()

	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	client, err := r.bridge.client(requestCtx)
	if err != nil {
		return ModelCatalog{}, err
	}
	response, err := listCursorModels(requestCtx, client, apiKey)
	if err != nil && isBridgeTransportFailure(err) {
		replacement, replaced, replacementErr := r.bridge.replacementClient(requestCtx, client)
		if replacementErr != nil {
			return ModelCatalog{}, replacementErr
		}
		if replaced {
			response, err = listCursorModels(requestCtx, replacement, apiKey)
		}
	}
	if err != nil {
		return ModelCatalog{}, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err, apiKey)
	}

	catalog := response.Msg.GetItems()
	if err := r.storeModelCatalog(sha256.Sum256([]byte(apiKey)), catalog); err != nil {
		return ModelCatalog{}, err
	}
	snapshot := ModelCatalog{
		Models:         make([]string, 0, len(catalog)*2),
		ContextWindows: make(map[string]int),
		EffortLevels:   make(map[string][]string),
		DefaultEfforts: make(map[string]string),
	}
	seen := make(map[string]struct{}, len(catalog)*2)
	add := func(id string, window int) {
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		snapshot.Models = append(snapshot.Models, id)
		if window > 0 {
			snapshot.ContextWindows[id] = window
		}
	}
	for _, item := range catalog {
		id := strings.TrimSpace(item.GetId())
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		defaultWindow := cursorDefaultContextWindow(item)
		add(id, defaultWindow)
		snapshot.recordCursorEfforts(id, item, nil)
		addCursorModelAliases(catalog, item, id, defaultWindow, add, &snapshot)
	}
	return snapshot, nil
}

func listCursorModels(
	ctx context.Context,
	client *bridgeClient,
	apiKey string,
) (*connect.Response[sdkv1.ListModelsResponse], error) {
	return client.cursor.ListModels(ctx, connect.NewRequest(&sdkv1.ListModelsRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: apiKey},
	}))
}

/**
 * @brief 恢复工具调用、重放已完成结果，或创建新的 Cursor Agent。
 * @param ctx 当前请求上下文；附着期间取消会终止 Agent，工具轮结束后解除绑定。
 * @param credential 含 User API Key 的 Cursor 凭证。
 * @param request 当前提示词、工具定义或客户端工具结果。
 * @return 当前请求的事件流；启动失败时为 nil。
 * @return 凭证、请求或 Bridge 操作错误；实际待发首选消息有 URL 图片时为 ErrURLImageUnsupported。
 * @note 启动后台事件消费；有网关身份的 Agent 在工具轮之间保留至空闲超时。
 */
func (r *SDKRunner) Run(
	ctx context.Context,
	credential *Credential,
	request Request,
) (<-chan Event, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	if credential == nil {
		return nil, ErrMissingAPIKey
	}
	apiKey := strings.TrimSpace(credential.APIKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	model := strings.TrimSpace(request.Model)
	if request.ToolChoice == "allowed_tools" && !request.toolChoiceNamed && request.allowedToolNames == nil {
		return nil, fmt.Errorf("%w: Cursor allowed_tools requires a valid list of custom functions", ErrInvalidRequest)
	}
	if request.SessionScope.reusable() && len(request.ToolResults) > 0 {
		events, err := r.resumeToolTurn(ctx, apiKey, model, request)
		if err != nil || events != nil {
			return events, err
		}
		// cursorTurnNew: trailing tool_result blocks are conversation history.
		// ParseRequest already inlined them into Prompt.
	}
	tools, err := customToolDefinitions(request, nil)
	if err != nil {
		return nil, err
	}
	events, _, err := r.runFreshTurn(ctx, apiKey, model, request, tools, false)
	return events, err
}

/**
 * @brief 用完整提示词创建 Agent，普通续轮可用匹配的已停放 Agent。
 * @param ctx 当前请求上下文。
 * @param apiKey 当前 Cursor User API Key。
 * @param model 实际路由模型；空时用默认模型。
 * @param request 当前轮完整历史和工具声明。
 * @param customTools 已按本轮权限选定、可注册到 Agent 的工具。
 * @param captureTurnReply 是否保留当前响应供相同混合续轮重试。
 * @return 本轮事件流；启动失败时为 nil。
 * @return 已启动的会话；启动失败时为 nil。
 * @return 请求验证或 Bridge 启动错误；成功时为 nil。
 */
func (r *SDKRunner) runFreshTurn(ctx context.Context, apiKey, model string, request Request, customTools map[string]*sdkv1.CustomToolDefinition, captureTurnReply bool) (<-chan Event, *sdkSession, error) {
	prompt := cursorRequestPrompt(request)
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, fmt.Errorf("%w: cursor prompt is required", ErrInvalidRequest)
	}
	if len(request.ToolResults) > 0 {
		prompt += "\n\n" + cursorSessionRecoveryNote
	}
	if model == "" {
		model = "default"
	}
	if err := r.begin(); err != nil {
		return nil, nil, err
	}
	defer r.active.Done()

	toolsDigest, err := customToolsDigest(customTools)
	if err != nil {
		return nil, nil, err
	}
	parkedKey, tail := parkedAgentContinuation(apiKey, toolsDigest, request)
	fullSendHasURLImage := hasURLImage(request.Images)
	if fullSendHasURLImage && !r.hasParkedAgent(parkedKey) {
		return nil, nil, ErrURLImageUnsupported
	}

	client, err := r.bridge.client(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(customTools) > 0 {
		if err := r.ensureToolCallback(ctx, client, apiKey); err != nil {
			return nil, nil, err
		}
	}
	selection, billingModel, err := r.modelSelection(ctx, client, apiKey, model, request.Effort)
	if err != nil {
		return nil, nil, err
	}
	run := sdkRunStart{
		client: client, apiKey: apiKey, scope: request.SessionScope, model: model, customTools: customTools, toolsDigest: toolsDigest,
		instructionsDigest: request.instructionsDigest,
		serialTools:        request.serialTools(false),
		transcriptDigest:   transcriptDigest(request.transcript), onDetachedUsage: request.OnDetachedUsage,
		message: prompt, images: request.Images, effort: cursorSentEffort(selection), billingModel: billingModel,
		captureTurnReply: captureTurnReply,
	}
	var agentID string
	if parkedKey != "" {
		agentID, err = r.takeParkedAgent(ctx, parkedKey, client.agent)
		if err != nil {
			return nil, nil, err
		}
	}
	if agentID != "" {
		continuation := run
		continuation.agentID = agentID
		continuation.message, continuation.images = renderTranscript(tail)
		continuation.sendModel = selection
		events, session, err := r.startRun(ctx, continuation)
		if err == nil || ctx.Err() != nil || !isBridgeAgentNotFound(err) {
			return events, session, err
		}
		if fullSendHasURLImage {
			return nil, nil, ErrURLImageUnsupported
		}
		log.Printf("[WARN] 续用 Cursor SDK Agent %s 失败，改为新建 Agent: %v", continuation.agentID, err)
	} else if fullSendHasURLImage {
		return nil, nil, ErrURLImageUnsupported
	}

	allowedTools := []string{}
	if len(customTools) > 0 {
		// Cursor exposes SDK custom tools through its synthetic MCP server.
		// An empty allowlist disables that MCP family along with every built-in.
		allowedTools = []string{"mcp"}
	}
	createStarted := time.Now()
	createCtx, createCancel := context.WithTimeout(ctx, RequestTimeout)
	created, err := client.agent.CreateAgent(createCtx, connect.NewRequest(&sdkv1.CreateAgentRequest{
		Options: &sdkv1.AgentOptions{
			Model:  selection,
			ApiKey: apiKey,
			Local: &sdkv1.LocalAgentOptions{
				Cwd: []string{client.workdir}, CustomTools: customTools,
			},
			Tools: &sdkv1.ToolList{Names: allowedTools},
		},
	}))
	if err != nil {
		classifiedErr := classifyBridgeOperationError("CreateAgent", createCtx, RequestTimeout, createStarted, err, apiKey)
		createCancel()
		return nil, nil, classifiedErr
	}
	createCancel()
	run.agentID = strings.TrimSpace(created.Msg.GetAgentId())
	if run.agentID == "" {
		return nil, nil, errors.New("cursor-sdk-bridge CreateAgent returned an empty agent_id")
	}
	return r.startRun(ctx, run)
}

/**
 * @brief 取得发给新 Agent 的请求正文，解析后的历史保留原始空白。
 * @param request 本轮请求。
 * @return 未附加工具恢复说明的正文。
 */
func cursorRequestPrompt(request Request) string {
	if len(request.transcript) > 0 {
		return request.Prompt
	}
	return strings.TrimSpace(request.Prompt)
}

/**
 * @brief 在已有 Agent 上发起一次 Run 所需的参数。
 */
type sdkRunStart struct {
	client             *bridgeClient
	agentID            string                                 ///< 新建或续用的 Agent
	apiKey             string                                 ///< 已去除首尾空白的 User API Key
	scope              SessionScope                           ///< 本轮不可变的网关与客户端会话归属
	instructionsDigest string                                 ///< 本轮显式指令摘要。
	serialTools        bool                                   ///< 本轮只向客户端公开一个工具调用。
	model              string                                 ///< 本轮实际路由模型，用于工具恢复和重放
	customTools        map[string]*sdkv1.CustomToolDefinition ///< Agent 创建时注册的客户端工具
	toolsDigest        string                                 ///< customTools 摘要，参与停放键
	transcriptDigest   string                                 ///< 当前请求完整消息摘要，参与停放键
	onDetachedUsage    func(Usage)                            ///< 工具会话晚到用量的接收方
	message            string                                 ///< 本次 Send 的提示词
	images             []Image                                ///< 本次 Send 的图片
	sendModel          *sdkv1.ModelSelection                  ///< 续用 Agent 时覆盖创建时的模型；新建时为 nil
	effort             string                                 ///< 本轮实际选择的思考档位，未指定时为空
	billingModel       string                                 ///< SDK 选择的 Cursor 自有模型与有效速度，供估算费用。
	captureTurnReply   bool                                   ///< 保留当前响应供混合续轮重试。
}

/**
 * @brief Run 因工具会话丢失而改为新建 Agent 时附加的提示。
 * @details 历史中的工具调用已执行，提示模型不要重复其外部副作用。
 */
const cursorSessionRecoveryNote = "HOST_SESSION_RECOVERY: The original tool session is no longer attachable. " +
	"The conversation above already includes every tool call and its exact result. " +
	"Treat each completed tool call as executed exactly once and do not repeat its external side effects; " +
	"call only genuinely new tools if the task still requires them."

/**
 * @brief 向 Agent 发送一轮消息并附着当前请求。
 * @param ctx 当前请求上下文；Send 期间取消会终止 Agent。
 * @param run Agent 与本轮消息。
 * @return 当前请求的事件流；Send 失败时为 nil。
 * @return 新建的会话；Send 失败时为 nil。
 * @return 会话注册或 Send 失败时返回已分类错误；失败时 Agent 已转交后台删除。
 * @note 调用方须持有 active 引用；成功时后台消费 Run，结束后删除或停放 Agent。
 */
func (r *SDKRunner) startRun(ctx context.Context, run sdkRunStart) (<-chan Event, *sdkSession, error) {
	client := run.client
	idleTimeout := r.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = sdkSessionIdleTimeout
	}
	// Native custom tools may suspend this run across multiple downstream HTTP
	// requests, so its lifetime cannot be derived from any one request context.
	runStarted := time.Now()
	runCtx, stopRun := context.WithCancelCause(context.Background())
	session := &sdkSession{
		agentID: run.agentID, apiKey: run.apiKey, scope: run.scope, model: run.model, workdir: client.workdir,
		instructionsDigest: run.instructionsDigest, serialTools: run.serialTools,
		client: client.agent, ctx: runCtx, cancel: stopRun,
		events: make(chan Event, 64), done: make(chan struct{}),
		tools:            run.customTools,
		toolsDigest:      run.toolsDigest,
		transcriptDigest: run.transcriptDigest,
		captureRaw:       rawResponseCaptureEnabled(ctx),
		idleTimeout:      idleTimeout,
	}
	if run.sendModel != nil {
		session.parkedStart = make(chan error, 1)
	}
	if err := r.registerSession(session); err != nil {
		stopRun(nil)
		r.active.Go(func() { r.deleteAgent(client.agent, run.agentID, client.workdir, run.apiKey) })
		return nil, nil, err
	}
	stopCaller := context.AfterFunc(ctx, func() { session.cancelRun(nil) })
	stream, err := client.agent.Send(runCtx, connect.NewRequest(&sdkv1.SendRequest{
		AgentId: run.agentID,
		Message: &sdkv1.UserMessage{Text: run.message, Images: sdkImages(run.images)},
		Options: &sdkv1.SendOptions{
			Model:        run.sendModel,
			EnableDeltas: false,
			EnableSteps:  false,
		},
	}))
	stopCaller()
	if err != nil {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		classifiedErr := classifyBridgeOperationError("Send", ctx, 0, runStarted, err, run.apiKey)
		session.abort(classifiedErr)
		r.unregisterSession(session)
		// A failed Send must return immediately; DeleteAgent/CloseAgent have
		// their own independent deadlines.
		if !isBridgeAgentNotFound(classifiedErr) {
			r.active.Go(func() { r.deleteAgent(client.agent, run.agentID, client.workdir, run.apiKey) })
		}
		return nil, nil, classifiedErr
	}

	effort := run.effort
	events, err := session.nextTurn(ctx, run.onDetachedUsage, &effort, run.captureTurnReply, nil)
	r.active.Add(1)
	go r.consumeRun(session, stream, run.billingModel)
	if session.parkedStart != nil {
		if startErr := <-session.parkedStart; startErr != nil {
			return nil, nil, startErr
		}
	}
	return events, session, err
}

/**
 * @brief 判断当前消息是否依赖 URL 图片源。
 * @param images 消息中按序出现的图片。
 * @return 存在未内联的图片时为 true，Local Agent 无法发送该消息。
 */
func hasURLImage(images []Image) bool {
	return slices.ContainsFunc(images, func(image Image) bool { return image.Data == "" })
}

/**
 * @brief 把内联图片转换为 Local Agent 的 SDK 消息图片。
 * @param images 提示词中按序出现的内联图片，调用前须检查图片源。
 * @return SDK 图片；无图片时为 nil。
 */
func sdkImages(images []Image) []*sdkv1.SdkImage {
	if len(images) == 0 {
		return nil
	}
	out := make([]*sdkv1.SdkImage, 0, len(images))
	for _, image := range images {
		out = append(out, &sdkv1.SdkImage{Source: &sdkv1.SdkImage_Data{
			Data: &sdkv1.SdkImageData{Data: image.Data, MimeType: image.MimeType},
		}})
	}
	return out
}

/**
 * @brief 解析非空模型选择之后的思考等级后缀。
 * @param model 已去除外层速度后缀的模型名。
 * @return 模型选择；无等级后缀时返回原名。
 * @return 后缀声明的思考等级；无后缀时为空。
 */
func splitCursorEffortSuffix(model string) (string, string) {
	lower := strings.ToLower(model)
	for _, suffix := range cursorEffortSuffixes {
		if strings.HasSuffix(lower, suffix) {
			if id := strings.TrimSpace(model[:len(model)-len(suffix)]); id != "" {
				return id, strings.TrimPrefix(strings.TrimPrefix(suffix, "-"), "thinking-")
			}
			break
		}
	}
	return model, ""
}

/**
 * @brief 查找目录中的原生模型，精确拼写优先。
 * @param catalog 当前凭证的模型目录。
 * @param id 客户端模型名或剥除一层后缀的模型名。
 * @return 匹配项；目录中无此模型时为 nil。
 */
func cursorCatalogModel(catalog []*sdkv1.SdkModel, id string) *sdkv1.SdkModel {
	var folded *sdkv1.SdkModel
	for _, item := range catalog {
		name := strings.TrimSpace(item.GetId())
		if name == id {
			return item
		}
		if folded == nil && strings.EqualFold(name, id) {
			folded = item
		}
	}
	return folded
}

/**
 * @brief 解析原生模型、目录变体或兼容后缀，并应用目录声明的思考参数。
 * @param ctx 当前请求上下文；用于按需拉取模型目录。
 * @param client 当前 Bridge 客户端。
 * @param apiKey User API Key。
 * @param model 渠道映射后的 Cursor 模型名。
 * @param requestEffort 请求体声明的思考等级；解析出的等级后缀优先。
 * @return 发给 CreateAgent 或续轮 Send 的模型选择。
 * @return Cursor 自有模型及计费速度；其它模型返回空字符串。
 * @return 需要消歧模型名或校验等级、但无法取得有效模型目录时返回错误。
 */
func (r *SDKRunner) modelSelection(
	ctx context.Context,
	client *bridgeClient,
	apiKey, model, requestEffort string,
) (*sdkv1.ModelSelection, string, error) {
	model = strings.TrimSpace(model)
	beforeSpeed, speed := splitCursorSpeedSuffix(model)
	base, suffixEffort := splitCursorEffortSuffix(beforeSpeed)
	hasContext := strings.Contains(beforeSpeed, "@")
	selection := &sdkv1.ModelSelection{Id: model}
	billingModel := util.CursorPricingModel(model)
	requiresCatalog := speed != "" || hasContext || suffixEffort != "" || requestEffort != ""
	if !requiresCatalog && billingModel == "" {
		return selection, "", nil
	}
	catalog, err := r.modelCatalog(ctx, client, apiKey)
	if err != nil {
		if requiresCatalog {
			return nil, "", err
		}
		// An unqualified model still works without model discovery; only the estimate loses speed information.
		return selection, billingModel, nil
	}
	item := cursorCatalogModel(catalog, model)
	if item == nil {
		item, selection.Params = resolveCursorModelOptions(catalog, beforeSpeed, speed)
		if item == nil && suffixEffort != "" {
			item, selection.Params = resolveCursorModelOptions(catalog, base, speed)
			requestEffort = suffixEffort
		}
		if speed == "false" && item != nil && selection.Params == nil {
			return selection, billingModel, nil
		}
	}
	if item != nil {
		selection.Id = strings.TrimSpace(item.GetId())
	} else if !hasContext && speed != "false" {
		// Manual -fast and effort aliases predate their declaration in the catalog.
		selection.Id = base
		if speed == "true" {
			selection.Params = []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}
		}
	}
	effortParams := cursorEffortParams(item, selection.Params, requestEffort)
	// Explicit default context aliases select the same window without sending it.
	if len(selection.Params) > 0 && strings.EqualFold(selection.Params[0].GetId(), "context") &&
		strings.EqualFold(selection.Params[0].GetValue(), cursorDefaultVariantParam(item, "context").GetValue()) {
		selection.Params = selection.Params[1:]
	}
	selection.Params = append(selection.Params, effortParams...)
	return selection, cursorSelectedPricingModel(selection, item), nil
}

/**
 * @brief 解析发给 SDK 的有效计费速度，包括原生速度 ID、显式参数和目录默认值。
 * @param selection 实际发送的模型与参数。
 * @param item 账号目录里的模型，未找到时为 nil。
 * @return Cursor 自有模型的计费名称；其它模型返回空字符串。
 */
func cursorSelectedPricingModel(selection *sdkv1.ModelSelection, item *sdkv1.SdkModel) string {
	model := util.CursorPricingModel(selection.GetId())
	if model == "" {
		return ""
	}
	for _, param := range selection.GetParams() {
		if strings.EqualFold(param.GetId(), "fast") {
			if strings.EqualFold(param.GetValue(), "true") {
				return strings.TrimSuffix(model, "-fast") + "-fast"
			}
			return strings.TrimSuffix(model, "-fast")
		}
	}
	if strings.HasSuffix(model, "-fast") {
		return model
	}
	beforeSpeed, speed := splitCursorSpeedSuffix(selection.GetId())
	beforeContext, _, _ := strings.Cut(beforeSpeed, "@")
	if _, nativeSpeed := splitCursorSpeedSuffix(beforeContext); speed == "" && nativeSpeed == "" &&
		strings.EqualFold(cursorSelectedDefaultSpeed(item, selection.GetParams()), "true") {
		return model + "-fast"
	}
	return model
}

/**
 * @brief 查询所选参数对应的默认速度，或该组合唯一声明的速度。
 * @param item 账号目录中的模型；缺少记录时速度未知。
 * @param params 发给 SDK 的显式模型参数，其中未指定速度。
 * @return 目录的 fast 值；没有速度声明时为空。
 */
func cursorSelectedDefaultSpeed(item *sdkv1.SdkModel, params []*sdkv1.ModelParameterValue) string {
	defaultSpeed := cursorDefaultVariantParam(item, "fast").GetValue()
	if len(params) == 0 {
		return defaultSpeed
	}
	var speed string
	var ambiguous bool
	for _, variant := range item.GetVariants() {
		if !slices.ContainsFunc(params, func(param *sdkv1.ModelParameterValue) bool {
			return !strings.EqualFold(cursorVariantParam(variant, param.GetId()).GetValue(), param.GetValue())
		}) {
			value := cursorVariantParam(variant, "fast").GetValue()
			if strings.EqualFold(value, defaultSpeed) {
				return defaultSpeed
			}
			ambiguous = ambiguous || (speed != "" && !strings.EqualFold(speed, value))
			speed = value
		}
	}
	if !ambiguous && speed != "" {
		return speed
	}
	return defaultSpeed
}

/**
 * @brief 已缓存的 Cursor 模型目录。
 */
type cursorModelCatalog struct {
	models    []*sdkv1.SdkModel ///< ListModels 返回的模型
	expiresAt time.Time         ///< 过期时刻
}

/**
 * @brief 模型目录缓存有效期；用于解析模型名与参数。
 */
const sdkModelCatalogTTL = 10 * time.Minute

/**
 * @brief 读取或拉取 API Key 对应的模型目录。
 * @param ctx 当前请求上下文；拉取受 RequestTimeout 限制。
 * @param client 当前 Bridge 客户端。
 * @param apiKey User API Key。
 * @return ListModels 返回的模型。
 * @return 拉取失败时返回已分类错误，目录无有效模型时返回空目录错误。
 * @note 并发安全；并发未命中会各自拉取一次。
 */
func (r *SDKRunner) modelCatalog(ctx context.Context, client *bridgeClient, apiKey string) ([]*sdkv1.SdkModel, error) {
	fingerprint := sha256.Sum256([]byte(apiKey))
	r.mu.Lock()
	cached, ok := r.catalogs[fingerprint]
	r.mu.Unlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.models, nil
	}
	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := listCursorModels(requestCtx, client, apiKey)
	if err != nil {
		classified := classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err, apiKey)
		// 模型选择只能短时使用该 Key 的旧参数，显式列目录和账号级错误仍返回实际结果。
		bridgeErr, typed := errors.AsType[*BridgeError](classified)
		if ok && ctx.Err() == nil && time.Now().Before(cached.expiresAt.Add(sdkModelCatalogTTL)) &&
			typed && bridgeErr.SDKCode == sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNSPECIFIED &&
			(bridgeErr.ConnectCode == connect.CodeUnavailable || bridgeErr.ConnectCode == connect.CodeDeadlineExceeded) {
			return cached.models, nil
		}
		return nil, classified
	}
	models := response.Msg.GetItems()
	if err := r.storeModelCatalog(fingerprint, models); err != nil {
		return nil, err
	}
	return models, nil
}

/**
 * @brief 缓存有效的模型目录并清理所有已过期的目录。
 * @param fingerprint User API Key 的 SHA-256 指纹。
 * @param models ListModels 返回的模型。
 * @return 目录不含任何非空模型 ID 时返回错误，保留现有缓存。
 * @note 并发安全。
 */
func (r *SDKRunner) storeModelCatalog(fingerprint [sha256.Size]byte, models []*sdkv1.SdkModel) error {
	if !slices.ContainsFunc(models, func(model *sdkv1.SdkModel) bool {
		return strings.TrimSpace(model.GetId()) != ""
	}) {
		return errors.New("cursor SDK returned an empty model catalog")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalogs == nil {
		r.catalogs = make(map[[sha256.Size]byte]cursorModelCatalog)
	}
	now := time.Now()
	for key, catalog := range r.catalogs {
		if !now.Before(catalog.expiresAt.Add(sdkModelCatalogTTL)) {
			delete(r.catalogs, key)
		}
	}
	r.catalogs[fingerprint] = cursorModelCatalog{models: models, expiresAt: now.Add(sdkModelCatalogTTL)}
	return nil
}

/**
 * @brief 生成 Cursor 目录支持的思考参数。
 * @param model ListModels 返回的目标模型；未登记时为 nil。
 * @param options 已选择的上下文和速度参数。
 * @param effort 请求的思考等级。
 * @return 参数名和值均沿用目录；未声明该等级时为 nil。
 * @details 关闭思考时优先使用 thinking=false；仅有布尔开关时 high 对应 true。
 */
func cursorEffortParams(model *sdkv1.SdkModel, options []*sdkv1.ModelParameterValue, effort string) []*sdkv1.ModelParameterValue {
	want := canonicalCursorEffort(effort)
	if want == "" {
		return nil
	}
	if want == "none" {
		if off, _ := cursorCatalogParamValue(model, "thinking", "false"); off != nil &&
			cursorVariantSupportsEffort(model, options, off) {
			return []*sdkv1.ModelParameterValue{off}
		}
	}
	var hasLevels bool
	for _, id := range []string{"effort", "reasoning_effort", "reasoning"} {
		value, declared := cursorCatalogParamValue(model, id, want)
		hasLevels = hasLevels || declared
		if value == nil {
			continue
		}
		if id != "reasoning" && want != "none" {
			if on, _ := cursorCatalogParamValue(model, "thinking", "true"); on != nil {
				if cursorVariantSupportsEffort(model, options, on, value) {
					return []*sdkv1.ModelParameterValue{on, value}
				}
				continue
			}
		}
		if cursorVariantSupportsEffort(model, options, value) {
			return []*sdkv1.ModelParameterValue{value}
		}
	}
	if !hasLevels && want == "high" {
		on, _ := cursorCatalogParamValue(model, "thinking", "true")
		off, _ := cursorCatalogParamValue(model, "thinking", "false")
		if on != nil && off != nil && cursorVariantSupportsEffort(model, options, on) {
			return []*sdkv1.ModelParameterValue{on}
		}
	}
	return nil
}

/**
 * @brief 读取 SDK 实际收到的思考档位。
 * @param selection 发给 CreateAgent 或续发 Send 的模型参数。
 * @return 参数或原生模型后缀指定的档位；无显式档位时为空。
 */
func cursorSentEffort(selection *sdkv1.ModelSelection) string {
	var toggle string
	for _, param := range selection.GetParams() {
		value := canonicalCursorEffort(param.GetValue())
		switch strings.ToLower(strings.TrimSpace(param.GetId())) {
		case "effort", "reasoning_effort", "reasoning":
			return value
		case "thinking":
			switch value {
			case "true":
				toggle = "high"
			case "false":
				toggle = "none"
			}
		}
	}
	_, suffix := splitCursorEffortSuffix(strings.TrimSuffix(strings.ToLower(selection.GetId()), "-fast"))
	return cmp.Or(toggle, canonicalCursorEffort(suffix))
}

/**
 * @brief 查找模型声明的 SDK 参数取值。
 * @param model ListModels 返回的目标模型；未登记时为 nil。
 * @param id SDK 参数名。
 * @param want 已规范化的取值。
 * @return 可直接发送的只读目录参数；未声明该值时为 nil。
 * @return 目录是否声明了该参数，包括仅声明其他值的情况。
 * @details 先查参数定义，再查变体，以兼容仅在变体中出现的取值。
 */
func cursorCatalogParamValue(model *sdkv1.SdkModel, id, want string) (*sdkv1.ModelParameterValue, bool) {
	var declared bool
	for _, parameter := range model.GetParameters() {
		if !strings.EqualFold(strings.TrimSpace(parameter.GetId()), id) {
			continue
		}
		declared = true
		for _, value := range parameter.GetValues() {
			if canonicalCursorEffort(value.GetValue()) == want {
				return &sdkv1.ModelParameterValue{Id: parameter.GetId(), Value: value.GetValue()}, true
			}
		}
	}
	for _, variant := range model.GetVariants() {
		for _, parameter := range variant.GetParams() {
			if !strings.EqualFold(strings.TrimSpace(parameter.GetId()), id) {
				continue
			}
			declared = true
			if canonicalCursorEffort(parameter.GetValue()) == want {
				return parameter, true
			}
		}
	}
	return nil, declared
}

/**
 * @brief 统一请求与目录中的同义参数值，供匹配使用。
 * @param effort 请求思考等级或目录参数值。
 * @return 小写匹配值；关闭思考统一为 none，额外高统一为 xhigh。
 */
func canonicalCursorEffort(effort string) string {
	switch effort = strings.ToLower(strings.TrimSpace(effort)); effort {
	case "extra-high", "extra_high":
		return "xhigh"
	case "off":
		return "none"
	default:
		return effort
	}
}

// Longer tokens first so -thinking-xhigh wins over -high and extra-high over high.
var cursorEffortSuffixes = []string{
	"-thinking-extra-high",
	"-thinking-xhigh",
	"-thinking-medium",
	"-thinking-minimal",
	"-thinking-high",
	"-thinking-none",
	"-thinking-low",
	"-thinking-max",
	"-extra-high",
	"-xhigh",
	"-medium",
	"-minimal",
	"-high",
	"-none",
	"-low",
	"-max",
}

type cursorTurnKind int

const (
	cursorTurnNew cursorTurnKind = iota
	cursorTurnResume
	cursorTurnReplay
)

type cursorToolTurn struct {
	kind    cursorTurnKind
	session *sdkSession
	pending []ToolResult
}

/**
 * @brief 跟踪一批已受理的工具结果和额外用户内容对应的替换 Run。
 * @note 生命周期由原 Run 的已完成调用记录管理；字段受 resumeMu 保护。
 */
type mixedCursorToolTurn struct {
	key         string                                 ///< 本批调用 ID 和额外用户内容的摘要。
	starting    bool                                   ///< 已保留发送资格，新 Run 正在准备。
	session     *sdkSession                            ///< 新 Run；启动失败时为空，允许再次尝试启动。
	inherited   map[string]*sdkv1.CustomToolDefinition ///< 原 Run 停用前的工具权限；供替换 Run 启动重试继承。
	tools       map[string]*sdkv1.CustomToolDefinition ///< 替换 Run 已验证的注册工具。
	serialTools bool                                   ///< 替换 Run 是否每轮只公开一个工具调用。
}

/**
 * @brief 关联已提交的工具调用 ID 与对应续轮的响应边界。
 */
type nativeToolReply struct {
	callIDs     []string                               ///< 为推进本轮而交付的工具调用 ID。
	ready       chan struct{}                          ///< 本轮已解除附着，可检查待处理回复或终态。
	tools       map[string]*sdkv1.CustomToolDefinition ///< 本轮开始前提交的有效工具权限，供重试核对。
	serialTools bool                                   ///< 本轮工具的串行设置，供重试核对。
}

/**
 * @brief 原 Run 工具结果获受理后，用完整历史与追加输入发起独立的新 Run。
 * @param ctx 当前请求上下文。
 * @param apiKey 当前 Cursor User API Key。
 * @param model 当前实际路由模型；省略时沿用原 Run 模型。
 * @param request 工具结果和新的用户内容。
 * @param original 已匹配的原 Run；未匹配时为 nil。
 * @param mixed 已保留发送资格的混合续轮；未匹配原 Run 时为 nil。
 * @return 新 Run 或相同混合轮可重放的事件；失败时为 nil。
 * @return 请求、启动或重放错误；混合轮正在执行时为 ErrToolTurnInProgress。
 */
func (r *SDKRunner) runMixedToolTurn(ctx context.Context, apiKey, model string, request Request, original *sdkSession, mixed *mixedCursorToolTurn) (<-chan Event, error) {
	var tools map[string]*sdkv1.CustomToolDefinition
	if mixed == nil {
		var err error
		tools, err = customToolDefinitions(request, nil)
		if err != nil {
			return nil, err
		}
	} else {
		if mixed.session != nil {
			return r.replayMixedToolTurn(ctx, mixed.session)
		}
		model = cmp.Or(model, original.model)
		tools = mixed.tools
		parallel := !mixed.serialTools
		request.ParallelToolCalls = &parallel
	}
	events, replacement, err := r.runFreshTurn(ctx, apiKey, model, request, tools, mixed != nil)
	if mixed != nil {
		r.resumeMu.Lock()
		mixed.session = replacement
		mixed.starting = false
		r.resumeMu.Unlock()
	}
	return events, err
}

/**
 * @brief 保留混合续轮的发送资格，并向原 Run 提交尚未交付的工具结果。
 * @param ctx 当前请求上下文。
 * @param model 当前实际路由模型；空时沿用原 Run 模型。
 * @param request 工具结果和新的用户内容。
 * @param turn 按调用 ID 分类后的工具续轮。
 * @return 可启动或与本请求相同的混合轮；仅凭历史启动时为空。
 * @return 上一混合轮仍在执行时为 ErrToolTurnInProgress，其他失败返回请求或提交错误。
 * @note 调用方须持有 resumeMu，且只在原 Run 未被请求附着时提交结果。
 */
func (r *SDKRunner) prepareMixedToolTurn(ctx context.Context, model string, request Request, turn cursorToolTurn) (*mixedCursorToolTurn, error) {
	if turn.kind == cursorTurnNew {
		return nil, nil
	}
	original := turn.session
	digest := sha256.New()
	writeDigestFields(digest, cmp.Or(model, original.model), request.Effort, cursorRequestPrompt(request),
		request.instructionsDigest, strconv.FormatBool(request.instructionsSet), request.ToolChoice, strconv.FormatBool(request.toolChoiceNamed), string(rawJSON(request.allowedToolNames)),
		string(rawJSON(request.Tools)), string(rawJSON(request.ToolResults)), string(rawJSON(request.ParallelToolCalls)), strconv.Itoa(len(request.Images)))
	for _, image := range request.Images {
		writeDigestFields(digest, image.MimeType, image.Data, image.URL)
	}
	key := string(digest.Sum(nil))
	mixed := original.mixedToolTurn
	if mixed != nil {
		if mixed.starting {
			return nil, ErrToolTurnInProgress
		}
		if mixed.session != nil {
			if key == mixed.key {
				return mixed, nil
			}
			select {
			case <-mixed.session.done:
			default:
				return nil, ErrToolTurnInProgress
			}
		}
	} else if turn.kind == cursorTurnReplay {
		select {
		case <-original.done:
		default:
			return nil, ErrToolTurnInProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if hasURLImage(request.Images) {
		return nil, ErrURLImageUnsupported
	}
	original.turnMu.Lock()
	defer original.turnMu.Unlock()
	inherited := original.effectiveTools()
	if mixed != nil {
		inherited = mixed.inherited
	}
	tools, err := customToolDefinitions(request, inherited)
	if err != nil {
		return nil, err
	}
	if turn.kind == cursorTurnResume {
		if original.attached {
			return nil, ErrToolTurnInProgress
		}
		if original.ctx.Err() != nil {
			return nil, ErrToolSessionNotFound
		}
		if err := r.resolveToolResults(original, turn.pending, nil, ""); err != nil {
			return nil, err
		}
		// Let the native callbacks receive their responses and the old Run
		// finish or collect late usage without holding up the fresh Send.
		original.armIdleTimerLocked(BridgeCleanupTimeout)
	}
	mixed = &mixedCursorToolTurn{key: key, starting: true, inherited: inherited, tools: tools, serialTools: request.serialTools(original.serialTools)}
	original.mixedToolTurn = mixed
	return mixed, nil
}

/**
 * @brief 重放相同混合续轮的终态或仍待交回结果的首个工具响应。
 * @param ctx 重试请求的上下文。
 * @param session 已匹配的替换 Run。
 * @return 可重放的事件流；没有可重放的响应时为 nil。
 * @return 当前响应不可重放时为 ErrToolTurnInProgress，重试已取消时返回取消原因。
 */
func (r *SDKRunner) replayMixedToolTurn(ctx context.Context, session *sdkSession) (<-chan Event, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	session.turnMu.Lock()
	events, err := r.toolReplayEventsLocked(session, session.nativeToolReply == nil)
	session.turnMu.Unlock()
	if err != nil {
		return nil, err
	}
	output := make(chan Event, len(events))
	for _, event := range events {
		output <- event
	}
	close(output)
	return output, nil
}

/**
 * @brief 取得已结束的终态或整批仍待交付结果的工具回复。
 * @param session 已匹配的 Run。
 * @param matchedTurn 重试是否对应最近的 HTTP 轮。
 * @return 去除计费用量的完整轮输出；冲突时为 nil。
 * @return 回复尚未完成、轮次已推进或工具结果不可提交时为 ErrToolTurnInProgress。
 * @note 调用方须持有 session.turnMu。返回的事件在发布后保持不变。
 */
func (r *SDKRunner) toolReplayEventsLocked(session *sdkSession, matchedTurn bool) ([]Event, error) {
	reply := session.turnReply
	if !matchedTurn || len(reply) == 0 {
		return nil, ErrToolTurnInProgress
	}
	if reply[len(reply)-1].Done {
		return reply, nil
	}
	if session.ctx.Err() != nil {
		return nil, ErrToolTurnInProgress
	}
	r.mu.Lock()
	missingCall := slices.ContainsFunc(reply, func(event Event) bool {
		return event.ToolCall != nil && r.calls[event.ToolCall.ID] == nil
	})
	r.mu.Unlock()
	if missingCall {
		return nil, ErrToolTurnInProgress
	}
	return reply, nil
}

/**
 * @brief 在已认证范围内恢复工具会话、重放已完成的工具轮或发起混合续轮。
 * @param ctx 当前请求上下文。
 * @param apiKey 已去除首尾空白的 User API Key。
 * @param model 当前实际路由模型；SDK 调用方省略模型时为空。
 * @param request 当前轮工具结果、历史与所有者身份。
 * @return 已有会话的事件流；需要新开会话时为 nil。
 * @return 扩充或更改工具定义返回 ErrInvalidRequest，重放权限冲突返回 ErrToolTurnInProgress，其他失败返回对应错误。
 * @note 分类与提交共享临界区，重复请求才能看到已提交结果。
 */
func (r *SDKRunner) resumeToolTurn(ctx context.Context, apiKey, model string, request Request) (<-chan Event, error) {
	// 分类与提交结果必须处于同一临界区，重复请求才能看到已提交状态并重放。
	r.resumeMu.Lock()
	turn, err := r.classifyCursorToolTurn(request.SessionScope, apiKey, model, request.ToolResults)
	if err != nil {
		r.resumeMu.Unlock()
		return nil, err
	}
	changedInstructions := turn.session != nil && request.instructionsSet && request.instructionsDigest != turn.session.instructionsDigest
	if request.hasToolSteering || changedInstructions {
		mixed, err := r.prepareMixedToolTurn(ctx, model, request, turn)
		r.resumeMu.Unlock()
		if err != nil {
			return nil, err
		}
		return r.runMixedToolTurn(ctx, apiKey, model, request, turn.session, mixed)
	}
	defer r.resumeMu.Unlock()
	switch turn.kind {
	case cursorTurnResume:
		session := turn.session
		return session.nextTurn(ctx, request.OnDetachedUsage, nil, false, func() (*nativeToolReply, error) {
			inherited := session.effectiveTools()
			tools, err := customToolDefinitions(request, inherited)
			if err != nil {
				return nil, err
			}
			if !toolDefinitionsSubset(tools, inherited) {
				return nil, fmt.Errorf("%w: a running Cursor tool turn cannot add or redefine tools", ErrInvalidRequest)
			}
			if err := context.Cause(ctx); err != nil {
				return nil, err
			}
			if err := r.resolveToolResults(session, turn.pending, tools, transcriptDigest(request.transcript)); err != nil {
				return nil, err
			}
			callIDs := make([]string, 0, len(turn.pending))
			for _, result := range turn.pending {
				callIDs = append(callIDs, strings.TrimSpace(result.CallID))
			}
			session.serialTools = request.serialTools(session.serialTools)
			return &nativeToolReply{callIDs: callIDs, ready: make(chan struct{}), tools: tools, serialTools: session.serialTools}, nil
		})
	case cursorTurnReplay:
		return r.replayNativeToolTurn(ctx, turn.session, request)
	default:
		return nil, nil
	}
}

/**
 * @brief 根据请求范围内可见的工具调用判断是否恢复或重放。
 * @param scope 当前渠道、已认证的调用方与客户端会话归属。
 * @param apiKey 已去除首尾空白的 User API Key。
 * @param model 当前实际路由模型；SDK 调用方省略模型时为空。
 * @param results 客户端附带的工具结果。
 * @return 匹配的会话与待提交结果；身份或模型不符以及不存在的调用按历史处理。
 * @return Bridge 关闭时为 ErrBridgeClosed；同时匹配范围内多个会话时为 ErrInvalidRequest。
 * @note 调用方须持有 resumeMu。
 */
func (r *SDKRunner) classifyCursorToolTurn(scope SessionScope, apiKey, model string, results []ToolResult) (cursorToolTurn, error) {
	var session *sdkSession
	pendingResults := make([]ToolResult, 0, len(results))
	unknown := 0
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return cursorToolTurn{}, ErrBridgeClosed
	}
	for _, result := range results {
		callID := strings.TrimSpace(result.CallID)
		var callSession *sdkSession
		if pending := r.calls[callID]; pending != nil && pending.session.matchesToolTurn(scope, apiKey, model) {
			callSession = pending.session
			pendingResults = append(pendingResults, result)
		}
		if callSession == nil {
			callSession = r.completedCalls[callID]
			if !callSession.matchesToolTurn(scope, apiKey, model) {
				unknown++
				continue
			}
		}
		if session != nil && session != callSession {
			return cursorToolTurn{}, fmt.Errorf("%w: tool results from different Cursor sessions cannot share one request", ErrInvalidRequest)
		}
		session = callSession
	}
	if unknown > 0 || session == nil {
		return cursorToolTurn{kind: cursorTurnNew}, nil
	}
	if len(pendingResults) == 0 {
		return cursorToolTurn{kind: cursorTurnReplay, session: session}, nil
	}
	return cursorToolTurn{kind: cursorTurnResume, session: session, pending: pendingResults}, nil
}

/**
 * @brief 判断工具结果是否属于同一网关身份、上游凭证与实际路由模型。
 * @param scope 当前渠道、已认证的调用方与客户端会话归属。
 * @param apiKey 已去除首尾空白的 User API Key。
 * @param model 实际路由模型；省略时沿用原 Run 的模型。
 * @return 身份与模型均匹配时为 true；未找到会话时为 false。
 */
func (s *sdkSession) matchesToolTurn(scope SessionScope, apiKey, model string) bool {
	return s != nil && s.scope == scope && s.apiKey == apiKey && (model == "" || s.model == model)
}

/**
 * @brief 等待匹配的原生续轮边界，再重放仍有效的工具回复或 Run 终态。
 * @param ctx 重试请求的上下文；取消时仅结束重放。
 * @param session 已匹配的 Run。
 * @param request 重试提供的已提交工具结果和工具权限。
 * @return 重放的事件流；续轮已推进或不可重放时报告 ErrToolTurnInProgress。
 * @return 已匹配的待重放续轮所用工具权限不同时返回 ErrToolTurnInProgress。
 */
func (r *SDKRunner) replayNativeToolTurn(ctx context.Context, session *sdkSession, request Request) (<-chan Event, error) {
	resultIDs := make(map[string]struct{}, len(request.ToolResults))
	for _, result := range request.ToolResults {
		resultIDs[strings.TrimSpace(result.CallID)] = struct{}{}
	}
	session.turnMu.Lock()
	reply := session.nativeToolReply
	if reply != nil && slices.ContainsFunc(reply.callIDs, func(callID string) bool {
		_, ok := resultIDs[callID]
		return !ok
	}) {
		reply = nil
	}
	if reply != nil {
		tools, err := customToolDefinitions(request, reply.tools)
		if err != nil || len(tools) != len(reply.tools) || !toolDefinitionsSubset(tools, reply.tools) || request.serialTools(reply.serialTools) != reply.serialTools {
			session.turnMu.Unlock()
			return nil, ErrToolTurnInProgress
		}
	}
	session.turnMu.Unlock()

	output := make(chan Event, 1)
	go func() {
		defer close(output)
		if reply != nil {
			select {
			case <-reply.ready:
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			output <- Event{Done: true, Err: context.Cause(ctx), Replayed: true}
			return
		}
		session.turnMu.Lock()
		events, err := r.toolReplayEventsLocked(session, reply != nil && session.nativeToolReply == reply)
		session.turnMu.Unlock()
		if err != nil {
			output <- Event{Done: true, Err: err, Replayed: true}
			return
		}
		for _, event := range events {
			select {
			case output <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return output, nil
}

/**
 * @brief 验证所有工具结果能够交付后，以原子操作提交同一会话的工具响应。
 * @param session 等待工具结果的会话。
 * @param results 待交付的客户端结果。
 * @param tools 新一轮允许的工具，须为本 Run 现有权限的子集；nil 表示禁用全部。
 * @param digest 当前请求的完整消息摘要；混合续轮保留空值，禁止停放原 Run。
 * @return 缺少本轮已交付的结果、提交多余或重复调用时为 ErrInvalidRequest；会话不再可用或编码失败时返回对应错误。
 * @note 调用方须持有 session.turnMu。
 */
func (r *SDKRunner) resolveToolResults(session *sdkSession, results []ToolResult, tools map[string]*sdkv1.CustomToolDefinition, digest string) error {
	values := make([]*structpb.Struct, len(results))
	for index, result := range results {
		value, err := callbackResult(result)
		if err != nil {
			return fmt.Errorf("encode result for Cursor custom tool %q: %w", result.CallID, err)
		}
		values[index] = value
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		callID := strings.TrimSpace(result.CallID)
		pending := r.calls[callID]
		if pending == nil || pending.session != session {
			return fmt.Errorf("%w: call_id %q", ErrToolSessionNotFound, result.CallID)
		}
		if _, duplicate := seen[callID]; duplicate {
			return fmt.Errorf("%w: tool result %q appears more than once", ErrInvalidRequest, result.CallID)
		}
		seen[callID] = struct{}{}
	}
	for _, callID := range session.deliveredToolCalls {
		if _, present := seen[callID]; !present {
			return fmt.Errorf("%w: submit all results from the Cursor tool turn together; missing call_id %q", ErrInvalidRequest, callID)
		}
	}
	if len(seen) != len(session.deliveredToolCalls) {
		return fmt.Errorf("%w: results include a tool call outside the Cursor tool turn", ErrInvalidRequest)
	}

	narrowed := len(tools) < len(session.effectiveTools())
	if narrowed {
		if tools == nil {
			tools = map[string]*sdkv1.CustomToolDefinition{}
		}
		session.toolPolicy = tools
	}
	session.eventMu.Lock()
	session.transcriptDigest = digest
	session.eventMu.Unlock()
	session.deliveredToolCalls = nil
	session.turnReply = nil
	session.nativeToolReply = nil
	if r.completedCalls == nil {
		r.completedCalls = make(map[string]*sdkSession)
	}
	for index, result := range results {
		callID := strings.TrimSpace(result.CallID)
		pending := r.calls[callID]
		delete(r.calls, callID)
		r.completedCalls[callID] = session
		pending.result <- toolCallbackResult{value: values[index]}
	}
	if narrowed {
		for callID, pending := range r.calls {
			if pending.session == session && tools[pending.name] == nil {
				delete(r.calls, callID)
				pending.result <- toolCallbackResult{err: errCustomToolsDisabled}
			}
		}
	}
	return nil
}

/**
 * @brief 将一个请求附着到 Agent，消费至终态或下一批客户端工具调用。
 * @param ctx 当前请求上下文；附着期间取消会终止 Agent。
 * @param onDetachedUsage 当前请求提供的迟到用量接收方；nil 表示无接收方。
 * @param sentEffort 新 Send 的实际思考档位；恢复工具轮时为 nil。
 * @param captureTurnReply 是否保留本轮响应供混合续轮重试。
 * @param before 提交工具结果并返回重放批次；提交成功后才完成附着，nil 表示无需提交。
 * @return 当前轮事件流；附着或提交失败时为 nil。
 * @return 已有附着请求或提交失败时返回错误，否则为 nil。
 * @note turnMu 保护附着与空闲计时器。已取消的 Run 由当前请求等待终态，关闭输出前解除请求取消绑定。
 * 无跨请求身份的 Run 在正常交付工具批次并解除附着后取消，剩余用量仍交给 onDetachedUsage。
 */
func (s *sdkSession) nextTurn(ctx context.Context, onDetachedUsage func(Usage), sentEffort *string, captureTurnReply bool, before func() (*nativeToolReply, error)) (<-chan Event, error) {
	s.turnMu.Lock()
	if s.attached {
		s.turnMu.Unlock()
		return nil, ErrToolTurnInProgress
	}
	var reply *nativeToolReply
	if before != nil {
		var err error
		reply, err = before()
		if err != nil {
			s.turnMu.Unlock()
			return nil, err
		}
		s.nativeToolReply = reply
	}
	toolPolicy := s.toolPolicy
	serialTools := s.serialTools
	s.attached = true
	s.onDetachedUsage = nil
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.turnMu.Unlock()
	stopCaller := context.AfterFunc(ctx, func() { s.cancelRun(nil) })
	var turnReply []Event
	var deliveredToolCalls []string
	var text strings.Builder
	release := func(atToolBoundary bool) bool {
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		if atToolBoundary {
			if s.ctx.Err() != nil || !stopCaller() {
				return false
			}
			s.onDetachedUsage = onDetachedUsage
			s.deliveredToolCalls = deliveredToolCalls
			s.eventMu.Lock()
			// Keep upstream text already queued for the next HTTP turn.
			queued := s.turnText.String()[text.Len():]
			s.turnText.Reset()
			s.turnText.WriteString(queued)
			s.eventMu.Unlock()
			if !s.scope.reusable() {
				s.cancel(nil)
			}
		} else {
			stopCaller()
		}
		s.turnReply = turnReply
		s.attached = false
		if s.ctx.Err() == nil {
			s.armIdleTimerLocked(s.idleTimeout)
		}
		return true
	}

	output := make(chan Event, 16)
	go func() {
		defer close(output)
		if reply != nil {
			defer close(reply.ready)
		}
		detached := false
		defer func() {
			if !detached {
				release(false)
			}
		}()
		var batchTimer *time.Timer
		var batch <-chan time.Time
		stopBatchTimer := func() {
			if batchTimer != nil && !batchTimer.Stop() {
				select {
				case <-batchTimer.C:
				default:
				}
			}
		}
		defer stopBatchTimer()
		cancelTurn := func(pending Event) {
			s.cancelRun(nil)
			final := Event{Text: text.String(), Done: true, Err: context.Cause(ctx)}
			final.mergeUsage(pending)
			<-s.done
			for {
				select {
				case event := <-s.events:
					final.mergeUsage(event)
				default:
					if !pending.Done {
						terminal, _ := s.terminalEvent()
						final.mergeUsage(terminal)
					}
					// This goroutine is the only sender. If the consumer stopped,
					// reclaim one queued slot and carry its usage into the terminal.
					select {
					case output <- final:
					default:
						select {
						case queued := <-output:
							final.mergeUsage(queued)
						default:
						}
						output <- final
					}
					return
				}
			}
		}
		deliver := func(event Event) bool {
			if event.ToolCall != nil && (serialTools && len(deliveredToolCalls) > 0 || toolPolicy != nil && toolPolicy[event.ToolCall.Name] == nil) {
				return true
			}
			if event.Delta != "" {
				text.WriteString(event.Delta)
			}
			if event.Delta != "" || event.Text != "" || event.Done {
				event.Text = text.String()
			}
			if event.Done && ctx.Err() != nil {
				event.Err = context.Cause(ctx)
			}
			if sentEffort != nil && ((event.Delta != "" && !event.terminalText) || event.ThinkingDelta != "" ||
				event.ToolCall != nil || (!event.Done && hasTokenUsage(event.Usage)) || (event.Done && event.Err == nil)) {
				event.SentEffort = sentEffort
				sentEffort = nil
			}
			select {
			case output <- event:
				if (captureTurnReply || reply != nil) && (event.Delta != "" || event.ThinkingDelta != "" || event.ToolCall != nil || event.Done) {
					turnReply = append(turnReply, Event{
						Delta: event.Delta, ThinkingDelta: event.ThinkingDelta, Text: event.Text,
						ToolCall: event.ToolCall, Done: event.Done, Err: event.Err, Replayed: true,
					})
				}
				if event.ToolCall != nil {
					deliveredToolCalls = append(deliveredToolCalls, event.ToolCall.ID)
					if !serialTools {
						stopBatchTimer()
						batchTimer = time.NewTimer(10 * time.Millisecond)
						batch = batchTimer.C
					}
				}
				return true
			case <-ctx.Done():
				cancelTurn(event)
				return false
			}
		}
		for {
			select {
			case event := <-s.events:
				if !deliver(event) {
					return
				}
				if event.Done {
					return
				}
				if serialTools && event.ToolCall != nil && len(deliveredToolCalls) == 1 {
					detached = release(true)
					if detached {
						return
					}
				}
			case <-batch:
				detached = release(true)
				if detached {
					return
				}
				batch = nil
			case <-s.done:
				for {
					select {
					case event := <-s.events:
						if !deliver(event) {
							return
						}
					default:
						if terminal, ok := s.terminalEvent(); ok {
							deliver(terminal)
						}
						return
					}
				}
			case <-ctx.Done():
				cancelTurn(Event{})
				return
			}
		}
	}()
	return output, nil
}

/**
 * @brief 调整未附着 Run 的回收时限。
 * @param timeout 从此刻起保留 Run 的时长。
 * @note 调用方须持有 turnMu；已启动的旧计时器回调不会取消本次保留。
 */
func (s *sdkSession) armIdleTimerLocked(timeout time.Duration) {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(timeout, func() {
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		if !s.attached && s.idleTimer == timer {
			s.idleTimer = nil
			s.cancel(nil)
		}
	})
	s.idleTimer = timer
}

/**
 * @brief 按序向尚未结束的会话发布一个事件。
 * @param event 要发布的 SDK 流事件或工具回调。
 * @return 事件入队时为 true；会话已结束或取消时可返回 false。
 * @note 与终态发布互斥；等待队列空间时不持有会话元数据锁。
 */
func (s *sdkSession) emit(event Event) bool {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	if event.Delta != "" {
		// Record text before it becomes visible to a turn that may detach.
		s.eventMu.Lock()
		s.turnText.WriteString(event.Delta)
		s.eventMu.Unlock()
	}
	select {
	case s.events <- event:
		if event.ToolCall != nil {
			s.confirmParkedStart(nil)
		}
		return true
	case <-s.ctx.Done():
		return false
	}
}

/**
 * @brief 确认一次停放 Agent 续发已开始，或报告开始前的拒绝。
 * @param err 首个 SDK 帧或工具回调到达时为 nil，否则为流启动错误。
 * @return 当前调用确实发布了确认结果时为 true。
 * @note 流读取与工具回调竞争报告时采用最先观察到的结果。
 */
func (s *sdkSession) confirmParkedStart(err error) bool {
	confirmed := false
	if s.parkedStart != nil {
		s.parkedStartOnce.Do(func() {
			s.parkedStart <- err
			confirmed = true
		})
	}
	return confirmed
}

/**
 * @brief 发布会话终态，将剩余真实用量交给附着请求或最后一轮提供的迟到用量接收方。
 * @param event 含终态补差的最终事件。
 * @note 会话取消后只调用一次；接收函数在锁外同步执行，计入 Run 的生命周期。
 */
func (s *sdkSession) finish(event Event) {
	s.turnMu.Lock()
	s.emitMu.Lock()
	var settle func(Usage)
	var detachedUsage Usage
	if s.onDetachedUsage != nil {
		settle = s.onDetachedUsage
		takeUsage := func(event *Event) {
			if event.Usage != nil {
				detachedUsage.Add(event.Usage)
				event.Usage = nil
				event.UsageIncrements = nil
			}
		}
		takeUsage(&event)
		for range len(s.events) {
			queued := <-s.events
			takeUsage(&queued)
			s.events <- queued
		}
	}
	s.onDetachedUsage = nil
	s.eventMu.Lock()
	s.terminal = &event
	s.terminalAt = time.Now()
	s.eventMu.Unlock()
	close(s.done)
	s.emitMu.Unlock()
	s.turnMu.Unlock()
	if hasTokenUsage(&detachedUsage) {
		settle(detachedUsage)
	}
}

func (s *sdkSession) terminalEvent() (Event, bool) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminal == nil {
		return Event{}, false
	}
	return *s.terminal, true
}

/**
 * @brief 取消 Agent 并停止工具轮之间的空闲回收计时器。
 * @param cause 终止原因；nil 使用 context.Canceled。
 * @note 并发安全；可重复调用。
 */
func (s *sdkSession) cancelRun(cause error) {
	s.turnMu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.cancel(cause)
	s.turnMu.Unlock()
}

/**
 * @brief 取消 Agent 并立即发布调用方指定的终态错误。
 * @param err 终止会话的原因。
 */
func (s *sdkSession) abort(err error) {
	s.cancelRun(err)
	s.finish(Event{Done: true, Err: err})
}

/**
 * @brief 取得 Runner 为当前工具轮保留的权限。
 * @return 收紧后的工具定义，尚未收紧时为注册时的定义。
 * @note 调用方须持有 runner.mu 或 session.turnMu；返回的映射不可变。
 */
func (s *sdkSession) effectiveTools() map[string]*sdkv1.CustomToolDefinition {
	if s.toolPolicy != nil {
		return s.toolPolicy
	}
	return s.tools
}

func (r *SDKRunner) registerSession(session *sdkSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	if r.sessions == nil {
		r.sessions = make(map[string]*sdkSession)
	}
	if _, exists := r.sessions[session.agentID]; exists {
		return fmt.Errorf("cursor SDK returned duplicate agent_id %q", session.agentID)
	}
	r.sessions[session.agentID] = session
	return nil
}

func (r *SDKRunner) unregisterSession(session *sdkSession) {
	r.mu.Lock()
	if r.sessions[session.agentID] == session {
		delete(r.sessions, session.agentID)
	}
	r.mu.Unlock()
}

func (r *SDKRunner) sessionByAgentID(agentID string) *sdkSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[agentID]
}

/**
 * @brief 仅登记当前策略允许的回调，与结果提交及权限收紧互斥。
 * @param callID 网关生成的全局调用 ID。
 * @param pending 绑定到原 Run 的回调。
 * @return Runner 关闭时为 ErrBridgeClosed，权限已撤销时为 errCustomToolsDisabled，ID 冲突时为普通错误。
 */
func (r *SDKRunner) registerToolCall(callID string, pending *pendingToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	if pending.session.effectiveTools()[pending.name] == nil {
		return errCustomToolsDisabled
	}
	if r.calls == nil {
		r.calls = make(map[string]*pendingToolCall)
	}
	if _, exists := r.calls[callID]; exists {
		return fmt.Errorf("duplicate Cursor custom-tool call_id %q", callID)
	}
	r.calls[callID] = pending
	return nil
}

func (r *SDKRunner) unregisterToolCall(callID string, pending *pendingToolCall) {
	r.mu.Lock()
	if r.calls[callID] == pending {
		delete(r.calls, callID)
	}
	r.mu.Unlock()
}

/**
 * @brief 取消当前 SDK 会话，交由各 Run 收齐已收到的真实用量并发布错误终态。
 * @param err 终止会话的原因。
 */
func (r *SDKRunner) failAllSessions(err error) {
	r.mu.Lock()
	sessions := make([]*sdkSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()
	for _, session := range sessions {
		session.cancelRun(err)
	}
}

func (r *SDKRunner) begin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
	}
	r.active.Add(1)
	return nil
}

/**
 * @brief 消费 Cursor Agent 的后台流并收尾用量、终态和资源清理。
 * @param session 当前 Agent 会话。
 * @param stream Send 返回的上游事件流。
 * @param billingModel 此 Run 的 Cursor 计费模型和速度；无 Cursor 自有价目时为空。
 * @note 持有一个 active 引用；取消和删除 Agent 使用独立清理预算；正常完成的 Agent 停放供普通续轮复用。
 */
func (r *SDKRunner) consumeRun(
	session *sdkSession,
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage],
	billingModel string,
) {
	defer r.active.Done()
	runCtx := session.ctx
	client := session.client
	agentID := session.agentID
	canceller := &runCanceller{client: client, agentID: agentID, apiKey: session.apiKey}
	state := &sdkRunState{
		agentID: agentID,
		onRunID: func(runID string) {
			canceller.SetRunID(runID)
		},
		onTerminal: canceller.Terminal,
	}
	runCallbackDone := make(chan struct{})
	stopRunCallback := context.AfterFunc(runCtx, func() {
		canceller.Request()
		close(runCallbackDone)
	})

	var undelivered Event
	deliverEvent := func(event Event) {
		if hasTokenUsage(event.Usage) {
			event.BillingModel = billingModel
		}
		if !session.emit(event) {
			undelivered.mergeUsage(event)
		}
	}
	var consumeErr error
	for stream.Receive() {
		session.confirmParkedStart(nil)
		message := stream.Msg()
		if session.captureRaw {
			raw, marshalErr := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(redactSDKDebugMessage(message, session.apiKey))
			if marshalErr == nil {
				deliverEvent(Event{RawResponse: raw})
			}
		}
		batch, err := state.consume(message)
		if err != nil {
			consumeErr = err
			break
		}
		for i := range batch {
			deliverEvent(batch[i])
		}
	}
	session.eventMu.Lock()
	turnText := session.turnText.String()
	session.eventMu.Unlock()
	if consumeErr == nil {
		consumeErr = state.finalError(stream.Err(), turnText)
	}
	if closeErr := stream.Close(); closeErr != nil && consumeErr == nil {
		consumeErr = classifyBridgeError(closeErr)
	}
	if consumeErr == nil {
		var delta string
		delta, consumeErr = cursorFinalTextDelta(turnText, state.finalText)
		if delta != "" {
			deliverEvent(Event{Delta: delta, terminalText: true})
		}
	}
	if consumeErr != nil {
		canceller.Request()
	}

	// Usage lookup is part of the run and must stop when its caller cancels.
	// Keep the session alive until the lookup completes; agent deletion below
	// retains its independent cleanup context.
	if runCtx.Err() == nil && !hasTokenUsage(state.usage) {
		if usage := loadRunUsage(runCtx, client, state.runID, agentID, session.workdir, session.apiKey); hasTokenUsage(usage) {
			state.usage = usage
		}
	}
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED && runCtx.Err() != nil {
		consumeErr = context.Cause(runCtx)
	}
	session.cancelRun(nil)
	if cause := context.Cause(runCtx); cause != context.Canceled {
		consumeErr = cause
	}
	if stopRunCallback() {
		close(runCallbackDone)
	} else {
		<-runCallbackDone
	}
	finalEvent := Event{
		Done: true, Err: redactBridgeError(consumeErr, session.apiKey), Usage: state.unreportedUsage(),
	}
	finalEvent.mergeUsage(undelivered)
	if hasTokenUsage(finalEvent.Usage) {
		finalEvent.BillingModel = billingModel
	}
	session.finish(finalEvent)
	r.unregisterSession(session)
	missingBeforeStart := session.confirmParkedStart(finalEvent.Err) && isBridgeAgentNotFound(finalEvent.Err)
	r.expireCompletedToolTurns(session)

	// Cancellation and durable Agent deletion are cleanup, not response work.
	// Keep them tracked by r.active for orderly shutdown, but never make the
	// client or request-duration metric wait behind their independent deadlines.
	canceller.Wait()
	if missingBeforeStart || (consumeErr == nil && r.parkAgent(session)) {
		return
	}
	r.deleteAgent(client, agentID, session.workdir, session.apiKey)
}

/**
 * @brief 在重放保留期后移除已完成 Agent 的工具调用标识。
 * @param session 已完成的 Agent 会话。
 * @note 混合续轮在替换 Run 完成后才开始重放保留期。
 */
func (r *SDKRunner) expireCompletedToolTurns(session *sdkSession) {
	var expire func()
	expire = func() {
		r.resumeMu.Lock()
		defer r.resumeMu.Unlock()
		if mixed := session.mixedToolTurn; mixed != nil {
			remaining := time.Duration(0)
			switch {
			case mixed.starting:
				remaining = sdkCompletedCallsTTL
			case mixed.session != nil:
				mixed.session.eventMu.Lock()
				finishedAt := mixed.session.terminalAt
				mixed.session.eventMu.Unlock()
				if finishedAt.IsZero() {
					remaining = sdkCompletedCallsTTL
				} else {
					remaining = time.Until(finishedAt.Add(sdkCompletedCallsTTL))
				}
			}
			if remaining > 0 {
				time.AfterFunc(remaining, expire)
				return
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for callID, completedSession := range r.completedCalls {
			if completedSession == session {
				delete(r.completedCalls, callID)
			}
		}
	}
	time.AfterFunc(sdkCompletedCallsTTL, expire)
}

func loadRunUsage(
	parentCtx context.Context,
	client sdkv1connect.SdkAgentServiceClient,
	runID, agentID, workdir, apiKey string,
) *Usage {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(parentCtx, BridgeCleanupTimeout)
	defer cancel()
	response, err := client.GetRun(ctx, connect.NewRequest(&sdkv1.GetRunRequest{
		RunId: runID,
		Options: &sdkv1.GetRunOptions{
			Runtime: sdkv1.Runtime_RUNTIME_LOCAL,
			Cwd:     workdir,
			AgentId: agentID,
			ApiKey:  apiKey,
		},
	}))
	if err != nil || response == nil || response.Msg.GetRun() == nil {
		return nil
	}
	run := response.Msg.GetRun()
	if strings.TrimSpace(run.GetRunId()) != runID {
		return nil
	}
	if snapshotAgentID := strings.TrimSpace(run.GetAgentId()); snapshotAgentID != "" && snapshotAgentID != agentID {
		return nil
	}
	return usageFromSDK(run.GetUsage())
}

func hasTokenUsage(usage *Usage) bool {
	return usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 ||
		usage.TotalTokens > 0 || usage.ReasoningTokens > 0)
}

/**
 * @brief 删除 Agent；失败时尝试释放 Bridge 中的 Agent 实例。
 * @param client 创建 Agent 的 Bridge 客户端。
 * @param agentID 要清理的 Agent ID。
 * @param workdir Agent 的工作目录。
 * @param apiKey 请求使用的 User API Key。
 * @note 清理使用独立超时；RPC 失败时记已遮蔽凭证的诊断。
 */
func (r *SDKRunner) deleteAgent(
	client sdkv1connect.SdkAgentServiceClient,
	agentID, workdir, apiKey string,
) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
	defer cancel()
	_, err := client.DeleteAgent(cleanupCtx, connect.NewRequest(&sdkv1.DeleteAgentRequest{
		AgentId: agentID,
		Options: &sdkv1.AgentOperationOptions{Cwd: workdir, ApiKey: apiKey},
	}))
	if err != nil {
		log.Printf("[WARN] 删除 Cursor SDK Agent %s 失败: %v", agentID, redactBridgeError(classifyBridgeError(err), apiKey))
		closeCtx, closeCancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
		_, closeErr := client.CloseAgent(closeCtx, connect.NewRequest(&sdkv1.CloseAgentRequest{
			AgentId: agentID,
		}))
		closeCancel()
		if closeErr != nil {
			log.Printf("[WARN] 释放 Cursor SDK Agent %s 失败: %v", agentID, redactBridgeError(classifyBridgeError(closeErr), apiKey))
		}
	}
}

// Close waits for active runs and terminates the managed bridge process.
func (r *SDKRunner) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	r.deleteParkedAgentsLocked()
	r.mu.Unlock()
	r.failAllSessions(ErrBridgeClosed)

	done := make(chan struct{})
	go func() {
		r.active.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		r.closeToolCallbackServer(ctx)
		_ = r.bridge.close(ctx)
		return context.Cause(ctx)
	case <-done:
	}
	r.closeToolCallbackServer(ctx)
	return r.bridge.close(ctx)
}

func (r *SDKRunner) closeToolCallbackServer(ctx context.Context) {
	r.callbackMu.Lock()
	server := r.callbackServer
	r.callbackServer = nil
	r.callbackListener = nil
	r.callbackClient = nil
	r.callbackMu.Unlock()
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}
}

/**
 * @brief 在已知 Run ID 后最多发起一次后台取消 RPC。
 */
type runCanceller struct {
	client  sdkv1connect.SdkAgentServiceClient
	agentID string
	apiKey  string ///< 当前 Run 的 User API Key，用于遮蔽 RPC 诊断。

	mu        sync.Mutex
	runID     string
	requested bool
	issued    bool
	terminal  bool
	active    sync.WaitGroup
}

func (c *runCanceller) Request() {
	c.mu.Lock()
	c.requested = true
	c.issueLocked()
	c.mu.Unlock()
}

func (c *runCanceller) SetRunID(runID string) {
	c.mu.Lock()
	if c.runID == "" {
		c.runID = runID
	}
	c.issueLocked()
	c.mu.Unlock()
}

func (c *runCanceller) Terminal(runID string) {
	c.mu.Lock()
	if c.runID == "" {
		c.runID = runID
	}
	c.terminal = true
	c.mu.Unlock()
}

/**
 * @brief 条件满足时登记并执行 Run 的后台取消。
 * @note 调用方持有 mu；RPC 失败时记已遮蔽凭证的诊断。
 */
func (c *runCanceller) issueLocked() {
	if !c.requested || c.issued || c.terminal || c.runID == "" {
		return
	}
	c.issued = true
	runID := c.runID
	c.active.Go(func() {
		cancelCtx, cancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
		defer cancel()
		_, err := c.client.CancelRun(cancelCtx, connect.NewRequest(&sdkv1.CancelRunRequest{
			RunId: runID, AgentId: &c.agentID,
		}))
		if err != nil {
			log.Printf("[WARN] 取消 Cursor SDK Run %s 失败: %v", runID, redactBridgeError(classifyBridgeError(err), c.apiKey))
		}
	})
}

func (c *runCanceller) Wait() {
	c.active.Wait()
}
