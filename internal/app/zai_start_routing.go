package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/zaiauth"
)

/**
 * @brief 尚未发送的合法 Trial 候选；唤醒只能复核此集合，不重放真实失败。
 */
type zaiStartPending struct {
	cfg      *model.Config         ///< 原请求候选快照。
	identity [32]byte              ///< 排队时的真实账号，配置换账号后不得复用。
	selected modelRoutingSelection ///< 排队前已选定的模型行。
}

/**
 * @brief 普通选路只为 ZAI Trial 增加有界等待，其他渠道仍使用单次遍历。
 * @param ctx 请求上下文，取消优先终止等待。
 * @param cands 已通过本请求授权与模型筛选的候选。
 * @param reqCtx 请求状态。
 * @param w 客户端响应写入器。
 * @param stopAfterFailure 调用方失败边界，可为 nil。
 * @return 最后实际失败或本地终态，无失败为 nil。
 * @return 是否已成功写入响应。
 * @note 最多等待六十秒，唤醒不重置期限；不持锁等待。
 */
func (s *Server) runProxyAttemptLoopWithFailureBoundary(ctx context.Context, cands []*model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter, stopAfterFailure func(current, next *model.Config, result *proxyResult) bool) (*proxyResult, bool) {
	hasZAI := false
	for _, cfg := range cands {
		hasZAI = hasZAI || cfg.UsesZAIOAuth()
	}
	if !hasZAI {
		return s.runProxyAttemptPass(ctx, cands, reqCtx, w, stopAfterFailure)
	}
	logical := &zaiStartRequest{routing: true}
	reqCtx.zaiStart = logical
	// 失败边界只由公共循环判断一次，ZAI 仅补充等待状态的收尾。
	if stopAfterFailure != nil {
		original := stopAfterFailure
		stopAfterFailure = func(current, next *model.Config, result *proxyResult) bool {
			stop := original(current, next, result)
			if stop {
				logical.pending = nil
			}
			return stop
		}
	}
	defer s.zaiStart.finish(logical)
	var deadline *time.Timer
	var queueStarted time.Time
	defer func() {
		if deadline != nil {
			deadline.Stop()
		}
	}()
	// 配置更新不一定触发账号释放，周期复核停用/资格变化；不产生上游请求。
	refresh := time.NewTicker(time.Second)
	defer refresh.Stop()
	var last *proxyResult
	for {
		changed := s.zaiStart.notification()
		logical.pending = nil
		result, succeeded := s.runProxyAttemptPass(ctx, cands, reqCtx, w, stopAfterFailure)
		if succeeded || (result != nil && shouldStopTryingChannels(result)) {
			return result, succeeded
		}
		if result != nil && (last == nil || result.zaiLocalPlanKind == "" || last.zaiLocalPlanKind != "") {
			last = result
		}
		if len(logical.pending) == 0 {
			return last, false
		}
		if deadline == nil {
			deadline = time.NewTimer(zaiStartQueueWait)
			queueStarted = time.Now()
		}
		waiting := logical.pending[0]
		waitCtx, cancelWait := context.WithCancelCause(ctx)
		// 原发送尝试的取消句柄已结束；等待绑定当前候选，不能继续展示最后的备用/失败渠道。
		if s.activeRequests != nil {
			reqCtx.activeReqID = s.activeRequests.BeginAttempt(reqCtx.activeReqID, activeRequestAttempt{
				StartTime: queueStarted, Model: reqCtx.originalModel, ClientIP: reqCtx.clientIP,
				Streaming: reqCtx.isStreaming, ChannelID: waiting.cfg.ID, ChannelName: waiting.cfg.Name,
				ClientProtocol: string(reqCtx.clientProtocol), TokenID: reqCtx.tokenID, Abort: cancelWait,
			})
		}
		timedOut := false
		select {
		case <-waitCtx.Done():
		case <-deadline.C:
			timedOut = true
		case <-changed:
		case <-refresh.C:
		}
		// 封闭本轮句柄后再读取原因；迟到的管理操作不能取消下一轮或父请求。
		cancelWait(nil)
		if err := ctx.Err(); err != nil {
			return buildCtxDoneResult(waiting.cfg, err), false
		}
		if errors.Is(context.Cause(waitCtx), errOperatorAbort) {
			aborted := zaiStartQueueFailure(http.StatusBadGateway, "zcode_trial_operator_abort", "管理员已跳过当前排队渠道")
			aborted.operatorAborted, aborted.nextAction = true, cooldown.ActionRetryChannel
			if last == nil || last.zaiLocalPlanKind != "" {
				last = aborted
			}
			remaining := logical.pending[:0]
			for _, pending := range logical.pending {
				if pending.cfg.ID != waiting.cfg.ID {
					remaining = append(remaining, pending)
				}
			}
			logical.pending = remaining
			if len(remaining) == 0 || (stopAfterFailure != nil && stopAfterFailure(waiting.cfg, remaining[0].cfg, aborted)) {
				return last, false
			}
		}
		// 中断与到期同时发生时也必须结束等待，不能消费一次性定时器后继续等。
		if timedOut || time.Since(queueStarted) >= zaiStartQueueWait {
			// 等待结束没有新的上游结果，不能把此前真实失败改写为本地限流。
			if last != nil && last.zaiLocalPlanKind == "" {
				return last, false
			}
			return zaiStartQueueFailure(http.StatusTooManyRequests, "zcode_trial_queue_timeout", "ZCode 体验等待空闲账号超时，请稍后重试"), false
		}
		cands = s.refreshZAIStartPending(ctx, reqCtx)
		if err := ctx.Err(); err != nil {
			return buildCtxDoneResult(waiting.cfg, err), false
		}
		if len(cands) == 0 {
			if last != nil && last.zaiLocalPlanKind == "" {
				return last, false
			}
			return zaiStartQueueFailure(http.StatusTooManyRequests, "zcode_trial_unavailable", "ZCode 排队候选已不可用，请重新请求"), false
		}
	}
}

