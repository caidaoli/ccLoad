package cursorauth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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
	catalogs       map[string]cursorModelCatalog ///< API Key → 模型目录；受 mu 保护
	parked         map[string]*parkedAgent       ///< 停放键 → 已完成普通轮、可续发的 Agent；受 mu 保护

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
	workdir            string
	client             sdkv1connect.SdkAgentServiceClient
	ctx                context.Context
	cancel             context.CancelFunc
	events             chan Event
	done               chan struct{}
	tools              map[string]struct{}
	toolsDigest        string ///< 自定义工具定义摘要，参与停放键。
	captureRaw         bool
	eventMu            sync.Mutex
	inputTokenEstimate int
	transcriptDigest   string          ///< 最近附着请求的完整消息摘要；受 eventMu 保护。
	turnText           strings.Builder ///< 最近附着请求收到的正文；受 eventMu 保护。
	terminated         bool
	terminal           *Event

	turnMu      sync.Mutex
	attached    bool
	idleTimeout time.Duration ///< 工具轮之间等待下一请求的最长间隔。
	idleTimer   *time.Timer   ///< 仅在无附着请求时运行，受 turnMu 保护。
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

// ListModels returns Cursor's model IDs plus the -fast form accepted by the SDK.
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
		return nil, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err)
	}

	r.storeModelCatalog(apiKey, response.Msg.GetItems())
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
		if supportsFastModelVariant(item) && !strings.HasSuffix(strings.ToLower(id), "-fast") {
			add(id + "-fast")
		}
	}
	if len(models) == 0 {
		return nil, errors.New("cursor SDK returned an empty model catalog")
	}
	return models, nil
}

func supportsFastModelVariant(model *sdkv1.SdkModel) bool {
	for _, variant := range model.GetVariants() {
		for _, parameter := range variant.GetParams() {
			if strings.EqualFold(strings.TrimSpace(parameter.GetId()), "fast") &&
				strings.EqualFold(strings.TrimSpace(parameter.GetValue()), "true") {
				return true
			}
		}
	}
	return false
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
 * @return 凭证、请求或 Bridge 操作错误；成功附着时为 nil。
 * @note 启动后台事件消费；工具轮之间保留 Agent，空闲超时后回收。
 */
func (r *SDKRunner) Run(
	ctx context.Context,
	credential *Credential,
	request Request,
) (<-chan Event, error) {
	if r == nil || r.bridge == nil {
		return nil, errors.New("cursor SDK runner is unavailable")
	}
	if credential == nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil, ErrMissingAPIKey
	}
	if len(request.ToolResults) > 0 {
		events, err := r.resumeToolTurn(ctx, credential, request)
		if err != nil || events != nil {
			return events, err
		}
		// cursorTurnNew: trailing tool_result blocks are conversation history.
		// ParseRequest already inlined them into Prompt.
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return nil, errors.New("cursor prompt is required")
	}
	if len(request.ToolResults) > 0 {
		prompt += "\n\n" + cursorSessionRecoveryNote
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = "default"
	}
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.active.Done()

	client, err := r.bridge.client(ctx)
	if err != nil {
		return nil, err
	}
	customTools, err := customToolDefinitions(request)
	if err != nil {
		return nil, err
	}
	if len(customTools) > 0 {
		if err := r.ensureToolCallback(ctx, client); err != nil {
			return nil, err
		}
	}
	toolsDigest, err := customToolsDigest(customTools)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(credential.APIKey)
	selection, err := r.modelSelection(ctx, client, apiKey, model, request.Effort)
	if err != nil {
		return nil, err
	}
	run := sdkRunStart{
		client: client, apiKey: apiKey, customTools: customTools, toolsDigest: toolsDigest,
		transcriptDigest: transcriptDigest(request.transcript), request: request,
	}
	if len(request.ToolResults) == 0 {
		if agentID, tail, ok := r.takeParkedAgent(run, request.transcript); ok {
			run.agentID = agentID
			run.message, run.images = renderTranscript(tail)
			run.sendModel = selection
			events, err := r.startRun(ctx, run)
			if err == nil || ctx.Err() != nil {
				return events, err
			}
			log.Printf("[WARN] 续用 Cursor SDK Agent %s 失败，改为新建 Agent: %v", agentID, err)
		}
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
		classifiedErr := classifyBridgeOperationError("CreateAgent", createCtx, RequestTimeout, createStarted, err)
		createCancel()
		return nil, classifiedErr
	}
	createCancel()
	run.agentID = strings.TrimSpace(created.Msg.GetAgentId())
	if run.agentID == "" {
		return nil, errors.New("cursor-sdk-bridge CreateAgent returned an empty agent_id")
	}
	run.message, run.images = prompt, request.Images
	return r.startRun(ctx, run)
}

