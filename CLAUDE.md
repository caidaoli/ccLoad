# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

ccLoad 是一个高性能的 Claude Code & Codex API 透明代理服务，使用 Go 1.24.0 构建，基于 Gin 框架。主要功能：

- **透明代理**：将 `/v1/messages` 请求转发到上游 Claude API，仅替换 API Key
- **智能路由**：基于模型支持、优先级和轮询策略选择渠道  
- **故障切换**：失败时自动切换渠道并实施指数退避冷却（起始1秒，错误翻倍，封顶30分钟）
- **身份验证**：管理页面需要密码登录，支持session管理和自动过期；API端点支持可选令牌认证
- **统计监控**：首页公开显示请求统计，管理界面提供详细的趋势和日志分析
- **前端管理**：提供现代化 Web 界面管理渠道、查看趋势、日志和调用统计

## 开发命令

### 构建和运行
```bash
# 开发环境运行（使用默认配置）
go run .
make dev         # Makefile 开发模式

# 使用环境变量配置
CCLOAD_PASS=your_password CCLOAD_AUTH=token1,token2 SQLITE_PATH=./data/ccload.db PORT=8080 go run .

# 使用.env文件配置（推荐）
echo "CCLOAD_PASS=your_password" > .env
echo "CCLOAD_AUTH=your_api_token" >> .env
echo "SQLITE_PATH=./data/ccload.db" >> .env  
echo "PORT=8080" >> .env
go run .

# 构建生产版本
go build -o ccload .
make build       # Makefile 构建

# 构建到临时目录
go build -o /tmp/ccload .
```

### macOS 服务管理（使用 Makefile）
```bash
make install-service    # 安装并启动 LaunchAgent 服务
make start             # 启动服务
make stop              # 停止服务
make restart           # 重启服务
make status            # 查看服务状态
make logs              # 查看服务日志
make error-logs        # 查看错误日志
make uninstall-service # 卸载服务
make clean             # 清理构建文件和日志
make info              # 显示服务详细信息
```

### 构建标签
```bash
go fmt ./...     # 格式化代码
go vet ./...     # 静态检查
go test ./...    # 运行测试（当前项目暂无测试文件）

# 支持构建标签（GOTAGS）
GOTAGS=go_json go build -tags go_json .     # 启用 goccy/go-json 构建标签（默认）
GOTAGS=std go build -tags std .             # 使用标准库 JSON
```

## 核心架构

### 系统组件分层

**HTTP层** (`server.go`, `admin.go`, `handlers.go`):
- `Server`: 主服务器结构，管理HTTP客户端、缓存、身份验证
- `handlers.go`: 通用HTTP处理工具（参数解析、响应处理、方法路由）
- `admin.go`: 管理API实现（渠道CRUD、日志查询、统计分析）
- 身份验证：Session-based管理界面 + 可选Bearer token API认证

**业务逻辑层** (`proxy.go`, `selector.go`):
- `proxy.go`: 核心代理逻辑，处理`/v1/messages`转发和流式响应
- `selector.go`: 智能渠道选择算法（优先级分组 + 组内轮询 + 故障排除）

**数据持久层** (`sqlite_store.go`, `query_builder.go`, `models.go`):
- `models.go`: 数据模型和Store接口定义
- `sqlite_store.go`: SQLite存储实现，支持连接池和事务
- `query_builder.go`: 查询构建器，消除SQL构建重复逻辑

### 关键数据结构
- `Config`（渠道）: 渠道配置（API Key、URL、优先级、支持的模型列表）
- `LogEntry`: 请求日志（时间、模型、渠道ID、状态码、性能指标）
- `Store` 接口: 数据持久化抽象层，支持配置、日志、统计、冷却管理
- `MetricPoint`: 时间序列数据点（用于趋势分析）
- `StatsEntry`: 统计数据聚合（按渠道和模型分组）

### 核心算法实现

**渠道选择算法** (`selectCandidates` in selector.go):
1. 从缓存获取渠道配置（60秒TTL，避免频繁数据库查询）
2. 过滤启用且支持指定模型的渠道
3. 排除冷却中的渠道（使用内存缓存，快速查询）
4. 按优先级降序分组
5. 同优先级内使用轮询算法（内存缓存轮询指针，定期持久化）

**代理转发流程** (`forwardOnce` in proxy.go):
1. 构建上游请求URL，合并查询参数
2. 复制请求头，跳过授权相关头，覆盖`x-api-key`
3. 发送POST请求到上游API（使用优化的HTTP客户端连接池）
4. 处理响应：2xx响应支持流式转发（64KB缓冲区），其他响应读取完整body
5. 异步记录日志到队列（批量写入数据库）

**故障切换机制**:
- 非2xx响应或网络错误触发切换
- 失败渠道按指数退避进入冷却（内存+数据库双重存储）
- 按候选列表顺序尝试下一个渠道
- 所有候选失败返回503 Service Unavailable

## 性能优化架构

**多级缓存系统**:
- **渠道配置缓存**: 60秒TTL，减少90%数据库查询
- **轮询指针缓存**: 内存存储，定期持久化，支持高并发
- **冷却状态缓存**: sync.Map实现，快速故障检测

**异步处理**:
- **日志系统**: 1000条缓冲队列，3个worker协程，批量写入
- **会话清理**: 后台协程每小时清理过期session
- **冷却清理**: 每分钟清理过期冷却状态

**连接池优化**:
- **SQLite连接池**: 25个连接，5分钟生命周期
- **HTTP客户端**: 100最大连接，10秒连接超时，keepalive优化
- **TLS优化**: LRU会话缓存，减少握手耗时

