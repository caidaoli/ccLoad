package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestAdminSettingsHandlers(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	origRestartFunc := RestartFunc
	defer func() {
		RestartFunc = origRestartFunc
	}()

	restartCh := make(chan struct{}, 10)
	RestartFunc = func() { restartCh <- struct{}{} }

	t.Run("AdminGetSetting_missing_key", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings/", nil))

		server.AdminGetSetting(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminGetSetting_not_found", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings/no_such_key", nil))
		c.Params = gin.Params{{Key: "key", Value: "no_such_key"}}

		server.AdminGetSetting(c)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("AdminGetSetting_ok", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings/log_retention_days", nil))
		c.Params = gin.Params{{Key: "key", Value: "log_retention_days"}}

		server.AdminGetSetting(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
		}

		resp := mustParseAPIResponse[*model.SystemSetting](t, w.Body.Bytes())
		if !resp.Success {
			t.Fatalf("success=false, error=%q", resp.Error)
		}
		if resp.Data == nil {
			t.Fatalf("data is nil, want SystemSetting")
		}
		if resp.Data.Key != "log_retention_days" {
			t.Fatalf("data.key=%v, want log_retention_days", resp.Data.Key)
		}
	})

	t.Run("AdminUpdateSetting_invalid_json", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/log_retention_days", []byte("{")))
		c.Params = gin.Params{{Key: "key", Value: "log_retention_days"}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminUpdateSetting_not_found", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/no_such_key", []byte(`{"value":"1"}`)))
		c.Params = gin.Params{{Key: "key", Value: "no_such_key"}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("AdminUpdateSetting_invalid_value", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/log_retention_days", []byte(`{"value":"0"}`)))
		c.Params = gin.Params{{Key: "key", Value: "log_retention_days"}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminUpdateSetting_ok_triggers_restart", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/log_retention_days", []byte(`{"value":"30"}`)))
		c.Params = gin.Params{{Key: "key", Value: "log_retention_days"}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		select {
		case <-restartCh:
		case <-time.After(1 * time.Second):
			t.Fatal("expected restart triggered")
		}
	})

	t.Run("AdminGetSetting_returns_latest_db_value_before_restart", func(t *testing.T) {
		if err := store.UpdateSetting(context.Background(), "channel_check_interval_hours", "1"); err != nil {
			t.Fatalf("failed to seed setting in db: %v", err)
		}

		seed, err := store.GetSetting(context.Background(), "channel_check_interval_hours")
		if err != nil {
			t.Fatalf("failed to read seeded setting: %v", err)
		}
		seed.Value = "1"

		server.configService.mu.Lock()
		server.configService.cache["channel_check_interval_hours"] = seed
		server.configService.mu.Unlock()

		updateCtx, updateW := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/channel_check_interval_hours", []byte(`{"value":"0"}`)))
		updateCtx.Params = gin.Params{{Key: "key", Value: "channel_check_interval_hours"}}

		server.AdminUpdateSetting(updateCtx)

		if updateW.Code != http.StatusOK {
			t.Fatalf("update status=%d, want %d body=%s", updateW.Code, http.StatusOK, updateW.Body.String())
		}

		select {
		case <-restartCh:
		case <-time.After(1 * time.Second):
			t.Fatal("expected restart triggered")
		}

		getCtx, getW := newTestContext(t, newRequest(http.MethodGet, "/admin/settings/channel_check_interval_hours", nil))
		getCtx.Params = gin.Params{{Key: "key", Value: "channel_check_interval_hours"}}

		server.AdminGetSetting(getCtx)

		if getW.Code != http.StatusOK {
			t.Fatalf("get status=%d, want %d body=%s", getW.Code, http.StatusOK, getW.Body.String())
		}

		resp := mustParseAPIResponse[*model.SystemSetting](t, getW.Body.Bytes())
		if !resp.Success {
			t.Fatalf("success=false, error=%q", resp.Error)
		}
		if resp.Data == nil {
			t.Fatal("data is nil, want SystemSetting")
		}
		if resp.Data.Value != "0" {
			t.Fatalf("data.value=%q, want 0", resp.Data.Value)
		}
	})

	t.Run("AdminResetSetting_ok_triggers_restart", func(t *testing.T) {
		// 先更新为一个不同值，再reset，最后验证数据库里变回默认值。
		if err := store.UpdateSetting(context.Background(), "log_retention_days", "30"); err != nil {
			t.Fatalf("UpdateSetting failed: %v", err)
		}

		defaultValue := server.configService.GetSetting("log_retention_days").DefaultValue

		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/settings/log_retention_days/reset", nil))
		c.Params = gin.Params{{Key: "key", Value: "log_retention_days"}}

		server.AdminResetSetting(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		select {
		case <-restartCh:
		case <-time.After(1 * time.Second):
			t.Fatal("expected restart triggered")
		}

		s, err := store.GetSetting(context.Background(), "log_retention_days")
		if err != nil {
			t.Fatalf("GetSetting failed: %v", err)
		}
		if s.Value != defaultValue {
			t.Fatalf("value after reset=%q, want default=%q", s.Value, defaultValue)
		}
	})

	t.Run("AdminBatchUpdateSettings_empty_body_reject", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/settings/batch", []byte(`{}`)))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminBatchUpdateSettings_unknown_key_reject", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/settings/batch", []byte(`{"no_such_key":"1"}`)))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminBatchUpdateSettings_invalid_value_reject", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/settings/batch", []byte(`{"log_retention_days":"0"}`)))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("AdminBatchUpdateSettings_ok_triggers_restart", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/admin/settings/batch", []byte(`{"log_retention_days":"14","max_key_retries":"5"}`)))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		select {
		case <-restartCh:
		case <-time.After(1 * time.Second):
			t.Fatal("expected restart triggered")
		}
	})
}
