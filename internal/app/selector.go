package app

import (
	"context"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：使用缓存层，内存查询 < 2ms vs 数据库查询 50ms+
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)
	matcher := func(cfg *modelpkg.Config) bool {
		return cfg.GetChannelType() == normalizedType
	}
	channels, err := s.getEnabledChannelsWithFallback(ctx,
		func() ([]*modelpkg.Config, error) { return s.GetEnabledChannelsByType(ctx, channelType) },
		matcher,
	)
	if err != nil {
		return nil, err
	}
	return s.filterCooldownChannels(ctx, channels)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	// 预计算 normalizedType（闭包捕获）
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

	fastPath := func() ([]*modelpkg.Config, error) {
		channels, err := s.GetEnabledChannelsByModel(ctx, model)
		if err != nil {
			return nil, err
		}
		// [FIX] 在判断是否回退前，先应用 channelType 过滤
		// 否则精确匹配到一个 openai 渠道会阻止回退到 anthropic 渠道
		filtered := filterByType(channels)
		if len(filtered) > 0 || !s.modelLookupStripDateSuffix || model == "*" {
			return filtered, nil
		}
		stripped, ok := stripTrailingYYYYMMDD(model)
		if !ok || stripped == model {
			return filtered, nil
		}
		channels, err = s.GetEnabledChannelsByModel(ctx, stripped)
		if err != nil {
			return nil, err
		}
		return filterByType(channels), nil
	}

	// matcher 也需要考虑 channelType
	matcher := func(cfg *modelpkg.Config) bool {
		if channelType != "" && cfg.GetChannelType() != normalizedType {
			return false
		}
		return s.configSupportsModelWithDateFallback(cfg, model)
	}

	channels, err := s.getEnabledChannelsWithFallback(ctx, fastPath, matcher)
	if err != nil {
		return nil, err
	}

	return s.filterCooldownChannels(ctx, channels)
}

// getEnabledChannelsWithFallback 统一的降级查询逻辑（DRY）
// 快路径：优先走缓存/索引；若结果为空，降级到全量扫描（用于"全冷却兜底"场景）
func (s *Server) getEnabledChannelsWithFallback(
	ctx context.Context,
	fastPath func() ([]*modelpkg.Config, error),
	matcher func(*modelpkg.Config) bool,
) ([]*modelpkg.Config, error) {
	candidates, err := fastPath()
	if err != nil {
		return nil, err
	}
	if len(candidates) > 0 {
		return candidates, nil
	}

	// 降级：全量查询，手动过滤（用于"全冷却兜底"场景）
	all, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	enabled := make([]*modelpkg.Config, 0, len(all))
	for _, cfg := range all {
		if cfg == nil || !cfg.Enabled {
			continue
		}
		if matcher(cfg) {
			enabled = append(enabled, cfg)
		}
	}
	return enabled, nil
}
