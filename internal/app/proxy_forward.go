package app

import (
	"bufio"
	"bytes"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// SSEProbeSize 用于探测 text/plain 内容是否包含 SSE 事件的前缀长度（2KB 足够覆盖小事件）
	SSEProbeSize = 2 * 1024
)

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

	// 检测超时错误：使用统一的内部状态码+冷却策略
	var statusCode int
	if reqCtx.firstByteTimeoutTriggered() {
		// 流式请求首字节超时（定时器触发）
		statusCode = util.StatusFirstByteTimeout
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", duration)
		timeout := s.firstByteTimeout
		if timeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, timeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		log.Printf("⏱️  [上游首字节超时] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, timeout, duration)
	} else if errors.Is(err, context.DeadlineExceeded) {
		if reqCtx.isStreaming {
			// 流式请求超时
			err = fmt.Errorf("upstream timeout after %.2fs (streaming): %w", duration, err)
			statusCode = util.StatusFirstByteTimeout
			log.Printf("⏱️  [流式请求超时] 渠道ID=%d, 耗时=%.2fs", cfg.ID, duration)
		} else {
			// 非流式请求超时（context.WithTimeout触发）
			err = fmt.Errorf("upstream timeout after %.2fs (non-stream, threshold=%v): %w",
				duration, s.nonStreamTimeout, err)
			statusCode = 504 // Gateway Timeout
			log.Printf("⏱️  [非流式请求超时] 渠道ID=%d, 阈值=%v, 耗时=%.2fs", cfg.ID, s.nonStreamTimeout, duration)
		}
	} else {
		// 其他错误：使用统一分类器
		statusCode, _, _ = util.ClassifyError(err)
	}

	return &fwResult{
		Status:        statusCode,
		Body:          []byte(err.Error()),
		FirstByteTime: duration,
	}, duration, err
}

// handleErrorResponse 处理错误响应（读取完整响应体）
// 从proxy.go提取，遵循SRP原则
// 限制错误体大小防止 OOM（与入站 2MB 限制对称）
const maxErrorBodySize = 2 * 1024 * 1024 // 2MB

func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if readErr != nil {
		s.AddLogAsync(&model.LogEntry{
			Time:    model.JSONTime{Time: time.Now()},
			Message: fmt.Sprintf("error reading upstream body: %v", readErr),
		})
	}

	duration := reqCtx.Duration()

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          rb,
		FirstByteTime: firstByteTime,
	}, duration, nil
}

// streamAndParseResponse 根据Content-Type选择合适的流式传输策略并解析usage
// 返回: (usageParser, streamErr)
func streamAndParseResponse(ctx context.Context, body io.ReadCloser, w http.ResponseWriter, contentType string, channelType string, isStreaming bool) (usageParser, error) {
	// SSE流式响应
	if strings.Contains(contentType, "text/event-stream") {
		parser := newSSEUsageParser(channelType)
		err := streamCopySSE(ctx, body, w, parser.Feed)
		return parser, err
	}

	// 非标准SSE场景：上游以text/plain发送SSE事件
	if strings.Contains(contentType, "text/plain") && isStreaming {
		reader := bufio.NewReader(body)
		probe, _ := reader.Peek(SSEProbeSize)

		if looksLikeSSE(probe) {
			parser := newSSEUsageParser(channelType)
			err := streamCopySSE(ctx, io.NopCloser(reader), w, parser.Feed)
			return parser, err
		}
		parser := newJSONUsageParser(channelType)
		err := streamCopy(ctx, io.NopCloser(reader), w, parser.Feed)
		return parser, err
	}

	// 非SSE响应：边转发边缓存
	parser := newJSONUsageParser(channelType)
	err := streamCopy(ctx, body, w, parser.Feed)
	return parser, err
}

// isClientDisconnectError 判断是否为客户端断开导致的错误
// 包括：context取消、HTTP/2流关闭、连接重置等
func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	// HTTP/2 流关闭相关错误（客户端断开时 resp.Body.Close() 触发）
	errStr := err.Error()
	return strings.Contains(errStr, "http2: response body closed") ||
		strings.Contains(errStr, "stream error: stream ID") ||
		strings.Contains(errStr, "client disconnected")
}

