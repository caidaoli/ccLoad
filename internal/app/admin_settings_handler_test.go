package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestServerRestartFuncIsConcurrentAndInstanceScoped(t *testing.T) {
	first := &Server{}
	second := &Server{}
	var firstCalls atomic.Int64
	var secondCalls atomic.Int64

	var firstRestart func()
	firstRestart = func() {
		firstCalls.Add(1)
		first.SetRestartFunc(firstRestart)
	}
	var secondRestart func()
	secondRestart = func() {
		secondCalls.Add(1)
		second.SetRestartFunc(secondRestart)
	}
	first.SetRestartFunc(firstRestart)
	second.SetRestartFunc(secondRestart)

	const triggersPerServer = 64
	var wg sync.WaitGroup
	wg.Add(triggersPerServer * 2)
	for range triggersPerServer {
		go func() {
			defer wg.Done()
			first.triggerRestart()
		}()
		go func() {
			defer wg.Done()
			second.triggerRestart()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restart callbacks deadlocked while replacing themselves")
	}

	if got := firstCalls.Load(); got != triggersPerServer {
		t.Fatalf("first restart calls=%d, want %d", got, triggersPerServer)
	}
	if got := secondCalls.Load(); got != triggersPerServer {
		t.Fatalf("second restart calls=%d, want %d", got, triggersPerServer)
	}
}

func findAdminSetting(t *testing.T, settings []map[string]any, key string) map[string]any {
	t.Helper()
	for _, setting := range settings {
		if setting["key"] == key {
			return setting
		}
	}
	t.Fatalf("setting %q not found", key)
	return nil
}

func TestAdminContainerUpdateSettingsDisabled(t *testing.T) {
	t.Setenv("CCLOAD_CONTAINER", "1")

	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	const disabledReason = "container_image_managed"
	updateKeys := []string{autoUpdateIntervalSettingKey, autoUpdateChannelSettingKey}

	t.Run("list and get expose disabled state", func(t *testing.T) {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings", nil))
		server.AdminListSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}

		resp := mustParseAPIResponse[[]map[string]any](t, w.Body.Bytes())
		for _, key := range updateKeys {
			setting := findAdminSetting(t, resp.Data, key)
			if editable, ok := setting["editable"].(bool); !ok || editable {
				t.Fatalf("setting %q editable=%v, want false", key, setting["editable"])
			}
			if reason := setting["disabled_reason"]; reason != disabledReason {
				t.Fatalf("setting %q disabled_reason=%v, want %q", key, reason, disabledReason)
			}

			c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/settings/"+key, nil))
			c.Params = gin.Params{{Key: "key", Value: key}}
			server.AdminGetSetting(c)
			if w.Code != http.StatusOK {
				t.Fatalf("get %q status=%d, want %d body=%s", key, w.Code, http.StatusOK, w.Body.String())
			}
			view := mustParseAPIResponse[map[string]any](t, w.Body.Bytes()).Data
			if view["editable"] != false || view["disabled_reason"] != disabledReason {
				t.Fatalf("get %q view=%v, want disabled container view", key, view)
			}
		}
	})

	restarted := make(chan struct{}, 1)
	server.SetRestartFunc(func() { restarted <- struct{}{} })

	t.Run("all write paths reject container-managed settings", func(t *testing.T) {
		for _, key := range updateKeys {
			before, err := store.GetSetting(context.Background(), key)
			if err != nil {
				t.Fatalf("GetSetting %q before write: %v", key, err)
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/settings/"+key, map[string]string{"value": before.DefaultValue}))
			c.Params = gin.Params{{Key: "key", Value: key}}
			server.AdminUpdateSetting(c)
			if w.Code != http.StatusConflict {
				t.Fatalf("update %q status=%d, want %d body=%s", key, w.Code, http.StatusConflict, w.Body.String())
			}

			c, w = newTestContext(t, newRequest(http.MethodPost, "/admin/settings/"+key+"/reset", nil))
			c.Params = gin.Params{{Key: "key", Value: key}}
			server.AdminResetSetting(c)
			if w.Code != http.StatusConflict {
				t.Fatalf("reset %q status=%d, want %d body=%s", key, w.Code, http.StatusConflict, w.Body.String())
			}

			after, err := store.GetSetting(context.Background(), key)
			if err != nil {
				t.Fatalf("GetSetting %q after writes: %v", key, err)
			}
			if after.Value != before.Value {
				t.Fatalf("setting %q changed from %q to %q", key, before.Value, after.Value)
			}
		}

		beforeLogRetention, err := store.GetSetting(context.Background(), "log_retention_days")
		if err != nil {
			t.Fatalf("GetSetting log_retention_days before batch: %v", err)
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
			"log_retention_days":        "30",
			autoUpdateChannelSettingKey: "preview",
		}))
		server.AdminBatchUpdateSettings(c)
		if w.Code != http.StatusConflict {
			t.Fatalf("batch status=%d, want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
		}
		afterLogRetention, err := store.GetSetting(context.Background(), "log_retention_days")
		if err != nil {
			t.Fatalf("GetSetting log_retention_days after batch: %v", err)
		}
		if afterLogRetention.Value != beforeLogRetention.Value {
			t.Fatalf("batch partially changed log_retention_days from %q to %q", beforeLogRetention.Value, afterLogRetention.Value)
		}

		select {
		case <-restarted:
			t.Fatal("rejected container setting write triggered restart")
		default:
		}
	})
}

