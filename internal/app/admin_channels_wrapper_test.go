package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestAdminChannelsWrappers(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	// 创建一个渠道供 GET 使用
	cfg, err := store.CreateConfig(context.Background(), &model.Config{
		Name:         "ch",
		URL:          "https://api.example.com",
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "m1"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	t.Run("HandleChannels_GET", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))

		server.HandleChannels(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("HandleChannels_method_not_allowed", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodPut, "/admin/channels", nil))

		server.HandleChannels(c)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusMethodNotAllowed, w.Body.String())
		}
	})

	t.Run("HandleChannelByID_invalid_id", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/abc", nil))
		c.Params = gin.Params{{Key: "id", Value: "abc"}}

		server.HandleChannelByID(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("HandleChannelByID_GET", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleChannelByID(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("HandleChannelByID_method_not_allowed", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/1", nil))
		c.Params = gin.Params{{Key: "id", Value: "1"}}

		server.HandleChannelByID(c)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusMethodNotAllowed, w.Body.String())
		}
	})

	// 防止未使用变量（cfg用于确保ID为1的存在性）
	_ = cfg
}
