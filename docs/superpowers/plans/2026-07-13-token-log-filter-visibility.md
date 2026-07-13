# Token Log Filter Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** API Token 登录后显示日志页的渠道类型、渠道名和模型筛选器，同时继续隐藏无效的 Token 与日志来源筛选器。

**Architecture:** 删除通用页面启动器中的筛选器 ID 黑名单，让每个筛选组件管理自己的 Token 角色行为。Token 选择器隐藏自身；日志来源筛选器在 Token 会话中固定为代理日志并隐藏自身；渠道与模型筛选器继续使用已有 Token-scoped `/dashboard` 数据源。

**Tech Stack:** 原生 JavaScript、Gin、Go 1.25、Node `node:test`、Sonic build tag。

## Global Constraints

- 所有 Go 命令必须带 `-tags sonic`。
- API Token 查询继续由服务端 `ApplyWebIdentityScope` 强制绑定当前会话 Token ID。
- 不开放 `/admin/*` 接口，不暴露其他 Token、API Key、上游 URL 或客户端 IP。
- 不复制页面或筛选器实现。
- 按仓库测试策略，不为纯展示行为新增 `hidden`、CSS 或 DOM 结构断言。

---

### Task 1: 把 Token 筛选器可见性归还给组件

**Files:**
- Modify: `web/assets/js/ui.js:1093-1117`
- Modify: `web/assets/js/ui.js:1251-1277`
- Modify: `web/assets/js/logs.js:1264-1287`

**Interfaces:**
- Consumes: `window.isAPITokenRole() boolean`。
- Consumes: `window.initAuthTokenFilter(options) Promise<AuthToken[]>`。
- Produces: Token 日志页显示 `f_channel_type`、`f_name`、`f_model`；隐藏 `f_auth_token`、`f_log_source`。

- [ ] **Step 1: 删除通用页面启动器的筛选器黑名单**

从 `initPageBootstrap` 删除以下角色分支：

```javascript
if (window.isAPITokenRole()) {
  ['f_channel_type', 'f_id', 'f_name', 'f_auth_token', 'f_log_source'].forEach((id) => {
    const control = document.getElementById(id);
    const group = control && control.closest('.filter-group');
    if (group) group.hidden = true;
  });
}
```

通用启动器只保留会话、页面权限、翻译、顶部导航和页面 `run` 调度。

- [ ] **Step 2: Token 选择器管理自身隐藏状态**

在 `initAuthTokenFilter` 完成 `selectId` 校验后增加：

```javascript
const select = document.getElementById(selectId);
const group = select && select.closest('.filter-group');
if (window.isAPITokenRole()) {
  if (group) group.hidden = true;
  return [];
}
if (group) group.hidden = false;
```

管理员继续执行原有预加载或 `/admin/auth-tokens` 加载路径；Token 会话不加载也不提交可变 Token ID。

- [ ] **Step 3: 日志来源筛选器显式处理 Token 会话**

在 `syncLogSourceVisibility` 获得 `group` 和 `select` 后增加：

```javascript
if (window.isAPITokenRole()) {
  group.hidden = true;
  select.value = 'proxy';
  return false;
}
```

这样 Token 页面不再请求 `/admin/settings/channel_check_interval_hours`，管理员仍按现有设置决定是否显示日志来源。

- [ ] **Step 4: 运行格式与前端验证**

```bash
git diff --check
make verify-web
```

Expected: 两个命令退出码均为 0，Node 测试无失败。

- [ ] **Step 5: 验证 Token 作用域 API 契约**

```bash
go test -tags sonic ./internal/app -run '^TestDashboardModelsMetricsAndStatsExposeOnlyScopedChannels$' -count=1
go test -tags sonic ./internal/app -run '^TestDashboardLogsForceTokenScopeAndExposeSafeChannelFields$' -count=1
```

Expected: Token bootstrap/models/stats/metrics/logs 仍只返回当前登录 Token 的渠道与模型维度，测试全部 PASS。

- [ ] **Step 6: 运行完整验证**

```bash
go test -tags sonic ./internal/...
golangci-lint run ./...
make build
git diff --check
```

Expected: Go 测试全部 PASS，lint 输出 `0 issues`，构建与 whitespace 检查成功。

- [ ] **Step 7: 页面验收**

- API Token 登录日志页：渠道类型、渠道名、模型控件可见。
- API Token 登录日志页：Token 与日志来源控件隐藏。
- 管理员日志页：所有原有筛选器保持现状。
- 修改渠道类型后，渠道名与模型选项仍来自 `/dashboard/models` 的当前 Token 作用域数据。

- [ ] **Step 8: 提交实现**

```bash
git add web/assets/js/ui.js web/assets/js/logs.js
git commit -m "fix: show scoped log filters for token sessions"
```
