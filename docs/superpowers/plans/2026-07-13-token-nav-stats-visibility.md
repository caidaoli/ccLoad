# Token Navigation and Stats Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Token 顶部导航移除渠道管理和模型测试，同时在调用统计中恢复渠道名称与渠道健康度显示。

**Architecture:** 保留现有服务端 Token 作用域和页面路由，只修正两个前端展示边界：`WebAuth.filterNavigation` 的 Token 白名单，以及误隐藏统计渠道列的角色 CSS。后端 `HandleStats` 已返回作用域内 `channel_name` 和 `health_timeline`，只补公开响应契约测试，不增加新的服务端实现。

**Tech Stack:** 原生 JavaScript、Node `node:test`、Go 1.25、Gin、Sonic build tag、现有共享统计页面。

## Global Constraints

- 所有 Go 命令必须带 `-tags sonic`。
- API Token 请求必须在服务端覆盖 `LogFilter.AuthTokenID`，不能信任前端角色或查询参数。
- Token 顶部导航固定为 `index`、`stats`、`trend`、`logs`。
- 不删除或修改 `channels.html`、`model-test.html` 的直接 URL 行为与既有后端路由。
- 管理员导航、统计页面结构和现有行为保持不变。
- 不为 CSS、`hidden`、HTML 结构或具体展示标记新增自动化断言。
- 测试公开模块输出和 handler/API 响应，不断言私有函数委托或源码字符串。

---

## File Structure

- `web/assets/js/web-auth.js`：Token 顶部导航白名单。
- `web/assets/js/web-auth.test.js`：公开导航过滤契约。
- `web/assets/css/styles.css`：Token 角色敏感列隐藏规则；统计渠道列不再隐藏。
- `internal/app/dashboard_scope_test.go`：公开 `/dashboard/stats` 响应继续包含渠道名称和健康时间线。

### Task 1: 修正 Token 导航和统计渠道列显示

**Files:**
- Modify: `web/assets/js/web-auth.js:10`
- Modify: `web/assets/js/web-auth.test.js:43-49`
- Modify: `web/assets/css/styles.css:5026-5033`
- Test: `internal/app/dashboard_scope_test.go:225-340`

**Interfaces:**
- Consumes: `WebAuth.filterNavigation(navKeys, role) string[]`。
- Consumes: `GET /dashboard/stats` 返回的 `[]model.StatsEntry`，其中 `channel_name` 和 `health_timeline` 已由服务端填充。
- Produces: Token 导航键集合 `['index', 'stats', 'trend', 'logs']`。

- [ ] **Step 1: 修改导航公开契约测试**

在 `web/assets/js/web-auth.test.js` 更新现有测试：

```javascript
test('navigation excludes administrative pages for API token role', () => {
  const navKeys = ['index', 'channels', 'tokens', 'stats', 'trend', 'logs', 'model-test', 'settings'];
  assert.deepEqual(WebAuth.filterNavigation(navKeys, 'api_token'), [
    'index', 'stats', 'trend', 'logs'
  ]);
  assert.deepEqual(WebAuth.filterNavigation(navKeys, 'admin'), navKeys);
});
```

- [ ] **Step 2: 运行导航测试确认旧白名单失败**

```bash
node --test web/assets/js/web-auth.test.js
```

Expected: FAIL；实际结果仍额外包含 `channels` 与 `model-test`。

- [ ] **Step 3: 收紧 Token 导航白名单**

在 `web/assets/js/web-auth.js` 修改常量：

```javascript
const API_TOKEN_NAV = new Set(['index', 'stats', 'trend', 'logs']);
```

不修改 `NAVS`、管理员分支、页面路由或直接 URL 访问逻辑。

- [ ] **Step 4: 扩展 stats 公开响应契约**

在 `internal/app/dashboard_scope_test.go` 的
`TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels` 中，保留现有渠道名称断言，并增加：

```go
for _, entry := range statsData.Stats {
	if entry.ChannelID == nil {
		t.Fatal("token stats entry missing channel id")
	}
	if entry.ChannelName == "" {
		t.Fatalf("token stats channel %d missing name", *entry.ChannelID)
	}
	if len(entry.HealthTimeline) != 48 {
		t.Fatalf("token stats channel %d health points=%d, want 48", *entry.ChannelID, len(entry.HealthTimeline))
	}
}
```

这是 handler/API 结构契约，不检查页面标记。现有后端正确时该断言应直接通过。

- [ ] **Step 5: 删除错误的 Token 统计列隐藏规则**

把 `web/assets/css/styles.css` 的角色隐藏规则收敛为：

```css
body.web-role-api-token .logs-col-ip,
body.web-role-api-token .logs-col-token-desc,
body.web-role-api-token .logs-col-api-key,
body.web-role-api-token .logs-col-channel {
  display: none !important;
}
```

只删除以下两个选择器：

```css
body.web-role-api-token .stats-table [data-column="channel_name"],
body.web-role-api-token .stats-col-channel
```

渠道名称和健康指示器共用 `.stats-col-channel`，删除隐藏规则后沿用管理员相同模板显示。

- [ ] **Step 6: 运行聚焦验证**

```bash
node --test web/assets/js/web-auth.test.js
go test -tags sonic ./internal/app -run '^TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels$' -count=1
```

Expected: 两个命令均 PASS，输出无 warning。

- [ ] **Step 7: 运行完整验证**

```bash
go test -tags sonic ./internal/...
make verify-web
golangci-lint run ./...
make build
git diff --check
```

Expected: Go 与 Web 测试全部 PASS，lint 为 `0 issues`，构建和 whitespace 检查成功。

- [ ] **Step 8: 页面验收**

- Token 登录：顶部导航只显示概览、调用统计、请求趋势、日志。
- Token 调用统计：渠道名称与 48 段健康时间线可见。
- 管理员登录：顶部导航与统计页保持原样。

- [ ] **Step 9: 提交**

```bash
git add web/assets/js/web-auth.js web/assets/js/web-auth.test.js web/assets/css/styles.css internal/app/dashboard_scope_test.go
git commit -m "fix: align token navigation and stats visibility"
```
