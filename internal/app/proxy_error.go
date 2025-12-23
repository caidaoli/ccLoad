package app

import (
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
	"context"
	"errors"
	"log"
	"time"
)

// ============================================================================
// 错误处理核心函数
// ============================================================================

// handleProxyError 统一错误处理与冷却决策（遵循OCP原则）
// 使用 cooldownManager 统一处理冷却逻辑（DRY原则）
// 返回：(处理动作, 是否需要保存响应信息)
func (s *Server) handleProxyError(ctx context.Context, cfg *model.Config, keyIndex int,
	res *fwResult, err error) (cooldown.Action, bool) {

	var statusCode int
	var errorBody []byte
	var isNetworkError bool
	var headers map[string][]string // 提取响应头用于429错误分析

	// 确定状态码、错误体和错误类型
	if err != nil {
		// 网络错误：使用统一分类器
		classifiedStatus, _, shouldRetry := util.ClassifyError(err)
		if !shouldRetry {
			return cooldown.ActionReturnClient, false
		}
		statusCode = classifiedStatus
		errorBody = []byte(err.Error())
		isNetworkError = true // [INFO] 标记为网络错误
		headers = nil         // 网络错误无响应头
	} else {
		// HTTP错误
		statusCode = res.Status
		errorBody = res.Body
		isNetworkError = false // [INFO] 标记为HTTP错误
		headers = res.Header   // 提取响应头用于429分析
	}

	// 使用 cooldownManager 统一处理冷却决策
	// 好处：消除重复逻辑，单一职责，便于测试和维护
	// manager.HandleError 现在不返回错误（日志记录方式）
	// 因此这里不再需要检查 cooldownErr，直接使用 action 即可
	//
	// [FIX] 2025-12: 使用 WithoutCancel 断开取消链，但保留 context 值（如 trace ID）
	// 原因：当客户端取消或首字节超时时，请求 ctx 已经 Done，
	// 会导致冷却写入被短路，同一个坏 Key/渠道被反复打爆
	cooldownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	action, _ := s.cooldownManager.HandleError(cooldownCtx, cfg.ID, keyIndex, statusCode, errorBody, isNetworkError, headers)

	// 根据冷却管理器的决策执行相应动作
	switch action {
	case cooldown.ActionRetryKey:
		// Key级错误：立即刷新相关缓存
		s.invalidateChannelRelatedCache(cfg.ID)
		return action, true

	case cooldown.ActionRetryChannel:
		// 渠道级错误：刷新渠道与冷却缓存，确保下次选择避开问题渠道
		s.invalidateChannelRelatedCache(cfg.ID)
		return action, true

	default:
		// 客户端错误或未知错误：直接返回
		return action, false
	}
}

// handleNetworkError 处理网络错误
// 从proxy.go提取，遵循SRP原则
// [FIX] 2025-12: 添加 res 和 reqCtx 参数，用于保留 499 场景下已消耗的 token 统计
// 契约: reqCtx 不能为 nil（用于获取 originalModel, tokenHash, isStreaming）
func (s *Server) handleNetworkError(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string, // [INFO] 重定向后的实际模型名称
	selectedKey string,
	authTokenID int64, // [INFO] API令牌ID（用于日志记录，2025-12新增）
	clientIP string, // [INFO] 客户端IP（用于日志记录，2025-12新增）
	duration float64,
	err error,
	res *fwResult, // [FIX] 流式响应中途取消时，res 包含已解析的 token 统计
	reqCtx *proxyRequestContext, // [FIX] 用于获取 tokenHash 和 isStreaming
) (*proxyResult, bool, bool) {
	statusCode, _, _ := util.ClassifyError(err)

	// 记录日志：requestModel=原始请求模型，actualModel=实际转发模型
	s.AddLogAsync(buildLogEntry(logEntryParams{
		RequestModel: reqCtx.originalModel,
		ActualModel:  actualModel,
		ChannelID:    cfg.ID,
		StatusCode:   statusCode,
		Duration:     duration,
		IsStreaming:  false,
		APIKeyUsed:   selectedKey,
		AuthTokenID:  authTokenID,
		ClientIP:     clientIP,
		Result:       res,
		ErrMsg:       err.Error(),
	}))

	// [FIX] 2025-12: 保留 499 场景下已消耗的 token 统计
	// 场景：流式响应中途取消（用户点"停止"），上游已消耗 token 但之前被丢弃
	// 修复：即使请求失败，也记录已解析的 token 统计（用于计费和统计）
	if res != nil && hasConsumedTokens(res) {
		// isSuccess=false 表示请求失败，但仍记录已消耗的 token
		s.updateTokenStatsAsync(reqCtx.tokenHash, false, duration, reqCtx.isStreaming, res, actualModel)
	}

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, nil, err)
	if action == cooldown.ActionReturnClient {
		return &proxyResult{
			status:           statusCode,
			body:             []byte(err.Error()),
			channelID:        &cfg.ID,
			message:          truncateErr(err.Error()),
			duration:         duration,
			succeeded:        false,
			isClientCanceled: errors.Is(err, context.Canceled),
		}, false, false
	}

	// 修复首字节超时不切换渠道的问题
	// 当 handleProxyError 返回 ActionRetryChannel 时，应该立即切换到下一个渠道
	// 而不是继续尝试当前渠道的其他Key
	if action == cooldown.ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}