func TestAdminUpdateModelCatalogSyncIntervalSetting(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	restartCh := make(chan struct{}, 3)
	server.SetRestartFunc(func() { restartCh <- struct{}{} })

	const key = "model_catalog_sync_interval_hours"
	tests := []struct {
		name     string
		value    string
		wantCode int
	}{
		{name: "disabled", value: "0", wantCode: http.StatusOK},
		{name: "fractional interval", value: "0.5", wantCode: http.StatusOK},
		{name: "default interval", value: "6", wantCode: http.StatusOK},
		{name: "negative interval", value: "-0.1", wantCode: http.StatusBadRequest},
		{name: "not a number", value: "NaN", wantCode: http.StatusBadRequest},
		{name: "positive infinity", value: "+Inf", wantCode: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := store.GetSetting(context.Background(), key)
			if err != nil {
				t.Fatalf("GetSetting before update failed: %v", err)
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/settings/"+key, map[string]string{"value": tt.value}))
			c.Params = gin.Params{{Key: "key", Value: key}}
			server.AdminUpdateSetting(c)

			if w.Code != tt.wantCode {
				t.Fatalf("status=%d, want %d, body=%s", w.Code, tt.wantCode, w.Body.String())
			}

			after, err := store.GetSetting(context.Background(), key)
			if err != nil {
				t.Fatalf("GetSetting after update failed: %v", err)
			}
			if tt.wantCode == http.StatusOK {
				if after.Value != tt.value {
					t.Fatalf("persisted value=%q, want %q", after.Value, tt.value)
				}
				select {
				case <-restartCh:
				case <-time.After(time.Second):
					t.Fatal("expected restart triggered")
				}
				return
			}
			if after.Value != before.Value {
				t.Fatalf("persisted value=%q, want unchanged %q", after.Value, before.Value)
			}
		})
	}
}

