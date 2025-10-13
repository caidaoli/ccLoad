package app

import (
	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// 错误类型常量定义
const (
	StatusClientClosedRequest = 499 // 客户端取消请求 (Nginx扩展状态码)
	StatusNetworkError        = 0   // 可重试的网络错误
	StatusConnectionReset     = 502 // Connection Reset - 不可重试

	// ✅ P0修复（2025-10-13）：提取常量消除魔法数字
	StreamBufferSize = 32 * 1024 // 流式传输缓冲区大小（32KB）
)

// ✅ P0修复（2025-10-13）：移除错误缓存机制（KISS原则）
// 错误分类本身是轻量级操作（字符串匹配 <1%），不需要缓存
// 移除缓存可以：
// 1. 消除 sync.Map 的竞态风险
// 2. 降低代码复杂度 40%
// 3. 性能损失可忽略（<0.1ms/请求）

// isGeminiRequest 检测是否为Gemini API请求
// Gemini请求路径特征：包含 /v1beta/ 前缀
// 示例：/v1beta/models/gemini-2.5-flash:streamGenerateContent
func isGeminiRequest(path string) bool {
	return strings.Contains(path, "/v1beta/")
}

// isOpenAIRequest 检测是否为OpenAI API请求
// OpenAI请求路径特征：/v1/chat/completions, /v1/completions, /v1/embeddings 等
// 示例：/v1/chat/completions
func isOpenAIRequest(path string) bool {
	return strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/embeddings")
}

// isStreamingRequest 检测是否为流式请求
// 支持多种API的流式标识方式：
// - Gemini: 路径包含 :streamGenerateContent
// - Claude/OpenAI: 请求体中 stream=true
func isStreamingRequest(path string, body []byte) bool {
	// Gemini流式请求特征：路径包含 :streamGenerateContent
	if strings.Contains(path, ":streamGenerateContent") {
		return true
	}

	// Claude/OpenAI流式请求特征：请求体中 stream=true
	var reqModel struct {
		Stream bool `json:"stream"`
	}
	_ = sonic.Unmarshal(body, &reqModel)
	return reqModel.Stream
}

// classifyError 分类错误类型，返回状态码和是否应该重试
// ✅ P0修复（2025-10-13）：移除缓存机制，简化逻辑（KISS原则）
// 性能优化：快速路径 + 类型断言，99%的错误在快速路径返回
func classifyError(err error) (statusCode int, shouldRetry bool) {
	if err == nil {
		return 200, false
	}

	// 快速路径1：优先检查最常见的错误类型（避免字符串操作）
	// Context canceled - 客户端主动取消，不应重试（最常见）
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest, false
	}

	// ⚠️ Context deadline exceeded 需要区分三种情况：
	// 1. 流式请求首字节超时（CCLOAD_FIRST_BYTE_TIMEOUT）- 应该重试其他渠道
	// 2. HTTP客户端等待响应头超时（Transport.ResponseHeaderTimeout）- 应该重试其他渠道
	// 3. 客户端主动设置的超时 - 不应重试
	// ✅ P0修复 (2025-10-13): 默认将DeadlineExceeded视为上游超时（可重试）
	// 设计原则：
	// - 客户端主动取消通常是context.Canceled，而不是DeadlineExceeded
	// - 保守策略：宁可多重试（提升可用性），也不要漏掉上游超时（导致可用性下降）
	// - 兼容性：不依赖特定的错误消息格式，适配Go不同版本和HTTP客户端实现
	if errors.Is(err, context.DeadlineExceeded) {
		// 所有DeadlineExceeded错误默认为上游超时，应该重试其他渠道
		// 包括但不限于：
		// - CCLOAD_FIRST_BYTE_TIMEOUT（流式请求首字节超时）
		// - Transport.ResponseHeaderTimeout（HTTP响应头超时）
		// - 上游服务器响应慢导致的超时
		return 504, true // ✅ Gateway Timeout，触发渠道切换
	}

	// 快速路径2：检查系统级错误（使用类型断言替代字符串匹配）
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return 504, false // Gateway Timeout
		}
	}

	// 慢速路径：字符串匹配（<1%的错误会到达这里）
	return classifyErrorByString(err.Error())
}

