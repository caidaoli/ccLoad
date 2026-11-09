package app

import (
	"context"
	"errors"
	"net/http"

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
 * @note 原地修改 result；真实最终故障退役活动账号，只有额度错误写 Trial 模型冷却并失效缓存。
 */
func (s *Server) applyZAIChannelTestCooldown(ctx context.Context, cfg *model.Config, keyIndex int, testReq *testutil.TestChannelRequest, persist bool, result map[string]any, actualModel string) map[string]any {
	if success, _ := result["success"].(bool); success {
		if persist {
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
	s.applyZAITrialCooldown(ctx, cfg, input)
	if input.IsNetworkError || !zaiGiftQuotaError(status, body) {
		result["cooldown_action"] = "trial_error_no_cooldown"
		return result
	}
	result["cooldown_action"] = "model_cooldown_applied"
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
		release, err := s.waitForUpstreamRequest(reqCtx, cfg)
		if err != nil {
			return nil, err
		}
		// 本地排队不计入协议超时；每次真实发送才启动计时并保留到响应处理结束。
		sendCtx, timeout := s.newChannelTestTimeoutContextWithTimeouts(reqCtx, testReq.Stream, s.resolveProtocolTimeouts(protocol.TransformPlan{
			UpstreamProtocol: protocol.Protocol(requestPlan.upstreamProtocol),
		}))
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
 * @param resp 最终上游响应。
 * @param testReq 检测请求，非 nil 时记录实际计费模型。
 * @param sendErr 最终发送错误；Trial 传输错误携带无响应时的实际模型。
 */
func (s *Server) syncZAIChannelTestRequest(cfg *model.Config, plan *channelTestRequestPlan, resp *http.Response, testReq *testutil.TestChannelRequest, sendErr error) {
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
	if resp == nil || resp.Request == nil {
		return
	}
	body, err := zaiRequestBody(resp.Request)
	if err != nil {
		return
	}
	plan.requestBody = body
	if testReq != nil {
		testReq.ZAI = testutil.ZAITestOutcome{
			WireModel:      gjson.GetBytes(body, "model").String(),
			Trial:          isZAITrialResponse(resp),
			LocalPlanError: isZAILocalPlanResponse(resp),
		}
	}
	plan.fullURL = resp.Request.URL.String()
	plan.upstreamHeaders = resp.Request.Header.Clone()
	plan.debugCapture = s.captureDebugRequest(resp.Request, body)
	if plan.clientProtocol != plan.upstreamProtocol {
		plan.debugCapture.markProtocolTransform(extractRequestPath(plan.clientURL), plan.clientHeaders, plan.clientBody)
	}
}
