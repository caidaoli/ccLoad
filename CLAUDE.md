# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

ccLoad 是一个高性能的 Claude Code & Codex API 透明代理服务，使用 Go 1.25.0 构建，基于 Gin 框架。主要功能：

- **透明代理**：支持Claude API（`/v1/messages`）和Gemini API（`/v1beta/*`）请求转发，智能识别并设置正确的认证头
- **智能路由**：基于模型支持、优先级和轮询策略选择渠道
- **多Key支持**：渠道支持配置多个API Key，提供顺序/轮询两种使用策略，Key级别故障切换和冷却
- **故障切换**：失败时自动切换Key/渠道并实施指数退避冷却（起始1秒，错误翻倍，封顶30分钟）
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

### 测试和代码质量
```bash
# 运行所有测试
go test ./...
go test -v ./...  # 详细输出

# 运行特定测试
go test -v -run TestSerializeModelRedirects        # 运行单个测试函数
go test -v -run "TestModelRedirect"               # 运行匹配模式的测试
go test -v -run "TestModelRedirect/重定向opus"    # 运行特定子测试

# 运行性能基准测试
go test -bench=. -benchmem

# 测试覆盖率
go test -cover ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# 代码质量检查
go fmt ./...     # 格式化代码
go vet ./...     # 静态检查

# 支持构建标签（GOTAGS）
GOTAGS=go_json go build -tags go_json .     # 启用高性能 JSON 库（默认）
GOTAGS=std go build -tags std .             # 使用标准库 JSON

# Docker 构建
docker build -t ccload:dev .               # 本地构建测试镜像
docker-compose up -d                       # 使用 compose 启动服务
```

## 核心架构

### 系统组件分层

**HTTP层** (`server.go`, `admin.go`, `handlers.go`):
- `Server`: 主服务器结构，管理HTTP客户端、缓存、身份验证
- `handlers.go`: 通用HTTP处理工具（参数解析、响应处理、方法路由）
- `admin.go`: 管理API实现（渠道CRUD、日志查询、统计分析）
- 身份验证：Session-based管理界面 + 可选Bearer token API认证

**业务逻辑层** (`proxy.go`, `selector.go`, `key_selector.go`):
- `proxy.go`: 核心代理逻辑，处理`/v1/messages`转发和流式响应
- `selector.go`: 智能渠道选择算法（优先级分组 + 组内轮询 + 故障排除）
- `key_selector.go`: Key选择器，实现多Key管理、策略选择和Key级别冷却（SRP原则）

**数据持久层** (`sqlite_store.go`, `query_builder.go`, `models.go`, `redis_sync.go`):
- `models.go`: 数据模型和Store接口定义
- `sqlite_store.go`: SQLite存储实现，支持连接池和事务
- `query_builder.go`: 查询构建器，消除SQL构建重复逻辑
- `redis_sync.go`: Redis同步模块，提供可选的渠道数据备份和恢复功能

### 关键数据结构
- `Config`（渠道）: 渠道配置（API Key、URL、优先级、支持的模型列表、模型重定向映射）
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
1. 解析请求体，提取原始请求的模型名称
2. 检查渠道的模型重定向配置，如果存在映射则替换为实际模型
3. 构建上游请求URL，合并查询参数
4. 复制请求头，跳过授权相关头，覆盖`x-api-key`
5. 如果模型发生重定向，修改请求体中的model字段
6. 发送POST请求到上游API（使用优化的HTTP客户端连接池）
7. 处理响应：2xx响应支持流式转发（64KB缓冲区），其他响应读取完整body
8. 异步记录日志到队列（始终记录原始模型，确保可追溯性）

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
- `REDIS_URL`: Redis连接URL（可选，用于渠道数据同步备份）

支持 `.env` 文件配置（优先于系统环境变量）

### API身份验证系统
- **管理界面**: 基于Session的认证，24小时有效期
- **API端点**: 当设置`CCLOAD_AUTH`时，`/v1/messages`需要`Authorization: Bearer <token>`
- **安全特性**: HttpOnly Cookie、SameSite保护、自动过期清理

## 数据库架构和迁移

### 核心表结构
- **channels**: 渠道配置（id, name, api_key, url, priority, models, model_redirects, enabled, timestamps）
  - `name`字段具有UNIQUE约束（通过`idx_channels_unique_name`索引实现）
  - `model_redirects`字段：JSON格式存储模型重定向映射（请求模型 → 实际转发模型）
