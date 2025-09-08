package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
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
		if !containsStr(c.Models, model) {
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
		if idx, ok := s.rrCache.Load(key); ok {
			start = idx.(int)
		} else {
			// 从数据库加载持久化的轮询指针
			start = s.store.NextRR(ctx, model, p, len(g))
			s.rrCache.Store(key, start)
		}

		// rotate: g[start:], then g[:start]
		out = append(out, g[start:]...)
		if start > 0 {
			out = append(out, g[:start]...)
		}

		// 更新轮询指针
		next := (start + 1) % len(g)
		s.rrCache.Store(key, next)
		_ = s.store.SetRR(ctx, model, p, next)
	}
	return out, nil
}
