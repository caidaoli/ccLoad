package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

type failAPIKeyAllowedModelsStore struct {
	storage.Store
}

func (s *failAPIKeyAllowedModelsStore) UpdateAPIKeyModelScopes(context.Context, int64, map[int]model.APIKeyModelScope) error {
	return errors.New("forced API key model scope update failure")
}

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
		store:             store,
		statsCache:        statsCache,
		client:            newTestHTTPClient(),
		antigravityClient: newTestHTTPClient(),
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
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
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

func TestHandleListChannelsIncludesActiveModelCooldowns(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "model-cooldown-list",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     100,
		ModelEntries: []model.ModelEntry{{Model: "external-model", RedirectModel: "upstream-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	until := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	if err := store.SetModelCooldown(ctx, created.ID, "upstream-model", until); err != nil {
		t.Fatalf("设置模型冷却失败: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	server.handleListChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    []struct {
			ID             int64               `json:"id"`
			ModelCooldowns []ModelCooldownInfo `json:"model_cooldowns"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.Success || len(resp.Data) != 1 {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	if resp.Data[0].ID != created.ID {
		t.Fatalf("id=%d, want %d", resp.Data[0].ID, created.ID)
	}
	if len(resp.Data[0].ModelCooldowns) != 1 {
		t.Fatalf("model_cooldowns=%v, want one active cooldown", resp.Data[0].ModelCooldowns)
	}
	got := resp.Data[0].ModelCooldowns[0]
	if got.Model != "upstream-model" {
		t.Fatalf("model=%q, want upstream-model", got.Model)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(until) {
		t.Fatalf("cooldown_until=%v, want %v", got.CooldownUntil, until)
	}
	if got.CooldownRemainingMS <= 0 {
		t.Fatalf("cooldown_remaining_ms=%d, want > 0", got.CooldownRemainingMS)
	}
}

func TestHandleListChannelsExactAndFuzzyFilters(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	fixtures := []struct {
		name  string
		model string
	}{
		{name: "gpt-5.4", model: "gpt-5.4"},
		{name: "gpt-5.4-mini", model: "gpt-5.4-mini"},
		{name: "claude", model: "claude-3-7-sonnet"},
	}

	for _, fixture := range fixtures {
		_, err := store.CreateConfig(ctx, &model.Config{
			Name:     fixture.name,
			URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority: 1,
			ModelEntries: []model.ModelEntry{
				{Model: fixture.model},
			},
			Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateConfig(%s) failed: %v", fixture.name, err)
		}
	}

	tests := []struct {
		name      string
		query     string
		wantNames []string
	}{
		{
			name:      "exact channel name",
			query:     "channel_name=gpt-5.4",
			wantNames: []string{"gpt-5.4"},
		},
		{
			name:      "fuzzy channel name",
			query:     "search=gpt-5.4",
			wantNames: []string{"gpt-5.4", "gpt-5.4-mini"},
		},
		{
			name:      "exact model",
			query:     "model=gpt-5.4",
			wantNames: []string{"gpt-5.4"},
		},
		{
			name:      "fuzzy model",
			query:     "model_like=gpt-5.4",
			wantNames: []string{"gpt-5.4", "gpt-5.4-mini"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels?"+tt.query+"&limit=20&offset=0", nil))

			server.handleListChannels(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
			}

			resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
			if resp.Count != len(tt.wantNames) {
				t.Fatalf("count=%d, want %d body=%s", resp.Count, len(tt.wantNames), w.Body.String())
			}
			if len(resp.Data) != len(tt.wantNames) {
				t.Fatalf("len(data)=%d, want %d body=%s", len(resp.Data), len(tt.wantNames), w.Body.String())
			}

			gotNames := make([]string, 0, len(resp.Data))
			for _, item := range resp.Data {
				gotNames = append(gotNames, item.Name)
			}
			sort.Strings(gotNames)
			if !slices.Equal(gotNames, tt.wantNames) {
				t.Fatalf("names=%v, want %v", gotNames, tt.wantNames)
			}
		})
	}
}

func TestHandleListChannelsFiltersByAuthenticationType(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	fixtures := []*model.Config{
		{
			Name: "api-auth", URLs: model.ChannelURLs{{URL: "https://api.example.com"}},
			AuthType: model.AuthTypeAPIKey, Enabled: true,
		},
		{
			Name: "codex-auth", URLs: model.ChannelURLs{{URL: "https://chatgpt.com/backend-api/codex/responses"}},
			AuthType: model.AuthTypeCodexOAuth, OAuthCredential: `{}`, Enabled: true,
		},
		{
			Name: "antigravity-auth", URLs: model.ChannelURLs{{URL: "https://cloudcode-pa.googleapis.com"}},
			AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: `{}`, Enabled: true,
		},
	}
	for _, fixture := range fixtures {
		if _, err := store.CreateConfig(ctx, fixture); err != nil {
			t.Fatalf("CreateConfig(%s) failed: %v", fixture.Name, err)
		}
	}

	for _, tt := range []struct {
		authType string
		wantName string
	}{
		{authType: model.AuthTypeAPIKey, wantName: "api-auth"},
		{authType: model.AuthTypeCodexOAuth, wantName: "codex-auth"},
		{authType: model.AuthTypeAntigravityOAuth, wantName: "antigravity-auth"},
	} {
		t.Run(tt.authType, func(t *testing.T) {
			path := "/admin/channels?auth_type=" + tt.authType + "&limit=20&offset=0"
			c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
			server.handleListChannels(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
			if resp.Count != 1 || len(resp.Data) != 1 || resp.Data[0].Name != tt.wantName {
				t.Fatalf("response=%s, want only %q", w.Body.String(), tt.wantName)
			}
		})
	}
}

func TestChannelCooldownFilterIncludesChannelKeyAndModelCooldowns(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	channelIDs := make(map[string]int64)
	for _, fixture := range []struct {
		name  string
		model string
	}{
		{name: "channel-cooldown", model: "channel-model"},
		{name: "key-cooldown", model: "key-model"},
		{name: "model-cooldown", model: "model-target"},
		{name: "healthy", model: "healthy-model"},
	} {
		created, err := store.CreateConfig(ctx, &model.Config{
			Name:         fixture.name,
			URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
			Priority:     1,
			ModelEntries: []model.ModelEntry{{Model: fixture.model}},
			Enabled:      true,
		})
		if err != nil {
			t.Fatalf("CreateConfig(%s) failed: %v", fixture.name, err)
		}
		channelIDs[fixture.name] = created.ID
	}

	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   channelIDs["key-cooldown"],
		KeyIndex:    0,
		APIKey:      "sk-key-cooldown",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	until := time.Now().Add(10 * time.Minute)
	if err := store.SetChannelCooldown(ctx, channelIDs["channel-cooldown"], until); err != nil {
		t.Fatalf("SetChannelCooldown failed: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, channelIDs["key-cooldown"], 0, until); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}
	if err := store.SetModelCooldown(ctx, channelIDs["model-cooldown"], "model-target", until); err != nil {
		t.Fatalf("SetModelCooldown failed: %v", err)
	}

	wantNames := []string{"channel-cooldown", "key-cooldown", "model-cooldown"}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels?status=cooldown&limit=20&offset=0", nil))
	server.handleListChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
	gotNames := make([]string, 0, len(resp.Data))
	for _, item := range resp.Data {
		gotNames = append(gotNames, item.Name)
	}
	sort.Strings(gotNames)
	if resp.Count != len(wantNames) || !slices.Equal(gotNames, wantNames) {
		t.Fatalf("cooldown list count=%d names=%v, want count=%d names=%v", resp.Count, gotNames, len(wantNames), wantNames)
	}

	optionsContext, optionsWriter := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/filter-options?status=cooldown", nil))
	server.HandleChannelsFilterOptions(optionsContext)
	if optionsWriter.Code != http.StatusOK {
		t.Fatalf("filter options status=%d, want %d body=%s", optionsWriter.Code, http.StatusOK, optionsWriter.Body.String())
	}
	var options struct {
		ChannelNames []string `json:"channel_names"`
	}
	mustUnmarshalAPIResponseData(t, optionsWriter.Body.Bytes(), &options)
	if !slices.Equal(options.ChannelNames, wantNames) {
		t.Fatalf("cooldown filter option names=%v, want %v", options.ChannelNames, wantNames)
	}
}

func TestHandleListChannelsTypeFilterUsesUpstreamProtocolOnly(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	fixtures := []*model.Config{
		{
			Name:         "native-openai",
			URLs:         model.ChannelURLs{{URL: "https://openai.example.com", Protocols: []string{"openai"}}},
			Priority:     10,
			ModelEntries: []model.ModelEntry{{Model: "native-model"}},
			Enabled:      true,
		},
		{
			Name:         "anthropic-channel",
			URLs:         model.ChannelURLs{{URL: "https://anthropic.example.com", Protocols: []string{"anthropic", "openai"}}},
			Priority:     20,
			ModelEntries: []model.ModelEntry{{Model: "bridge-model"}},
			Enabled:      true,
		},
		{
			Name:         "codex-channel",
			URLs:         model.ChannelURLs{{URL: "https://codex.example.com", Protocols: []string{"codex"}}},
			Priority:     30,
			ModelEntries: []model.ModelEntry{{Model: "skip-model"}},
			Enabled:      true,
		},
	}
	for _, fixture := range fixtures {
		if _, err := store.CreateConfig(ctx, fixture); err != nil {
			t.Fatalf("CreateConfig(%s) failed: %v", fixture.Name, err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels?protocol=openai&limit=20&offset=0", nil))

	server.handleListChannels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
	if resp.Count != 2 {
		t.Fatalf("count=%d, want 2 body=%s", resp.Count, w.Body.String())
	}
	gotNames := make([]string, 0, len(resp.Data))
	for _, item := range resp.Data {
		gotNames = append(gotNames, item.Name)
	}
	sort.Strings(gotNames)
	wantNames := []string{"anthropic-channel", "native-openai"}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("names=%v, want %v", gotNames, wantNames)
	}
}

func TestHandleChannelsFilterOptionsTypeFilterUsesUpstreamProtocolOnly(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	fixtures := []*model.Config{
		{
			Name:         "native-openai",
			URLs:         model.ChannelURLs{{URL: "https://openai.example.com", Protocols: []string{"openai"}}},
			Priority:     10,
			ModelEntries: []model.ModelEntry{{Model: "native-model"}},
			Enabled:      true,
		},
		{
			Name:         "anthropic-channel",
			URLs:         model.ChannelURLs{{URL: "https://anthropic.example.com", Protocols: []string{"anthropic", "openai"}}},
			Priority:     20,
			ModelEntries: []model.ModelEntry{{Model: "bridge-model"}},
			Enabled:      true,
		},
		{
			Name:         "codex-channel",
			URLs:         model.ChannelURLs{{URL: "https://codex.example.com", Protocols: []string{"codex"}}},
			Priority:     30,
			ModelEntries: []model.ModelEntry{{Model: "skip-model"}},
			Enabled:      true,
		},
	}
	for _, fixture := range fixtures {
		if _, err := store.CreateConfig(ctx, fixture); err != nil {
			t.Fatalf("CreateConfig(%s) failed: %v", fixture.Name, err)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/filter-options?protocol=openai", nil))

	server.HandleChannelsFilterOptions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var data struct {
		ChannelNames []string `json:"channel_names"`
		Models       []string `json:"models"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	wantNames := []string{"anthropic-channel", "native-openai"}
	if !slices.Equal(data.ChannelNames, wantNames) {
		t.Fatalf("channel_names=%v, want %v", data.ChannelNames, wantNames)
	}
	wantModels := []string{"bridge-model", "native-model"}
	if !slices.Equal(data.Models, wantModels) {
		t.Fatalf("models=%v, want %v", data.Models, wantModels)
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
				URLs:                  model.ChannelURLs{{URL: "https://api.new.com"}},
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
				URLs:        model.ChannelURLs{{URL: "https://api.multi.com"}},
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
				URLs:     channelURLsForTest("https://us.api.com", "https://eu.api.com"),
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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

func TestHandleCreateChannelDisablesEmptiedKeyScope(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	payload := map[string]any{
		"name": "new-emptied-key-scope",
		"urls": model.ChannelURLs{{URL: "https://api.example.com"}},
		"api_keys": []map[string]any{
			{"api_key": "sk-emptied", "allowed_models": []string{}, "model_scope_empty": true},
			{"api_key": "sk-unrestricted", "allowed_models": []string{}},
		},
		"models":  []model.ModelEntry{{Model: "model-a"}},
		"enabled": true,
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	created := mustParseAPIResponse[*model.Config](t, w.Body.Bytes()).Data
	if created == nil {
		t.Fatal("create response missing channel")
	}
	keys, err := store.GetAPIKeys(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys: %v", err)
	}
	if len(keys) != 2 || !keys[0].Disabled || keys[1].Disabled {
		t.Fatalf("created keys=%+v, want emptied scope disabled and unrestricted scope enabled", keys)
	}
}

func TestHandleCreateChannel_RejectsIncompleteCooldownRulesWithoutPersisting(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	before, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs() before create = %v", err)
	}

	payload := ChannelRequest{
		Name:   "incomplete-cooldown-rule",
		APIKey: "sk-test",
		URLs:   model.ChannelURLs{{URL: "https://api.example.com"}},
		Models: []model.ModelEntry{{Model: "test-model"}},
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Priority: 0, StatusCodes: []int{200},
			Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 60,
		}}},
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), ".name: required") {
		t.Fatalf("status=%d body=%s, want required cooldown rule name error", w.Code, w.Body.String())
	}
	after, err := store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs() after create = %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("incomplete cooldown rule persisted a channel: before=%d after=%d", len(before), len(after))
	}
}

