package app

import (
	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

var errUnknownChannelType = errors.New("unknown channel type for path")
var errBodyTooLarge = errors.New("request body too large")
var ErrAllKeysUnavailable = errors.New("all channel keys unavailable")
var ErrAllKeysExhausted = errors.New("all keys exhausted")

// ============================================================================
// 并发控制
// ============================================================================

// acquireConcurrencySlot 获取并发槽位，返回release函数和状态
// ok=false 表示客户端已取消请求
func (s *Server) acquireConcurrencySlot(c *gin.Context) (release func(), ok bool) {
	select {
	case s.concurrencySem <- struct{}{}:
		return func() { <-s.concurrencySem }, true
	case <-c.Request.Context().Done():
		ctxErr := c.Request.Context().Err()
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "request timeout while waiting for slot"})
			return nil, false
		}
		c.JSON(StatusClientClosedRequest, gin.H{"error": "request cancelled while waiting for slot"})
		return nil, false
	}
}

// ============================================================================
// 请求解析
// ============================================================================

// parseIncomingRequest 返回 (originalModel, body, isStreaming, error)
func parseIncomingRequest(c *gin.Context) (string, []byte, bool, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 读取请求体（带上限，防止大包打爆内存）
	// 默认 2MB，可通过 CCLOAD_MAX_BODY_BYTES 调整
	maxBody := int64(config.DefaultMaxBodyBytes)
	if v := os.Getenv("CCLOAD_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxBody = int64(n)
		}
	}
	limited := io.LimitReader(c.Request.Body, maxBody+1)
	all, err := io.ReadAll(limited)
	if err != nil {
		return "", nil, false, fmt.Errorf("failed to read body: %w", err)
	}
	_ = c.Request.Body.Close()
	if int64(len(all)) > maxBody {
		return "", nil, false, errBodyTooLarge
	}

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

// ============================================================================
// 路由选择
// ============================================================================

// selectRouteCandidates 根据请求选择路由候选
// 从proxy.go提取，遵循SRP原则
func (s *Server) selectRouteCandidates(ctx context.Context, c *gin.Context, originalModel string) ([]*model.Config, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 智能路由选择：根据请求类型选择不同的路由策略
	if requestMethod == http.MethodGet && util.DetectChannelTypeFromPath(requestPath) == util.ChannelTypeGemini {
		// 按渠道类型筛选Gemini渠道
		return s.selectCandidatesByChannelType(ctx, util.ChannelTypeGemini)
	}

	channelType := util.DetectChannelTypeFromPath(requestPath)
	if channelType == "" {
		return nil, errUnknownChannelType
	}

	return s.selectCandidatesByModelAndType(ctx, originalModel, channelType)
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

	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	originalModel, all, isStreaming, err := parseIncomingRequest(c)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	timeout := parseTimeout(c.Request.URL.Query(), c.Request.Header)
	ctx := c.Request.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cands, err := s.selectRouteCandidates(ctx, c, originalModel)
	if err != nil {
		if errors.Is(err, errUnknownChannelType) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unsupported path"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	if len(cands) == 0 {
		s.AddLogAsync(&model.LogEntry{
			Time:        model.JSONTime{Time: time.Now()},
			Model:       originalModel,
			StatusCode:  503,
			Message:     "no available upstream (all cooled or none)",
			IsStreaming: isStreaming,
			ClientIP:    c.ClientIP(),
		})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available upstream (all cooled or none)"})
		return
	}

	// 从context提取tokenHash和tokenID（用于统计和日志，2025-11新增tokenHash, 2025-12新增tokenID）
	tokenHash, _ := c.Get("token_hash")
	tokenHashStr, _ := tokenHash.(string)
	tokenID, _ := c.Get("token_id")
	tokenIDInt64, _ := tokenID.(int64)

	reqCtx := &proxyRequestContext{
		originalModel: originalModel,
		requestMethod: requestMethod,
		requestPath:   requestPath,
		rawQuery:      c.Request.URL.RawQuery,
		body:          all,
		header:        c.Request.Header,
		isStreaming:   isStreaming,
		tokenHash:     tokenHashStr,
		tokenID:       tokenIDInt64,
		clientIP:      c.ClientIP(),
	}

	// 按优先级遍历候选渠道，尝试转发
	var lastResult *proxyResult
	for _, cfg := range cands {
		result, err := s.tryChannelWithKeys(ctx, cfg, reqCtx, c.Writer)

		// 所有Key冷却：触发渠道级冷却(503)，防止后续请求重复尝试
		// 使用 cooldownManager.HandleError 统一处理（DRY原则）
		if err != nil && errors.Is(err, ErrAllKeysUnavailable) {
			// 统一走 applyCooldownDecision：断开取消链+按决策执行缓存失效
			s.applyCooldownDecision(ctx, cfg, httpErrorInputFromParts(cfg.ID, cooldown.NoKeyIndex, 503, nil, nil))
			continue
		}

		// [WARN] 所有Key验证失败（88code套餐等），尝试下一个渠道
		if err != nil && errors.Is(err, ErrAllKeysExhausted) {
			log.Printf("[WARN] 渠道 %s (ID=%d) 所有Key验证失败，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if result != nil {
			if result.succeeded {
				return
			}

			lastResult = result

			// 客户端已取消：别再浪费资源“重试”了。
			if result.isClientCanceled {
				break
			}

			if shouldStopTryingChannels(result) {
				break
			}
		}
	}

	// 所有渠道都失败：返回“最后一次实际失败”的状态码（并映射内部状态码），避免一律伪装成503。
	finalStatus := determineFinalClientStatus(lastResult)

	msg := "exhausted backends"
	if lastResult != nil && lastResult.isClientCanceled {
		msg = "client closed request (context canceled)"
	} else if lastResult != nil && lastResult.status == 499 && finalStatus != 499 {
		// 上游返回 499 没有任何“客户端取消”的语义价值：对外统一视为网关错误。
		msg = "upstream returned 499 (mapped)"
	} else if finalStatus != http.StatusServiceUnavailable {
		msg = fmt.Sprintf("upstream status %d", finalStatus)
	}

	// [FIX] 2025-12: 过滤无实际请求的客户端取消日志
	// 场景：请求在选择渠道/Key阶段就被取消，没有发送到上游
	// 判断：isClientCanceled=true 且 duration=0 说明没有实际请求
	// 这种情况不记录日志（噪音），有实际请求的取消会在 handleNetworkError 中记录
	skipLog := lastResult != nil && lastResult.isClientCanceled && lastResult.duration == 0
	if !skipLog {
		s.AddLogAsync(&model.LogEntry{
			Time:        model.JSONTime{Time: time.Now()},
			Model:       originalModel,
			StatusCode:  finalStatus,
			Message:     msg,
			IsStreaming: isStreaming,
			ClientIP:    reqCtx.clientIP,
		})
	}

	if lastResult != nil && lastResult.status != 0 {
		// 透明代理原则：透传所有上游响应（状态码+header+body）
		writeResponseWithHeaders(c.Writer, finalStatus, lastResult.header, lastResult.body)
		return
	}

	c.JSON(finalStatus, gin.H{"error": "no upstream available"})
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
