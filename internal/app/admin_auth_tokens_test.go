package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestAuthToken_MaskToken(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		expected string
	}{
		{
			name:     "Long token",
			token:    "sk-ant-1234567890abcdefghijklmnop",
			expected: "sk-a****mnop",
		},
		{
			name:     "Short token",
			token:    "short",
			expected: "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			masked := model.MaskToken(tt.token)
			if masked != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, masked)
			}
		})
	}
}

func TestAdminAPI_CreateAuthToken_Basic(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/auth-tokens", map[string]any{
		"description": "Test Token",
	}))

	server.HandleCreateAuthToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		} `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)

	if !response.Success || len(response.Data.Token) == 0 {
		t.Error("Token creation failed")
	}

	ctx := context.Background()
	stored, err := server.store.GetAuthToken(ctx, response.Data.ID)
	if err != nil {
		t.Fatalf("DB error: %v", err)
	}

	expectedHash := model.HashToken(response.Data.Token)
	if stored.Token != expectedHash {
		t.Error("Hash mismatch")
	}
}

func TestAdminAPI_ListAuthTokens_ResponseShape(t *testing.T) {
	server := newInMemoryServer(t)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/auth-tokens", nil))

	server.HandleListAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	type listResp struct {
		Tokens  []*model.AuthToken `json:"tokens"`
		IsToday bool               `json:"is_today"`
	}
	resp := mustParseAPIResponse[listResp](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data.Tokens == nil {
		t.Fatalf("tokens is null, want []")
	}
}
