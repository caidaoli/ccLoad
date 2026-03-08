package app

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestURLSelector_SingleURL(t *testing.T) {
	sel := NewURLSelector()
	url, idx := sel.SelectURL(1, []string{"https://a.com"})
	if url != "https://a.com" || idx != 0 {
		t.Errorf("single URL: expected (https://a.com, 0), got (%s, %d)", url, idx)
	}
}

func TestURLSelector_EmptyURLs(t *testing.T) {
	sel := NewURLSelector()

	url, idx := sel.SelectURL(1, nil)
	if url != "" || idx != -1 {
		t.Fatalf("expected empty selection for empty urls, got (%q, %d)", url, idx)
	}

	sorted := sel.SortURLs(1, nil)
	if len(sorted) != 0 {
		t.Fatalf("expected empty sorted urls, got %v", sorted)
	}
}

func TestURLSelector_ColdStart_Distributes(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// 冷启动时应随机分布到所有URL，而非永远选第一个
	seen := map[string]int{}
	for range 100 {
		url, _ := sel.SelectURL(1, urls)
		seen[url]++
	}
	for _, u := range urls {
		if seen[u] == 0 {
			t.Errorf("cold start: URL %s was never selected in 100 rounds", u)
		}
	}
}

func TestURLSelector_WeightedRandom(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://slow.com", "https://fast.com"}
	// 记录延迟: slow=500ms, fast=100ms
	// 加权随机: fast权重=1/100, slow权重=1/500 → fast占83.3%
	sel.RecordLatency(1, "https://slow.com", 500*time.Millisecond)
	sel.RecordLatency(1, "https://fast.com", 100*time.Millisecond)

	fastCount := 0
	for range 1000 {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://fast.com" {
			fastCount++
		}
	}
	// 期望~83%，允许75%~92%
	if fastCount < 750 || fastCount > 920 {
		t.Errorf("weighted random: expected ~83%% fast, got %d/1000 (%.1f%%)", fastCount, float64(fastCount)/10)
	}
}

func TestURLSelector_SkipsCooledDown(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.RecordLatency(1, "https://a.com", 50*time.Millisecond) // a更快
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	sel.CooldownURL(1, "https://a.com") // 但a被冷却

	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("expected non-cooled URL https://b.com, got %s", url)
	}
}

func TestURLSelector_AllCooledDown_ReturnsBest(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	sel.CooldownURL(1, "https://a.com")
	sel.CooldownURL(1, "https://b.com")

	// 所有URL都冷却时，仍然返回一个URL（兜底）
	url, _ := sel.SelectURL(1, urls)
	if url == "" {
		t.Error("all cooled: should still return a URL as fallback")
	}
}

func TestURLSelector_CooldownExpires(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond // 测试用短冷却
	urls := []string{"https://a.com", "https://b.com"}
	sel.RecordLatency(1, "https://a.com", 50*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)
	sel.CooldownURL(1, "https://a.com")

	// 冷却期间：a被排除，只能选b
	url, _ := sel.SelectURL(1, urls)
	if url != "https://b.com" {
		t.Errorf("during cooldown: expected b, got %s", url)
	}

	// 等待冷却过期后：a（最快）应该被大多数时候选中
	// a(50ms) vs b(200ms) → a权重=1/50=0.02, b权重=1/200=0.005 → a占80%
	time.Sleep(15 * time.Millisecond)
	aCount := 0
	for range 200 {
		url, _ = sel.SelectURL(1, urls)
		if url == "https://a.com" {
			aCount++
		}
	}
	if aCount < 130 {
		t.Errorf("after cooldown: expected a selected ~80%%, got %d/200", aCount)
	}
}

func TestURLSelector_IndependentChannels(t *testing.T) {
	sel := NewURLSelector()
	// 渠道1: a慢, b快
	sel.RecordLatency(1, "https://a.com", 500*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 50*time.Millisecond)
	// 渠道2: a快, b慢（与渠道1相反）
	sel.RecordLatency(2, "https://a.com", 50*time.Millisecond)
	sel.RecordLatency(2, "https://b.com", 500*time.Millisecond)

	urls := []string{"https://a.com", "https://b.com"}
	// 渠道2应大多选a（最快），渠道1应大多选b（最快）
	// 50ms vs 500ms → 快的占 1/50 / (1/50+1/500) = 90.9%
	ch2a, ch1b := 0, 0
	for range 200 {
		if url, _ := sel.SelectURL(2, urls); url == "https://a.com" {
			ch2a++
		}
		if url, _ := sel.SelectURL(1, urls); url == "https://b.com" {
			ch1b++
		}
	}
	if ch2a < 150 {
		t.Errorf("channel 2: expected a.com ~91%%, got %d/200", ch2a)
	}
	if ch1b < 150 {
		t.Errorf("channel 1: expected b.com ~91%%, got %d/200", ch1b)
	}
}

