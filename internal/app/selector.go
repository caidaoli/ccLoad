package app

import (
	"context"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)

	// 优先走缓存查询
	channels, err := s.GetEnabledChannelsByType(ctx, channelType)
	if err != nil {
		return nil, err
	}

	// 兜底：全量查询（用于“全冷却兜底”场景）
	if len(channels) == 0 {
		all, err := s.store.ListConfigs(ctx)
		if err != nil {
			return nil, err
		}
		channels = make([]*modelpkg.Config, 0, len(all))
		for _, cfg := range all {
			if cfg != nil && cfg.Enabled && cfg.GetChannelType() == normalizedType {
				channels = append(channels, cfg)
			}
		}
	}

	return s.filterCooldownChannels(ctx, channels)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)

	// 类型过滤辅助函数
	filterByType := func(channels []*modelpkg.Config) []*modelpkg.Config {
		if channelType == "" {
			return channels
		}
		filtered := make([]*modelpkg.Config, 0, len(channels))
		for _, cfg := range channels {
			if cfg.GetChannelType() == normalizedType {
				filtered = append(filtered, cfg)
			}
		}
		return filtered
	}

	// 优先走索引查询
	channels, err := s.GetEnabledChannelsByModel(ctx, model)
	if err != nil {
		return nil, err
	}

	// [FIX] 在判断是否回退前，先应用 channelType 过滤
	// 否则精确匹配到一个 openai 渠道会阻止回退到 anthropic 渠道
	channels = filterByType(channels)

	// 先做冷却/成本过滤，但不触发“全冷却兜底”，以便后续还能继续做模糊匹配回退。
	filtered, err := s.filterCooldownChannelsStrict(ctx, channels)
	if err != nil {
		return nil, err
	}
	if len(filtered) > 0 {
		return filtered, nil
	}

	// 兜底：全量查询（用于“模糊匹配回退”以及最终“全冷却兜底”场景）
	// 注意：此处不能以 len(channels)==0 作为是否回退的条件。
	// 精确候选可能存在但全部在冷却/成本限额下不可用，这时仍需尝试模糊匹配补充候选。
	var allCandidates []*modelpkg.Config
	if model != "*" {
		all, err := s.store.ListConfigs(ctx)
		if err != nil {
			return nil, err
		}
		allCandidates = make([]*modelpkg.Config, 0, len(all))
		for _, cfg := range all {
			if cfg == nil || !cfg.Enabled {
				continue
			}
			if channelType != "" && cfg.GetChannelType() != normalizedType {
				continue
			}
			if s.configSupportsModelWithFuzzyMatch(cfg, model) {
				allCandidates = append(allCandidates, cfg)
			}
		}
	}

	// 再次过滤，但仍不触发“全冷却兜底”：先把可用的候选尽可能找出来。
	filtered, err = s.filterCooldownChannelsStrict(ctx, allCandidates)
	if err != nil {
		return nil, err
	}
	if len(filtered) > 0 {
		return filtered, nil
	}

	// 最终兜底：如果候选存在但全部在冷却中，让全冷却兜底逻辑选择“最早恢复”的渠道。
	return s.filterCooldownChannels(ctx, allCandidates)
}
