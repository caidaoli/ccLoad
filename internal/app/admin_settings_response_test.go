package app

import (
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestAdminAPI_ListSettings_ResponseShape(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.configService = NewConfigService(store)

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings", nil))

	server.AdminListSettings(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	resp := mustParseAPIResponse[[]*model.SystemSetting](t, w.Body.Bytes())
	if !resp.Success {
		t.Fatalf("success=false, error=%q", resp.Error)
	}
	if resp.Data == nil {
		t.Fatalf("data is null, want []")
	}
}
