package app

import (
	"context"
	"log"
	"time"

	"ccLoad/internal/model"
)

type groupRouteCandidate struct {
	Group  *model.Group
	Item   model.GroupItem
	Config *model.Config
}

func (s *Server) getAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	return s.store.GetAllModelCooldowns(ctx)
}

func (s *Server) invalidateModelCooldownCache() {
	s.invalidateCooldownCache()
}

func (s *Server) selectGroupRouteCandidates(ctx context.Context, group *model.Group, clientProtocol string) ([]groupRouteCandidate, error) {
	if group == nil || len(group.Items) == 0 {
		return nil, nil
	}

	modelCooldowns, err := s.getAllModelCooldowns(ctx)
	if err != nil {
		log.Printf("[WARN] Failed to get model cooldowns, fallback to uncached group candidates: %v", err)
		modelCooldowns = make(map[int64]map[string]time.Time)
	}

	now := time.Now()
	items := orderGroupItems(group.Mode, filterGroupItemsByModelCooldown(group.Items, modelCooldowns, now))
	candidates := make([]groupRouteCandidate, 0, len(items))

	for _, item := range items {
		cfg, err := s.GetConfig(ctx, item.ChannelID)
		if err != nil {
			log.Printf("[WARN] Failed to load group candidate channel %d for group %q: %v", item.ChannelID, group.Name, err)
			continue
		}
		if cfg == nil || !cfg.Enabled {
			continue
		}
		if clientProtocol != "" && !cfg.SupportsProtocol(clientProtocol) {
			continue
		}
		if !cfg.SupportsModel(item.ModelName) {
			continue
		}
		if len(s.filterCostLimitExceededChannels([]*model.Config{cfg})) == 0 {
			continue
		}

		candidates = append(candidates, groupRouteCandidate{
			Group:  group,
			Item:   item,
			Config: cfg,
		})
	}

	if sticky, ok := getGroupStickySession(ctx, group); ok {
		for i := range candidates {
			if candidates[i].Config != nil && candidates[i].Config.ID == sticky.ChannelID {
				if i > 0 {
					stickyCandidate := candidates[i]
					copy(candidates[1:i+1], candidates[0:i])
					candidates[0] = stickyCandidate
				}
				break
			}
		}
	}

	return candidates, nil
}

func filterGroupRouteCandidatesByAllowedChannels(
	candidates []groupRouteCandidate,
	allowed []*model.Config,
) []groupRouteCandidate {
	if len(candidates) == 0 || len(allowed) == 0 {
		return nil
	}

	allowedIDs := make(map[int64]struct{}, len(allowed))
	for _, cfg := range allowed {
		if cfg == nil {
			continue
		}
		allowedIDs[cfg.ID] = struct{}{}
	}

	filtered := make([]groupRouteCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := allowedIDs[candidate.Config.ID]; ok {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}
