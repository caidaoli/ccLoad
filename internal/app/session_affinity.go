package app

import (
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// sessionAffinityTTL 与 Anthropic 最长的 1h prompt cache 对齐：超过它缓存已失效，绑定没有价值。
const sessionAffinityTTL = time.Hour

// sessionAffinityTarget 是会话上次成功落到的渠道与 Key。
type sessionAffinityTarget struct {
	channelID int64
	keyIndex  int
}

type sessionAffinityEntry struct {
	target    sessionAffinityTarget
	expiresAt time.Time
}

// sessionAffinityStore 把客户端会话绑定到上次成功的渠道/Key。
// prompt cache 按上游组织隔离：同层轮询每轮换账号，整段上下文都要按写缓存价重写。
// 绑定只在成功后写入，因此条目增长受真实上游请求速率约束，过期条目由后台循环回收。
type sessionAffinityStore struct {
	mu      sync.Mutex
	entries map[string]sessionAffinityEntry
}

func newSessionAffinityStore() *sessionAffinityStore {
	return &sessionAffinityStore{entries: make(map[string]sessionAffinityEntry)}
}

func (s *sessionAffinityStore) lookup(key string, now time.Time) (sessionAffinityTarget, bool) {
	if s == nil || key == "" {
		return sessionAffinityTarget{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return sessionAffinityTarget{}, false
	}
	return entry.target, true
}

// bind 覆盖写入并续期：绑定渠道冷却或失败后，会话随下一次成功迁移到新渠道。
func (s *sessionAffinityStore) bind(key string, target sessionAffinityTarget, now time.Time) {
	if s == nil || key == "" || target.channelID <= 0 {
		return
	}
	s.mu.Lock()
	s.entries[key] = sessionAffinityEntry{target: target, expiresAt: now.Add(sessionAffinityTTL)}
	s.mu.Unlock()
}

func (s *sessionAffinityStore) cleanupExpired(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
}

// anthropicSessionAffinityKey 按令牌隔离 Claude Code 会话；没有会话标识时不绑定，
// 不用消息内容哈希去猜会话。
func anthropicSessionAffinityKey(tokenHash string, headers http.Header, body []byte) string {
	if tokenHash == "" {
		return ""
	}
	sessionID := anthropicSessionIDFromHeaders(headers)
	if sessionID == "" {
		sessionID = anthropicSessionIDFromBody(body)
	}
	if sessionID == "" {
		return ""
	}
	return tokenHash + "\x00" + sessionID
}

// preferSessionAffinityChannel 仅在绑定渠道仍属候选中最高配置优先级时置顶：
// 粘性只替代同层轮询，更高优先级渠道恢复后会话随之迁移，主备意图优先于缓存命中。
func preferSessionAffinityChannel(cands []*model.Config, channelID int64) []*model.Config {
	var bound *model.Config
	for _, cfg := range cands {
		if cfg != nil && cfg.ID == channelID {
			bound = cfg
			break
		}
	}
	if bound == nil {
		return cands
	}
	for _, cfg := range cands {
		if cfg != nil && cfg.Priority > bound.Priority {
			return cands
		}
	}
	return prioritizePinnedChannel(cands, channelID)
}

// selectSessionAffinityKey 复用会话绑定的 Key，前提是它可用且处于最高可用 Key 优先级档。
func selectSessionAffinityKey(apiKeys []*model.APIKey, triedKeys map[int]bool, keyIndex int) (int, string, bool) {
	now := time.Now()
	var bound *model.APIKey
	topPriority := 0
	found := false
	for _, apiKey := range apiKeys {
		if apiKey == nil || apiKey.Disabled || triedKeys[apiKey.KeyIndex] || apiKey.IsCoolingDown(now) {
			continue
		}
		if !found || apiKey.Priority > topPriority {
			topPriority = apiKey.Priority
			found = true
		}
		if apiKey.KeyIndex == keyIndex {
			bound = apiKey
		}
	}
	if bound == nil || bound.Priority < topPriority {
		return 0, "", false
	}
	return bound.KeyIndex, bound.APIKey, true
}
