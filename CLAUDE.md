# CLAUDE.md

ccLoad 是 Claude/OpenAI/Gemini/Codex 多协议 API 网关，负责渠道选择、故障切换、协议转换与成本计量。

## 工作方式

- 修改任务完成到实现、相关验证及本次引入问题的修复；常规本地操作按现有授权连续执行。
- 按下表读取与当前改动有关的契约。新增或修正规则写入对应专题，根文件只保留通用约束与入口。
- 发布使用仓库 `ccload-release`；CLIProxyAPI 核心及 provider adapter 的同步/同步审计使用 `sync-cliproxy-core`。
- 上游参照源码优先用本机 checkout：CLIProxyAPI `~/Source/go/CLIProxyAPI`、sub2api `~/Source/go/sub2api`（`~/Source` 即 `~/Share/Source`）。checkout 不钉提交，对照时按专题文档或 `internal/protocol/cliproxy/UPSTREAM.md` 记录的提交用 `git -C <repo> show <commit>:<path>` 读，不读 HEAD；本机不存在时按记录的仓库与提交获取。

## 命令与验证

Go 构建和测试必须带 `-tags sonic`（Makefile 已处理）。环境变量契约见 `.env.example`（容器部署见 `.env.docker.example`）；本地 `.env` 被 gitignore，仅作个人覆盖。

```bash
make build                                         # 构建并注入版本
make dev                                           # 开发运行
go test -tags sonic ./internal/app -run 'TestXxx'  # 受影响测试
go test -tags sonic ./internal/...                 # 全量 Go 测试
make verify-web                                    # 前端验证，含 node:test
make race-fast                                     # 并发相关改动；全量用 make race
golangci-lint run ./...                            # Go 改动提交前零警告
```

- 迭代运行受影响包；Go 改动提交前跑全量测试与 lint，前端改动跑 `make verify-web`；独立检查可并行。
- 不用 `-count=1`，除非排查缓存、不稳定测试或测量性能。
- 调整测试并行度时读 [测试并行化](.claude/agent-guide/testing.md)。发布和核心同步的专属验证命令见各自技能。

## 通用代码约束

- 用 `any`；配置错误 fail-fast；`defer cancel()` 无条件调用，取消监听用 `context.AfterFunc`。
- lint 启用 errcheck/govet/staticcheck/unused/revive/bodyclose/modernize；gosec 已禁，revive `exported` 因与 Doxygen 注释冲突已禁，modernize 不检查 `internal/protocol/cliproxy/` 快照。
- JSON 重复 key 按 `encoding/json` 后者胜出，不加检测、拒绝或兼容兜底，审查也不作为问题；只有改写与渲染不一致时才就地收敛（见 `ordered_json.go`）。
- Web 单选下拉必须可搜索：统一由 `searchable-select.js` 增强并复用 `createSearchableCombobox`；原生 `<select>` 仅作隐藏的表单/业务值载体。保留动态选项、禁用态、`input/change`、表单提交、键盘/ARIA 与 `<dialog>` 顶层菜单；多选控件未提供可搜索实现前禁止新增。

### 注释

新增或修改的声明（类型、接口方法、常量、包级变量、函数/方法，含未导出辅助函数）紧贴写多行 Doxygen 块注释 `/** ... */`，不写单行 `/** */` 或 `//` 文档注释；结构体字段与常量组成员用行尾 `///<`，常量组注释写在 `const (` 上。存量代码不批量补，`internal/protocol/cliproxy/` 快照不加。

- **标签**：`@brief` 必填；`@param` 按序逐个说明（接收者除外），类型参数用 `@tparam`；每个返回值按序写一条 `@return`，说明 `nil`/零值/`false` 等特殊值，`error` 写触发条件及可 `errors.Is/As` 的哨兵或类型；会 panic 的写 `@throw panic` 及条件；调用限制与副作用（并发安全、持锁、`ctx` 取消、须 `Close`、改入参或共享状态、启动 goroutine）写 `@note`，设计细节写 `@details`；无内容不留空标签。
- **内容**：写业务契约与设计原因（单位、状态转换、错误边界），不复述代码或签名。

