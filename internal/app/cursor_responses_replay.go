package app

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"time"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 限制每个 HTTP Cursor 工具续轮的可重放响应，保留期与 SDK 工具重放一致。
 */
const (
	cursorResponsesIDPrefix    = "resp_ccload_cursor_" ///< 专属 ID 仅能在 ccLoad 执行会话中解析。
	cursorResponsesReplayTTL   = 3 * time.Minute       ///< 自该轮成功交付起可以重试的时间。
	cursorResponsesReplayBytes = 256 * 1024            ///< 单次可保留的下游正文上限，单位字节。
)

/**
 * @brief 记住最后一次完成的工具续轮与下游收到的完整 JSON 或 SSE。
 */
type cursorResponsesHTTPToolReplay struct {
	previousID  string            ///< 工具调用所在轮的本地响应 ID。
	request     [sha256.Size]byte ///< 原始路径、查询参数和请求正文的指纹。
	expires     time.Time         ///< 响应重放截止时间。
	channelID   int64             ///< 已成功交付响应的 Cursor 渠道。
	key         [sha256.Size]byte ///< 实际执行 Cursor Run 的 User API Key 指纹。
	actualModel string            ///< 实际交给 SDK 的模型，必须仍有可用配置行。
	body        []byte            ///< 响应不可重放时为空，仅保留已消费的 ID。
}

/**
 * @brief 在当前请求可访问的渠道中重新核对原响应的凭证和实际模型。
 * @param ctx 当前请求上下文。
 * @param candidates 已完成令牌授权和显式渠道选择的候选。
 * @param requested 请求的路由模型。
 * @param requestProtocol 客户端请求协议。
 * @param replay 已完成工具轮保存的路由身份。
 * @return 原渠道、User API Key 及实际模型仍可用时为 true。
 * @note 读取当前配置凭证，不刷新 OAuth 或推进模型轮转。
 */
func (s *Server) cursorResponsesReplayRouteAllowed(ctx context.Context, candidates []*model.Config, requested, requestProtocol string, replay *cursorResponsesHTTPToolReplay) bool {
	for _, cfg := range candidates {
		if cfg.ID != replay.channelID || !cfg.UsesCursorOAuth() || cfg.CooldownFallback {
			continue
		}
		credential, err := cursorauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || sha256.Sum256([]byte(credential.APIKey)) != replay.key {
			return false
		}
		_, err = s.selectChannelModelRow(ctx, cfg, requested, requestProtocol, nil, nil, replay.actualModel)
		return err == nil
	}
	return false
}

/**
 * @brief 识别对同一个 HTTP 请求的精确重试。
 * @param req 客户端的 HTTP 请求。
 * @param body 客户端原始 JSON，不含路由或隐私改写。
 * @return 请求目标与正文的指纹。
 */
func cursorResponsesReplayRequest(req *http.Request, body []byte) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = io.WriteString(digest, req.URL.RequestURI())
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return [sha256.Size]byte(digest.Sum(nil))
}

/**
 * @brief 记录隐私还原后写向客户端的原始响应，单次超出上限时丢弃整份副本。
 */
type cursorResponsesReplayWriter struct {
	gin.ResponseWriter
	body     []byte ///< 保存的完整响应字节。
	overflow bool   ///< 超出上限后不再捕获后续帧。
}

/**
 * @brief 原样写出响应并保存此次成功写出的字节。
 * @param data 响应正文或 SSE 帧。
 * @return 已成功写出的字节数。
 * @return 下游写入错误；成功为 nil。
 */
func (w *cursorResponsesReplayWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if !w.overflow {
		if len(w.body)+n > cursorResponsesReplayBytes {
			w.body, w.overflow = nil, true
		} else {
			w.body = append(w.body, data[:n]...)
		}
	}
	return n, err
}

/**
 * @brief 保留 Gin 字符串写入的响应内容。
 * @param data 响应正文。
 * @return 已成功写出的字节数。
 * @return 下游写入错误；成功为 nil。
 */
func (w *cursorResponsesReplayWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

/**
 * @brief 暴露下游写入器，供响应控制器设置写入截止时间。
 * @return 原下游写入器。
 */
func (w *cursorResponsesReplayWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
