# Channel Protocol Transforms Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为渠道新增 `protocol_transforms` 协议面声明，并接入 Anthropic/Codex/OpenAI/Gemini 的请求与响应协议转换链路，同时保留现有重试、冷却、多 URL、usage 统计和模型抓取行为。

**Architecture:** 保持 `channel_type` 仅表示上游真实协议，新增 `protocol_transforms` 表示额外暴露给客户端的协议面。候选渠道和模型视图按暴露协议筛选，真实转发时仍按 `channel_type` 做认证头、路径和 usage 解析。协议转换实现放在 `internal/protocol`，只做薄适配，不接管 executor 或重试逻辑。

**Tech Stack:** Go 1.25, Gin, SQLite/MySQL migration layer, existing `internal/app` proxy pipeline, existing `web/assets/js` test harness, `node --test`

---

## File Map

### Backend data model and persistence

- Modify: `internal/model/config.go`
- Modify: `internal/app/admin_types.go`
- Modify: `internal/storage/store.go`
- Modify: `internal/storage/schema/tables.go`
- Modify: `internal/storage/migrate.go`
- Modify: `internal/storage/sql/query.go`
- Modify: `internal/storage/sql/config.go`
- Modify: `internal/storage/cache.go`
- Test: `internal/storage/sql/config_test.go`
- Test: `internal/storage/cache_isolation_test.go`
- Test: `internal/storage/migrate_sqlite_test.go`
- Test: `internal/storage/migrate_mysql_test.go`

### Admin API, CSV, and channel editing payload

- Modify: `internal/app/admin_channels.go`
- Modify: `internal/app/admin_csv.go`
- Test: `internal/app/admin_crud_test.go`
- Test: `internal/app/admin_api_test.go`
- Test: `internal/app/csv_integration_test.go`

### Model listing and candidate selection

- Modify: `internal/app/proxy_gemini.go`
- Modify: `internal/app/proxy_handler.go`
- Modify: `internal/app/selector.go`
- Modify: `internal/app/server.go`
- Test: `internal/app/proxy_gemini_test.go`
- Test: `internal/app/selector_test.go`
- Test: `internal/app/server_misc_test.go`

### Protocol transform core

- Create: `internal/protocol/types.go`
- Create: `internal/protocol/registry.go`
- Create: `internal/protocol/request.go`
- Create: `internal/protocol/response.go`
- Create: `internal/protocol/builtin/register.go`
- Create: `internal/protocol/builtin/...` 裁剪自 `CLIProxyAPI` 的最小转换器文件
- Test: `internal/protocol/request_test.go`
- Test: `internal/protocol/response_test.go`

### Proxy forwarding integration

- Modify: `internal/app/proxy_forward.go`
- Modify: `internal/app/proxy_util.go`
- Modify: `internal/app/proxy_sse_parser.go` 仅在需要扩展接口时修改
- Test: `internal/app/proxy_handler_test.go`
- Test: `internal/app/proxy_integration_test.go`
- Test: `internal/app/proxy_forward_test.go`

### Frontend UI

- Modify: `web/channels.html`
- Modify: `web/assets/js/channels-modals.js`
- Modify: `web/assets/js/ui.js`
- Modify: `web/assets/locales/zh-CN.js`
- Modify: `web/assets/locales/en.js`
- Test: `web/assets/js/channels-protocol-transforms.test.js`
- Test: `web/assets/js/channels-actions.test.js`

---

### Task 1: 锁定持久化与查询接口

**Files:**
- Modify: `internal/model/config.go`
- Modify: `internal/storage/store.go`
- Test: `internal/storage/sql/config_test.go`
- Test: `internal/storage/cache_isolation_test.go`

- [ ] **Step 1: 先写失败测试，定义协议面查询语义**

