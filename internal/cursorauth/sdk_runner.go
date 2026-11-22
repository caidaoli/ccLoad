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
	agentID          string
	apiKey           string
	scope            SessionScope ///< 创建 Agent 的渠道和调用方，整个会话不可变。
	model            string       ///< 启动本次 Run 的实际路由模型，保留 Cursor 模型名后缀。
	workdir          string
	client           sdkv1connect.SdkAgentServiceClient
	ctx              context.Context
	cancel           context.CancelCauseFunc
	events           chan Event
	done             chan struct{}
	tools            map[string]*sdkv1.CustomToolDefinition ///< 只包含创建 Agent 时实际注册的工具。
	toolsDisabled    chan struct{}                          ///< 禁用本 Run 后续工具时关闭；关闭前提交本轮已有的工具结果。
	toolsDigest      string                                 ///< 自定义工具定义摘要，参与停放键。
	captureRaw       bool
	emitMu           sync.Mutex ///< 串行发布事件和终态，不阻塞附着时的会话元数据更新。
	eventMu          sync.Mutex
	transcriptDigest string          ///< 最近附着请求的完整消息摘要；受 eventMu 保护。
	turnText         strings.Builder ///< 最近附着请求收到的正文；受 eventMu 保护。
	terminal         *Event
	terminalAt       time.Time            ///< 终态发布时间；受 eventMu 保护。
	mixedToolTurn    *mixedCursorToolTurn ///< 替换本 Run 的混合续轮；受 runner.resumeMu 保护。
	parkedStart      chan error           ///< 续用时等待首个 SDK 帧、工具回调或启动错误；新 Agent 为 nil。
	parkedStartOnce  sync.Once            ///< 保证流读取与工具回调只报告一次续用结果。

	turnMu          sync.Mutex
	attached        bool
	idleTimeout     time.Duration ///< 工具轮之间等待下一请求的最长间隔。
	idleTimer       *time.Timer   ///< 仅在无附着请求时运行，受 turnMu 保护。
	onDetachedUsage func(Usage)   ///< 工具轮解除附着时登记的用量接收方，受 turnMu 保护。
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
 * @brief 获取 Cursor 的模型 ID 及 SDK 支持的 -fast 变体。
 * @param ctx 当前请求上下文；拉取受 RequestTimeout 限制。
 * @param apiKey 用于拉取目录的 User API Key，允许首尾空白。
 * @return 模型 ID 与支持的变体；请求失败时为 nil。
 * @return 缺少凭证时返回 ErrMissingAPIKey；目录为空或 Bridge 操作失败时返回对应错误。
 * @note 成功拉取时更新模型目录缓存。
 */
func (r *SDKRunner) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.active.Done()

	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	client, err := r.bridge.client(requestCtx)
	if err != nil {
		return nil, err
	}
	response, err := listCursorModels(requestCtx, client, apiKey)
	if err != nil && isBridgeTransportFailure(err) {
		replacement, replaced, replacementErr := r.bridge.replacementClient(requestCtx, client)
		if replacementErr != nil {
			return nil, replacementErr
		}
		if replaced {
			response, err = listCursorModels(requestCtx, replacement, apiKey)
		}
	}
	if err != nil {
		return nil, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err, apiKey)
	}

	r.storeModelCatalog(sha256.Sum256([]byte(apiKey)), response.Msg.GetItems())
	models := make([]string, 0, len(response.Msg.GetItems())*2)
	seen := make(map[string]struct{}, len(response.Msg.GetItems())*2)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	for _, item := range response.Msg.GetItems() {
		id := strings.TrimSpace(item.GetId())
		if id == "" {
			continue
		}
		add(id)
		alias := id + "-fast"
		fast, _ := cursorCatalogParamValue(item, "fast", "true")
		if !strings.HasSuffix(strings.ToLower(id), "-fast") && fast != nil &&
			cursorCatalogModel(response.Msg.GetItems(), alias) == nil {
			add(alias)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("cursor SDK returned an empty model catalog")
	}
	return models, nil
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
	if request.SessionScope.reusable() && len(request.ToolResults) > 0 {
		if request.hasToolSteering {
			return r.runMixedToolTurn(ctx, apiKey, model, request)
		}
		events, err := r.resumeToolTurn(ctx, apiKey, model, request)
		if err != nil || events != nil {
			return events, err
		}
		// cursorTurnNew: trailing tool_result blocks are conversation history.
		// ParseRequest already inlined them into Prompt.
	}
	events, _, err := r.runFreshTurn(ctx, apiKey, model, request, nil)
	return events, err
}

