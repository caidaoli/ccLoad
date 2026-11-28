package app

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

/**
 * @brief 本轮工具结果所属的仍可用的 Cursor 渠道与模型行。
 */
type cursorToolRoute struct {
	channelID int64                 ///< 原 Run 的渠道 ID。
	model     modelRoutingSelection ///< 与原 Run 实际模型匹配的配置行。
}

/**
 * @brief 提取路由预判和 Agent 恢复共用的身份。
 * @param channelID 候选或实际选中的渠道 ID。
 * @return 网关身份与客户端提交的会话和代理标识。
 */
func (reqCtx *proxyRequestContext) cursorSessionScope(channelID int64) cursorauth.SessionScope {
	scope := cursorauth.SessionScope{
		ChannelID: channelID, TokenID: reqCtx.tokenID,
		SessionID: strings.TrimSpace(reqCtx.header.Get("Session-Id")),
		ThreadID:  strings.TrimSpace(reqCtx.header.Get("Thread-Id")),
	}
	if reqCtx.clientProtocol == protocol.Anthropic {
		scope.ClaudeSessionID = strings.TrimSpace(reqCtx.header.Get("X-Claude-Code-Session-Id"))
		if scope.ClaudeSessionID == "" {
			scope.ClaudeSessionID = anthropicSessionIDFromBody(reqCtx.body)
		}
		if agent := strings.TrimSpace(anthropicHeaderValue(reqCtx.header, "X-Claude-Code-Agent-Id")); agent != "main" {
			scope.ClaudeAgentID = agent
		}
	}
	return scope
}

/**
 * @brief 在现有可用候选中优先尝试已持有本轮工具回调的 Cursor 渠道。
 * @param ctx 当前请求上下文。
 * @param candidates 已完成渠道资格、授权和显式选择过滤的候选。
 * @param reqCtx 当前请求；每次遍历都重新验证工具会话和模型行的可用性。
 * @return 调整顺序后的候选；无可恢复的会话时保持原顺序。
 */
func (s *Server) preferCursorToolRoute(ctx context.Context, candidates []*model.Config, reqCtx *proxyRequestContext) []*model.Config {
	reqCtx.cursorToolRoute = cursorToolRoute{}
	if reqCtx.tokenID <= 0 || reqCtx.requestMethod != http.MethodPost || !cursorSupportsRequestFamily(reqCtx.requestPath) {
		return candidates
	}
	if !slices.ContainsFunc(candidates, func(cfg *model.Config) bool { return cfg.UsesCursorOAuth() && !cfg.CooldownFallback }) {
		return candidates
	}
	runner, ok := s.cursorRunnerSnapshot().(*cursorauth.SDKRunner)
	if !ok || !runner.HasToolContinuation() {
		return candidates
	}
	pinnedChannel, _ := reqCtx.routingSession.routeChannelSnapshot()
	if candidates[0].ID != pinnedChannel {
		pinnedChannel = 0 // 已经不在本轮候选中。
	}
	body := cursorClientRequestBody(reqCtx)
	if client := cursorTranslatedClientProtocol(reqCtx.requestPath); client != "" {
		if client == protocol.Codex && (responsesCompactionRequested(reqCtx.requestPath, reqCtx.body) ||
			strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "") {
			return candidates
		}
		translated, err := s.protocolRegistry.TranslateRequest(client, protocol.OpenAI, reqCtx.originalModel, body, reqCtx.isStreaming)
		if err != nil {
			return candidates
		}
		body = translated
	}
	results := cursorauth.ParseRequestToolResults(body)
	if len(results) == 0 {
		return candidates
	}
	for _, cfg := range candidates {
		if !cfg.UsesCursorOAuth() || cfg.CooldownFallback || (pinnedChannel != 0 && cfg.ID != pinnedChannel) {
			continue
		}
		credential, err := s.cursorCredentials.credential(ctx, cfg, false)
		if err != nil {
			continue
		}
		scope := reqCtx.cursorSessionScope(cfg.ID)
		if upstreamModel := runner.ToolContinuationModel(scope, credential, results); upstreamModel != "" {
			row, err := s.selectChannelModelRow(ctx, cfg, reqCtx.originalModel, string(reqCtx.clientProtocol), nil, nil, upstreamModel)
			if err == nil {
				reqCtx.cursorToolRoute = cursorToolRoute{channelID: cfg.ID, model: row}
				return prioritizePinnedChannel(candidates, cfg.ID)
			}
		}
	}
	return candidates
}

/**
 * @brief 选择 Cursor 协议转换和工具路由所用的客户端正文。
 * @param reqCtx 当前请求及执行会话恢复的正文。
 * @return 已恢复的完整历史；无本地历史时为客户端原文。
 */
func cursorClientRequestBody(reqCtx *proxyRequestContext) []byte {
	if len(reqCtx.responsesReplayBody) > 0 {
		return reqCtx.responsesReplayBody
	}
	return reqCtx.body
}
