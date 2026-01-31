package app

import (
	"encoding/hex"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
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
		validTokens: make(map[string]time.Time),
	}

	s.tokensMux.Lock()
	s.validTokens[tokenHash] = time.Now().Add(-time.Second)
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
	s.validTokens[tokenHash] = time.Now().Add(time.Hour)
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