// hasConsumedTokens 检查响应是否包含已消耗的 token 统计
// 用于判断是否需要在错误场景下记录 token 统计
func hasConsumedTokens(res *fwResult) bool {
	if res == nil {
		return false
	}
	return res.InputTokens > 0 || res.OutputTokens > 0 ||
		res.CacheReadInputTokens > 0 || res.CacheCreationInputTokens > 0
}

type tokenStatsUpdate struct {
	tokenHash           string
	isSuccess           bool
	duration            float64
	isStreaming         bool
	firstByteTime       float64
	promptTokens        int64
	completionTokens    int64
	cacheReadTokens     int64
	cacheCreationTokens int64
	costUSD             float64
}

func (s *Server) tokenStatsWorker() {
	defer s.wg.Done()

	if s.tokenStatsCh == nil {
		return
	}

	for {
		select {
		case <-s.shutdownCh:
			s.drainTokenStats()
			return
		case upd := <-s.tokenStatsCh:
			s.applyTokenStatsUpdate(upd)
		}
	}
}

func (s *Server) drainTokenStats() {
	for {
		select {
		case upd := <-s.tokenStatsCh:
			s.applyTokenStatsUpdate(upd)
		default:
			return
		}
	}
}

func (s *Server) applyTokenStatsUpdate(upd tokenStatsUpdate) {
	updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := s.store.UpdateTokenStats(updateCtx, upd.tokenHash, upd.isSuccess, upd.duration, upd.isStreaming, upd.firstByteTime, upd.promptTokens, upd.completionTokens, upd.cacheReadTokens, upd.cacheCreationTokens, upd.costUSD); err != nil {
		log.Printf("ERROR: failed to update token stats for hash=%s: %v", upd.tokenHash, err)
	}
}

// updateTokenStatsAsync 异步更新Token统计（DRY原则：消除重复代码）
// 参数:
//   - tokenHash: Token哈希值
//   - isSuccess: 请求是否成功
//   - duration: 请求耗时
//   - isStreaming: 是否流式请求
//   - res: 转发结果（成功时用于提取token数量，失败时传nil）
//   - actualModel: 实际模型名称（用于计费）
func (s *Server) updateTokenStatsAsync(tokenHash string, isSuccess bool, duration float64, isStreaming bool, res *fwResult, actualModel string) {
	if tokenHash == "" || s.tokenStatsCh == nil {
		return
	}

	var promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens int64
	var costUSD float64
	var firstByteTime float64

	if res != nil {
		firstByteTime = res.FirstByteTime
	}
	if isSuccess && res != nil {
		promptTokens = int64(res.InputTokens)
		completionTokens = int64(res.OutputTokens)
		cacheReadTokens = int64(res.CacheReadInputTokens)
		cacheCreationTokens = int64(res.CacheCreationInputTokens)
		costUSD = util.CalculateCostDetailed(
			actualModel,
			res.InputTokens,
			res.OutputTokens,
			res.CacheReadInputTokens,
			res.Cache5mInputTokens,
			res.Cache1hInputTokens,
		)

		// 财务安全检查：费用为0但有token消耗时告警（可能是定价缺失）
		if costUSD == 0.0 && (res.InputTokens > 0 || res.OutputTokens > 0) {
			log.Printf("WARN: billing cost=0 for model=%s with tokens (in=%d, out=%d, cache_r=%d, cache_5m=%d, cache_1h=%d), pricing missing?",
				actualModel, res.InputTokens, res.OutputTokens, res.CacheReadInputTokens, res.Cache5mInputTokens, res.Cache1hInputTokens)
		}
	}

	upd := tokenStatsUpdate{
		tokenHash:           tokenHash,
		isSuccess:           isSuccess,
		duration:            duration,
		isStreaming:         isStreaming,
		firstByteTime:       firstByteTime,
		promptTokens:        promptTokens,
		completionTokens:    completionTokens,
		cacheReadTokens:     cacheReadTokens,
		cacheCreationTokens: cacheCreationTokens,
		costUSD:             costUSD,
	}

	// ✅ shutdown期间仍需保证在途请求的计费/用量落库：
	// - 这时 worker 可能正在退出/队列可能不再被消费
	// - 直接同步写入可避免“优雅关闭=静默丢账单”的时序窗口
	if s.isShuttingDown.Load() {
		s.applyTokenStatsUpdate(upd)
		return
	}

	// 优先级策略：成功请求（计费关键）必须记录，失败请求可丢弃
	if isSuccess {
		// 计费数据：带超时的阻塞发送（避免计费数据丢失）
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()

		select {
		case s.tokenStatsCh <- upd:
			// 成功发送
		case <-timer.C:
			// 超时后降级为非阻塞（避免卡住请求）
			select {
			case s.tokenStatsCh <- upd:
			default:
				count := s.tokenStatsDropCount.Add(1)
				log.Printf("[ERROR] 计费统计队列持续饱和，成功请求统计被迫丢弃 (累计: %d)", count)
			}
		}
	} else {
		// 非计费数据：非阻塞发送，队列满时直接丢弃
		select {
		case s.tokenStatsCh <- upd:
		default:
			count := s.tokenStatsDropCount.Add(1)
			if count%100 == 1 {
				log.Printf("[WARN]  Token统计队列已满，失败请求统计被丢弃 (累计: %d)", count)
			}
		}
	}
}

