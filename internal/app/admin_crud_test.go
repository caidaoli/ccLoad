package app

import (
	"bytes"
	"ccLoad/internal/model"
	"ccLoad/internal/storage/sqlite"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// setupAdminTestServer 创建测试服务器
func setupAdminTestServer(t *testing.T) (*Server, *sqlite.SQLiteStore, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/admin_crud_test.db"
	store, err := sqlite.NewSQLiteStore(tmpDB, nil)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	server := &Server{
		store: store,
	}

	cleanup := func() {
		store.Close()
	}

	return server, store, cleanup
}

// TestHandleListChannels 测试列表查询
func TestHandleListChannels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试数据
	for i := 1; i <= 3; i++ {
		cfg := &model.Config{
			Name:     "Test-Channel-" + string(rune('A'-1+i)),
			URL:      "https://api.example.com",
			Priority: i * 10,
			Models:   []string{"model-1", "model-2"},
			Enabled:  true,
		}
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 模拟请求
	req := httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	// 调用处理函数
	server.handleListChannels(c)

	// 验证响应
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200，实际%d", w.Code)
	}

	var resp struct {
		Success bool            `json:"success"`
		Data    []*model.Config `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if !resp.Success {
		t.Error("期望success=true")
	}

	if len(resp.Data) != 3 {
		t.Errorf("期望3个渠道，实际%d个", len(resp.Data))
	}

	// 验证按优先级降序排序
	if len(resp.Data) >= 2 {
		if resp.Data[0].Priority < resp.Data[1].Priority {
			t.Error("渠道应该按优先级降序排序")
		}
	}

	t.Logf("✅ 列表查询成功: 返回%d个渠道", len(resp.Data))
}

// TestHandleCreateChannel 测试创建渠道
func TestHandleCreateChannel(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	tests := []struct {
		name           string
		payload        ChannelRequest
		expectedStatus int
		checkSuccess   bool
	}{
		{
			name: "成功创建单Key渠道",
			payload: ChannelRequest{
				Name:     "New-Channel",
				APIKey:   "sk-test-key",
				URL:      "https://api.new.com",
				Priority: 100,
				Models:   []string{"gpt-4"},
				Enabled:  true,
			},
			expectedStatus: http.StatusCreated,
			checkSuccess:   true,
		},
		{
			name: "成功创建多Key渠道",
			payload: ChannelRequest{
				Name:        "Multi-Key-Channel",
				APIKey:      "sk-key1,sk-key2,sk-key3",
				URL:         "https://api.multi.com",
				Priority:    90,
				Models:      []string{"claude-3"},
				KeyStrategy: "round_robin",
				Enabled:     true,
			},
			expectedStatus: http.StatusCreated,
			checkSuccess:   true,
		},
		{
			name: "缺少name字段",
			payload: ChannelRequest{
				Name:     "",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 50,
				Models:   []string{"model"},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
		{
			name: "缺少api_key字段",
			payload: ChannelRequest{
				Name:     "Test",
				APIKey:   "",
				URL:      "https://api.com",
				Priority: 50,
				Models:   []string{"model"},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
		{
			name: "缺少models字段",
			payload: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 50,
				Models:   []string{},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 序列化请求
			body, _ := json.Marshal(tt.payload)
			req := httptest.NewRequest(http.MethodPost, "/admin/channels", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			// 调用处理函数
			server.handleCreateChannel(c)

			// 验证状态码
			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码%d，实际%d", tt.expectedStatus, w.Code)
			}

			// 验证响应
			if tt.checkSuccess {
				var resp struct {
					Success bool          `json:"success"`
					Data    *model.Config `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("解析响应失败: %v", err)
				}

				if !resp.Success {
					t.Error("期望success=true")
				}

				if resp.Data == nil {
					t.Error("期望返回创建的渠道数据")
				} else {
					if resp.Data.Name != tt.payload.Name {
						t.Errorf("期望名称%s，实际%s", tt.payload.Name, resp.Data.Name)
					}
				}

				t.Logf("✅ 创建成功: ID=%d, Name=%s", resp.Data.ID, resp.Data.Name)
			}
		})
	}
}

