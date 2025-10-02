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
var (
	errClassCache sync.Map // key: error string, value: [2]int{statusCode, shouldRetry(0/1)}
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

	// 透明代理：完全保持客户端原始请求路径和参数
	upstreamURL := strings.TrimRight(cfg.URL, "/") + requestPath
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	u, err := neturl.Parse(upstreamURL)
	if err != nil {
		return nil, 0, err
	}
	// 使用客户端原始HTTP方法，支持GET（无body）和POST（有body）等所有方法
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, 0, err
	}
	// Copy headers but override API key
	for k, vs := range hdr {
		// Skip hop-by-hop and auth
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	// 确定使用的API Key：优先使用指定的keyIndex，否则使用默认APIKey
	apiKey := cfg.APIKey
	if keyIndex >= 0 {
		keys := cfg.GetAPIKeys()
		if keyIndex < len(keys) {
			apiKey = keys[keyIndex]
		}
	}

	// API特定头设置：根据请求路径区分Gemini和Claude API
	if isGeminiRequest(requestPath) {
		// Gemini API：仅使用x-goog-api-key认证
		req.Header.Set("x-goog-api-key", apiKey)
	} else {
		// Claude API：使用x-api-key和Authorization Bearer认证
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

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
	for k, vs := range resp.Header {
		// 跳过hop-by-hop头
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 启动异步流式传输（管道式）
	var streamErr error

	defer resp.Body.Close()

	// 使用小缓冲区实现低延迟传输，支持ctx取消
	buf := make([]byte, 8*1024) // 8KB缓冲区，平衡延迟与系统调用开销
streamLoop:
	for {
		// 尝试上下文取消
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			break streamLoop
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// 立即写入
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				streamErr = writeErr
				break streamLoop
			}
			// 如果支持flusher，刷新减少延迟
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				streamErr = readErr
			}
			break streamLoop
		}
	}
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

