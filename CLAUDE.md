# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目简介

ccLoad 是一个高性能的 Claude Code & Codex & Gemini API 透明代理服务,使用 Go 1.25.0 和 Gin 框架构建。

**核心价值**: 智能路由、故障切换、多Key负载均衡、本地Token计数

**完整文档**: 详见 [README.md](README.md)

## 快速开始

### 开发运行
```bash
# 开发模式(热重载)
make dev                              # 或 go run -tags go_json .

# 构建二进制
make build                            # 或 go build -tags go_json -o ccload .

# macOS服务管理
make install-service                  # 安装LaunchAgent并启动
make status                           # 查看服务状态
make logs                             # 实时查看日志
make uninstall-service                # 卸载服务
```

### 测试
```bash
# 单元测试
go test ./internal/...  -v            # 所有单元测试
go test ./internal/app/proxy_*_test.go -v -run TestProxyHandler  # 特定测试

# 集成测试(需要启动服务)
go test ./test/integration/... -v

# 基准测试
go test ./internal/util/... -bench=. -benchmem

# 代码质量
go fmt ./... && go vet ./...
```

### 环境配置

必需环境变量(否则程序退出):
```bash
export CCLOAD_PASS=your_admin_password    # 管理界面密码(必填)
export CCLOAD_AUTH=token1,token2          # API访问令牌(访问/v1/*必填)
```

可选配置:
```bash
export PORT=8080                          # 服务端口
export SQLITE_PATH=./data/ccload.db       # 数据库路径
export REDIS_URL=rediss://...             # Redis备份(可选)
export CCLOAD_MAX_CONCURRENCY=1000        # 最大并发数
export CCLOAD_MAX_KEY_RETRIES=3           # Key重试次数
```

## 代码架构

### 目录结构(按职责分层)

```
internal/
├── app/                 # HTTP层 + 业务逻辑层
│   ├── server.go        # 服务器初始化、路由、认证中间件
│   ├── handlers.go      # 通用HTTP工具(参数解析、响应助手)
│   ├── request_context.go  # 请求上下文封装
│   │
│   ├── admin_*.go       # 管理API(按功能拆分SRP)
│   │   ├── admin_channels.go  # 渠道CRUD
│   │   ├── admin_models.go    # 模型列表获取(新增2025-11)
│   │   ├── admin_stats.go     # 统计分析
│   │   ├── admin_cooldown.go  # 冷却管理
│   │   ├── admin_csv.go       # CSV导入导出
│   │   └── admin_types.go     # 类型定义
│   │
│   ├── proxy_*.go       # 代理模块(按职责拆分SRP)
│   │   ├── proxy_handler.go   # HTTP入口、并发控制
│   │   ├── proxy_forward.go   # 核心转发、请求构建
│   │   ├── proxy_error.go     # 错误处理、重试逻辑
│   │   ├── proxy_stream.go    # 流式响应、首字节检测
│   │   ├── proxy_gemini.go    # Gemini API特殊处理
│   │   └── proxy_util.go      # 常量、类型、工具
│   │
│   ├── selector.go      # 渠道选择算法(优先级+轮询)
│   ├── key_selector.go  # 多Key管理(策略+冷却)
│   └── token_counter.go # 本地Token计数(符合官方规范)
│
├── model/               # 数据模型层(纯数据结构)
│   ├── config.go        # Config, APIKey
│   ├── log.go           # LogEntry
│   └── stats.go         # StatsEntry, MetricPoint
│
├── storage/             # 数据持久层
│   ├── store.go         # Store接口定义(抽象)
│   ├── cache.go         # 内存缓存(自定义实现)
│   ├── sqlite/          # SQLite实现
│   │   ├── store_impl.go    # Store接口实现
│   │   ├── migrate.go       # 数据库迁移、索引优化
│   │   ├── query.go         # SQL查询构建(防注入)
│   │   └── transaction.go   # 事务封装
│   └── redis/           # Redis同步
│       └── sync.go          # 异步渠道备份
│
├── cooldown/            # 冷却管理层(独立模块)
│   └── manager.go       # 统一冷却决策引擎(DRY原则)
│
├── service/             # 服务层
│   ├── auth_service.go  # 认证服务(Token生成/验证)
│   ├── log_service.go   # 日志服务(异步批量写入)
│   └── key_selector.go  # Key选择服务(策略执行)
│
├── util/                # 工具层(无状态函数)
│   ├── classifier.go    # HTTP错误分类器(Key级/渠道级/客户端)
│   ├── time.go          # 时间转换、冷却计算
│   ├── channel_types.go # 渠道类型管理(anthropic/codex/gemini)
│   ├── models_fetcher.go # 模型列表获取适配器(新增2025-11)
│   ├── apikeys.go       # API Key解析、验证
│   ├── serialize.go     # JSON序列化(Sonic/标准库切换)
│   ├── log_sanitizer.go # 日志脱敏(API Key、敏感信息)
│   └── rate_limiter.go  # 速率限制器
│
├── config/              # 配置层
│   └── defaults.go      # 默认配置常量(HTTP、SQLite、日志)
│
└── testutil/            # 测试工具
    ├── types.go         # 测试类型定义
    ├── api_tester.go    # HTTP API测试助手
    └── goroutine_leak.go # 协程泄漏检测
```