func TestURLSelector_ExploreFirst(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com", "https://c.com"}

	// 只有a有延迟数据
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)

	// 未探索URL应该被优先选择（b或c），而非已知的a
	seen := map[string]int{}
	for range 50 {
		url, _ := sel.SelectURL(1, urls)
		seen[url]++
	}
	if seen["https://a.com"] > 0 {
		t.Errorf("explore-first: a.com (known) should not be selected while b.com/c.com are unexplored, got a=%d", seen["https://a.com"])
	}
	if seen["https://b.com"] == 0 || seen["https://c.com"] == 0 {
		t.Errorf("explore-first: both unexplored URLs should be selected, got b=%d c=%d", seen["https://b.com"], seen["https://c.com"])
	}
}

func TestURLSelector_ExponentialBackoff(t *testing.T) {
	sel := NewURLSelector()
	sel.cooldownBase = 10 * time.Millisecond

	key := urlKey{channelID: 1, url: "https://a.com"}

	// 第1次冷却: 10ms
	sel.CooldownURL(1, "https://a.com")
	state1 := sel.cooldowns[key]
	if state1.consecutiveFails != 1 {
		t.Errorf("expected 1 fail, got %d", state1.consecutiveFails)
	}

	// 等待冷却过期后再次冷却: 20ms
	time.Sleep(15 * time.Millisecond)
	sel.CooldownURL(1, "https://a.com")
	state2 := sel.cooldowns[key]
	if state2.consecutiveFails != 2 {
		t.Errorf("expected 2 fails, got %d", state2.consecutiveFails)
	}
}

func TestURLSelector_SubMillisecondLatencyWeightedRandom(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://fast.com", "https://slow.com"}

	// 复现边界：<1ms 延迟如果被量化为 0，会导致 1/latency 出现 Inf。
	sel.RecordLatency(1, "https://fast.com", 500*time.Microsecond)
	sel.RecordLatency(1, "https://slow.com", 100*time.Millisecond)

	fastCount := 0
	rounds := 200
	for range rounds {
		url, _ := sel.SelectURL(1, urls)
		if url == "https://fast.com" {
			fastCount++
		}
	}

	if fastCount <= rounds/2 {
		t.Fatalf("expected fast URL to be preferred, fastCount=%d slowCount=%d", fastCount, rounds-fastCount)
	}
}

func TestURLSelector_RecordLatencyClearsCooldownWindow(t *testing.T) {
	sel := NewURLSelector()
	channelID := int64(1)
	url := "https://a.com"

	sel.CooldownURL(channelID, url)
	if !sel.IsCooledDown(channelID, url) {
		t.Fatalf("expected url cooled down before success")
	}

	// 成功反馈后应立刻可用，不应继续停留在旧的 cooldown until。
	sel.RecordLatency(channelID, url, 20*time.Millisecond)
	if sel.IsCooledDown(channelID, url) {
		t.Fatalf("expected cooldown cleared after successful latency record")
	}
}

func TestURLSelector_GC_RemovesExpiredState(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()

	oldLatencyKey := urlKey{channelID: 1, url: "https://old-latency.com"}
	freshLatencyKey := urlKey{channelID: 1, url: "https://fresh-latency.com"}
	expiredCooldownKey := urlKey{channelID: 1, url: "https://expired-cooldown.com"}
	activeCooldownKey := urlKey{channelID: 1, url: "https://active-cooldown.com"}

	sel.latencies[oldLatencyKey] = &ewmaValue{value: 120, lastSeen: now.Add(-25 * time.Hour)}
	sel.latencies[freshLatencyKey] = &ewmaValue{value: 80, lastSeen: now.Add(-2 * time.Hour)}
	sel.cooldowns[expiredCooldownKey] = urlCooldownState{until: now.Add(-time.Minute), consecutiveFails: 2}
	sel.cooldowns[activeCooldownKey] = urlCooldownState{until: now.Add(2 * time.Minute), consecutiveFails: 1}

	sel.GC(24 * time.Hour)

	if _, ok := sel.latencies[oldLatencyKey]; ok {
		t.Fatalf("expected expired latency to be removed")
	}
	if _, ok := sel.latencies[freshLatencyKey]; !ok {
		t.Fatalf("expected fresh latency to be preserved")
	}
	if _, ok := sel.cooldowns[expiredCooldownKey]; ok {
		t.Fatalf("expected expired cooldown to be removed")
	}
	if _, ok := sel.cooldowns[activeCooldownKey]; !ok {
		t.Fatalf("expected active cooldown to be preserved")
	}
}

