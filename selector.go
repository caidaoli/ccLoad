package main

import (
	"context"
	"fmt"
	"time"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 用于特殊场景（如GET /v1beta/models）需要按API类型而非模型筛选
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*Config, error) {
	// 阶段2优化：使用预建索引O(1)查找
	idx := s.channelIndex.Load()
	if idx == nil {
		_, err := s.getCachedConfigs(ctx)
		if err != nil {
			return nil, err
		}
		idx = s.channelIndex.Load()
		if idx == nil {
			return nil, fmt.Errorf("failed to build channel index")
		}
	}

	channelIdx := idx.(*channelIndex)
	now := time.Now()

	// O(1)查找：从byType索引获取指定类型的渠道
	candidates := channelIdx.byType[channelType]
	if len(candidates) == 0 {
		return nil, nil // 无匹配渠道
	}

	// 过滤冷却中的渠道
	out := make([]*Config, 0, len(candidates))
	for _, cfg := range candidates {
		if expireTime, ok := s.cooldownCache.Load(cfg.ID); ok {
			if expireTime.(time.Time).After(now) {
				continue
			}
		}
		out = append(out, cfg)
	}

	return out, nil
}

func (s *Server) selectCandidates(ctx context.Context, model string) ([]*Config, error) {
	// 阶段2优化：使用预建索引O(1)查找，替代O(n)扫描
	idx := s.channelIndex.Load()
	if idx == nil {
		// 索引未初始化，触发配置加载（会自动构建索引）
		_, err := s.getCachedConfigs(ctx)
		if err != nil {
			return nil, err
		}
		idx = s.channelIndex.Load()
		if idx == nil {
			return nil, fmt.Errorf("failed to build channel index")
		}
	}

	channelIdx := idx.(*channelIndex)
	now := time.Now()

	// O(1)查找：从索引直接获取支持该模型的渠道（已按优先级排序）
	var candidates []*Config
	if model == "*" {
		// 通配模型：使用所有启用的渠道
		candidates = channelIdx.allEnabled
	} else {
		// 精确匹配：从byModel索引查找
		candidates = channelIdx.byModel[model]
	}

	if len(candidates) == 0 {
		return nil, nil // 无匹配渠道
	}

	// 过滤冷却中的渠道（唯一运行时过滤）
	out := make([]*Config, 0, len(candidates))
	for _, cfg := range candidates {
		if expireTime, ok := s.cooldownCache.Load(cfg.ID); ok {
			if expireTime.(time.Time).After(now) {
				continue // 冷却中，跳过
			}
		}
		out = append(out, cfg)
	}

	return out, nil
}
