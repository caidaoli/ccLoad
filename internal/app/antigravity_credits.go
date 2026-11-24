package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

var errAntigravityCreditsUnavailable = errors.New("antigravity credits unavailable")

func antigravityCredentialAttempted(tried *map[string]bool, cfg *model.Config, credential *antigravityauth.Credential) bool {
	channel := fmt.Sprintf("channel:%d", cfg.ID)
	identity := credential.RefreshToken
	if credential.Email != "" {
		identity = credential.Email + "\x00" + credential.ProjectID
	}
	if (*tried)[channel] || (*tried)[identity] {
		return true
	}
	if *tried == nil {
		*tried = make(map[string]bool)
	}
	(*tried)[channel], (*tried)[identity] = true, true
	return false
}

func antigravityClaudeModel(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "claude-")
}

/**
 * @brief 在普通候选之后追加通过严格冷却过滤的 Credits 候选
 * @param ctx 请求上下文，含令牌渠道范围
 * @param ordinary 已选出的普通候选；可为空
 * @param modelName 路由模型
 * @param clientProtocol 客户端协议
 * @param body 用于排除不支持 Credits 的搜索请求
 * @return 普通候选与可尝试的 Credits 候选；查询失败时保留普通候选
 */
func (s *Server) appendAntigravityCreditsCandidates(ctx context.Context, ordinary []*model.Config, modelName, clientProtocol string, body []byte) []*model.Config {
	paid, err := s.antigravityCreditsCandidatePool(ctx, modelName, body)
	if err != nil {
		log.Printf("[WARN] Antigravity credits candidates unavailable: %v", err)
		return ordinary
	}
	paid, err = s.filterCooldownChannelsStrict(ctx, paid, modelName, clientProtocol)
	if err != nil {
		log.Printf("[WARN] Antigravity credits cooldown check failed: %v", err)
		return ordinary
	}
	for _, cfg := range paid {
		cfg.AntigravityCredits = true
		cfg.CooldownFallback = false
	}
	return append(ordinary, paid...)
}

/**
 * @brief 获取可参与 Credits 尝试的全部模型匹配候选，保留仍在冷却的渠道
 * @param ctx 请求上下文
 * @param modelName 路由模型
 * @param body 用于排除不支持 Credits 的搜索请求
 * @return 请求私有的配置副本，初始设置 CooldownFallback 供现有准入探测使用；不支持时为空
 * @return 候选存储读取失败
 */
