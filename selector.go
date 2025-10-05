package main

import (
	"context"
	"time"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 重构：直接查询数据库，移除内存缓存依赖
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*Config, error) {
	// 直接从数据库查询（已按优先级排序）
	candidates, err := s.store.GetEnabledChannelsByType(ctx, channelType)
	if err != nil {
		return nil, err
	}

	// 过滤冷却中的渠道（直接查询数据库）
	now := time.Now()
	out := make([]*Config, 0, len(candidates))
	for _, cfg := range candidates {
		// 直接查询数据库冷却状态
		until, exists := s.store.GetCooldownUntil(ctx, cfg.ID)
		if exists && until.After(now) {
			continue // 冷却中，跳过
		}
		out = append(out, cfg)
	}

	return out, nil
}

// selectCandidates 选择支持指定模型的候选渠道
// 重构：直接查询数据库，移除内存缓存依赖
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*Config, error) {
	// 直接从数据库查询（已按优先级排序）
	candidates, err := s.store.GetEnabledChannelsByModel(ctx, model)
	if err != nil {
		return nil, err
	}

	// 过滤冷却中的渠道（直接查询数据库）
	now := time.Now()
	out := make([]*Config, 0, len(candidates))
	for _, cfg := range candidates {
		// 直接查询数据库冷却状态
		until, exists := s.store.GetCooldownUntil(ctx, cfg.ID)
		if exists && until.After(now) {
			continue // 冷却中，跳过
		}
		out = append(out, cfg)
	}

	return out, nil
}
