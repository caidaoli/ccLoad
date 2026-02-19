package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

const (
	// SSEProbeSize 用于探测 text/plain 内容是否包含 SSE 事件的前缀长度（2KB 足够覆盖小事件）
	SSEProbeSize = 2 * 1024
)

// prependedBody 将已读取的前缀数据与原始Body合并，保留原Closer
type prependedBody struct {
	io.Reader
	io.Closer
}

// onceCloseReadCloser 确保 Close 只执行一次（用于协调 defer 与 context.AfterFunc 的并发关闭）
type onceCloseReadCloser struct {
	io.ReadCloser
	once sync.Once
}

func (rc *onceCloseReadCloser) Close() error {
	var closeErr error
	rc.once.Do(func() {
		closeErr = rc.ReadCloser.Close()
	})
	return closeErr
}

// prependToBody 将前缀数据合并到resp.Body（用于恢复已探测的数据）
func prependToBody(resp *http.Response, prefix []byte) {
	resp.Body = prependedBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body),
		Closer: resp.Body,
	}
}

// ============================================================================
// 请求构建和转发
// ============================================================================

// buildProxyRequest 构建上游代理请求（统一处理URL、Header、认证）
// 从proxy.go提取，遵循SRP原则
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

// ============================================================================
// 响应处理
// ============================================================================

// handleRequestError 处理网络请求错误
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleRequestError(
	reqCtx *requestContext,
	cfg *model.Config,
	err error,
) (*fwResult, float64, error) {
	reqCtx.stopFirstByteTimer()
	duration := reqCtx.Duration()
	durationSec := duration.Seconds()

	// 检测超时错误：使用统一的内部状态码+冷却策略
	var statusCode int
	if reqCtx.firstByteTimeoutTriggered() {
		// 流式请求首字节超时（定时器触发）
		statusCode = util.StatusFirstByteTimeout
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", durationSec)
		timeout := s.firstByteTimeout
		if timeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, timeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		log.Printf("[TIMEOUT] [上游首字节超时] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, timeout, durationSec)
	} else if errors.Is(err, context.DeadlineExceeded) {
		if reqCtx.isStreaming {
			// 流式请求超时
			err = fmt.Errorf("upstream timeout after %.2fs (streaming): %w", durationSec, err)
			statusCode = util.StatusFirstByteTimeout
			log.Printf("[TIMEOUT] [流式请求超时] 渠道ID=%d, 耗时=%.2fs", cfg.ID, durationSec)
		} else {
			// 非流式请求超时（context.WithTimeout触发）
			err = fmt.Errorf("upstream timeout after %.2fs (non-stream, threshold=%v): %w",
				durationSec, s.nonStreamTimeout, err)
			statusCode = 504 // Gateway Timeout
			log.Printf("[TIMEOUT] [非流式请求超时] 渠道ID=%d, 阈值=%v, 耗时=%.2fs", cfg.ID, s.nonStreamTimeout, durationSec)
		}
	} else {
		// 其他错误：使用统一分类器
		statusCode, _, _ = util.ClassifyError(err)
	}

	return &fwResult{
		Status:        statusCode,
		Body:          []byte(err.Error()),
		FirstByteTime: 0,
	}, durationSec, err
}

// handleErrorResponse 处理错误响应（读取完整响应体）
// 从proxy.go提取，遵循SRP原则
// 限制错误体大小防止 OOM（与入站 DefaultMaxBodyBytes 限制对称）
func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	firstBodyReadTimeSec *float64,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(config.DefaultMaxBodyBytes)))
	diagMsg := ""
	if readErr != nil {
		// 不要创建“孤儿日志”（StatusCode=0），而是把诊断信息合并到本次请求的日志中（KISS）。
		diagMsg = fmt.Sprintf("error reading upstream body: %v", readErr)
	}

	duration := reqCtx.Duration().Seconds()

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          rb,
		FirstByteTime: *firstBodyReadTimeSec,
		StreamDiagMsg: diagMsg,
	}, duration, nil
}

