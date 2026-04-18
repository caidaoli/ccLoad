# 渠道自定义请求头/请求体规则（高级功能）设计

- 日期：2026-04-18
- 目标：在渠道编辑弹窗"取消"按钮左侧加"高级"按钮，打开二级模态，允许管理员为渠道配置请求头与 JSON 请求体的改写规则（移除 / 覆盖 / 追加）。

## 1. 范围与边界

### 1.1 生效粒度
- **渠道级全局规则**：规则对该渠道发出的所有请求生效，不区分模型。
- 与 `daily_cost_limit` / `scheduled_check` 同级。

### 1.2 动作矩阵

| 对象 | remove | override | append |
|---|---|---|---|
| HTTP Header | 删除指定 header | `Header.Set`（替换所有值） | `Header.Add`（追加一个值，多值头语义） |
| JSON Body | 按路径删除 key | 按路径设置值（不存在则创建） | **不支持**（JSON 中 append 语义模糊） |

### 1.3 认证头保护（硬约束）
不可变黑名单：`authorization`、`x-api-key`、`x-goog-api-key`（大小写不敏感）。任何针对这三个 key 的 header 规则一律静默忽略并通过 `slog.Warn` 记录，不阻断请求。

### 1.4 非 JSON body 静默跳过
触发跳过的条件任一满足：
- body 为空（`len(body) == 0`）
- `Content-Type` 不包含 `application/json`
- `sonic.Unmarshal(body, &obj)` 返回错误
- body 的 JSON 根不是对象或数组（即无法按路径寻址）

静默跳过 = 返回原 body，不写警告、不阻断。

### 1.5 容量上限
- 单渠道 header 规则 ≤ 32 条
- 单渠道 body 规则 ≤ 32 条
- 单条 value（JSON 字面量字符串）≤ 8 KB
- 违反时 admin API 返回 400

## 2. 数据模型

### 2.1 Go 结构体（`internal/model/config.go`）

```go
type CustomHeaderRule struct {
    Action string `json:"action"` // "remove" | "override" | "append"
    Name   string `json:"name"`   // header 名（去空白、原样保留大小写）
    Value  string `json:"value"`  // remove 时忽略，其余必填
}

type CustomBodyRule struct {
    Action string          `json:"action"` // "remove" | "override"
    Path   string          `json:"path"`   // 点分路径，数组索引用数字
    Value  json.RawMessage `json:"value"`  // remove 时忽略，其余为任意 JSON 字面量
}

type CustomRequestRules struct {
    Headers []CustomHeaderRule `json:"headers,omitempty"`
    Body    []CustomBodyRule   `json:"body,omitempty"`
}

type Config struct {
    // ...既有字段...
    CustomRequestRules *CustomRequestRules `json:"custom_request_rules,omitempty"`
}
```

- `CustomRequestRules` 为指针：为 `nil` 表示"没有任何规则"，避免空对象混淆。
- `Value` 用 `json.RawMessage` 保留原始 JSON 结构，不做二次解析。

### 2.2 存储列（channels 表）

- 新列：`custom_request_rules TEXT`
- SQLite/MySQL 均存 JSON 字符串（`null` 或省略即空）
- 读写在 `internal/storage/sql/config.go` 中 Marshal/Unmarshal

### 2.3 迁移

- `internal/storage/migrate.go` 增量添加列：
  - SQLite：`ALTER TABLE channels ADD COLUMN custom_request_rules TEXT`
  - MySQL：`ALTER TABLE channels ADD COLUMN custom_request_rules TEXT`
- 列默认为 NULL，对既有渠道不产生行为变化

## 3. JSON 路径语法

子集约定，足够覆盖主流 AI 协议字段：
- 点分：`thinking.budget_tokens`、`generation_config.temperature`
- 数组索引（整数下标）：`messages.0.role`、`contents.1.parts.0.text`
- 不支持：通配符、负索引、引号转义、嵌套路径键含点

### 3.1 路径应用规则

**override**：
- 中间节点类型不匹配（期望 object 却是 array 等）→ 丢弃该规则并 `slog.Warn`
- 中间节点不存在 → 自动创建 object（整数段若位于新建链尾也按 object 处理；纯数组创建场景不支持，丢弃该规则）
- 叶节点存在 → 替换
- 叶节点不存在 → 创建

**remove**：
- 路径不存在 → 静默忽略
- 路径存在 → 删除对应 key 或数组元素（数组删除后元素前移）

## 4. 后端实现

### 4.1 新文件 `internal/app/custom_rules.go`

```go
var authHeaderBlacklist = map[string]struct{}{
    "authorization":  {},
    "x-api-key":      {},
    "x-goog-api-key": {},
}

// applyHeaderRules 按顺序应用 header 规则，认证头黑名单强制跳过。
func applyHeaderRules(h http.Header, rules []model.CustomHeaderRule) { ... }

// applyBodyRules 尝试解析为 JSON 并应用 body 规则，失败时返回原 body。
func applyBodyRules(contentType string, body []byte, rules []model.CustomBodyRule) []byte { ... }

// setJSONPath / removeJSONPath / splitJSONPath 为工具函数
```