### 关键数据结构

**Config(渠道配置)** - `internal/model/config.go`:
```go
type Config struct {
    ID                 int64
    Name               string            // UNIQUE约束
    URL                string
    Priority           int               // 路由优先级
    Models             []string          // 支持的模型列表
    ModelRedirects     map[string]string // 模型重定向
    ChannelType        string            // anthropic/codex/gemini
    Enabled            bool
    CooldownUntil      int64             // 冷却截止时间(内联)
    CooldownDurationMs int64             // 冷却持续时间(内联)
}
```

**APIKey(API密钥)** - `internal/model/config.go`:
```go
type APIKey struct {
    ID                 int64
    ChannelID          int64
    KeyIndex           int
    APIKey             string
    KeyStrategy        string  // sequential/round_robin
    CooldownUntil      int64   // Key级冷却(内联)
    CooldownDurationMs int64
}
```

### 核心算法流程

详细的故障切换机制、指数退避策略请参考:
- `internal/app/selector.go` - 渠道选择算法
- `internal/app/key_selector.go` - 多Key策略
- `internal/app/proxy_error.go` - 错误分类与重试逻辑
- `internal/cooldown/manager.go` - 冷却决策引擎
- `internal/util/classifier.go` - HTTP错误分类器

**关键点**:
- Key级错误(401/403/429) → 重试同渠道其他Key
- 渠道级错误(5xx/520/524) → 切换到其他渠道
- 客户端错误(404/405) → 不重试,直接返回
- 指数退避: 2min → 4min → 8min → 30min(上限)

### 数据库架构

**核心表** (`internal/storage/sqlite/migrate.go`):
- `channels`: 渠道配置(冷却数据内联,UNIQUE约束name)
- `api_keys`: API密钥(Key级冷却内联,UNIQUE(channel_id, key_index))
- `key_rr`: 轮询指针(channel_id → idx)
- `logs`: 请求日志

**性能优化** (2025-10月):
- 冷却数据内联(废弃独立cooldowns表,↓JOIN开销)
- 索引优化: `idx_api_keys_channel_id`(↓40-60%延迟)
- 外键约束(级联删除,保证一致性)

## 开发实践指南

### 模型列表获取功能(新增2025-11)

**功能说明**: 支持从渠道API自动获取可用模型列表,简化模型配置流程

**API endpoint**: `GET /admin/channels/:id/models/fetch`

**支持的渠道类型**:
- **Anthropic**: 调用`/v1/models`接口,实时获取(使用x-api-key + anthropic-version请求头)
- **OpenAI**: 调用`/v1/models`接口,实时获取
- **Codex**: 调用`/v1/models`接口,实时获取(复用OpenAI实现)
- **Gemini**: 调用`/v1beta/models`接口,实时获取

**前端使用** (`/web/channels.html:276-298`):
```javascript
// 获取模型列表按钮
await fetchModelsFromAPI()  // 自动合并现有模型

// 清除所有模型按钮
clearAllModels()
```

**设计模式**:
- **适配器模式**: 统一不同渠道的Models API接口
- **策略模式**: 根据渠道类型选择获取策略
- **工厂模式**: `NewModelsFetcher(channelType)`动态创建

**相关文件**:
- `internal/util/models_fetcher.go` - 核心适配器实现
- `internal/app/admin_models.go` - Admin API Handler
- `web/channels.html:2524-2597` - 前端JavaScript

### 添加新的Admin API端点

1. **定义请求/响应类型** (`internal/app/admin_types.go`):
```go
type MyFeatureRequest struct {
    Field1 string `json:"field1" binding:"required"`
    Field2 int    `json:"field2"`
}
```

2. **实现Handler** (新建`internal/app/admin_myfeature.go`):
```go
func (s *Server) HandleMyFeature(c *gin.Context) {
    var req MyFeatureRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        s.handlers.BadRequest(c, err.Error())
        return
    }

    // 业务逻辑...
    result := processFeature(req)

    s.handlers.Success(c, result)
}
```

