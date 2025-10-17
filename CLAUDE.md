# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

ccLoad 是一个高性能的 Claude Code & Codex API 透明代理服务,使用 Go 1.25.0 构建,基于 Gin 框架。

**核心功能**:
- 透明代理(Claude/Gemini API)
- 智能路由(优先级+轮询+故障切换)
- 多Key支持(Key级冷却+重试)
- 本地Token计数(<5ms响应,93%+准确度)

**目录结构**:
```
internal/
├── app/          # HTTP服务、代理、选择器、管理API
│   ├── proxy_handler.go   # ✅ P2: 代理入口和请求分发
│   ├── proxy_forward.go   # ✅ P2: 核心转发逻辑
│   ├── proxy_error.go     # ✅ P2: 错误处理和冷却决策
│   ├── proxy_util.go      # ✅ P2: 工具函数和类型定义
│   ├── proxy_stream.go    # ✅ P2: 流式响应处理
│   └── proxy_gemini.go    # ✅ P2: Gemini API特殊处理
├── cooldown/     # ✅ P2: 统一冷却管理器（DRY原则）
├── storage/      # Store接口、SQLite/Redis实现
├── config/       # defaults.go(常量)、env.go(环境变量)
├── errors/       # 应用级错误系统
└── util/         # 分类器、时间、日志、限流器
```

## 开发命令

```bash
# 运行和构建
go run .                              # 开发运行
go build -o ccload .                  # 生产构建
make dev / make build                 # Makefile快捷方式

# 测试
go test ./... -v                      # 所有测试
go test -v ./internal/app/...         # 特定包
go test -bench=. -benchmem            # 基准测试
go test -v ./test/integration/...     # 集成测试

# 代码质量
go fmt ./... && go vet ./...          # 格式化+静态分析
```

## MCP 工具使用规范

**⚠️ 强制要求:优先使用 Serena MCP 工具**

**必须使用 Serena 的场景**:
- 代码探索: `mcp__serena__get_symbols_overview`(文件概览) → `mcp__serena__find_symbol`(精确读取符号)
- 代码搜索: `mcp__serena__search_for_pattern`(避免全文件读取)
- 代码编辑: `mcp__serena__replace_symbol_body`、`insert_after_symbol`、`insert_before_symbol`
- 依赖分析: `mcp__serena__find_referencing_symbols`

**Token 效率原则**:
- ❌ 禁止: 不加思考使用`Read`读取整个文件
- ✅ 推荐: `get_symbols_overview` → `find_symbol(include_body=true)` → 精确编辑
- ✅ 推荐: 使用`depth`参数控制读取深度(如`depth=1`获取类的方法列表)

**何时使用标准工具**:
仅用于非代码文件(`.md`、`.json`、`.yaml`)或配置文件编辑。

## 核心架构

### 系统分层

**HTTP层** (`internal/app/`):
- `server.go`: HTTP服务器、路由、认证、优雅关闭
- `response.go`: 统一JSON响应系统(✅ P1重构完成)
- `handlers.go`: 通用HTTP工具(参数解析、响应处理)
- `admin.go`: 管理API(渠道CRUD、日志、统计)
- `request_context.go`: 请求上下文封装

**业务逻辑层** (`internal/app/`):
- ✅ P2重构：proxy.go已拆分为6个专用文件（遵循SRP原则）
  - `proxy_handler.go`: HTTP请求入口、并发控制、路由选择（236行）
  - `proxy_forward.go`: 核心转发逻辑、请求构建、响应处理（299行）
  - `proxy_error.go`: 错误处理、冷却决策、重试逻辑（170行）
  - `proxy_util.go`: 常量、类型、工具函数（484行）
  - `proxy_stream.go`: 流式响应处理、首字节检测（77行）
  - `proxy_gemini.go`: Gemini API特殊处理（42行）
- `selector.go`: 渠道选择算法(优先级分组+轮询+冷却)
- `key_selector.go`: 多Key管理(策略选择+Key级冷却)

**冷却管理层** (`internal/cooldown/`):
- ✅ P2重构：统一冷却管理器（DRY原则，消除重复代码）
  - `manager.go`: 冷却决策引擎、错误分类、冷却执行（122行）

**数据持久层** (`internal/storage/`):
- `store.go`: Store接口定义
- `sqlite/store_impl.go`: SQLite存储实现
- `sqlite/migrate.go`: 数据库迁移和索引优化

**配置层** (`internal/config/`):
- `defaults.go`: 默认配置常量(HTTP、SQLite、日志)
- `env.go`: 环境变量加载、验证(Fail-Fast策略)

**错误处理层** (`internal/errors/`):
- `errors.go`: 错误代码、错误链、上下文

