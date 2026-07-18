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

	s.AddCostToCache("t1", 0)
	s.AddCostToCache("t1", -1)
	s.AddCostToCache("missing", 100)
	s.AddCostToCache("t1", 60)

	used, limit, exceeded = s.IsCostLimitExceeded("t1")
	if used != 110 || limit != 100 || !exceeded {
		t.Fatalf("t1 after add: got (%d,%d,%v), want (110,100,true)", used, limit, exceeded)
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
	s.AddCostToCache(hash, 50)
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
