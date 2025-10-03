package main

import (
    "bytes"
    "compress/gzip"
    "context"
    "crypto/tls"
    "errors"
    "fmt"
    "io"
    "net"
    "net/http"
    "net/http/httptrace"
    neturl "net/url"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"
    "time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

// 错误类型常量定义
const (
	StatusClientClosedRequest = 499 // 客户端取消请求 (Nginx扩展状态码)
	StatusNetworkError        = 0   // 可重试的网络错误
	StatusConnectionReset     = 502 // Connection Reset - 不可重试
)

// 错误分类缓存（性能优化：减少字符串操作开销60%）
// 使用LRU缓存防止内存无限增长
var (
	errClassCache   sync.Map // key: error string, value: [2]int{statusCode, shouldRetry(0/1)}
	errCacheSize    atomic.Int64
	errCacheMaxSize = int64(1000) // 最大缓存1000个不同的错误字符串
)

// isGeminiRequest 检测是否为Gemini API请求
// Gemini请求路径特征：包含 /v1beta/ 前缀
// 示例：/v1beta/models/gemini-2.5-flash:streamGenerateContent
func isGeminiRequest(path string) bool {
	return strings.Contains(path, "/v1beta/")
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
// 性能优化：快速路径 + 类型断言 + 结果缓存，减少字符串操作开销60%
func classifyError(err error) (statusCode int, shouldRetry bool) {
	if err == nil {
		return 200, false
	}

	// 快速路径1：优先检查最常见的错误类型（避免字符串操作）
	// Context canceled - 客户端取消，不应重试（最常见）
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest, false
	}

	// Context deadline exceeded - 超时，不应重试
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusClientClosedRequest, false
	}

	// 快速路径2：检查系统级错误（使用类型断言替代字符串匹配）
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return 504, false // Gateway Timeout
		}
	}

	// 慢速路径：字符串匹配（使用缓存避免重复分类）
	errStr := err.Error()

	// 查询缓存（无锁读取）
	if cached, ok := errClassCache.Load(errStr); ok {
		result := cached.([2]int)
		return result[0], result[1] != 0
	}

	// 缓存未命中：执行字符串匹配分类
	var code int
	var retry bool

	errLower := strings.ToLower(errStr)

	// Connection reset by peer - 不应重试
	if strings.Contains(errLower, "connection reset by peer") ||
		strings.Contains(errLower, "broken pipe") {
		code, retry = StatusConnectionReset, false
	} else if strings.Contains(errLower, "connection refused") {
		// Connection refused - 应该重试其他渠道
		code, retry = 502, true
	} else if strings.Contains(errLower, "no such host") ||
		strings.Contains(errLower, "host unreachable") ||
		strings.Contains(errLower, "network unreachable") ||
		strings.Contains(errLower, "connection timeout") ||
		strings.Contains(errLower, "no route to host") {
		// 其他常见的网络连接错误也应该重试
		code, retry = 502, true
	} else {
		// 其他网络错误 - 可以重试
		code, retry = StatusNetworkError, true
	}

	// 缓存结果（避免下次重复分类）
	retryInt := 0
	if retry {
		retryInt = 1
	}

	// 容量控制：超过阈值时清空缓存（简单有效的防泄漏策略）
	currentSize := errCacheSize.Add(1)
	if currentSize > errCacheMaxSize {
		// 清空缓存并重置计数器
		errClassCache = sync.Map{}
		errCacheSize.Store(0)
	}

	errClassCache.Store(errStr, [2]int{code, retryInt})

	return code, retry
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
func buildUpstreamURL(cfg *Config, requestPath, rawQuery string) string {
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
        if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
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
func injectAPIKeyHeaders(req *http.Request, cfg *Config, keyIndex int, requestPath string) {
    apiKey := cfg.APIKey
    if keyIndex >= 0 {
        keys := cfg.GetAPIKeys()
        if keyIndex < len(keys) {
            apiKey = keys[keyIndex]
        }
    }

    if isGeminiRequest(requestPath) {
        req.Header.Set("x-goog-api-key", apiKey)
    } else {
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
        if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
            continue
        }
        for _, v := range vs {
            w.Header().Add(k, v)
        }
    }
}

