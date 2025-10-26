package app

import (
	modelpkg "ccLoad/internal/model"

	"context"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：冷却过滤已在 SQL 层面完成（LEFT JOIN cooldowns），无需额外循环
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	// 直接从数据库查询（已过滤冷却渠道，按优先级排序）
	return s.store.GetEnabledChannelsByType(ctx, channelType)
}

// selectCandidates 选择支持指定模型的候选渠道
// 性能优化：冷却过滤已在 SQL 层面完成（LEFT JOIN cooldowns），无需额外循环
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	// 直接从数据库查询（已过滤冷却渠道，按优先级排序）
	return s.store.GetEnabledChannelsByModel(ctx, model)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	configs, err := s.selectCandidates(ctx, model)
	if err != nil {
		return nil, err
	}

	if channelType == "" {
		return configs, nil
	}

	normalizedType := modelpkg.NormalizeChannelType(channelType)
	filtered := make([]*modelpkg.Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg.GetChannelType() == normalizedType {
			filtered = append(filtered, cfg)
		}
	}

	return filtered, nil
}
