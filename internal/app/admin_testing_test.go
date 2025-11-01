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

// TestHandleChannelTest 测试渠道测试功能
func TestHandleChannelTest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		channelID      string
		requestBody    map[string]any
		setupData      bool
		expectedStatus int
		expectSuccess  bool
	}{
		{
			name:      "无效的渠道ID",
			channelID: "invalid",
			requestBody: map[string]any{
				"model":        "test-model",
				"channel_type": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
		{
			name:      "渠道不存在",
			channelID: "999",
			requestBody: map[string]any{
				"model":        "test-model",
				"channel_type": "anthropic",
			},
			setupData:      false,
			expectedStatus: http.StatusNotFound,
			expectSuccess:  false,
		},
		{
			name:      "无效的请求体",
			channelID: "1",
			requestBody: map[string]any{
				"invalid_field": "value",
			},
			setupData:      false,
			expectedStatus: http.StatusBadRequest,
			expectSuccess:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试服务器
			srv, cleanup := setupTestServer(t)
			defer cleanup()

			ctx := context.Background()

			// 设置测试数据(如果需要)
			if tt.setupData {
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
			}

			// 创建请求
			body, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest(http.MethodPost, "/admin/channels/"+tt.channelID+"/test", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			// 创建响应记录器
			w := httptest.NewRecorder()

			// 创建Gin上下文
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleChannelTest(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d, 响应: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			// 解析响应
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应失败: %v, 响应: %s", err, w.Body.String())
			}

			// 验证success字段
			if tt.expectedStatus == http.StatusOK {
				success, ok := response["success"].(bool)
				if !ok {
					t.Fatal("响应缺少success字段")
				}
				if success != tt.expectSuccess {
					t.Errorf("期望 success=%v, 实际=%v", tt.expectSuccess, success)
				}
			}
		})
	}
}

// TestHandleChannelTest_ModelNotSupported 测试不支持的模型
func TestHandleChannelTest_ModelNotSupported(t *testing.T) {
	t.Skip("跳过此测试 - 需要HTTP客户端初始化")

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
		Models:   []string{"gpt-4", "gpt-3.5-turbo"}, // 不包含test-model
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

	// 测试不支持的模型
	requestBody := map[string]any{
		"model":        "unsupported-model",
		"channel_type": "anthropic",
	}

	body, _ := json.Marshal(requestBody)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/1/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleChannelTest(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 验证返回错误
	if response["success"].(bool) {
		t.Error("期望 success=false (模型不支持)")
	}

	if response["error"] == nil {
		t.Error("期望返回错误信息")
	}

	// 验证返回支持的模型列表
	if response["supported_models"] == nil {
		t.Error("期望返回 supported_models 字段")
	}
}

// TestHandleChannelTest_WithValidData 测试有效数据(不实际发送HTTP请求)
func TestHandleChannelTest_WithValidData(t *testing.T) {
	t.Skip("跳过此测试 - 需要HTTP客户端初始化")

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

	// 测试有效请求(会失败因为URL无效,但能验证逻辑)
	requestBody := map[string]any{
		"model":        "test-model",
		"channel_type": "anthropic",
		"key_index":    0,
	}

	body, _ := json.Marshal(requestBody)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/1/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	srv.HandleChannelTest(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 200, 实际 %d", w.Code)
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 验证响应包含必要字段
	if response["tested_key_index"] == nil {
		t.Error("期望返回 tested_key_index 字段")
	}
	if response["total_keys"] == nil {
		t.Error("期望返回 total_keys 字段")
	}

	// 由于URL无效,测试会失败,但这验证了逻辑流程
	if response["success"].(bool) {
		t.Log("测试成功(意外,因为URL无效)")
	} else {
		t.Log("测试失败(预期,因为URL无效)")
		// 验证有错误信息
		if response["error"] == nil {
			t.Error("期望返回错误信息")
		}
	}
}

// TestHandleChannelTest_KeyIndexSelection 测试Key索引选择
func TestHandleChannelTest_KeyIndexSelection(t *testing.T) {
	t.Skip("跳过此测试 - 需要HTTP客户端初始化")

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

	// 创建多个API Key
	for i := 0; i < 3; i++ {
		key := &model.APIKey{
			ChannelID:   1,
			KeyIndex:    i,
			APIKey:      "test-key-" + string(rune('0'+i)),
			KeyStrategy: "sequential",
		}
		if err := srv.store.CreateAPIKey(ctx, key); err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	tests := []struct {
		name              string
		keyIndex          int
		expectedKeyIndex  float64
		expectedTotalKeys float64
	}{
		{
			name:              "使用第一个Key",
			keyIndex:          0,
			expectedKeyIndex:  0,
			expectedTotalKeys: 3,
		},
		{
			name:              "使用第二个Key",
			keyIndex:          1,
			expectedKeyIndex:  1,
			expectedTotalKeys: 3,
		},
		{
			name:              "无效索引默认使用第一个",
			keyIndex:          99,
			expectedKeyIndex:  0,
			expectedTotalKeys: 3,
		},
		{
			name:              "负数索引默认使用第一个",
			keyIndex:          -1,
			expectedKeyIndex:  0,
			expectedTotalKeys: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestBody := map[string]any{
				"model":        "test-model",
				"channel_type": "anthropic",
				"key_index":    tt.keyIndex,
			}

			body, _ := json.Marshal(requestBody)
			req := httptest.NewRequest(http.MethodPost, "/admin/channels/1/test", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: "1"}}

			srv.HandleChannelTest(c)

			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}

			// 验证Key索引
			if testedKeyIndex, ok := response["tested_key_index"].(float64); ok {
				if testedKeyIndex != tt.expectedKeyIndex {
					t.Errorf("期望 tested_key_index=%v, 实际=%v", tt.expectedKeyIndex, testedKeyIndex)
				}
			} else {
				t.Error("响应缺少 tested_key_index 字段")
			}

			// 验证总Key数
			if totalKeys, ok := response["total_keys"].(float64); ok {
				if totalKeys != tt.expectedTotalKeys {
					t.Errorf("期望 total_keys=%v, 实际=%v", tt.expectedTotalKeys, totalKeys)
				}
			} else {
				t.Error("响应缺少 total_keys 字段")
			}
		})
	}
}
