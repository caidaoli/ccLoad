package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

func TestAuthService_LoginLogoutAndCleanup(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)

	svc := NewAuthService("pass", limiter, store)
	t.Cleanup(svc.Close)
	revoked := make(map[string]int)
	svc.RegisterWebSessionRevokeHook(func(sessionHash string) {
		revoked[sessionHash]++
	})

	mkCtx := func(method string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
		req := newJSONRequestBytes(method, "/admin/login", body)
		req.RemoteAddr = "1.2.3.4:1234"
		return newTestContext(t, req)
	}

	t.Run("invalid request", func(t *testing.T) {
		c, w := mkCtx(http.MethodPost, []byte(`{}`))
		svc.HandleLogin(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		c, w := mkCtx(http.MethodPost, []byte(`{"mode":"admin","password":"nope"}`))
		svc.HandleLogin(c)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	var token string
	t.Run("success login", func(t *testing.T) {
		c, w := mkCtx(http.MethodPost, []byte(`{"mode":"admin","password":"pass"}`))
		svc.HandleLogin(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Token     string `json:"token"`
				ExpiresIn int    `json:"expiresIn"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if !resp.Success || resp.Data.Token == "" || resp.Data.ExpiresIn <= 0 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		token = resp.Data.Token

		// 内存中应可验证
		if !svc.isValidToken(token) {
			t.Fatalf("expected token valid in memory")
		}

		// 数据库中应存在会话
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, exists, err := store.GetWebSession(ctx, token); err != nil || !exists {
			t.Fatalf("expected session in DB: exists=%v err=%v", exists, err)
		}
	})

	t.Run("logout", func(t *testing.T) {
		req := newRequest(http.MethodPost, "/admin/logout", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		c, w := newTestContext(t, req)

		svc.HandleLogout(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if svc.isValidToken(token) {
			t.Fatalf("expected token invalid after logout")
		}
		if got := revoked[model.HashToken(token)]; got != 1 {
			t.Fatalf("logout revoke hook calls=%d, want 1", got)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, exists, err := store.GetWebSession(ctx, token); err != nil || exists {
			t.Fatalf("expected session removed from DB: exists=%v err=%v", exists, err)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		// 连续失败超过 maxAttempts(5) 后，第6次应返回 429
		for i := range 5 {
			c, w := mkCtx(http.MethodPost, []byte(`{"mode":"admin","password":"nope"}`))
			svc.HandleLogin(c)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status=%d, want %d", i+1, w.Code, http.StatusUnauthorized)
			}
		}
		c, w := mkCtx(http.MethodPost, []byte(`{"mode":"admin","password":"nope"}`))
		svc.HandleLogin(c)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusTooManyRequests)
		}
	})

	t.Run("CleanExpiredTokens clears memory and DB", func(t *testing.T) {
		expiredPlain := "expired"
		validPlain := "valid"
		expiredHash := model.HashToken(expiredPlain)
		validHash := model.HashToken(validPlain)

		svc.tokensMux.Lock()
		svc.validTokens[expiredHash] = model.WebSession{TokenHash: expiredHash, Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(-time.Second)}
		svc.validTokens[validHash] = model.WebSession{TokenHash: validHash, Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(time.Hour)}
		svc.tokensMux.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.CreateWebSession(ctx, expiredPlain, model.WebSession{Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(-time.Hour)})
		_ = store.CreateWebSession(ctx, validPlain, model.WebSession{Role: model.WebRoleAdmin, ExpiresAt: time.Now().Add(time.Hour)})

		svc.CleanExpiredTokens()

		svc.tokensMux.RLock()
		_, expiredStill := svc.validTokens[expiredHash]
		_, validStill := svc.validTokens[validHash]
		svc.tokensMux.RUnlock()
		if expiredStill || !validStill {
			t.Fatalf("unexpected memory tokens: expired=%v valid=%v", expiredStill, validStill)
		}
		if got := revoked[expiredHash]; got != 1 {
			t.Fatalf("expiry revoke hook calls=%d, want 1", got)
		}
		if got := revoked[validHash]; got != 0 {
			t.Fatalf("valid session revoke hook calls=%d, want 0", got)
		}

		sessions, err := store.LoadWebSessions(ctx)
		if err != nil {
			t.Fatalf("LoadWebSessions failed: %v", err)
		}
		if _, ok := sessions[expiredHash]; ok {
			t.Fatalf("expected expired session removed from DB")
		}
	})
}

func TestAdminLoginIPv6PrefixRateLimit(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	svc := NewAuthService("pass", limiter, store)
	t.Cleanup(svc.Close)
	login := func(address, password string, want int) {
		t.Helper()
		w := runLoginHandler(t, svc, fmt.Sprintf(`{"mode":"admin","password":%q}`, password), address)
		if w.Code != want {
			t.Fatalf("login from %s: status=%d want=%d body=%s", address, w.Code, want, w.Body.String())
		}
	}
	// Rotating addresses within one /64 must not replenish the attempt budget.
	for i := 1; i <= 5; i++ {
		login(fmt.Sprintf("[2001:db8:1:2::%x]:1234", i), "wrong", http.StatusUnauthorized)
	}
	login("[2001:db8:1:2::ffff]:1234", "wrong", http.StatusTooManyRequests)
	login("[2001:0db8:0001:0002:0:0:0:abcd]:1234", "wrong", http.StatusTooManyRequests)
	login("[2001:db8:1:3::1]:1234", "wrong", http.StatusUnauthorized)
	// IPv4 addresses remain independent; mapped IPv4 has the same identity.
	for range 5 {
		login("192.0.2.1:1234", "wrong", http.StatusUnauthorized)
	}
	login("[::ffff:192.0.2.1]:1234", "wrong", http.StatusTooManyRequests)
	login("192.0.2.2:1234", "wrong", http.StatusUnauthorized)
	login("[::ffff:192.0.2.3]:1234", "wrong", http.StatusUnauthorized)
	// Successful password login clears the shared prefix's previous failures.
	for i := 1; i <= 4; i++ {
		login(fmt.Sprintf("[2001:db8:2:1::%x]:1234", i), "wrong", http.StatusUnauthorized)
	}
	login("[2001:db8:2:1::5]:1234", "pass", http.StatusOK)
	for i := 6; i <= 10; i++ {
		login(fmt.Sprintf("[2001:db8:2:1::%x]:1234", i), "wrong", http.StatusUnauthorized)
	}
	login("[2001:db8:2:1::b]:1234", "wrong", http.StatusTooManyRequests)
}
