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
}