```go
func TestConfig_GetEnabledChannelsByExposedProtocol(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "protocol_query.db")
	ctx := context.Background()

	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:               "gemini-openai",
		URL:                "https://example.com",
		ChannelType:        "gemini",
		ProtocolTransforms: []string{"openai"},
		Enabled:            true,
		ModelEntries: []model.ModelEntry{
			{Model: "gemini-2.5-pro"},
		},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if cfg.ID == 0 {
		t.Fatalf("CreateConfig returned zero ID")
	}

	channels, err := store.GetEnabledChannelsByExposedProtocol(ctx, "openai")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByExposedProtocol failed: %v", err)
	}
	if len(channels) != 1 || channels[0].Name != "gemini-openai" {
		t.Fatalf("unexpected channels: %#v", channels)
	}
}
```

- [ ] **Step 2: 跑失败测试，确认接口还不存在**

Run: `go test -tags go_json ./internal/storage/sql -run 'TestConfig_GetEnabledChannelsByExposedProtocol' -v`

Expected: 编译失败，提示 `GetEnabledChannelsByExposedProtocol` 未定义，或断言失败表明 `ProtocolTransforms` 未持久化。

- [ ] **Step 3: 最小实现 `ProtocolTransforms` 字段与 Store 接口**

```go
type Config struct {
	ID                 int64        `json:"id"`
	Name               string       `json:"name"`
	ChannelType        string       `json:"channel_type"`
	ProtocolTransforms []string     `json:"protocol_transforms,omitempty"`
	URL                string       `json:"url"`
	ModelEntries       []ModelEntry `json:"models"`
}

type Store interface {
	GetEnabledChannelsByExposedProtocol(ctx context.Context, protocol string) ([]*model.Config, error)
	GetEnabledChannelsByModelAndProtocol(ctx context.Context, modelName, protocol string) ([]*model.Config, error)
}
```

- [ ] **Step 4: 让缓存层测试也先卡住行为**

```go
func TestCacheIsolation_GetEnabledChannelsByExposedProtocol(t *testing.T) {
	ctx := context.Background()
	cache := newChannelCacheForTest(t)

	got, err := cache.GetEnabledChannelsByExposedProtocol(ctx, "openai")
	if err != nil {
		t.Fatalf("GetEnabledChannelsByExposedProtocol failed: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected transformed channel")
	}

	got[0].ProtocolTransforms[0] = "broken"

	again, err := cache.GetEnabledChannelsByExposedProtocol(ctx, "openai")
	if err != nil {
		t.Fatalf("second GetEnabledChannelsByExposedProtocol failed: %v", err)
	}
	if again[0].ProtocolTransforms[0] != "openai" {
		t.Fatalf("cache polluted: %#v", again[0].ProtocolTransforms)
	}
}
```

- [ ] **Step 5: 跑一组最小测试，确认模型层和接口层通过**

Run: `go test -tags go_json ./internal/storage/sql ./internal/storage -run 'Protocol|ExposedProtocol' -v`

Expected: 新增测试从编译失败转为断言级失败，说明接口层已立住，持久化尚未完成。

- [ ] **Step 6: 提交接口与失败测试**

```bash
git add internal/model/config.go internal/storage/store.go internal/storage/sql/config_test.go internal/storage/cache_isolation_test.go
git commit -m "test: define protocol transform storage contract"
```

### Task 2: 实现数据库、迁移与缓存索引

**Files:**
- Modify: `internal/storage/schema/tables.go`
- Modify: `internal/storage/migrate.go`
- Modify: `internal/storage/sql/query.go`
- Modify: `internal/storage/sql/config.go`
- Modify: `internal/storage/cache.go`
- Test: `internal/storage/migrate_sqlite_test.go`
- Test: `internal/storage/migrate_mysql_test.go`
- Test: `internal/storage/sql/config_test.go`

- [ ] **Step 1: 先补迁移失败测试**

```go
func TestMigrateSQLite_AddsChannelProtocolTransformsTable(t *testing.T) {
	db := openSQLiteForMigrationTest(t)
	ctx := context.Background()

	if err := migrateSQLite(ctx, db); err != nil {
		t.Fatalf("migrateSQLite failed: %v", err)
	}

	if !sqliteTableExists(t, db, "channel_protocol_transforms") {
		t.Fatalf("channel_protocol_transforms table not created")
	}
}
```

- [ ] **Step 2: 跑迁移测试，确认新表确实不存在**

