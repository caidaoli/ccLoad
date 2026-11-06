package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"ccLoad/internal/loomyauth"
	"ccLoad/internal/model"
)

/**
 * @brief 为 Loomy 重建出站请求头，隔离其他提供商的认证材料。
 * @param req 要修改的非 nil 出站请求。
 * @param cfg 提供自定义头规则的渠道配置。
 * @param session 当前 Loomy 会话。
 * @note 丢弃原请求头，先应用渠道规则，再以官方会话头覆盖认证相关字段。
 */
func injectLoomyHeaders(req *http.Request, cfg *model.Config, session string) {
	// Rebuild to avoid forwarding another provider's authentication headers.
	req.Header = make(http.Header)
	applyHeaderRules(req.Header, cfg.HeaderRules())
	loomyauth.ApplyHeaders(req.Header, session)
}

/**
 * @brief 通过共用 OAuth 转发流程执行 Loomy 聊天请求。
 * @param ctx 转发取消与截止时间上下文。
 * @param cfg 含私有 Loomy 凭证的渠道配置。
 * @param reqCtx 已经完成解析的下游代理请求上下文。
 * @param w 下游响应写入器。
 * @return 转发结果，包含成功响应或失败的故障切换决策；没有可用尝试时可为 nil。
 * @return 共用转发流程的错误；所有 Key 耗尽时为 ErrAllKeysExhausted，有可返回的上游结果时为 nil。
 * @note 可能向上游发送请求并提交下游响应；Loomy 没有独立续期路径。
 */
func (s *Server) tryLoomyChannel(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	return s.tryOAuthChannel(ctx, cfg, reqCtx, w, "Loomy", true,
		func(_ bool, _ string) (*model.Config, string, error) {
			credential, err := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil {
				return cfg, "", err
			}
			return cfg, credential.Session, nil
		}, func(result *proxyResult) bool {
			return result != nil && !result.succeeded && result.status == http.StatusUnauthorized
		})
}

/**
 * @brief 仅在确定会话失效且凭证仍匹配时禁用 Loomy 渠道。
 * @param ctx 持久化操作上下文。
 * @param cfg 失败请求所使用的渠道与凭证快照。
 * @param err 控制或聊天接口返回的错误。
 * @return 仅本次 CAS 实际禁用渠道时返回 true；已禁用、凭证变化、非会话失效或持久化失败时返回 false。
 * @note 可能条件禁用渠道；比较实际会话可避免旧请求误禁用重新登录后的新凭证。
 */
func (s *Server) handleLoomyRejection(ctx context.Context, cfg *model.Config, err error) bool {
	if cfg == nil || cfg.ID <= 0 || !errors.Is(err, loomyauth.ErrSessionExpired) {
		return false
	}
	_, disabled := s.applyTerminalOAuthRejection(ctx, cfg, "Loomy", &codexCredentialRefreshError{
		cause: loomyauth.ErrSessionExpired, authType: model.AuthTypeLoomySession, credential: cfg.OAuthCredential,
	})
	return disabled
}

/**
 * @brief 将非流式响应中的 Loomy 业务拒绝归一为可供代理分类的 HTTP 状态。
 * @param resp 待检查的上游响应；nil、无响应体及 SSE 直接跳过。
 * @return 读取失败时返回错误并关闭响应体；其他情况返回 nil，包括 JSON 无效或超过 2 MiB 而不分类的响应。
 * @note 修改响应状态及 Body 包装；不缓冲 SSE，不重放已经提交的流。
 * @details 最多读取 2 MiB 加一个探测字节并恢复正文；成功 HTTP 状态中的业务拒绝改为 502，明确会话失效改为 401。
 */
func normalizeLoomyResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil || responseContentTypeIsSSE(resp, false) {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		_ = resp.Body.Close()
		return err
	}
	// 与通用代理探测共用前缀恢复，保留原始 Body 的关闭与取消语义。
	prependToBody(resp, raw)
	if len(raw) > 2<<20 {
		return nil
	}
	var payload struct {
		Code string `json:"code"` ///< 用于聊天响应分类的上游业务码。
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	// 聊天与控制接口遵循相同的业务码归一规则。
	payload.Code = strings.TrimSpace(payload.Code)
	if payload.Code == "" || payload.Code == "000000" {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.StatusCode = http.StatusBadGateway
	}
	if loomyauth.IsSessionCode(payload.Code) {
		resp.StatusCode = http.StatusUnauthorized
	}
	resp.Status = http.StatusText(resp.StatusCode)
	return nil
}

/**
 * @brief 比较真实认证材料，避免积分与签到元数据变化干扰失效判断。
 * @param authType 渠道认证类型。
 * @param a 待比较的私有凭证文本。
 * @param b 另一份私有凭证文本。
 * @return Loomy 仅在双方凭证有效且用户 ID 与会话均一致时返回 true；其他类型沿用公共 token 比较。
 */
func sameOAuthCredentialMaterial(authType, a, b string) bool {
	if authType != model.AuthTypeLoomySession {
		return sameOAuthTokens(a, b)
	}
	left, err := loomyauth.ParseCredential([]byte(a))
	if err != nil {
		return false
	}
	right, err := loomyauth.ParseCredential([]byte(b))
	return err == nil && left.UserID == right.UserID && left.Session == right.Session
}
