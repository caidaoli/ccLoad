# CLAUDE.md

ccLoad 是 Claude/OpenAI/Gemini/Codex 多协议 API 网关，负责渠道选择、故障切换、协议转换与成本计量。

## 工作方式

- 修改任务完成到实现、相关验证及本次引入问题的修复；常规本地操作按现有授权连续执行。仅审查、解释或诊断时保持只读。
- 按下表读取与当前改动有关的契约；小改动不要求通读全部参考文档。新增或修正规则写入对应专题，根文件只保留通用约束与入口。
- 发布使用仓库 `ccload-release`；CLIProxyAPI 核心及 provider adapter 的同步/同步审计使用 `sync-cliproxy-core`。阅读或编辑技能不等于执行其中的发布、同步操作。

## 命令与验证

Go 构建和测试必须带 `-tags sonic`（Makefile 已处理）。环境变量定义见 `.env`。

```bash
make build                    # 构建并注入版本
make dev                      # 开发运行
go test -tags sonic ./internal/app -run 'TestXxx' # 受影响测试
go test -tags sonic ./internal/...
make verify-web               # 前端验证，含 node:test
make race-fast                # 并发相关改动；全量使用 make race
golangci-lint run ./...        # Go 改动提交前零警告
```

- 迭代运行受影响包；Go 改动提交前运行全量 `./internal/...` 与 lint，前端改动运行 `make verify-web`。文档、文案和布局改动只做相关检查，不新增行为测试。
- 不用 `-count=1`，除非排查缓存、不稳定测试或测量性能。独立检查可并行；检查通过后，仅在新改动或未解决问题需要时重跑。
- 调整测试并行度时读 [测试并行化](.claude/agent-guide/testing.md)。发布和核心同步的专属验证命令见各自技能。

## 通用代码约束

- 用 `any`；配置错误 fail-fast；`defer cancel()` 无条件调用，取消监听用 `context.AfterFunc`。
- lint 启用 errcheck/govet/staticcheck/unused/revive/bodyclose，gosec 已禁。
- Web 单选下拉必须可搜索：统一由 `searchable-select.js` 增强并复用 `createSearchableCombobox`；原生 `<select>` 仅作隐藏的表单/业务值载体。保留动态选项、禁用态、`input/change`、表单提交、键盘/ARIA 与 `<dialog>` 顶层菜单；多选控件未提供可搜索实现前禁止新增。

## 任务入口

路径相对仓库根目录；下表省略目录的 Go 文件位于 `internal/app/`。只读取相关专题；上游文档按提供商分节。

| 任务 | 代码入口与契约 |
| --- | --- |
| 代理、SSE、重试、冷却、取消 | `proxy_handler.go:HandleProxyRequest` → `runProxyAttemptLoop` → `proxy_forward.go` / `proxy_stream.go`；[代理契约](.claude/agent-guide/proxy.md) |
| Responses WebSocket | `proxy_responses_websocket.go`、`responses_execution_session.go`；[代理契约](.claude/agent-guide/proxy.md)及 [Codex 契约](.claude/agent-guide/upstream.md#codex) |
| 渠道/Key/URL、模型后缀、多模态回退 | `selector*.go`、`key_selector.go`、`url_selector.go`；[路由契约](.claude/agent-guide/routing.md) |
| 上游认证、请求指纹、provider wire | 各提供商 `*_wire.go` / `*_credentials.go` / `internal/*auth/`；[上游契约](.claude/agent-guide/upstream.md) |
| Registry 与协议转换 | `internal/protocol/registry.go` → `builtin/register.go` → `builtin/cliproxy_adapter.go`；[转换边界](.claude/agent-guide/protocol.md)，快照来源见 `internal/protocol/cliproxy/UPSTREAM.md` |
| 计费、访问控制、OAuth 额度 | `internal/util/cost_calculator.go`、`internal/oauthcost/`、`oauth_quota_cost.go`；[计费契约](.claude/agent-guide/billing.md) |
| 系统设置、超时、连接、存储与迁移 | `config_service.go`、`internal/storage/`；[运行与存储契约](.claude/agent-guide/runtime-storage.md)；写库后按改动失效 `InvalidateChannelListCache` / `InvalidateAPIKeysCache` |
| Admin API、管理账户与定时任务 | `admin_types.go` → `admin_<feature>.go` → `server.go:SetupRoutes`；账户核心为 `channel_management_service.go`；[管理与定时任务](.claude/agent-guide/runtime-storage.md#渠道管理与定时检测) |
| 发布、容器与进程内更新 | [.agents/skills/ccload-release/SKILL.md](.agents/skills/ccload-release/SKILL.md)；[更新机制](.claude/agent-guide/runtime-storage.md#发布与更新) |
| 管理后台 / 独立介绍站 | `web/` 是管理后台；`www/` 是独立介绍站，`make www-setup` 复制共享资源后可独立部署 |
