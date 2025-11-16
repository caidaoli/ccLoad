# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

AI API 透明代理服务(Claude/Codex/Gemini): 智能路由、故障切换、多Key负载均衡

## 快速命令

```bash
# 构建(必须使用 -tags go_json)
make build          # 或: go build -tags go_json -o ccload .
make dev            # 开发模式运行

# 测试
go test ./internal/... -v              # 单元测试
go test ./test/integration/... -v     # 集成测试

# macOS服务管理
make install-service  # 安装系统服务
make status          # 查看状态
make logs            # 查看日志
```

## 必需配置

```bash
export CCLOAD_PASS=your_admin_password  # 必填,否则程序退出
```

**重要**: API访问令牌通过Web界面(`/web/tokens.html`)配置,不再使用环境变量

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

### 关键算法

**故障切换策略**:
- Key级错误(401/403/429) → 重试同渠道其他Key
- 渠道级错误(5xx/520/524) → 切换到其他渠道
- 客户端错误(404/405) → 不重试,直接返回
- 指数退避: 2min → 4min → 8min → 30min(上限)

**核心入口**:
- `cooldown.DecideChannelAction()` - 冷却决策
- `cooldown.ApplyCooldownForError()` - 执行冷却

## 开发模式

**优先使用 Serena MCP 工具**:
- 代码浏览使用符号化工具(`mcp__serena__*`)而非直接读取整个文件
- 避免不必要的全文件读取,使用 `get_symbols_overview` 和 `find_symbol`
- 编辑代码使用符号化编辑(`replace_symbol_body`)而非正则替换

**添加Admin API**:
1. `internal/app/admin_types.go` - 定义类型
2. `internal/app/admin_<feature>.go` - 实现Handler
3. `internal/app/server.go:setupRoutes` - 注册路由
4. 使用 `s.handlers.Success/BadRequest/...` 统一响应

## 代码规范

- 使用 `any` 替代 `interface{}`
- 遵循 KISS/DRY/SOLID 原则
- Fail-Fast: 配置错误立即退出
- API Key脱敏: `util/log_sanitizer.go`