// streamAndParseResponse 根据Content-Type选择合适的流式传输策略并解析usage
// 返回: (usageParser, streamErr)
func streamAndParseResponse(ctx context.Context, body io.ReadCloser, w http.ResponseWriter, contentType string, channelType string, isStreaming bool) (usageParser, error) {
	// SSE流式响应
	if strings.Contains(contentType, "text/event-stream") {
		parser := newSSEUsageParser(channelType)
		streamErr := streamCopySSE(ctx, body, w, parser.Feed)
		return parser, streamErr
	}

	// 非标准SSE场景：上游以text/plain发送SSE事件
	if strings.Contains(contentType, "text/plain") && isStreaming {
		reader := bufio.NewReader(body)
		probe, _ := reader.Peek(SSEProbeSize)

		if looksLikeSSE(probe) {
			parser := newSSEUsageParser(channelType)
			sseErr := streamCopySSE(ctx, io.NopCloser(reader), w, parser.Feed)
			return parser, sseErr
		}
		parser := newJSONUsageParser(channelType)
		copyErr := streamCopy(ctx, io.NopCloser(reader), w, parser.Feed)
		return parser, copyErr
	}

	// 非SSE响应：边转发边缓存
	parser := newJSONUsageParser(channelType)
	copyErr := streamCopy(ctx, body, w, parser.Feed)
	return parser, copyErr
}

// isClientDisconnectError 判断是否为客户端主动断开导致的错误
// 只识别明确的客户端取消信号，不包括上游服务器错误
// 注意：http2: response body closed 和 stream error 是上游服务器问题，不是客户端断开！
func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	// context.Canceled 是明确的客户端取消信号（用户点"停止"）
	if errors.Is(err, context.Canceled) {
		return true
	}
	// "client disconnected" 是 gin/net/http 报告的客户端断开
	// 注意：http2: response body closed 和 stream error 是上游服务器问题，
	// 不应在此判断，否则会导致上游异常被忽略而不触发冷却逻辑
	errStr := err.Error()
	return strings.Contains(errStr, "client disconnected")
}

// buildStreamDiagnostics 生成流诊断消息
// 触发条件：流传输错误且未检测到流结束标志（[DONE]/message_stop）
// streamComplete: 是否检测到流结束标志（比 hasUsage 更可靠，因为不是所有请求都有 usage）
func buildStreamDiagnostics(streamErr error, readStats *streamReadStats, streamComplete bool, channelType string, contentType string) string {
	if readStats == nil {
		return ""
	}

	bytesRead := readStats.totalBytes
	readCount := readStats.readCount

	// 流传输异常中断(排除客户端主动断开)
	// 关键：如果检测到流结束标志（[DONE]/message_stop），说明流已完整传输
	if streamErr != nil && !isClientDisconnectError(streamErr) {
		// 已检测到流结束标志 = 流完整，http2关闭只是正常结束信号
		if streamComplete {
			return "" // 不触发冷却，数据已完整
		}
		return fmt.Sprintf("[WARN] 流传输中断: 错误=%v | 已读取=%d字节(分%d次) | 流结束标志=%v | 渠道=%s | Content-Type=%s",
			streamErr, bytesRead, readCount, streamComplete, channelType, contentType)
	}

	return ""
}

