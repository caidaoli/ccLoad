package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
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
		client:     newTestHTTPClient(),
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
				Name:                  "New-Channel",
				APIKey:                "sk-test-key",
				URL:                   "https://api.new.com",
				Priority:              100,
				Models:                []model.ModelEntry{{Model: "gpt-4", RedirectModel: ""}},
				Enabled:               true,
				ScheduledCheckEnabled: true,
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
			name: "成功创建多URL渠道",
			payload: ChannelRequest{
				Name:     "Multi-URL-Channel",
				APIKey:   "sk-test-key",
				URL:      "https://us.api.com\nhttps://eu.api.com",
				Priority: 80,
				Models:   []model.ModelEntry{{Model: "gpt-4o-mini", RedirectModel: ""}},
				Enabled:  true,
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
					if resp.Data.ScheduledCheckEnabled != tt.payload.ScheduledCheckEnabled {
						t.Errorf("期望 scheduled_check_enabled=%v，实际=%v", tt.payload.ScheduledCheckEnabled, resp.Data.ScheduledCheckEnabled)
					}
				}
			}
		})
	}
}

func TestHandleCreateChannel_PersistsProtocolTransforms(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	payload := ChannelRequest{
		Name:               "Transform-Channel",
		APIKey:             "sk-transform",
		URL:                "https://transform.example.com",
		Priority:           42,
		ChannelType:        "gemini",
		ProtocolTransforms: []string{"openai", "anthropic"},
		Models: []model.ModelEntry{
			{Model: "gemini-2.5-pro", RedirectModel: ""},
		},
		Enabled: true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("期望状态码 %d，实际 %d，响应体: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp struct {
		Success bool          `json:"success"`
		Data    *model.Config `json:"data"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if resp.Data == nil {
		t.Fatalf("期望返回创建后的渠道数据")
	}
	if len(resp.Data.ProtocolTransforms) != 2 || resp.Data.ProtocolTransforms[0] != "anthropic" || resp.Data.ProtocolTransforms[1] != "openai" {
		t.Fatalf("返回的 protocol transforms 不正确: %#v", resp.Data.ProtocolTransforms)
	}

	stored, err := store.GetConfig(context.Background(), resp.Data.ID)
	if err != nil {
		t.Fatalf("查询持久化渠道失败: %v", err)
	}
	if len(stored.ProtocolTransforms) != 2 || stored.ProtocolTransforms[0] != "anthropic" || stored.ProtocolTransforms[1] != "openai" {
		t.Fatalf("持久化 protocol transforms 不正确: %#v", stored.ProtocolTransforms)
	}
}

func TestHandleCreateChannel_AllowsUpstreamModeForExtraProtocols(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	payload := map[string]any{
		"name":                    "Upstream-Mode-Channel",
		"api_key":                 "sk-upstream-mode",
		"url":                     "https://upstream-mode.example.com",
		"priority":                23,
		"channel_type":            "anthropic",
		"protocol_transforms":     []string{"gemini"},
		"protocol_transform_mode": "upstream",
		"models": []map[string]any{
			{"model": "claude-sonnet-4-5"},
		},
		"enabled": true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("期望 status=%d 允许 upstream 模式创建，实际=%d，响应体: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), `"protocol_transform_mode":"upstream"`) {
		t.Fatalf("期望响应包含 protocol_transform_mode=upstream，实际响应: %s", w.Body.String())
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
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key-0", KeyStrategy: model.KeyStrategyRoundRobin},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-test-key-1", KeyStrategy: model.KeyStrategyRoundRobin},
	})
	if err != nil {
		t.Fatalf("创建测试 API Keys 失败: %v", err)
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
				rawResp := mustParseAPIResponse[json.RawMessage](t, w.Body.Bytes())
				if !rawResp.Success {
					t.Error("期望success=true")
				}

				var detail struct {
					ID          int64   `json:"id"`
					APIKey      *string `json:"api_key"`
					KeyStrategy string  `json:"key_strategy"`
				}
				if err := json.Unmarshal(rawResp.Data, &detail); err != nil {
					t.Fatalf("unmarshal data failed: %v", err)
				}

				if detail.ID != created.ID {
					t.Errorf("期望ID=%d，实际%d", created.ID, detail.ID)
				}
				if detail.APIKey != nil {
					t.Fatalf("渠道详情不应返回 api_key 字段")
				}
				if detail.KeyStrategy != model.KeyStrategyRoundRobin {
					t.Fatalf("期望 key_strategy=%q，实际=%q", model.KeyStrategyRoundRobin, detail.KeyStrategy)
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
				Enabled:               false,
				ScheduledCheckEnabled: true,
			},
			expectedStatus: http.StatusOK,
			checkSuccess:   true,
		},
		{
			name:      "重复模型应被提前拦截",
			channelID: "1",
			payload: ChannelRequest{
				Name:     "Updated-Name",
				APIKey:   "sk-updated-key",
				URL:      "https://api.updated.com",
				Priority: 100,
				Models: []model.ModelEntry{
					{Model: "gpt-5.2", RedirectModel: "gpt-5.2"},
					{Model: "gpt-5.2", RedirectModel: "gpt-5.2-2c"},
				},
				Enabled: true,
			},
			expectedStatus: http.StatusBadRequest,
			checkSuccess:   false,
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
				if resp.Data.ScheduledCheckEnabled != tt.payload.ScheduledCheckEnabled {
					t.Errorf("期望 scheduled_check_enabled=%v，实际=%v", tt.payload.ScheduledCheckEnabled, resp.Data.ScheduledCheckEnabled)
				}
			}
		})
	}
}

func TestHandleUpdateChannel_UpdatesProtocolTransforms(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:               "Update-Transform-Channel",
		URL:                "https://update-transform.example.com",
		Priority:           10,
		Enabled:            true,
		ChannelType:        "gemini",
		ProtocolTransforms: []string{"openai"},
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-pro", RedirectModel: ""},
		},
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-update-transform", KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 API Key 失败: %v", err)
	}

	payload := ChannelRequest{
		Name:               "Update-Transform-Channel",
		APIKey:             "sk-update-transform",
		URL:                "https://update-transform.example.com",
		Priority:           10,
		ChannelType:        "gemini",
		ProtocolTransforms: []string{"codex", "anthropic"},
		Models: []model.ModelEntry{
			{Model: "gemini-2.5-pro", RedirectModel: ""},
		},
		Enabled: true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(created.ID, 10), payload))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 %d，实际 %d，响应体: %s", http.StatusOK, w.Code, w.Body.String())
	}

	updated, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询更新后渠道失败: %v", err)
	}
	if len(updated.ProtocolTransforms) != 2 || updated.ProtocolTransforms[0] != "anthropic" || updated.ProtocolTransforms[1] != "codex" {
		t.Fatalf("更新后的 protocol transforms 不正确: %#v", updated.ProtocolTransforms)
	}
}

func TestHandleUpdateChannel_ClearCooldownShouldTakeEffectImmediately(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.cooldownManager = cooldown.NewManager(store, server)

	ctx := context.Background()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "Cooldown-Channel",
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-test-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 API Key 失败: %v", err)
	}

	if err := store.SetChannelCooldown(ctx, created.ID, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}

	// 先查询一次，预热冷却缓存（复现“更新后仍显示旧冷却”问题）
	c1, w1 := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c1)
	if w1.Code != http.StatusOK {
		t.Fatalf("预热列表请求失败: %d", w1.Code)
	}
	before := mustParseAPIResponse[[]ChannelWithCooldown](t, w1.Body.Bytes())
	if len(before.Data) != 1 || before.Data[0].CooldownRemainingMS <= 0 {
		t.Fatalf("预期渠道处于冷却中，实际 cooldown_remaining_ms=%d", before.Data[0].CooldownRemainingMS)
	}

	updatePayload := ChannelRequest{
		Name:     "Cooldown-Channel-Updated",
		APIKey:   "sk-test-key",
		URL:      "https://api.updated.com",
		Priority: 20,
		Models: []model.ModelEntry{
			{Model: "model-1", RedirectModel: ""},
		},
		Enabled: true,
	}

	c2, w2 := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(created.ID, 10), updatePayload))
	c2.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(c2, created.ID)
	if w2.Code != http.StatusOK {
		t.Fatalf("更新渠道失败: %d body=%s", w2.Code, w2.Body.String())
	}

	// 立即再次查询，必须看不到旧冷却状态
	c3, w3 := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c3)
	if w3.Code != http.StatusOK {
		t.Fatalf("更新后列表请求失败: %d", w3.Code)
	}
	after := mustParseAPIResponse[[]ChannelWithCooldown](t, w3.Body.Bytes())
	if len(after.Data) != 1 {
		t.Fatalf("预期1个渠道，实际%d个", len(after.Data))
	}
	if after.Data[0].CooldownUntil != nil || after.Data[0].CooldownRemainingMS > 0 {
		t.Fatalf("预期冷却已清除，实际 cooldown_until=%v cooldown_remaining_ms=%d", after.Data[0].CooldownUntil, after.Data[0].CooldownRemainingMS)
	}
}

func TestHandleUpdateChannel_PrunesURLSelectorState(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "update-prune",
		URL:          "https://old-1.example.com\nhttps://keep.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   cfg.ID,
		KeyIndex:    0,
		APIKey:      "sk-update-prune",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 key 失败: %v", err)
	}

	// 另一渠道状态，用于验证不会被误删
	otherCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "other-channel",
		URL:          "https://other.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}

	server.urlSelector.RecordLatency(cfg.ID, "https://old-1.example.com", 20*time.Millisecond)
	server.urlSelector.RecordLatency(cfg.ID, "https://keep.example.com", 30*time.Millisecond)
	server.urlSelector.CooldownURL(cfg.ID, "https://old-1.example.com")
	server.urlSelector.RecordLatency(otherCfg.ID, "https://other.example.com", 40*time.Millisecond)

	payload := ChannelRequest{
		Name:     "update-prune",
		APIKey:   "sk-update-prune",
		URL:      "https://keep.example.com\nhttps://new.example.com",
		Priority: 11,
		Models:   []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:  true,
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(cfg.ID, 10), payload))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(cfg.ID, 10)}}
	server.handleUpdateChannel(c, cfg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, ok := server.urlSelector.latencies[urlKey{channelID: cfg.ID, url: "https://old-1.example.com"}]; ok {
		t.Fatalf("expected old url latency state removed after update")
	}
	if _, ok := server.urlSelector.cooldowns[urlKey{channelID: cfg.ID, url: "https://old-1.example.com"}]; ok {
		t.Fatalf("expected old url cooldown state removed after update")
	}
	if _, ok := server.urlSelector.latencies[urlKey{channelID: cfg.ID, url: "https://keep.example.com"}]; !ok {
		t.Fatalf("expected kept url latency state preserved after update")
	}
	if _, ok := server.urlSelector.latencies[urlKey{channelID: otherCfg.ID, url: "https://other.example.com"}]; !ok {
		t.Fatalf("expected other channel state preserved")
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
			expectedStatus: http.StatusNotFound,
			checkSuccess:   false,
		},
		{
			name:           "无效的渠道ID",
			channelID:      "invalid",
			expectedStatus: http.StatusNotFound,
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

func TestHandleDeleteChannel_RemovesURLSelectorState(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	targetCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "delete-prune-target",
		URL:          "https://target-1.example.com\nhttps://target-2.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建目标渠道失败: %v", err)
	}
	otherCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "delete-prune-other",
		URL:          "https://other.example.com",
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}

	server.urlSelector.RecordLatency(targetCfg.ID, "https://target-1.example.com", 10*time.Millisecond)
	server.urlSelector.RecordLatency(targetCfg.ID, "https://target-2.example.com", 20*time.Millisecond)
	server.urlSelector.CooldownURL(targetCfg.ID, "https://target-1.example.com")
	server.urlSelector.RecordLatency(otherCfg.ID, "https://other.example.com", 30*time.Millisecond)

	c, w := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/"+strconv.FormatInt(targetCfg.ID, 10), nil))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(targetCfg.ID, 10)}}
	server.handleDeleteChannel(c, targetCfg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for key := range server.urlSelector.latencies {
		if key.channelID == targetCfg.ID {
			t.Fatalf("expected deleted channel latency state removed, found key=%+v", key)
		}
	}
	for key := range server.urlSelector.cooldowns {
		if key.channelID == targetCfg.ID {
			t.Fatalf("expected deleted channel cooldown state removed, found key=%+v", key)
		}
	}
	if _, ok := server.urlSelector.latencies[urlKey{channelID: otherCfg.ID, url: "https://other.example.com"}]; !ok {
		t.Fatalf("expected other channel state preserved")
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
