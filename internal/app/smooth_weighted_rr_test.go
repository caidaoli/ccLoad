package app

import (
	"testing"
	"time"

	modelpkg "ccLoad/internal/model"
)

func TestSmoothWeightedRR_ExactDistribution(t *testing.T) {
	// 测试平滑加权轮询的精确分布
	// 权重 A:3, B:1，期望严格的 3:1 分布

	rr := NewSmoothWeightedRR()

	iterations := 100
	firstPositionCount := make(map[string]int)

	for i := 0; i < iterations; i++ {
		channels := []*modelpkg.Config{
			{ID: 1, Name: "channel-A", Priority: 10, KeyCount: 3},
			{ID: 2, Name: "channel-B", Priority: 10, KeyCount: 1},
		}
		weights := []int{3, 1}

		result := rr.Select(channels, weights)
		firstPositionCount[result[0].Name]++
	}

	ratioA := float64(firstPositionCount["channel-A"]) / float64(iterations) * 100
	ratioB := float64(firstPositionCount["channel-B"]) / float64(iterations) * 100

	t.Logf("[STATS] 平滑加权轮询统计（%d次）:", iterations)
	t.Logf("  - channel-A (权重3) 首位: %d次 (%.1f%%), 期望75%%",
		firstPositionCount["channel-A"], ratioA)
	t.Logf("  - channel-B (权重1) 首位: %d次 (%.1f%%), 期望25%%",
		firstPositionCount["channel-B"], ratioB)

	// 平滑加权轮询是确定性的，应该精确匹配
	// 100次中：A应该75次，B应该25次
	expectedA := 75
	expectedB := 25

	if firstPositionCount["channel-A"] != expectedA {
		t.Errorf("平滑加权轮询分布错误: channel-A出现%d次，期望%d次",
			firstPositionCount["channel-A"], expectedA)
	}
	if firstPositionCount["channel-B"] != expectedB {
		t.Errorf("平滑加权轮询分布错误: channel-B出现%d次，期望%d次",
			firstPositionCount["channel-B"], expectedB)
	}
}

func TestSmoothWeightedRR_SequencePattern(t *testing.T) {
	// 验证 Nginx 平滑加权轮询的序列模式
	// 权重 A:3, B:1 的序列应该是: A, A, B, A, A, A, B, A...（平滑分布）

	rr := NewSmoothWeightedRR()

	channels := []*modelpkg.Config{
		{ID: 1, Name: "A", Priority: 10, KeyCount: 3},
		{ID: 2, Name: "B", Priority: 10, KeyCount: 1},
	}
	weights := []int{3, 1}

	// 连续8次选择
	sequence := make([]string, 8)
	for i := 0; i < 8; i++ {
		result := rr.Select(channels, weights)
		sequence[i] = result[0].Name
	}

	t.Logf("[SEQUENCE] 前8次选择: %v", sequence)

	// 统计连续的A
	maxConsecutiveA := 0
	currentConsecutiveA := 0
	for _, name := range sequence {
		if name == "A" {
			currentConsecutiveA++
			if currentConsecutiveA > maxConsecutiveA {
				maxConsecutiveA = currentConsecutiveA
			}
		} else {
			currentConsecutiveA = 0
		}
	}

	// 平滑加权轮询的特点：最大连续A不应超过权重比
	// 对于3:1，最大连续A应该是3
	if maxConsecutiveA > 3 {
		t.Errorf("平滑加权轮询不平滑: 最大连续A为%d，期望<=3", maxConsecutiveA)
	}

	// 验证8次中A出现6次，B出现2次（3:1比例）
	countA := 0
	countB := 0
	for _, name := range sequence {
		if name == "A" {
			countA++
		} else {
			countB++
		}
	}

	if countA != 6 || countB != 2 {
		t.Errorf("分布错误: A=%d, B=%d，期望 A=6, B=2", countA, countB)
	}
}

func TestSmoothWeightedRR_WithCooldown(t *testing.T) {
	// 测试冷却感知的平滑加权轮询
	// channel-A: 10 keys, 8个冷却 → 有效2个
	// channel-B: 2 keys, 0个冷却 → 有效2个
	// 期望严格的 1:1 分布

	rr := NewSmoothWeightedRR()

	now := time.Now()
	keyCooldowns := map[int64]map[int]time.Time{
		1: { // channel-A 的8个key处于冷却中
			0: now.Add(time.Minute),
			1: now.Add(time.Minute),
			2: now.Add(time.Minute),
			3: now.Add(time.Minute),
			4: now.Add(time.Minute),
			5: now.Add(time.Minute),
			6: now.Add(time.Minute),
			7: now.Add(time.Minute),
		},
	}

	iterations := 100
	firstPositionCount := make(map[string]int)

	for i := 0; i < iterations; i++ {
		channels := []*modelpkg.Config{
			{ID: 1, Name: "channel-A", Priority: 10, KeyCount: 10},
			{ID: 2, Name: "channel-B", Priority: 10, KeyCount: 2},
		}

		result := rr.SelectWithCooldown(channels, keyCooldowns, now)
		firstPositionCount[result[0].Name]++
	}

	t.Logf("[STATS] 冷却感知平滑加权轮询统计（%d次）:", iterations)
	t.Logf("  - channel-A (10 Keys, 8冷却, 有效2) 首位: %d次 (%.1f%%)",
		firstPositionCount["channel-A"],
		float64(firstPositionCount["channel-A"])/float64(iterations)*100)
	t.Logf("  - channel-B (2 Keys, 0冷却, 有效2) 首位: %d次 (%.1f%%)",
		firstPositionCount["channel-B"],
		float64(firstPositionCount["channel-B"])/float64(iterations)*100)

	// 有效权重相等，应该各50次
	expectedEach := 50

	if firstPositionCount["channel-A"] != expectedEach {
		t.Errorf("冷却感知分布错误: channel-A出现%d次，期望%d次",
			firstPositionCount["channel-A"], expectedEach)
	}
	if firstPositionCount["channel-B"] != expectedEach {
		t.Errorf("冷却感知分布错误: channel-B出现%d次，期望%d次",
			firstPositionCount["channel-B"], expectedEach)
	}
}