// handleSuccessResponse 处理成功响应（流式传输）
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
	firstBodyReadTimeSec *float64,
) (*fwResult, float64, error) {
	// [FIX] 流式请求：禁用 WriteTimeout，避免长时间流被服务器自己切断
	// Go 1.20+ http.ResponseController 支持动态调整 WriteDeadline
	if reqCtx.isStreaming {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			log.Printf("[WARN] 无法禁用流式请求的 WriteTimeout: %v", err)
		}
	}

	// 写入响应头
	filterAndWriteResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 流式传输并解析usage
	contentType := resp.Header.Get("Content-Type")
	parser, streamErr := streamAndParseResponse(
		reqCtx.ctx, resp.Body, w, contentType, channelType, reqCtx.isStreaming,
	)

	// 构建结果
	result := &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		FirstByteTime: *firstBodyReadTimeSec,
		BytesReceived: readStats.totalBytes, // 记录已接收字节数，用于499诊断
	}

	// 提取usage数据和错误事件
	var streamComplete bool
	if parser != nil {
		result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()

		// 提取5m和1h缓存细分字段（通过类型断言访问底层实现）
		// 设计原则：不修改接口避免破坏现有测试，通过类型断言优雅扩展
		switch p := parser.(type) {
		case *sseUsageParser:
			result.Cache5mInputTokens = p.Cache5mInputTokens
			result.Cache1hInputTokens = p.Cache1hInputTokens
		case *jsonUsageParser:
			result.Cache5mInputTokens = p.Cache5mInputTokens
			result.Cache1hInputTokens = p.Cache1hInputTokens
		}

		if errorEvent := parser.GetLastError(); errorEvent != nil {
			result.SSEErrorEvent = errorEvent
		}
		streamComplete = parser.IsStreamComplete()
	}

	// 生成流诊断消息（仅流请求）
	if reqCtx.isStreaming {
		// [VALIDATE] 诊断增强: 传递contentType帮助定位问题(区分SSE/JSON/其他)
		// 使用 streamComplete 而非 hasUsage，因为不是所有请求都有 usage 信息
		if diagMsg := buildStreamDiagnostics(streamErr, readStats, streamComplete, channelType, contentType); diagMsg != "" {
			result.StreamDiagMsg = diagMsg
			log.Print(diagMsg)
		} else if streamComplete && streamErr != nil {
			// [FIX] 流式请求：检测到流结束标志（[DONE]/message_stop）说明数据完整
			// 所有收尾阶段的错误都应忽略，包括：
			// - http2 流关闭（正常结束信号）
			// - context.Canceled（客户端在传输完成后取消，不应标记为499）
			streamErr = nil
		}
	} else {
		// [FIX] 非流式请求：如果有数据被传输，且错误是 HTTP/2 流关闭相关的，视为成功
		// 原因：streamCopy 已将数据写入 ResponseWriter，客户端已收到完整响应
		// http2 流关闭只是 "确认结束" 阶段的错误，不影响已传输的数据
		if readStats.totalBytes > 0 && streamErr != nil && isHTTP2StreamCloseError(streamErr) {
			streamErr = nil
		}
	}

	return result, reqCtx.Duration().Seconds(), streamErr
}

// isHTTP2StreamCloseError 判断是否是 HTTP/2 流关闭相关的错误
// 这类错误发生在数据传输完成后，不影响已传输的数据完整性
func isHTTP2StreamCloseError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "http2: response body closed") ||
		strings.Contains(errStr, "stream error:")
}

// looksLikeSSE 粗略判断文本内容是否包含 SSE 事件结构
func looksLikeSSE(data []byte) bool {
	// 同时包含 event: 与 data: 行的简单特征，可匹配大多数 SSE 片段
	return bytes.Contains(data, []byte("event:")) && bytes.Contains(data, []byte("data:"))
}