### 4.2 接入点 `internal/app/proxy_forward.go:buildProxyRequest`

```go
// 1.5.1 应用自定义 body 规则（JSON 失败静默跳过）
body = applyBodyRules(hdr.Get("Content-Type"), body, cfg.BodyRules())

// 2. 创建带上下文的请求
req, err := buildUpstreamRequest(reqCtx.ctx, method, upstreamURL, body)
// ... 既有 3~5 步 ...

// 6. 应用自定义 header 规则（最后生效，认证头黑名单保护）
applyHeaderRules(req.Header, cfg.HeaderRules())
```

- `cfg.HeaderRules()` / `cfg.BodyRules()` 为 nil-safe 访问器，返回空切片表示无规则。

### 4.3 Admin API 校验（`internal/app/admin_types.go` / `admin_channels.go`）

- 接收字段：`custom_request_rules`（可选、可为 null）
- 校验项：
  - 条数上限（各 ≤ 32）
  - `action` 合法性（header：`remove|override|append`；body：`remove|override`）
  - header `name` 非空、不含 `\r\n`、长度 ≤ 256
  - body `path` 非空、长度 ≤ 256、仅允许 `[A-Za-z0-9_.\-]` 与数字（不允许 `[`/`]`）
  - override/append 的 value 必须存在且长度 ≤ 8 KB
  - header remove 的 value 留空（有值时静默忽略，不报错）
- 校验失败返回 400，错误文案带具体规则索引

### 4.4 缓存失效

- 渠道更新后 `InvalidateChannelListCache()` 已有，无需额外处理
- `Config.CustomRequestRules` 通过 `SELECT channels` 随整行读取，天然随缓存刷新

## 5. 前端

### 5.1 HTML 结构（`web/channels.html`）

编辑弹窗 footer 操作区改为三按钮：

```html
<div class="channel-editor-footer-actions">
  <button type="button" class="btn btn-secondary" data-action="open-custom-rules-modal"
          data-i18n="channels.customRules.advanced">高级</button>
  <button type="button" class="btn btn-secondary" data-action="close-channel-modal"
          data-i18n="common.cancel">取消</button>
  <button type="submit" id="channelSaveBtn" class="btn btn-primary"
          data-i18n="common.save">保存</button>
</div>
```

新增二级模态 `customRulesModal`（与现有协议转换模态同级）：

```
┌ 自定义请求规则 ─────────────────────────┐
│  [请求头 (2) (?)]  [请求参数 (0) (?)]   │ ← Tab
├─────────────────────────────────────────┤
│  规则列表（按当前 Tab 显示）             │
│  ┌────────────────────────────────────┐ │
│  │ [覆盖▾] [X-Foo______] [bar___] [✕] │ │
│  │ [追加▾] [Accept_____] [xml___] [✕] │ │
│  └────────────────────────────────────┘ │
│  [+ 添加规则]                            │
├─────────────────────────────────────────┤
│             [取消]  [确定]               │
└─────────────────────────────────────────┘
```

- Tab 计数器显示当前规则条数
- Tab 标签右侧的 `(?)` 为 inline SVG，hover 有气泡提示、click 弹出说明模态（2.0 版本可直接用 tooltip）

### 5.2 帮助气泡内容（zh-CN）

**请求头**：
```
用于在发送给上游前改写 HTTP 请求头。

支持三种动作：
 • 移除 (remove)：删除指定 Header
 • 覆盖 (override)：设置或替换 Header 的值
 • 追加 (append)：对多值 Header 追加一个值

示例：
 1. 移除 User-Agent →  动作=移除, 名称=User-Agent
 2. 强制指定 API 版本 → 动作=覆盖, 名称=X-Api-Version, 值=2025-08-07
 3. 追加 Accept 类型 → 动作=追加, 名称=Accept, 值=application/xml

注意：Authorization / x-api-key / x-goog-api-key 为认证头，不可改写。
```

**请求参数**：
```
用于改写发送给上游的 JSON 请求体字段（仅对 JSON body 生效，二进制/表单请求自动跳过）。

支持两种动作：
 • 移除 (remove)：按路径删除字段
 • 覆盖 (override)：按路径设置值（不存在则创建）

路径语法：点分路径 + 数字数组索引
 • 顶层字段：temperature
 • 嵌套字段：thinking.budget_tokens
 • 数组元素：messages.0.role

值支持任意 JSON 字面量：
 • 数字：0.7
 • 布尔：true
 • 字符串：必须带引号 "claude-opus-4-5"
 • 对象：{"type":"adaptive"}
 • 数组：["a","b"]

示例：
 1. 强制开启自适应思考 → 动作=覆盖, 路径=thinking, 值={"type":"adaptive"}
 2. 限制 max_tokens →  动作=覆盖, 路径=max_tokens, 值=4096
 3. 移除 stop_sequences → 动作=移除, 路径=stop_sequences
```

