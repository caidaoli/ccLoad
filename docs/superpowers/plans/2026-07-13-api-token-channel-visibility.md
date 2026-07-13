# API Token Channel Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 API Token Web 会话在服务端强制绑定登录 Token 的前提下，复用现有页面展示渠道类型、渠道名、渠道 ID、模型、来源和渠道统计，同时保持渠道管理只读且不泄漏上游凭证。

**Architecture:** `ApplyWebIdentityScope` 继续作为唯一的数据隔离边界，覆盖客户端传入的 `auth_token_id`。删除响应层对渠道维度的额外裁剪，并增加只返回 Token 作用域内渠道安全投影的 `/dashboard/channels` 只读接口；前端根据会话角色选择 `/admin` 或 `/dashboard` 读取路径，但沿用同一套筛选、排序和渲染代码。

**Tech Stack:** Go 1.25、Gin、SQLite/MySQL storage abstraction、原生 JavaScript、Node `node:test`、Sonic build tag。

## Global Constraints

- 所有 Go 命令必须带 `-tags sonic`。
- API Token 请求必须在服务端覆盖 `LogFilter.AuthTokenID`，不能信任前端角色或查询参数。
- Token 模式只读；所有 `/admin/*` 写接口继续由 `RequireAdminAuth` 返回 `403`。
- Token 响应不得包含 API Key、API Key 哈希、完整上游 URL、代理 URL、客户端 IP 或调试日志内容。
- 管理员接口、管理员页面数据结构和现有行为保持不变。
- 复用现有页面和查询组件，不创建第二套 Token 专用页面。
- 测试公开 handler、API 响应和浏览器工作流，不断言私有函数委托或源码字符串。

---

## File Structure

- `internal/app/dashboard_scope.go`：Token 安全日志投影和 Web 身份作用域；删除不再需要的按模型聚合逻辑。
- `internal/app/admin_stats.go`：让已作用域的 stats、metrics、models、filter-options 保留渠道维度。
- `internal/app/admin_logs_bootstrap.go`：Token 日志页 bootstrap 返回已作用域的渠道和模型选项。
- `internal/app/dashboard_channels.go`：新增 Token 安全的只读渠道列表和筛选选项 handler。
- `internal/app/server.go`：注册 `/dashboard/channels` 只读路由。
- `internal/app/dashboard_scope_test.go`、`internal/app/web_auth_test.go`：后端公开行为测试。
- `web/assets/js/web-auth.js`：允许 Token 导航到 channels 页面。
- `web/assets/js/channels-state.js`：集中判断 channels 页面是否为 Token 只读模式。
- `web/assets/js/channels-data.js`：管理员读取 `/admin/*`，Token 读取 `/dashboard/*`。
- `web/assets/js/channels-init.js`、`channels-render.js`、`styles.css`：应用只读模式并隐藏管理动作。
- `web/assets/js/web-auth.test.js`、`channels-test.js`：前端角色、端点和只读行为测试。

---

### Task 1: 恢复已作用域统计、趋势、模型和日志的渠道字段

**Files:**
- Modify: `internal/app/dashboard_scope.go`
- Modify: `internal/app/admin_stats.go`
- Modify: `internal/app/admin_logs_bootstrap.go`
- Test: `internal/app/dashboard_scope_test.go`

**Interfaces:**
- Consumes: `BuildLogFilter(*gin.Context) model.LogFilter` 和 `ApplyWebIdentityScope(*gin.Context, *model.LogFilter)`。
- Produces: `projectTokenLogs(logs []*model.LogEntry, channelTypes map[int64]string) []tokenLogEntry`。

- [ ] **Step 1: 修改 handler 测试，声明新的公开响应契约**

在 `internal/app/dashboard_scope_test.go` 更新现有测试：