Run: `go test -tags go_json ./internal/storage -run 'ChannelProtocolTransforms' -v`

Expected: FAIL，提示表不存在或迁移未覆盖。

- [ ] **Step 3: 实现 schema、migration 和 SQL 持久化**

```go
func DefineChannelProtocolTransformsTable() *TableBuilder {
	return NewTable("channel_protocol_transforms").
		Column("channel_id INT NOT NULL").
		Column("protocol VARCHAR(64) NOT NULL").
		Column("PRIMARY KEY (channel_id, protocol)").
		Column("FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE").
		Index("idx_channel_protocol_transforms_protocol", "protocol")
}

func normalizeProtocolTransforms(channelType string, transforms []string) []string {
	seen := make(map[string]struct{}, len(transforms))
	out := make([]string, 0, len(transforms))
	base := util.NormalizeChannelType(channelType)
	for _, raw := range transforms {
		p := util.NormalizeChannelType(raw)
		if !util.IsValidChannelType(p) || p == base {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: 在 SQLStore 中读写 transform 关联表**

```go
func (s *SQLStore) saveProtocolTransformsTx(ctx context.Context, tx *sql.Tx, channelID int64, transforms []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_protocol_transforms WHERE channel_id = ?`, channelID); err != nil {
		return err
	}
	for _, protocol := range transforms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_protocol_transforms(channel_id, protocol) VALUES(?, ?)`,
			channelID, protocol,
		); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: 扩展缓存索引**

```go
type ChannelCache struct {
	channelsByType            map[string][]*modelpkg.Config
	channelsByExposedProtocol map[string][]*modelpkg.Config
}
```

并在刷新时构建：

```go
for _, cfg := range allChannels {
	byType[cfg.GetChannelType()] = append(byType[cfg.GetChannelType()], cfg)
	for _, protocol := range cfg.SupportedProtocols() {
		byExposed[protocol] = append(byExposed[protocol], cfg)
	}
}
```

- [ ] **Step 6: 跑存储与迁移回归**

Run: `go test -tags go_json ./internal/storage/... ./internal/storage/sql/... -run 'Protocol|Transforms|Config_GetEnabledChannelsByExposedProtocol' -v`

Expected: PASS

- [ ] **Step 7: 提交持久化实现**

```bash
git add internal/storage/schema/tables.go internal/storage/migrate.go internal/storage/sql/query.go internal/storage/sql/config.go internal/storage/cache.go internal/storage/migrate_sqlite_test.go internal/storage/migrate_mysql_test.go internal/storage/sql/config_test.go
git commit -m "feat: persist channel protocol transforms"
```

### Task 3: 管理 API、校验与 CSV 导入导出

**Files:**
- Modify: `internal/app/admin_types.go`
- Modify: `internal/app/admin_channels.go`
- Modify: `internal/app/admin_csv.go`
- Test: `internal/app/admin_crud_test.go`
- Test: `internal/app/admin_api_test.go`
- Test: `internal/app/csv_integration_test.go`

- [ ] **Step 1: 写失败测试，锁定创建/更新请求结构**

```go
func TestHandleCreateChannel_SavesProtocolTransforms(t *testing.T) {
	server := newTestServer(t)
	payload := map[string]any{
		"name": "gemini-xform",
		"api_key": "k1",
		"url": "https://example.com",
		"channel_type": "gemini",
		"protocol_transforms": []string{"openai", "anthropic"},
		"models": []map[string]string{{"model": "gemini-2.5-pro"}},
		"enabled": true,
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	cfg, err := server.store.GetConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if diff := cmp.Diff([]string{"anthropic", "openai"}, cfg.ProtocolTransforms); diff != "" {
		t.Fatalf("ProtocolTransforms mismatch (-want +got):\n%s", diff)
	}
}
```

- [ ] **Step 2: 跑 admin 测试，确认校验和回填都还没支持**

Run: `go test -tags go_json ./internal/app -run 'ProtocolTransforms|HandleCreateChannel_SavesProtocolTransforms' -v`

Expected: FAIL，可能表现为字段被忽略、排序不一致、重复值未去重。

