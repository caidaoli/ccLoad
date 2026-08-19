package app

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestAuthService_GenerateToken_LengthAndHex(t *testing.T) {
	t.Parallel()

	s := &AuthService{}
	token, err := s.generateToken()
	if err != nil {
		t.Fatalf("generateToken failed: %v", err)
	}
	if len(token) != config.TokenRandomBytes*2 {
		t.Fatalf("token length=%d, want %d", len(token), config.TokenRandomBytes*2)
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token should be hex: %v", err)
	}
}

func TestAuthService_IsValidToken_ExpiryAndDeletion(t *testing.T) {
	token := "t" // 明文token仅用于hash查找
	tokenHash := model.HashToken(token)

	s := &AuthService{
		validTokens: make(map[string]model.WebSession),
	}
	var revoked []string
	s.RegisterWebSessionRevokeHook(func(sessionHash string) {
		revoked = append(revoked, sessionHash)
	})

	s.tokensMux.Lock()
	s.validTokens[tokenHash] = model.WebSession{TokenHash: tokenHash, Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(-time.Second)}
	s.tokensMux.Unlock()

	if s.isValidToken(token) {
		t.Fatal("expected expired token invalid")
	}
	s.tokensMux.RLock()
	_, stillExists := s.validTokens[tokenHash]
	s.tokensMux.RUnlock()
	if stillExists {
		t.Fatal("expected expired token to be deleted from cache")
	}
	if len(revoked) != 1 || revoked[0] != tokenHash {
		t.Fatalf("expiry hook=%v, want [%s]", revoked, tokenHash)
	}
	if s.isValidToken(token) || len(revoked) != 1 {
		t.Fatalf("expired session notified more than once: %v", revoked)
	}

	s.tokensMux.Lock()
	s.validTokens[tokenHash] = model.WebSession{TokenHash: tokenHash, Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(time.Hour)}
	s.tokensMux.Unlock()
	if !s.isValidToken(token) {
		t.Fatal("expected unexpired token valid")
	}

	if s.isValidToken("missing") {
		t.Fatal("expected missing token invalid")
	}
}

func TestAuthService_RevokeHookRunsOutsideServiceLocks(t *testing.T) {
	t.Parallel()
	token := "lock-check-session"
	tokenHash := model.HashToken(token)
	s := &AuthService{validTokens: map[string]model.WebSession{
		tokenHash: {TokenHash: tokenHash, Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(-time.Second)},
	}}
	called := false
	s.RegisterWebSessionRevokeHook(func(sessionHash string) {
		called = true
		if sessionHash != tokenHash {
			t.Errorf("hook hash=%q, want %q", sessionHash, tokenHash)
		}
		if !s.tokensMux.TryLock() {
			t.Error("revoke hook ran while session lock was held")
		} else {
			s.tokensMux.Unlock()
		}
		s.RegisterWebSessionRevokeHook(func(string) {})
	})

	if s.isValidToken(token) || !called {
		t.Fatalf("expired session valid=%v hook called=%v", s.isValidToken(token), called)
	}
}

func TestAuthService_IsModelAllowed(t *testing.T) {
	t.Parallel()

	s := &AuthService{
		authTokenModels: map[string][]string{
			"t1": {"GPT-4", "claude"},
		},
	}

	if !s.IsModelAllowed("no_restriction", "anything") {
		t.Fatal("expected allow when no restriction")
	}
	if !s.IsModelAllowed("t1", "gpt-4") {
		t.Fatal("expected case-insensitive allow")
	}
	if s.IsModelAllowed("t1", "gemini") {
		t.Fatal("expected reject for non-allowed model")
	}
}

func TestAuthService_IsChannelAllowed(t *testing.T) {
	t.Parallel()

	s := &AuthService{
		authTokenChannels: map[string]model.ChannelRestriction{
			"allow1": mustChannelRestriction(t, model.ChannelRestrictionModeAllow, 2, 42),
			"deny1":  mustChannelRestriction(t, model.ChannelRestrictionModeDeny, 2, 42),
		},
	}

	if !s.IsChannelAllowed("no_restriction", 99) {
		t.Fatal("expected allow when no channel restriction")
	}
	if !s.IsChannelAllowed("allow1", 42) {
		t.Fatal("expected listed channel to be allowed in allow mode")
	}
	if s.IsChannelAllowed("allow1", 7) {
		t.Fatal("expected non-listed channel to be rejected in allow mode")
	}
	if s.IsChannelAllowed("deny1", 42) {
		t.Fatal("expected listed channel to be rejected in deny mode")
	}
	if !s.IsChannelAllowed("deny1", 7) {
		t.Fatal("expected non-listed channel to be allowed in deny mode")
	}
}

