package app

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

// sessionAffinityTTL 与 Anthropic 最长的 1h prompt cache 对齐：超过它缓存已失效，绑定没有价值。
// Codex 会话共用同一 TTL。
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

// codexSessionAffinityKey 按令牌隔离 Codex 线程：要求有 Session-Id，键取 Thread-Id
// （全局唯一，主/子代理各自独立），缺省时取 Session-Id（根线程两者相同）。按线程而非
// Session-Id+Thread-Id 组键，guardian 才能只凭父线程 ID 找到父线程的绑定。body 里的
// prompt_cache_key 只是缓存路由提示，不作会话标识。键带协议段，与 Anthropic 会话键不相交。
func codexSessionAffinityKey(tokenHash string, headers http.Header) string {
	sessionID := strings.TrimSpace(headers.Get("Session-Id"))
	if sessionID == "" {
		return ""
	}
	threadID := strings.TrimSpace(headers.Get("Thread-Id"))
	if threadID == "" {
		threadID = sessionID
	}
	return codexThreadAffinityKey(tokenHash, threadID)
}

func codexThreadAffinityKey(tokenHash, threadID string) string {
	if tokenHash == "" {
		return ""
	}
	return tokenHash + "\x00codex\x00" + threadID
}

// lookupCodexSessionAffinity 返回 Codex 请求的绑定键与请求开始时的绑定。guardian 审批
// 子代理（X-OpenAI-Subagent: guardian）审查父线程的计划动作，审查模型与父线程不同；
// 自身还没有绑定时沿用 X-Codex-Parent-Thread-Id 所指父线程的渠道与 Key，复用同一账号的
// prompt cache（对齐 CLIProxyAPI 2f6f2924）。只读父绑定、不为其续期，成功后只写自己的键；
// 普通子代理仍独立选路。
func (s *Server) lookupCodexSessionAffinity(tokenHash string, headers http.Header, now time.Time) (string, sessionAffinityTarget, bool) {
	key := codexSessionAffinityKey(tokenHash, headers)
	if target, ok := s.sessionAffinity.lookup(key, now); ok {
		return key, target, true
	}
	if key == "" || !strings.EqualFold(strings.TrimSpace(headers.Get("X-OpenAI-Subagent")), "guardian") {
		return key, sessionAffinityTarget{}, false
	}
	parentThreadID := strings.TrimSpace(headers.Get("X-Codex-Parent-Thread-Id"))
	if parentThreadID == "" {
		return key, sessionAffinityTarget{}, false
	}
	target, ok := s.sessionAffinity.lookup(codexThreadAffinityKey(tokenHash, parentThreadID), now)
	return key, target, ok
}

// preferSessionAffinityChannel 仅在绑定渠道仍属候选中最高优先级档时置顶：
// 健康评分开启时按有效分的 0.1 档比较，否则按配置优先级比较。
// 粘性只替代同层轮询，更高优先级渠道恢复后会话随之迁移，主备意图优先于缓存命中。
func (s *Server) preferSessionAffinityChannel(cands []*model.Config, channelID int64) []*model.Config {
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
	healthCfg := model.HealthScoreConfig{}
	var snapshot *channelHealthSnapshot
	if s.healthCache != nil {
		healthCfg = s.healthCache.Config()
		if healthCfg.Enabled {
			snapshot = s.healthCache.snapshot.Load()
		}
	}
	priorityGroup := func(cfg *model.Config) int64 {
		if healthCfg.Enabled {
			return effPriorityBucket(snapshot.effectivePriority(cfg, healthCfg))
		}
		return int64(cfg.Priority)
	}
	boundGroup := priorityGroup(bound)
	for _, cfg := range cands {
		if cfg != nil && priorityGroup(cfg) > boundGroup {
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