func TestAdminSettingContractValidation(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	restarted := make(chan struct{}, 32)
	server.SetRestartFunc(func() { restarted <- struct{}{} })

	tests := []struct {
		name     string
		key      string
		value    string
		wantCode int
	}{
		{name: "antigravity empty array", key: "antigravity_sensitive_words", value: `[]`, wantCode: http.StatusOK},
		{name: "antigravity null", key: "antigravity_sensitive_words", value: `null`, wantCode: http.StatusBadRequest},
		{name: "antigravity non-string array", key: "antigravity_sensitive_words", value: `[1]`, wantCode: http.StatusBadRequest},
		{name: "success penalty zero", key: "success_rate_penalty_weight", value: "0", wantCode: http.StatusOK},
		{name: "success penalty negative", key: "success_rate_penalty_weight", value: "-1", wantCode: http.StatusBadRequest},
		{name: "health window zero", key: "health_score_window_minutes", value: "0", wantCode: http.StatusBadRequest},
		{name: "health update zero", key: "health_score_update_interval", value: "0", wantCode: http.StatusBadRequest},
		{name: "health sample zero", key: "health_min_confident_sample", value: "0", wantCode: http.StatusBadRequest},
		{name: "ttfb penalty zero", key: "ttfb_penalty_weight", value: "0", wantCode: http.StatusOK},
		{name: "ttfb penalty negative", key: "ttfb_penalty_weight", value: "-0.1", wantCode: http.StatusBadRequest},
		{name: "ttfb slow ratio negative", key: "ttfb_max_slow_ratio", value: "-0.1", wantCode: http.StatusBadRequest},
		{name: "ttfb sample zero", key: "ttfb_min_confident_sample", value: "0", wantCode: http.StatusBadRequest},
		{name: "debug retention maximum", key: "debug_log_retention_minutes", value: "1440", wantCode: http.StatusOK},
		{name: "debug retention zero", key: "debug_log_retention_minutes", value: "0", wantCode: http.StatusBadRequest},
		{name: "debug retention too large", key: "debug_log_retention_minutes", value: "1441", wantCode: http.StatusBadRequest},
		{name: "auto refresh disabled", key: "auto_refresh_interval_seconds", value: "0", wantCode: http.StatusOK},
		{name: "auto refresh negative", key: "auto_refresh_interval_seconds", value: "-1", wantCode: http.StatusBadRequest},
		{name: "channel test content", key: "channel_test_content", value: "ping", wantCode: http.StatusOK},
		{name: "channel test blank", key: "channel_test_content", value: "  ", wantCode: http.StatusBadRequest},
		{name: "channel stats listed", key: "channel_stats_range", value: "last_month", wantCode: http.StatusOK},
		{name: "channel stats unknown", key: "channel_stats_range", value: "forever", wantCode: http.StatusBadRequest},
		{name: "duration maximum", key: "stream_timeout", value: strconv.FormatInt(maxSettingDurationSeconds, 10), wantCode: http.StatusOK},
		{name: "duration overflow", key: "stream_timeout", value: strconv.FormatInt(maxSettingDurationSeconds+1, 10), wantCode: http.StatusBadRequest},
		{name: "channel interval overflow", key: "model_catalog_sync_interval_hours", value: strconv.FormatInt(maxSettingDurationHours+1, 10), wantCode: http.StatusBadRequest},
		{name: "auto update overflow", key: autoUpdateIntervalSettingKey, value: strconv.FormatInt(maxSettingDurationHours+1, 10), wantCode: http.StatusBadRequest},
		{name: "websocket ttl default", key: responsesWebsocketSessionTTLSetting, value: "0", wantCode: http.StatusOK},
		{name: "websocket ttl overflow", key: responsesWebsocketSessionTTLSetting, value: strconv.FormatInt(maxSettingDurationMinutes+1, 10), wantCode: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := store.GetSetting(context.Background(), tt.key)
			if err != nil {
				t.Fatalf("GetSetting before update: %v", err)
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/settings/"+tt.key, map[string]string{"value": tt.value}))
			c.Params = gin.Params{{Key: "key", Value: tt.key}}

			server.AdminUpdateSetting(c)

			if w.Code != tt.wantCode {
				t.Fatalf("status=%d, want %d body=%s", w.Code, tt.wantCode, w.Body.String())
			}
			after, err := store.GetSetting(context.Background(), tt.key)
			if err != nil {
				t.Fatalf("GetSetting after update: %v", err)
			}
			if tt.wantCode == http.StatusOK {
				if after.Value != tt.value {
					t.Fatalf("persisted value=%q, want %q", after.Value, tt.value)
				}
				select {
				case <-restarted:
				case <-time.After(time.Second):
					t.Fatal("expected restart triggered")
				}
				return
			}
			if after.Value != before.Value {
				t.Fatalf("persisted value=%q, want unchanged %q", after.Value, before.Value)
			}
			select {
			case <-restarted:
				t.Fatal("rejected update triggered restart")
			default:
			}
		})
	}
}