// handleProxySuccess 处理代理成功响应（业务逻辑层）
// 使用 cooldownManager 统一管理冷却状态清除
// 注意：与 handleSuccessResponse（HTTP层）不同
func (s *Server) handleProxySuccess(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
) (*proxyResult, bool, bool) {
	// 使用 cooldownManager 清除冷却状态
	// 设计原则: 清除失败不应影响用户请求成功
	_ = s.cooldownManager.ClearChannelCooldown(ctx, cfg.ID)
	_ = s.cooldownManager.ClearKeyCooldown(ctx, cfg.ID, keyIndex)

	// 冷却状态已恢复，刷新相关缓存避免下次命中过期数据
	s.invalidateChannelRelatedCache(cfg.ID)

	// 记录成功日志
	s.AddLogAsync(buildLogEntry(logEntryParams{
		RequestModel: reqCtx.originalModel,
		ActualModel:  actualModel,
		ChannelID:    cfg.ID,
		StatusCode:   res.Status,
		Duration:     duration,
		IsStreaming:  reqCtx.isStreaming,
		APIKeyUsed:   selectedKey,
		AuthTokenID:  reqCtx.tokenID,
		ClientIP:     reqCtx.clientIP,
		Result:       res,
	}))

	// 异步更新Token统计
	s.updateTokenStatsAsync(reqCtx.tokenHash, true, duration, reqCtx.isStreaming, res, actualModel)

	return &proxyResult{
		status:    res.Status,
		header:    res.Header,
		channelID: &cfg.ID,
		message:   "ok",
		duration:  duration,
		succeeded: true,
	}, false, false
}

// handleStreamingErrorNoRetry 处理流式响应中途检测到的错误（597/599）
// 场景：HTTP 200 已发送，流传输中途检测到 SSE error 或流不完整
// 关键：响应头已发送，重试在 HTTP 协议层面不可能，只触发冷却+记录日志
func (s *Server) handleStreamingErrorNoRetry(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
) (*proxyResult, bool, bool) {
	// 记录错误日志
	s.AddLogAsync(buildLogEntry(logEntryParams{
		RequestModel: reqCtx.originalModel,
		ActualModel:  actualModel,
		ChannelID:    cfg.ID,
		StatusCode:   res.Status,
		Duration:     duration,
		IsStreaming:  reqCtx.isStreaming,
		APIKeyUsed:   selectedKey,
		AuthTokenID:  reqCtx.tokenID,
		ClientIP:     reqCtx.clientIP,
		Result:       res,
		ErrMsg:       res.StreamDiagMsg,
	}))

	// 触发冷却（保护后续请求）
	// 使用独立 context，避免请求取消导致冷却写入失败
	cooldownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = s.handleProxyError(cooldownCtx, cfg, keyIndex, res, nil)

	// 返回"成功"：数据已发送给客户端，不触发重试
	return &proxyResult{
		status:    res.Status,
		channelID: &cfg.ID,
		duration:  duration,
		succeeded: true, // 关键：标记为成功，避免触发重试逻辑
	}, false, false
}

// handleProxyErrorResponse 处理代理错误响应（业务逻辑层）
// 从proxy.go提取，遵循SRP原则
// 注意：与 handleErrorResponse（HTTP层）不同
func (s *Server) handleProxyErrorResponse(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
) (*proxyResult, bool, bool) {
	// 日志改进: 明确标识上游返回的499错误
	errMsg := ""
	if res.Status == 499 {
		errMsg = "upstream returned 499 (not client cancel)"
	}

	s.AddLogAsync(buildLogEntry(logEntryParams{
		RequestModel: reqCtx.originalModel,
		ActualModel:  actualModel,
		ChannelID:    cfg.ID,
		StatusCode:   res.Status,
		Duration:     duration,
		IsStreaming:  reqCtx.isStreaming,
		APIKeyUsed:   selectedKey,
		AuthTokenID:  reqCtx.tokenID,
		ClientIP:     reqCtx.clientIP,
		Result:       res,
		ErrMsg:       errMsg,
	}))

	// 异步更新Token统计（失败请求不计费）
	s.updateTokenStatsAsync(reqCtx.tokenHash, false, duration, reqCtx.isStreaming, res, actualModel)

	action, _ := s.handleProxyError(ctx, cfg, keyIndex, res, nil)
	if action == cooldown.ActionReturnClient {
		return &proxyResult{
			status:    res.Status,
			header:    res.Header,
			body:      res.Body,
			channelID: &cfg.ID,
			duration:  duration,
			succeeded: false,
		}, false, false
	}

	if action == cooldown.ActionRetryChannel {
		return nil, false, true // 切换到下一个渠道
	}

	return nil, true, false // 继续重试下一个Key
}