/**
 * @brief 在已有 Agent 上发起一次 Run 所需的参数。
 */
type sdkRunStart struct {
	client           *bridgeClient
	agentID          string                                 ///< 新建或续用的 Agent
	apiKey           string                                 ///< 已去除首尾空白的 User API Key
	customTools      map[string]*sdkv1.CustomToolDefinition ///< Agent 创建时注册的客户端工具
	toolsDigest      string                                 ///< customTools 摘要，参与停放键
	transcriptDigest string                                 ///< 当前请求完整消息摘要，参与停放键
	request          Request                                ///< 当前请求，提供用量估算
	message          string                                 ///< 本次 Send 的提示词
	images           []Image                                ///< 本次 Send 的图片
	sendModel        *sdkv1.ModelSelection                  ///< 续用 Agent 时覆盖创建时的模型；新建时为 nil
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
 * @return 会话注册或 Send 失败时返回已分类错误；失败时 Agent 已转交后台删除。
 * @note 调用方须持有 active 引用；成功时后台消费 Run，结束后删除或停放 Agent。
 */
func (r *SDKRunner) startRun(ctx context.Context, run sdkRunStart) (<-chan Event, error) {
	client := run.client
	idleTimeout := r.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = sdkSessionIdleTimeout
	}
	// Native custom tools may suspend this run across multiple downstream HTTP
	// requests, so its lifetime cannot be derived from any one request context.
	runStarted := time.Now()
	runCtx, stopRun := context.WithCancel(context.Background())
	session := &sdkSession{
		agentID: run.agentID, apiKey: run.apiKey, workdir: client.workdir,
		client: client.agent, ctx: runCtx, cancel: stopRun,
		events: make(chan Event, 64), done: make(chan struct{}),
		tools:              make(map[string]struct{}, len(run.customTools)),
		toolsDigest:        run.toolsDigest,
		transcriptDigest:   run.transcriptDigest,
		inputTokenEstimate: run.request.InputTokenEstimate,
		captureRaw:         rawResponseCaptureEnabled(ctx),
		idleTimeout:        idleTimeout,
	}
	for name := range run.customTools {
		session.tools[name] = struct{}{}
	}
	if err := r.registerSession(session); err != nil {
		stopRun()
		r.active.Go(func() { r.deleteAgent(client.agent, run.agentID, client.workdir, run.apiKey) })
		return nil, err
	}
	stopCaller := context.AfterFunc(ctx, session.cancelRun)
	defer stopCaller()
	stream, err := client.agent.Send(runCtx, connect.NewRequest(&sdkv1.SendRequest{
		AgentId: run.agentID,
		Message: &sdkv1.UserMessage{Text: run.message, Images: sdkImages(run.images)},
		Options: &sdkv1.SendOptions{
			Model:        run.sendModel,
			EnableDeltas: false,
			EnableSteps:  false,
		},
	}))
	if err != nil {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		classifiedErr := classifyBridgeOperationError("Send", ctx, 0, runStarted, err)
		session.abort(classifiedErr)
		r.unregisterSession(session)
		// A failed Send must return immediately; DeleteAgent/CloseAgent have
		// their own independent deadlines.
		r.active.Go(func() { r.deleteAgent(client.agent, run.agentID, client.workdir, run.apiKey) })
		return nil, classifiedErr
	}

	r.active.Add(1)
	go r.consumeRun(session, stream)
	return session.nextTurn(ctx, nil)
}

/**
 * @brief 把客户端图片转换为 SDK 消息图片。
 * @param images 提示词中按序出现的图片。
 * @return SDK 图片；无图片时为 nil。
 */
