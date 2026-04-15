package app

import (
	"net/http"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleGetDebugLog_NotFoundIncludesRelevantSettings(t *testing.T) {
	srv := newInMemoryServer(t)

	if err := srv.store.UpdateSetting(t.Context(), "debug_log_enabled", "false"); err != nil {
		t.Fatalf("update debug_log_enabled: %v", err)
	}
	if err := srv.store.UpdateSetting(t.Context(), "debug_log_retention_minutes", "15"); err != nil {
		t.Fatalf("update debug_log_retention_minutes: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/debug-logs/123", nil))
	c.Params = gin.Params{{Key: "log_id", Value: "123"}}

	srv.HandleGetDebugLog(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
	}

	type unavailableData struct {
		Reason                   string               `json:"reason"`
		DebugLogEnabled          *model.SystemSetting `json:"debug_log_enabled"`
		DebugLogRetentionMinutes *model.SystemSetting `json:"debug_log_retention_minutes"`
	}

	resp := mustParseAPIResponse[unavailableData](t, w.Body.Bytes())
	if resp.Success {
		t.Fatalf("success=%v, want false", resp.Success)
	}
	if resp.Error != "debug log unavailable" {
		t.Fatalf("error=%q, want %q", resp.Error, "debug log unavailable")
	}
	if resp.Data.Reason != "debug_log_not_found" {
		t.Fatalf("reason=%q, want %q", resp.Data.Reason, "debug_log_not_found")
	}
	if resp.Data.DebugLogEnabled == nil {
		t.Fatal("debug_log_enabled should be returned")
	}
	if resp.Data.DebugLogEnabled.Key != "debug_log_enabled" || resp.Data.DebugLogEnabled.Value != "false" {
		t.Fatalf("debug_log_enabled=%+v, want key/value debug_log_enabled/false", resp.Data.DebugLogEnabled)
	}
	if resp.Data.DebugLogRetentionMinutes == nil {
		t.Fatal("debug_log_retention_minutes should be returned")
	}
	if resp.Data.DebugLogRetentionMinutes.Key != "debug_log_retention_minutes" || resp.Data.DebugLogRetentionMinutes.Value != "15" {
		t.Fatalf("debug_log_retention_minutes=%+v, want key/value debug_log_retention_minutes/15", resp.Data.DebugLogRetentionMinutes)
	}
}