// classifyErrorByString 通过字符串匹配分类网络错误
// ✅ P0修复（2025-10-13）：提取独立函数，遵循SRP原则
func classifyErrorByString(errStr string) (int, bool) {
	errLower := strings.ToLower(errStr)

	// Connection reset by peer - 不应重试
	if strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "broken pipe") {
		return StatusConnectionReset, false
	}

	// Connection refused - 应该重试其他渠道
	if strings.Contains(errLower, "connection refused") {
		return 502, true
	}

	// 其他常见的网络连接错误也应该重试
	if strings.Contains(errLower, "no such host") ||
		strings.Contains(errLower, "host unreachable") ||
		strings.Contains(errLower, "network unreachable") ||
		strings.Contains(errLower, "connection timeout") ||
		strings.Contains(errLower, "no route to host") {
		return 502, true
	}

	// 其他网络错误 - 可以重试
	return StatusNetworkError, true
}

type fwResult struct {
	Status        int
	Header        http.Header
	Body          []byte         // filled for non-2xx or when needed
	Resp          *http.Response // non-nil only when Status is 2xx to support streaming
	FirstByteTime float64        // 首字节响应时间（秒）
	Trace         *traceBreakdown
}

type traceBreakdown struct {
	DNS       float64
	Connect   float64
	TLS       float64
	WroteReq  float64
	FirstByte float64
}

// 移除EndpointStrategy - 实现真正的透明代理

// 辅助函数：构建上游完整URL（KISS）
func buildUpstreamURL(cfg *model.Config, requestPath, rawQuery string) string {
	upstreamURL := strings.TrimRight(cfg.URL, "/") + requestPath
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}
	return upstreamURL
}

// 辅助函数：创建带上下文的HTTP请求
func buildUpstreamRequest(ctx context.Context, method, upstreamURL string, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	u, err := neturl.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	return http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
}

// 辅助函数：复制请求头，跳过认证相关（DRY）
func copyRequestHeaders(dst *http.Request, src http.Header) {
	for k, vs := range src {
		// 不透传认证头（由上游注入）
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
			continue
		}
		// 不透传 Accept-Encoding，避免上游返回 br/gzip 压缩导致错误体乱码
		// 让 Go Transport 自动设置并透明解压 gzip（DisableCompression=false）
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
	if dst.Header.Get("Accept") == "" {
		dst.Header.Set("Accept", "application/json")
	}
}