```go
func TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields(t *testing.T) {
	// 沿用当前测试的数据准备和 response 解码，然后替换字段断言：
	entry := response.Data[0]
	assertJSONNumber(t, entry, "channel_id", float64(secretChannel.ID))
	assertJSONString(t, entry, "channel_name", "secret-channel")
	assertJSONString(t, entry, "channel_type", "openai")
	assertJSONString(t, entry, "log_source", model.LogSourceProxy)
	assertJSONString(t, entry, "model", "gpt-5.6")
	for _, key := range []string{"api_key_used", "api_key_hash", "auth_token_id", "client_ip", "base_url"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("safe log response exposed %q", key)
		}
	}
}

func TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels(t *testing.T) {
	wantChannels := map[int64]string{ownerChannel.ID: "owner-channel", ownerChannel2.ID: "owner-channel-2"}
	if got := channelNameMap(models.Channels); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("models channels=%v, want %v", got, wantChannels)
	}
	for _, point := range metrics {
		if _, leaked := point.Channels["foreign-channel"]; leaked {
			t.Fatalf("metrics exposed foreign channel: %v", point.Channels)
		}
	}
	if got := statsChannelNameMap(statsData.Stats); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("stats channels=%v, want %v", got, wantChannels)
	}
	if got := channelNameMap(bootstrap.Channels); !reflect.DeepEqual(got, wantChannels) {
		t.Fatalf("bootstrap channels=%v, want %v", got, wantChannels)
	}
	if len(bootstrap.AuthTokens) != 0 {
		t.Fatalf("bootstrap auth tokens=%d, want 0", len(bootstrap.AuthTokens))
	}
}
```

在同一测试文件增加只解析公开响应的 helper：

```go
func assertJSONNumber(t testing.TB, entry map[string]json.RawMessage, key string, want float64) {
	t.Helper()
	var got float64
	if err := json.Unmarshal(entry[key], &got); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s=%v, want %v", key, got, want)
	}
}

func assertJSONString(t testing.TB, entry map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(entry[key], &got); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s=%q, want %q", key, got, want)
	}
}

func channelNameMap(channels []model.ChannelNameID) map[int64]string {
	result := make(map[int64]string, len(channels))
	for _, channel := range channels {
		result[channel.ID] = channel.Name
	}
	return result
}

func statsChannelNameMap(stats []model.StatsEntry) map[int64]string {
	result := make(map[int64]string, len(stats))
	for _, entry := range stats {
		if entry.ChannelID != nil {
			result[int64(*entry.ChannelID)] = entry.ChannelName
		}
	}
	return result
}
```

保留现有 `summary.TotalRequests == 2`，证明展示字段恢复没有破坏 Token 计数作用域。

- [ ] **Step 2: 运行测试确认旧实现失败**

```bash
go test -tags sonic ./internal/app -run '^(TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields|TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels)$' -count=1
```

Expected: FAIL，日志缺少渠道/来源字段，stats/metrics/models/bootstrap 仍隐藏渠道。

- [ ] **Step 3: 扩展安全日志投影并删除多余聚合代码**

在 `internal/app/dashboard_scope.go` 将投影扩展为：

```go
type tokenLogEntry struct {
	ID                       int64          `json:"id"`
	Time                     model.JSONTime `json:"time"`
	ChannelID                int64          `json:"channel_id"`
	ChannelName              string         `json:"channel_name"`
	ChannelType              string         `json:"channel_type"`
	LogSource                string         `json:"log_source"`
	Model                    string         `json:"model"`
	ActualModel              string         `json:"actual_model,omitempty"`
	StatusCode               int            `json:"status_code"`
	Message                  string         `json:"message"`
	Duration                 float64        `json:"duration"`
	IsStreaming              bool           `json:"is_streaming"`
	FirstByteTime            float64        `json:"first_byte_time"`
	ServiceTier              string         `json:"service_tier,omitempty"`
	ThinkingEffort           string         `json:"thinking_effort,omitempty"`
	InputTokens              int            `json:"input_tokens"`
	OutputTokens             int            `json:"output_tokens"`
	ReasoningTokens          int            `json:"reasoning_tokens,omitempty"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	Cache5mInputTokens       int            `json:"cache_5m_input_tokens"`
	Cache1hInputTokens       int            `json:"cache_1h_input_tokens"`
	Cost                     float64        `json:"cost"`
	EffectiveCost            float64        `json:"effective_cost"`
}