func TestAdminCooldownBoundsUseFreshAtomicSnapshot(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}
	if err := store.BatchUpdateSettings(context.Background(), map[string]string{
		cooldownMinSecondsSettingKey: "200",
		cooldownMaxSecondsSettingKey: "300",
	}); err != nil {
		t.Fatalf("seed cooldown bounds: %v", err)
	}

	restarted := make(chan struct{}, 2)
	server.SetRestartFunc(func() { restarted <- struct{}{} })

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/settings/"+cooldownMaxSecondsSettingKey, map[string]string{"value": "199"}))
	c.Params = gin.Params{{Key: "key", Value: cooldownMaxSecondsSettingKey}}
	server.AdminUpdateSetting(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("single update status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
		cooldownMinSecondsSettingKey: "250",
		cooldownMaxSecondsSettingKey: "250",
	}))
	server.AdminBatchUpdateSettings(c)
	if w.Code != http.StatusOK {
		t.Fatalf("batch update status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	for _, key := range []string{cooldownMinSecondsSettingKey, cooldownMaxSecondsSettingKey} {
		setting, err := store.GetSetting(context.Background(), key)
		if err != nil {
			t.Fatalf("GetSetting %s: %v", key, err)
		}
		if setting.Value != "250" {
			t.Fatalf("%s=%q, want 250", key, setting.Value)
		}
	}
}