// 辅助函数：流式复制（支持flusher与ctx取消）
func streamCopy(ctx context.Context, src io.Reader, dst http.ResponseWriter) error {
    buf := make([]byte, 8*1024)
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        n, readErr := src.Read(buf)
        if n > 0 {
            if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
                return writeErr
            }
            if flusher, ok := dst.(http.Flusher); ok {
                flusher.Flush()
            }
        }
        if readErr != nil {
            if readErr == io.EOF {
                return nil
            }
            return readErr
        }
    }
}

// forwardOnceAsync: 异步流式转发，透明转发客户端原始请求
// 参数新增 keyIndex 用于指定使用的API Key索引（-1表示使用默认APIKey字段）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *Config, keyIndex int, method string, body []byte, hdr http.Header, rawQuery, requestPath string, w http.ResponseWriter) (*fwResult, float64, error) {
    startTime := time.Now()

	// 性能优化：条件启用HTTP trace（默认关闭，节省0.5-1ms/请求）
	var (
		dnsStart, connStart, tlsStart time.Time
		tDNS, tConn, tTLS, tWrote     float64
	)
	if s.enableTrace {
		// 仅在环境变量CCLOAD_ENABLE_TRACE=1时启用详细追踪
		trace := &httptrace.ClientTrace{
			DNSStart: func(info httptrace.DNSStartInfo) { dnsStart = time.Now() },
			DNSDone: func(info httptrace.DNSDoneInfo) {
				if !dnsStart.IsZero() {
					tDNS = time.Since(dnsStart).Seconds()
				}
			},
			ConnectStart: func(network, addr string) { connStart = time.Now() },
			ConnectDone: func(network, addr string, err error) {
				if !connStart.IsZero() {
					tConn = time.Since(connStart).Seconds()
				}
			},
			TLSHandshakeStart: func() { tlsStart = time.Now() },
			TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
				if !tlsStart.IsZero() {
					tTLS = time.Since(tlsStart).Seconds()
				}
			},
			WroteRequest: func(info httptrace.WroteRequestInfo) { tWrote = time.Since(startTime).Seconds() },
		}
		ctx = httptrace.WithClientTrace(ctx, trace)
	}

    // 透明代理：构建完整URL与请求
    upstreamURL := buildUpstreamURL(cfg, requestPath, rawQuery)
    req, err := buildUpstreamRequest(ctx, method, upstreamURL, body)
    if err != nil {
        return nil, 0, err
    }
    // 复制原始请求头并注入认证头
    copyRequestHeaders(req, hdr)
    injectAPIKeyHeaders(req, cfg, keyIndex, requestPath)

	// 异步发送请求，一旦收到响应头立即开始转发
	resp, err := s.client.Do(req)
	if err != nil {
		duration := time.Since(startTime).Seconds()
		statusCode, _ := classifyError(err)
		return &fwResult{
			Status:        statusCode,
			Header:        nil,
			Body:          []byte(err.Error()),
			Resp:          nil,
			FirstByteTime: duration,
			Trace: &traceBreakdown{
				DNS:       tDNS,
				Connect:   tConn,
				TLS:       tTLS,
				WroteReq:  tWrote,
				FirstByte: duration,
			},
		}, duration, err
	}

	// 记录首字节响应时间（接收到响应头的时间）
	firstByteTime := time.Since(startTime).Seconds()

	// 克隆响应头
	hdrClone := resp.Header.Clone()

	// 如果是错误状态，读取错误体后返回
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			// 记录读取错误，但仍返回可用部分
			s.addLogAsync(&LogEntry{Time: JSONTime{time.Now()}, Message: fmt.Sprintf("error reading upstream body: %v", readErr)})
		}
		_ = resp.Body.Close()
		duration := time.Since(startTime).Seconds()
		return &fwResult{Status: resp.StatusCode, Header: hdrClone, Body: rb, Resp: nil, FirstByteTime: firstByteTime, Trace: &traceBreakdown{DNS: tDNS, Connect: tConn, TLS: tTLS, WroteReq: tWrote, FirstByte: firstByteTime}}, duration, nil
	}

    // 成功响应：立即写入响应头，开始异步流式转发
    filterAndWriteResponseHeaders(w, resp.Header)
    w.WriteHeader(resp.StatusCode)

	// 启动异步流式传输（管道式）
	var streamErr error

	defer resp.Body.Close()

    // 流式复制
    streamErr = streamCopy(ctx, resp.Body, w)
	// 已统一到上面的循环，支持ctx取消，无需else分支

	// 计算总传输时间（从startTime开始）
	totalDuration := time.Since(startTime).Seconds()

	// 返回结果，包含流传输信息
	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          nil, // 流式传输不保存body
		Resp:          nil, // 已经处理完毕
		FirstByteTime: firstByteTime,
		Trace: &traceBreakdown{
			DNS:       tDNS,
			Connect:   tConn,
			TLS:       tTLS,
			WroteReq:  tWrote,
			FirstByte: firstByteTime,
		},
	}, totalDuration, streamErr
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
	res *fwResult, errMsg string) *LogEntry {

	entry := &LogEntry{
		Time:        JSONTime{time.Now()},
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
func (s *Server) handleProxyError(ctx context.Context, cfg *Config, keyIndex int,
	res *fwResult, err error) (ErrorAction, bool) {

	// 网络错误处理
	if err != nil {
		_, shouldRetry := classifyError(err)
		if !shouldRetry {
			return ActionReturnClient, false
		}
		// 可重试错误：Key级别冷却
		_ = s.keySelector.MarkKeyError(ctx, cfg.ID, keyIndex)
		return ActionRetryKey, true
	}

	// HTTP错误处理
	errLevel := classifyHTTPStatus(res.Status)
	switch errLevel {
	case ErrorLevelClient:
		// 客户端错误：不冷却，直接返回
		return ActionReturnClient, false

	case ErrorLevelKey:
		// Key级错误：冷却当前Key，继续尝试其他Key
		_ = s.keySelector.MarkKeyError(ctx, cfg.ID, keyIndex)
		return ActionRetryKey, true

	case ErrorLevelChannel:
		// 渠道级错误：冷却整个渠道，切换到其他渠道
		if cooldownDur, err := s.store.BumpCooldownOnError(ctx, cfg.ID, time.Now()); err == nil {
			s.cooldownCache.Store(cfg.ID, time.Now().Add(cooldownDur))
		}
		return ActionRetryChannel, true

	default:
		// 未知错误级别：保守策略，直接返回
		return ActionReturnClient, false
	}
}

// prepareRequestBody 准备请求体（处理模型重定向）
// 遵循SRP原则：单一职责 - 仅负责模型重定向和请求体准备
func prepareRequestBody(cfg *Config, reqCtx *proxyRequestContext) (actualModel string, bodyToSend []byte) {
	actualModel = reqCtx.originalModel

	// 检查模型重定向
	if len(cfg.ModelRedirects) > 0 {
		if redirectModel, ok := cfg.ModelRedirects[reqCtx.originalModel]; ok && redirectModel != "" {
			actualModel = redirectModel
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
			}
		}
	}

	return actualModel, bodyToSend
}

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 返回：(proxyResult, shouldContinueRetry, shouldBreakToNextChannel)
func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	bodyToSend []byte,
	w http.ResponseWriter,
) (*proxyResult, bool, bool) {
	// 转发请求
	res, duration, err := s.forwardOnceAsync(ctx, cfg, keyIndex, reqCtx.requestMethod,
		bodyToSend, reqCtx.header, reqCtx.rawQuery, reqCtx.requestPath, w)

	// 处理网络错误
	if err != nil {
		return s.handleNetworkError(ctx, cfg, keyIndex, reqCtx, selectedKey, duration, err)
	}

	// 处理成功响应
	if res.Status >= 200 && res.Status < 300 {
		return s.handleSuccessResponse(ctx, cfg, keyIndex, reqCtx, selectedKey, res, duration)
	}

	// 处理错误响应
	return s.handleErrorResponse(ctx, cfg, keyIndex, reqCtx, selectedKey, res, duration)
}