func TestSmoothWeightedRR_Integration(t *testing.T) {
	// 集成测试：验证 SmoothWeightedRR 的完整工作流

	balancer := NewSmoothWeightedRR()

	channels := []*modelpkg.Config{
		{ID: 39, Name: "glm", Priority: 190, KeyCount: 3},
		{ID: 5, Name: "foxhank-glm", Priority: 190, KeyCount: 1},
	}

	now := time.Now()
	keyCooldowns := map[int64]map[int]time.Time{} // 无冷却

	iterations := 100
	callCount := make(map[int64]int)

	for i := 0; i < iterations; i++ {
		result := balancer.SelectWithCooldown(channels, keyCooldowns, now)
		callCount[result[0].ID]++
	}

	t.Logf("[STATS] SmoothWeightedRR 集成测试（%d次）:", iterations)
	t.Logf("  - 渠道39 (3 Keys): %d次 (%.1f%%), 期望75%%",
		callCount[39], float64(callCount[39])/float64(iterations)*100)
	t.Logf("  - 渠道5 (1 Key): %d次 (%.1f%%), 期望25%%",
		callCount[5], float64(callCount[5])/float64(iterations)*100)

	// 平滑加权轮询是确定性的
	if callCount[39] != 75 {
		t.Errorf("渠道39分布错误: %d次，期望75次", callCount[39])
	}
	if callCount[5] != 25 {
		t.Errorf("渠道5分布错误: %d次，期望25次", callCount[5])
	}
}

func TestSmoothWeightedRR_GroupKeyFormat(t *testing.T) {
	// 验证 groupKey 的格式与可读性：十进制 + 逗号分隔。
	// 这不是“修复玄学碰撞”，而是把 key 做成明确、可测试的字符串格式。

	rr := NewSmoothWeightedRR()

	// 场景1: [10, 36] 应该生成 "10,36"
	channels1 := []*modelpkg.Config{
		{ID: 10, Name: "ch10"},
		{ID: 36, Name: "ch36"},
	}
	key1 := rr.generateGroupKey(channels1)

	// 场景2: [370] 应该生成 "370"
	channels2 := []*modelpkg.Config{
		{ID: 370, Name: "ch370"},
	}
	key2 := rr.generateGroupKey(channels2)

	t.Logf("[KEY] 渠道组[10,36]的key: %q", key1)
	t.Logf("[KEY] 渠道组[370]的key:   %q", key2)

	if key1 == key2 {
		t.Errorf("哈希冲突检测失败: 不同渠道组合生成了相同的key %q", key1)
	}

	// 验证生成的key格式正确
	if key1 != "10,36" {
		t.Errorf("渠道组[10,36]的key错误: 得到 %q, 期望 \"10,36\"", key1)
	}
	if key2 != "370" {
		t.Errorf("渠道组[370]的key错误: 得到 %q, 期望 \"370\"", key2)
	}

	// 额外验证：确保轮询状态确实被隔离
	weights1 := []int{1, 1}
	weights2 := []int{1}

	// 对第一组轮询几次
	for i := 0; i < 5; i++ {
		rr.Select(channels1, weights1)
	}

	// 对第二组轮询，应该从初始状态开始
	result2 := rr.Select(channels2, weights2)
	if result2[0].ID != 370 {
		t.Errorf("轮询状态隔离失败: 期望选中370，实际选中%d", result2[0].ID)
	}
}

func TestSmoothWeightedRR_GroupKeyOrderIndependent(t *testing.T) {
	rr := NewSmoothWeightedRR()

	a := []*modelpkg.Config{
		{ID: 10, Name: "ch10"},
		{ID: 36, Name: "ch36"},
	}
	b := []*modelpkg.Config{
		{ID: 36, Name: "ch36"},
		{ID: 10, Name: "ch10"},
	}

	keyA := rr.generateGroupKey(a)
	keyB := rr.generateGroupKey(b)

	if keyA != keyB {
		t.Fatalf("same set should have same key: keyA=%q keyB=%q", keyA, keyB)
	}
	if keyA != "10,36" {
		t.Fatalf("unexpected key: %q", keyA)
	}
}

func TestSmoothWeightedRR_TieBreakIndependentOfInputOrder(t *testing.T) {
	chA := &modelpkg.Config{ID: 10, Name: "A", Priority: 10, KeyCount: 1}
	chB := &modelpkg.Config{ID: 36, Name: "B", Priority: 10, KeyCount: 1}

	weights := []int{1, 1}

	// 相同集合、相同权重，只是输入顺序不同：在“干净状态”下首选应一致（由 tie-break 决定）。
	rr1 := NewSmoothWeightedRR()
	rr2 := NewSmoothWeightedRR()
	r1 := rr1.Select([]*modelpkg.Config{chA, chB}, weights)
	r2 := rr2.Select([]*modelpkg.Config{chB, chA}, weights)

	if r1[0].ID != r2[0].ID {
		t.Fatalf("tie-break should be order independent: r1=%d r2=%d", r1[0].ID, r2[0].ID)
	}
	if r1[0].ID != 10 {
		t.Fatalf("expected smaller ID to win tie-break, got %d", r1[0].ID)
	}
}
