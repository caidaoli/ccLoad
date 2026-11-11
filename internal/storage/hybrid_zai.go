package storage

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

/**
 * @brief 从本地存储读取需要恢复的 OAuth 模型历史，不触发副本同步。
 * @param ctx 存储上下文，取消时终止查询。
 * @param authType OAuth 类型，不接受 API Key 类型。
 * @param name 模型冷却键，不得为空。
 * @param cutoff 历史截止时间，不得为零。
 * @return 仅含 ID、AuthType、OAuthCredential 的渠道快照，无历史时为空。
 * @return 输入非法、取消或数据库失败时返回错误，否则 nil。
 */
func (h *HybridStore) ListExpiredOAuthModelCooldowns(ctx context.Context, authType, name string, cutoff time.Time) ([]*model.Config, error) {
	return h.sqlite.ListExpiredOAuthModelCooldowns(ctx, authType, name, cutoff)
}

/**
 * @brief 仅同步身份匹配后成功写入的模型冷却。
 * @param ctx 数据库操作上下文。
 * @param channelID 冷却所属渠道 ID。
 * @param expectedAuthType 发起请求时的 OAuth 认证类型。
 * @param expectedCredential 发起请求时的完整凭证 JSON 快照。
 * @param model 模型冷却键，不得为空。
 * @param until 冷却截止时间。
 * @return 身份匹配且成功写入时为 true；渠道不存在或身份变化时为 false。
 * @return 身份或模型输入非法、数据库操作失败时返回错误；成功或身份不匹配时为 nil。
 * @note 仅匹配且成功写入后标记渠道待同步。
 */
func (h *HybridStore) SetModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, model string, until time.Time) (bool, error) {
	matched, err := h.sqlite.SetModelCooldownIfOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, model, until)
	if err == nil && matched {
		h.markChannelDirty(channelID, false)
	}
	return matched, err
}

/**
 * @brief 在本地原子校验 OAuth 身份并递增模型冷却，成功后同步副本。
 * @param ctx 存储操作上下文。
 * @param channelID 目标渠道 ID。
 * @param expectedAuthType 预期 OAuth 认证类型。
 * @param expectedCredential 完整凭证快照。
 * @param model 模型冷却键。
 * @param now 本次错误时间基准。
 * @param statusCode 已归一的上游状态码。
 * @return 成功写入的截止时间；拒写时为零值。
 * @return 身份匹配且成功写入为 true，否则为 false。
 * @return 数据库操作失败或输入非法时返回错误。
 */
func (h *HybridStore) BumpModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, model string, now time.Time, statusCode int) (time.Time, bool, error) {
	until, matched, err := h.sqlite.BumpModelCooldownIfOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, model, now, statusCode)
	if err == nil && matched {
		h.markChannelDirty(channelID, false)
	}
	return until, matched, err
}

/**
 * @brief 条件清理过期 OAuth 模型历史，仅实际清理后同步副本。
 * @param ctx 存储上下文。
 * @param channelID 渠道 ID。
 * @param expectedAuthType 预期 OAuth 类型。
 * @param expectedCredential 完整凭证快照。
 * @param model 模型冷却键。
 * @param cutoff 成功请求发送时间。
 * @return 实际清除返回 true；身份变化或无符合历史返回 false。
 * @return 数据库或输入错误，否则 nil。
 */
func (h *HybridStore) ResetExpiredModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, model string, cutoff time.Time) (bool, error) {
	changed, err := h.sqlite.ResetExpiredModelCooldownIfOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, model, cutoff)
	if changed && err == nil {
		h.markChannelDirty(channelID, false)
	}
	return changed, err
}