func TestAuthService_CostLimit(t *testing.T) {
	t.Parallel()

	s := &AuthService{
		authTokenCostLimits: map[string]tokenCostLimit{
			"t1": {usedMicroUSD: 50, limitMicroUSD: 100},
			"t0": {usedMicroUSD: 50, limitMicroUSD: 0},
		},
	}

	used, limit, exceeded := s.IsCostLimitExceeded("missing")
	if used != 0 || limit != 0 || exceeded {
		t.Fatalf("missing: got (%d,%d,%v), want (0,0,false)", used, limit, exceeded)
	}

	used, limit, exceeded = s.IsCostLimitExceeded("t0")
	if used != 0 || limit != 0 || exceeded {
		t.Fatalf("unlimited: got (%d,%d,%v), want (0,0,false)", used, limit, exceeded)
	}

	used, limit, exceeded = s.IsCostLimitExceeded("t1")
	if used != 50 || limit != 100 || exceeded {
		t.Fatalf("t1 before add: got (%d,%d,%v), want (50,100,false)", used, limit, exceeded)
	}

	s.AddCostToCache("t1", 0, time.Now())
	s.AddCostToCache("t1", -1, time.Now())
	s.AddCostToCache("missing", 100, time.Now())
	s.AddCostToCache("t1", 60, time.Now())

	used, limit, exceeded = s.IsCostLimitExceeded("t1")
	if used != 110 || limit != 100 || !exceeded {
		t.Fatalf("t1 after add: got (%d,%d,%v), want (110,100,true)", used, limit, exceeded)
	}
}

func TestAuthService_PeriodCostLimits(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dayStart, monthStart := model.AuthTokenCostPeriodStarts(now)
	yesterdayAt := now.AddDate(0, 0, -1)
	yesterday, _ := model.AuthTokenCostPeriodStarts(yesterdayAt)

	s := &AuthService{
		authTokenCostLimits: map[string]tokenCostLimit{
			"daily": {
				dailyUsedMicroUSD:  90,
				dailyLimitMicroUSD: 100,
				dailyPeriodStart:   dayStart,
			},
			"stale-day": {
				dailyUsedMicroUSD:  90,
				dailyLimitMicroUSD: 100,
				dailyPeriodStart:   yesterday,
			},
			"late-old-day": {
				dailyUsedMicroUSD:  50,
				dailyLimitMicroUSD: 100,
				dailyPeriodStart:   dayStart,
			},
		},
	}

	used, limit, exceeded := s.IsCostLimitExceeded("daily")
	if used != 90 || limit != 100 || exceeded {
		t.Fatalf("daily before add: got (%d,%d,%v), want (90,100,false)", used, limit, exceeded)
	}

	s.AddCostToCache("daily", 20, now)
	used, limit, exceeded = s.IsCostLimitExceeded("daily")
	if used != 110 || limit != 100 || !exceeded {
		t.Fatalf("daily after add: got (%d,%d,%v), want (110,100,true)", used, limit, exceeded)
	}

	used, limit, exceeded = s.IsCostLimitExceeded("stale-day")
	if used != 0 || limit != 100 || exceeded {
		t.Fatalf("stale day should not count yesterday usage: got (%d,%d,%v), want (0,100,false)", used, limit, exceeded)
	}
	s.AddCostToCache("stale-day", 20, now)
	used, limit, exceeded = s.IsCostLimitExceeded("stale-day")
	if used != 20 || limit != 100 || exceeded {
		t.Fatalf("stale day after add: got (%d,%d,%v), want (20,100,false)", used, limit, exceeded)
	}

	s.AddCostToCache("late-old-day", 20, yesterdayAt)
	used, limit, exceeded = s.IsCostLimitExceeded("late-old-day")
	if used != 50 || limit != 100 || exceeded {
		t.Fatalf("late old-period cost polluted current day: got (%d,%d,%v), want (50,100,false)", used, limit, exceeded)
	}

	s.authTokenCostLimits["monthly"] = tokenCostLimit{
		monthlyUsedMicroUSD:  50,
		monthlyLimitMicroUSD: 60,
		monthlyPeriodStart:   monthStart,
	}
	s.AddCostToCache("monthly", 10, now)
	used, limit, exceeded = s.IsCostLimitExceeded("monthly")
	if used != 60 || limit != 60 || !exceeded {
		t.Fatalf("monthly after add: got (%d,%d,%v), want (60,60,true)", used, limit, exceeded)
	}
}