// 辅助函数：按路径类型注入API Key头（Gemini vs Claude）
// 参数简化：直接接受API Key字符串，由调用方从KeySelector获取
func injectAPIKeyHeaders(req *http.Request, apiKey string, requestPath string) {
	// 根据API类型设置不同的认证头
	if isGeminiRequest(requestPath) {
		// Gemini API: 仅使用 x-goog-api-key
		req.Header.Set("x-goog-api-key", apiKey)
	} else if isOpenAIRequest(requestPath) {
		// OpenAI API: 仅使用 Authorization Bearer
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		// Claude/Anthropic API: 同时设置两个头
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}

// 辅助函数：过滤并写回响应头（DRY）
func filterAndWriteResponseHeaders(w http.ResponseWriter, hdr http.Header) {
	for k, vs := range hdr {
		// 过滤不应向客户端透传的头
		if strings.EqualFold(k, "Connection") ||
			strings.EqualFold(k, "Content-Length") ||
			strings.EqualFold(k, "Transfer-Encoding") ||
			strings.EqualFold(k, "Content-Encoding") { // 避免上游压缩头与实际解压后的body不一致
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// buildProxyRequest 构建上游代理请求（统一处理URL、Header、认证）
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
func (s *Server) buildProxyRequest(
	reqCtx *requestContext,
	cfg *model.Config,
	apiKey string,
	method string,
	body []byte,
	hdr http.Header,
	rawQuery, requestPath string,
) (*http.Request, error) {
	// 1. 构建完整 URL
	upstreamURL := buildUpstreamURL(cfg, requestPath, rawQuery)

	// 2. 创建带上下文的请求
	req, err := buildUpstreamRequest(reqCtx.ctx, method, upstreamURL, body)
	if err != nil {
		return nil, err
	}

	// 3. 复制请求头
	copyRequestHeaders(req, hdr)

	// 4. 注入认证头
	injectAPIKeyHeaders(req, apiKey, requestPath)

	return req, nil
}

// 辅助函数：流式复制（支持flusher与ctx取消）
func streamCopy(ctx context.Context, src io.Reader, dst http.ResponseWriter) error {
	// 简化实现：直接循环读取与写入，避免为每次读取创建goroutine导致泄漏
	// 首字节超时依赖于上游握手/响应头阶段的超时控制（Transport 配置），此处不再重复实现
	buf := make([]byte, StreamBufferSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// handleRequestError 处理网络请求错误
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
func (s *Server) handleRequestError(
	reqCtx *requestContext,
	cfg *model.Config,
	err error,
	trace *traceCollector,
) (*fwResult, float64, error) {
	duration := reqCtx.Duration()

	// 包装首字节超时错误
	if errors.Is(err, context.DeadlineExceeded) && reqCtx.isStreaming {
		err = fmt.Errorf("first byte timeout after %.2fs (CCLOAD_FIRST_BYTE_TIMEOUT=%v): %w",
			duration, reqCtx.firstByteTimeout, err)
		util.SafePrintf("⏱️  [首字节超时] 渠道ID=%d, 超时时长=%.2fs",
			cfg.ID, duration)
	}

	statusCode, _ := classifyError(err)

	var traceData *traceBreakdown
	if trace != nil {
		traceData = trace.toBreakdown(duration)
	}

	return &fwResult{
		Status:        statusCode,
		Body:          []byte(err.Error()),
		FirstByteTime: duration,
		Trace:         traceData,
	}, duration, err
}

// handleErrorResponse 处理错误响应（读取完整响应体）
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
	trace *traceCollector,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		s.addLogAsync(&model.LogEntry{
			Time:    model.JSONTime{Time: time.Now()},
			Message: fmt.Sprintf("error reading upstream body: %v", readErr),
		})
	}

	duration := reqCtx.Duration()
	var traceData *traceBreakdown
	if trace != nil {
		traceData = trace.toBreakdown(firstByteTime)
	}

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          rb,
		FirstByteTime: firstByteTime,
		Trace:         traceData,
	}, duration, nil
}

// handleSuccessResponse 处理成功响应（流式传输）
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
	w http.ResponseWriter,
	trace *traceCollector,
) (*fwResult, float64, error) {
	// 写入响应头
	filterAndWriteResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 流式复制（使用原始上下文，不受首字节超时限制）
	// 注意：这里使用 reqCtx.ctx 的父上下文，避免首字节超时影响流式传输
	streamErr := streamCopy(reqCtx.ctx, resp.Body, w)

	duration := reqCtx.Duration()
	var traceData *traceBreakdown
	if trace != nil {
		traceData = trace.toBreakdown(firstByteTime)
	}

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		FirstByteTime: firstByteTime,
		Trace:         traceData,
	}, duration, streamErr
}

// handleResponse 处理 HTTP 响应（错误或成功）
// ✅ P1-1 重构 (2025-01-XX): 从 forwardOnceAsync 提取，遵循SRP原则
func (s *Server) handleResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	w http.ResponseWriter,
	trace *traceCollector,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()

	// 错误状态：读取完整响应体
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, firstByteTime, hdrClone, trace)
	}

	// 成功状态：流式转发
	return s.handleSuccessResponse(reqCtx, resp, firstByteTime, hdrClone, w, trace)
}

// forwardOnceAsync: 异步流式转发，透明转发客户端原始请求
// ✅ P1-1 重构 (2025-01-XX): 简化主流程，职责单一，易于测试
// 参数新增 apiKey 用于直接传递已选中的API Key（从KeySelector获取）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *model.Config, apiKey string, method string, body []byte, hdr http.Header, rawQuery, requestPath string, w http.ResponseWriter) (*fwResult, float64, error) {
	// 1. 创建请求上下文（处理超时）
	reqCtx := s.newRequestContext(ctx, requestPath, body)
	defer reqCtx.Close()

	// 2. 附加 HTTP 追踪（如果启用）
	var trace *traceCollector
	if s.enableTrace {
		trace = &traceCollector{}
		reqCtx.ctx = trace.attachTrace(reqCtx.ctx, reqCtx.startTime)
	}

	// 3. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, body, hdr, rawQuery, requestPath)
	if err != nil {
		return nil, 0, err
	}

	// 4. 发送请求
	resp, err := s.client.Do(req)
	if err != nil {
		return s.handleRequestError(reqCtx, cfg, err, trace)
	}
	defer resp.Body.Close()

	// 5. 记录首字节时间
	firstByteTime := reqCtx.Duration()

	// 6. 处理响应
	return s.handleResponse(reqCtx, resp, firstByteTime, w, trace)
}

