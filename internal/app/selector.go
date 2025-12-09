package app

import (
	modelpkg "ccLoad/internal/model"
	"ccLoad/internal/util"

	"context"
	"log"
	"math/rand/v2"
	"time"
)

// selectCandidatesByChannelType 根据渠道类型选择候选渠道
// 性能优化：使用缓存层，内存查询 < 2ms vs 数据库查询 50ms+
func (s *Server) selectCandidatesByChannelType(ctx context.Context, channelType string) ([]*modelpkg.Config, error) {
	// 缓存可用时走缓存，否则退化到存储层
	channels, err := s.GetEnabledChannelsByType(ctx, channelType)
	if err != nil {
		return nil, err
	}
	return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(channels))
}

// selectCandidates 选择支持指定模型的候选渠道
// 性能优化：使用缓存层，消除JSON查询和聚合操作的性能杀手
func (s *Server) selectCandidates(ctx context.Context, model string) ([]*modelpkg.Config, error) {
	// 缓存优先查询（自动60秒TTL刷新，避免重复的数据库性能灾难）
	return s.GetEnabledChannelsByModel(ctx, model)
}

// selectCandidatesByModelAndType 根据模型和渠道类型筛选候选渠道
// 遵循SRP：数据库负责返回满足模型的渠道，本函数仅负责类型过滤
func (s *Server) selectCandidatesByModelAndType(ctx context.Context, model string, channelType string) ([]*modelpkg.Config, error) {
	configs, err := s.selectCandidates(ctx, model)
	if err != nil {
		return nil, err
	}

	if channelType == "" {
		return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(configs))
	}

	normalizedType := util.NormalizeChannelType(channelType)
	filtered := make([]*modelpkg.Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg.GetChannelType() == normalizedType {
			filtered = append(filtered, cfg)
		}
	}

	return s.filterCooldownChannels(ctx, shuffleSamePriorityChannels(filtered))
}

// filterCooldownChannels 过滤掉冷却中的渠道
// ✅ 修复 (2025-12-09): 在渠道选择阶段就过滤冷却渠道，避免无效尝试
// 过滤规则:
//   1. 渠道级冷却 → 直接过滤
//   2. 所有Key都在冷却 → 过滤
//   3. 至少有一个Key可用 → 保留
func (s *Server) filterCooldownChannels(ctx context.Context, channels []*modelpkg.Config) ([]*modelpkg.Config, error) {
	if len(channels) == 0 {
		return channels, nil
	}

	now := time.Now()

	// 批量查询冷却状态（使用缓存层，性能优化）
	channelCooldowns, err := s.getAllChannelCooldowns(ctx)
	if err != nil {
		// 降级处理：查询失败时不过滤，避免阻塞请求
		log.Printf("⚠️  WARNING: Failed to get channel cooldowns (degraded mode): %v", err)
		return channels, nil
	}

	keyCooldowns, err := s.getAllKeyCooldowns(ctx)
	if err != nil {
		// 降级处理：查询失败时不过滤
		log.Printf("⚠️  WARNING: Failed to get key cooldowns (degraded mode): %v", err)
		return channels, nil
	}

	// 过滤冷却中的渠道
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		// 1. 检查渠道级冷却
		if cooldownUntil, exists := channelCooldowns[cfg.ID]; exists {
			if cooldownUntil.After(now) {
				continue // 渠道冷却中，跳过
			}
		}

		// 2. 检查是否所有Key都在冷却
		keyMap, hasKeys := keyCooldowns[cfg.ID]
		if hasKeys {
			// 检查是否至少有一个Key可用
			hasAvailableKey := false
			for _, cooldownUntil := range keyMap {
				if cooldownUntil.Before(now) || cooldownUntil.Equal(now) {
					hasAvailableKey = true
					break
				}
			}
			if !hasAvailableKey {
				continue // 所有Key都冷却中，跳过
			}
		}

		// 渠道可用
		filtered = append(filtered, cfg)
	}

	return filtered, nil
}

// shuffleSamePriorityChannels 随机打乱相同优先级的渠道，实现负载均衡
// 设计原则：
// - KISS: 简单的随机化比复杂的状态管理更可靠
// - 无状态: 避免并发竞争和持久化开销
// - 保持优先级: 只在相同优先级组内打乱，不影响优先级排序
func shuffleSamePriorityChannels(channels []*modelpkg.Config) []*modelpkg.Config {
	if len(channels) <= 1 {
		return channels
	}

	// 按优先级分组
	type priorityGroup struct {
		priority int
		start    int
		end      int
	}

	var groups []priorityGroup
	currentPriority := channels[0].Priority
	groupStart := 0

	for i := 1; i < len(channels); i++ {
		if channels[i].Priority != currentPriority {
			// 发现新的优先级组
			groups = append(groups, priorityGroup{
				priority: currentPriority,
				start:    groupStart,
				end:      i,
			})
			currentPriority = channels[i].Priority
			groupStart = i
		}
	}
	// 添加最后一组
	groups = append(groups, priorityGroup{
		priority: currentPriority,
		start:    groupStart,
		end:      len(channels),
	})

	// 对每个优先级组内的渠道进行随机打乱
	result := make([]*modelpkg.Config, len(channels))
	copy(result, channels)

	for _, group := range groups {
		groupSize := group.end - group.start
		if groupSize > 1 {
			// Fisher-Yates洗牌算法
			for i := group.start; i < group.end-1; i++ {
				j := i + rand.IntN(groupSize-(i-group.start))
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return result
}
