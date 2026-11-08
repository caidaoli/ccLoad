package storage

import (
	"context"
	"time"
)

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