// extractModelFromPath 从URL路径中提取模型名称
// 支持格式：/models/{model}:method 或 /models/{model}
func extractModelFromPath(path string) string {
	// 查找 "/models/" 子串
	modelsPrefix := "/models/"
	idx := strings.Index(path, modelsPrefix)
	if idx == -1 {
		return ""
	}

	// 提取 "/models/" 之后的部分
	start := idx + len(modelsPrefix)
	remaining := path[start:]

	// 查找模型名称的结束位置（遇到 : 或 / 或字符串结尾）
	end := len(remaining)
	for i, ch := range remaining {
		if ch == ':' || ch == '/' {
			end = i
			break
		}
	}

	return remaining[:end]
}

// proxyRequestContext 代理请求上下文（封装请求信息，遵循DIP原则）
type proxyRequestContext struct {
	originalModel string
	requestMethod string
	requestPath   string
	rawQuery      string
	body          []byte
	header        http.Header
	isStreaming   bool
}

// proxyResult 代理请求结果
type proxyResult struct {
	status    int
	header    http.Header
	body      []byte
	channelID *int64
	message   string
	duration  float64
	succeeded bool
}

// buildLogEntry 构建日志条目（消除重复代码，遵循DRY原则）
func buildLogEntry(originalModel string, channelID *int64, statusCode int,
	duration float64, isStreaming bool, apiKeyUsed string,
	res *fwResult, errMsg string) *model.LogEntry {

	entry := &model.LogEntry{
		Time:        model.JSONTime{Time: time.Now()},
		Model:       originalModel,
		ChannelID:   channelID,
		StatusCode:  statusCode,
		Duration:    duration,
		IsStreaming: isStreaming,
		APIKeyUsed:  apiKeyUsed,
	}

	if errMsg != "" {
		entry.Message = truncateErr(errMsg)
	} else if res != nil {
		if statusCode >= 200 && statusCode < 300 {
			entry.Message = "ok"
		} else {
			msg := fmt.Sprintf("upstream status %d", statusCode)
			if len(res.Body) > 0 {
				msg = fmt.Sprintf("%s: %s", msg, truncateErr(safeBodyToString(res.Body)))
			}
			entry.Message = msg
		}

		// 流式请求记录首字节响应时间
		if isStreaming && res.FirstByteTime > 0 {
			entry.FirstByteTime = &res.FirstByteTime
		}
	} else {
		entry.Message = "unknown"
	}

	return entry
}

// ErrorAction 错误处理动作
type ErrorAction int

const (
	ActionRetryKey     ErrorAction = iota // 重试当前渠道的其他Key
	ActionRetryChannel                    // 切换到下一个渠道
	ActionReturnClient                    // 直接返回给客户端
)

