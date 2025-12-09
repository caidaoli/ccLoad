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

**Task子代理**(适用场景):
- **Explore/Plan**: 开放性问题、需遍历多目录的代码探索
- **general-purpose**: 独立的复杂子任务(如"审查这个PR的安全性")
- **上下文隔离**: 需读取大量代码但只需结论时(子代理返回摘要,不污染主对话)
- **并行调用**: 多个**无依赖**的任务,单条消息发起多个 Task 调用

**不要用子代理**:
- 已知符号名/文件路径 → 直接用 Serena 工具
- 单文件操作 → 直接处理
- 有依赖关系的任务链 → 必须串行,子代理无法传递中间状态
- 简单搜索 → 直接调用 Grep/Glob

**判断标准**:
1. 任务复杂度是否足以抵消启动开销(~2-3秒)?
2. 是否需要上下文隔离(避免大量代码撑爆主对话)?
3. 多任务是否真正独立(无数据依赖)?

**显式触发**:
- "审查/review/分析安全性" → general-purpose 子代理
- "探索/了解/分析架构/怎么工作" → Explore 子代理
- "改/加/修/删/重构/实现" → 直接操作,不启动子代理

**使用Serena MCP工具**(必须遵守):
- 代码浏览用符号化工具(`mcp__serena__get_symbols_overview`, `mcp__serena__find_symbol`)
- **禁止**直接读取整个文件,先用`get_symbols_overview`了解结构
- 编辑代码用`mcp__serena__replace_symbol_body`,不用正则替换

**使用Playwright MCP工具**(必须遵守):
- 截图**必须**使用 JPEG 格式: `type: "jpeg"`(默认 quality=80,体积比 PNG 小 5-10 倍)
- 需要极致压缩时用 `browser_run_code`: `await page.screenshot({ type: 'jpeg', quality: 50, path: '...' })`
- 交互操作前优先用 `browser_snapshot`(文本格式,零体积),视觉验证才截图
- **避免** `fullPage: true`,优先截取特定元素或可见区域

**添加Admin API**:
1. `internal/app/admin_types.go` - 定义请求/响应类型
2. `internal/app/admin_<feature>.go` - 实现Handler函数
3. `internal/app/server.go:SetupRoutes()` - 注册路由
4. 使用 `s.handlers.Success/BadRequest/ServerError/NotFound` 统一响应

**数据库操作**:
- Schema更新: `internal/storage/sqlite/migrate.go`启动时自动执行
- 事务封装: `storage.ExecInTransaction(func(tx) error { ... })`保证原子性
- 缓存失效: 修改渠道后调用`s.InvalidateChannelListCache()`

## 代码规范

- **必须**使用 `any` 替代 `interface{}`
- **必须**在测试中添加 `-tags go_json`,否则JSON序列化不一致
- **禁止**过度工程(Factory工厂、"万一需要"的功能)
- **Fail-Fast**: 配置错误直接`log.Fatal()`退出,不要容错
- **API Key脱敏**: 日志自动清洗,无需手动处理(`util/log_sanitizer.go`)

