package app

import (
	"testing"
	"time"
)

// TestInvalidateChannelListCache_ClearsChannelTypesCache 验证 P1-5：
// 渠道 CRUD 调用 InvalidateChannelListCache 时一并清 channelTypesCache，
// 避免 admin 改动后 60s TTL 内的脏读（read-after-write 一致性）。
func TestInvalidateChannelListCache_ClearsChannelTypesCache(t *testing.T) {
	t.Parallel()

	s := &Server{}
	// 预置缓存（模拟已缓存的渠道类型映射）
	s.channelTypesCacheMu.Lock()
	s.channelTypesCache = map[int64]string{1: "anthropic", 2: "openai"}
	s.channelTypesCacheTime = time.Now()
	s.channelTypesCacheMu.Unlock()

	// 空 Server 下 getChannelCache 返回 nil、channelBalancer 为 nil，调用安全
	s.InvalidateChannelListCache()

	s.channelTypesCacheMu.RLock()
	defer s.channelTypesCacheMu.RUnlock()
	if s.channelTypesCache != nil {
		t.Fatalf("InvalidateChannelListCache 后 channelTypesCache 应被清空, got %v", s.channelTypesCache)
	}
}
