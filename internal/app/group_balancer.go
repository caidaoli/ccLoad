package app

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
)

var groupRoundRobinCounter atomic.Uint64

func filterGroupItemsByModelCooldown(items []model.GroupItem, cooldowns map[int64]map[string]time.Time, now time.Time) []model.GroupItem {
	filtered := make([]model.GroupItem, 0, len(items))
	for _, item := range items {
		if byModel := cooldowns[item.ChannelID]; byModel != nil {
			if until, ok := byModel[item.ModelName]; ok && until.After(now) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func orderGroupItems(mode model.GroupMode, items []model.GroupItem) []model.GroupItem {
	ordered := slices.Clone(items)

	switch model.NormalizeGroupMode(mode) {
	case model.GroupModeRoundRobin:
		if len(ordered) <= 1 {
			return ordered
		}
		idx := int(groupRoundRobinCounter.Add(1)-1) % len(ordered)
		return append(ordered[idx:], ordered[:idx]...)

	case model.GroupModeRandom:
		rand.Shuffle(len(ordered), func(i, j int) {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		})
		return ordered

	case model.GroupModeWeighted:
		type weightedItem struct {
			item  model.GroupItem
			score float64
		}

		weighted := make([]weightedItem, 0, len(ordered))
		for _, item := range ordered {
			weight := item.Weight
			if weight <= 0 {
				weight = 1
			}
			weighted = append(weighted, weightedItem{
				item:  item,
				score: -math.Log(rand.Float64()) / float64(weight),
			})
		}

		slices.SortFunc(weighted, func(a, b weightedItem) int {
			switch {
			case a.score < b.score:
				return -1
			case a.score > b.score:
				return 1
			case a.item.Priority != b.item.Priority:
				return a.item.Priority - b.item.Priority
			case a.item.ID != b.item.ID:
				if a.item.ID < b.item.ID {
					return -1
				}
				return 1
			default:
				return 0
			}
		})

		for i := range weighted {
			ordered[i] = weighted[i].item
		}
		return ordered

	case model.GroupModeFailover:
		fallthrough
	default:
		slices.SortFunc(ordered, func(a, b model.GroupItem) int {
			if a.Priority != b.Priority {
				return a.Priority - b.Priority
			}
			if a.ID != b.ID {
				if a.ID < b.ID {
					return -1
				}
				return 1
			}
			if a.ChannelID != b.ChannelID {
				if a.ChannelID < b.ChannelID {
					return -1
				}
				return 1
			}
			switch {
			case a.ModelName < b.ModelName:
				return -1
			case a.ModelName > b.ModelName:
				return 1
			default:
				return 0
			}
		})
		return ordered
	}
}
