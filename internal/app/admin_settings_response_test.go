package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdminAPI_ListSettings_ResponseShape(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.configService = NewConfigService(store)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/settings", nil)

	server.AdminListSettings(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if resp["success"] != true {
		t.Fatalf("Expected success=true, got %v", resp["success"])
	}

	data := resp["data"]
	if data == nil {
		t.Fatalf("Expected data to be [], got null")
	}
	if _, ok := data.([]any); !ok {
		t.Fatalf("Expected data to be array, got %T", data)
	}
}