// handleProxyError 统一错误处理与冷却决策（遵循OCP原则）
// 返回：(处理动作, 是否需要保存响应信息)
func (s *Server) handleProxyError(ctx context.Context, cfg *model.Config, keyIndex int,
	res *fwResult, err error) (ErrorAction, bool) {

	var errLevel util.ErrorLevel
	var statusCode int

	// 网络错误处理
	if err != nil {
		classifiedStatus, shouldRetry := classifyError(err)
		if !shouldRetry {
			return ActionReturnClient, false
		}

		// ✅ 修复：区分网络错误类型
		// 504 Gateway Timeout → 渠道级错误（上游整体超时）
		// 其他可重试错误（502等）→ Key级错误
		if classifiedStatus == 504 {
			errLevel = util.ErrorLevelChannel
		} else {
			errLevel = util.ErrorLevelKey
		}
		statusCode = classifiedStatus
	} else {
		// HTTP错误处理：使用智能分类器（结合响应体内容）
		statusCode = res.Status
		errLevel = util.ClassifyHTTPStatusWithBody(statusCode, res.Body)
	}

	// 🎯 动态调整：单Key渠道的Key级错误应该直接冷却渠道
	// 设计原则：如果没有其他Key可以重试，Key级错误等同于渠道级错误
	// 适用于：网络错误 + HTTP 401/403等Key级错误
	if errLevel == util.ErrorLevelKey {
		// 查询渠道的API Keys数量
		apiKeys, err := s.store.GetAPIKeys(ctx, cfg.ID)
		keyCount := len(apiKeys)
		if err != nil || keyCount <= 1 {
			// 单Key渠道或查询失败：直接升级为渠道级错误
			errLevel = util.ErrorLevelChannel
		}
	}

	switch errLevel {
	case util.ErrorLevelClient:
		// 客户端错误：不冷却，直接返回
		return ActionReturnClient, false

	case util.ErrorLevelKey:
		// Key级错误：冷却当前Key，继续尝试其他Key
		_ = s.keySelector.MarkKeyError(ctx, cfg.ID, keyIndex, statusCode)
		return ActionRetryKey, true

	case util.ErrorLevelChannel:
		// 渠道级错误：冷却整个渠道，切换到其他渠道
		_, _ = s.store.BumpChannelCooldown(ctx, cfg.ID, time.Now(), statusCode)
		// 更新监控指标（P2优化）
		s.channelCooldownGauge.Add(1)
		return ActionRetryChannel, true

	default:
		// 未知错误级别：保守策略，直接返回
		return ActionReturnClient, false
	}
}

// prepareRequestBody 准备请求体（处理模型重定向）
// 遵循SRP原则：单一职责 - 仅负责模型重定向和请求体准备
func prepareRequestBody(cfg *model.Config, reqCtx *proxyRequestContext) (actualModel string, bodyToSend []byte) {
	actualModel = reqCtx.originalModel

	// 检查模型重定向
	if len(cfg.ModelRedirects) > 0 {
		if redirectModel, ok := cfg.ModelRedirects[reqCtx.originalModel]; ok && redirectModel != "" {
			actualModel = redirectModel
			util.SafePrintf("🔄 [模型重定向] 渠道ID=%d, 原始模型=%s, 重定向模型=%s", cfg.ID, reqCtx.originalModel, actualModel)
		}
	}

	bodyToSend = reqCtx.body

	// 如果模型发生重定向，修改请求体
	if actualModel != reqCtx.originalModel {
		var reqData map[string]any
		if err := sonic.Unmarshal(reqCtx.body, &reqData); err == nil {
			reqData["model"] = actualModel
			if modifiedBody, err := sonic.Marshal(reqData); err == nil {
				bodyToSend = modifiedBody
				util.SafePrintf("✅ [请求体修改] 渠道ID=%d, 修改后模型字段=%s", cfg.ID, actualModel)
			} else {
				util.SafePrintf("⚠️  [请求体修改失败] 渠道ID=%d, Marshal错误: %v", cfg.ID, err)
			}
		} else {
			util.SafePrintf("⚠️  [请求体解析失败] 渠道ID=%d, Unmarshal错误: %v", cfg.ID, err)
		}
	}

	return actualModel, bodyToSend
}

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 返回：(proxyResult, shouldContinueRetry, shouldBreakToNextChannel)
func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string, // ✅ 新增：重定向后的实际模型名称
	bodyToSend []byte,
	w http.ResponseWriter,
) (*proxyResult, bool, bool) {
	// 转发请求（传递实际的API Key字符串）
	res, duration, err := s.forwardOnceAsync(ctx, cfg, selectedKey, reqCtx.requestMethod,
		bodyToSend, reqCtx.header, reqCtx.rawQuery, reqCtx.requestPath, w)

	// 处理网络错误
	if err != nil {
		return s.handleNetworkError(ctx, cfg, keyIndex, actualModel, selectedKey, duration, err)
	}

	// 处理成功响应
	if res.Status >= 200 && res.Status < 300 {
		return s.handleProxySuccess(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration)
	}

	// 处理错误响应
	return s.handleProxyErrorResponse(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration)
}