- **logs**: 请求日志（id, time, model, channel_id, status_code, message, performance_metrics）
  - `model`字段：始终记录客户端请求的原始模型，非重定向后的模型
- **cooldowns**: 冷却状态（channel_id, until, duration_ms）
- **rr**: 轮询指针（key="model|priority", idx）

### 向后兼容的数据库迁移

项目实现了智能的数据库架构升级机制，确保向后兼容：

**UNIQUE约束迁移** (`ensureChannelNameUnique` in sqlite_store.go):
1. **清理旧索引**: `DROP INDEX IF EXISTS idx_channels_name`
2. **幂等检查**: 检查`idx_channels_unique_name`是否已存在，存在则跳过
3. **数据修复**: 查找重复name，自动重命名为`原name+id`格式
4. **创建约束**: `CREATE UNIQUE INDEX idx_channels_unique_name ON channels (name)`

**迁移特性**:
- **自动执行**: 服务启动时自动运行，无需手动干预
- **数据保护**: 重复数据自动重命名而非删除（如`api-1`变成`api-12`, `api-14`）
- **幂等操作**: 支持重复执行，不会产生副作用
- **KISS原则**: 简化的四步流程，代码简洁可靠

### 性能优化索引
- `idx_logs_time`: 日志时间索引，优化时间范围查询
- `idx_channels_unique_name`: 渠道名称UNIQUE索引，确保数据唯一性
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
GET         /admin/channels/export     # 导出渠道配置为CSV
POST        /admin/channels/import     # 从CSV导入渠道配置
GET         /admin/errors         # 请求日志列表（支持分页和过滤）
GET         /admin/stats          # 调用统计数据
GET         /admin/metrics        # 趋势数据（支持hours和bucket_min参数）
```

## 模型重定向功能

### 功能概述

模型重定向允许将客户端请求的模型自动映射到实际转发的模型，无需客户端修改代码。

**使用场景**:
- **模型升级迁移**: 将旧模型请求自动重定向到新模型（如 opus → sonnet-3.5）
- **成本优化**: 将高成本模型请求重定向到性价比更高的模型
- **A/B测试**: 灵活切换不同模型进行对比测试
- **渠道兼容**: 某些渠道不支持特定模型时，自动映射到支持的模型

### 配置方式

**Web界面配置**:
1. 访问 `/web/channels.html`
2. 创建或编辑渠道时，在"模型重定向"字段填入JSON格式映射
3. 格式示例：`{"claude-3-opus-20240229":"claude-3-5-sonnet-20241022"}`

**API配置**:
```bash
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude-Redirect",
    "api_key": "sk-ant-xxx",
    "url": "https://api.anthropic.com",
    "priority": 10,
    "models": ["claude-3-opus-20240229", "claude-3-5-sonnet-20241022"],
    "model_redirects": {
      "claude-3-opus-20240229": "claude-3-5-sonnet-20241022"
    },
    "enabled": true
  }'
```

### 工作原理

1. **请求解析**: 代理接收客户端请求，解析出原始模型名称
2. **重定向检查**: 查询渠道的 `model_redirects` 映射
3. **模型替换**: 如果存在映射，修改请求体中的 `model` 字段为目标模型
4. **上游转发**: 使用替换后的模型向上游API发送请求
5. **日志记录**: 始终记录原始模型名称（而非重定向后的模型），确保可追溯性

**重要特性**:
- **透明操作**: 客户端无感知，返回响应不包含重定向信息
- **可追溯性**: 日志中记录原始请求模型，便于统计和调试
- **向后兼容**: 不配置重定向时功能完全不影响现有行为
- **灵活配置**: 每个渠道可独立配置不同的重定向规则

### 数据格式

**JSON格式要求**:
```json
{
  "请求模型1": "实际转发模型1",
  "请求模型2": "实际转发模型2"
}
```

**数据库存储**:
- 字段：`model_redirects TEXT DEFAULT '{}'`
- 序列化：使用 `sonic.Marshal` 高性能JSON库
- 反序列化：`parseModelRedirectsJSON` 函数自动处理空值和格式验证

### 测试验证

项目包含完整的测试套件验证模型重定向功能：

**单元测试** (`model_redirect_test.go`):
- JSON序列化/反序列化测试
- 数据库CRUD操作测试
- 向后兼容性测试
- 性能基准测试

**集成测试** (`model_redirect_integration_test.go`):
- 代理转发重定向验证
- API端点测试（创建、更新渠道）
- CSV导入导出测试
- 错误处理和日志记录测试

运行测试：
```bash
# 运行所有模型重定向相关测试
go test -v -run "TestModelRedirect"

