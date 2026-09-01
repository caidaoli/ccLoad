package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

var errUnknownClientProtocol = errors.New("unknown client protocol for path")

// errBodyTooLarge 与 errBodyReadTimeout 是两种完全不同的失败：前者是请求体超过
// 配置的大小上限（立即拒绝），后者是客户端在读取超时内没把请求体传完。二者的处置
// 手段不同（调大小上限 vs 调读取超时），错误文案必须区分，否则会被误判。
var errBodyTooLarge = errors.New("request body exceeds the configured size limit (settings: max_body_bytes / max_image_body_bytes)")
var errBodyReadTimeout = errors.New("timed out reading the request body before the client finished uploading (setting: http_read_timeout_seconds)")

// ErrAllKeysUnavailable 表示所有渠道密钥都不可用
var ErrAllKeysUnavailable = errors.New("all channel keys unavailable")

// ErrAllKeysExhausted 表示所有密钥都已耗尽
var ErrAllKeysExhausted = errors.New("all keys exhausted")

// ErrNoAPIKeyForModel means this channel has keys, but none may serve the requested model.
// It must not be promoted to a channel cooldown: another model can still use the channel.
var ErrNoAPIKeyForModel = errors.New("no API key available for model")

// ErrChannelRPMExceeded 表示渠道RPM限制已达到
var ErrChannelRPMExceeded = errors.New("channel rpm limit exceeded")

// ErrChannelConcurrencyExceeded 表示渠道并发限制已达到
var ErrChannelConcurrencyExceeded = errors.New("channel concurrency limit exceeded")

// ============================================================================
// 并发控制
// ============================================================================

// acquireConcurrencySlot 获取并发槽位，返回release函数和状态
// ok=false 表示客户端已取消请求
func (s *Server) acquireConcurrencySlot(c *gin.Context) (release func(), ok bool) {
	release, err := s.acquireConcurrencySlotForContext(c.Request.Context())
	if err == nil {
		return release, true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "request timeout while waiting for slot"})
		return nil, false
	}
	c.JSON(StatusClientClosedRequest, gin.H{"error": "request cancelled while waiting for slot"})
	return nil, false
}