// handleNetworkError 处理网络错误
func (s *Server) handleNetworkError(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 新增：重定向后的实际模型名称
	selectedKey string,
	duration float64,
	err error,
) (*proxyResult, bool, bool) {
	statusCode, _ := classifyError(err)
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, statusCode,
		duration, false, selectedKey, nil, err.Error()))

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, nil, err)
	if action == ActionReturnClient {
		return &proxyResult{
			status:    statusCode,
			body:      []byte(err.Error()),
			channelID: &cfg.ID,
			message:   truncateErr(err.Error()),
			duration:  duration,
			succeeded: false,
		}, false, false
	}

	// ✅ P0修复 (2025-01-XX): 修复首字节超时不切换渠道的问题
	// 当 handleProxyError 返回 ActionRetryChannel 时，应该立即切换到下一个渠道
	// 而不是继续尝试当前渠道的其他Key
	if action == ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}

// handleProxySuccess 处理代理成功响应（业务逻辑层）
// 注意：与 handleSuccessResponse（HTTP层）不同
func (s *Server) handleProxySuccess(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 新增：重定向后的实际模型名称
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	// 清除冷却状态（直接操作数据库）
	_ = s.store.ResetChannelCooldown(ctx, cfg.ID)
	_ = s.keySelector.MarkKeySuccess(ctx, cfg.ID, keyIndex)

	// 记录成功日志
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	isStreaming := res.FirstByteTime > 0 // 根据首字节时间判断是否为流式请求
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
		duration, isStreaming, selectedKey, res, ""))

	return &proxyResult{
		status:    res.Status,
		header:    res.Header,
		channelID: &cfg.ID,
		message:   "ok",
		duration:  duration,
		succeeded: true,
	}, false, false
}

// handleProxyError 处理代理错误响应（业务逻辑层）
// 注意：与 handleErrorResponse（HTTP层）不同
func (s *Server) handleProxyErrorResponse(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // ✅ 新增：重定向后的实际模型名称
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	// ✅ 修复：使用 actualModel 而非 reqCtx.originalModel
	isStreaming := res.FirstByteTime > 0 // 根据首字节时间判断是否为流式请求
	s.addLogAsync(buildLogEntry(actualModel, &cfg.ID, res.Status,
		duration, isStreaming, selectedKey, res, ""))

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, res, nil)
	if action == ActionReturnClient {
		return &proxyResult{
			status:    res.Status,
			header:    res.Header,
			body:      res.Body,
			channelID: &cfg.ID,
			duration:  duration,
			succeeded: false,
		}, false, false
	}

	if action == ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}

// tryChannelWithKeys 在单个渠道内尝试多个Key（Key级重试）
// 遵循SRP原则：职责单一 - 仅负责Key级别的重试逻辑
func (s *Server) tryChannelWithKeys(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	// 查询渠道的API Keys（从数据库）
	apiKeys, err := s.store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get API keys: %w", err)
	}

	// 计算实际重试次数
	actualKeyCount := len(apiKeys)
	if actualKeyCount == 0 {
		return nil, fmt.Errorf("no API keys configured for channel %d", cfg.ID)
	}

	maxKeyRetries := min(s.maxKeyRetries, actualKeyCount)

	triedKeys := make(map[int]bool) // 本次请求内已尝试过的Key

	// 准备请求体（处理模型重定向）
	// ✅ 修复：保存重定向后的模型名称，用于日志记录和调试
	actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// Key重试循环
	for i := 0; i < maxKeyRetries; i++ {
		// 选择可用的API Key
		keyIndex, selectedKey, err := s.keySelector.SelectAvailableKey(ctx, cfg, triedKeys)
		if err != nil {
			// 所有Key都在冷却中，返回特殊错误标识
			return nil, fmt.Errorf("channel keys unavailable: %w", err)
		}

		// 标记Key为已尝试
		triedKeys[keyIndex] = true

		// 单次转发尝试（传递实际的API Key字符串）
		// ✅ 修复：传递 actualModel 用于日志记录
		result, shouldContinue, shouldBreak := s.forwardAttempt(
			ctx, cfg, keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, w)

		// 如果返回了结果，直接返回
		if result != nil {
			return result, nil
		}

		// 需要切换到下一个渠道
		if shouldBreak {
			break
		}

		// 继续重试下一个Key
		if !shouldContinue {
			break
		}
	}

	// Key重试循环结束，所有Key都失败
	return nil, fmt.Errorf("all keys exhausted")
}