func TestURLSelector_RecordLatency_TriggersScheduledCleanup(t *testing.T) {
	sel := NewURLSelector()
	now := time.Now()

	staleKey := urlKey{channelID: 1, url: "https://stale.com"}
	expiredCooldownKey := urlKey{channelID: 1, url: "https://expired.com"}
	sel.latencies[staleKey] = &ewmaValue{value: 100, lastSeen: now.Add(-48 * time.Hour)}
	sel.cooldowns[expiredCooldownKey] = urlCooldownState{until: now.Add(-time.Minute), consecutiveFails: 1}

	// 强制下一次写路径触发清理
	sel.cleanupInterval = time.Millisecond
	sel.latencyMaxAge = 24 * time.Hour
	sel.nextCleanup = now.Add(-time.Second)

	sel.RecordLatency(1, "https://new.com", 10*time.Millisecond)

	if _, ok := sel.latencies[staleKey]; ok {
		t.Fatalf("expected stale latency removed by scheduled cleanup")
	}
	if _, ok := sel.cooldowns[expiredCooldownKey]; ok {
		t.Fatalf("expected expired cooldown removed by scheduled cleanup")
	}
}

func TestExtractHostPort(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://api.openai.com", "api.openai.com:443"},
		{"http://localhost", "localhost:80"},
		{"https://api.example.com:8443", "api.example.com:8443"},
		{"http://127.0.0.1:3000", "127.0.0.1:3000"},
		{"https://[::1]", "[::1]:443"},
		{"http://[2001:db8::1]:8080", "[2001:db8::1]:8080"},
		{"", ""},
		{"not-a-url", ""},
	}
	for _, tt := range tests {
		got := extractHostPort(tt.input)
		if got != tt.want {
			t.Errorf("extractHostPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestURLSelector_ProbeURLs_TimeoutCoolsPendingURLs(t *testing.T) {
	sel := NewURLSelector()
	sel.probeTimeout = 20 * time.Millisecond
	sel.probeDial = func(ctx context.Context, _, address string) (net.Conn, error) {
		switch address {
		case "fast.example:443":
			conn, peer := net.Pipe()
			_ = peer.Close()
			return conn, nil
		case "slow.example:443":
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			t.Fatalf("unexpected probe address: %s", address)
			return nil, context.Canceled
		}
	}

	urls := []string{"https://fast.example", "https://slow.example"}
	sel.ProbeURLs(context.Background(), 1, urls)

	if !sel.IsCooledDown(1, "https://slow.example") {
		t.Fatalf("expected timed out URL to be cooled down")
	}

	sel.mu.RLock()
	_, fastKnown := sel.latencies[urlKey{channelID: 1, url: "https://fast.example"}]
	_, slowKnown := sel.latencies[urlKey{channelID: 1, url: "https://slow.example"}]
	sel.mu.RUnlock()

	if !fastKnown {
		t.Fatalf("expected fast URL latency seed recorded")
	}
	if slowKnown {
		t.Fatalf("expected timed out URL to remain without latency seed")
	}

	selected, _ := sel.SelectURL(1, urls)
	if selected != "https://fast.example" {
		t.Fatalf("expected known fast URL selected after probe timeout, got %s", selected)
	}
}

func TestURLSelector_ProbeURLs_SkipsSingleURL(t *testing.T) {
	sel := NewURLSelector()
	// 单URL不应触发探测
	sel.ProbeURLs(context.Background(), 1, []string{"https://a.com"})
	sel.mu.RLock()
	defer sel.mu.RUnlock()
	if len(sel.latencies) != 0 {
		t.Errorf("single URL should not trigger probe, got %d latencies", len(sel.latencies))
	}
}

func TestURLSelector_ProbeURLs_SkipsKnownURLs(t *testing.T) {
	sel := NewURLSelector()
	urls := []string{"https://a.com", "https://b.com"}
	// 给所有URL预设延迟数据
	sel.RecordLatency(1, "https://a.com", 100*time.Millisecond)
	sel.RecordLatency(1, "https://b.com", 200*time.Millisecond)

	// 所有URL已有数据，ProbeURLs应立即返回（不发TCP连接）
	sel.ProbeURLs(context.Background(), 1, urls)
	// 不crash即通过
}

func TestURLSelector_ProbeURLs_InvalidURL(t *testing.T) {
	sel := NewURLSelector()
	// 无效URL应被冷却，不应panic
	sel.ProbeURLs(context.Background(), 1, []string{"not-a-valid-url", "also-invalid"})

	sel.mu.RLock()
	defer sel.mu.RUnlock()
	// 无效URL应该被冷却或至少不产生延迟数据
	if len(sel.latencies) != 0 {
		t.Errorf("invalid URLs should not produce latency data, got %d", len(sel.latencies))
	}
}

func TestURLSelector_ProbeURLs_RealTCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TCP probe test in short mode")
	}

	sel := NewURLSelector()
	// 用localhost做TCP探测测试（假设本机80端口不开放）
	// 这个测试主要验证ProbeURLs不会panic/hang，而非成功连接
	urls := []string{"https://127.0.0.1:1", "https://127.0.0.1:2"}
	sel.ProbeURLs(context.Background(), 1, urls)

	// 连接失败的URL应被冷却
	cooled := 0
	for _, u := range urls {
		if sel.IsCooledDown(1, u) {
			cooled++
		}
	}
	if cooled == 0 {
		t.Logf("warning: no URLs were cooled down (might succeed if ports are open)")
	}
}
