package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"

	"github.com/tidwall/gjson"
)

/**
 * @brief ZAI 管理检测的冷却政策不进入公共渠道/Key 清理逻辑。
 * @param ctx 冷却写入上下文。
 * @param cfg 实际检测的渠道快照。
 * @param keyIndex 本次检测的持久化 Key 序号。
 * @param testReq 检测参数及实际套餐标记。
 * @param persist 是否允许写入持久化冷却。
 * @param result 待标注的检测结果映射，必须非 nil。
 * @param actualModel 实际请求模型，用于选择对应 Trial 冷却键。
 * @return 原结果映射，补充 cooldown_action。
 * @note 原地修改 result；只有真实体验失败登记体验范围冷却，付费与其他渠道状态独立。
 */
func (s *Server) applyZAIChannelTestCooldown(ctx context.Context, cfg *model.Config, keyIndex int, testReq *testutil.TestChannelRequest, persist bool, result map[string]any, actualModel string) map[string]any {
	if success, _ := result["success"].(bool); success {
		if persist {
			cooldownCtx, cancel := cooldownWriteContext(ctx)
			defer cancel()
			s.recoverZAITrialCooldown(cooldownCtx, cfg, actualModel, testReq.ZAI.Started)
			s.invalidateChannelRelatedCache(cfg.ID)
		}
		return result
	}
	if limited, _ := result["rpm_limited"].(bool); limited {
		result["cooldown_action"] = "rpm_limited_no_cooldown"
		return result
	}
	if limited, _ := result["concurrency_limited"].(bool); limited {
		result["cooldown_action"] = "concurrency_limited_no_cooldown"
		return result
	}
	if existing, _ := result["cooldown_action"].(string); existing != "" {
		return result
	}
	if !persist {
		result["cooldown_action"] = "request_key_no_cooldown"
		return result
	}
	if testReq.ZAI.LocalPlanError {
		result["cooldown_action"] = "local_plan_no_cooldown"
		return result
	}
	status, body, headers := buildTestFailureClassificationInput(result)
	input := cooldownInputForModel(httpErrorInputFromParts(cfg.ID, keyIndex, status, body, headers), zaiTrialModelKey(actualModel))
	input.IsNetworkError, _ = result["network_error"].(bool)
	decision := s.evaluateZAITrialCooldown(cfg, input)
	_, accepted := s.applyZAITrialCooldown(ctx, cfg, input)
	if !accepted {
		result["cooldown_action"] = "trial_error_no_cooldown"
	} else {
		result["cooldown_action"] = "trial_" + decision.scope + "_cooldown"
	}
	return result
}

/**
 * @brief ZAI 管理检测在真实发送时开始协议超时，排队不占用发送预算。
 * @param reqCtx 检测上下文，取消时中止容量等待及实际发送。
 * @param cfg 本次检测的渠道配置与凭证快照。
 * @param testReq 检测参数，用于选择流式或非流式超时。
 * @param requestPlan 检测计划，timeout 会更新为最近一次实际发送的超时状态。
 * @param req 已构造的上游请求，包含套餐选择所需的上下文标记。
 * @param cancel 已有清理函数，必须非 nil；返回函数会包含该清理责任。
 * @return 最终上游或本地套餐拒绝响应；失败时可能为 nil，非 nil 响应体须关闭。
 * @return 完整清理函数；无论成功或失败，调用方均须在响应处理结束后执行。
 * @return 套餐准备、容量等待、发送或取消错误；成功时为 nil。
 * @note 多次实际发送会累积超时清理函数；返回的清理函数不能代替关闭响应体。
 */
