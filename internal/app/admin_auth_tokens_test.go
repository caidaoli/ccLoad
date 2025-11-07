package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
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
	server, cleanup := setupTestServer(t)
	defer cleanup()

	requestBody := map[string]any{
		"description": "Test Token",
	}

	body, _ := json.Marshal(requestBody)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/auth-tokens", bytes.NewBuffer(body))
	c.Request.Header.Set("Content-Type", "application/json")

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
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

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
