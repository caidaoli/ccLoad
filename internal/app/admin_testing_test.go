package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

// TestHandleChannelTest 测试渠道测试功能
func TestHandleChannelTest(t *testing.T) {
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
			srv := newInMemoryServer(t)

			ctx := context.Background()

			// 设置测试数据(如果需要)
			if tt.setupData {
				cfg := &model.Config{
					ID:           1,
					Name:         "test-channel",
					URL:          "http://test.example.com",
					Priority:     1,
					ModelEntries: []model.ModelEntry{{Model: "test-model", RedirectModel: ""}},
					Enabled:      true,
				}
				_, err := srv.store.CreateConfig(ctx, cfg)
				if err != nil {
					t.Fatalf("创建测试渠道失败: %v", err)
				}
			}

			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+tt.channelID+"/test", tt.requestBody))
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 调用处理函数
			srv.HandleChannelTest(c)

			// 验证响应状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d, 实际 %d, 响应: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			resp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
			if resp.Success != tt.expectSuccess {
				t.Errorf("期望 success=%v, 实际=%v, error=%q", tt.expectSuccess, resp.Success, resp.Error)
			}
		})
	}
}
