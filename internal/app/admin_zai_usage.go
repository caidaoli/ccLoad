package app

import (
	"context"
	"errors"

	"ccLoad/internal/model"
	"ccLoad/internal/zaiauth"
)

/**
 * @brief ZAI 套餐与付费额度拼装，公共用量入口仅分发。
 * @param ctx 查询上下文，取消时中止上游请求。
 * @param cfg 当前渠道及凭证快照。
 * @return 套餐与付费额度汇总；整体失败时为 nil，付费额度局部失败写入 Warnings。
 * @return 管理器缺失时返回 errZAIUsageManagerUnavailable；凭证、套餐或额度查询整体失败时返回错误。
 * @note 可能刷新凭证并更新套餐查询缓存。
 */
func (s *Server) fetchZAIUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	if s.zaiCredentials == nil {
		return nil, errZAIUsageManagerUnavailable
	}
	credential, err := s.zaiCredentials.credential(ctx, cfg, false)
	if err != nil {
		return nil, oauthUsageCredentialRefreshError(err, "usage: Z.ai credential refresh failed")
	}
	if credential.JWTToken == "" {
		summary, usageErr := requestZAIUsage(ctx, s.zaiPlanService(cfg, credential), credential.APIKey)
		if usageErr == nil {
			summary.zaiIdentity = zaiPlanIdentity(cfg, credential)
			// 付费额度查询成功并不代表体验套餐已查询，前端按此标记提示登录。
			summary.DisplayMessage = "zai_login_required"
		}
		return summary, usageErr
	}
	plans, err := s.zaiAccountPlans(ctx, cfg, credential, true)
	if err != nil {
		return nil, err
	}
	summary := &oauthUsageSummary{Provider: zaiauth.ChannelType, PlanType: "ZCode", ZAIPlans: plans, Windows: []oauthUsageWindow{}, Warnings: append([]string(nil), plans.Warnings...), zaiIdentity: zaiPlanIdentity(cfg, credential)}
	if plans.CanUsePaid(credential) {
		paid, paidErr := requestZAIUsage(ctx, s.zaiPlanService(cfg, credential), credential.APIKey)
		if paidErr != nil {
			summary.Warnings = append(summary.Warnings, "付费额度查询失败")
		} else {
			summary.Windows = paid.Windows
		}
	}
	if !plans.GiftKnown && !plans.PaidKnown && len(summary.Windows) == 0 {
		return nil, errors.New("ZCode 套餐与额度暂时无法查询")
	}
	return summary, nil
}