# 运行特定测试
go test -v -run "TestModelRedirectProxyIntegration/重定向opus到sonnet"
```

## 渠道数据管理

### CSV导入导出功能

项目支持批量管理渠道配置，通过CSV格式进行导入导出：

**导出功能** (`/admin/channels/export`):
- 导出所有渠道配置为CSV文件
- 包含完整渠道信息：名称、API Key、URL、优先级、支持模型、启用状态
- 文件名格式：`channels-YYYYMMDD-HHMMSS.csv`
- 支持UTF-8编码，Excel兼容

**导入功能** (`/admin/channels/import`):
- 支持从CSV文件批量导入渠道配置
- 智能列名映射（支持中英文列名）
- 数据验证和错误提示
- 支持增量导入和覆盖更新

**CSV格式示例**:
```csv
name,api_key,url,priority,models,enabled
Claude-API-1,sk-ant-xxx,https://api.anthropic.com,10,"[\"claude-3-sonnet-20240229\"]",true
Claude-API-2,sk-ant-yyy,https://api.anthropic.com,5,"[\"claude-3-opus-20240229\"]",true
```

**列名映射支持**:
- `name/名称` → 渠道名称
- `api_key/密钥/API密钥` → API密钥
- `url/地址/URL` → API地址
- `priority/优先级` → 优先级（数字）
- `models/模型/支持模型` → 支持的模型列表（JSON数组字符串）
- `model_redirects/模型重定向` → 模型重定向映射（JSON对象字符串）
- `enabled/启用/状态` → 启用状态（true/false）

**使用方式**:
- **Web界面**: 访问`/web/channels.html`，使用"导出CSV"和"导入CSV"按钮
- **API调用**:
  ```bash
  # 导出
  curl -H "Cookie: session=xxx" http://localhost:8080/admin/channels/export > channels.csv

  # 导入
  curl -X POST -H "Cookie: session=xxx" \
    -F "file=@channels.csv" \
    http://localhost:8080/admin/channels/import
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
- 智能识别API类型，自动设置正确的认证头：
  - **Claude API** (`/v1/messages` 等)：设置 `x-api-key` 和 `Authorization: Bearer`
  - **Gemini API** (`/v1beta/*`)：仅设置 `x-goog-api-key`
- 客户端需自行设置 `anthropic-version`（Claude）或其他API特定头
- 2xx 响应支持流式转发，使用 64KB 缓冲区
- 模型重定向在请求体层面操作，对客户端完全透明

**模型重定向注意事项**:
- 日志中始终记录客户端请求的原始模型，而非重定向后的模型
- 确保目标模型在渠道的 `models` 列表中，否则可能导致上游错误
- 重定向配置为JSON格式，必须是有效的对象（非数组或其他类型）
- 空的重定向配置会被序列化为 `{}`，不影响功能

**安全考虑**:
- 生产环境必须设置强密码 `CCLOAD_PASS`
- 建议设置 `CCLOAD_AUTH` 以保护 `/v1/messages` 端点
- API Key不记录到日志中，仅在内存中使用
- 生产环境需限制 `data/` 目录访问权限
- 使用 HTTPS 部署以保护传输中的认证令牌

## Redis同步功能

### 功能概述
ccLoad现已支持可选的Redis同步功能，用于渠道配置的备份和恢复：

**核心特性**:
- **可选启用**: 设置`REDIS_URL`环境变量启用，未设置则使用纯SQLite模式
- **实时同步**: 渠道增删改操作自动同步到Redis
- **启动恢复**: 数据库文件不存在时自动从Redis恢复渠道配置
- **故障隔离**: Redis操作失败不影响核心功能
- **数据一致性**: 使用事务确保SQLite和Redis数据同步

### Redis数据结构
- **Key格式**: `ccload:channels` (Hash类型)
- **Field**: 渠道名称 (确保唯一性)
- **Value**: JSON序列化的完整渠道配置