// TestHandleGetChannel 测试获取单个渠道
func TestHandleGetChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "Test-Get-Channel",
		URL:      "https://api.example.com",
		Priority: 100,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	tests := []struct {
		name           string
		channelID      string
		expectedStatus int
		checkSuccess   bool
	}{
		{
			name:           "获取存在的渠道",
			channelID:      "1",
			expectedStatus: http.StatusOK,
			checkSuccess:   true,
		},
		{
			name:           "获取不存在的渠道",
			channelID:      "999",
			expectedStatus: http.StatusNotFound,
			checkSuccess:   false,
		},
		{
			name:           "无效的渠道ID",
			channelID:      "invalid",
			expectedStatus: http.StatusNotFound, // strconv.ParseInt失败会传入0，查不到返回404
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/channels/"+tt.channelID, nil)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 从Params中解析ID并调用
			id, _ := strconv.ParseInt(tt.channelID, 10, 64)
			server.handleGetChannel(c, id)

			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码%d，实际%d", tt.expectedStatus, w.Code)
			}

			if tt.checkSuccess {
				var resp struct {
					Success bool          `json:"success"`
					Data    *model.Config `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("解析响应失败: %v", err)
				}

				if !resp.Success {
					t.Error("期望success=true")
				}

				if resp.Data.ID != created.ID {
					t.Errorf("期望ID=%d，实际%d", created.ID, resp.Data.ID)
				}

				t.Logf("✅ 获取成功: ID=%d, Name=%s", resp.Data.ID, resp.Data.Name)
			}
		})
	}
}

// TestHandleUpdateChannel 测试更新渠道
func TestHandleUpdateChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "Original-Name",
		URL:      "https://api.original.com",
		Priority: 50,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建API Key
	err = store.CreateAPIKey(ctx, &model.APIKey{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-original-key",
		KeyStrategy: "sequential",
	})
	if err != nil {
		t.Fatalf("创建API Key失败: %v", err)
	}

	tests := []struct {
		name           string
		channelID      string
		payload        ChannelRequest
		expectedStatus int
		checkSuccess   bool
	}{
		{
			name:      "成功更新渠道",
			channelID: "1",
			payload: ChannelRequest{
				Name:     "Updated-Name",
				APIKey:   "sk-updated-key",
				URL:      "https://api.updated.com",
				Priority: 100,
				Models:   []string{"model-1", "model-2"},
				Enabled:  false,
			},
			expectedStatus: http.StatusOK,
			checkSuccess:   true,
		},
		{
			name:      "更新不存在的渠道",
			channelID: "999",
			payload: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 50,
				Models:   []string{"model"},
			},
			expectedStatus: http.StatusNotFound,
			checkSuccess:   false,
		},
		{
			name:      "无效的请求数据",
			channelID: "1",
			payload: ChannelRequest{
				Name:     "",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 50,
				Models:   []string{"model"},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.payload)
			req := httptest.NewRequest(http.MethodPut, "/admin/channels/"+tt.channelID, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 从Params中解析ID并调用
			id, _ := strconv.ParseInt(tt.channelID, 10, 64)
			server.handleUpdateChannel(c, id)

			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码%d，实际%d", tt.expectedStatus, w.Code)
			}

			if tt.checkSuccess {
				var resp struct {
					Success bool          `json:"success"`
					Data    *model.Config `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("解析响应失败: %v", err)
				}

				if !resp.Success {
					t.Error("期望success=true")
				}

				if resp.Data.Name != tt.payload.Name {
					t.Errorf("期望名称%s，实际%s", tt.payload.Name, resp.Data.Name)
				}

				if resp.Data.Priority != tt.payload.Priority {
					t.Errorf("期望优先级%d，实际%d", tt.payload.Priority, resp.Data.Priority)
				}

				t.Logf("✅ 更新成功: Name=%s, Priority=%d", resp.Data.Name, resp.Data.Priority)
			}
		})
	}
}