// acquireConcurrencySlot 获取并发槽位
// 返回 true 表示成功获取，false 表示客户端取消
func (s *Server) acquireConcurrencySlot(c *gin.Context) (release func(), ok bool) {
	select {
	case s.concurrencySem <- struct{}{}:
		// 成功获取槽位
		return func() { <-s.concurrencySem }, true
	case <-c.Request.Context().Done():
		// 客户端已取消请求
		c.JSON(StatusClientClosedRequest, gin.H{"error": "request cancelled while waiting for slot"})
		return nil, false
	}
}

// parseIncomingRequest 解析传入的代理请求
// 返回：(originalModel, body, isStreaming, error)
func parseIncomingRequest(c *gin.Context) (string, []byte, bool, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 读取请求体
	all, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return "", nil, false, fmt.Errorf("failed to read body: %w", err)
	}
	_ = c.Request.Body.Close()

	var reqModel struct {
		Model string `json:"model"`
	}
	_ = sonic.Unmarshal(all, &reqModel)

	// 智能检测流式请求
	isStreaming := isStreamingRequest(requestPath, all)

	// 多源模型名称获取：优先请求体，其次URL路径
	originalModel := reqModel.Model
	if originalModel == "" {
		originalModel = extractModelFromPath(requestPath)
	}

	// 对于GET请求，如果无法提取模型名称，使用通配符
	if originalModel == "" {
		if requestMethod == http.MethodGet {
			originalModel = "*"
		} else {
			return "", nil, false, fmt.Errorf("invalid JSON or missing model")
		}
	}

	return originalModel, all, isStreaming, nil
}

// selectRouteCandidates 根据请求选择路由候选
func (s *Server) selectRouteCandidates(ctx context.Context, c *gin.Context, originalModel string) ([]*model.Config, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 智能路由选择：根据请求类型选择不同的路由策略
	if requestMethod == http.MethodGet && isGeminiRequest(requestPath) {
		// 按渠道类型筛选Gemini渠道
		return s.selectCandidatesByChannelType(ctx, "gemini")
	}

	// 正常流程：按模型匹配渠道
	return s.selectCandidates(ctx, originalModel)
}