// handleResponse 处理 HTTP 响应（错误或成功）
// 从proxy.go提取，遵循SRP原则
// channelType: 渠道类型,用于精确识别usage格式
// cfg: 渠道配置,用于提取渠道ID
// apiKey: 使用的API Key,用于日志记录
func (s *Server) handleResponse(
	reqCtx *requestContext,
	resp *http.Response,
	w http.ResponseWriter,
	channelType string,
	cfg *model.Config,
	apiKey string,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()

	// 首字节响应时间（秒）：以第一次从 resp.Body 读到 n>0 的时刻为准。
	// 流式请求：该时刻同时用于停止 firstByteTimeout。
	firstBodyReadTimeSec := 0.0
	readStats := &streamReadStats{}
	resp.Body = &firstByteDetector{
		ReadCloser: resp.Body,
		stats:      readStats,
		onFirstRead: func() {
			if reqCtx.isStreaming {
				reqCtx.stopFirstByteTimer()
			}
			if firstBodyReadTimeSec == 0 {
				firstBodyReadTimeSec = reqCtx.Duration().Seconds()
			}
			if reqCtx.isStreaming && observer != nil && observer.OnFirstByteRead != nil {
				observer.OnFirstByteRead()
			}
		},
		onBytesRead: func(n int64) {
			if observer != nil && observer.OnBytesRead != nil {
				observer.OnBytesRead(n)
			}
		},
	}

	// [INFO] 软错误检测：200状态码但响应体包含明确错误信息（如"当前模型负载过高"）
	// 检测条件：Content-Type为text/plain或application/json
	// 针对渠道17等上游返回200但实际内容为错误信息的情况
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == 200 &&
		!reqCtx.isStreaming &&
		shouldCheckSoftErrorForChannelType(channelType) &&
		(strings.Contains(ct, "text/plain") || strings.Contains(ct, "application/json")) {
		// 预读 512 字节进行检测
		peekSize := 512
		buf := make([]byte, peekSize)
		// 使用 Read 读取一次（非阻塞等待填满），避免流式响应强制等待 512 字节导致首字延迟
		// 之前的 io.ReadFull 会导致 stream 必须积累 2-3 秒数据才返回，这是不可接受的
		n, err := resp.Body.Read(buf)
		if err != nil && err != io.EOF {
			// 读取错误，记录日志但不中断流程
			log.Printf("[WARN] 软错误检测读取失败: %v", err)
		}

		validData := buf[:n]
		if n > 0 && checkSoftError(validData, ct) {
			// 检测到软错误！
			log.Printf("[WARN] [软错误检测] 渠道ID=%d, 响应200但疑似错误响应: %s", cfg.ID, truncateErr(safeBodyToString(validData)))

			// [FIX] 使用 StatusSSEError (597) 而非 503，让 ClassifyHTTPResponse 能正确分析 error.type
			// 原因：简单改为503会导致所有软错误都被误判为渠道级故障（如429限流被当作渠道过载）
			// 现在：利用现有的 classifySSEError 逻辑，根据 error.type 精确分类为 Key级/渠道级
			// [FIX] 区分 1308 错误与其他 SSE 错误
			// 1308 错误 (StatusQuotaExceeded) 不计入成功率统计
			if _, is1308 := util.ParseResetTimeFrom1308Error(validData); is1308 {
				resp.StatusCode = util.StatusQuotaExceeded // 596
			} else {
				// 其他软错误使用 597
				resp.StatusCode = util.StatusSSEError // 597
			}

			// 恢复 Body 以便 handleErrorResponse 读取完整信息
			prependToBody(resp, validData)

			// 转交给错误处理流程
			return s.handleErrorResponse(reqCtx, resp, hdrClone, &firstBodyReadTimeSec)
		}

		// 未检测到错误，必须恢复 Body 供后续流程使用
		if n > 0 {
			prependToBody(resp, validData)
		}
	}

	// 错误状态：读取完整响应体
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, hdrClone, &firstBodyReadTimeSec)
	}

	// [INFO] 空响应检测：200状态码但Content-Length=0视为上游故障
	// 常见于CDN/代理错误、认证失败等异常场景，应触发渠道级重试
	if contentLen := resp.Header.Get("Content-Length"); contentLen == "0" {
		duration := reqCtx.Duration().Seconds()
		err := fmt.Errorf("upstream returned empty response (200 OK with Content-Length: 0)")

		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          []byte(err.Error()),
			FirstByteTime: firstBodyReadTimeSec,
		}, duration, err
	}

	// 成功状态：流式转发（传递渠道信息用于日志记录，传递观测回调）
	return s.handleSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats, &firstBodyReadTimeSec)
}

// ============================================================================
// 核心转发函数
// ============================================================================

