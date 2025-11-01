package app

import (
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

	// 检测首字节超时错误：使用统一的内部状态码+冷却策略
	var statusCode int
	if reqCtx.firstByteTimeoutTriggered() {
		statusCode = util.StatusFirstByteTimeout
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", duration)
		if s.firstByteTimeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, s.firstByteTimeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		util.SafePrintf("⏱️  [上游首字节超时] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, s.firstByteTimeout, duration)
	} else if errors.Is(err, context.DeadlineExceeded) && reqCtx.isStreaming {
		// 流式请求读取首字节超时：保留历史逻辑
		err = fmt.Errorf("upstream timeout after %.2fs (streaming request): %w",
			duration, err)
		statusCode = util.StatusFirstByteTimeout
		util.SafePrintf("⏱️  [上游超时] 渠道ID=%d, 超时时长=%.2fs, 将触发冷却", cfg.ID, duration)
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
func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(resp.Body)
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

// handleSuccessResponse 处理成功响应（流式传输）
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	hdrClone http.Header,
	w http.ResponseWriter,
) (*fwResult, float64, error) {
	// 写入响应头
	filterAndWriteResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 🔍 诊断：记录首字节数据实际到达时间和传输统计
	actualFirstByteTime := firstByteTime
	var readStats *streamReadStats
	if reqCtx.isStreaming {
		readStats = &streamReadStats{}
		// 创建包装Reader，记录读取统计信息
		bodyWrapper := &firstByteDetector{
			ReadCloser: resp.Body,
			stats:      readStats,
			onFirstRead: func() {
				actualFirstByteTime = reqCtx.Duration()
			},
		}
		resp.Body = bodyWrapper
	}

	// ✅ SSE优化（2025-10-17）：根据Content-Type选择合适的缓冲区大小
	// text/event-stream → 4KB缓冲区（降低首Token延迟60~80%）
	// 其他类型 → 32KB缓冲区（保持大文件传输性能）
	var streamErr error
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		// SSE流式响应：使用小缓冲区优化实时性
		streamErr = streamCopySSE(reqCtx.ctx, resp.Body, w)
	} else {
		// 非SSE响应：使用大缓冲区优化吞吐量
		streamErr = streamCopy(reqCtx.ctx, resp.Body, w)
	}

	duration := reqCtx.Duration()

	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		FirstByteTime: actualFirstByteTime, // 使用实际的首字节时间
	}, duration, streamErr
}

// handleResponse 处理 HTTP 响应（错误或成功）
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleResponse(
	reqCtx *requestContext,
	resp *http.Response,
	firstByteTime float64,
	w http.ResponseWriter,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()

	// 错误状态：读取完整响应体
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, firstByteTime, hdrClone)
	}

	// 成功状态：流式转发
	return s.handleSuccessResponse(reqCtx, resp, firstByteTime, hdrClone, w)
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
	// 移除defer reqCtx.Close()调用（Close方法已删除）
	reqCtx := s.newRequestContext(ctx, requestPath, body)

	// 2. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, body, hdr, rawQuery, requestPath)
	if err != nil {
		return nil, 0, err
	}

	// 3. 发送请求
	resp, err := s.client.Do(req)
	if err != nil {
		return s.handleRequestError(reqCtx, cfg, err)
	}
	defer resp.Body.Close()

	// 4. 首字节到达，停止计时器
	reqCtx.stopFirstByteTimer()
	firstByteTime := reqCtx.Duration()

	// 5. 处理响应
	return s.handleResponse(reqCtx, resp, firstByteTime, w)
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
	for i := 0; i < maxKeyRetries; i++ {
		// 选择可用的API Key（直接传入apiKeys，避免重复查询）
		keyIndex, selectedKey, err := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)
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
