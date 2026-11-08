package app

import (
	"context"
	"fmt"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/zaiauth"
)

/**
 * @brief 合并账号的付费模型与赠送模型，体验别名保留套餐选择语义。
 * @param ctx 查询上下文。
 * @param cfg 渠道与出口配置。
 * @param credential 账号认证材料。
 * @return 去重的模型目录；套餐尚未提供模型时可以为空。
 * @return 无法确认任何模型目录或查询取消时返回错误，不把查询失败解释为无套餐。
 */
func (s *Server) zaiAvailableModels(ctx context.Context, cfg *model.Config, credential *zaiauth.Credential) ([]string, error) {
	if credential.JWTToken == "" {
		return s.zaiCodingPlanModels(ctx, cfg, credential)
	}
	plans, err := s.zaiAccountPlans(ctx, cfg, credential, false)
	if err != nil {
		return nil, err
	}
	onlyGift := !plans.CanUsePaid(credential)
	var names []string
	if !onlyGift {
		names, err = s.zaiCodingPlanModels(ctx, cfg, credential)
		if err != nil {
			names = append([]string(nil), zaiauth.DefaultModels...)
		}
	}
	seen := make(map[string]bool)
	for _, name := range names {
		seen[strings.ToLower(name)] = true
	}
	add := func(name string) {
		if !seen[strings.ToLower(name)] {
			names = append(names, name)
			seen[strings.ToLower(name)] = true
		}
	}
	for _, gift := range plans.Gifts {
		if gift.Status != "active" && gift.Status != "exhausted" && gift.Status != "pending" {
			continue
		}
		for _, raw := range gift.Models {
			name := zaiauth.WireModel(raw)
			if onlyGift {
				add(name)
			}
			add(name + "-Trial")
		}
	}
	if len(names) == 0 && !plans.GiftKnown {
		return nil, fmt.Errorf("ZCode 模型目录暂无法确认：%s", strings.Join(plans.Warnings, "；"))
	}
	return names, nil
}
