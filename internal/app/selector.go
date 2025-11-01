package app

import (
	modelpkg "ccLoad/internal/model"

	"context"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：使用缓存层，内存查询 < 2ms vs 数据库查询 50ms+
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	// 缓存可用时走缓存，否则退化到存储层
	return s.getEnabledChannelsByType(ctx, channelType)
}

// selectCandidates 选择支持指定模型的候选渠道
// 性能优化：使用缓存层，消除JSON查询和聚合操作的性能杀手
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	// 缓存优先查询（自动60秒TTL刷新，避免重复的数据库性能灾难）
	return s.getEnabledChannelsByModel(ctx, model)
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
