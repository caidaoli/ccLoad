# 实施计划：渠道自定义请求头/请求体规则

设计参考：`docs/superpowers/specs/2026-04-18-channel-custom-request-rules-design.md`

## 阶段 1：后端数据层

### T1.1 model.Config 增加字段
**文件**：`internal/model/config.go`
**变更**：
- 新增 `CustomHeaderRule`、`CustomBodyRule`、`CustomRequestRules` 三个结构体
- `Config` 增加 `CustomRequestRules *CustomRequestRules` 字段（带 `json:"custom_request_rules,omitempty"`）
- 新增方法 `(c *Config) HeaderRules() []CustomHeaderRule` / `BodyRules() []CustomBodyRule`，nil-safe

### T1.2 Schema 新列 & 迁移
**文件**：
- `internal/storage/schema/tables.go` — channels 表加 `Column("custom_request_rules TEXT NULL")`
- `internal/storage/migrate.go` — SQLite/MySQL 增量 `ALTER TABLE channels ADD COLUMN custom_request_rules TEXT`（不存在时才加）

### T1.3 Storage 读写
**文件**：`internal/storage/sql/config.go`
**变更**：
- INSERT / UPDATE SQL 加列与占位符
- 读取行时 `sql.NullString` → `json.Unmarshal` 到 `*CustomRequestRules`（空/NULL 则 nil）
- 写入时 `json.Marshal`，nil 或空规则写 NULL

**验证**：`go test -tags go_json ./internal/storage/... -run TestConfig -race`

## 阶段 2：后端核心逻辑

### T2.1 custom_rules.go 实现
**新文件**：`internal/app/custom_rules.go`
**要求**：
- `applyHeaderRules(h http.Header, rules []model.CustomHeaderRule)` — 认证头黑名单保护 + 按顺序 remove/override/append
- `applyBodyRules(contentType string, body []byte, rules []model.CustomBodyRule) []byte` — 非 JSON 跳过 + 按顺序 remove/override
- `setJSONPath(root any, segs []string, value json.RawMessage) (any, error)` — 递归设置路径
- `removeJSONPath(root any, segs []string) any` — 递归删除路径
- `splitJSONPath(p string) []string` — 点分切分
- 所有 warn 日志用 `slog.Warn` 且不记 value（仅记 channel id / rule index / reason）

### T2.2 custom_rules_test.go
**新文件**：`internal/app/custom_rules_test.go`
**覆盖**：设计文档 6.1 节全部用例

**验证**：`go test -tags go_json ./internal/app/... -run TestApply -race -v`

### T2.3 接入 buildProxyRequest
**文件**：`internal/app/proxy_forward.go`
**变更**（`buildProxyRequest` 函数，~L64-99）：
- 在 `maybeInjectAnyrouterAdaptiveThinking` 之后、`buildUpstreamRequest` 之前加：
  ```go
  body = applyBodyRules(hdr.Get("Content-Type"), body, cfg.BodyRules())
  ```
- 在 `injectAnthropicBetaFlag` 之后追加：
  ```go
  applyHeaderRules(req.Header, cfg.HeaderRules())
  ```

**验证**：`go build -tags go_json ./...`

## 阶段 3：Admin API

### T3.1 admin_types.go 校验
**文件**：`internal/app/admin_types.go`
**变更**：
- `ChannelUpsertRequest`（若存在，或相应类型）新增 `CustomRequestRules *model.CustomRequestRules`
- 新函数 `validateCustomRequestRules(r *model.CustomRequestRules) error`，覆盖设计 4.3 所有检查

### T3.2 admin_channels.go 持久化
**文件**：`internal/app/admin_channels.go`
**变更**：
- 创建/更新渠道路径读取新字段并调用校验
- 将校验后的规则赋给 `Config.CustomRequestRules`

**验证**：`go test -tags go_json ./internal/app/... -race`

## 阶段 4：前端

### T4.1 HTML 结构
**文件**：`web/channels.html`
**变更**：
- footer 操作区前置"高级"按钮（`data-action="open-custom-rules-modal"`）
- 新增 `<div id="customRulesModal" class="modal">` 含 Tab / 规则列表 / 确定/取消

### T4.2 JS 模块
**新文件**：`web/assets/js/channels-custom-rules.js`
**导出**：
- `openCustomRulesModal()` / `closeCustomRulesModal()`
- `resetCustomRulesState(rules)` / `collectCustomRulesFromForm()`
- `validateRulesLocally(rules)` — 同步后端校验
- 内部 `renderRuleList` / `addRule` / `deleteRule` / `switchTab` / `showHelpPopup`

**集成**（`web/assets/js/channels-modals.js`）：
- `editChannel` 中 `resetCustomRulesState(channel.custom_request_rules)`
- `saveChannel` 的 `formData` 加 `custom_request_rules: collectCustomRulesFromForm()`
- 新渠道重置表单时清空规则
- `handleEvent` switch 新增 `open-custom-rules-modal` 分支

### T4.3 样式
**文件**：`web/assets/css/channels.css`
**变更**：`.custom-rules-modal` / `.custom-rules-tabs` / `.custom-rules-list` / `.custom-rules-row` / `.custom-rules-help-icon` / `.custom-rules-help-popup`

### T4.4 本地化
**文件**：`web/assets/locales/zh-CN.js`、`web/assets/locales/en.js`
**变更**：新增 `channels.customRules.*` 命名空间（设计 5.6 列表）

### T4.5 `web/index.html` / 各页面引入新 JS
**文件**：`web/channels.html`
**变更**：在现有 `<script>` 列表中追加 `channels-custom-rules.js`

### T4.6 前端单测
**新文件**：`web/assets/js/channels-custom-rules.test.js`
**覆盖**：设计 6.2

**验证**：`make web-test`

## 阶段 5：最终验证

### T5.1 构建与 lint
- `make build` — 注入版本号后可正常启动
- `golangci-lint run ./...` — 零警告
- `go test -tags go_json -race ./internal/...`
- `make web-test`

### T5.2 手动冒烟（可选，若起服务）
- 启动 `make dev`，编辑任意渠道 → 点击"高级" → 配置一条 header override / body override → 保存 → 发一条请求观察上游 headers / body（如有测试账号）

### T5.3 提交 & 完结
- 单次 commit，消息格式：`feat: 渠道编辑新增高级面板支持自定义请求头与 JSON 参数规则`
- 调用 superpowers:finishing-a-development-branch 收尾