func (s *Server) acquireConcurrencySlotForContext(ctx context.Context) (func(), error) {
	select {
	case s.concurrencySem <- struct{}{}:
		return func() { <-s.concurrencySem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ============================================================================
// 请求解析
// ============================================================================

type incomingRequest struct {
	// originalModel 是去掉思考后缀的基名，用于选路、鉴权与冷却；命中多模态
	// 回退后会替换成回退模型，因此日志必须在替换前单独保留客户端模型。
	originalModel string
	// requestedModel 是客户端字面写的模型名，可能带思考后缀。
	requestedModel string
	body           []byte
	isStreaming    bool
	hasModel       bool
}

func (r incomingRequest) authorizationModel() string {
	if !r.hasModel {
		return ""
	}
	return r.originalModel
}

// isRequestReadTimeout 判断读取失败是否来自读取截止时间（HTTP Server ReadTimeout），
// 而不是客户端断开或其它 I/O 故障。
func isRequestReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func parseIncomingRequest(c *gin.Context, bodyLimits requestBodyLimits) (incomingRequest, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 读取请求体（带上限，防止大包打爆内存）
	maxBody := bodyLimits.maxForPath(requestPath)
	limited := io.LimitReader(c.Request.Body, maxBody+1)
	all, err := io.ReadAll(limited)
	if err != nil {
		if isRequestReadTimeout(err) {
			return incomingRequest{}, fmt.Errorf("%w: %v", errBodyReadTimeout, err)
		}
		return incomingRequest{}, fmt.Errorf("failed to read body: %w", err)
	}
	_ = c.Request.Body.Close()
	if int64(len(all)) > maxBody {
		return incomingRequest{}, errBodyTooLarge
	}
	contentType := c.Request.Header.Get("Content-Type")
	mediaType, mediaParams, _ := mime.ParseMediaType(contentType)
	if shouldValidateStrictJSONBody(contentType, all) {
		// 失败路径才走标准库解码，为的是把出错偏移量带给客户端；
		// json.Unmarshal 同样拒绝顶层值之后的尾随数据。
		if !sonic.Valid(all) {
			err := json.Unmarshal(all, new(json.RawMessage))
			if err == nil {
				err = errors.New("expected exactly one JSON value")
			}
			return incomingRequest{}, fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	declaredMediaType := strings.ToLower(strings.TrimSpace(contentType))
	if separator := strings.IndexByte(declaredMediaType, ';'); separator >= 0 {
		declaredMediaType = strings.TrimSpace(declaredMediaType[:separator])
	}
	if mediaType == "multipart/form-data" || declaredMediaType == "multipart/form-data" {
		boundary := strings.TrimSpace(mediaParams["boundary"])
		if boundary == "" {
			return incomingRequest{}, errors.New("invalid multipart body: boundary is missing")
		}
		reader := multipart.NewReader(bytes.NewReader(all), boundary)
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				return incomingRequest{}, fmt.Errorf("invalid multipart body: %w", partErr)
			}
			if _, partErr = io.Copy(io.Discard, part); partErr != nil {
				_ = part.Close()
				return incomingRequest{}, fmt.Errorf("invalid multipart body: %w", partErr)
			}
			if partErr = part.Close(); partErr != nil {
				return incomingRequest{}, fmt.Errorf("invalid multipart body: %w", partErr)
			}
		}
	}

	var reqModel struct {
		Model string `json:"model"`
	}
	_ = sonic.Unmarshal(all, &reqModel)

	// multipart/form-data 支持：当 JSON 解析无 model 时，尝试从 multipart 表单字段提取
	if reqModel.Model == "" && mediaType == "multipart/form-data" {
		if boundary := mediaParams["boundary"]; boundary != "" {
			reqModel.Model = extractModelFromMultipart(all, boundary)
		}
	}

	// 智能检测流式请求
	isStreaming := isStreamingRequest(requestPath, all)

	// 多源模型名称获取：优先请求体，其次URL路径
	originalModel := reqModel.Model
	if originalModel == "" {
		originalModel = extractModelFromPath(requestPath)
	}
	hasModel := originalModel != ""
	requestFamily := protocol.DetectRequestFamily(requestPath)

	// GET 请求保留既有通配选路语义；Codex alpha/search 的业务模型保持为空。
	if originalModel == "" {
		switch {
		case requestFamily == protocol.RequestFamilyAlphaSearch:
		case requestMethod == http.MethodGet:
			originalModel = "*"
		default:
			return incomingRequest{}, fmt.Errorf("invalid JSON or missing model")
		}
	}
	return incomingRequest{
		originalModel:  model.RoutingModelName(originalModel),
		requestedModel: originalModel,
		body:           all,
		isStreaming:    isStreaming,
		hasModel:       hasModel,
	}, nil
}

// requestBodyLimits 是单个 Server 的不可变请求体上限。
type requestBodyLimits struct {
	standard int64
	images   int64
}

func normalizeMaxBodyBytes(maxBodyBytes int64) int64 {
	if maxBodyBytes <= 0 {
		return config.DefaultMaxBodyBytes
	}
	return maxBodyBytes
}

func newRequestBodyLimits(maxBody, maxImageBody int) requestBodyLimits {
	if maxBody <= 0 {
		maxBody = config.DefaultMaxBodyBytes
	}
	if maxImageBody <= 0 {
		maxImageBody = config.DefaultMaxImageBodyBytes
	}
	return requestBodyLimits{standard: int64(maxBody), images: int64(maxImageBody)}
}

func (l requestBodyLimits) maxForPath(requestPath string) int64 {
	if l.standard <= 0 {
		l.standard = config.DefaultMaxBodyBytes
	}
	if l.images <= 0 {
		l.images = config.DefaultMaxImageBodyBytes
	}
	if strings.HasPrefix(requestPath, "/v1/images/") {
		return l.images
	}
	return l.standard
}

// extractModelFromMultipart 从 multipart/form-data 原始字节中提取 model 字段
func extractModelFromMultipart(body []byte, boundary string) string {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(io.LimitReader(part, 256))
			_ = part.Close()
			if err == nil {
				return strings.TrimSpace(string(val))
			}
			break
		}
		_ = part.Close()
	}
	return ""
}

// ============================================================================
// 路由选择
// ============================================================================

// selectRouteCandidates 根据请求选择路由候选
// 从proxy.go提取，遵循SRP原则
func (s *Server) selectRouteCandidates(ctx context.Context, c *gin.Context, originalModel string, clientProtocol string) ([]*model.Config, error) {
	requestMethod := c.Request.Method
	requestFamily := protocol.DetectRequestFamily(c.Request.URL.Path)

	// 智能路由选择：根据请求类型选择不同的路由策略
	if requestMethod == http.MethodGet && clientProtocol == util.ProtocolGemini {
		// Gemini 模型列表请求仍可路由到任意启用渠道。
		return s.selectCandidatesByClientProtocol(ctx, util.ProtocolGemini)
	}

	if clientProtocol == "" {
		return nil, errUnknownClientProtocol
	}
	if requestFamily == protocol.RequestFamilyAlphaSearch {
		return s.selectAlphaSearchCandidates(ctx, originalModel)
	}

	return s.selectCandidatesByModelAndClientProtocol(ctx, originalModel, clientProtocol)
}

// ============================================================================
// 主请求处理器
// ============================================================================

// handleSpecialRoutes 处理特殊路由（模型列表、token计数等）
// 返回 true 表示已处理，调用方应直接返回
func (s *Server) handleSpecialRoutes(c *gin.Context) bool {
	path := c.Request.URL.Path
	method := c.Request.Method

	switch {
	case method == http.MethodGet && path == "/v1/models":
		s.handleListOpenAIModels(c)
		return true
	case method == http.MethodGet && path == "/v1beta/models":
		s.handleListGeminiModels(c)
		return true
	case method == http.MethodPost && path == "/v1/messages/count_tokens":
		s.handleCountTokens(c)
		return true
	}
	return false
}

// HandleProxyRequest 通用透明代理处理器
func (s *Server) HandleProxyRequest(c *gin.Context) {
	// Responses GET is reserved for the downstream WebSocket handshake. A plain
	// GET must stop here instead of entering the generic proxy, where it has no
	// JSON model and is otherwise routed as "*" to every eligible channel.
	if c.Request.Method == http.MethodGet && isResponsesWebsocketPath(c.Request.URL.Path) {
		s.HandleResponsesWebsocket(c)
		return
	}
	httpMetrics := s.httpRuntime.begin()
	defer func() {
		httpMetrics.finish(c.Writer.Status(), c.Writer.Size())
	}()

	startTime := time.Now()

	// 并发控制
	release, ok := s.acquireConcurrencySlot(c)
	if !ok {
		return
	}
	defer release()

	// 特殊路由优先处理
	if s.handleSpecialRoutes(c) {
		return
	}

	requestMethod := c.Request.Method

	incoming, err := parseIncomingRequest(c, s.bodyLimits)
	if err != nil {
		switch {
		case errors.Is(err, errBodyTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
		case errors.Is(err, errBodyReadTimeout):
			// 408 而不是 400：请求本身没问题，是客户端没在读取超时内传完。
			c.JSON(http.StatusRequestTimeout, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	all := incoming.body
	isStreaming := incoming.isStreaming
	httpMetrics.observeRequest(isStreaming, len(all))

	clientProtocol, effectiveRequestPath := clientRequestMetadata(c)
	if err := validateClientBodyMatchesProtocol(clientProtocol, all); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if protocol.DetectRequestFamily(effectiveRequestPath) == protocol.RequestFamilyAlphaSearch {
		all = sanitizeCodexAlphaSearchBody(all)
	}

	// 多模态回退：多模态请求自动切到配置的回退模型。改写必须发生在
	// applyThinkingSuffix 与 Token 白名单之前——渠道候选过滤（SQL 按模型 JOIN）、
	// 模型冷却过滤、Key 白名单全部按模型名做决策，事后换模型只会换来
	// 渠道未声明的模型 + 404 + 逐渠道失败。改 incoming 字段后下游全部自动跟随。
	clientModel := incoming.originalModel
	if fallback := s.multimodalFallbackModel(incoming.originalModel, requestHasNonTextContent(clientProtocol, all)); fallback != "" {
		incoming.originalModel = model.RoutingModelName(fallback)
		incoming.requestedModel = fallback
	}
	originalModel := incoming.originalModel

	// 先把后缀写成客户端协议字段；选定渠道后会再按实际上游模型能力收敛等级。
	all = applyThinkingSuffix(all, clientProtocol, incoming.requestedModel)
	thinkingEffort := thinkingEffortFromRequest(incoming.requestedModel, all)

	tokenHashStr := ""
	if v, ok := c.Get("token_hash"); ok {
		tokenHashStr, _ = v.(string)
	}
	tokenID, _ := c.Get("token_id")
	tokenIDInt64, _ := tokenID.(int64)

	if !s.enforceTokenLimits(c, tokenHashStr, incoming.authorizationModel()) {
		return
	}

	timeout := parseTimeout(c.Request.URL.Query(), c.Request.Header)
	ctx := c.Request.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var executionSession *responsesExecutionSession
	var routingSession *responsesExecutionSession
	var executionSessionRequestBody []byte
	var nativeRequestBody []byte
	if clientProtocol == protocol.Codex && isStreaming && requestMethod == http.MethodPost &&
		protocol.DetectRequestFamily(effectiveRequestPath) == protocol.RequestFamilyResponses {
		sessionID := responsesExecutionSessionID(c.Request.Header)
		// Ordinary HTTP requests only need process-local state when the client supplied
		// the explicit Session-Id contract. Cache routing hints are not conversation IDs.
		if tokenHashStr != "" && sessionID != "" {
			var releaseSession func()
			var errSession error
			executionSession, releaseSession, errSession = s.responsesExecutionSessions.acquire(tokenHashStr, sessionID)
			if errSession != nil {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": errSession.Error()})
				return
			}
			defer releaseSession()
			if errAcquire := executionSession.acquireTurn(ctx); errAcquire != nil {
				c.JSON(http.StatusRequestTimeout, gin.H{"error": errAcquire.Error()})
				return
			}
			defer executionSession.releaseTurn()
			routingSession = executionSession
			replayBody, incrementalBody, localContinuation, errNormalize :=
				executionSession.transcript.normalizeHTTPRequests(all)
			if errNormalize != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": errNormalize.Error()})
				return
			}
			if !localContinuation {
				executionSession = nil
			} else {
				executionSessionRequestBody = replayBody
				// HTTP may continue an already established upstream websocket, but it must
				// never create one. Without an attached socket, preserve HTTP wire semantics.
				if _, connected := executionSession.upstream.targetSnapshot(); connected {
					all = replayBody
					nativeRequestBody = incrementalBody
				}
			}
		}
	}

	cands, err := s.selectRouteCandidates(ctx, c, originalModel, string(clientProtocol))
	if err != nil {
		if errors.Is(err, errUnknownClientProtocol) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unsupported path"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	if len(cands) == 0 {
		if protocol.DetectRequestFamily(effectiveRequestPath) == protocol.RequestFamilyAlphaSearch {
			writeEmptyAlphaSearchResponse(c.Writer)
			return
		}
		s.AddLogAsync(&model.LogEntry{
			Time:           model.JSONTime{Time: time.Now()},
			Model:          clientModel,
			LogSource:      model.LogSourceProxy,
			AuthTokenID:    tokenIDInt64,
			ClientProtocol: string(clientProtocol),
			StatusCode:     503,
			Message:        "no available upstream (all cooled or none)",
			IsStreaming:    isStreaming,
			ClientIP:       c.ClientIP(),
			ThinkingEffort: thinkingEffort,
		})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available upstream (all cooled or none)"})
		return
	}

	if tokenHashStr != "" {
		filtered, restricted := s.authService.FilterAllowedChannels(tokenHashStr, cands)
		if restricted {
			cands = filtered
			if len(cands) == 0 {
				c.JSON(http.StatusForbidden, gin.H{
					"error": "no allowed upstream channel for this token",
				})
				return
			}
		}
	}
	if routingSession != nil {
		if channelID, ok := routingSession.routeChannelSnapshot(); ok {
			cands = prioritizePinnedCodexChannel(cands, channelID)
		}
	}

	reqCtx := &proxyRequestContext{
		clientModel:    clientModel,
		originalModel:  originalModel,
		requestedModel: incoming.requestedModel,
		clientProtocol: clientProtocol,
		codexClient:    isCodexMultiAgentClient(codexMultiAgentUserAgent(c.Request.Header)),
		requestMethod:  requestMethod,
		requestPath:    effectiveRequestPath,
		rawQuery:       c.Request.URL.RawQuery,
		body:           all,
		translatedBody: all,
		header:         c.Request.Header,
		isStreaming:    isStreaming,
		tokenHash:      tokenHashStr,
		tokenID:        tokenIDInt64,
		clientIP:       c.ClientIP(),
		startTime:      startTime,
		thinkingEffort: thinkingEffort,
	}
	if routingSession != nil {
		reqCtx.routingSession = routingSession
	}
	if executionSession != nil {
		reqCtx.codexMultiAgentV2Optimized = executionSession.codexMultiAgentV2StateSnapshot()
	}
	ctx = withCodexMultiAgentV2RequestContext(ctx, reqCtx)
	if executionSession != nil && nativeRequestBody != nil {
		reqCtx.nativeCodexWS = executionSession.upstream
		reqCtx.nativeCodexBody = nativeRequestBody
	}
	reqCtx.observer = &ForwardObserver{
		OnBytesRead: func(n int64) {
			s.activeRequests.AddBytes(reqCtx.activeReqID, n)
		},
		OnFirstByteRead: func(firstByteTime time.Duration) {
			s.activeRequests.SetClientFirstByteTime(reqCtx.activeReqID, firstByteTime)
		},
		OnUpstreamWebsocket: func(upstreamWebsocket bool) {
			s.activeRequests.SetUpstreamWebsocket(reqCtx.activeReqID, upstreamWebsocket)
		},
		OnDebugCapture: func(dc *debugCapture) {
			s.activeRequests.SetDebugCapture(reqCtx.activeReqID, dc)
		},
	}
	defer func() {
		if reqCtx.activeReqID > 0 {
			s.activeRequests.Remove(reqCtx.activeReqID)
		}
	}()

	lastResult, succeeded := s.runProxyAttemptLoop(ctx, cands, reqCtx, c.Writer)
	if executionSession != nil {
		s.updateCodexMultiAgentV2SessionState(executionSession, reqCtx)
	}
	if succeeded {
		if executionSession != nil && lastResult != nil && lastResult.hasResponsesTurn {
			s.responsesExecutionSessions.commit(executionSession, executionSessionRequestBody, lastResult.responsesTurn)
		}
		return
	}

	s.writeFinalProxyResponse(c, reqCtx, isStreaming, lastResult, len(cands))
}

func determineFinalClientStatus(lastResult *proxyResult) int {
	if lastResult == nil || lastResult.status == 0 {
		return http.StatusServiceUnavailable
	}

	status := lastResult.status

	// 499处理：区分客户端取消 vs 上游返回的499
	if status == util.StatusClientClosedRequest {
		if lastResult.isClientCanceled {
			return status // 真正的客户端取消，透传499
		}
		return http.StatusBadGateway // 上游499，映射为502
	}

	// 仅映射内部状态码（596-599），其他全部透传
	return util.ClientStatusFor(status)
}

func (s *Server) clientFacingFinalStatus(codexClient bool, lastResult *proxyResult) int {
	status := determineFinalClientStatus(lastResult)
	if s.codexMap429To503 && codexClient && status == http.StatusTooManyRequests {
		return http.StatusServiceUnavailable
	}
	return status
}

func shouldStopTryingChannels(result *proxyResult) bool {
	if result == nil {
		return true
	}
	// 客户端取消：立即停止
	if result.isClientCanceled {
		return true
	}
	return result.nextAction == cooldown.ActionReturnClient
}

// enforceTokenLimits 检查 token 的模型限制与费用限额。
// 违规时已写响应并返回 false，调用方应直接 return。
func (s *Server) enforceTokenLimits(c *gin.Context, tokenHash, originalModel string) bool {
	// 检查令牌模型限制（2026-01新增）
	if tokenHash != "" && originalModel != "" {
		if !s.authService.IsModelAllowed(tokenHash, originalModel) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": fmt.Sprintf("model '%s' is not allowed for this token", originalModel),
			})
			return false
		}
	}

	// 检查令牌费用限额（2026-01新增）
	// 设计决策：在请求开始时检查，费用在请求完成后记账。
	// 超额窗口：预检（IsCostLimitExceeded/RLock）与记账（AddCostToCache/Lock）之间是
	// check-then-act。设了 max_concurrency 时最多超额并发上限个请求；未设上限时 N 个并发
	// 请求可同时通过预检后全部超额——费用最终都会记账，限额是“滞后 N 个请求才封顶”，非永久绕过。
	// 原因：费用只有在请求完成后才能精确计算（token数量由上游返回），此处只能做预检查。
	// 严格“先扣费后请求”需复杂的预估+退款机制，不值得（YAGNI）。
	if tokenHash != "" {
		usedMicro, limitMicro, window, exceeded := s.authService.costLimitState(tokenHash)
		if exceeded {
			used := util.MicroUSDToUSD(usedMicro)
			limit := util.MicroUSDToUSD(limitMicro)
			prefix := "Cost"
			switch window {
			case "daily":
				prefix = "Daily cost"
			case "monthly":
				prefix = "Monthly cost"
			case "total":
				prefix = "Total cost"
			}
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("%s limit exceeded: $%.2f used of $%.2f limit", prefix, used, limit),
					"type":    "insufficient_quota",
					"code":    "cost_limit_exceeded",
				},
			})
			return false
		}
	}

	return true
}