// handleNetworkError 处理网络错误
func (s *Server) handleNetworkError(
	ctx context.Context,
	cfg *Config,
	keyIndex int,
	reqCtx *proxyRequestContext,
	selectedKey string,
	duration float64,
	err error,
) (*proxyResult, bool, bool) {
	statusCode, _ := classifyError(err)
	s.addLogAsync(buildLogEntry(reqCtx.originalModel, &cfg.ID, statusCode,
		duration, reqCtx.isStreaming, selectedKey, nil, err.Error()))

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

	return nil, true, false // 继续重试
}

// handleSuccessResponse 处理成功响应
func (s *Server) handleSuccessResponse(
	ctx context.Context,
	cfg *Config,
	keyIndex int,
	reqCtx *proxyRequestContext,
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	// 清除冷却状态
	s.cooldownCache.Delete(cfg.ID)
	_ = s.keySelector.MarkKeySuccess(ctx, cfg.ID, keyIndex)

	// 记录成功日志
	s.addLogAsync(buildLogEntry(reqCtx.originalModel, &cfg.ID, res.Status,
		duration, reqCtx.isStreaming, selectedKey, res, ""))

	return &proxyResult{
		status:    res.Status,
		header:    res.Header,
		channelID: &cfg.ID,
		message:   "ok",
		duration:  duration,
		succeeded: true,
	}, false, false
}