// 通用透明代理处理器
func (s *Server) handleProxyRequest(c *gin.Context) {
	// 性能优化：并发控制 - 使用信号量限制最大并发请求数
	// 获取信号量（阻塞直到有槽位可用）
	select {
	case s.concurrencySem <- struct{}{}:
		// 成功获取槽位，确保函数结束时释放
		defer func() { <-s.concurrencySem }()
	case <-c.Request.Context().Done():
		// 客户端已取消请求，直接返回
		c.JSON(StatusClientClosedRequest, gin.H{"error": "request cancelled while waiting for slot"})
		return
	}

	// 获取客户端原始请求路径和方法（透明转发的关键）
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 特殊处理：拦截 GET /v1beta/models 请求，返回本地模型列表
	if requestMethod == http.MethodGet && (requestPath == "/v1beta/models" || requestPath == "/v1/models") {
		s.handleListGeminiModels(c)
		return
	}

	// 全量读取再转发，KISS（GET请求body通常为空）
	all, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	_ = c.Request.Body.Close()
	var reqModel struct {
		Model string `json:"model"`
	}
	_ = sonic.Unmarshal(all, &reqModel)

	// 智能检测流式请求（支持Gemini路径特征和Claude/OpenAI请求体标识）
	isStreaming := isStreamingRequest(requestPath, all)

	// 多源模型名称获取：优先请求体，其次URL路径
	originalModel := reqModel.Model
	if originalModel == "" {
		// 尝试从URL路径提取模型名称（支持Gemini API格式）
		originalModel = extractModelFromPath(requestPath)
	}

	// 对于GET请求，如果无法提取模型名称，使用默认值"*"表示通配
	// 这允许列出所有模型等操作（如 GET /v1beta/models）
	if originalModel == "" {
		if requestMethod == http.MethodGet {
			originalModel = "*" // 通配符，匹配所有渠道
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON or missing model"})
			return
		}
	}

	// 保存原始请求模型（用于日志记录和渠道选择）

	// 解析超时
	timeout := parseTimeout(c.Request.URL.Query(), c.Request.Header)

	ctx := c.Request.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// 智能路由选择：根据请求类型选择不同的路由策略
	var cands []*Config
	// 特殊处理：GET /v1beta/models 等Gemini API元数据请求，使用渠道类型路由
	if requestMethod == http.MethodGet && isGeminiRequest(requestPath) {
		// 按渠道类型筛选Gemini渠道（不依赖模型匹配）
		cands, err = s.selectCandidatesByChannelType(ctx, "gemini")
	} else {
		// 正常流程：按模型匹配渠道
		cands, err = s.selectCandidates(ctx, originalModel)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	// If no candidates available (all cooled or none support), return 503
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

	// 异步代理：按候选顺序尝试，使用管道式流转发减少延迟
	var lastStatus int
	var lastBody []byte
	var lastHeader http.Header
	for _, cfg := range cands {
		// 渠道内Key级别重试循环：限制重试次数避免过多延迟
		actualKeyCount := len(cfg.GetAPIKeys())
		if actualKeyCount == 0 {
			actualKeyCount = 1 // 至少尝试一次（兼容旧的APIKey字段）
		}

		// 使用配置的最大重试次数，但不超过实际Key数量
		maxKeyRetries := s.maxKeyRetries
		if maxKeyRetries > actualKeyCount {
			maxKeyRetries = actualKeyCount
		}

		var channelExhausted bool
		for keyRetry := 0; keyRetry < maxKeyRetries; keyRetry++ {
			// 多Key支持：选择可用的API Key
			keyIndex, selectedKey, err := s.keySelector.SelectAvailableKey(ctx, cfg)
			if err != nil {
				// 所有Key都在冷却中，触发渠道级别冷却并跳到下一个渠道
				// 注意：不记录日志，因为这是内部状态，不是上游API的真实错误
				// 只有真正调用上游API并得到响应时才记录日志
				if keyRetry == 0 {
					// 仅在第一次尝试时触发冷却，避免重复操作
					// 触发渠道级别冷却，防止后续请求重复尝试该渠道
					if cooldownDur, cooldownErr := s.store.BumpCooldownOnError(ctx, cfg.ID, time.Now()); cooldownErr == nil {
						// 同步更新内存缓存，确保渠道选择器能正确过滤冷却中的渠道
						s.cooldownCache.Store(cfg.ID, time.Now().Add(cooldownDur))
					}
				}
				channelExhausted = true
				break // 跳出Key重试循环，尝试下一个渠道
			}

			// 模型重定向：检查渠道是否配置了模型重定向映射
			actualModel := originalModel
			if len(cfg.ModelRedirects) > 0 {
				if redirectModel, ok := cfg.ModelRedirects[originalModel]; ok && redirectModel != "" {
					actualModel = redirectModel
				}
			}

			// 如果模型发生重定向，修改请求体中的模型字段
			bodyToSend := all
			if actualModel != originalModel {
				var reqData map[string]any
				if err := sonic.Unmarshal(all, &reqData); err == nil {
					reqData["model"] = actualModel
					if modifiedBody, err := sonic.Marshal(reqData); err == nil {
						bodyToSend = modifiedBody
					}
				}
			}

			// 透明转发：使用选择的Key和HTTP方法进行转发
			res, duration, err := s.forwardOnceAsync(ctx, cfg, keyIndex, requestMethod, bodyToSend, c.Request.Header, c.Request.URL.RawQuery, requestPath, c.Writer)
			if err != nil {
				// 分类错误类型
				statusCode, shouldRetry := classifyError(err)

				// 记录日志（使用原始模型，记录使用的Key）
				s.addLogAsync(&LogEntry{
					Time:        JSONTime{time.Now()},
					Model:       originalModel,
					ChannelID:   &cfg.ID,
					StatusCode:  statusCode,
					Message:     truncateErr(err.Error()),
					Duration:    duration,
					IsStreaming: isStreaming,
					APIKeyUsed:  selectedKey,
				})

				// 如果是不可重试的错误，直接返回
				if !shouldRetry {
					// 根据错误类型返回适当的响应
					switch statusCode {
					case StatusConnectionReset:
						c.JSON(502, gin.H{"error": "upstream connection reset"})
					case StatusClientClosedRequest:
						c.JSON(499, gin.H{"error": "request cancelled"})
					case 504:
						c.JSON(504, gin.H{"error": "gateway timeout"})
					default:
						c.JSON(statusCode, gin.H{"error": truncateErr(err.Error())})
					}
					return
				}

				// 可重试错误：Key级别冷却，继续尝试当前渠道的其他Key
				_ = s.keySelector.MarkKeyError(ctx, cfg.ID, keyIndex)

				lastStatus = statusCode
				lastBody = []byte(err.Error())
				lastHeader = nil
				continue // 继续Key重试循环
			}
			if res.Status >= 200 && res.Status < 300 {
				// 成功时清除渠道级和Key级冷却状态
				// 清除渠道级冷却缓存（避免内存泄漏，确保缓存一致性）
				s.cooldownCache.Delete(cfg.ID)
				// 重置Key级别冷却（保留原有逻辑）
				_ = s.keySelector.MarkKeySuccess(ctx, cfg.ID, keyIndex)

				// 记录成功日志（使用原始模型，记录使用的Key）
				logEntry := &LogEntry{
					Time:        JSONTime{time.Now()},
					Model:       originalModel,
					ChannelID:   &cfg.ID,
					StatusCode:  res.Status,
					Duration:    duration,
					Message:     "ok",
					IsStreaming: isStreaming,
					APIKeyUsed:  selectedKey,
				}

				// 流式请求记录首字节响应时间
				if isStreaming {
					logEntry.FirstByteTime = &res.FirstByteTime
				}

				s.addLogAsync(logEntry)
				return // 成功完成，直接返回
			}

			// 非2xx响应：检查是否为特殊状态码（如499）
			if res.Status == StatusClientClosedRequest {
				// 客户端取消请求，直接返回，不尝试其他Key和渠道
				msg := fmt.Sprintf("upstream status %d", res.Status)
				if len(res.Body) > 0 {
					msg = fmt.Sprintf("%s: %s", msg, truncateErr(safeBodyToString(res.Body)))
				}

				// 记录日志（使用原始模型，记录使用的Key）
				logEntry := &LogEntry{
					Time:        JSONTime{time.Now()},
					Model:       originalModel,
					ChannelID:   &cfg.ID,
					StatusCode:  res.Status,
					Message:     msg,
					Duration:    duration,
					IsStreaming: isStreaming,
					APIKeyUsed:  selectedKey,
				}
				if isStreaming {
					logEntry.FirstByteTime = &res.FirstByteTime
				}
				s.addLogAsync(logEntry)

				// 直接返回，不切换Key和渠道
				c.JSON(res.Status, gin.H{"error": msg})
				return
			}

			// 其他非2xx响应：记录日志并触发渠道级冷却
			// 设计原则（SRP）：401/429/500等错误本质上是渠道级问题（配置错误/欠费/限流）
			// 而非单个Key问题，因此应该直接冷却整个渠道，切换到其他渠道重试
			msg := fmt.Sprintf("upstream status %d", res.Status)
			if len(res.Body) > 0 {
				msg = fmt.Sprintf("%s: %s", msg, truncateErr(safeBodyToString(res.Body)))
			}

			// 记录错误日志（使用原始模型，记录使用的Key）
			logEntry := &LogEntry{
				Time:        JSONTime{time.Now()},
				Model:       originalModel,
				ChannelID:   &cfg.ID,
				StatusCode:  res.Status,
				Message:     msg,
				Duration:    duration,
				IsStreaming: isStreaming,
				APIKeyUsed:  selectedKey,
			}
			if isStreaming {
				logEntry.FirstByteTime = &res.FirstByteTime
			}
			s.addLogAsync(logEntry)

			// 触发渠道级冷却（指数退避：1s -> 2s -> 4s -> ... -> 30分钟）
			if cooldownDur, err := s.store.BumpCooldownOnError(ctx, cfg.ID, time.Now()); err == nil {
				// 同步更新内存缓存，确保渠道选择器能正确过滤冷却中的渠道
				s.cooldownCache.Store(cfg.ID, time.Now().Add(cooldownDur))
			}

			// 保存最后的响应信息（用于最终的503响应）
			lastStatus = res.Status
			lastBody = res.Body
			lastHeader = res.Header

			// 立即跳过当前渠道，尝试下一个候选渠道
			// 不再进行Key级别重试，因为这类错误通常是渠道整体问题
			break // 跳出Key重试循环
		}

		// Key重试循环结束，检查是否因为"所有Key都在冷却中"而退出
		if channelExhausted {
			// 渠道冷却已在第457行触发，直接跳到下一个渠道
			continue
		}
	}

	// All failed
	s.addLogAsync(&LogEntry{
		Time:        JSONTime{time.Now()},
		Model:       originalModel,
		StatusCode:  503,
		Message:     "exhausted backends",
		IsStreaming: isStreaming,
	})
	if lastStatus != 0 {
		// surface last upstream response info
		for k, vs := range lastHeader {
			if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
				continue
			}
			for _, v := range vs {
				c.Header(k, v)
			}
		}
		c.Data(http.StatusServiceUnavailable, "application/json", lastBody)
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