/**
 * @brief 用完整提示词创建 Agent，普通续轮可用匹配的已停放 Agent。
 * @param ctx 当前请求上下文。
 * @param apiKey 当前 Cursor User API Key。
 * @param model 实际路由模型；空时用默认模型。
 * @param request 当前轮完整历史和工具声明。
 * @param inherited 已匹配的原 Run 注册的工具；只有请求省略工具声明时可继承。
 * @return 本轮事件流；启动失败时为 nil。
 * @return 已启动的会话；启动失败时为 nil。
 * @return 请求验证或 Bridge 启动错误；成功时为 nil。
 */
func (r *SDKRunner) runFreshTurn(ctx context.Context, apiKey, model string, request Request, inherited map[string]*sdkv1.CustomToolDefinition) (<-chan Event, *sdkSession, error) {
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

	customTools, err := customToolDefinitions(request, inherited)
	if err != nil {
		return nil, nil, err
	}
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
	selection, err := r.modelSelection(ctx, client, apiKey, model, request.Effort)
	if err != nil {
		return nil, nil, err
	}
	run := sdkRunStart{
		client: client, apiKey: apiKey, scope: request.SessionScope, model: model, customTools: customTools, toolsDigest: toolsDigest,
		transcriptDigest: transcriptDigest(request.transcript), onDetachedUsage: request.OnDetachedUsage,
		message: prompt, images: request.Images, effort: cursorSentEffort(selection),
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
	client           *bridgeClient
	agentID          string                                 ///< 新建或续用的 Agent
	apiKey           string                                 ///< 已去除首尾空白的 User API Key
	scope            SessionScope                           ///< 本轮不可变的渠道和调用方身份
	model            string                                 ///< 本轮实际路由模型，用于工具恢复和重放
	customTools      map[string]*sdkv1.CustomToolDefinition ///< Agent 创建时注册的客户端工具
	toolsDigest      string                                 ///< customTools 摘要，参与停放键
	transcriptDigest string                                 ///< 当前请求完整消息摘要，参与停放键
	onDetachedUsage  func(Usage)                            ///< 工具会话晚到用量的接收方
	message          string                                 ///< 本次 Send 的提示词
	images           []Image                                ///< 本次 Send 的图片
	sendModel        *sdkv1.ModelSelection                  ///< 续用 Agent 时覆盖创建时的模型；新建时为 nil
	effort           string                                 ///< 本轮实际选择的思考档位，未指定时为空
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
		client: client.agent, ctx: runCtx, cancel: stopRun,
		events: make(chan Event, 64), done: make(chan struct{}),
		tools:            run.customTools,
		toolsDigest:      run.toolsDigest,
		transcriptDigest: run.transcriptDigest,
		captureRaw:       rawResponseCaptureEnabled(ctx),
		idleTimeout:      idleTimeout,
	}
	if len(session.tools) > 0 {
		session.toolsDisabled = make(chan struct{})
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
	events, err := session.nextTurn(ctx, run.onDetachedUsage, &effort, nil)
	r.active.Add(1)
	go r.consumeRun(session, stream)
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
 * @brief 解析非空基础模型之后的思考等级后缀。
 * @param model 已去除外层 fast 后缀的模型名。
 * @return 基础模型名；无等级后缀时返回原名。
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
 * @brief 解析原生模型或兼容后缀，并应用目录声明的思考参数。
 * @param ctx 当前请求上下文；用于按需拉取模型目录。
 * @param client 当前 Bridge 客户端。
 * @param apiKey User API Key。
 * @param model 渠道映射后的 Cursor 模型名。
 * @param requestEffort 请求体声明的思考等级；解析出的等级后缀优先。
 * @return 发给 CreateAgent 或续轮 Send 的模型选择。
 * @return 需要消歧模型名或校验等级、但模型目录拉取失败时返回已分类错误。
 */
func (r *SDKRunner) modelSelection(
	ctx context.Context,
	client *bridgeClient,
	apiKey, model, requestEffort string,
) (*sdkv1.ModelSelection, error) {
	model = strings.TrimSpace(model)
	beforeFast := model
	if len(model) > len("-fast") && strings.HasSuffix(strings.ToLower(model), "-fast") {
		beforeFast = strings.TrimSpace(model[:len(model)-len("-fast")])
	}
	base, suffixEffort := splitCursorEffortSuffix(beforeFast)
	selection := &sdkv1.ModelSelection{Id: model}
	if beforeFast == model && suffixEffort == "" && requestEffort == "" {
		return selection, nil
	}
	catalog, err := r.modelCatalog(ctx, client, apiKey)
	if err != nil {
		return nil, err
	}
	item := cursorCatalogModel(catalog, model)
	fast := item == nil && beforeFast != model
	if fast {
		selection.Id = beforeFast
		item = cursorCatalogModel(catalog, beforeFast)
	}
	if item == nil && suffixEffort != "" {
		selection.Id = base
		item = cursorCatalogModel(catalog, base)
		requestEffort = suffixEffort
	}
	if item != nil {
		selection.Id = strings.TrimSpace(item.GetId())
	}
	if fast {
		param, _ := cursorCatalogParamValue(item, "fast", "true")
		if param == nil {
			// Explicit -fast aliases predate parameter declarations in the catalog.
			param = &sdkv1.ModelParameterValue{Id: "fast", Value: "true"}
		}
		selection.Params = append(selection.Params, param)
	}
	selection.Params = append(selection.Params, cursorEffortParams(item, requestEffort)...)
	return selection, nil
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
 * @return 拉取失败时返回已分类错误。
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
		return nil, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err, apiKey)
	}
	models := response.Msg.GetItems()
	r.storeModelCatalog(fingerprint, models)
	return models, nil
}