func sdkImages(images []Image) []*sdkv1.SdkImage {
	if len(images) == 0 {
		return nil
	}
	out := make([]*sdkv1.SdkImage, 0, len(images))
	for _, image := range images {
		if image.Data != "" {
			out = append(out, &sdkv1.SdkImage{Source: &sdkv1.SdkImage_Data{
				Data: &sdkv1.SdkImageData{Data: image.Data, MimeType: image.MimeType},
			}})
			continue
		}
		out = append(out, &sdkv1.SdkImage{Source: &sdkv1.SdkImage_Url{Url: &sdkv1.SdkImageUrl{Url: image.URL}}})
	}
	return out
}

/**
 * @brief 解析客户端模型名中的 SDK 模型、fast 档与思考等级后缀。
 * @param model 客户端或渠道映射后的 Cursor 模型名。
 * @return 不含思考等级的模型选择；仅 fast 写入参数。
 * @return 模型名后缀声明的小写思考等级；无后缀时为空。
 */
func cursorModelSelection(model string) (*sdkv1.ModelSelection, string) {
	id, fast, effort := normalizeCursorModelID(model)
	selection := &sdkv1.ModelSelection{Id: id}
	if fast && id != "" {
		selection.Params = []*sdkv1.ModelParameterValue{{Id: "fast", Value: "true"}}
	}
	return selection, effort
}

/**
 * @brief 把客户端/目录别名映射为 SDK ListModels 模型 ID。
 * @param model 客户端或渠道映射后的模型名。
 * @return SDK 模型 ID；去除后缀后为空时原样返回。
 * @return 是否以 -fast 结尾。
 * @return 去除的思考等级后缀（小写、不含 thinking- 前缀）；无后缀时为空。
 * @details Cursor 把思考等级作为 ModelSelection 的 effort 参数而不是模型名后缀。
 */
func normalizeCursorModelID(model string) (id string, fast bool, effort string) {
	id = strings.TrimSpace(model)
	if id == "" {
		return "", false, ""
	}
	lower := strings.ToLower(id)
	if strings.HasSuffix(lower, "-fast") {
		fast = true
		id = strings.TrimSpace(id[:len(id)-len("-fast")])
		lower = strings.ToLower(id)
	}
	for _, suffix := range cursorEffortSuffixes {
		if strings.HasSuffix(lower, suffix) {
			id = strings.TrimSpace(id[:len(id)-len(suffix)])
			effort = strings.TrimPrefix(strings.TrimPrefix(suffix, "-"), "thinking-")
			break
		}
	}
	if id == "" {
		return strings.TrimSpace(model), false, ""
	}
	return id, fast, effort
}

/**
 * @brief 生成带思考等级参数的模型选择。
 * @param ctx 当前请求上下文；用于按需拉取模型目录。
 * @param client 当前 Bridge 客户端。
 * @param apiKey User API Key。
 * @param model 渠道映射后的 Cursor 模型名。
 * @param requestEffort 请求体声明的思考等级；模型名后缀优先。
 * @return 模型选择；目录未声明该等级时不写 effort 参数。
 * @return 需要校验等级但拉取模型目录失败时返回已分类错误。
 */
func (r *SDKRunner) modelSelection(
	ctx context.Context,
	client *bridgeClient,
	apiKey, model, requestEffort string,
) (*sdkv1.ModelSelection, error) {
	selection, effort := cursorModelSelection(model)
	if effort == "" {
		effort = requestEffort
	}
	if effort == "" {
		return selection, nil
	}
	catalog, err := r.modelCatalog(ctx, client, apiKey)
	if err != nil {
		return nil, err
	}
	if value := cursorEffortValue(catalog, selection.GetId(), effort); value != "" {
		selection.Params = append(selection.Params, &sdkv1.ModelParameterValue{Id: "effort", Value: value})
	}
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
 * @brief 模型目录缓存有效期；目录只用于校验思考等级参数。
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
	r.mu.Lock()
	cached, ok := r.catalogs[apiKey]
	r.mu.Unlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.models, nil
	}
	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := listCursorModels(requestCtx, client, apiKey)
	if err != nil {
		return nil, classifyBridgeOperationError("ListModels", requestCtx, RequestTimeout, started, err)
	}
	models := response.Msg.GetItems()
	r.storeModelCatalog(apiKey, models)
	return models, nil
}