- [ ] **Step 3: 实现请求校验与配置转换**

```go
type ChannelRequest struct {
	Name               string             `json:"name"`
	APIKey             string             `json:"api_key"`
	ChannelType        string             `json:"channel_type,omitempty"`
	ProtocolTransforms []string           `json:"protocol_transforms,omitempty"`
	Models             []model.ModelEntry `json:"models"`
}

func (cr *ChannelRequest) Validate() error {
	cr.ChannelType = util.NormalizeChannelType(cr.ChannelType)
	cr.ProtocolTransforms = normalizeProtocolTransforms(cr.ChannelType, cr.ProtocolTransforms)
	return nil
}

func (cr *ChannelRequest) ToConfig() *model.Config {
	return &model.Config{
		Name:               strings.TrimSpace(cr.Name),
		ChannelType:        cr.ChannelType,
		ProtocolTransforms: append([]string(nil), cr.ProtocolTransforms...),
	}
}
```

- [ ] **Step 4: 补 CSV 导入导出测试**

```go
func TestCSVExportImport_RoundTripProtocolTransforms(t *testing.T) {
	// 创建带 protocol_transforms 的渠道
	// 导出 CSV
	// 清库
	// 再导入
	// 验证 protocol_transforms 恢复为同一组值
}
```

- [ ] **Step 5: 实现 CSV 列**

```go
header := []string{
	"id", "name", "api_key", "url", "priority", "models", "model_redirects",
	"channel_type", "protocol_transforms", "key_strategy", "enabled",
	"scheduled_check_enabled", "scheduled_check_model",
}

record = append(record, strings.Join(cfg.ProtocolTransforms, ","))
```

导入解析：