/**
 * @brief 缓存模型目录并清理所有已过期的目录。
 * @param fingerprint User API Key 的 SHA-256 指纹。
 * @param models ListModels 返回的模型。
 * @note 并发安全。
 */
func (r *SDKRunner) storeModelCatalog(fingerprint [sha256.Size]byte, models []*sdkv1.SdkModel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalogs == nil {
		r.catalogs = make(map[[sha256.Size]byte]cursorModelCatalog)
	}
	now := time.Now()
	for key, catalog := range r.catalogs {
		if !now.Before(catalog.expiresAt) {
			delete(r.catalogs, key)
		}
	}
	r.catalogs[fingerprint] = cursorModelCatalog{models: models, expiresAt: now.Add(sdkModelCatalogTTL)}
}

/**
 * @brief 生成 Cursor 目录支持的思考参数。
 * @param model ListModels 返回的目标模型；未登记时为 nil。
 * @param effort 请求的思考等级。
 * @return 参数名和值均沿用目录；未声明该等级时为 nil。
 * @details 关闭思考时优先使用 thinking=false；仅有布尔开关时 high 对应 true。
 */
func cursorEffortParams(model *sdkv1.SdkModel, effort string) []*sdkv1.ModelParameterValue {
	want := canonicalCursorEffort(effort)
	if want == "" {
		return nil
	}
	if want == "none" {
		if off, _ := cursorCatalogParamValue(model, "thinking", "false"); off != nil {
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
				return []*sdkv1.ModelParameterValue{on, value}
			}
		}
		return []*sdkv1.ModelParameterValue{value}
	}
	if !hasLevels && want == "high" {
		on, _ := cursorCatalogParamValue(model, "thinking", "true")
		off, _ := cursorCatalogParamValue(model, "thinking", "false")
		if on != nil && off != nil {
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
	key      string      ///< 本批调用 ID 和额外用户内容的摘要。
	starting bool        ///< 已保留发送资格，新 Run 正在准备。
	session  *sdkSession ///< 新 Run；启动失败时为空，允许再次尝试启动。
}

/**
 * @brief 将工具结果交给原 Run，再用完整历史与追加输入发起独立的新 Run。
 * @param ctx 当前请求上下文。
 * @param apiKey 当前 Cursor User API Key。
 * @param model 当前实际路由模型；省略时沿用原 Run 模型。
 * @param request 工具结果和新的用户内容。
 * @return 新 Run 或已完成的混合轮事件；提交或启动失败时为 nil。
 * @return 请求或启动错误；重复请求的替换 Run 未结束时为 ErrToolTurnInProgress。
 */
func (r *SDKRunner) runMixedToolTurn(ctx context.Context, apiKey, model string, request Request) (<-chan Event, error) {
	mixed, original, err := r.prepareMixedToolTurn(ctx, apiKey, model, request)
	if err != nil {
		return nil, err
	}
	var inherited map[string]*sdkv1.CustomToolDefinition
	if original != nil {
		if model == "" {
			model = original.model
		}
		inherited = original.tools
	}
	if mixed != nil && mixed.session != nil {
		return replayCompletedToolTurn(ctx, mixed.session), nil
	}
	events, replacement, err := r.runFreshTurn(ctx, apiKey, model, request, inherited)
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
 * @param apiKey 当前 Cursor User API Key。
 * @param model 当前实际路由模型；空时沿用原 Run 模型。
 * @param request 工具结果和新的用户内容。
 * @return 可启动或已完成的混合轮；仅凭历史启动时为空。
 * @return 匹配的原 Run；结果未知或范围不符时为空。
 * @return 上一混合轮仍在执行时为 ErrToolTurnInProgress，其他失败返回请求或提交错误。
 * @note 与纯工具结果续轮串行，且在原 Run 未被请求附着时提交结果。
 */
func (r *SDKRunner) prepareMixedToolTurn(ctx context.Context, apiKey, model string, request Request) (*mixedCursorToolTurn, *sdkSession, error) {
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	turn, err := r.classifyCursorToolTurn(request.SessionScope, apiKey, model, request.ToolResults)
	if err != nil || turn.kind == cursorTurnNew {
		return nil, nil, err
	}
	original := turn.session
	digest := sha256.New()
	writeDigestFields(digest, cmp.Or(model, original.model), request.Effort, cursorRequestPrompt(request),
		request.ToolChoice, string(rawJSON(request.Tools)), string(rawJSON(request.ToolResults)), strconv.Itoa(len(request.Images)))
	for _, image := range request.Images {
		writeDigestFields(digest, image.MimeType, image.Data, image.URL)
	}
	key := string(digest.Sum(nil))
	mixed := original.mixedToolTurn
	if mixed != nil {
		if mixed.starting {
			return nil, nil, ErrToolTurnInProgress
		}
		if mixed.session != nil {
			select {
			case <-mixed.session.done:
			default:
				return nil, nil, ErrToolTurnInProgress
			}
			if key == mixed.key {
				return mixed, original, nil
			}
		}
	} else if turn.kind == cursorTurnReplay {
		select {
		case <-original.done:
		default:
			return nil, nil, ErrToolTurnInProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, context.Cause(ctx)
	}
	if hasURLImage(request.Images) {
		return nil, nil, ErrURLImageUnsupported
	}
	if turn.kind == cursorTurnResume {
		original.turnMu.Lock()
		defer original.turnMu.Unlock()
		if original.attached {
			return nil, nil, ErrToolTurnInProgress
		}
		if original.ctx.Err() != nil {
			return nil, nil, ErrToolSessionNotFound
		}
		if err := r.resolveToolResults(original, turn.pending, true); err != nil {
			return nil, nil, err
		}
		original.setTranscriptDigest("")
		// Let the native callbacks receive their responses and the old Run
		// finish or collect late usage without holding up the fresh Send.
		original.armIdleTimerLocked(BridgeCleanupTimeout)
	}
	mixed = &mixedCursorToolTurn{key: key, starting: true}
	original.mixedToolTurn = mixed
	return mixed, original, nil
}

/**
 * @brief 在已认证的范围内恢复工具会话或重放已完成的工具轮。
 * @param ctx 当前请求上下文。
 * @param apiKey 已去除首尾空白的 User API Key。
 * @param model 当前实际路由模型；SDK 调用方省略模型时为空。
 * @param request 当前轮工具结果、历史与所有者身份。
 * @return 已有会话的事件流；需要新开会话时为 nil。
 * @return 会话关闭、结果不一致或提交失败时的错误；其余情况为 nil。
 * @note 分类与提交共享临界区，重复请求才能看到已提交结果。
 */
func (r *SDKRunner) resumeToolTurn(ctx context.Context, apiKey, model string, request Request) (<-chan Event, error) {
	// 分类与提交结果必须处于同一临界区，重复请求才能看到已提交状态并重放。
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	turn, err := r.classifyCursorToolTurn(request.SessionScope, apiKey, model, request.ToolResults)
	if err != nil {
		return nil, err
	}
	switch turn.kind {
	case cursorTurnResume:
		return turn.session.nextTurn(ctx, request.OnDetachedUsage, nil, func() error {
			turn.session.setTranscriptDigest(transcriptDigest(request.transcript))
			return r.resolveToolResults(turn.session, turn.pending, request.ToolChoice == "none")
		})
	case cursorTurnReplay:
		return replayCompletedToolTurn(ctx, turn.session), nil
	default:
		return nil, nil
	}
}

/**
 * @brief 根据请求范围内可见的工具调用判断是否恢复或重放。
 * @param scope 已认证的调用方与当前渠道。
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
			pending.mu.Lock()
			resolved := pending.resolved
			pending.mu.Unlock()
			if !resolved {
				callSession = pending.session
				pendingResults = append(pendingResults, result)
			}
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
 * @param scope 已认证的调用方与当前渠道。
 * @param apiKey 已去除首尾空白的 User API Key。
 * @param model 实际路由模型；省略时沿用原 Run 的模型。
 * @return 身份与模型均匹配时为 true；未找到会话时为 false。
 */
func (s *sdkSession) matchesToolTurn(scope SessionScope, apiKey, model string) bool {
	return s != nil && s.scope == scope && s.apiKey == apiKey && (model == "" || s.model == model)
}

func replayCompletedToolTurn(ctx context.Context, session *sdkSession) <-chan Event {
	output := make(chan Event, 1)
	go func() {
		defer close(output)
		select {
		case <-session.done:
		case <-ctx.Done():
			output <- Event{Done: true, Err: context.Cause(ctx), Replayed: true}
			return
		}
		terminal, ok := session.terminalEvent()
		if !ok {
			terminal = Event{Done: true, Err: ErrToolSessionNotFound}
		}
		terminal.Replayed = true
		terminal.Usage = nil
		output <- terminal
	}()
	return output
}

/**
 * @brief 验证所有工具结果能够交付后，以原子操作提交同一会话的工具响应。
 * @param session 等待工具结果的会话。
 * @param results 待交付的客户端结果。
 * @param disableTools 交付现有结果后是否拒绝本 Run 后续的工具调用。
 * @return 重复提交同一调用时返回 ErrInvalidRequest；会话不再可用或编码失败时返回对应错误。
 */
func (r *SDKRunner) resolveToolResults(session *sdkSession, results []ToolResult, disableTools bool) error {
	type preparedResult struct {
		pending *pendingToolCall
		value   *structpb.Struct
	}
	prepared := make([]preparedResult, 0, len(results))
	values := make([]*structpb.Struct, len(results))
	for index, result := range results {
		value, err := callbackResult(result)
		if err != nil {
			return fmt.Errorf("encode result for Cursor custom tool %q: %w", result.CallID, err)
		}
		values[index] = value
	}

	r.mu.Lock()
	seen := make(map[*pendingToolCall]struct{}, len(results))
	for index, result := range results {
		pending := r.calls[strings.TrimSpace(result.CallID)]
		if pending == nil || pending.session != session {
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("%w: call_id %q", ErrToolSessionNotFound, result.CallID)
		}
		if _, duplicate := seen[pending]; duplicate {
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("%w: tool result %q appears more than once", ErrInvalidRequest, result.CallID)
		}
		seen[pending] = struct{}{}
		pending.mu.Lock()
		if pending.resolved {
			pending.mu.Unlock()
			for _, item := range prepared {
				item.pending.mu.Unlock()
			}
			r.mu.Unlock()
			return fmt.Errorf("tool result %q was already submitted", result.CallID)
		}
		prepared = append(prepared, preparedResult{pending: pending, value: values[index]})
	}
	for _, item := range prepared {
		item.pending.resolved = true
	}
	if r.completedCalls == nil {
		r.completedCalls = make(map[string]*sdkSession)
	}
	for index, item := range prepared {
		result := results[index]
		result.CallID = strings.TrimSpace(result.CallID)
		r.completedCalls[result.CallID] = item.pending.session
	}
	for _, item := range prepared {
		item.pending.mu.Unlock()
	}
	for _, item := range prepared {
		item.pending.result <- toolCallbackResult{value: item.value}
	}
	if disableTools && !session.toolsAreDisabled() {
		close(session.toolsDisabled)
	}
	r.mu.Unlock()
	return nil
}

/**
 * @brief 将一个请求附着到 Agent，消费至终态或下一批客户端工具调用。
 * @param ctx 当前请求上下文；附着期间取消会终止 Agent。
 * @param onDetachedUsage 当前请求提供的迟到用量接收方；nil 表示无接收方。
 * @param sentEffort 新 Send 的实际思考档位；恢复工具轮时为 nil。
 * @param before 提交工具结果的操作；提交成功后才完成附着，nil 表示无需提交。
 * @return 当前轮事件流；附着或提交失败时为 nil。
 * @return 已有附着请求或提交失败时返回错误，否则为 nil。
 * @note turnMu 保护附着与空闲计时器。已取消的 Run 由当前请求等待终态，关闭输出前解除请求取消绑定。
 * 无跨请求身份的 Run 在正常交付工具批次并解除附着后取消，剩余用量仍交给 onDetachedUsage。
 */
func (s *sdkSession) nextTurn(ctx context.Context, onDetachedUsage func(Usage), sentEffort *string, before func() error) (<-chan Event, error) {
	s.turnMu.Lock()
	if s.attached {
		s.turnMu.Unlock()
		return nil, errors.New("cursor tool session already has an attached request")
	}
	s.eventMu.Lock()
	s.turnText.Reset()
	s.eventMu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			s.turnMu.Unlock()
			return nil, err
		}
	}
	s.attached = true
	s.onDetachedUsage = nil
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.turnMu.Unlock()
	stopCaller := context.AfterFunc(ctx, func() { s.cancelRun(nil) })
	release := func(atToolBoundary bool) bool {
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		if atToolBoundary {
			if s.ctx.Err() != nil || !stopCaller() {
				return false
			}
			s.onDetachedUsage = onDetachedUsage
			if !s.scope.reusable() {
				s.cancel(nil)
			}
		} else {
			stopCaller()
		}
		s.attached = false
		if s.ctx.Err() == nil {
			s.armIdleTimerLocked(s.idleTimeout)
		}
		return true
	}

	output := make(chan Event, 16)
	go func() {
		defer close(output)
		detached := false
		defer func() {
			if !detached {
				release(false)
			}
		}()
		var text strings.Builder
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
			collect := func(event Event) {
				if event.Usage != nil {
					if final.Usage == nil {
						final.Usage = &Usage{}
					}
					final.Usage.Add(event.Usage)
				}
			}
			collect(pending)
			<-s.done
			for {
				select {
				case event := <-s.events:
					collect(event)
				default:
					if !pending.Done {
						terminal, _ := s.terminalEvent()
						collect(terminal)
					}
					// This goroutine is the only sender. If the consumer stopped,
					// reclaim one queued slot and carry its usage into the terminal.
					select {
					case output <- final:
					default:
						select {
						case queued := <-output:
							collect(queued)
						default:
						}
						output <- final
					}
					return
				}
			}
		}
		deliver := func(event Event) bool {
			if event.ToolCall != nil && s.toolsAreDisabled() {
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
				if event.ToolCall != nil && !s.toolsAreDisabled() {
					stopBatchTimer()
					batchTimer = time.NewTimer(10 * time.Millisecond)
					batch = batchTimer.C
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
	select {
	case s.events <- event:
		if event.ToolCall != nil {
			s.confirmParkedStart(nil)
		}
		if event.Delta != "" {
			s.eventMu.Lock()
			s.turnText.WriteString(event.Delta)
			s.eventMu.Unlock()
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

/**
 * @brief 记录最近附着请求的完整消息摘要，供 Run 结束后停放 Agent。
 * @param digest transcriptDigest 结果。
 */
func (s *sdkSession) setTranscriptDigest(digest string) {
	s.eventMu.Lock()
	s.transcriptDigest = digest
	s.eventMu.Unlock()
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

func (s *sdkSession) allowsTool(name string) bool {
	_, ok := s.tools[name]
	return ok
}

/**
 * @brief 查询此 Run 是否已停止向客户端转发新工具调用。
 * @return 恢复工具会话时禁用了后续工具则为 true。
 * @note 并发安全。
 */
func (s *sdkSession) toolsAreDisabled() bool {
	select {
	case <-s.toolsDisabled:
		return true
	default:
		return false
	}
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

func (r *SDKRunner) registerToolCall(callID string, pending *pendingToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrBridgeClosed
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
 * @note 持有一个 active 引用；取消和删除 Agent 使用独立清理预算；正常完成的 Agent 停放供普通续轮复用。
 */
func (r *SDKRunner) consumeRun(
	session *sdkSession,
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage],
) {
	defer r.active.Done()
	runCtx := session.ctx
	client := session.client
	agentID := session.agentID
	canceller := &runCanceller{client: client, agentID: agentID, apiKey: session.apiKey}
	state := &sdkRunState{
		agentID: agentID,
		apiKey:  session.apiKey,
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

	var undeliveredUsage Usage
	deliverEvent := func(event Event) {
		if !session.emit(event) && event.Usage != nil {
			undeliveredUsage.Add(event.Usage)
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
	if consumeErr == nil {
		consumeErr = state.finalError(stream.Err())
	}
	if closeErr := stream.Close(); closeErr != nil && consumeErr == nil {
		consumeErr = classifyBridgeError(closeErr)
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
		Text: state.text, Done: true, Err: redactBridgeError(consumeErr, session.apiKey), Usage: state.unreportedUsage(),
	}
	if hasTokenUsage(&undeliveredUsage) {
		if finalEvent.Usage == nil {
			finalEvent.Usage = &Usage{}
		}
		finalEvent.Usage.Add(&undeliveredUsage)
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