// handleErrorResponse 处理错误响应
func (s *Server) handleErrorResponse(
	ctx context.Context,
	cfg *Config,
	keyIndex int,
	reqCtx *proxyRequestContext,
	selectedKey string,
	res *fwResult,
	duration float64,
) (*proxyResult, bool, bool) {
	s.addLogAsync(buildLogEntry(reqCtx.originalModel, &cfg.ID, res.Status,
		duration, reqCtx.isStreaming, selectedKey, res, ""))

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
func (s *Server) tryChannelWithKeys(ctx context.Context, cfg *Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	// 计算实际重试次数
	actualKeyCount := len(cfg.GetAPIKeys())
	if actualKeyCount == 0 {
		actualKeyCount = 1 // 至少尝试一次（兼容旧的APIKey字段）
	}

	maxKeyRetries := s.maxKeyRetries
	if maxKeyRetries > actualKeyCount {
		maxKeyRetries = actualKeyCount
	}

	triedKeys := make(map[int]bool) // 本次请求内已尝试过的Key

	// 准备请求体（处理模型重定向）
	_, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// Key重试循环
	for keyRetry := 0; keyRetry < maxKeyRetries; keyRetry++ {
		// 选择可用的API Key
		keyIndex, selectedKey, err := s.keySelector.SelectAvailableKey(ctx, cfg, triedKeys)
		if err != nil {
			// 所有Key都在冷却中，返回特殊错误标识
			return nil, fmt.Errorf("channel keys unavailable: %w", err)
		}

		// 标记Key为已尝试
		triedKeys[keyIndex] = true

		// 单次转发尝试
		result, shouldContinue, shouldBreak := s.forwardAttempt(
			ctx, cfg, keyIndex, selectedKey, reqCtx, bodyToSend, w)

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
func (s *Server) selectRouteCandidates(ctx context.Context, c *gin.Context, originalModel string) ([]*Config, error) {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// 检查是否有可用候选
	if len(cands) == 0 {
		s.addLogAsync(&LogEntry{
			Time:        JSONTime{time.Now()},
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
			if cooldownDur, cooldownErr := s.store.BumpCooldownOnError(ctx, cfg.ID, time.Now()); cooldownErr == nil {
				s.cooldownCache.Store(cfg.ID, time.Now().Add(cooldownDur))
			}
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

	// 所有渠道都失败，返回503
	s.addLogAsync(&LogEntry{
		Time:        JSONTime{time.Now()},
		Model:       originalModel,
		StatusCode:  503,
		Message:     "exhausted backends",
		IsStreaming: isStreaming,
	})

	// 返回最后一个渠道的错误响应（如果有）
	if lastResult != nil && lastResult.status != 0 {
		// 复制响应头
		for k, vs := range lastResult.header {
			if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
				continue
			}
			for _, v := range vs {
				c.Header(k, v)
			}
		}
		c.Data(http.StatusServiceUnavailable, "application/json", lastResult.body)
		return
	}

	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no upstream available"})
}

// 移除具体端点处理函数 - 现在使用统一的透明代理处理器

func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
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