// forwardOnceAsync 异步流式转发，透明转发客户端原始请求
// 从proxy.go提取，遵循SRP原则
// 参数新增 apiKey 用于直接传递已选中的API Key（从KeySelector获取）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *model.Config, apiKey string, method string, body []byte, hdr http.Header, rawQuery, requestPath string, w http.ResponseWriter, observer *ForwardObserver) (*fwResult, float64, error) {
	// 1. 创建请求上下文（处理超时）
	reqCtx := s.newRequestContext(ctx, requestPath, body)
	defer reqCtx.cleanup() // [INFO] 统一清理：定时器 + context（总是安全）

	// 2. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, body, hdr, rawQuery, requestPath)
	if err != nil {
		return nil, 0, err
	}

	// 3. 发送请求
	resp, err := s.client.Do(req)

	// [INFO] 修复（2025-12）：客户端取消时主动关闭 response body，立即中断上游传输
	// 问题：streamCopy 中的 Read 阻塞时，无法立即响应 context 取消，上游继续生成完整响应
	// 解决：使用 Go 1.21+ context.AfterFunc 替代手动 goroutine（零泄漏风险）
	//   - HTTP/1.1: 关闭 TCP 连接 → 上游收到 RST，立即停止发送
	//   - HTTP/2: 发送 RST_STREAM 帧 → 取消当前 stream（不影响同连接的其他请求）
	// 效果：避免 AI 流式生成场景下，用户点"停止"后上游仍生成数千 tokens 的浪费
	if resp != nil {
		// 注意：resp.Body 后续会被包装（例如 firstByteDetector）。
		// 因此需要先把 body 封装成“稳定引用”，避免取消 goroutine 与包装赋值发生 data race。
		body := &onceCloseReadCloser{ReadCloser: resp.Body}
		resp.Body = body

		// 正常返回时关闭（Close 幂等，允许与 AfterFunc 并发触发）
		defer func() { _ = resp.Body.Close() }()

		// [INFO] 使用 context.AfterFunc 监听请求取消/超时（Go 1.21+，标准库保证无泄漏）
		// 必须监听 reqCtx.ctx（而非父 ctx），否则 nonStreamTimeout/firstByteTimeout 触发时无法强制打断阻塞 Read。
		stop := context.AfterFunc(reqCtx.ctx, func() { _ = body.Close() })
		defer stop() // 取消注册（请求正常结束时避免内存泄漏）
	}

	if err != nil {
		return s.handleRequestError(reqCtx, cfg, err)
	}

	// 4. 处理响应(传递channelType用于精确识别usage格式,传递渠道信息用于日志记录,传递观测回调)
	var res *fwResult
	var duration float64
	res, duration, err = s.handleResponse(reqCtx, resp, w, cfg.ChannelType, cfg, apiKey, observer)

	// [FIX] 2025-12: 流式传输过程中首字节超时的错误修正
	// 场景：响应头已收到(200 OK)，但在读取响应体时超时定时器触发
	// 此时 streamCopy 返回 context.Canceled，但实际原因是首字节超时
	// 需要将错误包装为 ErrUpstreamFirstByteTimeout，确保正确分类和日志记录
	if err != nil && reqCtx.firstByteTimeoutTriggered() {
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", duration)
		if s.firstByteTimeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, s.firstByteTimeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		res.Status = util.StatusFirstByteTimeout
		log.Printf("[TIMEOUT] [上游首字节超时-流传输中断] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, s.firstByteTimeout, duration)
	}

	return res, duration, err
}

