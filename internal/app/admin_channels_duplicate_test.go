package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestHandleCheckDuplicateChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:         "existing-channel",
		ChannelType:  "anthropic",
		URL:          "https://api.anthropic.com/v1\nhttps://backup.anthropic.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "claude-3-5-sonnet"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	// parseResp 解析包装在 APIResponse 中的 CheckDuplicateResponse
	parseResp := func(t *testing.T, body []byte) CheckDuplicateResponse {
		t.Helper()
		var wrapped APIResponse[CheckDuplicateResponse]
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		return wrapped.Data
	}

	t.Run("no duplicate - different type", func(t *testing.T) {
		body, _ := json.Marshal(CheckDuplicateRequest{
			ChannelType: "openai",
			URLs:        []string{"https://api.anthropic.com/v1"},
		})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := parseResp(t, w.Body.Bytes())
		if len(resp.Duplicates) != 0 {
			t.Fatalf("expected 0 duplicates, got %d", len(resp.Duplicates))
		}
	})

	t.Run("no duplicate - different url", func(t *testing.T) {
		body, _ := json.Marshal(CheckDuplicateRequest{
			ChannelType: "anthropic",
			URLs:        []string{"https://other.example.com"},
		})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := parseResp(t, w.Body.Bytes())
		if len(resp.Duplicates) != 0 {
			t.Fatalf("expected 0 duplicates, got %d", len(resp.Duplicates))
		}
	})

	t.Run("duplicate - first url matches", func(t *testing.T) {
		body, _ := json.Marshal(CheckDuplicateRequest{
			ChannelType: "anthropic",
			URLs:        []string{"https://api.anthropic.com/v1"},
		})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := parseResp(t, w.Body.Bytes())
		if len(resp.Duplicates) != 1 {
			t.Fatalf("expected 1 duplicate, got %d", len(resp.Duplicates))
		}
		if resp.Duplicates[0].Name != "existing-channel" {
			t.Fatalf("expected name=existing-channel, got %s", resp.Duplicates[0].Name)
		}
	})

	t.Run("duplicate - second url matches", func(t *testing.T) {
		body, _ := json.Marshal(CheckDuplicateRequest{
			ChannelType: "anthropic",
			URLs:        []string{"https://backup.anthropic.com"},
		})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := parseResp(t, w.Body.Bytes())
		if len(resp.Duplicates) != 1 {
			t.Fatalf("expected 1 duplicate, got %d", len(resp.Duplicates))
		}
	})

	t.Run("duplicate - same channel only reported once", func(t *testing.T) {
		body, _ := json.Marshal(CheckDuplicateRequest{
			ChannelType: "anthropic",
			URLs:        []string{"https://api.anthropic.com/v1", "https://backup.anthropic.com"},
		})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		resp := parseResp(t, w.Body.Bytes())
		if len(resp.Duplicates) != 1 {
			t.Fatalf("expected 1 duplicate (same channel reported once), got %d", len(resp.Duplicates))
		}
	})

	t.Run("bad request - missing channel_type", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"urls": []string{"https://example.com"}})
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/check-duplicate", bytes.NewReader(body)))
		server.HandleCheckDuplicateChannel(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})
}
