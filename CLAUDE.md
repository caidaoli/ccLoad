# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

ccLoad 是一个高性能的 Claude Code & Codex API 透明代理服务，使用 Go 1.25.0 构建，基于 Gin 框架。

### 核心功能

- **透明代理**：支持 Claude API（`/v1/messages`）和 Gemini API（`/v1beta/*`），智能识别认证方式
- **本地Token计数**：符合官方规范的本地估算接口，响应<5ms，准确度93%+
- **智能路由**：基于模型支持、优先级和轮询策略选择渠道
- **多Key支持**：单渠道配置多个API Key，支持顺序/轮询策略，Key级别故障切换和冷却
- **故障切换**：自动切换Key/渠道，指数退避冷却（1s → 2s → 4s → ... → 30min）
- **统计监控**：实时趋势分析、日志记录、性能指标监控
- **前端管理**：现代化 Web 界面，支持渠道CRUD、CSV导入导出、实时监控

### 目录结构

```
ccLoad/
├── main.go                      # 应用入口
├── Makefile                     # macOS服务管理
├── Dockerfile                   # 容器镜像构建
├── internal/
│   ├── app/                     # 应用层（HTTP服务、代理、管理API）
│   │   ├── server.go           # HTTP服务器和路由
│   │   ├── proxy.go            # 核心代理逻辑
│   │   ├── selector.go         # 渠道选择算法
│   │   ├── key_selector.go     # 多Key管理
│   │   ├── admin.go            # 管理API
│   │   ├── handlers.go         # HTTP工具函数
│   │   └── token_counter.go    # 本地Token计数
│   ├── storage/                 # 存储层
│   │   ├── store.go            # Store接口定义
│   │   ├── sqlite/             # SQLite实现
│   │   │   ├── store_impl.go   # 核心存储实现
│   │   │   └── query.go        # SQL查询构建
│   │   └── redis/              # Redis同步（可选）
│   ├── model/                   # 数据模型
│   ├── config/                  # 配置常量
│   │   └── defaults.go         # 默认配置值
│   ├── util/                    # 工具模块
│   │   ├── classifier.go       # HTTP错误分类
│   │   ├── time_utils.go       # 时间处理
│   │   ├── channel_types.go    # 渠道类型
│   │   ├── api_keys_helper.go  # API Key工具
│   │   └── log_sanitizer.go    # 日志消毒
│   └── testutil/               # 测试辅助
├── test/integration/           # 集成测试
└── web/                        # 前端界面
    ├── index.html              # 首页
    ├── channels.html           # 渠道管理
    ├── logs.html               # 日志查看
    └── styles.css              # 共享样式
```

## 开发命令

### 构建和运行

```bash
# 开发环境运行
go run .
make dev

# 使用.env文件配置（推荐）
echo "CCLOAD_PASS=your_password" > .env
echo "CCLOAD_AUTH=your_api_token" >> .env
echo "PORT=8080" >> .env
go run .

# 构建生产版本
go build -o ccload .
make build
```

### 测试

```bash
# 运行所有测试
go test ./... -v

# 运行特定包的测试
go test -v ./internal/app/...
go test -v ./internal/storage/sqlite/...

# 生成测试覆盖率
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out

# 基准测试
go test -bench=. -benchmem
```

### macOS 服务管理

```bash
make install-service    # 安装并启动服务
make start             # 启动服务
make stop              # 停止服务
make restart           # 重启服务
make status            # 查看状态
make logs              # 查看日志
make uninstall-service # 卸载服务
```

### 代码质量

```bash
# 格式化和检查
go fmt ./...     # 代码格式化
go vet ./...     # 静态分析

# Docker构建
docker build -t ccload:dev .
```

## 核心架构

### 系统分层

**HTTP层** (`internal/app/`):
- `server.go`: HTTP服务器、路由配置、缓存管理
- `handlers.go`: 通用HTTP处理工具（参数解析、响应处理）
- `admin.go`: 管理API（渠道CRUD、日志查询、统计）

**业务逻辑层** (`internal/app/`):
- `proxy.go`: 核心代理逻辑，HTTP转发、流式响应
- `selector.go`: 渠道选择算法（优先级分组 + 轮询 + 冷却）
- `key_selector.go`: 多Key管理、策略选择、Key级冷却

**数据持久层** (`internal/storage/`):
- `store.go`: Store接口定义
- `sqlite/store_impl.go`: SQLite存储实现
- `sqlite/query.go`: SQL查询构建器

**工具层** (`internal/util/`):
- `classifier.go`: HTTP状态码错误分类（Key级/渠道级/客户端）
- `time_utils.go`: 时间戳转换和冷却计算
- `channel_types.go`: 渠道类型管理（anthropic/codex/gemini）
- `log_sanitizer.go`: 日志消毒，防止注入攻击