英文版同义翻译。

### 5.3 JS 模块（`web/assets/js/channels-custom-rules.js`）

导出函数：
- `openCustomRulesModal()` / `closeCustomRulesModal()`
- `resetCustomRulesState(rules)` — editChannel 时回填
- `collectCustomRulesFromForm()` — saveChannel 时提交
- `renderRuleList(tab)` / `addRule(tab)` / `deleteRule(tab, index)`
- `validateRulesLocally(rules)` — 保存前本地校验，返回错误列表

全局状态：`window.channelCustomRulesState = { headers: [...], body: [...] }`

### 5.4 集成点修改（`channels-modals.js`）

- `editChannel`：`resetCustomRulesState(channel.custom_request_rules)`
- `saveChannel` 的 `formData`：`custom_request_rules: collectCustomRulesFromForm()`
- 新渠道表单重置时清空规则

### 5.5 样式（`web/assets/css/channels.css`）

- `.custom-rules-modal` / `.custom-rules-tabs` / `.custom-rules-tab-button` / `.custom-rules-list` / `.custom-rules-row`
- 帮助气泡：`.custom-rules-help-icon` + `.custom-rules-help-popup`（CSS-only hover 方案即可，click 下集备）

### 5.6 本地化（`locales/{zh-CN,en}.js`）

新增命名空间 `channels.customRules.*`：`advanced`、`modalTitle`、`tabHeaders`、`tabBody`、`actionRemove`、`actionOverride`、`actionAppend`、`addRule`、`deleteRule`、`placeholderHeaderName`、`placeholderHeaderValue`、`placeholderPath`、`placeholderValue`、`helpHeaders`、`helpBody`、校验错误文案。

## 6. 测试

### 6.1 后端单测 `internal/app/custom_rules_test.go`

- `TestApplyHeaderRules_Override / Remove / Append`
- `TestApplyHeaderRules_SkipAuthBlacklist`：override `Authorization` 应不生效
- `TestApplyBodyRules_NonJSON_Passthrough`：二进制 body 原样返回
- `TestApplyBodyRules_InvalidJSON_Passthrough`
- `TestApplyBodyRules_OverrideNested`：`thinking.budget_tokens=8192`
- `TestApplyBodyRules_OverrideObjectValue`：`thinking={"type":"adaptive"}`
- `TestApplyBodyRules_RemoveNonExistent_NoOp`
- `TestApplyBodyRules_ArrayIndex`：`messages.0.role=system`

### 6.2 前端单测 `web/assets/js/channels-custom-rules.test.js`（node:test）

- 规则添加/删除/回填
- 校验：条数上限、非法 action、header name 含 CRLF、JSON 值解析失败
- Tab 切换计数

### 6.3 集成验证（手动）

- `make build` + `make web-test`
- `go test -tags go_json -race ./internal/app/... ./internal/model/... ./internal/storage/...`
- `golangci-lint run ./...`

## 7. 安全考量

- **认证头保护**：强制黑名单兜底，即便前端校验被绕过后端也会忽略
- **CRLF 注入**：header name/value 禁止 `\r\n`，防御 HTTP 请求分裂
- **容量上限**：条数 + value 长度双上限，防止内存放大
- **日志**：违反黑名单 / 路径类型冲突时 `slog.Warn`（含渠道 ID、规则索引），不泄漏 value
- **导出/导入**：CSV 导入导出里 `custom_request_rules` 作为 JSON 字符串列处理（现有 CSV 逻辑已支持 JSON 列）

## 8. 与既有机制的关系

- **anyrouter adaptive thinking 注入**：位于 `custom_request_rules` 之前，若用户通过规则显式设 `thinking`，现有 `maybeInjectAnyrouterAdaptiveThinking` 会检测到 `thinking` 已存在而不覆盖（现有逻辑已支持）
- **anyrouter beta flag**：`injectAnthropicBetaFlag` 在 header 规则**之前**执行，规则无法删除它（因为它不在黑名单内但用户显式要求覆盖也不合理；**权衡**：保持规则最后生效更符合用户心智，允许用户覆盖 beta flag，但不允许碰认证头）
- **协议转换**：协议转换在请求/响应的内容翻译层，与 header/body 改写正交，二者共存

## 9. 回滚

- 字段/列均为可选；删除前端 UI 不会导致后端异常（列为空 JSON）
- 必要时可 `ALTER TABLE channels DROP COLUMN custom_request_rules`（SQLite 需建表迁移）

## 10. 里程碑

1. schema / model / storage —— 最底层，其他改动依赖
2. custom_rules.go 与接入点 —— 核心逻辑
3. admin API 校验 —— 对外接口
4. 前端 HTML / JS / CSS / i18n —— UI
5. 单测 + make web-test + golangci-lint —— 验证
6. commit & push