func projectTokenLogs(logs []*model.LogEntry, channelTypes map[int64]string) []tokenLogEntry {
	projected := make([]tokenLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry == nil {
			continue
		}
		multiplier := entry.CostMultiplier
		if multiplier < 0 {
			multiplier = 1
		}
		projected = append(projected, tokenLogEntry{
			ID: entry.ID, Time: entry.Time,
			ChannelID: entry.ChannelID, ChannelName: entry.ChannelName,
			ChannelType: channelTypes[entry.ChannelID], LogSource: entry.LogSource,
			Model: entry.Model, ActualModel: entry.ActualModel,
			StatusCode: entry.StatusCode, Message: sanitizeTokenLogMessage(entry),
			Duration: entry.Duration, IsStreaming: entry.IsStreaming, FirstByteTime: entry.FirstByteTime,
			ServiceTier: entry.ServiceTier, ThinkingEffort: entry.ThinkingEffort,
			InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
			ReasoningTokens: entry.ReasoningTokens,
			CacheReadInputTokens: entry.CacheReadInputTokens,
			CacheCreationInputTokens: entry.CacheCreationInputTokens,
			Cache5mInputTokens: entry.Cache5mInputTokens, Cache1hInputTokens: entry.Cache1hInputTokens,
			Cost: entry.Cost, EffectiveCost: entry.Cost * multiplier,
		})
	}
	return projected
}
```

删除只为隐藏渠道而存在的 `tokenStatsAccumulator`、`aggregateTokenStats`、`addInt64Ptr`、`addFloat64Ptr`、`newerTimestamp`。`sanitizeTokenLogMessage` 仍清洗消息正文中的 URL、Key、哈希、渠道名和客户端 IP。

- [ ] **Step 4: 让 dashboard handler 返回原始渠道维度**

在 `internal/app/admin_stats.go`：

```go
func (s *Server) HandleMetrics(c *gin.Context) {
	// 删除 `if isAPITokenWebRequest(c)` 清空 Channels 的整个分支；其余代码不变。
}

func (s *Server) HandleStats(c *gin.Context) {
	channelHealth := s.fillHealthTimeline(c.Request.Context(), stats, startTime, endTime, &lf, isToday)
	RespondJSON(c, http.StatusOK, gin.H{
		"stats": stats, "duration_seconds": durationSeconds,
		"rpm_stats": rpmStats, "is_today": isToday, "channel_health": channelHealth,
	})
}
```

`HandleGetModels` 和 `HandleStatsFilterOptions` 始终执行带 `BuildLogFilter` 的渠道查询。

为 `HandleErrors` 增加失败可降级的类型映射：

```go
func (s *Server) tokenLogChannelTypes(ctx context.Context, logs []*model.LogEntry) map[int64]string {
	needed := make(map[int64]struct{})
	for _, entry := range logs {
		if entry != nil && entry.ChannelID > 0 {
			needed[entry.ChannelID] = struct{}{}
		}
	}
	types := make(map[int64]string, len(needed))
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return types
	}
	for _, cfg := range configs {
		if _, ok := needed[cfg.ID]; ok {
			types[cfg.ID] = cfg.ChannelType
		}
	}
	return types
}
```

调用 `projectTokenLogs(logs, s.tokenLogChannelTypes(c.Request.Context(), logs))`。类型补全失败时允许空类型，不得退回未投影日志。

- [ ] **Step 5: Token logs bootstrap 返回已作用域渠道选项**

在 `handleTokenLogsBootstrap` 使用同一个 `filter := BuildLogFilter(c)`：

```go
models, err := s.store.GetDistinctModels(ctx, since, until, "", &filter)
// 现有错误响应
channels, err := s.store.GetDistinctChannels(ctx, since, until, "", &filter)
// 现有错误响应
RespondJSON(c, http.StatusOK, LogsBootstrapResponse{
	AuthTokens: make([]*model.AuthToken, 0),
	Models: models,
	Channels: channels,
})
```

不得返回全量 Token 列表或管理员设置。

- [ ] **Step 6: 运行测试并提交**

```bash
go test -tags sonic ./internal/app -run '^(TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields|TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels)$' -count=1
git add internal/app/dashboard_scope.go internal/app/admin_stats.go internal/app/admin_logs_bootstrap.go internal/app/dashboard_scope_test.go
git commit -m "feat: expose token-scoped channel dimensions"
```

Expected: 测试 PASS，提交只包含 Task 1 文件。

---

### Task 2: 新增 Token 安全的只读渠道查询

**Files:**
- Create: `internal/app/dashboard_channels.go`
- Modify: `internal/app/server.go`
- Modify: `internal/app/admin_channels.go`
- Test: `internal/app/dashboard_scope_test.go`
- Test: `internal/app/web_auth_test.go`

**Interfaces:**
- Consumes: `BuildLogFilter`、`ParsePaginationParams`、`GetDistinctChannels`、`ListConfigs`、`applyChannelListFilters`、`paginateChannels`。
- Produces: `HandleDashboardChannels`、`HandleDashboardChannelFilterOptions`。

- [ ] **Step 1: 写只读渠道 API 失败测试**

```go
func TestDashboardChannelsForceTokenScopeAndHideSensitiveConfig(t *testing.T) {
	response := mustParseAPIResponse[[]map[string]json.RawMessage](t, w.Body.Bytes())
	if len(response.Data) != 1 {
		t.Fatalf("channels=%d, want 1", len(response.Data))
	}
	entry := response.Data[0]
	assertJSONNumber(t, entry, "id", float64(ownerChannel.ID))
	assertJSONString(t, entry, "name", "owner-channel")
	assertJSONString(t, entry, "channel_type", "openai")
	for _, key := range []string{"url", "proxy_url", "custom_request_rules", "key_strategy", "key_cooldowns"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("dashboard channel exposed %q", key)
		}
	}
}

