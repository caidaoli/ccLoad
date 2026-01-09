package app

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// ============================================================================
// 错误处理核心函数
// ============================================================================

const cooldownWriteTimeout = 3 * time.Second

var cooldownClearChannelFailCount atomic.Uint64
var cooldownClearKeyFailCount atomic.Uint64

func cooldownWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	// 断开请求取消链，但保留 ctx.Value（例如 trace ID）。
	// 避免客户端取消/首字节超时导致冷却写入或清理被短路，从而出现“坏 Key/渠道反复被打爆”或“冷却未清除”的假象。
	return context.WithTimeout(context.WithoutCancel(ctx), cooldownWriteTimeout)
}

func (s *Server) applyCooldownDecision(
	ctx context.Context,
	cfg *model.Config,
	in cooldown.ErrorInput,
) cooldown.Action {
	cooldownCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()

	// 设置渠道类型，用于特定渠道的错误处理策略
	in.ChannelType = cfg.ChannelType

	action := s.cooldownManager.HandleError(cooldownCtx, in)

	if action == cooldown.ActionRetryKey || action == cooldown.ActionRetryChannel {
		s.invalidateChannelRelatedCache(cfg.ID)
	}

	return action
}

func httpErrorInput(channelID int64, keyIndex int, res *fwResult) cooldown.ErrorInput {
	if res == nil {
		return httpErrorInputFromParts(channelID, keyIndex, 0, nil, nil)
	}
	return httpErrorInputFromParts(channelID, keyIndex, res.Status, res.Body, res.Header)
}

func httpErrorInputFromParts(
	channelID int64,
	keyIndex int,
	statusCode int,
	body []byte,
	headers map[string][]string,
) cooldown.ErrorInput {
	return cooldown.ErrorInput{
		ChannelID:      channelID,
		KeyIndex:       keyIndex,
		StatusCode:     statusCode,
		ErrorBody:      body,
		IsNetworkError: false,
		Headers:        headers,
	}
}

func networkErrorInput(channelID int64, keyIndex int, statusCode int) cooldown.ErrorInput {
	return cooldown.ErrorInput{
		ChannelID:      channelID,
		KeyIndex:       keyIndex,
		StatusCode:     statusCode,
		ErrorBody:      nil,
		IsNetworkError: true,
		Headers:        nil,
	}
}

func (s *Server) logProxyResult(
	reqCtx *proxyRequestContext,
	cfg *model.Config,
	actualModel string,
	selectedKey string,
	statusCode int,
	duration float64,
	res *fwResult,
	errMsg string,
) {
	s.AddLogAsync(buildLogEntry(logEntryParams{
		RequestModel: reqCtx.originalModel,
		ActualModel:  actualModel,
		ChannelID:    cfg.ID,
		StatusCode:   statusCode,
		Duration:     duration,
		IsStreaming:  reqCtx.isStreaming,
		APIKeyUsed:   selectedKey,
		AuthTokenID:  reqCtx.tokenID,
		ClientIP:     reqCtx.clientIP,
		Result:       res,
		ErrMsg:       errMsg,
		StartTime:    reqCtx.attemptStartTime,
	}))
}