/**
 * @brief 构造没有上游调用的排队终态，最终日志须保留本地来源。
 * @param status 客户端 HTTP 状态码。
 * @param kind 本地错误类别。
 * @param message 客户端错误描述。
 * @return 可直接返回客户端的本地错误结果，429 附带重试间隔。
 */
func zaiStartQueueFailure(status int, kind, message string) *proxyResult {
	resp := zaiPlanError((&http.Request{}).WithContext(context.Background()), status, kind, message)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if status == http.StatusTooManyRequests {
		resp.Header.Set("Retry-After", "1")
	}
	return &proxyResult{status: resp.StatusCode, header: resp.Header, body: body, zaiLocalPlanKind: kind, nextAction: cooldown.ActionReturnClient}
}

/**
 * @brief 重新核对未发送候选，固定账号和完整模型行，禁止唤醒时改变请求目标。
 * @param ctx 请求上下文。
 * @param reqCtx 请求状态，含本轮等待候选。
 * @return 仍合法且未改变模型配置的候选；空表示等待失效。冷却读取失败时原样返回当前候选。
 * @note 刷新等待账号集合，移除失效候选，避免阻挡后续请求；冷却读取失败时不修改等待状态。
 */
func (s *Server) refreshZAIStartPending(ctx context.Context, reqCtx *proxyRequestContext) []*model.Config {
	logical := reqCtx.zaiStart
	modelCooldowns, modelErr := s.getAllModelCooldowns(ctx)
	channelCooldowns, channelErr := s.getAllChannelCooldowns(ctx)
	if modelErr != nil || channelErr != nil {
		// 冷却读取的瞬时故障不代表候选失效；保留原候选与固定模型，下次复核或排队超时再决定。
		cands := make([]*model.Config, 0, len(logical.pending))
		for _, pending := range logical.pending {
			cands = append(cands, pending.cfg)
		}
		return cands
	}
	logical.models = make(map[int64]modelRoutingSelection)
	var cands []*model.Config
	waiting := make(map[[32]byte]bool)
	now := time.Now()
	for _, pending := range logical.pending {
		cfg, err := s.store.GetConfig(ctx, pending.cfg.ID)
		if err != nil || !cfg.Enabled || !cfg.UsesZAIOAuth() || !cfg.IsAvailableAt(now) || channelCooldowns[cfg.ID].After(now) {
			continue
		}
		// 二级重定向和请求规则也影响最终模型；变化时让客户端重新选路。
		if !reflect.DeepEqual(cfg.ModelEntries, pending.cfg.ModelEntries) || !reflect.DeepEqual(cfg.CustomRequestRules, pending.cfg.CustomRequestRules) {
			continue
		}
		credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || zaiStartIdentity(credential) != pending.identity {
			continue
		}
		for _, selected := range s.enumerateModelRows(cfg, reqCtx.originalModel) {
			if !reflect.DeepEqual(selected, pending.selected) || !s.modelRowApplicable(cfg, selected, now) {
				continue
			}
			ready, authorized := s.modelRowWithKeysReadyAt(cfg, selected, string(reqCtx.clientProtocol), modelCooldowns[cfg.ID], nil, now)
			if !authorized || ready.After(now) {
				continue
			}
			logical.models[cfg.ID] = selected
			cands = append(cands, cfg)
			waiting[pending.identity] = true
			break
		}
	}
	s.zaiStart.mu.Lock()
	if !reflect.DeepEqual(logical.waiting, waiting) {
		logical.waiting = waiting
		s.zaiStart.notifyLocked()
	}
	s.zaiStart.mu.Unlock()
	return cands
}

