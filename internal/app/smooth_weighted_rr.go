package app

import (
	"strconv"
	"strings"
	"sync"
	"time"

	modelpkg "ccLoad/internal/model"
)

// SmoothWeightedRR 平滑加权轮询调度器
// 算法来源：Nginx upstream smooth weighted round-robin
// 特点：确定性分流，无随机性，分布均匀
type SmoothWeightedRR struct {
	mu     sync.Mutex
	states map[string]*rrGroupState // key: 渠道ID组合的签名
}

// rrGroupState 单个优先级组的轮询状态
type rrGroupState struct {
	currentWeights map[int64]int // channelID -> currentWeight
	lastAccess     time.Time     // 最后访问时间，用于过期清理
}

// NewSmoothWeightedRR 创建平滑加权轮询调度器
func NewSmoothWeightedRR() *SmoothWeightedRR {
	rr := &SmoothWeightedRR{
		states: make(map[string]*rrGroupState),
	}
	return rr
}

// Select 从渠道列表中选择下一个渠道（平滑加权轮询）
// channels: 同优先级的渠道列表（已按优先级分组）
// weights: 每个渠道的权重（通常是有效Key数量）
// 返回: 按轮询顺序排列的渠道列表（第一个是本次选中的）
func (rr *SmoothWeightedRR) Select(
	channels []*modelpkg.Config,
	weights []int,
) []*modelpkg.Config {
	n := len(channels)
	if n == 0 {
		return channels
	}
	if n == 1 {
		return channels
	}

	// 生成组签名（用于区分不同的渠道组合）
	groupKey := rr.generateGroupKey(channels)

	rr.mu.Lock()
	defer rr.mu.Unlock()

	// 获取或创建组状态
	state, exists := rr.states[groupKey]
	if !exists {
		state = &rrGroupState{
			currentWeights: make(map[int64]int),
		}
		rr.states[groupKey] = state
	}
	state.lastAccess = time.Now()

	// 计算总权重
	totalWeight := 0
	for _, w := range weights {
		totalWeight += w
	}
	if totalWeight == 0 {
		return channels
	}

	// Nginx 平滑加权轮询算法：
	// 1. 每个节点的 currentWeight += weight
	// 2. 选择 currentWeight 最大的节点
	// 3. 被选中节点的 currentWeight -= totalWeight

	// 步骤1: 增加权重
	for i, ch := range channels {
		state.currentWeights[ch.ID] += weights[i]
	}

	// 步骤2: 找到 currentWeight 最大的节点
	maxWeight := state.currentWeights[channels[0].ID]
	selectedIdx := 0
	for i := 1; i < n; i++ {
		cw := state.currentWeights[channels[i].ID]
		if cw > maxWeight {
			maxWeight = cw
			selectedIdx = i
		}
	}

	// 步骤3: 减去总权重
	state.currentWeights[channels[selectedIdx].ID] -= totalWeight

	// 构建结果：将选中的渠道放在第一位
	result := make([]*modelpkg.Config, n)
	result[0] = channels[selectedIdx]
	idx := 1
	for i, ch := range channels {
		if i != selectedIdx {
			result[idx] = ch
			idx++
		}
	}

	return result
}

// SelectWithCooldown 带冷却感知的平滑加权轮询
// 权重 = 有效Key数量（总Key - 冷却中Key）
func (rr *SmoothWeightedRR) SelectWithCooldown(
	channels []*modelpkg.Config,
	keyCooldowns map[int64]map[int]time.Time,
	now time.Time,
) []*modelpkg.Config {
	n := len(channels)
	if n <= 1 {
		return channels
	}

	// 计算有效权重
	weights := make([]int, n)
	for i, ch := range channels {
		weights[i] = calcEffectiveKeyCount(ch, keyCooldowns, now)
	}

	return rr.Select(channels, weights)
}

// generateGroupKey 生成渠道组的唯一标识
// 使用所有渠道ID拼接，确保不同渠道组合生成不同的key
func (rr *SmoothWeightedRR) generateGroupKey(channels []*modelpkg.Config) string {
	n := len(channels)
	if n == 0 {
		return ""
	}

	// 预估容量：每个ID约3-4字符 + 分隔符
	var b strings.Builder
	b.Grow(n * 4)

	for i, ch := range channels {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strconv.FormatInt(ch.ID, 36))
	}
	return b.String()
}

// Cleanup 清理过期的轮询状态（可选，避免内存泄漏）
// 建议在后台定期调用
func (rr *SmoothWeightedRR) Cleanup(maxAge time.Duration) {
	rr.mu.Lock()
	defer rr.mu.Unlock()

	now := time.Now()
	for key, state := range rr.states {
		if now.Sub(state.lastAccess) > maxAge {
			delete(rr.states, key)
		}
	}
}

// ResetAll 重置所有轮询状态（渠道配置变更时调用）
func (rr *SmoothWeightedRR) ResetAll() {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.states = make(map[string]*rrGroupState)
}