// runProxyAttemptLoop 按优先级遍历候选渠道。
// 返回最后一次结果（可能 nil），调用方据此决定是否兜底响应。
// succeeded 时内部已写响应，调用方应停止后续 writeFinal 步骤。
func (s *Server) runProxyAttemptLoop(
	ctx context.Context,
	cands []*model.Config,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (lastResult *proxyResult, succeeded bool) {
	return s.runProxyAttemptLoopWithFailureBoundary(ctx, cands, reqCtx, w, nil)
}

func (s *Server) runProxyAttemptLoopWithFailureBoundary(
	ctx context.Context,
	cands []*model.Config,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
	stopAfterFailure func(current, next *model.Config, result *proxyResult) bool,
) (lastResult *proxyResult, succeeded bool) {
	sawAlphaSearchUnsupported := false
	for index, cfg := range cands {
		result, err := s.tryChannelWithKeys(ctx, cfg, reqCtx, w)
		if err != nil && errors.Is(err, ErrNoAPIKeyForModel) {
			log.Printf("[INFO] 渠道 %s (ID=%d) 没有可用于模型 %s 的 Key，跳过该渠道", cfg.Name, cfg.ID, reqCtx.originalModel)
			continue
		}

		// 所有Key冷却：触发渠道级冷却(503)，防止后续请求重复尝试
		// 使用 cooldownManager.HandleError 统一处理（DRY原则）
		if err != nil && errors.Is(err, ErrAllKeysUnavailable) {
			// 统一走 applyCooldownDecision：断开取消链+按决策执行缓存失效
			s.applyCooldownDecision(ctx, cfg, httpErrorInputFromParts(cfg.ID, cooldown.NoKeyIndex, 503, nil, nil))
			continue
		}

		// [WARN] 所有Key验证失败，尝试下一个渠道
		if err != nil && errors.Is(err, ErrAllKeysExhausted) {
			log.Printf("[WARN] 渠道 %s (ID=%d) 所有Key验证失败，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if err != nil && errors.Is(err, ErrChannelRPMExceeded) {
			log.Printf("[INFO] 渠道 %s (ID=%d) 已达到RPM限制，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if err != nil && errors.Is(err, ErrChannelConcurrencyExceeded) {
			log.Printf("[INFO] 渠道 %s (ID=%d) 已达到并发限制，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if result != nil {
			if result.protocolCapabilityMissing && protocol.DetectRequestFamily(reqCtx.requestPath) == protocol.RequestFamilyAlphaSearch {
				sawAlphaSearchUnsupported = true
			}
			if result.succeeded {
				return result, true
			}

			lastResult = result

			// 客户端已取消：别再浪费资源“重试”了。
			if result.isClientCanceled {
				break
			}

			if shouldStopTryingChannels(result) {
				break
			}

			if stopAfterFailure != nil && index+1 < len(cands) &&
				stopAfterFailure(cfg, cands[index+1], result) {
				break
			}
		}
	}
	if sawAlphaSearchUnsupported &&
		protocol.DetectRequestFamily(reqCtx.requestPath) == protocol.RequestFamilyAlphaSearch &&
		(lastResult == nil || (!lastResult.isClientCanceled && lastResult.nextAction != cooldown.ActionReturnClient)) {
		writeEmptyAlphaSearchResponse(w)
		return &proxyResult{status: http.StatusOK, succeeded: true, nextAction: cooldown.ActionReturnClient}, true
	}

	return lastResult, false
}

func writeEmptyAlphaSearchResponse(w http.ResponseWriter) {
	header := make(http.Header, 2)
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("X-CCLoad-Search-Fallback", "empty")
	writeResponseWithHeaders(w, http.StatusOK, header, []byte(`{"encrypted_output":null,"output":"","results":[]}`))
}

// writeFinalProxyResponse 所有渠道失败时写最终响应：
// 计算 finalStatus、决定 skipLog、透传 body 或 JSON 错误。
func (s *Server) writeFinalProxyResponse(
	c *gin.Context,
	reqCtx *proxyRequestContext,
	isStreaming bool,
	lastResult *proxyResult,
	candidateCount int,
) {
	// 所有渠道都失败：返回“最后一次实际失败”的状态码（并映射内部状态码），避免一律伪装成503。
	upstreamFinalStatus := determineFinalClientStatus(lastResult)
	finalStatus := s.clientFacingFinalStatus(reqCtx.codexClient, lastResult)

	msg := "exhausted backends"
	if lastResult != nil && lastResult.isClientCanceled {
		msg = "client closed request (context canceled)"
	} else if lastResult != nil && lastResult.status == 499 && finalStatus != 499 {
		// 上游返回 499 没有任何“客户端取消”的语义价值：对外统一视为网关错误。
		msg = "upstream returned 499 (mapped)"
	} else if upstreamFinalStatus != finalStatus {
		msg = fmt.Sprintf("upstream status %d (mapped to %d for Codex)", upstreamFinalStatus, finalStatus)
	} else if finalStatus != http.StatusServiceUnavailable {
		msg = fmt.Sprintf("upstream status %d", finalStatus)
	}

	// 过滤不需要汇总日志的场景
	// - 客户端取消（499）：已在 handleNetworkError 中记录渠道级日志
	// - 客户端错误（400）：已在渠道级日志记录，汇总日志冗余
	// - 候选池 ≤1：实际只尝试了 1 个渠道，渠道级日志已完整反映失败原因，汇总日志冗余
	skipLog := lastResult != nil && (lastResult.isClientCanceled || finalStatus == http.StatusBadRequest)
	skipLog = skipLog || candidateCount <= 1
	if !skipLog {
		s.AddLogAsync(&model.LogEntry{
			Time:           model.JSONTime{Time: reqCtx.startTime},
			Model:          reqCtx.requestLogModel(),
			LogSource:      model.LogSourceProxy,
			ClientProtocol: string(reqCtx.clientProtocol),
			StatusCode:     upstreamFinalStatus,
			Message:        msg,
			Duration:       time.Since(reqCtx.startTime).Seconds(),
			IsStreaming:    isStreaming,
			ClientIP:       reqCtx.clientIP,
		})
	}

	if lastResult != nil && lastResult.status != 0 {
		// 透明代理原则：透传所有上游响应（状态码+header+body）
		writeResponseWithHeaders(c.Writer, finalStatus, lastResult.header, lastResult.body)
		return
	}

	disableResponseWriteTimeout(c.Writer, "最终响应")
	c.JSON(finalStatus, gin.H{"error": "no upstream available"})
}
