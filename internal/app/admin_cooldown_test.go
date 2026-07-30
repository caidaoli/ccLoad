package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleCooldownDetectionTestEvaluatesDraftWithoutPersistingCooldown(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		ID:           1,
		Name:         "cooldown-detection-test",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig() error = %v", err)
	}
	originalUntil := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	if err := srv.store.SetChannelCooldown(ctx, created.ID, originalUntil); err != nil {
		t.Fatalf("SetChannelCooldown() error = %v", err)
	}

	payload := map[string]any{
		"status_code": 200,
		"error_body":  `2026-07-24T15:30:00+08:00 [WARN] upstream status 406: {"error":{"code":"maintenance","message":"planned maintenance"}}`,
		"cooldown_detection_rules": map[string]any{
			"rules": []map[string]any{{
				"enabled":          true,
				"name":             "Maintenance",
				"priority":         9,
				"status_codes":     []int{406},
				"message_pattern":  "planned maintenance",
				"scope":            model.CooldownScopeChannel,
				"mode":             model.CooldownModeFixed,
				"cooldown_seconds": 120,
			}},
		},
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
	srv.HandleCooldownDetectionTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var data struct {
		Code              string     `json:"code"`
		StatusCode        int        `json:"status_code"`
		ParsedLog         bool       `json:"parsed_log"`
		Matched           bool       `json:"matched"`
		Actionable        bool       `json:"actionable"`
		Priority          *int       `json:"priority"`
		CooldownUntil     *time.Time `json:"cooldown_until"`
		FallbackToBuiltin bool       `json:"fallback_to_builtin"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Code != "MAINTENANCE" || !data.Matched || !data.Actionable || data.Priority == nil || *data.Priority != 0 {
		t.Fatalf("unexpected rule test result: %+v", data)
	}
	if data.StatusCode != http.StatusNotAcceptable || !data.ParsedLog {
		t.Fatalf("normalized test input = (status=%d, parsed_log=%t), want (406, true)", data.StatusCode, data.ParsedLog)
	}
	if data.CooldownUntil == nil || !data.CooldownUntil.After(time.Now()) || data.FallbackToBuiltin {
		t.Fatalf("unexpected cooldown result: %+v", data)
	}

	updated, err := srv.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if updated.CooldownUntil != originalUntil.Unix() {
		t.Fatalf("test endpoint changed channel cooldown: got %d, want %d", updated.CooldownUntil, originalUntil.Unix())
	}
}

func TestHandleCooldownDetectionTestKeepsExplicitStatusForRawBody(t *testing.T) {
	srv := newInMemoryServer(t)
	payload := map[string]any{
		"status_code": http.StatusOK,
		"error_body":  `{"error":{"message":"soft quota error: upstream status 999: is provider text"}}`,
		"cooldown_detection_rules": map[string]any{
			"rules": []map[string]any{{
				"enabled":          true,
				"name":             "HTTP 200 soft error",
				"priority":         0,
				"status_codes":     []int{http.StatusOK},
				"message_pattern":  "soft quota error",
				"scope":            model.CooldownScopeChannel,
				"mode":             model.CooldownModeFixed,
				"cooldown_seconds": 60,
			}},
		},
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
	srv.HandleCooldownDetectionTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var data struct {
		StatusCode int  `json:"status_code"`
		ParsedLog  bool `json:"parsed_log"`
		Actionable bool `json:"actionable"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.StatusCode != http.StatusOK || data.ParsedLog || !data.Actionable {
		t.Fatalf("unexpected raw body result: %+v", data)
	}
}

func TestHandleCooldownDetectionTestRejectsMalformedStandardLog(t *testing.T) {
	tests := []struct {
		name string
		log  string
	}{
		{name: "invalid status", log: `upstream status 999: {"error":{"message":"bad"}}`},
		{name: "empty body", log: "upstream status 200:   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			payload := map[string]any{
				"status_code": http.StatusOK,
				"error_body":  tt.log,
				"cooldown_detection_rules": map[string]any{
					"rules": []map[string]any{{
						"enabled": true, "name": "Status only", "priority": 0,
						"status_codes": []int{http.StatusOK}, "scope": model.CooldownScopeChannel,
						"mode": model.CooldownModeFixed, "cooldown_seconds": 60,
					}},
				},
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
			srv.HandleCooldownDetectionTest(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

// TestHandleSetChannelCooldown 测试设置渠道冷却
func TestHandleSetChannelCooldown(t *testing.T) {
	tests := []struct {
		name           string
		channelID      string
		requestBody    map[string]any
		setupChannel   bool
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:      "成功设置渠道冷却",
			channelID: "1",
			requestBody: map[string]any{
				"duration_ms": 60000, // 60秒
			},
			setupChannel:   true,
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:      "无效的渠道ID",
			channelID: "invalid",
			requestBody: map[string]any{
				"duration_ms": 60000,
			},
			setupChannel:   false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "无效的请求体",
			channelID: "1",
			requestBody: map[string]any{
				"invalid_field": "value",
			},
			setupChannel:   true,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "大冷却时长",
			channelID: "1",
			requestBody: map[string]any{
				"duration_ms": 1800000, // 30分钟
			},
			setupChannel:   true,
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试服务器
			srv := newInMemoryServer(t)

			// 设置渠道(如果需要)
			if tt.setupChannel {
				cfg := &model.Config{
					ID:           1,
					Name:         "test-channel",
					URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
					Priority:     1,
					ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
					Enabled:      true,
				}
				_, err := srv.store.CreateConfig(context.Background(), cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+tt.channelID+"/cooldown", tt.requestBody))
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleSetChannelCooldown(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d", tt.expectedStatus, w.Code)
			}

			resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
			if resp.Success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v, error=%q", tt.expectSuccess, resp.Success, resp.Error)
			}
		})
	}
}

// TestHandleSetKeyCooldown 测试设置Key冷却
func TestHandleSetKeyCooldown(t *testing.T) {
	tests := []struct {
		name           string
		channelID      string
		keyIndex       string
		requestBody    map[string]any
		setupData      bool
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:      "成功设置Key冷却",
			channelID: "1",
			keyIndex:  "0",
			requestBody: map[string]any{
				"duration_ms": 30000, // 30秒
			},
			setupData:      true,
			expectedStatus: http.StatusOK,
			expectSuccess:  true,
		},
		{
			name:      "无效的渠道ID",
			channelID: "invalid",
			keyIndex:  "0",
			requestBody: map[string]any{
				"duration_ms": 30000,
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "无效的Key索引",
			channelID: "1",
			keyIndex:  "invalid",
			requestBody: map[string]any{
				"duration_ms": 30000,
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "负数Key索引",
			channelID: "1",
			keyIndex:  "-1",
			requestBody: map[string]any{
				"duration_ms": 30000,
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "无效的请求体",
			channelID: "1",
			keyIndex:  "0",
			requestBody: map[string]any{
				"invalid_field": "value",
			},
			setupData:      true,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试服务器
			srv := newInMemoryServer(t)

			// 设置测试数据(如果需要)
			if tt.setupData {
				ctx := context.Background()

				// 创建渠道
				cfg := &model.Config{
					ID:           1,
					Name:         "test-channel",
					URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
					Priority:     1,
					ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
					Enabled:      true,
				}
				_, err := srv.store.CreateConfig(ctx, cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}

				// 创建API Key
				key := &model.APIKey{
					ChannelID:   1,
					KeyIndex:    0,
					APIKey:      "test-key",
					KeyStrategy: model.KeyStrategySequential,
				}
				if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{key}); err != nil {
					t.Fatalf("创建API Key失败: %v", err)
				}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+tt.channelID+"/keys/"+tt.keyIndex+"/cooldown", tt.requestBody))
			c.Params = gin.Params{
				{Key: "id", Value: tt.channelID},
				{Key: "keyIndex", Value: tt.keyIndex},
			}

			// 调用处理函数
			srv.HandleSetKeyCooldown(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d", tt.expectedStatus, w.Code)
			}

			resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
			if resp.Success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v, error=%q", tt.expectSuccess, resp.Success, resp.Error)
			}
		})
	}
}

