package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestProxyGemini_ListModelsHandlers(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:        "g1",
		URL:         "https://example.com",
		Priority:    1,
		Enabled:     true,
		ChannelType: "gemini",
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-flash-20250101"},
			{Model: "gemini-1.5-pro"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig gemini failed: %v", err)
	}
	_, err = store.CreateConfig(ctx, &model.Config{
		Name:        "o1",
		URL:         "https://example.com",
		Priority:    1,
		Enabled:     true,
		ChannelType: "openai",
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-4o"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig openai failed: %v", err)
	}

	t.Run("handleListGeminiModels", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Models) != 2 {
			t.Fatalf("models len=%d, want 2", len(resp.Models))
		}
		for _, m := range resp.Models {
			if m.Name == "" || m.DisplayName == "" {
				t.Fatalf("bad model entry: %+v", m)
			}
			if m.Name[:7] != "models/" {
				t.Fatalf("expected gemini name prefix models/, got %q", m.Name)
			}
		}
	})

	t.Run("handleListOpenAIModels", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Object != "list" || len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("handleListOpenAIModels filters by token allowed models", func(t *testing.T) {
		server.authService = newTestAuthService(t)
		tokenHash := model.HashToken("restricted-openai-token")
		server.authService.authTokensMux.Lock()
		server.authService.authTokenModels[tokenHash] = []string{"gpt-4o"}
		server.authService.authTokensMux.Unlock()

		_, err := store.CreateConfig(ctx, &model.Config{
			Name:        "o2",
			URL:         "https://example.com",
			Priority:    2,
			Enabled:     true,
			ChannelType: "openai",
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-5"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig openai extra failed: %v", err)
		}

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))
		c.Set("token_hash", tokenHash)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
			t.Fatalf("unexpected filtered resp: %+v", resp)
		}
	})

	t.Run("handleListOpenAIModels includes transformed gemini channel", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:               "g2-oai",
			URL:                "https://example.com",
			Priority:           3,
			Enabled:            true,
			ChannelType:        "gemini",
			ProtocolTransforms: []string{"openai"},
			ModelEntries: []model.ModelEntry{
				{Model: "gemini-2.5-pro"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig transformed gemini failed: %v", err)
		}

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		found := false
		for _, item := range resp.Data {
			if item.ID == "gemini-2.5-pro" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected transformed gemini model in openai model list, got %+v", resp.Data)
		}
	})

	t.Run("handleListGeminiModels ignores unsupported legacy gemini transforms", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:               "o2-gemini",
			URL:                "https://example.com",
			Priority:           4,
			Enabled:            true,
			ChannelType:        "openai",
			ProtocolTransforms: []string{"gemini"},
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-4.1"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig transformed openai failed: %v", err)
		}

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		for _, item := range resp.Models {
			if item.Name == "models/gpt-4.1" {
				t.Fatalf("unsupported legacy gemini transform should not be exposed, got %+v", resp.Models)
			}
		}
	})

	t.Run("handleListOpenAIModels returns anthropic style for anthropic view", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:               "g3-anthropic",
			URL:                "https://example.com",
			Priority:           5,
			Enabled:            true,
			ChannelType:        "gemini",
			ProtocolTransforms: []string{"anthropic"},
			ModelEntries: []model.ModelEntry{
				{Model: "claude-3-5-sonnet"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig transformed anthropic failed: %v", err)
		}

		req := newRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("anthropic-version", "2023-06-01")
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				Type        string `json:"type"`
				CreatedAt   string `json:"created_at"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.HasMore {
			t.Fatalf("expected has_more=false, got true")
		}
		if len(resp.Data) == 0 {
			t.Fatalf("expected anthropic models, got empty response")
		}
		found := false
		for _, item := range resp.Data {
			if item.ID == "claude-3-5-sonnet" {
				found = true
				if item.Type != "model" {
					t.Fatalf("expected anthropic type=model, got %q", item.Type)
				}
				if item.DisplayName == "" || item.CreatedAt == "" {
					t.Fatalf("expected anthropic display_name/created_at, got %+v", item)
				}
				break
			}
		}
		if !found {
			t.Fatalf("expected transformed anthropic model in anthropic view, got %+v", resp.Data)
		}
		if resp.FirstID == "" || resp.LastID == "" {
			t.Fatalf("expected anthropic pagination ids, got first=%q last=%q", resp.FirstID, resp.LastID)
		}
	})

	t.Run("handleListOpenAIModels keeps openai shape for codex view", func(t *testing.T) {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:        "c1",
			URL:         "https://example.com",
			Priority:    6,
			Enabled:     true,
			ChannelType: "codex",
			ModelEntries: []model.ModelEntry{
				{Model: "gpt-5-codex"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig codex failed: %v", err)
		}

		req := newRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("User-Agent", "codex-cli/1.0")
		c, w := newTestContext(t, req)

		server.handleListOpenAIModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if resp.Object != "list" {
			t.Fatalf("expected openai-style list object for codex view, got %+v", resp)
		}
		found := false
		for _, item := range resp.Data {
			if item.ID == "gpt-5-codex" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected codex model in openai-shaped view, got %+v", resp.Data)
		}
	})

	t.Run("handleListGeminiModels filters by token allowed models", func(t *testing.T) {
		server.authService = newTestAuthService(t)
		tokenHash := model.HashToken("restricted-gemini-token")
		server.authService.authTokensMux.Lock()
		server.authService.authTokenModels[tokenHash] = []string{"gemini-1.5-pro"}
		server.authService.authTokensMux.Unlock()

		c, w := newTestContext(t, newRequest(http.MethodGet, "/v1beta/models", nil))
		c.Set("token_hash", tokenHash)

		server.handleListGeminiModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
		if len(resp.Models) != 1 || resp.Models[0].Name != "models/gemini-1.5-pro" {
			t.Fatalf("unexpected filtered resp: %+v", resp)
		}
	})
}
