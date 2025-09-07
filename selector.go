package main

import (
	"context"
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
	cfgs, err := s.store.ListConfigs(ctx)
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
		if until, ok := s.store.GetCooldownUntil(ctx, c.ID); ok && until.After(now) {
			// skip cooled down ones (design choice: allow fallback to others)
			continue
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
		start := s.store.NextRR(ctx, model, p, len(g))
		// rotate: g[start:], then g[:start]
		out = append(out, g[start:]...)
		if start > 0 {
			out = append(out, g[:start]...)
		}
	}
	return out, nil
}