/**
 * @brief 获取本地等待前选定的模型，不影响普通模型轮转。
 * @param reqCtx 请求状态。
 * @param channelID 当前渠道。
 * @return 固定模型，未排队为零值。
 * @return 是否存在固定模型。
 */
func zaiStartPinnedModel(reqCtx *proxyRequestContext, channelID int64) (modelRoutingSelection, bool) {
	if reqCtx.zaiStart == nil {
		return modelRoutingSelection{}, false
	}
	selected, ok := reqCtx.zaiStart.models[channelID]
	return selected, ok
}

/**
 * @brief 将待选账号和发送计数限定在 ZAI 调度边界，公共渠道尝试保持原调用。
 * @param ctx 当前请求上下文，取消时终止渠道尝试。
 * @param cfg 当前候选渠道快照。
 * @param reqCtx 请求及 Trial 等待状态，本次尝试会更新候选和发送计数。
 * @param w 客户端响应写入器。
 * @return 渠道尝试结果；未产生结果时为 nil。
 * @return 渠道尝试错误；正常完成时为 nil。
 * @note 切到非 ZAI 渠道时释放 Trial 名额，取消此前等待候选。
 */
func (s *Server) tryChannelWithZAIStartRouting(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	logical := reqCtx.zaiStart
	if logical == nil {
		return s.tryChannelWithSameChannelRetries(ctx, cfg, reqCtx, w)
	}
	if !cfg.UsesZAIOAuth() {
		s.zaiStart.finish(logical)
		logical.pending = nil
	}
	sends, departures := logical.sends.Load(), logical.departures.Load()
	result, err := s.tryChannelWithSameChannelRetries(ctx, cfg, reqCtx, w)
	if logical.departures.Load() != departures {
		logical.pending = nil
	}
	if result != nil {
		if logical.sends.Load() == sends && result.zaiLocalPlanKind == "zcode_trial_wait" && !shouldStopTryingChannels(result) {
			credential, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
			if parseErr == nil {
				logical.pending = append(logical.pending, zaiStartPending{cfg: cfg, identity: zaiStartIdentity(credential), selected: reqCtx.attemptModel})
			}
		}
		if result.operatorAborted {
			logical.pending = nil
		}
	}
	return result, err
}

/**
 * @brief ZAI 本地非终态不能覆盖真实失败或容量拒绝，终态保持公共优先级。
 * @param previous 上一候选结果，允许 nil。
 * @param current 本次候选结果，必须非 nil。
 * @return 应保留的结果；本地等待或备用跳过不覆盖已有真实失败及容量拒绝。
 */
func selectZAIStartResult(previous, current *proxyResult) *proxyResult {
	if current.zaiLocalPlanKind != "" && !shouldStopTryingChannels(current) && previous != nil && (previous.zaiLocalPlanKind == "" || previous.zaiLocalPlanKind == "zcode_trial_capacity") {
		return previous
	}
	return current
}

/**
 * @brief 仅保存 ZAI 已实际失败的尝试，避免后续本地繁忙重置重试预算。
 */
type zaiRetryFailure struct {
	result  *proxyResult          ///< 最近真实失败。
	inputs  []cooldown.ErrorInput ///< 对应延迟冷却输入。
	attempt int                   ///< 对应尝试序号。
}

/**
 * @brief 在公共重试判断前恢复真实失败；非 ZAI 不修改任何状态。
 * @param cfg 当前渠道配置。
 * @param result 本次结果指针，本地非终态可能被最近真实失败替换。
 * @param pending 待应用的同渠道冷却输入，会随结果一起恢复。
 * @param attempt 当前尝试序号，会随结果一起恢复。
 * @return 本次是否恢复了最近真实失败；非 ZAI 或无结果时为 false。
 * @note 更新当前请求内的重试状态，不可跨请求共享。
 */
func (f *zaiRetryFailure) reconcile(cfg *model.Config, result **proxyResult, pending *pendingSameChannelCooldowns, attempt *int) bool {
	if !cfg.UsesZAIOAuth() || *result == nil {
		return false
	}
	local := (*result).zaiLocalPlanKind != "" && f.result != nil
	if local {
		*result, pending.inputs, *attempt = f.result, f.inputs, f.attempt
	}
	if !(*result).succeeded && (*result).zaiLocalPlanKind == "" {
		f.result, f.inputs, f.attempt = *result, pending.inputs, *attempt
	}
	return local
}