/**
 * @brief 缓存 API Key 对应的模型目录。
 * @param apiKey User API Key。
 * @param models ListModels 返回的模型。
 */
func (r *SDKRunner) storeModelCatalog(apiKey string, models []*sdkv1.SdkModel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalogs == nil {
		r.catalogs = make(map[string]cursorModelCatalog)
	}
	r.catalogs[apiKey] = cursorModelCatalog{models: models, expiresAt: time.Now().Add(sdkModelCatalogTTL)}
}

/**
 * @brief 在模型目录中查找与请求等级对应的 effort 参数值。
 * @param catalog ListModels 返回的模型。
 * @param modelID SDK 模型 ID。
 * @param effort 请求的思考等级。
 * @return 目录中的原样参数值；模型未收录或未声明该等级时为空。
 * @details 参数定义和变体参数都视为声明；xhigh 与 extra-high 视为同一档。
 */
func cursorEffortValue(catalog []*sdkv1.SdkModel, modelID, effort string) string {
	want := canonicalCursorEffort(effort)
	for _, model := range catalog {
		if !strings.EqualFold(strings.TrimSpace(model.GetId()), modelID) {
			continue
		}
		for _, parameter := range model.GetParameters() {
			if !isCursorEffortParameter(parameter.GetId()) {
				continue
			}
			for _, value := range parameter.GetValues() {
				if canonicalCursorEffort(value.GetValue()) == want {
					return strings.TrimSpace(value.GetValue())
				}
			}
		}
		for _, variant := range model.GetVariants() {
			for _, parameter := range variant.GetParams() {
				if isCursorEffortParameter(parameter.GetId()) && canonicalCursorEffort(parameter.GetValue()) == want {
					return strings.TrimSpace(parameter.GetValue())
				}
			}
		}
		return ""
	}
	return ""
}

func isCursorEffortParameter(id string) bool {
	return strings.EqualFold(strings.TrimSpace(id), "effort")
}

