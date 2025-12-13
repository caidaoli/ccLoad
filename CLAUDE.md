# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 环境要求

- **Go 版本**: 1.25+ (必须)
  - 使用 Go 1.21+ 特性: `context.AfterFunc` (零 goroutine 泄漏)
  - 构建标签: `-tags go_json` (sonic JSON 库)

## 快速命令

```bash
# 构建(必须使用 -tags go_json)
go build -tags go_json -o ccload .
go run -tags go_json .

# 测试(必须带 -tags go_json,否则JSON库不匹配导致失败)
go test -tags go_json ./internal/... -v
go test -tags go_json ./internal/app -run TestName -v  # 单个测试
go test -tags go_json -race ./internal/...             # 竞态检测

# 环境变量(优先使用.env文件)
export CCLOAD_PASS=test123  # 或在项目根目录创建.env文件
```

## 核心架构

```
internal/
├── app/               # HTTP层+业务逻辑
│   ├── selector.go    # 渠道选择(优先级+轮询)
│   ├── key_selector.go # 多Key策略(sequential/round_robin)
│   ├── proxy_*.go     # 代理模块(按职责SRP拆分)
│   └── admin_*.go     # 管理API(按功能SRP拆分)
├── cooldown/          # 冷却决策引擎(manager.go统一入口)
├── storage/
│   ├── sql/           # 数据持久层(SQLite/MySQL统一实现)
│   ├── schema/        # 表结构定义
│   └── migrate.go     # 启动时自动迁移
├── validator/         # 渠道验证器(subscription.go)
└── util/
    ├── classifier.go  # HTTP错误分类器
    └── models_fetcher.go # 模型列表适配器
```

**故障切换策略**(核心业务逻辑):
- Key级错误(401/403/429) → 重试同渠道其他Key
- 渠道级错误(5xx/520/524) → 切换到其他渠道
- 客户端错误(404/405) → 不重试,直接返回
- 指数退避: 2min → 4min → 8min → 30min(上限)

**关键入口函数**:
- `cooldown.Manager.HandleError()` - 执行上述策略的决策引擎
- `util.ClassifyHTTPStatus()` - 错误分类(区分Key/Channel/Client级)
- `app.KeySelector.SelectAvailableKey()` - 多Key负载均衡(sequential/round_robin)

## 开发指南

### Task 子代理使用策略

**优先使用子代理的场景**:
- 开放式代码探索(如"错误处理在哪里?") → `Explore` (medium/very thorough)
- 需要多步探索+分析的复杂任务 → `general-purpose` (安全审计、性能分析)
- 多个独立任务需并行执行 → **单条消息并行调用多个 Task**

**不应使用子代理的场景**:
- 已知具体文件路径 → 直接用 `Read` 工具
- 搜索特定类/函数定义 → 直接用 `mcp__serena__find_symbol`
- 按文件名查找 → 直接用 `Glob` 工具(如 `**/*selector*.go`)
- 在 2-3 个已知文件内搜索 → 直接用 `Read` + `Grep`
- 有依赖的任务链 → 子代理无法传递中间状态，必须串行执行

**并行调用原则**:
- 需同时分析 API 层和数据层 → 一条消息发起两个 `Task`，而非串行
- 并行条件：任务间无依赖、都需要大量上下文探索

### Serena MCP 工具使用规范

**代码浏览**:
- 优先用符号化工具: `mcp__serena__get_symbols_overview` → `mcp__serena__find_symbol`
- **禁止**直接读取整文件，先用 `get_symbols_overview` 了解结构
- 查找引用关系: `mcp__serena__find_referencing_symbols`

**代码编辑**:
- 替换整个符号: `mcp__serena__replace_symbol_body`(类/函数/方法)
- 插入新代码: `mcp__serena__insert_after_symbol` / `insert_before_symbol`
- **禁止**用正则替换编辑代码(用 `Edit` 工具处理符号外的小改动)



### Playwright MCP 工具策略

- 截图**必须**使用 JPEG 格式: `type: "jpeg"`(默认 quality=80，体积比 PNG 小 5-10 倍)
- 需要极致压缩时用 `browser_run_code`: `await page.screenshot({ type: 'jpeg', quality: 50, path: '...' })`
- 交互操作前优先用 `browser_snapshot`(文本格式，零体积)，视觉验证才截图
- **避免** `fullPage: true`，优先截取特定元素或可见区域


### 添加 Admin API 流程
1. `internal/app/admin_types.go` - 定义请求/响应类型
2. `internal/app/admin_<feature>.go` - 实现Handler函数
3. `internal/app/server.go:SetupRoutes()` - 注册路由
4. 使用 `internal/app/handlers.go` 的 `RespondJSON/RespondError/RespondErrorMsg` 统一响应


### 数据库操作规范

- Schema更新: `internal/storage/migrate.go` 启动时自动执行
- 事务封装: `internal/storage/sql/transaction.go` 的 `(*SQLStore).WithTransaction(ctx, func(tx *sql.Tx) error { ... })`（sql层内部使用）
- 缓存失效: `InvalidateChannelListCache()` / `InvalidateAPIKeysCache()` / `invalidateCooldownCache()`

## 代码规范

- **优先**使用最新版本Go语言特性(项目要求 Go 1.25+)
- **必须**使用 `any` 替代 `interface{}`
- **必须**在测试中添加 `-tags go_json`,否则JSON序列化不一致
- **禁止**过度工程(Factory工厂、"万一需要"的功能)
- **Fail-Fast**: 配置错误直接`log.Fatal()`退出,不要容错
- **API Key脱敏**: 日志写入时自动脱敏,无需手动处理(`internal/storage/sql/log.go`, `internal/storage/sql/helpers.go`)

### Context 管理规范 

- 所有 `context.WithCancel/WithTimeout` 必须无条件 `defer cancel()`
- 监听取消用 `context.AfterFunc`，禁止手动 `go func() { <-ctx.Done() }`
- 参考: `internal/app/request_context.go`
