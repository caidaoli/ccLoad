package app

import (
	"context"
	"strings"

	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

func normalizeOptionalChannelType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return util.NormalizeChannelType(value)
}

func (s *Server) getEnabledChannelsByExposedProtocol(ctx context.Context, protocol string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(protocol)
	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*modelpkg.Config, error) {
			return cache.GetEnabledChannelsByExposedProtocol(ctx, normalizedType)
		},
		func() ([]*modelpkg.Config, error) {
			return s.store.GetEnabledChannelsByExposedProtocol(ctx, normalizedType)
		},
	)
}

func (s *Server) getEnabledChannelsByModelAndProtocol(ctx context.Context, model string, protocol string) ([]*modelpkg.Config, error) {
	normalizedType := normalizeOptionalChannelType(protocol)
	if normalizedType == "" {
		return s.GetEnabledChannelsByModel(ctx, model)
	}

	return readThroughChannelCache(
		s,
		func(cache *storage.ChannelCache) ([]*modelpkg.Config, error) {
			return cache.GetEnabledChannelsByModelAndProtocol(ctx, model, normalizedType)
		},
		func() ([]*modelpkg.Config, error) {
			return s.store.GetEnabledChannelsByModelAndProtocol(ctx, model, normalizedType)
		},
	)
}

// selectCandidatesByChannelType 根据客户端协议选择候选渠道
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := util.NormalizeChannelType(channelType)

	// 优先走缓存查询
	channels, err := s.getEnabledChannelsByExposedProtocol(ctx, normalizedType)
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
			if cfg != nil && cfg.Enabled && cfg.SupportsProtocol(normalizedType) {
				channels = append(channels, cfg)
			}
		}
	}

	return s.filterCooldownChannels(ctx, channels)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	normalizedType := normalizeOptionalChannelType(channelType)

	// 优先走索引查询
	channels, err := s.getEnabledChannelsByModelAndProtocol(ctx, model, normalizedType)
	if err != nil {
		return nil, err
	}

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
		source := make([]*modelpkg.Config, 0)
		if normalizedType != "" {
			source, err = s.getEnabledChannelsByModelAndProtocol(ctx, "*", normalizedType)
			if err != nil {
				return nil, err
			}
		}
		if len(source) == 0 {
			source, err = s.store.ListConfigs(ctx)
			if err != nil {
				return nil, err
			}
		}

		allCandidates = make([]*modelpkg.Config, 0, len(source))
		for _, cfg := range source {
			if cfg == nil || !cfg.Enabled {
				continue
			}
			if channelType != "" && !cfg.SupportsProtocol(normalizedType) {
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