func TestDashboardChannelFilterOptionsUseBoundToken(t *testing.T) {
	data := mustParseAPIResponse[struct {
		ChannelNames []string `json:"channel_names"`
		Models       []string `json:"models"`
	}](t, w.Body.Bytes()).Data
	if !reflect.DeepEqual(data.ChannelNames, []string{"owner-channel"}) {
		t.Fatalf("channel names=%v", data.ChannelNames)
	}
	if !reflect.DeepEqual(data.Models, []string{"owner-model"}) {
		t.Fatalf("models=%v", data.Models)
	}
}
```

在 `web_auth_test.go` 增加 `TestAPITokenChannelRoutesAreReadOnly`：Token 会话 GET `/dashboard/channels` 为 200，POST `/admin/channels` 为 403。

- [ ] **Step 2: 运行测试确认 handler/route 尚不存在**

```bash
go test -tags sonic ./internal/app -run '^(TestDashboardChannelsForceTokenScopeAndHideSensitiveConfig|TestDashboardChannelFilterOptionsUseBoundToken|TestAPITokenChannelRoutesAreReadOnly)$' -count=1
```

Expected: FAIL。

- [ ] **Step 3: 实现安全响应类型和 Token 可见渠道集合**

创建 `internal/app/dashboard_channels.go`：

```go
package app

