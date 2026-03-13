package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleChannelURLStats_NilSelectorReturnsEmpty(t *testing.T) {
	srv := newInMemoryServer(t)

	cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name:         "url-stats-nil-selector",
		URL:          "https://a.example\nhttps://b.example",
		Priority:     1,
		ChannelType:  "anthropic",
		ModelEntries: []model.ModelEntry{{Model: "claude-sonnet-4-20250514"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	srv.urlSelector = nil

	target := fmt.Sprintf("/admin/channels/%d/url-stats", cfg.ID)
	c, w := newTestContext(t, newRequest(http.MethodGet, target, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", cfg.ID)}}

	srv.HandleChannelURLStats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[[]URLStat](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("expected success=true, resp=%+v", resp)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("expected empty stats when selector is nil, got %+v", resp.Data)
	}
}