// TestHandleDeleteChannel 测试删除渠道
func TestHandleDeleteChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "To-Be-Deleted",
		URL:      "https://api.example.com",
		Priority: 50,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	tests := []struct {
		name           string
		channelID      string
		expectedStatus int
		checkSuccess   bool
	}{
		{
			name:           "成功删除渠道",
			channelID:      "1",
			expectedStatus: http.StatusOK, // Gin测试: c.Status()未写入响应时默认200
			checkSuccess:   true,
		},
		{
			name:           "删除不存在的渠道",
			channelID:      "999",
			expectedStatus: http.StatusOK, // DeleteConfig对不存在ID返回nil，不触发错误分支
			checkSuccess:   true,          // 需要验证删除效果
		},
		{
			name:           "无效的渠道ID",
			channelID:      "invalid",
			expectedStatus: http.StatusOK, // strconv.ParseInt失败传入0，Delete(0)也不报错
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, "/admin/channels/"+tt.channelID, nil)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Params = gin.Params{{Key: "id", Value: tt.channelID}}

			// 从Params中解析ID并调用
			id, _ := strconv.ParseInt(tt.channelID, 10, 64)
			server.handleDeleteChannel(c, id)

			if w.Code != tt.expectedStatus {
				t.Errorf("期望状态码%d，实际%d，响应体: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			if tt.checkSuccess {
				// 删除成功，无响应体
				// 验证渠道是否真的被删除（仅对首个测试）
				if tt.channelID == "1" {
					_, err := store.GetConfig(ctx, created.ID)
					if err == nil {
						t.Error("渠道应该已被删除")
					}
					t.Logf("✅ 删除成功: 渠道ID=%d已被删除", created.ID)
				} else {
					t.Logf("✅ 删除操作成功(幂等): ID=%s", tt.channelID)
				}
			}
		})
	}
}

// TestHandleGetChannelKeys 测试获取渠道的API Keys
func TestHandleGetChannelKeys(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 创建测试渠道
	cfg := &model.Config{
		Name:     "Test-Keys-Channel",
		URL:      "https://api.example.com",
		Priority: 100,
		Models:   []string{"model-1"},
		Enabled:  true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建多个API Keys
	for i := 0; i < 3; i++ {
		err = store.CreateAPIKey(ctx, &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      "sk-test-key-" + string(rune('0'+i)),
			KeyStrategy: "sequential",
		})
		if err != nil {
			t.Fatalf("创建API Key失败: %v", err)
		}
	}

	// 测试获取Keys
	req := httptest.NewRequest(http.MethodGet, "/admin/channels/1/keys", nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "1"}}

	// 从Params中解析ID并调用
	id, _ := strconv.ParseInt("1", 10, 64)
	server.handleGetChannelKeys(c, id)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200，实际%d", w.Code)
	}

	var resp struct {
		Success bool            `json:"success"`
		Data    []*model.APIKey `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if !resp.Success {
		t.Error("期望success=true")
	}

	if len(resp.Data) != 3 {
		t.Errorf("期望3个API Keys，实际%d个", len(resp.Data))
	}

	// 验证Keys按KeyIndex排序
	for i, key := range resp.Data {
		if key.KeyIndex != i {
			t.Errorf("Keys应该按KeyIndex排序，位置%d期望KeyIndex=%d，实际%d", i, i, key.KeyIndex)
		}
	}

	t.Logf("✅ 获取渠道Keys成功: 返回%d个Keys", len(resp.Data))
}

// TestChannelRequestValidate 测试ChannelRequest验证
func TestChannelRequestValidate(t *testing.T) {
	tests := []struct {
		name      string
		req       ChannelRequest
		wantError bool
		errorMsg  string
	}{
		{
			name: "有效请求",
			req: ChannelRequest{
				Name:     "Valid-Channel",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []string{"model-1"},
			},
			wantError: false,
		},
		{
			name: "缺少name",
			req: ChannelRequest{
				Name:     "",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []string{"model-1"},
			},
			wantError: true,
			errorMsg:  "name cannot be empty",
		},
		{
			name: "缺少api_key",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []string{"model-1"},
			},
			wantError: true,
			errorMsg:  "api_key cannot be empty",
		},
		{
			name: "缺少models",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []string{},
			},
			wantError: true,
			errorMsg:  "models cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()

			if tt.wantError {
				if err == nil {
					t.Error("期望返回错误，但成功了")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("期望错误消息'%s'，实际'%s'", tt.errorMsg, err.Error())
				} else {
					t.Logf("✅ 验证错误正确: %s", err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("期望成功，但返回错误: %v", err)
				} else {
					t.Logf("✅ 验证成功")
				}
			}
		})
	}
}
