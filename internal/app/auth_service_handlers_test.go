package app

import (
	"context"
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
		c, w := mkCtx(http.MethodPost, []byte(`{"password":"nope"}`))
		svc.HandleLogin(c)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	var token string
	t.Run("success login", func(t *testing.T) {
		c, w := mkCtx(http.MethodPost, []byte(`{"password":"pass"}`))
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
		if _, exists, err := store.GetAdminSession(ctx, token); err != nil || !exists {
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

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, exists, err := store.GetAdminSession(ctx, token); err != nil || exists {
			t.Fatalf("expected session removed from DB: exists=%v err=%v", exists, err)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		// 连续失败超过 maxAttempts(5) 后，第6次应返回 429
		for i := 0; i < 5; i++ {
			c, w := mkCtx(http.MethodPost, []byte(`{"password":"nope"}`))
			svc.HandleLogin(c)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status=%d, want %d", i+1, w.Code, http.StatusUnauthorized)
			}
		}
		c, w := mkCtx(http.MethodPost, []byte(`{"password":"nope"}`))
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
		svc.validTokens[expiredHash] = time.Now().Add(-time.Second)
		svc.validTokens[validHash] = time.Now().Add(1 * time.Hour)
		svc.tokensMux.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.CreateAdminSession(ctx, expiredPlain, time.Now().Add(-time.Hour))
		_ = store.CreateAdminSession(ctx, validPlain, time.Now().Add(1*time.Hour))

		svc.CleanExpiredTokens()

		svc.tokensMux.RLock()
		_, expiredStill := svc.validTokens[expiredHash]
		_, validStill := svc.validTokens[validHash]
		svc.tokensMux.RUnlock()
		if expiredStill || !validStill {
			t.Fatalf("unexpected memory tokens: expired=%v valid=%v", expiredStill, validStill)
		}

		sessions, err := store.LoadAllSessions(ctx)
		if err != nil {
			t.Fatalf("LoadAllSessions failed: %v", err)
		}
		if _, ok := sessions[expiredHash]; ok {
			t.Fatalf("expected expired session removed from DB")
		}
	})
}