import (
	"net/http"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type dashboardChannelView struct {
	ID                    int64              `json:"id"`
	Name                  string             `json:"name"`
	ChannelType           string             `json:"channel_type"`
	ProtocolTransformMode string             `json:"protocol_transform_mode,omitempty"`
	ProtocolTransforms    []string           `json:"protocol_transforms,omitempty"`
	Priority              int                `json:"priority"`
	Enabled               bool               `json:"enabled"`
	Models                []model.ModelEntry `json:"models"`
	CostMultiplier        float64            `json:"cost_multiplier"`
	CooldownRemainingMS   int64              `json:"cooldown_remaining_ms,omitempty"`
}

func (s *Server) tokenScopedChannelConfigs(c *gin.Context) ([]*model.Config, map[int64]time.Time, error) {
	params := ParsePaginationParams(c)
	if params.Range == "" {
		params.Range = "today"
	}
	since, until := params.GetTimeRange()
	filter := BuildLogFilter(c)
	filter.LogSource = model.LogSourceProxy
	visible, err := s.store.GetDistinctChannels(c.Request.Context(), since, until, "", &filter)
	if err != nil {
		return nil, nil, err
	}
	visibleIDs := make(map[int64]struct{}, len(visible))
	for _, channel := range visible {
		visibleIDs[channel.ID] = struct{}{}
	}
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		return nil, nil, err
	}
	scoped := make([]*model.Config, 0, len(visibleIDs))
	for _, cfg := range configs {
		if _, ok := visibleIDs[cfg.ID]; ok {
			scoped = append(scoped, cfg)
		}
	}
	cooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
	if err != nil {
		cooldowns = make(map[int64]time.Time)
	}
	return scoped, cooldowns, nil
}
```

不得调用 `GetAllAPIKeys`、`getAllKeyCooldowns` 或返回 `ChannelWithCooldown`。

- [ ] **Step 4: 实现列表与筛选选项 handler**

```go
func (s *Server) HandleDashboardChannels(c *gin.Context) {
	configs, cooldowns, err := s.tokenScopedChannelConfigs(c)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
	configs = applyChannelListFilters(configs, c, cooldowns, now)
	total := len(configs)
	configs = paginateChannels(configs, c)
	out := make([]dashboardChannelView, 0, len(configs))
	for _, cfg := range configs {
		view := dashboardChannelView{
			ID: cfg.ID, Name: cfg.Name, ChannelType: cfg.ChannelType,
			ProtocolTransformMode: cfg.ProtocolTransformMode,
			ProtocolTransforms: append([]string(nil), cfg.ProtocolTransforms...),
			Priority: cfg.Priority, Enabled: cfg.Enabled,
			Models: append([]model.ModelEntry(nil), cfg.ModelEntries...),
			CostMultiplier: cfg.CostMultiplier,
		}
		if until, ok := cooldowns[cfg.ID]; ok && until.After(now) {
			view.CooldownRemainingMS = until.Sub(now).Milliseconds()
		}
		out = append(out, view)
	}
	RespondPaginated(c, http.StatusOK, out, total)
}
```

从 `HandleChannelsFilterOptions` 提取响应类型、type/status 过滤和去重纯函数：

```go
type channelFilterOptionsResponse struct {
	ChannelNames []string `json:"channel_names"`
	Models       []string `json:"models"`
}

func buildChannelFilterOptions(cfgs []*model.Config) channelFilterOptionsResponse

func filterChannelOptionConfigs(
	cfgs []*model.Config,
	channelType string,
	status string,
	cooldowns map[int64]time.Time,
	now time.Time,
) []*model.Config
```

`filterChannelOptionConfigs` 搬运当前 `HandleChannelsFilterOptions` 中完全相同的 type/status 语义，不读取 search/model/pagination 参数。管理员 handler 和新的 dashboard handler 都按以下顺序调用，避免筛选下拉被当前名称或模型选择意外收窄：

```go
configs = filterChannelOptionConfigs(
	configs,
	strings.TrimSpace(c.Query("type")),
	strings.TrimSpace(c.Query("status")),
	cooldowns,
	time.Now(),
)
RespondJSON(c, http.StatusOK, buildChannelFilterOptions(configs))
```

- [ ] **Step 5: 注册只读路由**

在 `internal/app/server.go` 的 `/dashboard` 组加入：

```go
dashboard.GET("/channels", s.HandleDashboardChannels)
dashboard.GET("/channels/filter-options", s.HandleDashboardChannelFilterOptions)
```

不要注册 POST/PUT/DELETE、keys、URL 或 test 路由。

- [ ] **Step 6: 运行测试并提交**

```bash
go test -tags sonic ./internal/app -run '^(TestDashboardChannelsForceTokenScopeAndHideSensitiveConfig|TestDashboardChannelFilterOptionsUseBoundToken|TestAPITokenChannelRoutesAreReadOnly)$' -count=1
git add internal/app/dashboard_channels.go internal/app/admin_channels.go internal/app/server.go internal/app/dashboard_scope_test.go internal/app/web_auth_test.go
git commit -m "feat: add token-scoped read-only channels"
```

Expected: PASS。

---

### Task 3: 让现有 channels 页面切换到 Token 只读数据源

**Files:**
- Modify: `web/assets/js/web-auth.js`
- Modify: `web/assets/js/channels-state.js`
- Modify: `web/assets/js/channels-data.js`
- Modify: `web/assets/js/channels-init.js`
- Modify: `web/assets/js/channels-render.js`
- Modify: `web/assets/css/styles.css`
- Modify: `web/channels.html`（为新增按钮补稳定 ID；导入/导出按钮已有 ID）
- Test: `web/assets/js/web-auth.test.js`
- Test: `web/assets/js/channels-test.js`

**Interfaces:**
- Consumes: `window.WebAuth.isAPITokenRole(localStorage)`。
- Produces: `isTokenChannelsReadOnly() boolean`、`channelsReadURL(adminPath, dashboardPath) string`。

- [ ] **Step 1: 写导航、端点选择和只读模式测试**

在 `web-auth.test.js`：

```javascript
assert.deepEqual(
  WebAuth.filterNavigation(['index', 'channels', 'tokens', 'stats'], 'api_token'),
  ['index', 'channels', 'stats']
);
```

在 `channels-test.js` 增加：

```javascript
test('api token channels use dashboard read endpoints', async () => {
  const runtime = loadChannelsData({ role: 'api_token' });
  await runtime.mod.loadChannels('all');
  await runtime.mod.loadChannelsFilterOptions('all', 'all');
  await runtime.mod.loadChannelStats('today');
  assert.match(runtime.urls[0], /^\/dashboard\/channels\?/);
  assert.match(runtime.urls[1], /^\/dashboard\/channels\/filter-options\?/);
  assert.match(runtime.urls[2], /^\/dashboard\/stats\?/);
  assert.ok(runtime.urls.every((url) => !url.includes('auth_token_id=')));
  runtime.restore();
});