// buildStreamDiagnostics 生成流诊断消息
// 触发条件：(1) 流传输错误  (2) 流式请求但没有usage数据（疑似不完整响应）
func buildStreamDiagnostics(streamErr error, readStats *streamReadStats, hasUsage bool, channelType string, contentType string, parser usageParser) string {
	if readStats == nil {
		return ""
	}

	bytesRead := readStats.totalBytes
	readCount := readStats.readCount
	needsUsageCheck := channelType == util.ChannelTypeAnthropic || channelType == util.ChannelTypeCodex

	// 获取接收到的数据（用于诊断）
	var receivedSnippet string
	if parser != nil {
		data := parser.GetReceivedData()
		maxLen := 500 // 最多显示500字节
		if len(data) > maxLen {
			receivedSnippet = string(data[:maxLen]) + "..."
		} else {
			receivedSnippet = string(data)
		}
	}

	// 情况1:流传输异常中断(排除客户端主动断开:499/HTTP2流关闭)
	if streamErr != nil && !isClientDisconnectError(streamErr) {
		if needsUsageCheck {
			return fmt.Sprintf("⚠️ 流传输中断: 错误=%v | 已读取=%d字节(分%d次) | usage数据=%v | 渠道=%s | Content-Type=%s | 接收内容=%q",
				streamErr, bytesRead, readCount, hasUsage, channelType, contentType, receivedSnippet)
		}
		return fmt.Sprintf("⚠️ 流传输中断: 错误=%v | 已读取=%d字节(分%d次) | 渠道=%s | Content-Type=%s | 接收内容=%q",
			streamErr, bytesRead, readCount, channelType, contentType, receivedSnippet)
	}

	// 情况2:流正常结束但没有usage数据(疑似上游未发送完整响应)
	if !hasUsage && bytesRead > 0 && needsUsageCheck {
		// 🔍 诊断增强:添加渠道+Content-Type,帮助定位问题源
		// 如果Content-Type不是text/event-stream,可能是上游错误响应
		return fmt.Sprintf("⚠️ 流响应不完整: 正常EOF但无usage | 已读取=%d字节(分%d次) | 渠道=%s | Content-Type=%s | 接收内容=%q",
			bytesRead, readCount, channelType, contentType, receivedSnippet)
	}

	return ""
}

// handleSuccessResponse 处理成功响应（流式传输）
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	_ *int64,
	_ string,
) (*fwResult, float64, error) {
	// 写入响应头
	filterAndWriteResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 设置流式读取统计
	actualFirstByteTime := firstByteTime
	var readStats *streamReadStats
	if reqCtx.isStreaming {
		readStats = &streamReadStats{}
		resp.Body = &firstByteDetector{
			ReadCloser: resp.Body,
			stats:      readStats,
			onFirstRead: func() {
				actualFirstByteTime = reqCtx.Duration()
			},
		}
	}

	// 流式传输并解析usage
	contentType := resp.Header.Get("Content-Type")
	usageParser, streamErr := streamAndParseResponse(reqCtx.ctx, resp.Body, w, contentType, channelType, reqCtx.isStreaming)

	// 构建结果
	result := &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		FirstByteTime: actualFirstByteTime,
	}

	// 提取usage数据和错误事件
	if usageParser != nil {
		result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = usageParser.GetUsage()
		if errorEvent := usageParser.GetLastError(); errorEvent != nil {
			result.SSEErrorEvent = errorEvent
		}
	}

	// 生成流诊断消息
	if reqCtx.isStreaming {
		hasUsage := result.InputTokens > 0 || result.OutputTokens > 0
		// 🔍 诊断增强: 传递contentType+parser帮助定位问题(区分SSE/JSON/其他,记录接收内容)
		if diagMsg := buildStreamDiagnostics(streamErr, readStats, hasUsage, channelType, contentType, usageParser); diagMsg != "" {
			result.StreamDiagMsg = diagMsg
			log.Print(diagMsg)
		}
	}

	return result, reqCtx.Duration(), streamErr
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
	firstByteTime float64,
	w http.ResponseWriter,
	channelType string,
	cfg *model.Config,
	apiKey string,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()

	// 错误状态：读取完整响应体
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, firstByteTime, hdrClone)
	}

	// ✅ 空响应检测：200状态码但Content-Length=0视为上游故障
	// 常见于CDN/代理错误、认证失败等异常场景，应触发渠道级重试
	if contentLen := resp.Header.Get("Content-Length"); contentLen == "0" {
		duration := reqCtx.Duration()
		err := fmt.Errorf("upstream returned empty response (200 OK with Content-Length: 0)")

		return &fwResult{
			Status:        resp.StatusCode, // 保留原始200状态码
			Header:        hdrClone,
			Body:          []byte(err.Error()),
			FirstByteTime: firstByteTime,
		}, duration, err
	}

	// 成功状态：流式转发（传递渠道信息用于日志记录）
	channelID := &cfg.ID
	return s.handleSuccessResponse(reqCtx, resp, firstByteTime, hdrClone, w, channelType, channelID, apiKey)
}