func TestAdminSettingsHandlers(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(context.Background()); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	restartCh := make(chan struct{}, 10)
	server.SetRestartFunc(func() { restartCh <- struct{}{} })
	drainRestarts := func() {
		for len(restartCh) > 0 {
			<-restartCh
		}
	}
	assertNoRestart := func(t *testing.T) {
		t.Helper()
		select {
		case <-restartCh:
			t.Fatal("unexpected restart triggered")
		case <-time.After(200 * time.Millisecond):
		}
	}

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

	t.Run("AdminUpdateSetting_rejects_duplicated_oauth_url_scheme", func(t *testing.T) {
		c, w := newTestContext(t, newJSONRequestBytes(
			http.MethodPut,
			"/admin/settings/ANTIGRAVITY_URL",
			[]byte(`{"value":"https://https://antigravity.hz-dao.deno.net"}`),
		))
		c.Params = gin.Params{{Key: "key", Value: "ANTIGRAVITY_URL"}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "use https://antigravity.hz-dao.deno.net") {
			t.Fatalf("body=%s, want corrected URL", w.Body.String())
		}
		setting, err := store.GetSetting(context.Background(), "ANTIGRAVITY_URL")
		if err != nil {
			t.Fatalf("GetSetting() error = %v", err)
		}
		if setting.Value != "" {
			t.Fatalf("ANTIGRAVITY_URL=%q, want unchanged empty default", setting.Value)
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

	t.Run("AdminUpdateSetting_multimodal_fallback_hot_reloads_without_restart", func(t *testing.T) {
		drainRestarts()
		mapping := `{"gpt-text":"gpt-vision-update"}`
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/settings/"+modelMultimodalFallbackSettingKey, map[string]string{
			"value": mapping,
		}))
		c.Params = gin.Params{{Key: "key", Value: modelMultimodalFallbackSettingKey}}

		server.AdminUpdateSetting(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "重启") {
			t.Fatalf("hot update response must not claim restart: %s", w.Body.String())
		}
		if got := server.multimodalFallbackModel("gpt-text", true); got != "gpt-vision-update" {
			t.Fatalf("runtime fallback=%q, want gpt-vision-update", got)
		}
		persisted, err := store.GetSetting(context.Background(), modelMultimodalFallbackSettingKey)
		if err != nil {
			t.Fatalf("GetSetting failed: %v", err)
		}
		if persisted.Value != mapping {
			t.Fatalf("persisted mapping=%q, want %q", persisted.Value, mapping)
		}
		assertNoRestart(t)
	})

	t.Run("AdminGetSetting_returns_latest_db_value_before_restart", func(t *testing.T) {
		if err := store.UpdateSetting(context.Background(), "model_catalog_sync_interval_hours", "1"); err != nil {
			t.Fatalf("failed to seed setting in db: %v", err)
		}

		seed, err := store.GetSetting(context.Background(), "model_catalog_sync_interval_hours")
		if err != nil {
			t.Fatalf("failed to read seeded setting: %v", err)
		}
		seed.Value = "1"

		server.configService.mu.Lock()
		server.configService.cache["model_catalog_sync_interval_hours"] = seed
		server.configService.mu.Unlock()

		updateCtx, updateW := newTestContext(t, newJSONRequestBytes(http.MethodPut, "/admin/settings/model_catalog_sync_interval_hours", []byte(`{"value":"0"}`)))
		updateCtx.Params = gin.Params{{Key: "key", Value: "model_catalog_sync_interval_hours"}}

		server.AdminUpdateSetting(updateCtx)

		if updateW.Code != http.StatusOK {
			t.Fatalf("update status=%d, want %d body=%s", updateW.Code, http.StatusOK, updateW.Body.String())
		}

		select {
		case <-restartCh:
		case <-time.After(1 * time.Second):
			t.Fatal("expected restart triggered")
		}

		getCtx, getW := newTestContext(t, newRequest(http.MethodGet, "/admin/settings/model_catalog_sync_interval_hours", nil))
		getCtx.Params = gin.Params{{Key: "key", Value: "model_catalog_sync_interval_hours"}}

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

	t.Run("AdminResetSetting_multimodal_fallback_hot_clears_without_restart", func(t *testing.T) {
		drainRestarts()
		mapping := `{"gpt-text":"gpt-vision-reset"}`
		if err := store.UpdateSetting(context.Background(), modelMultimodalFallbackSettingKey, mapping); err != nil {
			t.Fatalf("seed multimodal fallback: %v", err)
		}
		server.setMultimodalFallbackModels(map[string]string{"gpt-text": "gpt-vision-reset"})

		c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/settings/"+modelMultimodalFallbackSettingKey+"/reset", nil))
		c.Params = gin.Params{{Key: "key", Value: modelMultimodalFallbackSettingKey}}
		server.AdminResetSetting(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "重启") {
			t.Fatalf("hot reset response must not claim restart: %s", w.Body.String())
		}
		if got := server.multimodalFallbackModel("gpt-text", true); got != "" {
			t.Fatalf("runtime fallback after reset=%q, want empty", got)
		}
		persisted, err := store.GetSetting(context.Background(), modelMultimodalFallbackSettingKey)
		if err != nil {
			t.Fatalf("GetSetting failed: %v", err)
		}
		if persisted.Value != "{}" {
			t.Fatalf("persisted mapping after reset=%q, want {}", persisted.Value)
		}
		assertNoRestart(t)
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

	t.Run("AdminBatchUpdateSettings_invalid_global_cooldown_rules_reject", func(t *testing.T) {
		before, err := store.GetSetting(context.Background(), globalCooldownDetectionRulesSettingKey)
		if err != nil {
			t.Fatalf("GetSetting before update failed: %v", err)
		}
		invalidRules := `{"rules":[{"enabled":true,"name":"Broken","priority":0,"status_codes":[429],"scope":"channel","mode":"fixed","cooldown_seconds":0}]}`
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
			globalCooldownDetectionRulesSettingKey: invalidRules,
		}))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		after, err := store.GetSetting(context.Background(), globalCooldownDetectionRulesSettingKey)
		if err != nil {
			t.Fatalf("GetSetting after update failed: %v", err)
		}
		if after.Value != before.Value {
			t.Fatalf("persisted value=%q, want unchanged %q", after.Value, before.Value)
		}
	})

	t.Run("AdminBatchUpdateSettings_invalid_multimodal_fallback_reject", func(t *testing.T) {
		before, err := store.GetSetting(context.Background(), modelMultimodalFallbackSettingKey)
		if err != nil {
			t.Fatalf("GetSetting before update failed: %v", err)
		}
		drainRestarts()
		server.setMultimodalFallbackModels(map[string]string{"existing-text": "existing-vision"})

		invalidMapping := `{"gpt-5.6-luna":"gpt-5.6-luna"}`
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
			modelMultimodalFallbackSettingKey: invalidMapping,
		}))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		after, err := store.GetSetting(context.Background(), modelMultimodalFallbackSettingKey)
		if err != nil {
			t.Fatalf("GetSetting after update failed: %v", err)
		}
		if after.Value != before.Value {
			t.Fatalf("persisted value=%q, want unchanged %q", after.Value, before.Value)
		}
		if got := server.multimodalFallbackModel("existing-text", true); got != "existing-vision" {
			t.Fatalf("runtime fallback=%q, want unchanged existing-vision", got)
		}
		assertNoRestart(t)
	})

	t.Run("AdminBatchUpdateSettings_multimodal_fallback_hot_reloads_without_restart", func(t *testing.T) {
		drainRestarts()
		mapping := `{"gpt-text":"gpt-vision-batch"}`
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
			modelMultimodalFallbackSettingKey: mapping,
		}))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "重启") {
			t.Fatalf("hot batch response must not claim restart: %s", w.Body.String())
		}
		if got := server.multimodalFallbackModel("gpt-text", true); got != "gpt-vision-batch" {
			t.Fatalf("runtime fallback=%q, want gpt-vision-batch", got)
		}
		persisted, err := store.GetSetting(context.Background(), modelMultimodalFallbackSettingKey)
		if err != nil {
			t.Fatalf("GetSetting failed: %v", err)
		}
		if persisted.Value != mapping {
			t.Fatalf("persisted mapping=%q, want %q", persisted.Value, mapping)
		}
		assertNoRestart(t)
	})

	t.Run("AdminBatchUpdateSettings_mixed_multimodal_update_still_restarts", func(t *testing.T) {
		drainRestarts()
		mapping := `{"gpt-text":"gpt-vision-mixed"}`
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{
			modelMultimodalFallbackSettingKey: mapping,
			"log_retention_days":              "21",
		}))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := server.multimodalFallbackModel("gpt-text", true); got != "gpt-vision-mixed" {
			t.Fatalf("runtime fallback=%q, want gpt-vision-mixed", got)
		}
		select {
		case <-restartCh:
		case <-time.After(time.Second):
			t.Fatal("mixed settings update must trigger restart")
		}
	})

	t.Run("AdminBatchUpdateSettings_responses_websocket_zero_uses_defaults", func(t *testing.T) {
		updates := map[string]string{
			responsesWebsocketMaxSessionsSetting:            "0",
			responsesWebsocketSessionTTLSetting:             "0",
			responsesWebsocketMaxTranscriptBytesSetting:     "0",
			responsesWebsocketMaxConnectionsSetting:         "0",
			responsesWebsocketMaxConnectionsPerTokenSetting: "0",
		}
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", updates))

		server.AdminBatchUpdateSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		for key := range updates {
			setting, err := store.GetSetting(context.Background(), key)
			if err != nil {
				t.Fatalf("GetSetting %q: %v", key, err)
			}
			if setting.Value != "0" {
				t.Fatalf("setting %q value=%q, want 0", key, setting.Value)
			}
		}
		select {
		case <-restartCh:
		case <-time.After(time.Second):
			t.Fatal("expected restart triggered")
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