func TestHandleCreateChannel_PersistsProtocolTransformModeAndIgnoresRemovedTransforms(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	payload := map[string]any{
		"name":                    "Transform-Channel",
		"api_key":                 "sk-transform",
		"urls":                    []map[string]any{{"url": "https://transform.example.com"}},
		"priority":                42,
		"protocol_transforms":     []string{"openai", "anthropic"},
		"protocol_transform_mode": "local",
		"models": []map[string]any{
			{"model": "gemini-2.5-pro"},
		},
		"enabled": true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("期望状态码 %d，实际 %d，响应体: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if resp.Data["protocol_transform_mode"] != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol_transform_mode=%v, want local", resp.Data["protocol_transform_mode"])
	}
	if _, exists := resp.Data["protocol_transforms"]; exists {
		t.Fatalf("response must not expose removed protocol_transforms: %+v", resp.Data)
	}
}

func TestHandleCreateChannel_PersistsUpstreamProtocolTransformMode(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()

	payload := map[string]any{
		"name":                    "Upstream-Mode-Channel",
		"api_key":                 "sk-upstream-mode",
		"urls":                    []map[string]any{{"url": "https://upstream-mode.example.com"}},
		"priority":                23,
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

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if resp.Data["protocol_transform_mode"] != model.ProtocolTransformModeUpstream {
		t.Fatalf("protocol_transform_mode=%v, want upstream", resp.Data["protocol_transform_mode"])
	}
	if _, exists := resp.Data["protocol_transforms"]; exists {
		t.Fatalf("response must not echo removed protocol_transforms: %+v", resp.Data)
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
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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

func TestHandleGetChannelIncludesActiveModelCooldowns(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:     "model-cooldown-detail",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 100,
		ModelEntries: []model.ModelEntry{
			{Model: "external-model", RedirectModel: "upstream-model"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	until := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	if err := store.SetModelCooldown(ctx, created.ID, "upstream-model", until); err != nil {
		t.Fatalf("设置模型冷却失败: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+strconv.FormatInt(created.ID, 10), nil))
	server.handleGetChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			ModelCooldowns []struct {
				Model               string     `json:"model"`
				CooldownUntil       *time.Time `json:"cooldown_until"`
				CooldownRemainingMS int64      `json:"cooldown_remaining_ms"`
			} `json:"model_cooldowns"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success=false body=%s", w.Body.String())
	}
	if len(resp.Data.ModelCooldowns) != 1 {
		t.Fatalf("model_cooldowns=%v, want one active cooldown", resp.Data.ModelCooldowns)
	}
	got := resp.Data.ModelCooldowns[0]
	if got.Model != "upstream-model" {
		t.Fatalf("model=%q, want upstream-model", got.Model)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(until) {
		t.Fatalf("cooldown_until=%v, want %v", got.CooldownUntil, until)
	}
	if got.CooldownRemainingMS <= 0 {
		t.Fatalf("cooldown_remaining_ms=%d, want > 0", got.CooldownRemainingMS)
	}
}

func TestHandleChannelModelStatsReturnsTodayStatsForRequestedChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "model-stats-channel",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "external-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	other, err := store.CreateConfig(ctx, &model.Config{
		Name:         "other-model-stats-channel",
		URLs:         model.ChannelURLs{{URL: "https://other.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "external-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	entries := []*model.LogEntry{
		{
			Time:          model.JSONTime{Time: now.Add(-3 * time.Minute)},
			ChannelID:     created.ID,
			Model:         "external-model",
			StatusCode:    http.StatusOK,
			Duration:      2,
			IsStreaming:   true,
			FirstByteTime: 1,
		},
		{
			Time:          model.JSONTime{Time: now.Add(-2 * time.Minute)},
			ChannelID:     created.ID,
			Model:         "external-model",
			StatusCode:    http.StatusOK,
			Duration:      4,
			IsStreaming:   true,
			FirstByteTime: 3,
		},
		{
			Time:       model.JSONTime{Time: now.Add(-time.Minute)},
			ChannelID:  created.ID,
			Model:      "external-model",
			StatusCode: http.StatusServiceUnavailable,
		},
		{
			Time:          model.JSONTime{Time: now.Add(-time.Minute)},
			ChannelID:     other.ID,
			Model:         "external-model",
			StatusCode:    http.StatusOK,
			Duration:      100,
			IsStreaming:   true,
			FirstByteTime: 100,
		},
		{
			Time:          model.JSONTime{Time: now.AddDate(0, 0, -1)},
			ChannelID:     created.ID,
			Model:         "external-model",
			StatusCode:    http.StatusOK,
			Duration:      50,
			IsStreaming:   true,
			FirstByteTime: 50,
		},
	}
	for _, entry := range entries {
		if err := store.AddLog(ctx, entry); err != nil {
			t.Fatalf("写入测试日志失败: %v", err)
		}
	}

	path := "/admin/channels/" + strconv.FormatInt(created.ID, 10) + "/model-stats"
	c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleChannelModelStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[[]ChannelModelStats](t, w.Body.Bytes())
	if len(resp.Data) != 1 {
		t.Fatalf("stats=%v, want one model", resp.Data)
	}
	got := resp.Data[0]
	if got.Model != "external-model" || got.Success != 2 || got.Error != 1 || got.Total != 3 {
		t.Fatalf("stats=%+v, want model=external-model success=2 error=1 total=3", got)
	}
	if got.AvgFirstByteTimeSeconds == nil || math.Abs(*got.AvgFirstByteTimeSeconds-2) > 1e-9 {
		t.Fatalf("avg_first_byte_time_seconds=%v, want 2", got.AvgFirstByteTimeSeconds)
	}
	if got.AvgDurationSeconds == nil || math.Abs(*got.AvgDurationSeconds-3) > 1e-9 {
		t.Fatalf("avg_duration_seconds=%v, want 3", got.AvgDurationSeconds)
	}
}

func TestHandleChannelEditorAggregatesInitialState(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	server.urlSelector = NewURLSelector()
	server.configService = NewConfigService(store)
	if err := server.configService.LoadDefaults(ctx); err != nil {
		t.Fatalf("加载系统设置失败: %v", err)
	}

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "channel-editor-bootstrap",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "external-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID,
		KeyIndex:  0,
		APIKey:    "sk-editor-test",
		Note:      "primary",
	}}); err != nil {
		t.Fatalf("创建 API Key 失败: %v", err)
	}
	if err := store.AddLog(ctx, &model.LogEntry{
		Time:       model.JSONTime{Time: time.Now()},
		ChannelID:  created.ID,
		Model:      "external-model",
		StatusCode: http.StatusOK,
		BaseURL:    "https://api.example.com",
	}); err != nil {
		t.Fatalf("写入模型统计日志失败: %v", err)
	}
	server.urlSelector.RecordLatency(created.ID, "https://api.example.com", 50*time.Millisecond)
	server.urlSelector.RecordRequestResult(created.ID, "https://api.example.com", http.StatusOK)

	path := "/admin/channels/" + strconv.FormatInt(created.ID, 10) + "/editor"
	c, w := newTestContext(t, newRequest(http.MethodGet, path, nil))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleChannelEditor(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	resp := mustParseAPIResponse[struct {
		Channel    ChannelWithCooldown `json:"channel"`
		Keys       []*model.APIKey     `json:"keys"`
		ModelStats struct {
			Available bool                `json:"available"`
			Items     []ChannelModelStats `json:"items"`
		} `json:"model_stats"`
		URLStats struct {
			Available bool      `json:"available"`
			Items     []URLStat `json:"items"`
		} `json:"url_stats"`
		Features struct {
			ScheduledCheckEnabled bool `json:"scheduled_check_enabled"`
		} `json:"features"`
	}](t, w.Body.Bytes())

	if resp.Data.Channel.ID != created.ID || resp.Data.Channel.Name != created.Name {
		t.Fatalf("channel=%+v, want id=%d name=%q", resp.Data.Channel, created.ID, created.Name)
	}
	if len(resp.Data.Keys) != 1 || resp.Data.Keys[0].APIKey != "sk-editor-test" {
		t.Fatalf("keys=%+v, want configured key", resp.Data.Keys)
	}
	if !resp.Data.ModelStats.Available || len(resp.Data.ModelStats.Items) != 1 {
		t.Fatalf("model_stats=%+v, want available stats", resp.Data.ModelStats)
	}
	if !resp.Data.URLStats.Available || len(resp.Data.URLStats.Items) != 1 || resp.Data.URLStats.Items[0].Requests != 1 {
		t.Fatalf("url_stats=%+v, want available runtime stats", resp.Data.URLStats)
	}
	if !resp.Data.Features.ScheduledCheckEnabled {
		t.Fatalf("features=%+v, want scheduled check enabled", resp.Data.Features)
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
		URLs:         model.ChannelURLs{{URL: "https://api.original.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.updated.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.updated.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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

func TestHandleUpdateChannel_IgnoresRemovedProtocolFields(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:     "Update-Transform-Channel",
		URLs:     model.ChannelURLs{{URL: "https://update-transform.example.com"}},
		Priority: 10,
		Enabled:  true,
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

	payload := map[string]any{
		"name":                    "Update-Transform-Channel",
		"api_key":                 "sk-update-transform",
		"urls":                    []map[string]any{{"url": "https://update-transform.example.com"}},
		"priority":                10,
		"protocol_transforms":     []string{"codex", "anthropic"},
		"protocol_transform_mode": "upstream",
		"models": []map[string]any{
			{"model": "gemini-2.5-pro"},
		},
		"enabled": true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(created.ID, 10), payload))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("期望状态码 %d，实际 %d，响应体: %s", http.StatusOK, w.Code, w.Body.String())
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if resp.Data["protocol_transform_mode"] != model.ProtocolTransformModeUpstream {
		t.Fatalf("protocol_transform_mode=%v, want upstream", resp.Data["protocol_transform_mode"])
	}
	if _, exists := resp.Data["protocol_transforms"]; exists {
		t.Fatalf("response must not echo removed protocol_transforms: %+v", resp.Data)
	}
}

func TestHandleUpdateChannel_ClearAllCooldownsShouldTakeEffectImmediately(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.cooldownManager = cooldown.NewManager(store, server)

	ctx := context.Background()

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "Cooldown-Channel",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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
	if err := store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("设置 Key 冷却失败: %v", err)
	}
	if err := store.SetModelCooldown(ctx, created.ID, "model-1", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("设置模型冷却失败: %v", err)
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
	if len(before.Data[0].KeyCooldowns) != 1 || before.Data[0].KeyCooldowns[0].CooldownRemainingMS <= 0 {
		t.Fatalf("预期 Key 处于冷却中，实际 key_cooldowns=%+v", before.Data[0].KeyCooldowns)
	}

	// 预热模型冷却缓存，确保保存后返回的是最新状态。
	channelPath := "/admin/channels/" + strconv.FormatInt(created.ID, 10)
	cModelBefore, wModelBefore := newTestContext(t, newRequest(http.MethodGet, channelPath, nil))
	server.handleGetChannel(cModelBefore, created.ID)
	if wModelBefore.Code != http.StatusOK {
		t.Fatalf("预热模型冷却缓存失败: %d", wModelBefore.Code)
	}
	modelBefore := mustParseAPIResponse[ChannelWithCooldown](t, wModelBefore.Body.Bytes())
	if len(modelBefore.Data.ModelCooldowns) != 1 {
		t.Fatalf("预期模型处于冷却中，实际 model_cooldowns=%+v", modelBefore.Data.ModelCooldowns)
	}

	updatePayload := ChannelRequest{
		Name:     "Cooldown-Channel-Updated",
		APIKey:   "sk-test-key",
		URLs:     model.ChannelURLs{{URL: "https://api.updated.com"}},
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
	if len(after.Data[0].KeyCooldowns) != 1 || after.Data[0].KeyCooldowns[0].CooldownUntil != nil || after.Data[0].KeyCooldowns[0].CooldownRemainingMS > 0 {
		t.Fatalf("预期 Key 冷却已清除，实际 key_cooldowns=%+v", after.Data[0].KeyCooldowns)
	}

	cKeys, wKeys := newTestContext(t, newRequest(http.MethodGet, channelPath+"/keys", nil))
	server.handleGetChannelKeys(cKeys, created.ID)
	if wKeys.Code != http.StatusOK {
		t.Fatalf("更新后查询 Key 失败: %d", wKeys.Code)
	}
	keysAfter := mustParseAPIResponse[[]*model.APIKey](t, wKeys.Body.Bytes())
	if len(keysAfter.Data) != 1 || keysAfter.Data[0].CooldownUntil != 0 || keysAfter.Data[0].CooldownDurationMs != 0 {
		t.Fatalf("预期 Key 完整冷却状态已清除，实际 keys=%+v", keysAfter.Data)
	}

	cModelAfter, wModelAfter := newTestContext(t, newRequest(http.MethodGet, channelPath, nil))
	server.handleGetChannel(cModelAfter, created.ID)
	if wModelAfter.Code != http.StatusOK {
		t.Fatalf("更新后查询模型冷却失败: %d", wModelAfter.Code)
	}
	modelAfter := mustParseAPIResponse[ChannelWithCooldown](t, wModelAfter.Body.Bytes())
	if len(modelAfter.Data.ModelCooldowns) != 0 {
		t.Fatalf("预期模型冷却已清除，实际 model_cooldowns=%+v", modelAfter.Data.ModelCooldowns)
	}
}

func TestHandleUpdateChannel_EnableClearsAllCooldownsImmediately(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	server.channelCache = storage.NewChannelCache(store, time.Minute)
	server.cooldownManager = cooldown.NewManager(store, server)

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "Enable-Cooldown-Channel",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      false,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID:   created.ID,
		KeyIndex:    0,
		APIKey:      "sk-enable-test-key",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 API Key 失败: %v", err)
	}

	cooldownUntil := time.Now().Add(2 * time.Minute)
	if err := store.SetChannelCooldown(ctx, created.ID, cooldownUntil); err != nil {
		t.Fatalf("设置渠道冷却失败: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, created.ID, 0, cooldownUntil); err != nil {
		t.Fatalf("设置 Key 冷却失败: %v", err)
	}
	if err := store.SetModelCooldown(ctx, created.ID, "model-1", cooldownUntil); err != nil {
		t.Fatalf("设置模型冷却失败: %v", err)
	}
	storedBefore, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询启用前渠道失败: %v", err)
	}
	keysBefore, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询启用前 Key 失败: %v", err)
	}
	modelCooldownsBefore, err := store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("查询启用前模型冷却失败: %v", err)
	}
	if storedBefore.CooldownUntil <= time.Now().Unix() || len(keysBefore) != 1 ||
		keysBefore[0].CooldownUntil <= time.Now().Unix() || len(modelCooldownsBefore[created.ID]) != 1 {
		t.Fatalf("预期启用前存在三层冷却，实际 channel=%+v keys=%+v models=%+v",
			storedBefore, keysBefore, modelCooldownsBefore[created.ID])
	}

	channelPath := "/admin/channels/" + strconv.FormatInt(created.ID, 10)
	beforeCtx, beforeWriter := newTestContext(t, newRequest(http.MethodGet, channelPath, nil))
	server.handleGetChannel(beforeCtx, created.ID)
	if beforeWriter.Code != http.StatusOK {
		t.Fatalf("预热渠道冷却缓存失败: %d body=%s", beforeWriter.Code, beforeWriter.Body.String())
	}

	updateCtx, updateWriter := newTestContext(t, newJSONRequest(t, http.MethodPut, channelPath, map[string]any{
		"enabled": true,
	}))
	updateCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(updateCtx, created.ID)
	if updateWriter.Code != http.StatusOK {
		t.Fatalf("启用渠道失败: %d body=%s", updateWriter.Code, updateWriter.Body.String())
	}

	updated := mustParseAPIResponse[*model.Config](t, updateWriter.Body.Bytes())
	if updated.Data == nil || !updated.Data.Enabled {
		t.Fatalf("预期渠道已启用，实际 data=%+v", updated.Data)
	}

	afterCtx, afterWriter := newTestContext(t, newRequest(http.MethodGet, channelPath, nil))
	server.handleGetChannel(afterCtx, created.ID)
	if afterWriter.Code != http.StatusOK {
		t.Fatalf("启用后查询渠道失败: %d body=%s", afterWriter.Code, afterWriter.Body.String())
	}
	after := mustParseAPIResponse[ChannelWithCooldown](t, afterWriter.Body.Bytes())
	if after.Data.CooldownUntil != nil || after.Data.CooldownRemainingMS > 0 {
		t.Fatalf("预期渠道冷却已清除，实际 cooldown_until=%v cooldown_remaining_ms=%d", after.Data.CooldownUntil, after.Data.CooldownRemainingMS)
	}
	for _, keyCooldown := range after.Data.KeyCooldowns {
		if keyCooldown.CooldownUntil != nil || keyCooldown.CooldownRemainingMS > 0 {
			t.Fatalf("预期 Key 冷却已清除，实际 key_cooldowns=%+v", after.Data.KeyCooldowns)
		}
	}
	if len(after.Data.ModelCooldowns) != 0 {
		t.Fatalf("预期模型冷却已清除，实际 model_cooldowns=%+v", after.Data.ModelCooldowns)
	}

	stored, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询启用后渠道失败: %v", err)
	}
	if stored.CooldownUntil != 0 || stored.CooldownDurationMs != 0 {
		t.Fatalf("预期渠道完整冷却状态已清除，实际 cooldown_until=%d duration_ms=%d", stored.CooldownUntil, stored.CooldownDurationMs)
	}
	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询启用后 Key 失败: %v", err)
	}
	if len(keys) != 1 || keys[0].CooldownUntil != 0 || keys[0].CooldownDurationMs != 0 {
		t.Fatalf("预期 Key 完整冷却状态已清除，实际 keys=%+v", keys)
	}
}

func TestHandleUpdateChannel_TogglesModelDisabledWithoutChangingChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:     "model-toggle",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "keep-on", RedirectModel: "keep-on"},
			{Model: "toggle-me", RedirectModel: "toggle-me"},
		},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	channelPath := "/admin/channels/" + strconv.FormatInt(created.ID, 10)
	disableCtx, disableWriter := newTestContext(t, newJSONRequest(t, http.MethodPut, channelPath, map[string]any{
		"model":    "toggle-me",
		"disabled": true,
	}))
	server.handleUpdateChannel(disableCtx, created.ID)
	if disableWriter.Code != http.StatusOK {
		t.Fatalf("禁用模型失败: %d body=%s", disableWriter.Code, disableWriter.Body.String())
	}

	stored, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询渠道失败: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("禁用模型不应关闭渠道")
	}
	disabledByName := map[string]bool{}
	for _, entry := range stored.ModelEntries {
		disabledByName[entry.Model] = entry.Disabled
	}
	if disabledByName["toggle-me"] != true || disabledByName["keep-on"] {
		t.Fatalf("模型禁用状态=%v, want toggle-me=true keep-on=false", disabledByName)
	}

	enableCtx, enableWriter := newTestContext(t, newJSONRequest(t, http.MethodPut, channelPath, map[string]any{
		"model":    "TOGGLE-ME",
		"disabled": false,
	}))
	server.handleUpdateChannel(enableCtx, created.ID)
	if enableWriter.Code != http.StatusOK {
		t.Fatalf("启用模型失败: %d body=%s", enableWriter.Code, enableWriter.Body.String())
	}
	stored, err = store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("再次查询渠道失败: %v", err)
	}
	for _, entry := range stored.ModelEntries {
		if entry.Disabled {
			t.Fatalf("预期模型已全部启用，实际 %+v", stored.ModelEntries)
		}
	}

	missingCtx, missingWriter := newTestContext(t, newJSONRequest(t, http.MethodPut, channelPath, map[string]any{
		"model":    "not-on-channel",
		"disabled": true,
	}))
	server.handleUpdateChannel(missingCtx, created.ID)
	if missingWriter.Code != http.StatusNotFound {
		t.Fatalf("缺失模型期望 404，实际 %d body=%s", missingWriter.Code, missingWriter.Body.String())
	}
}

func TestHandleAPIKeyToggleRejectsMissingOrNegativeKeyIndex(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "toggle-validation",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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
		APIKey:      "sk-validation",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 key 失败: %v", err)
	}

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "缺失 key_index", body: map[string]any{}},
		{name: "负数 key_index", body: map[string]any{"key_index": -1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/key-disable", tt.body))
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}

			server.HandleAPIKeyDisable(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("期望状态码 %d，实际 %d，响应体: %s", http.StatusBadRequest, w.Code, w.Body.String())
			}

			key, err := store.GetAPIKey(ctx, created.ID, 0)
			if err != nil {
				t.Fatalf("查询 key 失败: %v", err)
			}
			if key.Disabled {
				t.Fatalf("非法请求不应禁用 key_index=0")
			}
		})
	}
}

func TestHandleAPIKeyToggleInvalidatesKeyCooldownCache(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.channelCache = storage.NewChannelCache(store, time.Minute)

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "toggle-cache",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-cooling", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-healthy", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("创建测试 keys 失败: %v", err)
	}
	if err := store.SetKeyCooldown(ctx, created.ID, 0, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("设置 key 冷却失败: %v", err)
	}

	cached, err := server.getAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("预热 key 冷却缓存失败: %v", err)
	}
	if _, ok := cached[created.ID][0]; !ok {
		t.Fatalf("预期 key_index=0 冷却被缓存，实际: %#v", cached)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/key-disable", map[string]any{"key_index": 0}))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleAPIKeyDisable(c)
	if w.Code != http.StatusOK {
		t.Fatalf("禁用 key 失败: %d body=%s", w.Code, w.Body.String())
	}

	fresh, err := server.getAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("重新读取 key 冷却失败: %v", err)
	}
	if keyMap := fresh[created.ID]; keyMap != nil {
		if _, ok := keyMap[0]; ok {
			t.Fatalf("禁用 key 后冷却缓存仍包含 key_index=0: %#v", fresh)
		}
	}
}

func TestHandleUpdateChannelPreservesDisabledKeysWhenRebuilding(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "preserve-disabled",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-disabled", KeyStrategy: model.KeyStrategySequential},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-live", KeyStrategy: model.KeyStrategySequential},
	}); err != nil {
		t.Fatalf("创建测试 keys 失败: %v", err)
	}
	if err := store.SetAPIKeyDisabled(ctx, created.ID, 0, true); err != nil {
		t.Fatalf("禁用测试 key 失败: %v", err)
	}

	payload := ChannelRequest{
		Name:     "preserve-disabled",
		APIKey:   "sk-live,sk-disabled,sk-new",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 10,
		Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:  true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(created.ID, 10), payload))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("更新渠道失败: %d body=%s", w.Code, w.Body.String())
	}

	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("查询更新后 keys 失败: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("期望 3 个 key，实际 %d", len(keys))
	}
	assertKey := func(pos int, value string, disabled bool) {
		t.Helper()
		if keys[pos].KeyIndex != pos || keys[pos].APIKey != value || keys[pos].Disabled != disabled {
			t.Fatalf("keys[%d] = {index:%d api_key:%q disabled:%v}, want {index:%d api_key:%q disabled:%v}",
				pos, keys[pos].KeyIndex, keys[pos].APIKey, keys[pos].Disabled, pos, value, disabled)
		}
	}
	assertKey(0, "sk-live", false)
	assertKey(1, "sk-disabled", true)
	assertKey(2, "sk-new", false)
}

func TestHandleChannelAPIKeyNotesCreateReadAndUpdate(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	createPayload := ChannelRequest{
		Name: "key-notes",
		URLs: model.ChannelURLs{{URL: "https://api.example.com"}},
		APIKeys: []ChannelAPIKeyRequest{
			{APIKey: "sk-primary", Note: "primary", AllowedModels: []string{"model-1"}},
			{APIKey: "sk-backup", Note: "backup"},
		},
		Priority: 10,
		Models:   []model.ModelEntry{{Model: "model-1"}},
		Enabled:  true,
	}

	createCtx, createW := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", createPayload))
	server.handleCreateChannel(createCtx)
	if createW.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", createW.Code, createW.Body.String())
	}
	createResp := mustParseAPIResponse[*model.Config](t, createW.Body.Bytes())
	if createResp.Data == nil {
		t.Fatalf("create response missing channel: %s", createW.Body.String())
	}
	channelID := createResp.Data.ID

	readCtx, readW := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/"+strconv.FormatInt(channelID, 10)+"/keys", nil))
	server.handleGetChannelKeys(readCtx, channelID)
	if readW.Code != http.StatusOK {
		t.Fatalf("get keys status=%d body=%s", readW.Code, readW.Body.String())
	}
	readResp := mustParseAPIResponse[[]*model.APIKey](t, readW.Body.Bytes())
	if len(readResp.Data) != 2 || readResp.Data[0].Note != "primary" || readResp.Data[1].Note != "backup" {
		t.Fatalf("created key notes = %#v, want primary/backup", readResp.Data)
	}
	if !slices.Equal(readResp.Data[0].AllowedModels, []string{"model-1"}) || len(readResp.Data[1].AllowedModels) != 0 {
		t.Fatalf("created key model scopes = %#v, want [model-1]/unrestricted", readResp.Data)
	}

	cooldownUntil := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	if err := store.SetKeyCooldown(ctx, channelID, 1, cooldownUntil); err != nil {
		t.Fatalf("set key cooldown: %v", err)
	}

	updatePayload := ChannelRequest{
		Name: "key-notes",
		URLs: model.ChannelURLs{{URL: "https://api.example.com"}},
		APIKeys: []ChannelAPIKeyRequest{
			{APIKey: "sk-primary", Note: "primary-renamed"},
			{APIKey: "sk-backup", Note: "backup-renamed", AllowedModels: []string{"model-1"}},
		},
		Priority: 10,
		Models:   []model.ModelEntry{{Model: "model-1"}},
		Enabled:  true,
	}
	updateCtx, updateW := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(channelID, 10), updatePayload))
	updateCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(channelID, 10)}}
	server.handleUpdateChannel(updateCtx, channelID)
	if updateW.Code != http.StatusOK {
		t.Fatalf("update channel status=%d body=%s", updateW.Code, updateW.Body.String())
	}

	keys, err := store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get keys after update: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys)=%d, want 2", len(keys))
	}
	if keys[0].APIKey != "sk-primary" || keys[0].Note != "primary-renamed" {
		t.Fatalf("keys[0]=%+v, want sk-primary with updated note", keys[0])
	}
	if keys[1].APIKey != "sk-backup" || keys[1].Note != "backup-renamed" {
		t.Fatalf("keys[1]=%+v, want sk-backup with updated note", keys[1])
	}
	if !slices.Equal(keys[0].AllowedModels, []string{"model-1"}) || !slices.Equal(keys[1].AllowedModels, []string{"model-1"}) {
		t.Fatalf("key model scopes after omitted/explicit update = [%v, %v]", keys[0].AllowedModels, keys[1].AllowedModels)
	}
	if keys[1].CooldownUntil != cooldownUntil.Unix() {
		t.Fatalf("key cooldown after note-only update=%d, want %d", keys[1].CooldownUntil, cooldownUntil.Unix())
	}

	legacyUpdate := ChannelRequest{
		Name:     "key-notes",
		APIKey:   "sk-primary,sk-backup",
		URLs:     model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority: 10,
		Models:   []model.ModelEntry{{Model: "model-1"}},
		Enabled:  true,
	}
	legacyCtx, legacyW := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(channelID, 10), legacyUpdate))
	legacyCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(channelID, 10)}}
	server.handleUpdateChannel(legacyCtx, channelID)
	if legacyW.Code != http.StatusOK {
		t.Fatalf("legacy update status=%d body=%s", legacyW.Code, legacyW.Body.String())
	}
	keys, err = store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get keys after legacy update: %v", err)
	}
	if !slices.Equal(keys[0].AllowedModels, []string{"model-1"}) || !slices.Equal(keys[1].AllowedModels, []string{"model-1"}) {
		t.Fatalf("legacy update cleared model scopes: [%v, %v]", keys[0].AllowedModels, keys[1].AllowedModels)
	}

	clearPayload := map[string]any{
		"name": "key-notes",
		"api_keys": []map[string]any{
			{"api_key": "sk-primary", "allowed_models": []string{}},
			{"api_key": "sk-backup"},
		},
		"urls":     model.ChannelURLs{{URL: "https://api.example.com"}},
		"priority": 10,
		"models":   []model.ModelEntry{{Model: "model-1"}},
		"enabled":  true,
	}
	clearCtx, clearW := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(channelID, 10), clearPayload))
	clearCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(channelID, 10)}}
	server.handleUpdateChannel(clearCtx, channelID)
	if clearW.Code != http.StatusOK {
		t.Fatalf("explicit clear status=%d body=%s", clearW.Code, clearW.Body.String())
	}
	keys, err = store.GetAPIKeys(ctx, channelID)
	if err != nil {
		t.Fatalf("get keys after explicit clear: %v", err)
	}
	if len(keys[0].AllowedModels) != 0 || !slices.Equal(keys[1].AllowedModels, []string{"model-1"}) {
		t.Fatalf("explicit clear result = [%v, %v]", keys[0].AllowedModels, keys[1].AllowedModels)
	}
}

func TestHandleUpdateChannelDisablesEmptiedScopeWhenRebuildingKeys(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name: "scope-rebuild", URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "model-a"}, {Model: "model-b"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-scoped", AllowedModels: []string{"model-b"},
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	payload := map[string]any{
		"name": "scope-rebuild",
		"urls": model.ChannelURLs{{URL: "https://api.example.com"}},
		"api_keys": []map[string]any{
			// The editor is stale and still submits the deleted model. The
			// backend must prune it before rebuilding the key rows.
			{"api_key": "sk-scoped", "allowed_models": []string{"model-b"}},
			{"api_key": "sk-new", "allowed_models": []string{}},
		},
		"models":  []model.ModelEntry{{Model: "model-a"}},
		"enabled": true,
	}
	requestPath := "/admin/channels/" + strconv.FormatInt(created.ID, 10)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, requestPath, payload))
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(c, created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}

	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys: %v", err)
	}
	if len(keys) != 2 || !keys[0].Disabled || keys[1].Disabled {
		t.Fatalf("rebuilt keys=%+v, want emptied existing scope disabled and new unrestricted key enabled", keys)
	}

	// An automatically disabled empty-scope key cannot be enabled without
	// explicitly choosing a scope first.
	enableCtx, enableW := newTestContext(t, newJSONRequest(t, http.MethodPost,
		"/admin/channels/"+strconv.FormatInt(created.ID, 10)+"/key-enable",
		map[string]any{"key_index": 0}))
	enableCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.HandleAPIKeyEnable(enableCtx)
	if enableW.Code != http.StatusConflict {
		t.Fatalf("enable empty-scope key status=%d body=%s, want 409", enableW.Code, enableW.Body.String())
	}

	// Explicitly clearing the scope means unrestricted access and must restore
	// a key that was disabled only by the automatic empty-scope transition.
	allowAllPayload := map[string]any{
		"name": "scope-rebuild",
		"urls": model.ChannelURLs{{URL: "https://api.example.com"}},
		"api_keys": []map[string]any{
			{"api_key": "sk-scoped", "allowed_models": []string{}, "model_scope_empty": false},
			{"api_key": "sk-new", "allowed_models": []string{}, "model_scope_empty": false},
		},
		"models":  []model.ModelEntry{{Model: "model-a"}},
		"enabled": true,
	}
	allowAllCtx, allowAllW := newTestContext(t, newJSONRequest(t, http.MethodPut, requestPath, allowAllPayload))
	allowAllCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(allowAllCtx, created.ID)
	if allowAllW.Code != http.StatusOK {
		t.Fatalf("allow-all update status=%d body=%s", allowAllW.Code, allowAllW.Body.String())
	}
	keys, err = store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAPIKeys after allow-all: %v", err)
	}
	if len(keys) != 2 || keys[0].Disabled || keys[0].ModelScopeEmpty || !keys[0].AllowsModel("model-a") {
		t.Fatalf("allow-all scope result=%+v, want first key enabled and unrestricted", keys)
	}
}

func TestHandleUpdateChannel_APIKeyAllowedModelsFailureIsReturned(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()

	ctx := context.Background()
	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "scope-update-failure",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "model-1"}, {Model: "model-2"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-primary", AllowedModels: []string{"model-1"},
	}}); err != nil {
		t.Fatalf("create API key: %v", err)
	}
	server.store = &failAPIKeyAllowedModelsStore{Store: store}

	payload := map[string]any{
		"name": "scope-update-failure",
		"api_keys": []map[string]any{{
			"api_key": "sk-primary", "allowed_models": []string{"model-2"},
		}},
		"urls":     model.ChannelURLs{{URL: "https://api.example.com"}},
		"priority": 10,
		"models":   []model.ModelEntry{{Model: "model-1"}, {Model: "model-2"}},
		"enabled":  true,
	}
	updateCtx, updateW := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(created.ID, 10), payload))
	updateCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(created.ID, 10)}}
	server.handleUpdateChannel(updateCtx, created.ID)
	if updateW.Code != http.StatusInternalServerError {
		t.Fatalf("update status=%d body=%s, want 500", updateW.Code, updateW.Body.String())
	}
	keys, err := store.GetAPIKeys(ctx, created.ID)
	if err != nil {
		t.Fatalf("get keys: %v", err)
	}
	if len(keys) != 1 || !slices.Equal(keys[0].AllowedModels, []string{"model-1"}) {
		t.Fatalf("persisted model scope after failed update=%v, want [model-1]", keys)
	}
}

func TestHandleUpdateChannel_PrunesURLSelectorState(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "update-prune",
		URLs:         channelURLsForTest("https://old-1.example.com", "https://keep.example.com"),
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
		URLs:         model.ChannelURLs{{URL: "https://other.example.com"}},
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
		URLs:     channelURLsForTest("https://keep.example.com", "https://new.example.com"),
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

func TestHandleUpdateChannel_CleansOrphanedURLDisabledState(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.urlSelector = NewURLSelector()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "update-url-state",
		URLs:         channelURLsForTest("https://old-state.example.com", "https://keep-state.example.com"),
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
		APIKey:      "sk-update-url-state",
		KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("创建测试 key 失败: %v", err)
	}

	otherCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "other-url-state",
		URLs:         model.ChannelURLs{{URL: "https://other-state.example.com"}},
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}

	if err := store.SetURLDisabled(ctx, cfg.ID, "https://old-state.example.com", true); err != nil {
		t.Fatalf("禁用旧 URL 失败: %v", err)
	}
	if err := store.SetURLDisabled(ctx, cfg.ID, "https://keep-state.example.com", true); err != nil {
		t.Fatalf("禁用保留 URL 失败: %v", err)
	}
	if err := store.SetURLDisabled(ctx, otherCfg.ID, "https://other-state.example.com", true); err != nil {
		t.Fatalf("禁用其他渠道 URL 失败: %v", err)
	}

	payload := ChannelRequest{
		Name:     "update-url-state",
		APIKey:   "sk-update-url-state",
		URLs:     channelURLsForTest("https://keep-state.example.com", "https://new-state.example.com"),
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

	disabledURLs, err := store.LoadDisabledURLs(ctx)
	if err != nil {
		t.Fatalf("加载禁用 URL 状态失败: %v", err)
	}
	if slices.Contains(disabledURLs[cfg.ID], "https://old-state.example.com") {
		t.Fatalf("期望渠道更新后旧 URL 禁用状态被清理")
	}
	if !slices.Contains(disabledURLs[cfg.ID], "https://keep-state.example.com") {
		t.Fatalf("期望保留 URL 的禁用状态仍存在")
	}
	if !slices.Contains(disabledURLs[otherCfg.ID], "https://other-state.example.com") {
		t.Fatalf("期望其他渠道禁用状态不受影响")
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
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     50,
		ModelEntries: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
		Enabled:      true,
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}
	other, err := store.CreateConfig(ctx, &model.Config{
		Name:    "Still-Available",
		URLs:    model.ChannelURLs{{URL: "https://other.example.com"}},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建其他渠道失败: %v", err)
	}
	const tokenHash = "delete-channel-token-hash"
	authToken := &model.AuthToken{
		Token:                  tokenHash,
		Description:            "delete channel restriction reload",
		IsActive:               true,
		AllowedChannelIDs:      []int64{created.ID},
		ChannelRestrictionMode: model.ChannelRestrictionModeAllow,
	}
	if err := store.CreateAuthToken(ctx, authToken); err != nil {
		t.Fatalf("创建 API 令牌失败: %v", err)
	}
	server.authService = newTestAuthService(t)
	server.authService.store = store
	if err := server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("加载 API 令牌失败: %v", err)
	}
	if server.authService.IsChannelAllowed(tokenHash, other.ID) {
		t.Fatal("删除前令牌不应允许其他渠道")
	}

	tests := []struct {
		name           string
		channelID      string
		expectedStatus int
		checkSuccess   bool
	}{
		{
			name:           "成功删除渠道",
			channelID:      strconv.FormatInt(created.ID, 10),
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
				_, err := store.GetConfig(ctx, created.ID)
				if err == nil {
					t.Error("渠道应该已被删除")
				}
				storedToken, err := store.GetAuthToken(ctx, authToken.ID)
				if err != nil {
					t.Fatalf("读取 API 令牌失败: %v", err)
				}
				if len(storedToken.AllowedChannelIDs) != 0 {
					t.Fatalf("删除后 allowed_channel_ids=%v，期望为空", storedToken.AllowedChannelIDs)
				}
				if storedToken.IsActive {
					t.Fatal("删除唯一白名单渠道后令牌应被禁用")
				}
				if server.authService.IsTokenActive(tokenHash) {
					t.Fatal("删除后热更新未生效：被禁用令牌仍在鉴权缓存中")
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
		URLs:         channelURLsForTest("https://target-1.example.com", "https://target-2.example.com"),
		Priority:     10,
		ModelEntries: []model.ModelEntry{{Model: "m1", RedirectModel: ""}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("创建目标渠道失败: %v", err)
	}
	otherCfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "delete-prune-other",
		URLs:         model.ChannelURLs{{URL: "https://other.example.com"}},
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
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "  "}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0].url cannot be empty",
		},
		{
			name: "URL缺少scheme",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "api.com"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: invalid url: \"api.com\"",
		},
		{
			name: "URL scheme非法",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "ftp://api.com"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: invalid url scheme: \"ftp\" (allowed: http, https)",
		},
		{
			name: "URL包含userinfo",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://user:pass@api.com"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: url must not contain user info",
		},
		{
			name: "URL包含query",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com?x=1"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: url must not contain query or fragment",
		},
		{
			name: "URL包含fragment",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com#x"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: url must not contain query or fragment",
		},
		{
			name: "URL以#结尾保留完整地址标记",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com", Exact: true}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://api.com#",
		},
		{
			name: "URL包含/v1路径（禁止）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com/v1"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: url should not contain API endpoint path like /v1 (current path: \"/v1\")",
		},
		{
			name: "URL包含/v1/messages路径（禁止）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com/v1/messages"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError: true,
			errorMsg:  "urls[0]: url should not contain API endpoint path like /v1 (current path: \"/v1/messages\")",
		},
		{
			name: "URL以#结尾允许显式/v1完整地址",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com/v1/messages", Exact: true}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://api.com/v1/messages#",
		},
		{
			name: "URL以#结尾不再依赖协议处理配置",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com/v1/messages", Exact: true}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "https://api.com/v1/messages#",
		},
		{
			name: "URL包含/api路径（允许）",
			req: ChannelRequest{
				Name:     "Test",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://example.com/api"}},
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
				URLs:     model.ChannelURLs{{URL: "https://example.com/openai/"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com/"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com:8080/"}},
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
				URLs:     model.ChannelURLs{{URL: "http://api.example.com:8080/"}},
				Priority: 100,
				Models:   []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
			},
			wantError:       false,
			expectNormalize: "http://api.example.com:8080",
		},
		{
			name: "缺少name",
			req: ChannelRequest{
				Name:     "",
				APIKey:   "sk-test",
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
				URLs:     model.ChannelURLs{{URL: "https://api.com"}},
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
					if tt.expectNormalize != "" && tt.req.URLs[0].RuntimeURL() != tt.expectNormalize {
						t.Errorf("URL标准化失败：期望 %q，实际 %q", tt.expectNormalize, tt.req.URLs[0].RuntimeURL())
					}
				}
			}
		})
	}
}