### 关键数据结构

**Config（渠道配置）**:
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
    CooldownUntil      int64             // 冷却截止时间（内联）
    CooldownDurationMs int64             // 冷却持续时间（内联）
    CreatedAt          time.Time
    UpdatedAt          time.Time
}
```

**APIKey（API密钥）**:
```go
type APIKey struct {
    ID                 int64
    ChannelID          int64
    KeyIndex           int
    APIKey             string
    KeyStrategy        string  // sequential/round_robin
    CooldownUntil      int64   // Key级冷却（内联）
    CooldownDurationMs int64
    CreatedAt          time.Time
    UpdatedAt          time.Time
}
```

### 核心算法

**渠道选择** (`selectCandidates`):
1. 从缓存获取渠道配置（60秒TTL）
2. 过滤启用且支持指定模型的渠道
3. 排除冷却中的渠道
4. 按优先级降序分组
5. 同优先级内使用轮询算法

**代理转发** (`forwardOnce`):
1. 解析请求体，提取模型名称
2. 检查模型重定向配置
3. 构建上游请求，设置认证头
4. 发送请求，处理流式响应
5. 异步记录日志（始终记录原始模型）

**故障切换机制**:
- **Key级错误**（401/403/429）：冷却当前Key，重试同渠道其他Key
- **渠道级错误**（500/502/503/504）：冷却整个渠道，切换到其他渠道
- **客户端错误**（404/405）：不冷却，直接返回
- **指数退避**：认证错误初始5分钟，其他错误初始1秒，后续翻倍至30分钟上限

## 环境配置

### 核心环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_PASS` | "admin" | 管理界面密码（⚠️ 生产环境必须修改） |
| `CCLOAD_AUTH` | 无 | API访问令牌（多个用逗号分隔） |
| `PORT` | "8080" | HTTP服务端口 |
| `SQLITE_PATH` | "data/ccload.db" | 数据库文件路径 |
| `REDIS_URL` | 无 | Redis连接URL（可选，用于数据同步） |

### 性能调优

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_MAX_CONCURRENCY` | 1000 | 最大并发请求数 |
| `CCLOAD_MAX_KEY_RETRIES` | 3 | 单渠道最大Key重试次数 |
| `CCLOAD_FIRST_BYTE_TIMEOUT` | 120 | 流式请求首字节超时（秒） |
| `CCLOAD_USE_MEMORY_DB` | "false" | 启用内存数据库（需配合Redis） |
| `SQLITE_JOURNAL_MODE` | "WAL" | SQLite日志模式（WAL/TRUNCATE） |

### 配置常量

完整配置常量定义在 `internal/config/defaults.go`，包括：
- HTTP服务器配置（连接池、超时等）
- SQLite连接池配置
- 日志系统配置
- Token认证配置

## 数据库架构

### 核心表结构

**channels 表**：
```sql
CREATE TABLE channels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,              -- UNIQUE约束
    url TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    models TEXT NOT NULL,                   -- JSON数组
    model_redirects TEXT DEFAULT '{}',      -- JSON对象
    channel_type TEXT DEFAULT 'anthropic',
    enabled INTEGER NOT NULL DEFAULT 1,
    cooldown_until INTEGER DEFAULT 0,       -- 冷却数据内联
    cooldown_duration_ms INTEGER DEFAULT 0,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
