package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

// TestHandleSetChannelCooldown 测试设置渠道冷却
func TestHandleSetChannelCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
			srv, cleanup := setupTestServer(t)
			defer cleanup()

			// 设置渠道(如果需要)
			if tt.setupChannel {
				cfg := &model.Config{
					ID:       1,
					Name:     "test-channel",
					URL:      "http://test.example.com",
					Priority: 1,
					Models:   []string{"test-model"},
					Enabled:  true,
				}
				_, err := srv.store.CreateConfig(context.Background(), cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}
			}

			// 创建请求
			body, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest(http.MethodPost, "/admin/channels/"+tt.channelID+"/cooldown", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			// 创建响应记录器
			w := httptest.NewRecorder()

			// 创建Gin上下文
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleSetChannelCooldown(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d", tt.expectedStatus, w.Code)
			}

			// 解析响应
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}

			// 验证success字段
			success, ok := response["success"].(bool)
			if !ok {
				t.Fatal("响应缺少success字段")
			}
			if success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v", tt.expectSuccess, success)
			}
		})
	}
}

// TestHandleSetKeyCooldown 测试设置Key冷却
func TestHandleSetKeyCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
			srv, cleanup := setupTestServer(t)
			defer cleanup()

			// 设置测试数据(如果需要)
			if tt.setupData {
				ctx := context.Background()

				// 创建渠道
				cfg := &model.Config{
					ID:       1,
					Name:     "test-channel",
					URL:      "http://test.example.com",
					Priority: 1,
					Models:   []string{"test-model"},
					Enabled:  true,
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
					KeyStrategy: "sequential",
				}
				if err := srv.store.CreateAPIKey(ctx, key); err != nil {
					t.Fatalf("创建API Key失败: %v", err)
				}
			}

			// 创建请求
			body, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest(http.MethodPost,
				"/admin/channels/"+tt.channelID+"/keys/"+tt.keyIndex+"/cooldown",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			// 创建响应记录器
			w := httptest.NewRecorder()

			// 创建Gin上下文
			c, _ := gin.CreateTestContext(w)
			c.Request = req
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

			// 解析响应
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}

			// 验证success字段
			success, ok := response["success"].(bool)
			if !ok {
				t.Fatal("响应缺少success字段")
			}
			if success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v", tt.expectSuccess, success)
			}
		})
	}
}

// TestSetChannelCooldown_Integration 测试渠道冷却集成
func TestSetChannelCooldown_Integration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		ID:       1,
		Name:     "test-channel",
		URL:      "http://test.example.com",
		Priority: 1,
		Models:   []string{"test-model"},
		Enabled:  true,
	}
	_, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 设置冷却
	requestBody := map[string]any{
		"duration_ms": 120000, // 2分钟
	}
	body, _ := json.Marshal(requestBody)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/1/cooldown", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleSetChannelCooldown(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if !response["success"].(bool) {
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
	gin.SetMode(gin.TestMode)

	srv, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		ID:       1,
		Name:     "test-channel",
		URL:      "http://test.example.com",
		Priority: 1,
		Models:   []string{"test-model"},
		Enabled:  true,
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
		KeyStrategy: "sequential",
	}
	if err := srv.store.CreateAPIKey(ctx, key); err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	// 设置Key冷却
	requestBody := map[string]any{
		"duration_ms": 90000, // 90秒
	}
	body, _ := json.Marshal(requestBody)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/1/keys/0/cooldown", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{
		{Key: "id", Value: "1"},
		{Key: "keyIndex", Value: "0"},
	}

	srv.HandleSetKeyCooldown(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if !response["success"].(bool) {
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