test('admin channels keep admin endpoints', async () => {
  const runtime = loadChannelsData({ role: 'admin' });
  await runtime.mod.loadChannels('all');
  await runtime.mod.loadChannelsFilterOptions('all', 'all');
  await runtime.mod.loadChannelStats('today');
  assert.match(runtime.urls[0], /^\/admin\/channels\?/);
  assert.match(runtime.urls[1], /^\/admin\/channels\/filter-options\?/);
  assert.match(runtime.urls[2], /^\/admin\/stats\?/);
  runtime.restore();
});

```

在测试文件按现有 `replaceGlobals` 模式实现 `loadChannelsData`，并在 `channels-data.js` 的 Node 环境导出数据加载入口；浏览器环境仍使用现有全局函数。只读模式的 DOM 隐藏属于展示行为，不新增 CSS class、`hidden` 或具体标记断言，统一放到 Task 4 的真实浏览器工作流验证。

- [ ] **Step 2: 运行 Web 测试确认失败**

```bash
node --test web/assets/js/web-auth.test.js web/assets/js/channels-test.js
```

Expected: FAIL，Token 导航尚未包含 channels，读取接口仍指向 `/admin/*`。

- [ ] **Step 3: 增加角色与读取路径 helper**

在 `web-auth.js`：

```javascript
const API_TOKEN_NAV = new Set(['index', 'channels', 'stats', 'trend', 'logs', 'model-test']);
```

在 `channels-state.js`：

```javascript
function isTokenChannelsReadOnly() {
  return Boolean(window.WebAuth && window.WebAuth.isAPITokenRole(localStorage));
}

function channelsReadURL(adminPath, dashboardPath) {
  return isTokenChannelsReadOnly() ? dashboardPath : adminPath;
}
```

- [ ] **Step 4: 复用 loader，只替换读取路径**

在 `channels-data.js` 保留现有分页、赋值和错误处理，只替换 URL，并在文件末尾导出测试入口：

```javascript
const listBase = channelsReadURL('/admin/channels', '/dashboard/channels');
params.set('range', channelStatsRange);
const resp = await fetchAPIWithAuth(`${listBase}?${params.toString()}`);

const optionsBase = channelsReadURL('/admin/channels/filter-options', '/dashboard/channels/filter-options');
params.set('range', channelStatsRange);
const data = await fetchDataWithAuth(`${optionsBase}?${params.toString()}`);

const statsBase = channelsReadURL('/admin/stats', '/dashboard/stats');
const data = await fetchDataWithAuth(`${statsBase}?${params.toString()}`);

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { loadChannels, loadChannelsFilterOptions, loadChannelStats };
}
```

不得添加 `auth_token_id` 参数；Token ID 只来自 Web session。

- [ ] **Step 5: 初始化只读模式并阻止管理动作**

在 `channels-init.js`：

```javascript
function applyChannelsAccessMode() {
	const readOnly = isTokenChannelsReadOnly();
	document.body.classList.toggle('channels-readonly', readOnly);
	for (const id of ['addChannelBtn', 'exportCsvBtn', 'importCsvBtn', 'batchFloatingMenu']) {
		const el = document.getElementById(id);
		if (el) el.hidden = readOnly;
	}
}
```

使用页面真实的新增按钮 ID；若没有，给按钮增加稳定 ID。DOM 初始化最前面调用该函数。Token 模式固定 `channelStatsRange = 'today'`，跳过 `loadChannelStatsRange` 和 `loadDefaultTestContent` 的管理员设置请求。

在 `channels-render.js` 的事件委托加入：

```javascript
if (isTokenChannelsReadOnly() && ['edit', 'test', 'copy', 'delete', 'toggle'].includes(action)) {
  return;
}
```

- [ ] **Step 6: CSS 隐藏敏感和管理列**

在 `styles.css`：

```css
.channels-readonly .ch-col-checkbox,
.channels-readonly .ch-col-enabled,
.channels-readonly .ch-col-actions,
.channels-readonly .ch-url-line,
.channels-readonly #batchFloatingMenu,
.channels-readonly #addChannelBtn,
.channels-readonly #exportCsvBtn,
.channels-readonly #importCsvBtn {
  display: none !important;
}
```

Token API 本身不返回 `url`；CSS 只负责布局，不是安全边界。

- [ ] **Step 7: 运行 Web 测试并提交**

```bash
node --test web/assets/js/web-auth.test.js web/assets/js/channels-test.js
make verify-web
git add web/assets/js/web-auth.js web/assets/js/channels-state.js web/assets/js/channels-data.js web/assets/js/channels-init.js web/assets/js/channels-render.js web/assets/css/styles.css web/assets/js/web-auth.test.js web/assets/js/channels-test.js web/channels.html
git commit -m "feat: add read-only token channel view"
```

Expected: 全部 PASS。

---

### Task 4: 全链路验证和安全回归

**Files:**
- No planned production edits; return to the failing task if verification exposes a defect.

**Interfaces:**
- Consumes: Tasks 1-3 的 `/dashboard` API 与前端只读模式。
- Produces: 可交付功能和验证证据。

- [ ] **Step 1: 运行完整测试、lint 和构建**

```bash
go test -tags sonic ./internal/...
make verify-web
golangci-lint run ./...
make build
git diff --check
```

Expected: 后端和 Web 测试全部 PASS，lint 为 `0 issues`，构建完成，无空白错误。

- [ ] **Step 2: 真实 Token 会话验证服务端覆盖查询参数**

```bash
curl -sS 'http://localhost:1111/dashboard/stats?range=this_month&auth_token_id=999999' \
  -H "Authorization: Bearer $WEB_SESSION_TOKEN" | jq '.data.stats'

curl -sS 'http://localhost:1111/dashboard/channels?range=this_month&auth_token_id=999999' \
  -H "Authorization: Bearer $WEB_SESSION_TOKEN" | jq '.data'
```

Expected:

- 两个响应只包含登录 Token 的渠道。
- Stats 有 `channel_id/channel_name/channel_type/model`。
- Channels 没有 `url/proxy_url/custom_request_rules/key_strategy/key_cooldowns`。
- 伪造 Token ID 不改变结果。

- [ ] **Step 3: 浏览器验证 channels 页面**

- Token 登录：导航显示“渠道”；页面显示渠道类型、名称、模型、来源和统计；不显示 URL、新增、启停、编辑、复制、测试、删除、批量选择和 Key 管理。
- 管理员登录：页面与当前行为一致，所有管理能力仍存在。
- Token 用户直接 POST `/admin/channels`：返回 `403`。

- [ ] **Step 4: 提交验证发现的必要修复**

若 Step 1-3 无缺陷，不创建空提交。若有真实缺陷，先为该缺陷补公开行为失败测试，再回到对应 Task 的实现与提交步骤，不在验证阶段打包临时修补。