// ============================================================================
// 单次转发尝试
// ============================================================================

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 从proxy.go提取，遵循SRP原则
// 返回：(proxyResult, nextAction)
func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string, // [INFO] 重定向后的实际模型名称
	bodyToSend []byte,
	requestPath string, // [FIX] 2026-01: 可能经过模型名替换的请求路径
	w http.ResponseWriter,
) (*proxyResult, cooldown.Action) {
	// 记录渠道尝试开始时间（用于日志记录，每次渠道/Key切换时更新）
	reqCtx.attemptStartTime = time.Now()

	// 转发请求（传递实际的API Key字符串和观测回调）
	// [FIX] 2026-01: 使用传入的 requestPath（可能已替换模型名）而非 reqCtx.requestPath
	res, duration, err := s.forwardOnceAsync(ctx, cfg, selectedKey, reqCtx.requestMethod,
		bodyToSend, reqCtx.header, reqCtx.rawQuery, requestPath, w, reqCtx.observer)

	// 处理网络错误或异常响应（如空响应）
	// [INFO] 修复：handleResponse可能返回err即使StatusCode=200（例如Content-Length=0）
	// [FIX] 2025-12: 传递 res 和 reqCtx，用于保留 499 场景下已消耗的 token 统计
	if err != nil {
		return s.handleNetworkError(ctx, cfg, keyIndex, actualModel, selectedKey, reqCtx.tokenID, reqCtx.clientIP, duration, err, res, reqCtx)
	}

	// 处理成功响应（仅当err==nil且状态码2xx时）
	if res.Status >= 200 && res.Status < 300 {
		// [INFO] 检查SSE流中是否有error事件（如1308错误）
		// 虽然HTTP状态码是200，但error事件表示实际上发生了错误，需要触发冷却逻辑
		if res.SSEErrorEvent != nil {
			// 将SSE error事件当作HTTP错误处理
			// 使用内部状态码 StatusSSEError 标识，便于日志筛选和统计
			log.Printf("[WARN]  [SSE错误处理] HTTP状态码200但检测到SSE error事件，触发冷却逻辑")
			res.Body = res.SSEErrorEvent
			// [FIX] 区分 1308 错误
			// 如果是 1308 错误，使用 596 状态码，避免影响渠道成功率
			if _, is1308 := util.ParseResetTimeFrom1308Error(res.SSEErrorEvent); is1308 {
				res.Status = util.StatusQuotaExceeded // 596
				res.StreamDiagMsg = fmt.Sprintf("Quota Exceeded (1308): %s", safeBodyToString(res.SSEErrorEvent))
			} else {
				res.Status = util.StatusSSEError // 597 - SSE error事件
				res.StreamDiagMsg = fmt.Sprintf("SSE error event: %s", safeBodyToString(res.SSEErrorEvent))
			}
			// [FIX] 流式响应已开始（响应头已发送），重试不可能
			// 只触发冷却+记录日志，不尝试重试（避免产生 499 混乱日志）
			return s.handleStreamingErrorNoRetry(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
		}

		// [INFO] 检查流响应是否不完整（2025-12新增）
		// 虽然HTTP状态码是200且流传输结束，但检测到流响应不完整或流传输中断，需要触发冷却逻辑
		// 触发条件：(1) 流传输错误  (2) 流式请求但没有usage数据（疑似不完整响应）
		if res.StreamDiagMsg != "" {
			log.Printf("[WARN]  [流响应不完整] HTTP状态码200但检测到流响应不完整，触发冷却逻辑: %s", res.StreamDiagMsg)
			// 使用内部状态码 StatusStreamIncomplete 标识流响应不完整
			// 这将触发渠道级冷却，因为这通常是上游服务问题（网络不稳定、负载过高等）
			res.Body = []byte(res.StreamDiagMsg)
			res.Status = util.StatusStreamIncomplete // 599 - 流响应不完整
			// [FIX] 流式响应已开始（响应头已发送），重试不可能
			return s.handleStreamingErrorNoRetry(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
		}

		return s.handleProxySuccess(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
	}

	// 处理错误响应
	return s.handleProxyErrorResponse(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
}

// ============================================================================
// 渠道内Key重试
// ============================================================================

// tryChannelWithKeys 在单个渠道内尝试多个Key（Key级重试）
// 从proxy.go提取，遵循SRP原则
func (s *Server) tryChannelWithKeys(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	makeCtxDoneResult := func(ctxErr error) *proxyResult {
		status := util.StatusClientClosedRequest
		isClientCanceled := errors.Is(ctxErr, context.Canceled)
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}

		return &proxyResult{
			status:           status,
			body:             []byte(`{"error":"` + ctxErr.Error() + `"}`),
			channelID:        &cfg.ID,
			succeeded:        false,
			isClientCanceled: isClientCanceled,
			nextAction:       cooldown.ActionReturnClient,
		}
	}

	// Fail-fast：ctx 已结束（客户端断开/请求超时）时不要再做任何 I/O（查库、选Key、发请求）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return makeCtxDoneResult(ctxErr), nil
	}

	// 查询渠道的API Keys（缓存优先，缓存不可用自动降级到数据库查询）
	apiKeys, err := s.getAPIKeys(ctx, cfg.ID)
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

	var lastFailure *proxyResult

	// 准备请求体（处理模型重定向）
	// [INFO] 修复：保存重定向后的模型名称，用于日志记录和调试
	actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)

	// [FIX] 2026-01: 模型名变更时同步替换 URL 路径
	// 场景：Gemini API 的模型名在 URL 路径中（如 /v1beta/models/gemini-3-flash:streamGenerateContent）
	// 如果模糊匹配将 gemini-3-flash 改为 gemini-3-flash-preview，URL 路径也需要同步更新
	requestPath := replaceModelInPath(reqCtx.requestPath, reqCtx.originalModel, actualModel)

	// Key重试循环
	for range maxKeyRetries {
		// 检查context是否已取消/超时
		if ctxErr := ctx.Err(); ctxErr != nil {
			return makeCtxDoneResult(ctxErr), nil
		}

		// 选择可用的API Key（直接传入apiKeys，避免重复查询）
		keyIndex, selectedKey, selectErr := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)
		if selectErr != nil {
			// 所有Key都在冷却中，返回特殊错误标识（使用sentinel error而非魔法字符串）
			return nil, fmt.Errorf("%w: %v", ErrAllKeysUnavailable, selectErr)
		}

		// 标记Key为已尝试
		triedKeys[keyIndex] = true

		// 更新活跃请求的渠道信息（用于前端显示）
		if reqCtx.activeReqID > 0 {
			s.activeRequests.Update(reqCtx.activeReqID, cfg.ID, cfg.Name, cfg.GetChannelType(), selectedKey, reqCtx.tokenID)
		}

		// 单次转发尝试（传递实际的API Key字符串）
		// [INFO] 修复：传递 actualModel 用于日志记录
		// [FIX] 2026-01: 传递 requestPath（可能经过模型名替换）
		result, nextAction := s.forwardAttempt(
			ctx, cfg, keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, requestPath, w)

		if result != nil {
			if result.succeeded {
				return result, nil
			}
			lastFailure = result
		}

		if nextAction == cooldown.ActionRetryKey {
			continue
		}
		if nextAction == cooldown.ActionRetryChannel {
			break
		}
		break
	}

	// Key重试循环结束：返回最后一次失败结果
	if lastFailure != nil {
		return lastFailure, nil
	}

	// 所有Key都尝试过但都失败（无 lastFailure 说明循环未执行或逻辑异常）
	return nil, ErrAllKeysExhausted
}

