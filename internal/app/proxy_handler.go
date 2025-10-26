package app

import (
    "ccLoad/internal/config"
    "ccLoad/internal/model"
    "ccLoad/internal/util"
    "context"
    "errors"
    "fmt"
    "io"
    "net/http"
    "os"
    "strconv"
    "strings"
    "time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

var errUnknownChannelType = errors.New("unknown channel type for path")
var errBodyTooLarge = errors.New("request body too large")

// ============================================================================
// 并发控制
// ============================================================================

// acquireConcurrencySlot 获取并发槽位
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
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

// ============================================================================
// 请求解析
// ============================================================================

// parseIncomingRequest 解析传入的代理请求
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
// 返回：(originalModel, body, isStreaming, error)
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
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
func (s *Server) selectRouteCandidates(ctx context.Context, c *gin.Context, originalModel string) ([]*model.Config, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 智能路由选择：根据请求类型选择不同的路由策略
	if requestMethod == http.MethodGet && detectChannelTypeFromPath(requestPath) == util.ChannelTypeGemini {
		// 按渠道类型筛选Gemini渠道
		return s.selectCandidatesByChannelType(ctx, util.ChannelTypeGemini)
	}

	channelType := detectChannelTypeFromPath(requestPath)
	if channelType == "" {
		return nil, errUnknownChannelType
	}

	return s.selectCandidatesByModelAndType(ctx, originalModel, channelType)
}

// ============================================================================
// 主请求处理器
// ============================================================================

// handleProxyRequest 通用透明代理处理器
// ✅ P2重构: 从proxy.go提取，遵循SRP原则
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
        if errors.Is(err, errBodyTooLarge) {
            c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
            return
        }
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
		if errors.Is(err, errUnknownChannelType) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unsupported path"})
			return
		}
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
            // 精确计数（P1）：记录渠道进入冷却
            s.noteChannelCooldown(cfg.ID, true)
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