func (s *Server) antigravityCreditsCandidatePool(ctx context.Context, modelName string, body []byte) ([]*model.Config, error) {
	if s.antigravityCredentials == nil || wantsAntigravityWebSearch(body) {
		return nil, nil
	}
	source, err := s.getEnabledChannelsSnapshotByModel(ctx, "*")
	if err != nil {
		return nil, err
	}
	paid := make([]*model.Config, 0)
	for _, cfg := range source {
		if cfg == nil || !cfg.Enabled || !cfg.UsesAntigravityOAuth() || !s.configSupportsModelWithFuzzyMatch(cfg, modelName) {
			continue
		}
		eligible := false
		for _, row := range s.enumerateModelRows(cfg, modelName) {
			if antigravityClaudeModel(s.resolveFinalUpstreamModel(cfg, row, string(protocol.Gemini))) {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		clone := cfg.Clone()
		clone.CooldownFallback = true
		paid = append(paid, clone)
	}
	return paid, nil
}

func (m *antigravityCredentialManager) standardQuotaUntil(cfg *model.Config, modelName string) time.Time {
	if m == nil || cfg == nil || !cfg.UsesAntigravityOAuth() {
		return time.Time{}
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return time.Time{}
	}
	return credential.StandardQuota[modelName]
}

func (m *antigravityCredentialManager) updateQuotaState(ctx context.Context, cfg *model.Config, modelName string, until time.Time, insufficient bool) {
	if m == nil {
		return
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil || credential.AccessToken != cfg.AntigravityAccessToken {
		return
	}
	writeCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()
	now := time.Now()
	_, err = m.updateStoredMetadata(writeCtx, cfg.ID, credential, func(current *antigravityauth.Credential) {
		if insufficient {
			if current.Credits == nil {
				current.Credits = &antigravityauth.Credits{}
			}
			current.Credits.UnavailableAt = now.UnixMilli()
			return
		}
		for name, expiry := range current.StandardQuota {
			if !expiry.After(now) {
				delete(current.StandardQuota, name)
			}
		}
		if until.IsZero() {
			delete(current.StandardQuota, modelName)
			return
		}
		if current.StandardQuota == nil {
			current.StandardQuota = make(map[string]time.Time)
		}
		current.StandardQuota[modelName] = until
	})
	if err != nil {
		log.Printf("[WARN] Antigravity quota state persistence failed: channel_id=%d err=%v", cfg.ID, err)
	}
}

func (s *Server) prepareAntigravityCredits(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, credential *antigravityauth.Credential) (*antigravityauth.Credential, error) {
	if credential == nil {
		return nil, errAntigravityCreditsUnavailable
	}
	actualModel := s.resolveFinalUpstreamModel(cfg, reqCtx.attemptModel, string(protocol.Gemini))
	if !antigravityClaudeModel(actualModel) || wantsAntigravityWebSearch(reqCtx.body) || !credential.StandardQuota[actualModel].After(time.Now()) {
		return credential, errAntigravityCreditsUnavailable
	}
	if !credential.Credits.Fresh(time.Now()) {
		var err error
		credential, err = s.antigravityCredentials.completeStoredMetadata(ctx, cfg, credential, true)
		if err != nil {
			if oauthRefreshTokenRejected(err) {
				return credential, err
			}
			return credential, errAntigravityCreditsUnavailable
		}
	}
	if credential == nil || !credential.Credits.Fresh(time.Now()) || !credential.Credits.Available() || !credential.StandardQuota[actualModel].After(time.Now()) {
		return credential, errAntigravityCreditsUnavailable
	}
	eligible, err := s.filterCooldownChannelsStrict(ctx, []*model.Config{cfg}, reqCtx.originalModel, string(reqCtx.clientProtocol))
	if err != nil || len(eligible) == 0 {
		return credential, errAntigravityCreditsUnavailable
	}
	return credential, nil
}

/**
 * @brief 仅从 Google ErrorInfo 识别可触发独立额度状态的失败原因。
 * @param body 上游 HTTP 429 正文或 SSE 错误。
 * @return 已知限流原因；仅有延迟提示时为空。
 */
func antigravityLimitReason(body []byte) (reason string) {
	if gjson.GetBytes(body, "error.status").String() != "RESOURCE_EXHAUSTED" {
		return ""
	}
	for _, detail := range gjson.GetBytes(body, "error.details").Array() {
		if detail.Get("@type").String() == "type.googleapis.com/google.rpc.ErrorInfo" {
			value := detail.Get("reason").String()
			order := []string{"RATE_LIMIT_EXCEEDED", "QUOTA_EXHAUSTED", "INSUFFICIENT_G1_CREDITS_BALANCE"}
			if slices.Index(order, value) > slices.Index(order, reason) {
				reason = value
			}
		}
	}
	return reason
}

/**
 * @brief 从正文读取 Antigravity 恢复时间；没有有效提示时读取 HTTP 错误响应头。
 * @param body 上游 HTTP 错误正文或 SSE 错误。
 * @param headers 实际 HTTP 错误的响应头；内嵌错误传 nil。
 * @param now 相对恢复时间的起算时间。
 * @return 明确的恢复时间；无提示时为零值，显式零秒返回 now。
 */
func antigravityRetryAt(body []byte, headers http.Header, now time.Time) time.Time {
	details := gjson.GetBytes(body, "error.details").Array()
	for _, detail := range details {
		if detail.Get("@type").String() == "type.googleapis.com/google.rpc.RetryInfo" {
			delay, err := time.ParseDuration(strings.ToLower(strings.TrimSpace(detail.Get("retryDelay").String())))
			if err == nil && delay >= 0 {
				return now.Add(delay)
			}
		}
	}
	if until, ok := util.GoogleRateLimitRetryAt(body, now); ok {
		return until
	}
	if until, ok := util.ParseRetryAfterHint(headers, now); ok {
		return until
	}
	return time.Time{}
}

/**
 * @brief 读取 Antigravity 最终尝试的恢复时间，SSE 错误只接受帧内提示。
 * @param res 上游 429 或已归类的 SSE 失败结果。
 * @param now 相对恢复时间的起算时间。
 * @return 明确恢复时间；未收到提示时为零值。
 */
func antigravityAttemptRetryAt(res *fwResult, now time.Time) time.Time {
	if len(res.SSEErrorEvent) > 0 {
		return antigravityRetryAt(res.SSEErrorEvent, nil, now)
	}
	return antigravityRetryAt(res.Body, res.Header, now)
}

/**
 * @brief 按 Antigravity 的明确限流原因写模型恢复时间、Claude 标准额度或积分状态。
 * @param ctx 当前请求上下文。
 * @param cfg 本次出站凭证。
 * @param modelName 实际上游模型。
 * @param res 已归类的上游失败。
 * @return 该额度状态由提供商接管时返回 true。
 */
func (s *Server) recordAntigravity429State(ctx context.Context, cfg *model.Config, modelName string, res *fwResult) bool {
	if !cfg.UsesAntigravityOAuth() || res.Status != http.StatusTooManyRequests {
		return false
	}
	body, _ := forwardResultErrorPayload(res)
	reason := antigravityLimitReason(body)
	now := time.Now()
	if reason == "RATE_LIMIT_EXCEEDED" {
		until := antigravityAttemptRetryAt(res, now)
		if until.IsZero() {
			return false
		}
		if until.After(now) {
			writeCtx, cancel := cooldownWriteContext(ctx)
			defer cancel()
			// The cooldown store has second precision. Round up to preserve the wait.
			if second := until.Truncate(time.Second); !until.Equal(second) {
				until = second.Add(time.Second)
			}
			if err := s.store.SetModelCooldown(writeCtx, cfg.ID, modelName, until); err != nil {
				log.Printf("[WARN] Antigravity rate cooldown persistence failed: channel_id=%d err=%v", cfg.ID, err)
			}
			s.invalidateChannelRelatedCache(cfg.ID)
		}
		return true
	}
	if cfg.AntigravityCredits {
		if reason != "INSUFFICIENT_G1_CREDITS_BALANCE" {
			return false
		}
		s.antigravityCredentials.updateQuotaState(ctx, cfg, modelName, time.Time{}, true)
		return true
	}
	if reason != "QUOTA_EXHAUSTED" || !antigravityClaudeModel(modelName) {
		return false
	}
	until := antigravityAttemptRetryAt(res, now)
	if until.IsZero() {
		until = now.Add(time.Minute)
	}
	if until.After(now) {
		s.antigravityCredentials.updateQuotaState(ctx, cfg, modelName, until, false)
	}
	return true
}