// TestSetChannelCooldown_Integration 测试渠道冷却集成
func TestSetChannelCooldown_Integration(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		ID:           1,
		Name:         "test-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	}
	_, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 设置冷却
	requestBody := map[string]any{
		"duration_ms": 120000, // 2分钟
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/cooldown", requestBody))
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleSetChannelCooldown(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if !resp.Success {
		t.Error("期望 success=true")
	}

	// 验证数据库中的冷却状态
	updatedCfg, err := srv.store.GetConfig(ctx, 1)
	if err != nil {
		t.Fatalf("获取渠道失败: %v", err)
	}

	if updatedCfg.CooldownUntil == 0 {
		t.Error("期望渠道被冷却, 但 CooldownUntil=0")
	}
}

// TestSetKeyCooldown_Integration 测试Key冷却集成
func TestSetKeyCooldown_Integration(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		ID:           1,
		Name:         "test-channel",
		URLs:         model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
		Enabled:      true,
	}
	_, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建API Key
	key := &model.APIKey{
		ChannelID:   1,
		KeyIndex:    0,
		APIKey:      "test-key",
		KeyStrategy: model.KeyStrategySequential,
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{key}); err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 设置Key冷却
	requestBody := map[string]any{
		"duration_ms": 90000, // 90秒
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/keys/0/cooldown", requestBody))
	c.Params = gin.Params{
		{Key: "id", Value: "1"},
		{Key: "keyIndex", Value: "0"},
	}

	srv.HandleSetKeyCooldown(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
	if !resp.Success {
		t.Error("期望 success=true")
	}

	// 验证数据库中的Key冷却状态
	updatedKey, err := srv.store.GetAPIKey(ctx, 1, 0)
	if err != nil {
		t.Fatalf("获取API Key失败: %v", err)
	}

	if updatedKey.CooldownUntil == 0 {
		t.Error("期望Key被冷却, 但 CooldownUntil=0")
	}
}