func (s *Server) sendZAIChannelTest(reqCtx context.Context, cfg *model.Config, testReq *testutil.TestChannelRequest, requestPlan *channelTestRequestPlan, req *http.Request, cancel context.CancelFunc) (*http.Response, context.CancelFunc, error) {
	resp, err := s.doUpstreamRequestWithSender(cfg, req, func(outbound *http.Request) (*http.Response, error) {
		if attempt, _ := outbound.Context().Value(zaiSendAttemptKey{}).(*zaiSendAttempt); attempt != nil {
			attempt.waitsForCapacity = true
		}
		release, err := s.waitForUpstreamRequest(reqCtx, cfg)
		if err != nil {
			return nil, err
		}
		// 本地排队不计入协议超时；每次真实发送才启动计时并保留到响应处理结束。
		sendCtx, timeout := s.newChannelTestTimeoutContextWithTimeouts(reqCtx, testReq.Stream, s.resolveProtocolTimeouts(protocol.TransformPlan{
			UpstreamProtocol: protocol.Protocol(requestPlan.upstreamProtocol),
		}))
		// 发送超时从管理上下文派生，显式保留体验发送时间供成功时条件恢复。
		if started, ok := outbound.Context().Value(zaiTrialStartedKey{}).(time.Time); ok {
			if state := zaiState(outbound); state != nil {
				if state.started.IsZero() {
					state.started = time.Now()
				}
				started = state.started
			}
			sendCtx = context.WithValue(sendCtx, zaiTrialStartedKey{}, started)
		}
		// 只携带下游仍需消费的私有记录，保留本次新建的实际发送超时。
		if state := zaiState(outbound); state != nil {
			sendCtx = context.WithValue(sendCtx, zaiRequestStateKey{}, state)
		}
		if attempt, _ := outbound.Context().Value(zaiSendAttemptKey{}).(*zaiSendAttempt); attempt != nil {
			sendCtx = context.WithValue(sendCtx, zaiSendAttemptKey{}, attempt)
			attempt.req, attempt.timeout = outbound.WithContext(sendCtx), timeout
		}
		requestPlan.timeout = timeout
		previousCancel := cancel
		cancel = func() {
			previousCancel()
			timeout.cancelAll()
		}
		return s.doReservedUpstreamRequest(cfg, outbound.WithContext(sendCtx), release, nil)
	})
	return resp, cancel, err
}

/**
 * @brief 管理检测的调试信息记录最终套餐请求，避免把付费候选误报为实际请求。
 * @param cfg 当前渠道。
 * @param plan 管理检测计划。
 * @param req 原请求，携带无响应时的实际尝试记录。
 * @param resp 最终上游响应。
 * @param testReq 检测请求，非 nil 时记录实际计费模型。
 * @param sendErr 最终发送错误；Trial 传输错误携带无响应时的实际模型。
 * @note 将实际请求头同步到原请求，使管理结果与调试捕获使用同一份出站信息。
 */
func (s *Server) syncZAIChannelTestRequest(cfg *model.Config, plan *channelTestRequestPlan, req *http.Request, resp *http.Response, testReq *testutil.TestChannelRequest, sendErr error) {
	if !cfg.UsesZAIOAuth() {
		return
	}
	if sendErr != nil && testReq != nil {
		// 先同步响应，再以真实传输错误覆盖 Trial 标记及无响应时的模型归因。
		defer func() {
			var trialErr *zaiTrialRequestError
			testReq.ZAI.Trial = errors.As(sendErr, &trialErr)
			if testReq.ZAI.Trial {
				testReq.ZAI.WireModel = trialErr.model
			}
		}()
	}
	actual := req
	if state := zaiState(req); state != nil && state.last != nil {
		actual = state.last.req
		if state.last.timeout != nil {
			plan.timeout = state.last.timeout
		}
	} else if resp != nil && resp.Request != nil {
		actual = resp.Request
	} else {
		return
	}
	body, err := zaiRequestBody(actual)
	if err != nil {
		return
	}
	plan.requestBody = body
	if testReq != nil {
		started, _ := actual.Context().Value(zaiTrialStartedKey{}).(time.Time)
		testReq.ZAI = testutil.ZAITestOutcome{
			Started:        started,
			WireModel:      gjson.GetBytes(body, "model").String(),
			Trial:          isZAITrialResponse(resp),
			LocalPlanError: isZAILocalPlanResponse(resp),
		}
	}
	plan.fullURL = actual.URL.String()
	plan.upstreamHeaders = actual.Header.Clone()
	req.Header = actual.Header.Clone()
	plan.debugCapture = s.captureDebugRequest(actual, body)
	if plan.clientProtocol != plan.upstreamProtocol {
		plan.debugCapture.markProtocolTransform(extractRequestPath(plan.clientURL), plan.clientHeaders, plan.clientBody)
	}
}
