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
- `server.go`: HTTP服务器、路由、缓存(60秒TTL)
- `handlers.go`: 通用HTTP工具(参数解析、响应处理)
- `admin.go`: 管理API(渠道CRUD、日志、统计)

**业务逻辑层** (`internal/app/`):
- `proxy.go`: 核心代理逻辑、HTTP转发、流式响应
- `selector.go`: 渠道选择算法(优先级分组+轮询+冷却)
- `key_selector.go`: 多Key管理(策略选择+Key级冷却)

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

**代理转发** (`forwardOnce`):
1. 解析请求体,提取模型名称
2. 检查模型重定向配置
3. 构建上游请求,设置认证头
4. 发送请求,处理流式响应
5. 异步记录日志(始终记录原始模型)

**故障切换机制**:
- **Key级错误**(401/403/429): 冷却当前Key,重试同渠道其他Key
- **渠道级错误**(500/502/503/504): 冷却整个渠道,切换到其他渠道
- **客户端错误**(404/405): 不冷却,直接返回
- **指数退避策略**:
  - 渠道级严重错误(500/502/503/504): 初始5分钟,后续翻倍至30分钟上限
  - 认证错误(401/402/403): 初始5分钟,后续翻倍至30分钟上限
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
- `CCLOAD_FIRST_BYTE_TIMEOUT`: 流式请求首字节超时(默认120秒)

## 代码规范

### Go 语言现代化要求
- ✅ 使用`any`替代`interface{}`(Go 1.18+)
- ✅ 充分利用泛型和类型推导
- ✅ 遵循KISS原则,优先简洁可读的代码
- ✅ 强制执行`go fmt`和`go vet`

### 错误处理规范
- ✅ 使用`internal/errors`包的应用级错误系统
- ✅ 错误代码机器可识别(如`ErrCodeNoKeys`、`ErrCodeAllCooldown`)
- ✅ 支持错误链(Go 1.13+ `errors.Unwrap`)
- ✅ 携带上下文信息(`WithContext`方法)

### 配置管理规范
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

## 技术栈

- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.38.2
- **缓存**: Ristretto v2.3.0(内存缓存)
- **Redis**: go-redis v9.7.0(可选同步)
- **JSON**: Sonic v1.14.1(高性能)
- **环境配置**: godotenv v1.5.1