func shouldCheckSoftErrorForChannelType(channelType string) bool {
	switch util.NormalizeChannelType(channelType) {
	case util.ChannelTypeAnthropic, util.ChannelTypeCodex:
		return true
	default:
		return false
	}
}

// checkSoftError 检测“200 OK 但实际是错误”的软错误响应
// 原则：宁可漏判也不要误判（避免把正常响应当错误导致重试/冷却）
//
// 规则：
// - JSON：先解析，只看顶层结构（顶层 error 字段 或 顶层 type=="error"）
// - text/plain：只接受“前缀匹配 + 短消息”，禁止 Contains 误判用户内容
// - SSE：若看起来像 SSE（data:/event:），直接跳过
func checkSoftError(data []byte, contentType string) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}

	// 非 JSON 形态下，先排除 SSE（上游可能用 text/plain 返回 SSE）
	if trimmed[0] != '{' {
		if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
			bytes.Contains(data, []byte("\ndata:")) || bytes.Contains(data, []byte("\nevent:")) {
			return false
		}
	}

	ctLower := strings.ToLower(contentType)
	isJSONCT := strings.Contains(ctLower, "application/json")

	// JSON：仅看顶层结构
	if isJSONCT || trimmed[0] == '{' {
		var obj map[string]any
		if err := sonic.Unmarshal(trimmed, &obj); err == nil {
			if v, ok := obj["error"]; ok && v != nil {
				return true
			}
			if t, ok := obj["type"].(string); ok && strings.EqualFold(t, "error") {
				return true
			}
			return false
		}
		// 形态像 JSON（以 '{' 开头）但解析失败：不猜，避免误判
		if trimmed[0] == '{' {
			return false
		}
		// Content-Type 标注为 JSON 但内容不是 JSON：允许继续走 text/plain 的“前缀+短消息”兜底
	}

	// text/plain：仅前缀 + 短消息
	const maxPlainLen = 256
	if len(trimmed) > maxPlainLen {
		return false
	}
	if bytes.HasPrefix(trimmed, []byte("当前模型负载过高")) {
		return true
	}
	if bytes.HasPrefix(trimmed, []byte("Current model load too high")) {
		return true
	}

	return false
}
