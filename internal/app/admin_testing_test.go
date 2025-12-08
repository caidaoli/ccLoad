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
