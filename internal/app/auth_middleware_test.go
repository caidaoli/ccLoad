package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
)

// ============================================================================
// 认证中间件测试
// 覆盖 RequireAPIAuth 和 RequireTokenAuth 的各种认证场景
// ============================================================================

// ============================================================================
// RequireAPIAuth 测试
// ============================================================================

func TestRequireAPIAuth_BearerToken(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "sk-test-123", 0, 1) // expiresAt=0 永不过期

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-test-123")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_XAPIKey(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "key-abc", 0, 2)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", "key-abc")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_GoogleKey(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "AIza-google-key", 0, 3)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("x-goog-api-key", "AIza-google-key")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_QueryParam(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "query-key-789", 0, 4)

	req := httptest.NewRequest(http.MethodGet, "/test?key=query-key-789", nil)

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_InvalidToken(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "real-token", 0, 1)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_NoToken(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "some-token", 0, 1)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_NoConfiguredTokens(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t) // 不注入任何 token

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer any-token")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAPIAuth_ExpiredToken(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	// 设置过期时间为过去（毫秒时间戳）
	expiredAt := time.Now().Add(-time.Hour).UnixMilli()
	injectAPIToken(svc, "expired-token", expiredAt, 5)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer expired-token")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}

	// 验证响应包含 "token expired"
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["error"] != "token expired" {
		t.Fatalf("expected 'token expired' error, got: %s", resp["error"])
	}

	// 验证懒惰删除：token 应已从内存中移除
	tokenHash := model.HashToken("expired-token")
	svc.authTokensMux.RLock()
	_, stillExists := svc.authTokens[tokenHash]
	svc.authTokensMux.RUnlock()
	if stillExists {
		t.Fatal("expected expired token to be lazily deleted from memory")
	}
}

func TestRequireAPIAuth_ContextValues(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "ctx-token", 0, 42)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer ctx-token")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// 验证 token_hash 被设置到 context
	expectedHash := model.HashToken("ctx-token")
	if got, ok := resp["token_hash"].(string); !ok || got != expectedHash {
		t.Fatalf("expected token_hash=%s, got=%v", expectedHash, resp["token_hash"])
	}

	// 验证 token_id 被设置到 context
	if got, ok := resp["token_id"].(float64); !ok || int64(got) != 42 {
		t.Fatalf("expected token_id=42, got=%v", resp["token_id"])
	}
}

func TestRequireAPIAuth_LastUsedUpdate(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "lu-token", 0, 1)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer lu-token")

	_ = runMiddleware(t, svc.RequireAPIAuth(), req)

	// 验证 tokenHash 被发送到 lastUsedCh（非阻塞通道）
	expectedHash := model.HashToken("lu-token")
	select {
	case hash := <-svc.lastUsedCh:
		if hash != expectedHash {
			t.Fatalf("expected lastUsedCh to receive %s, got %s", expectedHash, hash)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected tokenHash to be sent to lastUsedCh")
	}
}

// ============================================================================
// RequireTokenAuth 测试
// ============================================================================

func TestRequireTokenAuth_ValidBearer(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAdminToken(svc, "admin-token-valid", time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer admin-token-valid")

	w := runMiddleware(t, svc.RequireTokenAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireTokenAuth_InvalidBearer(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAdminToken(svc, "admin-token", time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-admin-token")

	w := runMiddleware(t, svc.RequireTokenAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireTokenAuth_MissingHeader(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAdminToken(svc, "admin-token", time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	w := runMiddleware(t, svc.RequireTokenAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireTokenAuth_ExpiredToken(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAdminToken(svc, "admin-expired", time.Now().Add(-time.Second))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer admin-expired")

	w := runMiddleware(t, svc.RequireTokenAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}

	// 验证过期 token 已从内存中删除
	tokenHash := model.HashToken("admin-expired")
	svc.tokensMux.RLock()
	_, stillExists := svc.validTokens[tokenHash]
	svc.tokensMux.RUnlock()
	if stillExists {
		t.Fatal("expected expired admin token to be deleted from memory")
	}
}

func TestRequireTokenAuth_NoBearerPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAdminToken(svc, "admin-token", time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "admin-token") // 没有 Bearer 前缀

	w := runMiddleware(t, svc.RequireTokenAuth(), req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequireAPIAuth_TokenPriority 验证 token 提取优先级（Bearer > X-API-Key > x-goog-api-key > query）
func TestRequireAPIAuth_TokenPriority(t *testing.T) {
	t.Parallel()
	svc := newTestAuthService(t)
	injectAPIToken(svc, "bearer-token", 0, 1)

	// 同时设置 Bearer 和 X-API-Key，Bearer 应优先
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bearer-token")
	req.Header.Set("X-API-Key", "wrong-key")

	w := runMiddleware(t, svc.RequireAPIAuth(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (Bearer should take priority), got %d: %s", w.Code, w.Body.String())
	}
}
