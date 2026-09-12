package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestNormalizeOpenCodeGoUsageProjectsOfficialWindows(t *testing.T) {
	t.Parallel()
	if _, err := normalizeOpenCodeGoUsage(&opencodeGoUsagePayload{}); err == nil {
		t.Fatal("normalizeOpenCodeGoUsage() expected an error for empty windows")
	}
	payload := &opencodeGoUsagePayload{}
	payload.Usage.Rolling = &opencodeGoUsageWindow{Status: "ok", Percent: 19.5, ResetsAt: "2026-09-12T06:38:42.393Z"}
	payload.Usage.Weekly = &opencodeGoUsageWindow{Status: "ok", Percent: 29.7, ResetsAt: "2026-09-14T00:00:00.393Z"}
	payload.Usage.Monthly = &opencodeGoUsageWindow{Status: "ok", Percent: 25, ResetsAt: "2026-10-11T01:58:47.393Z"}
	summary, err := normalizeOpenCodeGoUsage(payload)
	if err != nil {
		t.Fatalf("normalizeOpenCodeGoUsage() error = %v", err)
	}
	if summary.Provider != opencodeGoUsageProvider || summary.PlanType != "go" || len(summary.Windows) != 3 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.Windows[0].LimitName != "five_hour" || summary.Windows[0].Kind != "rolling" ||
		summary.Windows[0].UsedPercent != 19.5 || summary.Windows[0].RemainingPercent != 80.5 ||
		summary.Windows[0].LimitWindowSeconds != opencodeGoRollingSeconds ||
		summary.Windows[0].ResetAt != time.Date(2026, time.September, 12, 6, 38, 42, 393000000, time.UTC).Unix() {
		t.Fatalf("rolling window = %#v", summary.Windows[0])
	}
	if summary.Windows[1].LimitName != "weekly" || summary.Windows[1].UsedPercent != 29.7 {
		t.Fatalf("weekly window = %#v", summary.Windows[1])
	}
	if summary.Windows[2].LimitName != "monthly" || summary.Windows[2].UsedPercent != 25 {
		t.Fatalf("monthly window = %#v", summary.Windows[2])
	}
}

func TestHandleOAuthUsageReturnsOpenCodeGoQuotaWithoutLeakingKey(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	ctx := context.Background()
	channel, err := store.CreateConfig(ctx, &model.Config{
		Name: "OpenCode Go", AuthType: model.AuthTypeAPIKey,
		URLs:    model.ChannelURLs{{URL: "https://opencode.ai/zen/go"}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "glm-5.3-flash"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: channel.ID, APIKey: "sk-opencode-secret",
	}}); err != nil {
		t.Fatal(err)
	}
	var sawUsageRequest bool
	server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != opencodeHost || request.URL.Path != opencodeGoUsagePath {
			t.Fatalf("usage URL = %s", request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer sk-opencode-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != opencodeGoUsageUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		sawUsageRequest = true
		body := `{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":"2026-09-12T06:38:42Z"},"weekly":{"status":"ok","percent":40,"resetsAt":"2026-09-14T00:00:00Z"},"monthly":{"status":"ok","percent":8,"resetsAt":"2026-10-11T01:58:47Z"}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}

	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", w.Code, w.Body.String())
	}
	if !sawUsageRequest {
		t.Fatal("OpenCode Go usage endpoint was not called")
	}
	if strings.Contains(w.Body.String(), "sk-opencode-secret") {
		t.Fatalf("usage response leaked credential: %s", w.Body.String())
	}
	response := mustParseAPIResponse[oauthUsageSummary](t, w.Body.Bytes())
	if response.Data.Provider != opencodeGoUsageProvider || response.Data.PlanType != "go" || len(response.Data.Windows) != 3 {
		t.Fatalf("usage summary = %#v", response.Data)
	}
	if response.Data.Windows[0].UsedPercent != 12.5 || response.Data.Windows[1].UsedPercent != 40 {
		t.Fatalf("windows = %#v", response.Data.Windows)
	}
}

func TestHandleOAuthUsageRejectsNonOpenCodeAPIKeyChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	channel, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "generic", AuthType: model.AuthTypeAPIKey,
		URLs:    model.ChannelURLs{{URL: "https://example.com"}},
		Enabled: true, ModelEntries: []model.ModelEntry{{Model: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, w := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/oauth-usage", channel.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleOAuthUsage(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