### 使用场景
1. **多实例部署**: 不同实例间共享渠道配置
2. **数据备份**: Redis作为渠道配置的实时备份
3. **快速恢复**: 新环境快速从Redis恢复配置
4. **配置同步**: 开发、测试、生产环境配置同步

### 配置示例
```bash
# 启用Redis同步
export REDIS_URL="redis://localhost:6379"
# 或使用密码认证
export REDIS_URL="redis://user:password@localhost:6379/0"
# 或使用TLS
export REDIS_URL="rediss://user:password@redis.example.com:6380/0"

# 测试Redis功能
go run . test-redis
```

### 启动行为
- **数据库不存在 + Redis启用**: 从Redis恢复渠道配置到SQLite
- **数据库存在 + Redis启用**: 同步SQLite中的渠道配置到Redis
- **Redis未配置**: 使用纯SQLite模式，无同步功能

## 测试架构

### 测试文件组织

项目采用测试金字塔架构，确保代码质量和功能可靠性：

**单元测试** (60%):
- `model_redirect_test.go`: 模型重定向序列化、数据库CRUD、向后兼容性
- 聚焦于函数级别的逻辑验证
- 快速执行，无外部依赖

**集成测试** (30%):
- `model_redirect_integration_test.go`: API端点、代理转发、错误处理
- `integration_test.go`: 基础集成测试
- `redis_test.go`: Redis同步功能测试
- `import_redis_test.go`: CSV导入与Redis集成
- `simple_import_test.go`: CSV导入基础功能
- 验证组件间协作

**端到端测试** (10%):
- 完整请求流程测试（客户端 → 代理 → 上游API → 响应）

### 测试最佳实践

**异步操作处理**:
```go
// ❌ 错误：固定等待时间，不可靠
time.Sleep(100 * time.Millisecond)
logs, _ := store.ListLogs(ctx, ...)

// ✅ 正确：重试循环，适应不同系统负载
var logs []*LogEntry
for i := 0; i < 10; i++ {
    time.Sleep(200 * time.Millisecond)
    logs, err = store.ListLogs(ctx, ...)
    if len(logs) > 0 {
        break
    }
}
```

**API响应解析**:
```go
// ✅ 处理泛型APIResponse包装器
var response struct {
    Success bool   `json:"success"`
    Data    Config `json:"data"`
    Error   string `json:"error,omitempty"`
}
sonic.Unmarshal(w.Body.Bytes(), &response)
created := response.Data
```

**测试隔离**:
- 每个测试使用独立的临时数据库（`t.TempDir()`）
- 清理测试服务器和资源（`defer server.Close()`）
- 避免全局状态污染

### 性能基准测试

项目包含性能基准测试，用于优化关键路径：

```bash
# 运行所有基准测试
go test -bench=. -benchmem

# 运行特定基准测试
go test -bench=BenchmarkSerializeModelRedirects -benchmem
go test -bench=BenchmarkParseModelRedirectsJSON -benchmem
```

示例输出：
```
BenchmarkSerializeModelRedirects-8    500000    2.5 ns/op    0 B/op    0 allocs/op
BenchmarkParseModelRedirectsJSON-8    300000    4.2 ns/op    128 B/op  2 allocs/op
```

## 技术栈

- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.14.32（嵌入式）
- **缓存**: Ristretto v2.3.0（内存缓存）
- **Redis客户端**: go-redis v9.7.0（可选同步功能）
- **JSON**: Sonic v1.14.1（高性能JSON库）
- **环境配置**: godotenv v1.5.1
- **前端**: 原生HTML/CSS/JavaScript（无框架依赖）
- **测试**: Go标准testing包 + httptest

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

## 调试和故障排除

### 常见开发问题

**端口被占用**:
```bash
# 查找占用8080端口的进程
lsof -i :8080
# 终止进程
kill -9 <PID>
```

**SQLite数据库锁定**:
```bash
# 检查数据库状态
sqlite3 data/ccload.db ".timeout 3000"
# 清理WAL文件（服务停止时）
rm -f data/ccload.db-wal data/ccload.db-shm
```

**容器调试**:
```bash
# 查看容器日志
docker logs ccload -f
# 进入容器调试
docker exec -it ccload /bin/sh
# 检查健康状态
docker inspect ccload --format='{{.State.Health.Status}}'
```