```go
/**
 * @brief 获取第三方平台的接口调用凭据
 * @param ctx 请求上下文；取消时中止刷新且不写缓存
 * @param user 当前登录用户及租户信息
 * @return 接口调用凭据，不含 Bearer 前缀
 * @return 刷新失败时返回 ErrTokenExpired
 * @note 并发安全；同租户并发刷新合并为一次上游请求
 */
func getAccessToken(ctx context.Context, user *Member) (string, error)

/**
 * @brief 已缓存的第三方平台凭据
 */
type credential struct {
	token     string    ///< 访问令牌，不含 Bearer 前缀
	expiresAt time.Time ///< 过期时刻（UTC），提前 60 秒视为过期
}
```

## 任务入口

路径相对仓库根目录；下表省略目录的 Go 文件位于 `internal/app/`。只读取相关专题；上游文档按提供商分节。

| 任务 | 代码入口与契约 |
| --- | --- |
| 代理、SSE、重试、冷却、取消 | `proxy_handler.go:HandleProxyRequest` → `runProxyAttemptLoop` → `proxy_forward.go` / `proxy_stream.go`；[代理契约](.claude/agent-guide/proxy.md) |
| 隐私脱敏与还原 | `internal/privacy/`、`privacy.go`、`privacy_output.go`；[代理契约](.claude/agent-guide/proxy.md#隐私脱敏) |
| Responses WebSocket | `proxy_responses_websocket.go`、`responses_execution_session.go`；[代理契约](.claude/agent-guide/proxy.md)及 [Codex 契约](.claude/agent-guide/upstream.md#codex) |
| 渠道/Key/URL、模型后缀、动态思考等级、多模态回退 | `selector*.go`、`key_selector.go`、`url_selector.go`、`reasoning_*.go`；[路由契约](.claude/agent-guide/routing.md) |
| 上游认证、请求指纹、provider wire | 各提供商 `*_wire.go` / `*_credentials.go` / `internal/*auth/`；[上游契约](.claude/agent-guide/upstream.md) |
| Registry 与协议转换 | `internal/protocol/registry.go` → `builtin/register.go` → `builtin/cliproxy_adapter.go`；[转换边界](.claude/agent-guide/protocol.md)，快照来源见 `internal/protocol/cliproxy/UPSTREAM.md` |
| 计费、访问控制、OAuth 额度 | `internal/util/cost_calculator.go`、`internal/oauthcost/`、`oauth_quota_cost.go`；[计费契约](.claude/agent-guide/billing.md) |
| 系统设置、超时、连接、存储与迁移 | `config_service.go`、`internal/storage/`；[运行与存储契约](.claude/agent-guide/runtime-storage.md)；写库后按改动失效 `InvalidateChannelListCache` / `InvalidateAPIKeysCache` |
| Admin API、管理账户与定时任务 | `admin_types.go` → `admin_<feature>.go` → `server.go:SetupRoutes`；账户核心为 `channel_management_service.go`；[管理与定时任务](.claude/agent-guide/runtime-storage.md#渠道管理与定时检测) |
| 发布、容器与进程内更新 | [.agents/skills/ccload-release/SKILL.md](.agents/skills/ccload-release/SKILL.md)；[更新机制](.claude/agent-guide/runtime-storage.md#发布与更新) |
| 管理后台 / 独立介绍站 | `web/` 是管理后台；`www/` 是独立介绍站，`make www-setup` 复制共享资源后可独立部署 |
## Agent skills

### Issue tracker

事项和规格使用本地 Markdown；创建、读取或更新事项前，读 `docs/agents/issue-tracker.md`。

### Triage labels

使用五个默认 triage 状态；分类事项前，读 `docs/agents/triage-labels.md`。

### Domain docs

采用 single-context 布局；探索业务代码前，按 `docs/agents/domain.md` 读取领域文档。