func TestReloadAuthTokens_DoesNotRegressPeriodUsage(t *testing.T) {
	t.Parallel()

	const hash = "period-token-hash"
	now := time.Now()
	dayStart, monthStart := model.AuthTokenCostPeriodStarts(now)

	stub := &reloadStubStore{
		tokens: []*model.AuthToken{{
			Token:                    hash,
			ID:                       1,
			CostDailyUsedMicroUSD:    40,
			CostDailyLimitMicroUSD:   100,
			CostDailyPeriodStart:     dayStart,
			CostMonthlyUsedMicroUSD:  80,
			CostMonthlyLimitMicroUSD: 200,
			CostMonthlyPeriodStart:   monthStart,
			MaxConcurrency:           1,
		}},
	}

	s := newTestAuthService(t)
	s.store = stub
	if err := s.ReloadAuthTokens(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	s.AddCostToCache(hash, 15, now)
	if err := s.ReloadAuthTokens(); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	used, limit, exceeded := s.IsCostLimitExceeded(hash)
	if used != 55 || limit != 100 || exceeded {
		t.Fatalf("same-period reload: got (%d,%d,%v), want (55,100,false)", used, limit, exceeded)
	}
}

func TestReloadAuthTokens_ResetsStalePeriodUsage(t *testing.T) {
	t.Parallel()

	const hash = "stale-period-token-hash"
	now := time.Now()
	yesterday, _ := model.AuthTokenCostPeriodStarts(now.AddDate(0, 0, -1))

	stub := &reloadStubStore{
		tokens: []*model.AuthToken{{
			Token:                  hash,
			ID:                     1,
			CostDailyUsedMicroUSD:  90,
			CostDailyLimitMicroUSD: 100,
			CostDailyPeriodStart:   yesterday,
			MaxConcurrency:         1,
		}},
	}

	s := newTestAuthService(t)
	s.store = stub
	if err := s.ReloadAuthTokens(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	used, limit, exceeded := s.IsCostLimitExceeded(hash)
	if used != 0 || limit != 100 || exceeded {
		t.Fatalf("stale-period reload: got (%d,%d,%v), want (0,100,false)", used, limit, exceeded)
	}
}

// reloadStubStore 仅覆盖 ListActiveAuthTokens，用于模拟 DB 返回值。
// 其余 storage.Store 方法未实现（嵌入 nil 接口），ReloadAuthTokens 不会调用它们。
type reloadStubStore struct {
	storage.Store
	tokens []*model.AuthToken
}

func (s *reloadStubStore) ListActiveAuthTokens(_ context.Context) ([]*model.AuthToken, error) {
	return s.tokens, nil
}

// TestReloadAuthTokens_DoesNotRegressUsage 复现 P0-1：
// AddCostToCache 只更新内存，DB 由 UpdateTokenStats 异步落盘。
// 在落盘窗口内触发 ReloadAuthTokens 时，不得用 DB 滞后值覆盖内存实时累加，否则限额被绕过。
func TestReloadAuthTokens_DoesNotRegressUsage(t *testing.T) {
	t.Parallel()

	const hash = "p0-token-hash" // DB 中存哈希；ReloadAuthTokens 直接将其作为内存 map key
	stub := &reloadStubStore{
		tokens: []*model.AuthToken{{
			Token:             hash,
			ID:                1,
			CostUsedMicroUSD:  100, // DB 落盘值（滞后）
			CostLimitMicroUSD: 1000,
			MaxConcurrency:    1,
		}},
	}

	s := newTestAuthService(t)
	s.store = stub

	// 初次加载：内存与 DB 一致
	if err := s.ReloadAuthTokens(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if used, _, _ := s.IsCostLimitExceeded(hash); used != 100 {
		t.Fatalf("after initial reload used=%d, want 100", used)
	}

	// 请求完成 → 内存累加 +50；此时 DB 仍滞后为 100（stub 返回值不变）
	s.AddCostToCache(hash, 50, time.Now())
	if used, _, _ := s.IsCostLimitExceeded(hash); used != 150 {
		t.Fatalf("after AddCostToCache used=%d, want 150", used)
	}

	// 落盘窗口内再次 reload：DB 仍返回 100，内存累加不得被覆盖
	if err := s.ReloadAuthTokens(); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if used, _, _ := s.IsCostLimitExceeded(hash); used != 150 {
		t.Fatalf("reload regressed in-memory usage: used=%d, want 150 (DB lagging value must not overwrite memory)", used)
	}
}

func TestAuthService_AcquireTokenConcurrencySlot(t *testing.T) {
	t.Parallel()

	s := &AuthService{
		authTokenMaxConns: map[string]int{
			"limited": 1,
		},
		authTokenActiveReqs: make(map[string]int),
	}

	release, active, limit, ok := s.acquireTokenConcurrencySlot("unlimited")
	if !ok || active != 1 || limit != 0 {
		t.Fatalf("unlimited got active=%d limit=%d ok=%v, want 1,0,true", active, limit, ok)
	}
	if got := s.authTokenActiveReqs["unlimited"]; got != 1 {
		t.Fatalf("unlimited active reqs=%d, want 1", got)
	}
	release()
	if _, exists := s.authTokenActiveReqs["unlimited"]; exists {
		t.Fatal("expected unlimited token active reqs to be cleaned after release")
	}

	release, active, limit, ok = s.acquireTokenConcurrencySlot("limited")
	if !ok || active != 1 || limit != 1 {
		t.Fatalf("first acquire got active=%d limit=%d ok=%v, want 1,1,true", active, limit, ok)
	}

	_, active, limit, ok = s.acquireTokenConcurrencySlot("limited")
	if ok || active != 1 || limit != 1 {
		t.Fatalf("second acquire got active=%d limit=%d ok=%v, want 1,1,false", active, limit, ok)
	}

	release()

	release, active, limit, ok = s.acquireTokenConcurrencySlot("limited")
	if !ok || active != 1 || limit != 1 {
		t.Fatalf("after release got active=%d limit=%d ok=%v, want 1,1,true", active, limit, ok)
	}
	release()
}