**性能监控**:
- 管理界面：`http://localhost:8080/web/trend.html` 查看趋势图
- 日志分析：`http://localhost:8080/web/logs.html` 查看请求日志
- API统计：`GET /admin/stats` 获取统计数据

### 配置验证
```bash
# 检查环境变量
env | grep CCLOAD
# 验证数据库连接
sqlite3 data/ccload.db "SELECT COUNT(*) FROM channels;"
# 测试API端点
curl -s http://localhost:8080/public/summary | jq
```

## 多Key支持功能

### 功能概述

从v1.0开始，ccLoad支持为单个渠道配置多个API Key，实现更细粒度的故障切换和负载均衡。

**核心特性**：
- **多Key配置**：在单个渠道中使用逗号分割配置多个API Key
- **Key级别冷却**：每个Key独立冷却，不影响同渠道其他Key的可用性
- **灵活策略**：支持顺序访问（sequential）和轮询访问（round_robin）两种模式
- **向后兼容**：单Key场景完全兼容旧版本，无需修改配置

### 使用场景

1. **提高可用性**：单个Key被限流时自动切换到备用Key
2. **负载均衡**：使用轮询策略均匀分配请求到多个Key
3. **成本优化**：合理利用多个Key的额度限制
4. **灵活扩展**：无需创建多个渠道即可实现Key级别管理

### 配置方式

**Web界面配置**：
1. 访问 `/web/channels.html` 渠道管理页面
2. 在"API Key"字段输入多个Key，用英文逗号分隔：
   ```
   sk-ant-key1,sk-ant-key2,sk-ant-key3
   ```
3. 选择"Key使用策略"：
   - **顺序访问**（默认）：按顺序尝试，失败时切换到下一个
   - **轮询访问**：请求均匀分配到所有可用Key

**API配置示例**：
```bash
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

### 工作原理

**顺序访问策略（sequential）**：
1. 从第一个Key开始尝试
2. 如果Key失败或冷却中，自动切换到下一个可用Key
3. 所有Key都不可用时返回错误

**轮询访问策略（round_robin）**：
1. 使用轮询指针均匀分配请求
2. 自动跳过冷却中的Key
3. 轮询状态持久化，服务重启后保持

**Key级别冷却机制**：
- **触发条件**：Key返回错误或非2xx响应
- **冷却时长**：指数退避（1s → 2s → 4s → ... → 最大30分钟）
- **独立冷却**：每个Key的冷却状态互不影响
- **自动恢复**：Key成功响应后立即重置冷却状态

### 数据库架构

新增表结构支持Key级别管理：

```sql
-- Key级别冷却表
CREATE TABLE key_cooldowns (
  channel_id INTEGER NOT NULL,
  key_index INTEGER NOT NULL,
  until TIMESTAMP NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(channel_id, key_index)
);

-- Key轮询指针表
CREATE TABLE key_rr (
  channel_id INTEGER PRIMARY KEY,
  idx INTEGER NOT NULL
);
```

渠道表新增字段：
- `api_keys`：JSON数组存储多个Key（优先级高于`api_key`）
- `key_strategy`：Key使用策略（`sequential` | `round_robin`）

### 向后兼容性

**单Key场景**：
- 继续使用`api_key`字段，无需修改
- 自动识别为单Key模式，不触发多Key逻辑
- 性能与旧版本完全一致（YAGNI原则）

**旧数据迁移**：
- 数据库自动添加新字段（默认值兼容）
- `api_key`字段支持逗号分割，自动解析为多Key
- 前端界面兼容新旧两种配置方式

### 监控和调试

**查看Key冷却状态**：
```bash
# 查询特定渠道的Key冷却信息
sqlite3 data/ccload.db \
  "SELECT channel_id, key_index, until, duration_ms FROM key_cooldowns WHERE channel_id = 1;"
```

**日志跟踪**：
- 日志中记录使用的Key索引（脱敏处理）
- 错误日志包含"channel keys unavailable"标识
- 成功日志不暴露具体Key内容

### 测试验证

项目包含完整的测试套件：

```bash
# 运行多Key功能测试
go test -v -run "TestKeySelector"