```go
func parseProtocolTransformsCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 6: 跑 admin 与 CSV 回归**

Run: `go test -tags go_json ./internal/app -run 'ProtocolTransforms|CSV' -v`

Expected: PASS

- [ ] **Step 7: 提交管理端与 CSV 变更**

```bash
git add internal/app/admin_types.go internal/app/admin_channels.go internal/app/admin_csv.go internal/app/admin_crud_test.go internal/app/admin_api_test.go internal/app/csv_integration_test.go
git commit -m "feat: expose protocol transforms in admin api"
```

### Task 4: 按暴露协议修正模型视图与候选渠道

**Files:**
- Modify: `internal/app/proxy_gemini.go`
- Modify: `internal/app/proxy_handler.go`
- Modify: `internal/app/selector.go`
- Modify: `internal/app/server.go`
- Test: `internal/app/proxy_gemini_test.go`
- Test: `internal/app/selector_test.go`
- Test: `internal/app/server_misc_test.go`

- [ ] **Step 1: 先写 `/v1/models` 失败测试**

```go
func TestHandleListOpenAIModels_IncludesTransformedGeminiChannel(t *testing.T) {
	server, store := newModelListTestServer(t)
	ctx := context.Background()

	_, err := store.CreateConfig(ctx, &model.Config{
		Name:               "g-to-oai",
		URL:                "https://example.com",
		ChannelType:        "gemini",
		ProtocolTransforms: []string{"openai"},
		Enabled:            true,
		ModelEntries: []model.ModelEntry{{Model: "gemini-2.5-pro"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/v1/models", nil))
	server.handleListOpenAIModels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gemini-2.5-pro") {
		t.Fatalf("response missing transformed model: %s", w.Body.String())
	}
}
```

- [ ] **Step 2: 写候选渠道失败测试**

```go
func TestSelectCandidatesByModelAndType_MatchesExposedProtocol(t *testing.T) {
	server := newSelectorTestServer(t)
	cfg := newTestChannelConfig("gemini", []string{"openai"}, "gemini-2.5-pro")
	server.store = stubStoreReturning(cfg)

	got, err := server.selectCandidatesByModelAndType(context.Background(), "gemini-2.5-pro", "openai")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndType failed: %v", err)
	}
	if len(got) != 1 || got[0].Name != cfg.Name {
		t.Fatalf("unexpected candidates: %#v", got)
	}
}
```

- [ ] **Step 3: 跑失败测试**

Run: `go test -tags go_json ./internal/app -run 'IncludesTransformedGeminiChannel|MatchesExposedProtocol' -v`

Expected: FAIL，当前只按 `channel_type` 查，transform 渠道不会被命中。

- [ ] **Step 4: 实现客户端协议解析与暴露协议查询**

```go
func resolveModelsProtocol(c *gin.Context) string {
	if c.GetHeader("anthropic-version") != "" {
		return util.ChannelTypeAnthropic
	}
	if strings.HasPrefix(c.GetHeader("User-Agent"), "claude-cli") {
		return util.ChannelTypeAnthropic
	}
	if strings.Contains(strings.ToLower(c.GetHeader("User-Agent")), "codex") {
		return util.ChannelTypeCodex
	}
	return util.ChannelTypeOpenAI
}
```

并替换：

```go
models, err := s.getModelsByExposedProtocol(ctx, protocol)
```

- [ ] **Step 5: 修正 selector 只按暴露协议过滤**

```go
filterByProtocol := func(channels []*modelpkg.Config) []*modelpkg.Config {
	filtered := make([]*modelpkg.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg.SupportsProtocol(normalizedType) {
			filtered = append(filtered, cfg)
		}
	}
	return filtered
}
```

- [ ] **Step 6: 跑模型视图和 selector 回归**

Run: `go test -tags go_json ./internal/app -run 'handleList(OpenAI|Gemini)Models|SelectCandidatesByModelAndType' -v`

Expected: PASS

- [ ] **Step 7: 提交协议筛选修正**

```bash
git add internal/app/proxy_gemini.go internal/app/proxy_handler.go internal/app/selector.go internal/app/server.go internal/app/proxy_gemini_test.go internal/app/selector_test.go internal/app/server_misc_test.go
git commit -m "feat: route models and selectors by exposed protocol"
```

### Task 5: 裁剪并接入 protocol translator 核心

**Files:**
- Create: `internal/protocol/types.go`
- Create: `internal/protocol/registry.go`
- Create: `internal/protocol/request.go`
- Create: `internal/protocol/response.go`
- Create: `internal/protocol/builtin/register.go`
- Create: `internal/protocol/builtin/...`
- Test: `internal/protocol/request_test.go`
- Test: `internal/protocol/response_test.go`

- [ ] **Step 1: 先写 request/response 单测，锁定最小 API**

```go
func TestRegistry_TranslateRequest_OpenAIToGemini(t *testing.T) {
	reg := protocol.NewRegistry()
	protocolbuiltin.Register(reg)

	raw := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	got := reg.TranslateRequest(protocol.OpenAI, protocol.Gemini, "gemini-2.5-pro", raw, false)

	if !bytes.Contains(got, []byte(`"contents"`)) {
		t.Fatalf("translated request missing Gemini contents: %s", got)
	}
}
```

- [ ] **Step 2: 跑失败测试，确认新包还不存在**

Run: `go test -tags go_json ./internal/protocol/... -v`

Expected: 编译失败，提示 `internal/protocol` 或 `Register` 未定义。

- [ ] **Step 3: 建最小 registry 与 plan 类型**

```go
type Protocol string

const (
	Anthropic Protocol = "anthropic"
	Codex     Protocol = "codex"
	OpenAI    Protocol = "openai"
	Gemini    Protocol = "gemini"
)

type TransformPlan struct {
	ClientProtocol   Protocol
	UpstreamProtocol Protocol
	OriginalPath     string
	UpstreamPath     string
	OriginalBody     []byte
	TranslatedBody   []byte
	Streaming        bool
}
```

- [ ] **Step 4: 从 `CLIProxyAPI` 裁剪最小转换器**

```go
// internal/protocol/builtin/register.go
func Register(reg *protocol.Registry) {
	registerOpenAIToGemini(reg)
	registerGeminiToOpenAI(reg)
	registerAnthropicToGemini(reg)
	registerGeminiToAnthropic(reg)
	registerCodexToGemini(reg)
	registerGeminiToCodex(reg)
}
```

- [ ] **Step 5: 跑 protocol 单测**

Run: `go test -tags go_json ./internal/protocol/... -v`

Expected: PASS

- [ ] **Step 6: 提交 protocol 核心**

```bash
git add internal/protocol
git commit -m "feat: add internal protocol translation registry"
```

### Task 6: 接入请求翻译与非流式响应翻译

**Files:**
- Modify: `internal/app/proxy_forward.go`
- Modify: `internal/app/proxy_util.go`
- Test: `internal/app/proxy_handler_test.go`
- Test: `internal/app/proxy_forward_test.go`
- Test: `internal/app/proxy_integration_test.go`

- [ ] **Step 1: 写非流式失败测试**

```go
func TestHandleProxyRequest_TransformsOpenAIRequestToGeminiUpstream(t *testing.T) {
	upstreamBody := make(chan []byte, 1)
	server, upstream := newTransformTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`))
	})
	defer upstream.Close()

	req := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", req))
	server.HandleProxyRequest(c)

	if got := <-upstreamBody; !bytes.Contains(got, []byte(`"contents"`)) {
		t.Fatalf("upstream body not translated to Gemini: %s", got)
	}
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("response not translated back to OpenAI: %s", w.Body.String())
	}
}
```

- [ ] **Step 2: 跑失败测试**

Run: `go test -tags go_json ./internal/app -run 'TransformsOpenAIRequestToGeminiUpstream' -v`

Expected: FAIL，当前上游请求仍是 OpenAI body，返回也不会回翻。

- [ ] **Step 3: 在 forward 链插入 TransformPlan**

```go
plan, err := s.protocolRegistry.BuildPlan(
	reqCtx.clientProtocol,
	protocol.Protocol(cfg.GetChannelType()),
	requestPath,
	bodyToSend,
	actualModel,
	reqCtx.isStreaming,
)
if err != nil {
	return nil, cooldown.None
}

requestPath = plan.UpstreamPath
bodyToSend = plan.TranslatedBody
```

- [ ] **Step 4: 非流式响应先按上游解析，再翻译给客户端**

```go
rawBody, err := io.ReadAll(resp.Body)
if err != nil {
	return nil, reqCtx.Duration().Seconds(), err
}

parser := newJSONUsageParser(channelType)
_ = parser.Feed(rawBody)

clientBody, err := s.protocolRegistry.TranslateResponseNonStream(plan, rawBody)
if err != nil {
	return nil, reqCtx.Duration().Seconds(), err
}

_, _ = w.Write(clientBody)
```

- [ ] **Step 5: 跑非流式 proxy 回归**

Run: `go test -tags go_json ./internal/app -run 'Transform|NonStream|OpenAIRequestToGemini' -v`

Expected: PASS

- [ ] **Step 6: 提交非流式链路**

```bash
git add internal/app/proxy_forward.go internal/app/proxy_util.go internal/app/proxy_handler_test.go internal/app/proxy_forward_test.go internal/app/proxy_integration_test.go
git commit -m "feat: translate non-stream proxy requests and responses"
```

### Task 7: 接入流式响应翻译且保留上游 parser 语义

**Files:**
- Modify: `internal/app/proxy_forward.go`
- Modify: `internal/app/proxy_sse_parser.go` 仅当接口不够用时改
- Test: `internal/app/proxy_integration_test.go`
- Test: `internal/app/proxy_forward_test.go`

- [ ] **Step 1: 写流式失败测试**

```go
func TestHandleProxyRequest_TransformsStreamingGeminiToOpenAI(t *testing.T) {
	server, upstream := newTransformTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer upstream.Close()

	req := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, w := newTestContext(t, newJSONRequestBytes(http.MethodPost, "/v1/chat/completions", req))
	server.HandleProxyRequest(c)

	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("stream response not translated to OpenAI chunks: %s", w.Body.String())
	}
}
```

- [ ] **Step 2: 跑失败测试**

Run: `go test -tags go_json ./internal/app -run 'StreamingGeminiToOpenAI' -v`

Expected: FAIL，当前 chunk 直透，客户端拿到的是 Gemini 片段。

- [ ] **Step 3: 改造 stream copy 回调，先喂 parser 再写翻译 chunk**

```go
translatedChunks, err := s.protocolRegistry.TranslateResponseStream(plan, rawChunk, &state)
if err != nil {
	return err
}
for _, chunk := range translatedChunks {
	if _, err := w.Write(chunk); err != nil {
		return err
	}
}
```

- [ ] **Step 4: 明确保留内部诊断**

```go
if err := parser.Feed(rawChunk); err != nil {
	return err
}
// parser 始终吃上游原始 chunk
// translator 只负责写给客户端的字节
```

- [ ] **Step 5: 跑流式回归**

Run: `go test -tags go_json ./internal/app -run 'Streaming|SSEError|1308|Transform' -v`

Expected: PASS，且 1308/SSE 错误测试不回归。

- [ ] **Step 6: 提交流式链路**

```bash
git add internal/app/proxy_forward.go internal/app/proxy_sse_parser.go internal/app/proxy_integration_test.go internal/app/proxy_forward_test.go
git commit -m "feat: translate streaming proxy responses"
```

### Task 8: 前端渠道编辑 UI 与提交载荷

**Files:**
- Modify: `web/channels.html`
- Modify: `web/assets/js/channels-modals.js`
- Modify: `web/assets/js/ui.js`
- Modify: `web/assets/locales/zh-CN.js`
- Modify: `web/assets/locales/en.js`
- Test: `web/assets/js/channels-protocol-transforms.test.js`

- [ ] **Step 1: 写前端失败测试**

```js
test('saveChannel includes protocol_transforms and keeps channel_type separate', async () => {
  document.body.innerHTML = createChannelModalFixture();
  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios', 'gemini');
  renderProtocolTransformCheckboxes(['openai', 'anthropic']);

  document.querySelector('input[name="channelType"][value="gemini"]').checked = true;
  document.querySelector('input[name="protocolTransform"][value="openai"]').checked = true;

  const payload = await collectChannelPayloadForTest();
  assert.equal(payload.channel_type, 'gemini');
  assert.deepEqual(payload.protocol_transforms, ['openai']);
});
```

- [ ] **Step 2: 跑前端单测，确认当前页面没有 transform 控件**

Run: `node --test web/assets/js/channels-protocol-transforms.test.js`

Expected: FAIL，fixture 中不存在 `protocolTransform` 控件或 payload 不含该字段。

- [ ] **Step 3: 在弹窗中增加多选控件**

```html
<div class="form-group">
  <label data-i18n="channels.modal.protocolTransforms">额外协议转换</label>
  <div id="protocolTransformsContainer" class="protocol-transforms-group"></div>
  <small data-i18n="channels.modal.protocolTransformsHint">仅声明额外暴露协议，不包含原生上游协议</small>
</div>
```

- [ ] **Step 4: 在 JS 中渲染、回填、收集 payload**

```js
function getSelectedProtocolTransforms(channelType) {
  return Array.from(document.querySelectorAll('input[name="protocolTransform"]:checked'))
    .map(input => input.value)
    .filter(value => value !== channelType)
    .sort();
}

formData.protocol_transforms = getSelectedProtocolTransforms(channelType);
```

- [ ] **Step 5: 跑前端回归**

Run: `node --test web/assets/js/channels-protocol-transforms.test.js web/assets/js/channels-actions.test.js`

Expected: PASS

- [ ] **Step 6: 提交前端变更**

```bash
git add web/channels.html web/assets/js/channels-modals.js web/assets/js/ui.js web/assets/locales/zh-CN.js web/assets/locales/en.js web/assets/js/channels-protocol-transforms.test.js
git commit -m "feat(web): add protocol transforms to channel editor"
```

### Task 9: 端到端验证与收尾

**Files:**
- Modify: `docs/superpowers/specs/2026-04-12-channel-protocol-transforms-design.md` 仅在实现偏离 spec 时回填

- [ ] **Step 1: 跑后端完整回归**

Run: `go test -tags go_json ./internal/... -v`

Expected: 除已知基线失败外，新加协议转换相关测试全部通过。

- [ ] **Step 2: 跑竞态与前端测试**

Run: `go test -tags go_json -race ./internal/...`

Expected: 通过；若 `TestAnthropicModelsFetcher` 仍因基线问题失败，单独记录，不把它算进本次回归。

Run: `make web-test`

Expected: PASS

- [ ] **Step 3: 跑 lint**

Run: `golangci-lint run ./...`

Expected: PASS，零警告。

- [ ] **Step 4: 手工烟测说明**

```text
1. 新建 gemini 渠道，勾选 openai + anthropic transform
2. 在管理页点击“从渠道API获取模型”
3. 用 OpenAI /v1/models 查看模型是否可见
4. 用 Anthropic /v1/messages 发请求，确认实际命中 gemini 上游
5. 检查日志中的 channel_type 仍是 gemini，usage 和错误分类正常
```

- [ ] **Step 5: 提交最终整合**

```bash
git add .
git commit -m "feat: add channel protocol transforms"
```

---

## Review Notes

- Spec coverage: 已覆盖数据模型、CSV、模型视图、候选渠道、协议转换核心、前端交互和验证收口。
- Placeholder scan: 无 `TODO/TBD/implement later` 之类占位词。
- Type consistency:
  - 一律使用 `protocol_transforms`
  - Store 查询一律使用 `ExposedProtocol`
  - `channel_type` 始终表示上游真实协议

## Progress Note (2026-04-12)

截至当前工作树状态，以下任务已基本落地并有 targeted 验证：

- Task 1 / Task 2：`protocol_transforms` 持久化、迁移、缓存索引
- Task 3：Admin API + CSV
- Task 4：模型视图与按暴露协议筛选
- Task 5：`internal/protocol` 核心
- Task 6 / Task 7：最小代理链已扩展到：
  - OpenAI / Anthropic / Codex -> Gemini
  - OpenAI / Codex -> Anthropic
  - 覆盖非流式和流式最小路径
- Task 8：前端弹窗与 payload

当前仍待本地最终收口：

- `go test -tags go_json ./internal/... -v`
- `go test -tags go_json -race ./internal/...`
- `make web-test`
- `golangci-lint run ./...`

说明：

- 当前沙箱下，full internal suite 已实际跑过，最终失败点落在 `internal/util/TestAnthropicModelsFetcher`：`httptest.NewServer` 触发 `listen tcp6 [::1]:0: bind: operation not permitted`
- 当前沙箱下，如不显式设置 `GOCACHE` / `GOLANGCI_LINT_CACHE` 到工作树可写目录，Go 构建与 lint 会被缓存目录权限问题误伤
- 当前沙箱下无法创建 `.git/worktrees/.../index.lock`，因此不能完成提交步骤
- 当前沙箱下已经额外验证通过：
  - `GOCACHE=$(pwd)/.cache/go-build go test -tags go_json ./internal/... -run '^$'`
  - `GOCACHE=$(pwd)/.cache/go-build go test -tags go_json ./internal/... -v`（除 `internal/util/TestAnthropicModelsFetcher` 的 socket 绑定限制外，其余已跑通）
  - `GOCACHE=$(pwd)/.cache/go-build go test -tags go_json -race ./internal/protocol ./internal/app -run 'ProtocolTransforms|ExposedProtocol|OpenAIToGemini|AnthropicToGemini|CodexToGemini|OpenAIToAnthropic|CodexToAnthropic|UnsupportedStructured|UsesResolvedActualModel|TextPlainSSE'`
  - `GOCACHE=$(pwd)/.cache/go-build go test -tags go_json -race ./internal/storage/... ./internal/util -run 'GetEnabledChannelsByExposedProtocol|Classify|ChannelType' -v`
  - `GOCACHE=$(pwd)/.cache/go-build GOLANGCI_LINT_CACHE=$(pwd)/.cache/golangci-lint golangci-lint run ./internal/protocol/... ./internal/app/... ./internal/storage/... ./internal/util/...`
  - `GOCACHE=$(pwd)/.cache/go-build GOLANGCI_LINT_CACHE=$(pwd)/.cache/golangci-lint golangci-lint run ./...`
  - `make web-test`