// ============================================================================
// 核心转发函数
// ============================================================================

// forwardOnceAsync 异步流式转发，透明转发客户端原始请求
// 从proxy.go提取，遵循SRP原则
// 参数新增 apiKey 用于直接传递已选中的API Key（从KeySelector获取）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *model.Config, apiKey string, method string, body []byte, hdr http.Header, rawQuery, requestPath string, w http.ResponseWriter) (*fwResult, float64, error) {
	// 1. 创建请求上下文（处理超时）
	reqCtx := s.newRequestContext(ctx, requestPath, body)
	defer reqCtx.cleanup() // ✅ 统一清理：定时器 + context（总是安全）

	// 2. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, body, hdr, rawQuery, requestPath)
	if err != nil {
		return nil, 0, err
	}

	// 3. 发送请求
	resp, err := s.client.Do(req)

	// ✅ 修复（2025-12）：客户端取消时主动关闭 response body，立即中断上游传输
	// 问题：streamCopy 中的 Read 阻塞时，无法立即响应 context 取消，上游继续生成完整响应
	// 解决：使用 Go 1.21+ context.AfterFunc 替代手动 goroutine（零泄漏风险）
	//   - HTTP/1.1: 关闭 TCP 连接 → 上游收到 RST，立即停止发送
	//   - HTTP/2: 发送 RST_STREAM 帧 → 取消当前 stream（不影响同连接的其他请求）
	// 效果：避免 AI 流式生成场景下，用户点"停止"后上游仍生成数千 tokens 的浪费
	if resp != nil {
		// 使用 sync.Once 确保 body 只关闭一次（协调 defer 和 AfterFunc）
		var bodyCloseOnce sync.Once
		closeBodySafely := func() {
			bodyCloseOnce.Do(func() {
				resp.Body.Close()
			})
		}

		// ✅ 使用 context.AfterFunc 监听客户端取消（Go 1.21+，标准库保证无泄漏）
		stop := context.AfterFunc(ctx, closeBodySafely)
		defer stop() // 取消注册（请求正常结束时避免内存泄漏）

		// 正常返回时关闭（与 AfterFunc 互斥，Once 保证只执行一次）
		defer closeBodySafely()
	}

	if err != nil {
		return s.handleRequestError(reqCtx, cfg, err)
	}

	// 4. 首字节到达，停止计时器
	reqCtx.stopFirstByteTimer()
	firstByteTime := reqCtx.Duration()

	// 5. 处理响应(传递channelType用于精确识别usage格式,传递渠道信息用于日志记录)
	return s.handleResponse(reqCtx, resp, firstByteTime, w, cfg.ChannelType, cfg, apiKey)
}