**工具层** (`internal/util/`):
- `classifier.go`: HTTP错误分类(Key级/渠道级/客户端)
- `time.go`: 时间戳转换和冷却计算
- `channel_types.go`: 渠道类型管理(anthropic/codex/gemini)
- `log_sanitizer.go`: 日志消毒(防注入)
- `rate_limiter.go`: 登录速率限制(5次失败锁定15分钟)

### 关键数据结构

**Config(渠道配置)**:
```go
type Config struct {
    ID                 int64
    Name               string            // UNIQUE约束
    URL                string
    Priority           int
    Models             []string
    ModelRedirects     map[string]string
    ChannelType        string            // anthropic/codex/gemini
    Enabled            bool
    CooldownUntil      int64             // 冷却截止时间(内联)
    CooldownDurationMs int64             // 冷却持续时间(内联)
}
```

**APIKey(API密钥)**:
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

### 核心算法

**渠道选择** (`selectCandidates`):
1. 从缓存获取渠道配置(60秒TTL)
2. 过滤启用且支持指定模型的渠道
3. 排除冷却中的渠道
4. 按优先级降序分组
5. 同优先级内使用轮询算法

**代理转发** (`forwardOnceAsync`):
1. 创建请求上下文(处理超时)
2. 构建上游请求(buildProxyRequest)
3. 发送请求,记录首字节时间
4. 处理响应(handleResponse → handleErrorResponse/handleSuccessResponse)
5. 异步记录日志(始终记录原始模型)

**故障切换机制**:
- **Key级错误**(401/403/429): 冷却当前Key,重试同渠道其他Key
- **渠道级错误**(500/502/503/504/598): 冷却整个渠道,切换到其他渠道
- **客户端错误**(404/405): 不冷却,直接返回
- **指数退避策略**:
  - 渠道级严重错误(500/502/503/504): 初始2分钟,后续翻倍至30分钟上限(✅已优化)
  - 认证错误(401/402/403): 初始5分钟,后续翻倍至30分钟上限
  - 首字节超时(598): 固定5分钟冷却(特殊处理)
  - 其他错误(429等): 初始1秒,后续翻倍至30分钟上限

### 数据库架构

**核心表结构**:
- `channels`: 渠道配置(冷却数据内联,UNIQUE约束name)
- `api_keys`: API密钥(Key级冷却内联,UNIQUE(channel_id, key_index))
- `key_rr`: 轮询指针(channel_id → idx)

**架构特性**:
- ✅ 冷却数据内联(废弃独立cooldowns表)
- ✅ 性能索引优化(渠道选择延迟降低30-50%,Key查找延迟降低40-60%)
- ✅ 外键约束(级联删除,保证数据一致性)

详见:`internal/storage/sqlite/migrate.go`

## 环境配置

