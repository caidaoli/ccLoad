package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleUpdateAuthToken(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	// 只需要支持 ReloadAuthTokens 的最小实例
	server.authService = &AuthService{store: store}

	ctx := context.Background()
	expiresAt := time.Now().Add(24 * time.Hour).UnixMilli()
	token := &model.AuthToken{
		Token:         model.HashToken("plain-token"),
		Description:   "old",
		ExpiresAt:     nil,
		IsActive:      true,
		AllowedModels: []string{"old-model"},
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}

	t.Run("invalid id", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/auth-tokens/abc", []byte(`{}`)))
		c.Params = gin.Params{{Key: "id", Value: "abc"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/auth-tokens/1", []byte(`{`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("negative cost limit", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/auth-tokens/1", []byte(`{"cost_limit_usd":-1}`)))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("not found", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/auth-tokens/999", []byte(`{"allowed_models":[]}`)))
		c.Params = gin.Params{{Key: "id", Value: "999"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("success", func(t *testing.T) {
		body := map[string]any{
			"description":         "new-desc",
			"is_active":           false,
			"expires_at":          expiresAt,
			"allowed_models":      []string{"m1", "m2"},
			"allowed_channel_ids": []int64{11, 22},
			"cost_limit_usd":      1.5,
			"unknown_ignored":     "x",
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/auth-tokens/1", body))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		type respData struct {
			Description       string  `json:"description"`
			IsActive          bool    `json:"is_active"`
			Token             string  `json:"token"`
			ExpiresAt         *int64  `json:"expires_at,omitempty"`
			CostLimitUSD      float64 `json:"cost_limit_usd"`
			AllowedChannelIDs []int64 `json:"allowed_channel_ids"`
		}
		resp := mustParseAPIResponse[respData](t, w.Body.Bytes())
		if !resp.Success {
			t.Fatalf("success=false, error=%q", resp.Error)
		}
		if resp.Data.Description != "new-desc" {
			t.Fatalf("description=%v, want %q", resp.Data.Description, "new-desc")
		}
		if resp.Data.IsActive {
			t.Fatalf("is_active=%v, want false", resp.Data.IsActive)
		}
		if resp.Data.Token != model.HashToken("plain-token") {
			t.Fatalf("token should be hash value for dual-path auth, got %q", resp.Data.Token)
		}
		if resp.Data.ExpiresAt == nil || *resp.Data.ExpiresAt != expiresAt {
			t.Fatalf("expiresAt=%v, want %d", resp.Data.ExpiresAt, expiresAt)
		}
		if resp.Data.CostLimitUSD < 1.49 || resp.Data.CostLimitUSD > 1.51 {
			t.Fatalf("cost_limit_usd=%v, want ~1.5", resp.Data.CostLimitUSD)
		}
		if len(resp.Data.AllowedChannelIDs) != 2 || resp.Data.AllowedChannelIDs[0] != 11 || resp.Data.AllowedChannelIDs[1] != 22 {
			t.Fatalf("allowed_channel_ids=%v, want [11 22]", resp.Data.AllowedChannelIDs)
		}

		updated, err := store.GetAuthToken(ctx, token.ID)
		if err != nil {
			t.Fatalf("GetAuthToken failed: %v", err)
		}
		if updated.Description != "new-desc" || updated.IsActive {
			t.Fatalf("db state mismatch: desc=%q active=%v", updated.Description, updated.IsActive)
		}
		if updated.ExpiresAt == nil || *updated.ExpiresAt != expiresAt {
			t.Fatalf("expiresAt=%v, want %d", updated.ExpiresAt, expiresAt)
		}
		if updated.CostLimitMicroUSD != 1_500_000 {
			t.Fatalf("CostLimitMicroUSD=%d, want %d", updated.CostLimitMicroUSD, 1_500_000)
		}
		if len(updated.AllowedModels) != 2 {
			t.Fatalf("AllowedModels=%v, want 2 items", updated.AllowedModels)
		}
		if len(updated.AllowedChannelIDs) != 2 || updated.AllowedChannelIDs[0] != 11 || updated.AllowedChannelIDs[1] != 22 {
			t.Fatalf("AllowedChannelIDs=%v, want [11 22]", updated.AllowedChannelIDs)
		}
	})

	t.Run("preserve allowed models and channels when fields omitted", func(t *testing.T) {
		token2 := &model.AuthToken{
			Token:             model.HashToken("plain-token-2"),
			Description:       "keep-models",
			IsActive:          true,
			AllowedModels:     []string{"keep-a", "keep-b"},
			AllowedChannelIDs: []int64{101, 202},
		}
		if err := store.CreateAuthToken(ctx, token2); err != nil {
			t.Fatalf("CreateAuthToken token2 failed: %v", err)
		}

		body := map[string]any{
			"description": "keep-models-updated",
			"is_active":   false,
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/auth-tokens/2", body))
		c.Params = gin.Params{{Key: "id", Value: "2"}}

		server.HandleUpdateAuthToken(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		updated, err := store.GetAuthToken(ctx, token2.ID)
		if err != nil {
			t.Fatalf("GetAuthToken token2 failed: %v", err)
		}
		if updated.Description != "keep-models-updated" || updated.IsActive {
			t.Fatalf("db state mismatch: desc=%q active=%v", updated.Description, updated.IsActive)
		}
		if len(updated.AllowedModels) != 2 || updated.AllowedModels[0] != "keep-a" || updated.AllowedModels[1] != "keep-b" {
			t.Fatalf("AllowedModels=%v, want preserved values", updated.AllowedModels)
		}
		if len(updated.AllowedChannelIDs) != 2 || updated.AllowedChannelIDs[0] != 101 || updated.AllowedChannelIDs[1] != 202 {
			t.Fatalf("AllowedChannelIDs=%v, want preserved values", updated.AllowedChannelIDs)
		}
	})
}

func TestHandleDeleteAuthToken(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.authService = &AuthService{store: store}

	ctx := context.Background()
	token := &model.AuthToken{
		Token:       model.HashToken("plain-token"),
		Description: "to-delete",
		IsActive:    true,
	}
	if err := store.CreateAuthToken(ctx, token); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/auth-tokens/1", nil))
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	server.HandleDeleteAuthToken(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	type deleteResp struct {
		ID int64 `json:"id"`
	}
	resp := mustParseAPIResponse[deleteResp](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.ID != 1 {
		t.Fatalf("id=%d, want 1", resp.Data.ID)
	}

	if _, err := store.GetAuthToken(ctx, token.ID); err == nil {
		t.Fatalf("expected token deleted from DB")
	}
}
