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

func TestAPITokenLoginCreatesScopedWebSession(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	plainToken := "sk-dashboard-owner"
	authToken := &model.AuthToken{
		Token:         model.HashToken(plainToken),
		Description:   "dashboard owner",
		CreatedAt:     time.Now(),
		IsActive:      true,
		AllowedModels: []string{"gpt-5.6"},
	}
	if err := store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	svc := NewAuthService("admin-pass", limiter, store)
	t.Cleanup(svc.Close)

	req := newJSONRequestBytes(http.MethodPost, "/login", []byte(`{"mode":"api_token","token":"sk-dashboard-owner"}`))
	req.RemoteAddr = "1.2.3.4:1234"
	c, w := newTestContext(t, req)
	svc.HandleLogin(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	var data struct {
		Token     string        `json:"token"`
		ExpiresIn int           `json:"expiresIn"`
		Role      model.WebRole `json:"role"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Token == "" || data.ExpiresIn <= 0 || data.Role != model.WebRoleAPIToken {
		t.Fatalf("unexpected login data: %+v", data)
	}
	if data.Token == plainToken {
		t.Fatal("login returned the plaintext API token instead of a web session")
	}

	session, exists, err := store.GetWebSession(context.Background(), data.Token)
	if err != nil || !exists {
		t.Fatalf("persisted session exists=%v err=%v", exists, err)
	}
	if session.Role != model.WebRoleAPIToken || session.AuthTokenID != authToken.ID {
		t.Fatalf("persisted identity=(%q,%d), want=(%q,%d)", session.Role, session.AuthTokenID, model.WebRoleAPIToken, authToken.ID)
	}

	authW := runWebAuthMiddleware(t, svc.RequireWebAuth(), data.Token)
	if authW.Code != http.StatusOK {
		t.Fatalf("web auth status=%d, want 200: %s", authW.Code, authW.Body.String())
	}
	var identity WebIdentity
	mustUnmarshalJSON(t, authW.Body.Bytes(), &identity)
	if identity.Role != model.WebRoleAPIToken || identity.AuthTokenID != authToken.ID {
		t.Fatalf("context identity=(%q,%d), want=(%q,%d)", identity.Role, identity.AuthTokenID, model.WebRoleAPIToken, authToken.ID)
	}

	sessionReq := newRequest(http.MethodGet, "/dashboard/session", nil)
	sessionCtx, sessionW := newTestContext(t, sessionReq)
	sessionCtx.Set(webIdentityContextKey, identity)
	svc.HandleWebSession(sessionCtx)
	if sessionW.Code != http.StatusOK {
		t.Fatalf("session status=%d, want 200: %s", sessionW.Code, sessionW.Body.String())
	}
	var sessionData struct {
		Role          model.WebRole `json:"role"`
		Description   string        `json:"description"`
		AllowedModels []string      `json:"allowed_models"`
		Token         string        `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, sessionW.Body.Bytes(), &sessionData)
	if sessionData.Role != model.WebRoleAPIToken || sessionData.Description != "dashboard owner" {
		t.Fatalf("unexpected session data: %+v", sessionData)
	}
	if len(sessionData.AllowedModels) != 1 || sessionData.AllowedModels[0] != "gpt-5.6" {
		t.Fatalf("allowed models=%v, want [gpt-5.6]", sessionData.AllowedModels)
	}
	if sessionData.Token != "" {
		t.Fatal("session endpoint exposed token material")
	}
}

func TestAPITokenLoginAcceptsStoredTokenHash(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	authToken := &model.AuthToken{
		Token:       model.HashToken("sk-hash-login"),
		Description: "hash login",
		CreatedAt:   time.Now(),
		IsActive:    true,
	}
	if err := store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	svc := NewAuthService("admin-pass", limiter, store)
	t.Cleanup(svc.Close)

	body := []byte(`{"mode":"api_token","token":"` + authToken.Token + `"}`)
	req := newJSONRequestBytes(http.MethodPost, "/login", body)
	req.RemoteAddr = "1.2.3.4:1234"
	c, w := newTestContext(t, req)
	svc.HandleLogin(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	var data struct {
		Token string        `json:"token"`
		Role  model.WebRole `json:"role"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Token == "" || data.Role != model.WebRoleAPIToken {
		t.Fatalf("unexpected login data: %+v", data)
	}
}

func TestAPITokenWebSessionCannotUseAdminMiddleware(t *testing.T) {
	svc := newTestAuthService(t)
	plainSession := "readonly-session"
	svc.validTokens[model.HashToken(plainSession)] = model.WebSession{
		TokenHash:   model.HashToken(plainSession),
		Role:        model.WebRoleAPIToken,
		AuthTokenID: 9,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	injectAPIToken(svc, "backing-token", 0, 9)

	w := runWebAuthMiddleware(t, svc.RequireAdminAuth(), plainSession)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestAPITokenChannelRoutesAreReadOnly(t *testing.T) {
	server := newInMemoryServer(t)
	plainSession := "readonly-channel-session"
	sessionHash := model.HashToken(plainSession)
	server.authService.validTokens[sessionHash] = model.WebSession{
		TokenHash:   sessionHash,
		Role:        model.WebRoleAPIToken,
		AuthTokenID: 9,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	injectAPIToken(server.authService, "backing-channel-token", 0, 9)

	router := gin.New()
	server.SetupRoutes(router)

	getRequest := httptest.NewRequest(http.MethodGet, "/dashboard/channels", nil)
	getRequest.Header.Set("Authorization", "Bearer "+plainSession)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/channels status=%d, want 200: %s", getResponse.Code, getResponse.Body.String())
	}

	postRequest := httptest.NewRequest(http.MethodPost, "/admin/channels", nil)
	postRequest.Header.Set("Authorization", "Bearer "+plainSession)
	postResponse := httptest.NewRecorder()
	router.ServeHTTP(postResponse, postRequest)
	if postResponse.Code != http.StatusForbidden {
		t.Fatalf("POST /admin/channels status=%d, want 403: %s", postResponse.Code, postResponse.Body.String())
	}
}

func TestAPITokenWebSessionAttachesProxyIdentity(t *testing.T) {
	svc := newTestAuthService(t)
	plainSession := "proxy-web-session"
	sessionHash := model.HashToken(plainSession)
	svc.validTokens[sessionHash] = model.WebSession{
		TokenHash:   sessionHash,
		Role:        model.WebRoleAPIToken,
		AuthTokenID: 17,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	injectAPIToken(svc, "backing-proxy-token", 0, 17)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.GET("/test", svc.RequireWebAuth(), svc.RequireWebAPITokenProxyAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"token_hash": c.GetString("token_hash"),
			"token_id":   c.GetInt64("token_id"),
		})
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+plainSession)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		TokenHash string `json:"token_hash"`
		TokenID   int64  `json:"token_id"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &got)
	if got.TokenHash != model.HashToken("backing-proxy-token") || got.TokenID != 17 {
		t.Fatalf("proxy identity=(%q,%d), want backing hash and 17", got.TokenHash, got.TokenID)
	}
}

func TestWebAuthRejectsUnknownPersistedRole(t *testing.T) {
	svc := newTestAuthService(t)
	plainSession := "invalid-role-session"
	hash := model.HashToken(plainSession)
	svc.validTokens[hash] = model.WebSession{
		TokenHash: hash,
		Role:      model.WebRole("unexpected"),
		ExpiresAt: time.Now().Add(time.Hour),
	}

	w := runWebAuthMiddleware(t, svc.RequireWebAuth(), plainSession)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401: %s", w.Code, w.Body.String())
	}
}

func TestReloadAuthTokensRevokesPersistedWebSessions(t *testing.T) {
	_, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	plainToken := "sk-revoked-owner"
	authToken := &model.AuthToken{
		Token:       model.HashToken(plainToken),
		Description: "revoked owner",
		CreatedAt:   time.Now(),
		IsActive:    true,
	}
	if err := store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("create auth token: %v", err)
	}

	limiter := util.NewLoginRateLimiter()
	t.Cleanup(limiter.Stop)
	svc := NewAuthService("admin-pass", limiter, store)
	t.Cleanup(svc.Close)

	req := newJSONRequestBytes(http.MethodPost, "/login", []byte(`{"mode":"api_token","token":"sk-revoked-owner"}`))
	req.RemoteAddr = "1.2.3.4:1234"
	c, w := newTestContext(t, req)
	svc.HandleLogin(c)
	var login struct {
		Token string `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &login)

	authToken.IsActive = false
	if err := store.UpdateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("disable auth token: %v", err)
	}
	if err := svc.ReloadAuthTokens(); err != nil {
		t.Fatalf("reload auth tokens: %v", err)
	}

	if _, exists, err := store.GetWebSession(context.Background(), login.Token); err != nil || exists {
		t.Fatalf("revoked persisted session exists=%v err=%v, want false,nil", exists, err)
	}
	w = runWebAuthMiddleware(t, svc.RequireWebAuth(), login.Token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked web session status=%d, want 401", w.Code)
	}
}

func runWebAuthMiddleware(t testing.TB, middleware gin.HandlerFunc, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.GET("/test", middleware, func(c *gin.Context) {
		identity, ok := WebIdentityFromContext(c)
		if !ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, identity)
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	engine.ServeHTTP(w, req)
	return w
}