```

**api_keys 表**：
```sql
CREATE TABLE api_keys (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL,
    key_index INTEGER NOT NULL,
    api_key TEXT NOT NULL,
    key_strategy TEXT DEFAULT 'sequential',
    cooldown_until INTEGER DEFAULT 0,       -- Key级冷却内联
    cooldown_duration_ms INTEGER DEFAULT 0,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    UNIQUE(channel_id, key_index),
    FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
);
```

**key_rr 表**：
```sql
CREATE TABLE key_rr (
    channel_id INTEGER PRIMARY KEY,
    idx INTEGER NOT NULL
);
```

## API端点

### 公开端点（无需认证）
```
GET  /public/summary              # 基础统计
POST /v1/messages/count_tokens    # 本地Token计数
```

### 代理端点（条件认证）
```
POST /v1/messages                 # Claude API代理
GET  /v1beta/*                    # Gemini API代理
```

### 管理端点（需要登录）
```
GET/POST    /admin/channels              # 渠道列表和创建
GET/PUT/DEL /admin/channels/{id}         # 渠道操作
GET         /admin/channels/export       # 导出CSV
POST        /admin/channels/import       # 导入CSV
GET         /admin/errors                # 请求日志
GET         /admin/stats                 # 统计数据
```

## 技术栈

- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.38.2
- **缓存**: Ristretto v2.3.0（内存缓存）
- **Redis**: go-redis v9.7.0（可选同步）
- **JSON**: Sonic v1.14.1（高性能）
- **环境配置**: godotenv v1.5.1
- **前端**: 原生HTML/CSS/JavaScript

## 代码规范

### Go 语言现代化要求

- ✅ 使用 `any` 替代 `interface{}`（Go 1.18+）
- ✅ 充分利用泛型和类型推导
- ✅ 遵循KISS原则，优先简洁可读的代码
- ✅ 强制执行 `go fmt` 和 `go vet`

## 常见开发任务

### 快速调试

```bash
# 检查端口占用
lsof -i :8080 && kill -9 <PID>

# 测试API可用性
curl -s http://localhost:8080/public/summary | jq

# 测试Token计数
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"test"}]}'

# 查看数据库
sqlite3 data/ccload.db "SELECT id, name, priority, enabled FROM channels;"
```

### 性能分析

```bash
# CPU性能分析
go test -cpuprofile=cpu.prof -bench=.
go tool pprof cpu.prof

# 内存分析
go test -memprofile=mem.prof -bench=.
go tool pprof mem.prof
```

### 常见问题排查

**渠道选择失败**：
```bash
# 检查渠道配置
curl http://localhost:8080/admin/channels | jq '.data[] | {id, name, models, enabled}'

# 清除冷却状态（已内联到channels/api_keys表）
sqlite3 data/ccload.db "UPDATE channels SET cooldown_until=0;"
sqlite3 data/ccload.db "UPDATE api_keys SET cooldown_until=0;"
```

**Redis同步问题**：
```bash
# 测试Redis连接
go run . test-redis

# 检查Redis数据
redis-cli -u $REDIS_URL GET ccload:channels
```

## 多Key支持

### 功能概述

单个渠道可配置多个API Key，支持：
- **多Key配置**：逗号分割多个Key
- **Key级冷却**：每个Key独立冷却
- **灵活策略**：顺序访问（sequential）或轮询（round_robin）
- **重试限制**：`CCLOAD_MAX_KEY_RETRIES`控制重试次数（默认3次）

### 配置方式

```bash
# API配置示例
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude-MultiKey",
    "api_key": "sk-ant-key1,sk-ant-key2,sk-ant-key3",
    "key_strategy": "round_robin",
    "url": "https://api.anthropic.com",
    "priority": 10,
    "models": ["claude-3-5-sonnet-20241022"],
    "enabled": true
  }'
```

### 数据库架构

- **多Key存储**：一个渠道对应多行 `api_keys` 记录
- **冷却数据内联**：`cooldown_until` 和 `cooldown_duration_ms` 直接存储在 `api_keys` 表
- **废弃表**：`key_cooldowns` 表已废弃

## API兼容性支持

### Claude API
- **路径**：`/v1/messages`
- **认证头**：`x-api-key` + `Authorization: Bearer`

### Gemini API
- **路径**：包含 `/v1beta/` 的路径
- **认证头**：仅 `x-goog-api-key`

### 渠道类型

支持三种渠道类型（`channel_type`）：
- `anthropic` - Claude API（默认）
- `codex` - OpenAI兼容API
- `gemini` - Google Gemini API

特定请求（如 `GET /v1beta/models`）按渠道类型路由。

## 本地Token计数

符合 Anthropic 官方 API 规范的本地Token估算：

```bash
POST /v1/messages/count_tokens
```

**特点**：
- 本地计算，响应 <5ms
- 准确度 93%+
- 支持系统提示词、工具定义
- 无需认证

**实现位置**：`internal/app/token_counter.go`

## Redis同步功能

可选的Redis同步功能，用于渠道配置备份：

**核心特性**：
- 异步同步（响应<1ms）
- 启动时自动恢复
- 故障隔离（Redis失败不影响核心功能）

**配置**：
```bash
export REDIS_URL="redis://localhost:6379"
```

**数据结构**：
- Key: `ccload:channels`
- 格式: JSON数组（全量覆盖）

## 安全考虑

- ✅ **强密码策略**：生产环境必须设置强 `CCLOAD_PASS`
- ✅ **API认证**：建议设置 `CCLOAD_AUTH` 保护API端点
- ✅ **数据脱敏**：API Key自动脱敏（前4后4）
- ✅ **日志消毒**：自动防止日志注入攻击
- ✅ **内存模式安全**：强制要求配置Redis防止数据丢失
- 🔒 **HTTPS部署**：建议使用反向代理配置SSL