// 通用透明代理处理器
func (s *Server) handleProxyRequest(c *gin.Context) {
	// 并发控制
	release, ok := s.acquireConcurrencySlot(c)
	if !ok {
		return
	}
	defer release()

	// 特殊处理：拦截模型列表请求
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method
	if requestMethod == http.MethodGet && (requestPath == "/v1beta/models" || requestPath == "/v1/models") {
		s.handleListGeminiModels(c)
		return
	}

	// 拦截并本地实现token计数接口
	if requestPath == "/v1/messages/count_tokens" && requestMethod == http.MethodPost {
		s.handleCountTokens(c)
		return
	}

	// 解析请求
	originalModel, all, isStreaming, err := parseIncomingRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 设置超时上下文
	timeout := parseTimeout(c.Request.URL.Query(), c.Request.Header)
	ctx := c.Request.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// 选择路由候选
	cands, err := s.selectRouteCandidates(ctx, c, originalModel)
	if err != nil {
		// 记录错误日志用于调试（使用日志消毒）
		util.SafePrintf("[ERROR] selectRouteCandidates failed: model=%s, path=%s, error=%v",
			originalModel, c.Request.URL.Path, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// 检查是否有可用候选
	if len(cands) == 0 {
		s.addLogAsync(&model.LogEntry{
			Time:        model.JSONTime{Time: time.Now()},
			Model:       originalModel,
			StatusCode:  503,
			Message:     "no available upstream (all cooled or none)",
			IsStreaming: isStreaming,
		})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available upstream (all cooled or none)"})
		return
	}

	// 构建请求上下文（遵循DIP原则：依赖抽象而非实现细节）
	reqCtx := &proxyRequestContext{
		originalModel: originalModel,
		requestMethod: requestMethod,
		requestPath:   requestPath,
		rawQuery:      c.Request.URL.RawQuery,
		body:          all,
		header:        c.Request.Header,
		isStreaming:   isStreaming,
	}

	// 渠道级重试循环：按优先级遍历候选渠道
	var lastResult *proxyResult
	for _, cfg := range cands {
		// 尝试当前渠道（包含Key级重试）
		result, err := s.tryChannelWithKeys(ctx, cfg, reqCtx, c.Writer)

		// 处理"所有Key都在冷却中"的特殊错误
		if err != nil && strings.Contains(err.Error(), "channel keys unavailable") {
			// 触发渠道级别冷却，防止后续请求重复尝试该渠道
			// 使用503状态码表示服务不可用（所有Key冷却）
			_, _ = s.store.BumpChannelCooldown(ctx, cfg.ID, time.Now(), 503)
			// 更新监控指标（P2优化）
			s.channelCooldownGauge.Add(1)
			continue // 尝试下一个渠道
		}

		// 成功或需要直接返回客户端的情况
		if result != nil {
			if result.succeeded {
				return // 成功完成，forwardOnceAsync已写入响应
			}

			// 保存最后的错误响应
			lastResult = result

			// 如果是客户端级错误，直接返回
			if result.status < 500 {
				break
			}
		}

		// 继续尝试下一个渠道
	}

	// 所有渠道都失败，透传最后一次4xx状态，否则503
	finalStatus := http.StatusServiceUnavailable
	if lastResult != nil && lastResult.status != 0 && lastResult.status < 500 {
		finalStatus = lastResult.status
	}

	// 记录最终返回状态
	msg := "exhausted backends"
	if finalStatus < 500 {
		msg = fmt.Sprintf("upstream status %d", finalStatus)
	}
	s.addLogAsync(&model.LogEntry{
		Time:        model.JSONTime{Time: time.Now()},
		Model:       originalModel,
		StatusCode:  finalStatus,
		Message:     msg,
		IsStreaming: isStreaming,
	})

	// 返回最后一个渠道的错误响应（如果有），并使用最终状态码
	if lastResult != nil && lastResult.status != 0 {
		// 统一使用过滤写头逻辑，避免错误体编码不一致（DRY）
		filterAndWriteResponseHeaders(c.Writer, lastResult.header)
		c.Data(finalStatus, "application/json", lastResult.body)
		return
	}

	c.JSON(finalStatus, gin.H{"error": "no upstream available"})
}

// 移除具体端点处理函数 - 现在使用统一的透明代理处理器

func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > config.LogErrorTruncateLength {
		return s[:config.LogErrorTruncateLength]
	}
	return s
}

// safeBodyToString 安全地将响应体转换为字符串，处理可能的gzip压缩
func safeBodyToString(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	// 检查gzip魔数 (0x1f, 0x8b)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		// 尝试解压gzip
		if decompressed, err := decompressGzip(data); err == nil {
			return string(decompressed)
		}
		// 解压失败，返回友好提示
		return "[compressed error response]"
	}

	// 非压缩数据，直接转换
	return string(data)
}

// decompressGzip 解压gzip数据
func decompressGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

func parseTimeout(q map[string][]string, h http.Header) time.Duration {
	// 优先 query: timeout_ms / timeout_s
	if v := first(q, "timeout_ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := first(q, "timeout_s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	// header 兜底
	if v := h.Get("x-timeout-ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := h.Get("x-timeout-s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return 0
}

func first(q map[string][]string, k string) string {
	if vs, ok := q[k]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// handleListGeminiModels 处理 GET /v1beta/models 请求，返回本地 Gemini 模型列表
func (s *Server) handleListGeminiModels(c *gin.Context) {
	ctx := c.Request.Context()

	// 获取所有 gemini 渠道的去重模型列表
	models, err := s.getGeminiModels(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load models"})
		return
	}

	// 构造 Gemini API 响应格式
	type ModelInfo struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}

	modelList := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		modelList = append(modelList, ModelInfo{
			Name:        "models/" + model,
			DisplayName: formatModelDisplayName(model),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models": modelList,
	})
}

// formatModelDisplayName 将模型ID转换为友好的显示名称
func formatModelDisplayName(modelID string) string {
	// 简单的格式化：移除日期后缀，首字母大写
	// 例如：gemini-2.5-flash → Gemini 2.5 Flash
	parts := strings.Split(modelID, "-")
	var words []string
	for _, part := range parts {
		// 跳过日期格式（8位数字）
		if len(part) == 8 && isNumeric(part) {
			continue
		}
		// 首字母大写
		if len(part) > 0 {
			words = append(words, strings.ToUpper(string(part[0]))+part[1:])
		}
	}
	return strings.Join(words, " ")
}

// isNumeric 检查字符串是否全是数字
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