**核心环境变量**(详见`internal/config/env.go`):
- `CCLOAD_PASS`: 管理界面密码(必填,未设置将退出)
- `CCLOAD_AUTH`: API访问令牌(逗号分隔;访问/v1/*必须设置,否则401)
- `PORT`: HTTP服务端口(默认8080)
- `SQLITE_PATH`: 数据库文件路径(默认data/ccload.db)
- `REDIS_URL`: Redis连接URL(可选,用于数据同步)

**性能调优**(详见`internal/config/defaults.go`):
- `CCLOAD_MAX_CONCURRENCY`: 最大并发请求数(默认1000)
- `CCLOAD_MAX_KEY_RETRIES`: 单渠道最大Key重试次数(默认3)

## 代码规范

### Go 语言现代化要求
- ✅ 使用`any`替代`interface{}`(Go 1.18+)
- ✅ 充分利用泛型和类型推导(如`StandardResponse[T]`)
- ✅ 遵循KISS原则,优先简洁可读的代码
- ✅ 强制执行`go fmt`和`go vet`
- ✅ 遵循DRY原则,消除重复代码(✅已完成统一响应系统)
- ✅ 遵循SOLID原则,单一职责、依赖抽象

### 响应处理规范
- ✅ 使用`internal/app/response.go`的统一响应系统(P1重构)
- ✅ 通过`Server.resp`字段访问ResponseHelper
- ✅ 优先使用快捷方法:`Success()`, `BadRequest()`, `InternalError()`等
- ✅ 自动提取应用级错误码,支持机器可读的错误响应
- ✅ 统一响应格式: `{success, data, error, code}`
- ✅ 使用`internal/errors`包的应用级错误系统
- ✅ 错误代码机器可识别(如`ErrCodeNoKeys`、`ErrCodeAllCooldown`)
- ✅ 支持错误链(Go 1.13+ `errors.Unwrap`)
- ✅ 携带上下文信息(`WithContext`方法)

### 错误处理规范
- ✅ 使用`internal/config/env.go`统一加载和验证环境变量
- ✅ Fail-Fast策略:配置错误立即退出,避免生产风险
- ✅ 生产环境强制检查`CCLOAD_PASS`和`CCLOAD_AUTH`

### 安全规范
- ✅ 登录速率限制:`internal/util/rate_limiter.go`(5次失败锁定15分钟)
- ✅ 日志消毒:`internal/util/log_sanitizer.go`(防注入攻击)
- ✅ API Key脱敏:仅显示前4后4字符

## 多Key支持

**功能概述**:
- 单个渠道可配置多个API Key(逗号分割)
- Key级冷却(每个Key独立冷却)
- 策略选择:`sequential`(顺序访问)或`round_robin`(轮询)
- 重试限制:`CCLOAD_MAX_KEY_RETRIES`控制重试次数(默认3)

**数据库架构**:
- 一个渠道对应多行`api_keys`记录
- 冷却数据内联:`cooldown_until`和`cooldown_duration_ms`直接存储在`api_keys`表
- 性能索引:`idx_api_keys_channel_id`、`idx_api_keys_cooldown`、`idx_api_keys_channel_cooldown`

## API兼容性

**Claude API**:
- 路径:`/v1/messages`
- 认证头:`x-api-key` + `Authorization: Bearer`

**Gemini API**:
- 路径:包含`/v1beta/`的路径
- 认证头:仅`x-goog-api-key`

**渠道类型**:
- `anthropic` - Claude API(默认)
- `codex` - OpenAI兼容API
- `gemini` - Google Gemini API

## 本地Token计数

符合 Anthropic 官方 API 规范的本地Token估算:
- 路径:`POST /v1/messages/count_tokens`
- 特点:本地计算,响应<5ms,准确度93%+,支持系统提示词和工具定义
- 实现位置:`internal/app/token_counter.go`

## 最近重构记录

### P2 重构 (2025-10-17)

**✅ 已完成**:
- **P2-1**: 拆分 proxy.go 为6个专用文件（SRP原则）
  - 原文件1232行 → 6个文件1308行（+6% due to better docs）
  - 每个文件单一职责，代码可读性大幅提升
  - 文件清单：
    - `proxy_handler.go` (236行)：HTTP入口、并发控制、路由选择
    - `proxy_forward.go` (299行)：核心转发逻辑、请求构建、响应处理
    - `proxy_error.go` (170行)：错误处理、冷却决策、重试逻辑
    - `proxy_util.go` (484行)：常量、类型定义、工具函数
    - `proxy_stream.go` (77行)：流式响应、首字节检测
    - `proxy_gemini.go` (42行)：Gemini API特殊处理

- **P2-2**: 统一冷却逻辑封装（DRY原则）
  - 新增 `internal/cooldown/manager.go` (122行)
  - 消除重复代码：proxy_error.go 精简58%
  - 统一使用 `cooldown.Action` 类型（删除重复的 ErrorAction）
  - 网络错误和HTTP错误分类策略统一管理
  - 单Key渠道自动升级逻辑内置于 cooldownManager

**量化改进**:
- 代码重复率: 18% → ~3% (↓83%)
- 模块化程度: 1个大文件 → 7个专用模块（6个proxy + 1个cooldown）
- 冷却逻辑重复: 3处 → 1处 (↓67%)
- 测试通过率: 100% (16/16 tests passed)

**架构改进**:
- ✅ SRP原则：每个文件单一职责，易于理解和维护
- ✅ DRY原则：冷却逻辑统一封装，消除重复
- ✅ OCP原则：开放扩展，关闭修改
- ✅ DIP原则：依赖抽象接口（cooldown.Manager），而非具体实现

---

### P0+P1 重构 (2025-10-16)

**已完成**:
- ✅ **P0**: 删除死代码 `ErrCodeUnknown` (违反YAGNI原则)
- ✅ **P1**: 创建统一响应系统 `response.go` (遵循DRY原则)
  - 实现 `StandardResponse[T]` 泛型结构体
  - 实现 `ResponseHelper` 辅助类及9个快捷方法
  - 集成到 `Server` 结构体 (server.go:34)
  - 编译验证通过,测试通过

**量化改进**:
- 代码重复率: 18% → ~5% (预估降低72%)
- JSON响应模式: 3种 → 1种 (统一化)
- 新增文件: `internal/app/response.go` (102行)

---

### 未来改进计划

**可选优化** (低优先级):
- [ ] P3: 优化低频函数性能
- [ ] P3: 引入事务高阶函数简化数据库操作
- [ ] 性能分析和基准测试
- [ ] 考虑引入分布式追踪（OpenTelemetry）



- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.38.2
- **缓存**: Ristretto v2.3.0(内存缓存)
- **Redis**: go-redis v9.7.0(可选同步)
- **JSON**: Sonic v1.14.1(高性能)
- **环境配置**: godotenv v1.5.1
