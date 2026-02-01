package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

// setupAdminTestServer 创建测试服务器
func setupAdminTestServer(t *testing.T) (*Server, storage.Store, func()) {
	t.Helper()

	tmpDB := t.TempDir() + "/admin_crud_test.db"
	store, err := storage.CreateSQLiteStore(tmpDB)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	statsCache := NewStatsCache(store)
	server := &Server{
		store:      store,
		statsCache: statsCache,
	}

	cleanup := func() {
		statsCache.Close() // 关闭后台清理 goroutine
		_ = store.Close()
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
			ModelEntries: []model.ModelEntry{
				{Model: "model-1", RedirectModel: ""},
				{Model: "model-2", RedirectModel: ""},
			},
			Enabled: true,
		}
		_, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("创建测试渠道失败: %v", err)
		}
	}

	// 模拟请求
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))

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
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)

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
				Models:   []model.ModelEntry{{Model: "gpt-4", RedirectModel: ""}},
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
				Models:      []model.ModelEntry{{Model: "claude-3", RedirectModel: ""}},
				KeyStrategy: model.KeyStrategyRoundRobin,
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
				Models:   []model.ModelEntry{{Model: "model", RedirectModel: ""}},
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
				Models:   []model.ModelEntry{{Model: "model", RedirectModel: ""}},
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
				Models:   []model.ModelEntry{},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", tt.payload))

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
				mustUnmarshalJSON(t, w.Body.Bytes(), &resp)

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
		Name:         "Test-Get-Channel",
		URL:          "https://api.example.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
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
			c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+tt.channelID, nil))
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
				mustUnmarshalJSON(t, w.Body.Bytes(), &resp)

				if !resp.Success {
					t.Error("期望success=true")
				}

				if resp.Data.ID != created.ID {
					t.Errorf("期望ID=%d，实际%d", created.ID, resp.Data.ID)
				}

				// 治本：渠道详情不应包含明文Key或策略派生字段（Keys 只能通过 /keys 端点获取）
				rawResp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
				var leakCheck struct {
					APIKey      *string `json:"api_key"`
					KeyStrategy *string `json:"key_strategy"`
				}
				if err := json.Unmarshal(rawResp.Data, &leakCheck); err != nil {
					t.Fatalf("unmarshal data failed: %v", err)
				}
				if leakCheck.APIKey != nil {
					t.Fatalf("渠道详情不应返回 api_key 字段")
				}
				if leakCheck.KeyStrategy != nil {
					t.Fatalf("渠道详情不应返回 key_strategy 字段")
				}
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
		Name:         "Original-Name",
		URL:          "https://api.original.com",
		Priority:     50,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建API Key
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-original-key",
		KeyStrategy: model.KeyStrategySequential,
	}})
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
				Models: []model.ModelEntry{
					{Model: "model-1", RedirectModel: ""},
					{Model: "model-2", RedirectModel: ""},
				},
				Enabled: false,
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
				Models:   []model.ModelEntry{{Model: "model", RedirectModel: ""}},
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
				Models:   []model.ModelEntry{{Model: "model", RedirectModel: ""}},
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+tt.channelID, tt.payload))
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
				mustUnmarshalJSON(t, w.Body.Bytes(), &resp)

				if !resp.Success {
					t.Error("期望success=true")
				}

				if resp.Data.Name != tt.payload.Name {
					t.Errorf("期望名称%s，实际%s", tt.payload.Name, resp.Data.Name)
				}

				if resp.Data.Priority != tt.payload.Priority {
					t.Errorf("期望优先级%d，实际%d", tt.payload.Priority, resp.Data.Priority)
				}
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
		Name:         "To-Be-Deleted",
		URL:          "https://api.example.com",
		Priority:     50,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
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
			c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/"+tt.channelID, nil))
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
		Name:         "Test-Keys-Channel",
		URL:          "https://api.example.com",
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	// 创建多个API Keys
	keys := make([]*model.APIKey, 3)
	for i := 0; i < 3; i++ {
		keys[i] = &model.APIKey{
			ChannelID:   created.ID,
			KeyIndex:    i,
			APIKey:      "sk-test-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
		}
	}
	if err = store.CreateAPIKeysBatch(ctx, keys); err != nil {
		t.Fatalf("批量创建API Keys失败: %v", err)
	}

	// 测试获取Keys
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/1/keys", nil))
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
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)

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
}

// TestChannelRequestValidate 测试ChannelRequest验证
func TestChannelRequestValidate(t *testing.T) {
	tests := []struct {
		name            string
		req             ChannelRequest
		wantError       bool
		errorMsg        string
		expectNormalize string // URL 标准化后的期望值
	}{
		{
			name: "有效请求",
			req: ChannelRequest{
				Name:     "Valid-Channel",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: false,
		},
		{
			name: "URL为空",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "  ",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url cannot be empty",
		},
		{
			name: "URL缺少scheme",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "api.com",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "invalid url: \"api.com\"",
		},
		{
			name: "URL scheme非法",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "ftp://api.com",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "invalid url scheme: \"ftp\" (allowed: http, https)",
		},
		{
			name: "URL包含userinfo",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://user:pass@api.com",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url must not contain user info",
		},
		{
			name: "URL包含query",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com?x=1",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url must not contain query or fragment",
		},
		{
			name: "URL包含fragment",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com#x",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url must not contain query or fragment",
		},
		{
			name: "URL包含/v1路径（禁止）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com/v1",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url should not contain API endpoint path like /v1 (current path: \"/v1\")",
		},
		{
			name: "URL包含/v1/messages路径（禁止）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com/v1/messages",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "url should not contain API endpoint path like /v1 (current path: \"/v1/messages\")",
		},
		{
			name: "URL包含/api路径（允许）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://example.com/api",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://example.com/api",
		},
		{
			name: "URL包含/openai路径（允许）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://example.com/openai/",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://example.com/openai",
		},
		{
			name: "URL标准化：去掉trailing slash",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com/",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://api.com",
		},
		{
			name: "URL标准化：保留端口号",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "https://api.com:8080/",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://api.com:8080",
		},
		{
			name: "URL标准化：HTTP协议也支持",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URL:      "http://localhost:8080/",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "http://localhost:8080",
		},
		{
			name: "缺少name",
			req: ChannelRequest{
				Name:     "",
				APIKey:   "sk-test",
				URL:      "https://api.com",
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
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
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
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
				Models:   []model.ModelEntry{},
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
				}
			} else {
				if err != nil {
					t.Errorf("期望成功，但返回错误: %v", err)
				} else {
					// 验证 URL 标准化
					if tt.expectNormalize != "" && tt.req.URL != tt.expectNormalize {
						t.Errorf("URL标准化失败：期望 %q，实际 %q", tt.expectNormalize, tt.req.URL)
					}
				}
			}
		})
	}
}
