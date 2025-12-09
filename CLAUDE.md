# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 快速命令

```bash
# 构建(必须使用 -tags go_json)
go build -tags go_json -o ccload .
go run -tags go_json .

# 测试(必须带 -tags go_json,否则JSON库不匹配导致失败)
go test -tags go_json ./internal/... -v
go test -tags go_json ./internal/app -run TestName -v  # 单个测试
go test -tags go_json -race ./internal/...             # 竞态检测

# 环境变量
export CCLOAD_PASS=test123  # 必填,否则程序启动失败
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
├── storage/sqlite/    # 数据持久层(冷却数据内联,migrate.go)
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

**必须使用子代理:**
- 代码探索/架构分析 → `Explore` (medium/very thorough)
- 独立复杂任务(如安全审计、性能分析) → `general-purpose`
- 需大量代码上下文但只要结论 → 避免污染主对话
- 多个无依赖任务 → **单条消息并行调用多个 Task**

**禁止使用子代理:**
- 简单文件读取(直接用 Read/Glob/Grep)
- 有依赖的任务链(子代理无法传递中间状态)
- 已有子代理运行时(检查 resume 参数复用)

**并行调用原则:**
需同时分析 API 层和数据层 → 一条消息发起两个 Task，而非串行

### Serena MCP 工具策略

**强制使用符号化工具:**
- 代码浏览 → `mcp__serena__get_symbols_overview`(先看结构)
- 精确定位 → `mcp__serena__find_symbol`(查找具体符号)
- 代码编辑 → `mcp__serena__replace_symbol_body`(替换函数体/类定义)

**严格禁止:**
- ❌ 直接 `Read` 整个文件(除非明确需要完整上下文)
- ❌ 用 `Edit` 工具做正则替换(易出错)
- ✅ 先 `get_symbols_overview` 了解文件结构，再针对性操作


### Playwright MCP 工具策略

- 截图**必须**使用 JPEG 格式: `type: "jpeg"`(默认 quality=80，体积比 PNG 小 5-10 倍)
- 需要极致压缩时用 `browser_run_code`: `await page.screenshot({ type: 'jpeg', quality: 50, path: '...' })`
- 交互操作前优先用 `browser_snapshot`(文本格式，零体积)，视觉验证才截图
- **避免** `fullPage: true`，优先截取特定元素或可见区域


### 添加 Admin API 流程
1. `internal/app/admin_types.go` - 定义请求/响应类型
2. `internal/app/admin_<feature>.go` - 实现Handler函数
3. `internal/app/server.go:SetupRoutes()` - 注册路由
4. 使用 `s.handlers.Success/BadRequest/ServerError/NotFound` 统一响应


### 数据库操作规范

- Schema更新: `internal/storage/sqlite/migrate.go`启动时自动执行
- 事务封装: `storage.ExecInTransaction(func(tx) error { ... })`保证原子性
- 缓存失效: 修改渠道后调用`s.InvalidateChannelListCache()`

## 代码规范

- **必须**使用 `any` 替代 `interface{}`
- **必须**在测试中添加 `-tags go_json`,否则JSON序列化不一致
- **禁止**过度工程(Factory工厂、"万一需要"的功能)
- **Fail-Fast**: 配置错误直接`log.Fatal()`退出,不要容错
- **API Key脱敏**: 日志自动清洗,无需手动处理(`util/log_sanitizer.go`)

