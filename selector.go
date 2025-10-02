package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 用于特殊场景（如GET /v1beta/models）需要按API类型而非模型筛选
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*Config, error) {
	cfgs, err := s.getCachedConfigs(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	groups := map[int][]*Config{} // priority -> configs
	for _, c := range cfgs {
		if !c.Enabled || c.APIKey == "" || c.URL == "" {
			continue
		}
		// 按渠道类型筛选（使用GetChannelType确保默认值）
		if c.GetChannelType() != channelType {
			continue
		}
		// 检查内存中的冷却状态
		if expireTime, ok := s.cooldownCache.Load(c.ID); ok {
			if expireTime.(time.Time).After(now) {
				continue
			}
		}
		groups[c.Priority] = append(groups[c.Priority], c)
	}
	// order priorities desc
	prios := make([]int, 0, len(groups))
	for p := range groups {
		prios = append(prios, p)
	}
	sort.Slice(prios, func(i, j int) bool { return prios[i] > prios[j] })
	// build final candidate list with per-priority RR rotation
	out := make([]*Config, 0)
	for _, p := range prios {
		g := groups[p]
		if len(g) <= 1 {
			out = append(out, g...)
			continue
		}
		// stable order by ID for deterministic rotation baseline
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })

		// 使用内存轮询缓存（使用channelType作为Key的一部分）
		key := fmt.Sprintf("type:%s_%d", channelType, p)
		start := 0
		val, found := s.rrCache.Get(key)
		if found {
			start = val
		} else {
			// 从数据库加载持久化的轮询指针（使用特殊模型名）
			start = s.store.NextRR(ctx, "type:"+channelType, p, len(g))
			s.rrCache.Set(key, start, 1)
		}

		// rotate: g[start:], then g[:start]
		out = append(out, g[start:]...)
		if start > 0 {
			out = append(out, g[:start]...)
		}

		// 更新轮询指针（内存立即更新，数据库异步批量持久化）
		next := (start + 1) % len(g)
		s.rrCache.Set(key, next, 1)
		// 性能优化：异步批量持久化，减少90% I/O开销
		select {
		case s.rrUpdateChan <- rrUpdate{"type:" + channelType, p, next}:
		default:
			// 通道满时降级为同步写入（罕见场景）
			_ = s.store.SetRR(ctx, "type:"+channelType, p, next)
		}
	}
	return out, nil
}

func (s *Server) selectCandidates(ctx context.Context, model string) ([]*Config, error) {
	cfgs, err := s.getCachedConfigs(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	groups := map[int][]*Config{} // priority -> configs
	for _, c := range cfgs {
		if !c.Enabled || c.APIKey == "" || c.URL == "" {
			continue
		}
		// 特殊处理：模型为"*"表示通配（如GET /v1beta/models无模型信息）
		// 此时跳过模型匹配检查
		if model != "*" && !c.HasModel(model) {
			continue
		}
		// 检查内存中的冷却状态
		if expireTime, ok := s.cooldownCache.Load(c.ID); ok {
			if expireTime.(time.Time).After(now) {
				continue
			}
		}
		groups[c.Priority] = append(groups[c.Priority], c)
	}
	// order priorities desc
	prios := make([]int, 0, len(groups))
	for p := range groups {
		prios = append(prios, p)
	}
	sort.Slice(prios, func(i, j int) bool { return prios[i] > prios[j] })
	// build final candidate list with per-priority RR rotation
	out := make([]*Config, 0)
	for _, p := range prios {
		g := groups[p]
		if len(g) <= 1 {
			out = append(out, g...)
			continue
		}
		// stable order by ID for deterministic rotation baseline
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })

		// 使用内存轮询缓存
		key := fmt.Sprintf("%s_%d", model, p)
		start := 0
		val, found := s.rrCache.Get(key)
		if found {
			start = val
		} else {
			// 从数据库加载持久化的轮询指针
			start = s.store.NextRR(ctx, model, p, len(g))
			s.rrCache.Set(key, start, 1)
		}

		// rotate: g[start:], then g[:start]
		out = append(out, g[start:]...)
		if start > 0 {
			out = append(out, g[:start]...)
		}

		// 更新轮询指针（内存立即更新，数据库异步批量持久化）
		next := (start + 1) % len(g)
		s.rrCache.Set(key, next, 1)
		// 性能优化：异步批量持久化，减少90% I/O开销
		select {
		case s.rrUpdateChan <- rrUpdate{model, p, next}:
		default:
			// 通道满时降级为同步写入（罕见场景）
			_ = s.store.SetRR(ctx, model, p, next)
		}
	}
	return out, nil
}