func (s *Server) updateTokenStatsForProxy(
	reqCtx *proxyRequestContext,
	isSuccess bool,
	duration float64,
	res *fwResult,
	actualModel string,
) {
	s.updateTokenStatsAsync(reqCtx.tokenHash, isSuccess, duration, reqCtx.isStreaming, res, actualModel)
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
	_ int64, // authTokenID: API令牌ID（用于日志记录，2025-12新增，当前未使用）
	_ string, // clientIP: 客户端IP（用于日志记录，2025-12新增，当前未使用）
	duration float64,
	err error,
	res *fwResult, // [FIX] 流式响应中途取消时，res 包含已解析的 token 统计
	reqCtx *proxyRequestContext, // [FIX] 用于获取 tokenHash 和 isStreaming
) (*proxyResult, cooldown.Action) {
	statusCode, _, shouldRetry := util.ClassifyError(err)

	// 记录日志：requestModel=原始请求模型，actualModel=实际转发模型
	s.logProxyResult(reqCtx, cfg, actualModel, selectedKey, statusCode, duration, res, err.Error())

	failure := &proxyResult{
		status:           statusCode,
		body:             []byte(err.Error()),
		channelID:        &cfg.ID,
		duration:         duration,
		succeeded:        false,
		isClientCanceled: errors.Is(err, context.Canceled),
	}

	// [FIX] 2025-12: 保留 499 场景下已消耗的 token 统计
	// 场景：流式响应中途取消（用户点"停止"），上游已消耗 token 但之前被丢弃
	// 修复：即使请求失败，也记录已解析的 token 统计（用于计费和统计）
	if res != nil && hasConsumedTokens(res) {
		// isSuccess=false 表示请求失败，但仍记录已消耗的 token
		s.updateTokenStatsForProxy(reqCtx, false, duration, res, actualModel)
	}

	if !shouldRetry {
		failure.nextAction = cooldown.ActionReturnClient
		return failure, cooldown.ActionReturnClient
	}

	action := s.applyCooldownDecision(ctx, cfg, networkErrorInput(cfg.ID, keyIndex, statusCode))
	failure.nextAction = action
	return failure, action
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
		return // 数据库更新失败，不更新内存缓存，保持一致性
	}

	// 数据库更新成功后，同步更新费用缓存（用于限额检查，2026-01新增）
	if upd.isSuccess && upd.costUSD > 0 {
		s.authService.AddCostToCache(upd.tokenHash, util.USDToMicroUSD(upd.costUSD))
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
		// 注意：费用缓存更新已移至 applyTokenStatsUpdate，确保数据库先写成功
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
) (*proxyResult, cooldown.Action) {
	cooldownCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()

	// 使用 cooldownManager 清除冷却状态
	// 设计原则: 清除失败不应影响用户请求成功
	if err := s.cooldownManager.ClearChannelCooldown(cooldownCtx, cfg.ID); err != nil {
		count := cooldownClearChannelFailCount.Add(1)
		if count%100 == 1 {
			log.Printf("[WARN] ClearChannelCooldown 失败 (累计: %d): channel_id=%d err=%v", count, cfg.ID, err)
		}
	}
	if err := s.cooldownManager.ClearKeyCooldown(cooldownCtx, cfg.ID, keyIndex); err != nil {
		count := cooldownClearKeyFailCount.Add(1)
		if count%100 == 1 {
			log.Printf("[WARN] ClearKeyCooldown 失败 (累计: %d): channel_id=%d key_index=%d err=%v", count, cfg.ID, keyIndex, err)
		}
	}

	// 冷却状态已恢复，刷新相关缓存避免下次命中过期数据
	s.invalidateChannelRelatedCache(cfg.ID)

	// 记录成功日志
	s.logProxyResult(reqCtx, cfg, actualModel, selectedKey, res.Status, duration, res, "")

	// 异步更新Token统计
	s.updateTokenStatsForProxy(reqCtx, true, duration, res, actualModel)

	return &proxyResult{
		status:     res.Status,
		header:     res.Header,
		channelID:  &cfg.ID,
		duration:   duration,
		succeeded:  true,
		nextAction: cooldown.ActionReturnClient,
	}, cooldown.ActionReturnClient
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
) (*proxyResult, cooldown.Action) {
	// 记录错误日志
	s.logProxyResult(reqCtx, cfg, actualModel, selectedKey, res.Status, duration, res, res.StreamDiagMsg)

	// 触发冷却（保护后续请求）
	_ = s.applyCooldownDecision(ctx, cfg, httpErrorInput(cfg.ID, keyIndex, res))

	// 返回"成功"：数据已发送给客户端，不触发重试
	return &proxyResult{
		status:     res.Status,
		channelID:  &cfg.ID,
		duration:   duration,
		succeeded:  true, // 关键：标记为成功，避免触发重试逻辑
		nextAction: cooldown.ActionReturnClient,
	}, cooldown.ActionReturnClient
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
) (*proxyResult, cooldown.Action) {
	// 日志改进: 明确标识上游返回的499错误
	errMsg := ""
	if res.Status == 499 {
		errMsg = "upstream returned 499 (not client cancel)"
	}

	s.logProxyResult(reqCtx, cfg, actualModel, selectedKey, res.Status, duration, res, errMsg)

	// 异步更新Token统计（失败请求不计费）
	s.updateTokenStatsForProxy(reqCtx, false, duration, res, actualModel)

	failure := &proxyResult{
		status:    res.Status,
		header:    res.Header,
		body:      res.Body,
		channelID: &cfg.ID,
		duration:  duration,
		succeeded: false,
	}

	action := s.applyCooldownDecision(ctx, cfg, httpErrorInput(cfg.ID, keyIndex, res))
	failure.nextAction = action
	return failure, action
}