## 重构架构说明

项目经过大规模重构，采用现代Go开发模式：

**HTTP处理器模式** (`handlers.go`):
- `PaginationParams`: 统一参数解析和验证
- `APIResponse[T]`: 类型安全的泛型响应结构
- `MethodRouter`: 声明式HTTP方法路由，替代switch-case
- `RequestValidator`: 接口驱动的请求验证

**查询构建器模式** (`query_builder.go`):
- `WhereBuilder`: 动态SQL条件构建，防止SQL注入
- `QueryBuilder`: 组合式查询构建，支持链式调用
- `ConfigScanner`: 统一数据库行扫描，消除重复逻辑

## 环境配置

### 环境变量
- `CCLOAD_PASS`: 管理后台密码（默认: "admin"，生产环境必须设置）
- `CCLOAD_AUTH`: API访问令牌（可选，多个令牌用逗号分隔）
- `SQLITE_PATH`: SQLite数据库路径（默认: "data/ccload.db"）
- `PORT`: HTTP服务端口（默认: "8080"）

支持 `.env` 文件配置（优先于系统环境变量）

### API身份验证系统
- **管理界面**: 基于Session的认证，24小时有效期
- **API端点**: 当设置`CCLOAD_AUTH`时，`/v1/messages`需要`Authorization: Bearer <token>`
- **安全特性**: HttpOnly Cookie、SameSite保护、自动过期清理

## 数据库架构

### 核心表结构
- **channels**: 渠道配置（id, name, api_key, url, priority, models, enabled, timestamps）
- **logs**: 请求日志（id, time, model, channel_id, status_code, message, performance_metrics）
- **cooldowns**: 冷却状态（channel_id, until, duration_ms）
- **rr**: 轮询指针（key="model|priority", idx）

### 性能优化索引
- `idx_logs_time`: 日志时间索引，优化时间范围查询
- `idx_channels_name`: 渠道名称索引，优化过滤查询
- `idx_logs_status`: 状态码索引，优化错误统计

## API端点架构

### 公开端点（无需认证）
```
GET  /public/summary       # 基础统计数据
GET  /web/index.html       # 首页
GET  /web/login.html       # 登录页面
```

### API认证端点
```
POST /v1/messages          # Claude API 透明代理（条件认证）
```

### 管理端点（需要登录）
```
GET/POST    /admin/channels       # 渠道列表和创建
GET/PUT/DEL /admin/channels/{id}  # 渠道详情、更新、删除
POST        /admin/channels/{id}/test  # 渠道测试
GET         /admin/errors         # 请求日志列表（支持分页和过滤）
GET         /admin/stats          # 调用统计数据
GET         /admin/metrics        # 趋势数据（支持hours和bucket_min参数）
```

## 前端架构

纯HTML/CSS/JavaScript实现，无框架依赖的单页应用：

### 页面文件
- `web/index.html`: 首页，显示24小时请求统计
- `web/login.html`: 登录页面
- `web/channels.html`: 渠道管理（CRUD操作）
- `web/logs.html`: 请求日志（支持分页）
- `web/stats.html`: 调用统计（按渠道/模型分组）
- `web/trend.html`: 趋势图表（SVG绘制24小时曲线）
- `web/styles.css`: 共享样式文件
- `web/ui.js`: 共享JavaScript工具函数

### 技术特点
- 响应式设计，支持移动端
- 实时数据更新和图表渲染
- 模态框交互和表单验证
- 深色模式兼容的配色方案

## 重要注意事项

**透明转发原则**:
- 仅替换 `x-api-key` 和 `Authorization` 头为配置的 API Key
- 客户端需自行设置 `anthropic-version` 等必需头
- 2xx 响应支持流式转发，使用 64KB 缓冲区

**安全考虑**:
- 生产环境必须设置强密码 `CCLOAD_PASS`
- 建议设置 `CCLOAD_AUTH` 以保护 `/v1/messages` 端点
- API Key不记录到日志中，仅在内存中使用
- 生产环境需限制 `data/` 目录访问权限
- 使用 HTTPS 部署以保护传输中的认证令牌

## 技术栈

- **语言**: Go 1.24.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.14.32（嵌入式）
- **缓存**: Ristretto v2.3.0（内存缓存）
- **JSON**: Sonic v1.14.1（高性能JSON库）
- **环境配置**: godotenv v1.5.1
- **前端**: 原生HTML/CSS/JavaScript（无框架依赖）

## 代码规范

### Go 语言现代化要求

**类型声明现代化**:
- ✅ **使用 `any` 替代 `interface{}`**: 遵循 Go 1.18+ 社区最佳实践
- ✅ **泛型优先**: 在合适场景使用 Go 1.18+ 泛型语法
- ✅ **类型推导**: 充分利用现代Go的类型推导能力

**代码质量标准**:
- **KISS原则**: 优先选择更简洁、可读性更强的现代语法
- **一致性要求**: 全项目统一使用现代Go语法规范
- **向前兼容**: 充分利用Go语言版本特性，保持技术栈先进性

**具体规范**:
```go
// ✅ 推荐：使用现代语法
func processData(data map[string]any) any {
    return data["result"]
}

// ❌ 避免：过时语法  
func processData(data map[string]interface{}) interface{} {
    return data["result"]
}
```

**工具链要求**:
- **go fmt**: 强制代码格式化
- **go vet**: 静态分析检查
- **现代化检查**: 定期审查并升级代码语法到最新标准