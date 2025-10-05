package main

import (
	"context"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：冷却过滤已在 SQL 层面完成（LEFT JOIN cooldowns），无需额外循环
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*Config, error) {
	// 直接从数据库查询（已过滤冷却渠道，按优先级排序）
	return s.store.GetEnabledChannelsByType(ctx, channelType)
}

// selectCandidates 选择支持指定模型的候选渠道
// 性能优化：冷却过滤已在 SQL 层面完成（LEFT JOIN cooldowns），无需额外循环
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*Config, error) {
	// 直接从数据库查询（已过滤冷却渠道，按优先级排序）
	return s.store.GetEnabledChannelsByModel(ctx, model)
}