3. **注册路由** (`internal/app/server.go:setupRoutes`):
```go
adminAPI.POST("/myfeature", s.HandleMyFeature)
```

### 使用统一响应系统

**通过`Server.handlers`访问ResponseHelper** (`internal/app/handlers.go`):

```go
// 成功响应(200)
s.handlers.Success(c, data)

// 错误响应
s.handlers.BadRequest(c, "invalid parameter")       // 400
s.handlers.Unauthorized(c, "token expired")         // 401
s.handlers.NotFound(c, "channel not found")         // 404
s.handlers.InternalError(c, "database error")       // 500

// 自定义状态码
s.handlers.ErrorWithCode(c, 429, "rate limited", "RATE_LIMIT")
```

**响应格式** (自动生成):
```json
{
    "success": true|false,
    "data": {...},        // 仅成功时
    "error": "message",   // 仅失败时
    "code": "ERROR_CODE"  // 可选,机器可读错误码
}
```

### 调用冷却管理器

**统一入口** (`internal/cooldown/manager.go`):

```go
import "ccload/internal/cooldown"

// 标准冷却决策
action := cooldown.DecideChannelAction(
    statusCode,   // HTTP状态码
    err,          // 错误对象(用于检测context.Canceled)
    channelID,
    keyCount,     // 该渠道的Key数量
)

switch action {
case cooldown.ActionRetryKey:      // Key级错误,重试其他Key
    // ...
case cooldown.ActionRetryChannel:  // 渠道级错误,切换渠道
    // ...
case cooldown.ActionReturnClient:  // 客户端错误,直接返回
    // ...
}

// 执行冷却(自动计算持续时间)
err := cooldown.ApplyCooldownForError(store, channelID, keyID, statusCode, err)
```

### 测试指南

**单元测试模式**:
```go
// 使用testutil的API测试助手
import "ccload/internal/testutil"

func TestMyFeature(t *testing.T) {
    tester := testutil.NewAPITester(t, store)
    defer tester.Close()

    // 使用辅助方法测试API
    resp := tester.PostJSON("/admin/myfeature", payload)
    tester.AssertSuccess(resp)
    tester.AssertData(resp, expectedData)
}
```

**集成测试** (`test/integration/`):
```bash
# 需要启动真实服务
go test ./test/integration/... -v
```

## 代码规范

### Go语言现代化要求
- 使用`any`替代`interface{}`(Go 1.18+)
- 充分利用泛型和类型推导
- 遵循**KISS原则**,优先简洁可读的代码
- 遵循**DRY原则**,消除重复代码
- 遵循**SOLID原则**,单一职责、依赖抽象
- 强制执行`go fmt`和`go vet`

### 错误处理
- 使用标准Go错误处理(`error`接口和`errors`包)
- 支持错误链(Go 1.13+ `errors.Unwrap`)
- **Fail-Fast策略**: 配置错误立即退出,避免生产风险

### 安全规范
- **API Key脱敏**: 仅显示前4后4字符(`internal/util/log_sanitizer.go`)
- **认证机制**: Token认证系统(`internal/service/auth_service.go`)
- **输入验证**: 使用`validator/v10`验证请求参数

## MCP工具使用规范

**⚠️ 强制要求: 优先使用Serena MCP工具**

**代码探索**:
```
mcp__serena__get_symbols_overview → mcp__serena__find_symbol(include_body=true)
```

**代码编辑**(Symbol级别):
```
mcp__serena__replace_symbol_body / insert_after_symbol / insert_before_symbol
```

**代码搜索**:
```
mcp__serena__search_for_pattern  # 避免全文件读取
```

**依赖分析**:
```
mcp__serena__find_referencing_symbols  # 查找符号引用
```

**Token效率原则**:
- ❌ 禁止: 不加思考使用`Read`读取整个文件
- ✅ 推荐: Overview → Find Symbol → 精确编辑
- ⚠️ 标准工具: 仅用于非代码文件(`.md`/`.json`/`.yaml`)

## 技术栈

- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.38.2 (纯Go实现)
- **Redis**: go-redis v9.7.0 (可选同步)
- **JSON**: Sonic v1.14.1 (通过`-tags go_json`启用)
- **配置**: godotenv v1.5.1

## 参考资源

- **完整文档**: [README.md](README.md) - 部署、使用、故障排除
- **性能优化**: `internal/storage/sqlite/migrate.go` - 数据库索引策略
- **错误分类**: `internal/util/classifier.go` - HTTP错误处理规则
- **冷却策略**: `internal/cooldown/manager.go` - 指数退避算法