func canonicalCursorEffort(effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "extra-high" || effort == "extra_high" {
		return "xhigh"
	}
	return effort
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

func (r *SDKRunner) resumeToolTurn(ctx context.Context, credential *Credential, request Request) (<-chan Event, error) {
	// 分类与提交结果必须处于同一临界区，重复请求才能看到已提交状态并重放。
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	turn, err := r.classifyCursorToolTurn(credential, request.ToolResults)
	if err != nil {
		return nil, err
	}
	switch turn.kind {
	case cursorTurnResume:
		return turn.session.nextTurn(ctx, func() error {
			turn.session.setInputTokenEstimate(request.InputTokenEstimate)
			turn.session.setTranscriptDigest(transcriptDigest(request.transcript))
			return r.resolveToolResults(turn.session, turn.pending)
		})
	case cursorTurnReplay:
		turn.session.setInputTokenEstimate(request.InputTokenEstimate)
		return replayCompletedToolTurn(ctx, turn.session), nil
	default:
		return nil, nil
	}
}

func (r *SDKRunner) classifyCursorToolTurn(credential *Credential, results []ToolResult) (cursorToolTurn, error) {
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
		pending := r.calls[callID]
		pendingResolved := false
		if pending != nil {
			pending.mu.Lock()
			pendingResolved = pending.resolved
			pending.mu.Unlock()
		}
		if pending == nil || pending.session == nil || pendingResolved {
			completedSession := r.completedCalls[callID]
			if completedSession == nil {
				unknown++
				continue
			}
			if session == nil {
				session = completedSession
			} else if session != completedSession {
				return cursorToolTurn{}, errors.New("tool results from different Cursor sessions cannot share one request")
			}
			continue
		}
		pendingResults = append(pendingResults, result)
		if session == nil {
			session = pending.session
		} else if session != pending.session {
			return cursorToolTurn{}, errors.New("tool results from different Cursor sessions cannot share one request")
		}
	}
	if unknown > 0 || session == nil {
		return cursorToolTurn{kind: cursorTurnNew}, nil
	}
	if subtleAPIKeyMismatch(session.apiKey, credential.APIKey) {
		return cursorToolTurn{kind: cursorTurnNew}, nil
	}
	if len(pendingResults) == 0 {
		return cursorToolTurn{kind: cursorTurnReplay, session: session}, nil
	}
	return cursorToolTurn{kind: cursorTurnResume, session: session, pending: pendingResults}, nil
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

func (r *SDKRunner) resolveToolResults(session *sdkSession, results []ToolResult) error {
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
			return fmt.Errorf("tool result %q appears more than once", result.CallID)
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
	r.mu.Unlock()
	for _, item := range prepared {
		item.pending.result <- toolCallbackResult{value: item.value}
	}
	return nil
}

func subtleAPIKeyMismatch(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left == "" || right == "" || left != right
}

/**
 * @brief 将一个请求附着到 Agent，消费至终态或下一批客户端工具调用。
 * @param ctx 当前请求上下文；附着期间取消会终止 Agent。
 * @param before 附着后、消费事件前提交工具结果的操作；nil 表示无需提交。
 * @return 当前轮事件流；附着或提交失败时为 nil。
 * @return 已有附着请求或提交失败时返回错误，否则为 nil。
 * @note turnMu 保护附着与空闲计时器；关闭输出前解除请求取消绑定。
 */
func (s *sdkSession) nextTurn(ctx context.Context, before func() error) (<-chan Event, error) {
	s.turnMu.Lock()
	if s.attached {
		s.turnMu.Unlock()
		return nil, errors.New("cursor tool session already has an attached request")
	}
	s.attached = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.turnMu.Unlock()
	s.eventMu.Lock()
	s.turnText.Reset()
	s.eventMu.Unlock()
	stopCaller := context.AfterFunc(ctx, s.cancelRun)
	release := func() {
		stopCaller()
		s.turnMu.Lock()
		s.attached = false
		if s.ctx.Err() == nil {
			var timer *time.Timer
			timer = time.AfterFunc(s.idleTimeout, func() {
				s.turnMu.Lock()
				defer s.turnMu.Unlock()
				if !s.attached && s.idleTimer == timer {
					s.idleTimer = nil
					s.cancel()
				}
			})
			s.idleTimer = timer
		}
		s.turnMu.Unlock()
	}
	if before != nil {
		if err := before(); err != nil {
			release()
			return nil, err
		}
	}

	output := make(chan Event, 16)
	go func() {
		defer close(output)
		defer release()
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
			s.cancelRun()
			final := Event{Text: text.String(), Done: true, Err: context.Cause(ctx)}
			collect := func(event Event) {
				if event.Usage != nil && !event.UsageEstimated {
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
			if event.Delta != "" {
				text.WriteString(event.Delta)
			}
			if event.Delta != "" || event.Text != "" || event.Done {
				event.Text = text.String()
			}
			if event.Done && ctx.Err() != nil {
				event.Err = context.Cause(ctx)
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
				if event.ToolCall != nil {
					stopBatchTimer()
					batchTimer = time.NewTimer(10 * time.Millisecond)
					batch = batchTimer.C
				}
			case <-batch:
				return
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

func (s *sdkSession) emit(event Event) bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminated {
		return false
	}
	select {
	case s.events <- event:
		s.turnText.WriteString(event.Delta)
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *sdkSession) finish(event Event) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.terminated {
		return
	}
	s.terminal = &event
	s.terminated = true
	close(s.done)
}

func (s *sdkSession) setInputTokenEstimate(value int) {
	if value <= 0 {
		return
	}
	s.eventMu.Lock()
	s.inputTokenEstimate = value
	s.eventMu.Unlock()
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

func (s *sdkSession) estimatedUsage() *Usage {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.inputTokenEstimate <= 0 {
		return nil
	}
	return &Usage{InputTokens: s.inputTokenEstimate, TotalTokens: s.inputTokenEstimate}
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
 * @note 并发安全；可重复调用。
 */
func (s *sdkSession) cancelRun() {
	s.turnMu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.cancel()
	s.turnMu.Unlock()
}

/**
 * @brief 取消 Agent 并立即发布调用方指定的终态错误。
 * @param err 终止会话的原因。
 */
func (s *sdkSession) abort(err error) {
	s.cancelRun()
	s.finish(Event{Done: true, Err: err})
}

func (s *sdkSession) allowsTool(name string) bool {
	_, ok := s.tools[name]
	return ok
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

func (r *SDKRunner) failAllSessions(err error) {
	r.mu.Lock()
	sessions := make([]*sdkSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()
	for _, session := range sessions {
		session.abort(err)
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
	canceller := newRunCanceller(client, agentID)
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

	var undeliveredUsage Usage
	deliverEvent := func(event Event) {
		if !session.emit(event) && event.Usage != nil && !event.UsageEstimated {
			undeliveredUsage.Add(event.Usage)
		}
	}
	var consumeErr error
	for stream.Receive() {
		message := stream.Msg()
		if session.captureRaw {
			raw, marshalErr := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
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
	if state.status == sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED && runCtx.Err() != nil {
		consumeErr = context.Cause(runCtx)
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
	session.cancelRun()
	if stopRunCallback() {
		close(runCallbackDone)
	} else {
		<-runCallbackDone
	}
	finalEvent := Event{Text: state.text, Done: true, Err: consumeErr, Usage: state.unreportedUsage()}
	if hasTokenUsage(&undeliveredUsage) {
		if finalEvent.Usage == nil {
			finalEvent.Usage = &Usage{}
		}
		finalEvent.Usage.Add(&undeliveredUsage)
	}
	if !hasTokenUsage(state.usage) && !hasTokenUsage(&state.reportedUsage) {
		finalEvent.Usage = session.estimatedUsage()
		finalEvent.UsageEstimated = finalEvent.Usage != nil
	}
	session.finish(finalEvent)
	r.unregisterSession(session)
	r.expireCompletedToolTurns(session)

	// Cancellation and durable Agent deletion are cleanup, not response work.
	// Keep them tracked by r.active for orderly shutdown, but never make the
	// client or request-duration metric wait behind their independent deadlines.
	canceller.Wait()
	if consumeErr == nil && r.parkAgent(session) {
		return
	}
	r.deleteAgent(client, agentID, session.workdir, session.apiKey)
}

/**
 * @brief 在固定重放保留期后移除一个已完成 Agent 的工具调用标识。
 * @param session 已完成的 Agent 会话。
 * @note 异步清理 completedCalls，与活动请求时限和工具会话空闲预算独立。
 */
func (r *SDKRunner) expireCompletedToolTurns(session *sdkSession) {
	time.AfterFunc(sdkCompletedCallsTTL, func() {
		r.mu.Lock()
		for callID, completedSession := range r.completedCalls {
			if completedSession == session {
				delete(r.completedCalls, callID)
			}
		}
		r.mu.Unlock()
	})
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
		log.Printf("[WARN] 删除 Cursor SDK Agent %s 失败: %v", agentID, classifyBridgeError(err))
		closeCtx, closeCancel := context.WithTimeout(context.Background(), BridgeCleanupTimeout)
		_, closeErr := client.CloseAgent(closeCtx, connect.NewRequest(&sdkv1.CloseAgentRequest{
			AgentId: agentID,
		}))
		closeCancel()
		if closeErr != nil {
			log.Printf("[WARN] 释放 Cursor SDK Agent %s 失败: %v", agentID, classifyBridgeError(closeErr))
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

type runCanceller struct {
	client  sdkv1connect.SdkAgentServiceClient
	agentID string

	mu        sync.Mutex
	runID     string
	requested bool
	issued    bool
	terminal  bool
	active    sync.WaitGroup
}

func newRunCanceller(client sdkv1connect.SdkAgentServiceClient, agentID string) *runCanceller {
	return &runCanceller{client: client, agentID: agentID}
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
			log.Printf("[WARN] 取消 Cursor SDK Run %s 失败: %v", runID, classifyBridgeError(err))
		}
	})
}

func (c *runCanceller) Wait() {
	c.active.Wait()
}