# 覆盖测试场景：
# - 单Key兼容性
# - 顺序访问策略
# - 轮询访问策略
# - 全Key冷却场景
# - 指数退避验证
```

### 最佳实践

1. **Key数量**：建议配置2-3个Key，平衡可用性与管理复杂度
2. **策略选择**：
   - 备用场景：使用顺序策略，主Key失败时切换备用
   - 负载均衡：使用轮询策略，平均分配请求负载
3. **监控**：定期检查日志中的"keys unavailable"错误，及时补充Key
4. **安全**：使用环境变量或配置文件管理Key，不要硬编码
## API兼容性支持

### 支持的API类型

ccLoad现已支持多种AI API的透明代理，通过智能路径检测自动适配不同API的认证方式：

#### Claude API（Anthropic）
- **路径特征**：`/v1/messages`、`/v1/complete` 等非 `/v1beta/` 路径
- **认证头设置**：
  ```
  x-api-key: <API_KEY>
  Authorization: Bearer <API_KEY>
  ```
- **客户端要求**：需自行设置 `anthropic-version` 头（如 `2023-06-01`）
- **示例请求**：
  ```bash
  curl -X POST http://localhost:8080/v1/messages \
    -H "Content-Type: application/json" \
    -d '{"model":"claude-3-5-sonnet-20241022","messages":[...],"max_tokens":1024}'
  ```

#### Gemini API（Google）
- **路径特征**：包含 `/v1beta/` 的路径
- **认证头设置**：
  ```
  x-goog-api-key: <API_KEY>
  ```
  注意：**不**发送 `x-api-key` 和 `Authorization` 头
- **典型路径格式**：
  ```
  /v1beta/models/{model}:streamGenerateContent?alt=sse
  /v1beta/models/{model}:generateContent
  ```
- **示例请求**：
  ```bash
  curl -X POST "http://localhost:8080/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" \
    -H "Content-Type: application/json" \
    -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'
  ```

### 路径检测逻辑

**实现位置**：`proxy.go:isGeminiRequest(path string) bool`

**检测规则**：
- 使用 `strings.Contains(path, "/v1beta/")` 检测路径
- 大小写敏感（`/v1beta/` 不匹配 `/V1BETA/`）
- 适用于所有包含该子串的路径（如 `/api/v1beta/test` 也被识别为Gemini）

**性能特点**：
- 单次检测耗时 ~2-3ns（基准测试验证）
- 零额外内存分配
- 对代理性能影响可忽略

### 测试覆盖

项目包含完整的API兼容性测试套件：

**单元测试** (`proxy_api_test.go`):
```bash
# 路径检测测试（9个测试用例）
go test -v -run TestIsGeminiRequest

# 性能基准测试
go test -bench=BenchmarkIsGeminiRequest -benchmem
```

**集成测试** (`goog_api_key_test.go`):
```bash
# Claude API头设置验证
go test -v -run TestGoogAPIKeyUpstream

# Gemini API头设置验证
go test -v -run TestGeminiRequestHeaders
```

### 扩展新API

如需支持新的API类型（如OpenAI、Azure等），按以下步骤扩展：

1. **添加检测函数**（proxy.go）：
   ```go
   func isOpenAIRequest(path string) bool {
       return strings.HasPrefix(path, "/v1/chat/completions")
   }
   ```

2. **修改头设置逻辑**（proxy.go:166-174）：
   ```go
   if isGeminiRequest(requestPath) {
       req.Header.Set("x-goog-api-key", apiKey)
   } else if isOpenAIRequest(requestPath) {
       req.Header.Set("Authorization", "Bearer "+apiKey)
   } else {
       // Claude默认逻辑
       req.Header.Set("x-api-key", apiKey)
       req.Header.Set("Authorization", "Bearer "+apiKey)
   }
   ```

3. **添加测试**（新建或扩展 `*_test.go`）：
   - 路径检测单元测试
   - 头设置集成测试
   - 完整请求-响应端到端测试

### 设计原则

**KISS（Keep It Simple）**：
- 使用简单字符串匹配，无需正则表达式
- 路径检测函数单一职责，易于测试和维护

**性能优先**：
- 避免反射和复杂逻辑
- 快速路径检测不影响代理性能

**向后兼容**：
- Claude API作为默认行为，确保现有用户无感知
- 新API通过显式路径特征识别，不影响其他请求