// ============================================================================
// 单次转发尝试
// ============================================================================

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 从proxy.go提取，遵循SRP原则
// 返回：(proxyResult, shouldContinueRetry, shouldBreakToNextChannel)
func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string, // ✅ 重定向后的实际模型名称
	bodyToSend []byte,
	w http.ResponseWriter,
) (*proxyResult, bool, bool) {
	// 🔍 Key级验证器检查(88code套餐验证等)
	// 每个Key单独验证，避免误杀免费key或误放付费key
	if s.validatorManager != nil {
		available, reason := s.validatorManager.ValidateChannel(ctx, cfg, selectedKey)
		if !available {
			// Key验证失败: 跳过此key，尝试下一个
			log.Printf("🔍 渠道 %s (ID=%d) Key#%d 验证失败: %s, 跳过", cfg.Name, cfg.ID, keyIndex, reason)
			return nil, true, false // shouldContinue=true, shouldBreak=false
		}
	}

	// 转发请求（传递实际的API Key字符串）
	res, duration, err := s.forwardOnceAsync(ctx, cfg, selectedKey, reqCtx.requestMethod,
		bodyToSend, reqCtx.header, reqCtx.rawQuery, reqCtx.requestPath, w)

	// 处理网络错误或异常响应（如空响应）
	// ✅ 修复：handleResponse可能返回err即使StatusCode=200（例如Content-Length=0）
	if err != nil {
		return s.handleNetworkError(ctx, cfg, keyIndex, actualModel, selectedKey, reqCtx.tokenID, duration, err)
	}

	// 处理成功响应（仅当err==nil且状态码2xx时）
	if res.Status >= 200 && res.Status < 300 {
		// ✅ 检查SSE流中是否有error事件（如1308错误）
		// 虽然HTTP状态码是200，但error事件表示实际上发生了错误，需要触发冷却逻辑
		if res.SSEErrorEvent != nil {
			// 将SSE error事件当作HTTP错误处理
			// 注意：不改变HTTP状态码，因为上游确实返回的是200
			// 但我们需要将错误体传递给冷却管理器来触发冷却
			log.Printf("⚠️  [SSE错误处理] HTTP状态码200但检测到SSE error事件，触发冷却逻辑")
			// 将error事件存入Body字段，用于冷却管理器解析
			res.Body = res.SSEErrorEvent
			return s.handleProxyErrorResponse(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
		}

		// ✅ 检查流响应是否不完整（2025-12新增）
		// 虽然HTTP状态码是200且流传输结束，但检测到流响应不完整或流传输中断，需要触发冷却逻辑
		// 触发条件：(1) 流传输错误  (2) 流式请求但没有usage数据（疑似不完整响应）
		if res.StreamDiagMsg != "" {
			log.Printf("⚠️  [流响应不完整] HTTP状态码200但检测到流响应不完整，触发冷却逻辑: %s", res.StreamDiagMsg)
			// 使用内部状态码 StatusStreamIncomplete 标识流响应不完整
			// 这将触发渠道级冷却，因为这通常是上游服务问题（网络不稳定、负载过高等）
			res.Body = []byte(res.StreamDiagMsg)
			originalStatus := res.Status
			res.Status = util.StatusStreamIncomplete // 599 - 流响应不完整
			defer func() { res.Status = originalStatus }() // 恢复原始状态码（日志已记录）
			return s.handleProxyErrorResponse(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
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
	// 查询渠道的API Keys（使用缓存层，<1ms vs 数据库查询10-20ms）
	// 性能优化：缓存优先，避免高并发场景下的数据库瓶颈
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

	// 准备请求体（处理模型重定向）
	// ✅ 修复：保存重定向后的模型名称，用于日志记录和调试
	actualModel, bodyToSend := prepareRequestBody(cfg, reqCtx)

	// Key重试循环
	for range maxKeyRetries {
		// 选择可用的API Key（直接传入apiKeys，避免重复查询）
		keyIndex, selectedKey, err := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)
		if err != nil {
			// 所有Key都在冷却中，返回特殊错误标识（使用sentinel error而非魔法字符串）
			return nil, fmt.Errorf("%w: %v", ErrAllKeysUnavailable, err)
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
