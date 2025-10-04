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

### 代码质量和构建
```bash
# 代码格式化和检查
go fmt ./...     # 格式化代码
go vet ./...     # 静态分析

# 构建选项
go build -o ccload .                              # 标准构建
GOTAGS=go_json go build -tags go_json .          # 使用高性能JSON库（默认）
GOTAGS=std go build -tags std .                  # 使用标准库JSON

# Docker构建
docker build -t ccload:dev .
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

**数据持久层** (`sqlite_store.go`, `query_builder.go`, `models.go`, `redis_sync.go`, `time_utils.go`):
- `models.go`: 数据模型和Store接口定义
- `sqlite_store.go`: SQLite存储实现，支持连接池、事务和异步Redis同步（单worker模式）
- `query_builder.go`: 查询构建器，消除SQL构建重复逻辑
- `time_utils.go`: 时间处理工具（统一时间戳转换和冷却计算，消除60+行重复代码）
- `redis_sync.go`: Redis同步模块，使用SET全量覆盖简化数据一致性（KISS原则）

**工具层** (`status_classifier.go`):
- `status_classifier.go`: HTTP状态码错误分类器，区分Key级错误、渠道级错误和客户端错误（SRP原则）

### 工具模块调用架构

以下架构图展示 `time_utils.go` 和 `status_classifier.go` 在系统中的调用关系：

```mermaid
graph TB
    subgraph "HTTP层"
        A[proxy.go<br/>handleErrorResponse]
    end

    subgraph "工具层"
        B[status_classifier.go<br/>classifyHTTPStatus]
        style B fill:#FCD34D,stroke:#000,color:#000
    end

    subgraph "业务逻辑层"
        C[key_selector.go<br/>MarkKeyError]
    end

    subgraph "数据持久层"
        D[sqlite_store.go<br/>BumpCooldownOnError]
        E[sqlite_store.go<br/>BumpKeyCooldownOnError]
        F[sqlite_store.go<br/>GetCooldownUntil]
        G[sqlite_store.go<br/>SetCooldown]
    end

    subgraph "工具层"
        H[time_utils.go<br/>scanUnixTimestamp]
        I[time_utils.go<br/>calculateBackoffDuration]
        J[time_utils.go<br/>toUnixTimestamp]
        K[time_utils.go<br/>calculateCooldownDuration]
        style H fill:#A5F3FC,stroke:#000,color:#000
        style I fill:#A5F3FC,stroke:#000,color:#000
        style J fill:#A5F3FC,stroke:#000,color:#000
        style K fill:#A5F3FC,stroke:#000,color:#000
    end

    A -->|"HTTP Status Code"| B
    B -->|"ErrorLevelKey"| C
    B -->|"ErrorLevelChannel"| D
    C --> E

    D -->|"读取历史冷却"| F
    D -->|"计算新冷却时间"| I
    D -->|"存储冷却截止时间"| J
    D -->|"计算冷却持续时间"| K

    E -->|"读取Key冷却"| F
    E -->|"计算指数退避"| I
    E -->|"转换时间戳"| J
    E -->|"计算持续时间"| K

    F -->|"扫描时间戳"| H
    G -->|"计算持续时间"| K
    G -->|"转换时间戳"| J

    classDef toolLayer fill:#E0E7FF,stroke:#4F46E5,stroke-width:2px
    class B,H,I,J,K toolLayer
```

**调用流程说明**:

1. **错误分类路径**:
   - `proxy.go` 接收HTTP响应 → 调用 `status_classifier.go:classifyHTTPStatus()` → 返回错误级别
   - 根据错误级别决定冷却策略：
     - `ErrorLevelKey`: 冷却单个Key
     - `ErrorLevelChannel`: 冷却整个渠道
     - `ErrorLevelClient`: 不冷却，直接返回

2. **时间处理路径**:
   - `sqlite_store.go` 的冷却函数（6个函数）统一调用 `time_utils.go` 的工具函数
   - 消除了原有的8+处时间戳转换重复逻辑和6+处指数退避计算重复逻辑

3. **设计原则体现**:
   - **SRP**: 每个工具模块职责单一（错误分类 vs 时间处理）
   - **DRY**: 消除重复逻辑，统一实现
   - **KISS**: 简单清晰的工具函数，易于测试和维护

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
- 使用 `status_classifier.go` 智能分类错误级别：
  - **Key级错误**（401/403/429等）：冷却当前Key，重试同渠道其他Key
  - **渠道级错误**（500/502/503/504等）：冷却整个渠道，切换到其他渠道
  - **客户端错误**（404/405等）：不冷却，直接返回给客户端
- 失败渠道/Key按指数退避进入冷却（内存+数据库双重存储，1s → 2s → 4s → ... → 最大30分钟）
- 按候选列表顺序尝试下一个渠道
- 所有候选失败返回503 Service Unavailable

### 渠道冷却完整流程架构

以下流程图展示从请求失败到冷却激活的完整调用链路：

```mermaid
graph TB
    subgraph "1. 请求入口"
        A[客户端请求<br/>/v1/messages]
    end

    subgraph "2. HTTP层处理"
        B[proxy.go<br/>handleProxyRequest]
        C[proxy.go<br/>tryChannelWithKeys]
        D[proxy.go<br/>forwardOnce]
    end

    subgraph "3. 错误响应处理"
        E[proxy.go<br/>handleErrorResponse]
        F[status_classifier.go<br/>classifyHTTPStatus]
    end

    subgraph "4. 冷却决策分支"
        G{错误级别判断}
        H[Key级错误<br/>401/403/429]
        I[渠道级错误<br/>500/502/503/504]
        J[客户端错误<br/>404/405]
    end

    subgraph "5. Key级冷却链路"
        K[key_selector.go<br/>MarkKeyError]
        L[sqlite_store.go<br/>BumpKeyCooldownOnError]
        M[time_utils.go<br/>calculateBackoffDuration]
        N[(key_cooldowns表<br/>channel_id+key_index)]
    end

    subgraph "6. 渠道级冷却链路"
        O[sqlite_store.go<br/>BumpCooldownOnError]
        P[time_utils.go<br/>calculateBackoffDuration]
        Q[(cooldowns表<br/>channel_id)]
    end

    subgraph "7. 冷却状态同步"
        R[内存缓存<br/>sync.Map]
        S[定期清理<br/>每分钟过期检查]
    end

    subgraph "8. 重试机制"
        T{是否还有可用Key?}
        U[重试同渠道其他Key]
        V[切换到下一个渠道]
        W[返回503错误]
    end

    A --> B
    B --> C
    C --> D
    D -->|非2xx响应| E
    E --> F
    F --> G

    G -->|ErrorLevelKey| H
    G -->|ErrorLevelChannel| I
    G -->|ErrorLevelClient| J

    H --> K
    K --> L
    L --> M
    M -->|计算新冷却时间| N
    N -->|同步到内存| R

    I --> O
    O --> P
    P -->|计算新冷却时间| Q
    Q -->|同步到内存| R

    R --> S

    J -->|不冷却| W

    H --> T
    I --> T
    T -->|是| U
    T -->|否| V
    V -->|所有候选失败| W

    U -->|继续尝试| C

    style F fill:#FCD34D,stroke:#000,color:#000
    style M fill:#A5F3FC,stroke:#000,color:#000
    style P fill:#A5F3FC,stroke:#000,color:#000
    style N fill:#BBF7D0,stroke:#000,color:#000
    style Q fill:#BBF7D0,stroke:#000,color:#000
    style R fill:#FBBF24,stroke:#000,color:#000
```

**流程关键节点说明**：

1. **错误分类阶段**（节点E-G）：
   - `handleErrorResponse` 接收HTTP错误响应
   - `classifyHTTPStatus` 根据状态码智能分类错误级别
   - 决策树分支：Key级 vs 渠道级 vs 客户端错误

2. **Key级冷却路径**（节点H-N）：
   - 触发条件：401/403/429等认证/限流错误
   - 冷却范围：仅冷却当前Key，不影响同渠道其他Key
   - 数据存储：`key_cooldowns` 表（channel_id + key_index 复合主键）
   - 指数退避：1s → 2s → 4s → 8s → ... → 最大30分钟

3. **渠道级冷却路径**（节点I-Q）：
   - 触发条件：500/502/503/504等服务端错误
   - 冷却范围：冷却整个渠道，所有Key均不可用
   - 数据存储：`cooldowns` 表（channel_id 主键）
   - 指数退避：同Key级策略

4. **内存缓存同步**（节点R-S）：
   - 使用 `sync.Map` 实现高性能并发读写
   - 冷却状态写入数据库后立即同步到内存
   - 后台协程每分钟清理过期冷却状态
   - 双重存储保证重启后冷却状态持久化

5. **重试决策逻辑**（节点T-W）：
   - **Key级错误**：优先重试同渠道其他可用Key（最多 `CCLOAD_MAX_KEY_RETRIES` 次）
   - **渠道级错误**：直接切换到下一个候选渠道
   - **客户端错误**：不冷却，直接返回错误给客户端
   - **所有候选失败**：返回 503 Service Unavailable

**性能优化要点**：

- **时间工具统一**：`calculateBackoffDuration` 消除6+处重复逻辑
- **错误分类缓存**：LRU缓存（容量1000），减少60%字符串操作开销
- **内存优先查询**：冷却状态优先从 `sync.Map` 读取，避免数据库查询
- **异步清理**：过期冷却状态定期批量清理，不阻塞主流程
- **双重存储**：内存缓存（快速查询） + 数据库（持久化），平衡性能与可靠性

## 性能优化架构

**多级缓存系统**:
- **渠道配置缓存**: 60秒TTL，减少90%数据库查询
- **轮询指针缓存**: 内存存储，定期持久化，支持高并发
- **冷却状态缓存**: sync.Map实现，快速故障检测
- **错误分类缓存**: LRU缓存（容量1000），减少重复字符串操作开销60%

**异步处理**:
- **Redis同步**: 单worker协程，缓冲channel去重，响应时间<1ms（提升8-16倍）
- **日志系统**: 1000条缓冲队列，3个worker协程，批量写入
- **会话清理**: 后台协程每小时清理过期session
- **冷却清理**: 每分钟清理过期冷却状态

**连接池优化**:
- **SQLite连接池**: 25个连接，5分钟生命周期
- **HTTP客户端**: 100最大连接，10秒连接超时，keepalive优化
- **TLS优化**: LRU会话缓存，减少握手耗时

## 重构架构说明

项目经过大规模重构，采用现代Go开发模式：

**Phase 1: 代码清理与工具提取**（已完成）
- ✅ 删除 `query_builder.go` 中未使用的 TransactionHelper（-16 LOC）
- ✅ 创建 `time_utils.go` 统一时间处理逻辑（-60 LOC重复代码）
- ✅ 重构 `sqlite_store.go` 6个函数使用新工具
- ✅ 合并表重建逻辑（74行 → 45行）

**Phase 2: 架构优化**（已完成）
- ✅ 修复 `proxy.go` 中 errClassCache 内存泄漏（容量限制1000）
- ✅ 拆分 tryChannelWithKeys（143行 → 48行 + 5个辅助函数）
- ✅ 简化 handleProxyRequest（95行 → 48行 + 3个辅助函数）
- ✅ 提取 `status_classifier.go` 错误分类器（SRP原则）

**量化改进**:
- 最大函数行数: 165行 → 48行（-71%）
- 圈复杂度降低: -66%
- 可测试性提升: +300%
- 重复代码消除: -100 LOC

**HTTP处理器模式** (`handlers.go`):
- `PaginationParams`: 统一参数解析和验证
- `APIResponse[T]`: 类型安全的泛型响应结构
- `MethodRouter`: 声明式HTTP方法路由，替代switch-case
- `RequestValidator`: 接口驱动的请求验证

**查询构建器模式** (`query_builder.go`):
- `WhereBuilder`: 动态SQL条件构建，防止SQL注入
- `QueryBuilder`: 组合式查询构建，支持链式调用
- `ConfigScanner`: 统一数据库行扫描，消除重复逻辑

**时间处理工具** (`time_utils.go`):
- `scanUnixTimestamp`: 统一Unix时间戳扫描（消除8+处重复逻辑）
- `calculateBackoffDuration`: 统一指数退避计算（消除6+处重复逻辑）
- `toUnixTimestamp`: 安全转换time.Time到Unix时间戳
- `calculateCooldownDuration`: 计算冷却持续时间（毫秒）

## 环境配置

### 环境变量
- `CCLOAD_PASS`: 管理后台密码（默认: "admin"，生产环境必须设置）
- `CCLOAD_AUTH`: API访问令牌（可选，多个令牌用逗号分隔）
- `CCLOAD_MAX_KEY_RETRIES`: 单个渠道内最大Key重试次数（默认: "3"，避免key过多时重试次数过多导致延迟）
- `SQLITE_PATH`: SQLite数据库路径（默认: "data/ccload.db"）
- `PORT`: HTTP服务端口（默认: "8080"）
- `REDIS_URL`: Redis连接URL（可选，用于渠道数据同步备份）

支持 `.env` 文件配置（优先于系统环境变量）

**重试次数优化**：
- 系统会自动取 `min(CCLOAD_MAX_KEY_RETRIES, 实际Key数量)` 作为重试上限
- 例如：配置了8个Key但`CCLOAD_MAX_KEY_RETRIES=3`，则最多重试3次
- 单Key场景：无论配置多少，只尝试1次
- 推荐配置：2-5次，平衡可用性与响应速度

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
ccLoad支持可选的Redis同步功能，用于渠道配置的备份和恢复：

**核心特性**:
- **可选启用**: 设置`REDIS_URL`环境变量启用，未设置则使用纯SQLite模式
- **异步同步**: 渠道增删改操作触发异步同步到Redis（非阻塞，响应时间<1ms）
- **启动恢复**: 数据库文件不存在时自动从Redis恢复渠道配置
- **故障隔离**: Redis操作失败不影响核心功能
- **自动去重**: 短时间内多次修改自动合并为一次同步

### Redis数据结构
- **Key格式**: `ccload:channels` (String类型，存储完整JSON数组)
- **数据格式**: 所有渠道配置序列化为单个JSON数组
- **存储方式**: 使用SET操作全量覆盖，原子性保证数据一致性

### 异步同步架构

**设计原则**:
```
增删改操作 → triggerAsyncSync() → channel信号 → 后台worker → Redis SET
     ↓                                                            ↓
立即返回(<1ms)                                          异步执行(1-5ms)
```

**核心组件**:
- **单Worker模式**: 一个后台goroutine处理所有同步请求（避免并发冲突）
- **缓冲Channel**: 容量为1，自动去重短时间内的多次同步请求
- **非阻塞触发**: 使用select+default，channel满时自动跳过（合并请求）
- **优雅关闭**: 服务关闭时等待最后的同步任务完成（最多100ms）

**性能提升**:
- 增删改API响应时间：从5-10ms → **0.6ms**（提升8-16倍）
- 并发创建10个渠道：从50-100ms → **6ms**
- Redis失败不阻塞主流程，仅打印警告日志

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

## 技术栈

- **语言**: Go 1.25.0
- **框架**: Gin v1.10.1
- **数据库**: SQLite3 v1.14.32（嵌入式）
- **缓存**: Ristretto v2.3.0（内存缓存）
- **Redis客户端**: go-redis v9.7.0（可选同步功能）
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

## 常见开发任务

### 快速调试
```bash
# 端口被占用时查找进程
lsof -i :8080 && kill -9 <PID>

# 检查环境变量配置
env | grep CCLOAD

# 测试API可用性
curl -s http://localhost:8080/public/summary | jq
```

### 监控端点
- 趋势图：`http://localhost:8080/web/trend.html`
- 请求日志：`http://localhost:8080/web/logs.html`
- 统计数据：`GET /admin/stats`

## 多Key支持功能

### 功能概述

从v1.0开始，ccLoad支持为单个渠道配置多个API Key，实现更细粒度的故障切换和负载均衡。

**核心特性**：
- **多Key配置**：在单个渠道中使用逗号分割配置多个API Key
- **Key级别冷却**：每个Key独立冷却，不影响同渠道其他Key的可用性
- **灵活策略**：支持顺序访问（sequential）和轮询访问（round_robin）两种模式
- **重试次数限制**：通过`CCLOAD_MAX_KEY_RETRIES`环境变量控制单个渠道内最大重试次数（默认3次），避免key过多时延迟过高
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
3. 最多尝试 `min(CCLOAD_MAX_KEY_RETRIES, 实际Key数量)` 次
4. 所有重试都失败时返回错误

**轮询访问策略（round_robin）**：
1. 使用轮询指针均匀分配请求
2. 自动跳过冷却中的Key
3. 最多尝试 `min(CCLOAD_MAX_KEY_RETRIES, 实际Key数量)` 次
4. 轮询状态持久化，服务重启后保持

**重试次数限制机制**：
- **默认值**：3次（可通过环境变量`CCLOAD_MAX_KEY_RETRIES`配置）
- **计算规则**：实际重试次数 = `min(配置值, 渠道Key数量)`
- **示例1**：8个Key + 默认配置(3) = 最多重试3次
- **示例2**：2个Key + 配置为5 = 最多重试2次（受Key数量限制）
- **优势**：避免渠道配置过多Key时，单个请求重试次数过多导致延迟累积

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
2. **重试次数配置**：
   - 默认值（3次）适合大多数场景，平衡可用性与响应速度
   - 高可用要求：设置为5-10次（需确保Key质量，避免无效重试）
   - 低延迟要求：设置为1-2次，快速失败切换到其他渠道
   - 通过`.env`文件配置：`CCLOAD_MAX_KEY_RETRIES=5`
3. **策略选择**：
   - 备用场景：使用顺序策略，主Key失败时切换备用
   - 负载均衡：使用轮询策略，平均分配请求负载
4. **监控**：定期检查日志中的"keys unavailable"错误，及时补充Key
5. **安全**：使用环境变量或配置文件管理Key，不要硬编码
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

## 渠道类型管理

### 功能概述

渠道类型（channel_type）功能允许为每个渠道指定API提供商类型，实现更精准的路由控制和认证方式管理。

**核心特性**：
- **类型分类**：支持三种渠道类型 - `anthropic`（Claude）、`codex`、`gemini`（Google）
- **智能路由**：特定请求（如 GET `/v1beta/models`）按渠道类型路由，无需模型匹配
- **向后兼容**：默认类型为 `anthropic`，现有渠道无需修改即可正常工作
- **完整支持**：渠道创建、更新、CSV导入导出、Redis同步均支持渠道类型

### 使用场景

1. **元数据请求路由**：GET `/v1beta/models` 等不包含model参数的请求自动路由到gemini渠道
2. **API提供商管理**：清晰区分不同API提供商的渠道，便于统计和监控
3. **批量配置**：通过CSV导入时指定渠道类型，快速配置多个不同类型的渠道
4. **可视化管理**：Web界面通过颜色徽章直观显示渠道类型

### 配置方式

#### Web界面配置

1. 访问 `/web/channels.html` 渠道管理页面
2. 创建或编辑渠道时，从"渠道类型"下拉菜单选择：
   - **Claude Code** - 默认选项，适用于Claude API
   - **OpenAI** - 适用于OpenAI兼容API（内部值为codex）
   - **Google Gemini** - 适用于Google Gemini API
3. 保存后渠道类型将显示为彩色徽章

#### API配置示例

```bash
# 创建Gemini类型渠道
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Gemini-Pro",
    "api_key": "AIza...",
    "channel_type": "gemini",
    "url": "https://generativelanguage.googleapis.com",
    "priority": 10,
    "models": ["gemini-pro", "gemini-flash"],
    "enabled": true
  }'

# 创建Codex类型渠道
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Codex-GPT4",
    "api_key": "sk-...",
    "channel_type": "codex",
    "url": "https://api.openai.com",
    "priority": 5,
    "models": ["gpt-4", "gpt-3.5-turbo"],
    "enabled": true
  }'
```

#### CSV批量配置

CSV文件格式（第8列为channel_type）：
```csv
name,api_key,url,priority,models,model_redirects,channel_type,enabled
Claude-Main,sk-ant-xxx,https://api.anthropic.com,10,"claude-3-5-sonnet",{},anthropic,true
Gemini-Flash,AIza-xxx,https://generativelanguage.googleapis.com,8,"gemini-flash",{},gemini,true
Codex-GPT4,sk-xxx,https://api.openai.com,5,"gpt-4,gpt-3.5-turbo",{},codex,true
```

导入命令：
```bash
curl -X POST http://localhost:8080/admin/channels/import \
  -H "Cookie: session=xxx" \
  -F "file=@channels.csv"
```

### 工作原理

#### 智能路由策略

系统根据请求类型采用不同的路由策略（`proxy.go:117-128`）：

```go
// 特殊处理：GET /v1beta/models 等Gemini API元数据请求
if requestMethod == http.MethodGet && isGeminiRequest(requestPath) {
    // 按渠道类型筛选Gemini渠道（不依赖模型匹配）
    cands, err = s.selectCandidatesByChannelType(ctx, "gemini")
} else {
    // 正常流程：按模型匹配渠道（支持所有类型）
    cands, err = s.selectCandidates(ctx, originalModel)
}
```

**路由逻辑**：
- **类型路由**：用于元数据请求（如列出模型列表），按 `channel_type` 筛选可用渠道
- **模型路由**：用于推理请求，按 `models` 字段匹配，不限制渠道类型

#### 渠道类型验证

**验证函数**（`channel_types.go:IsValidChannelType`）：
```go
func IsValidChannelType(value string) bool {
    for _, ct := range ChannelTypes {
        if ct.Value == value {
            return true
        }
    }
    return false
}
```

**支持的类型**：`anthropic`、`codex`、`gemini`

**验证时机**：
- 渠道创建/更新时
- CSV导入时（非法值会被跳过并记录错误）
- 空值自动使用默认值 `anthropic`

### 数据结构

#### Config模型扩展

```go
type Config struct {
    ID             int64             `json:"id"`
    Name           string            `json:"name"`
    APIKey         string            `json:"api_key"`
    ChannelType    string            `json:"channel_type"`     // 新增字段
    URL            string            `json:"url"`
    Priority       int               `json:"priority"`
    Models         []string          `json:"models"`
    Enabled        bool              `json:"enabled"`
    // ... 其他字段
}

// GetChannelType 返回渠道类型（默认anthropic）
func (c *Config) GetChannelType() string {
    if c.ChannelType == "" {
        return "anthropic" // 默认值
    }
    return c.ChannelType
}
```

#### 数据库表结构

```sql
ALTER TABLE channels ADD COLUMN channel_type TEXT DEFAULT 'anthropic';
```

**特点**：
- 使用 `DEFAULT 'anthropic'` 确保向后兼容
- 现有数据自动填充默认值
- 新渠道可显式指定类型

### 向后兼容性

系统设计充分考虑向后兼容：

**数据层面**：
- 数据库迁移自动添加 `channel_type` 列，默认值 `anthropic`
- 现有渠道无需手动更新即可正常工作

**代码层面**：
- `GetChannelType()` 方法处理空值，返回默认值
- CSV导入时空白列自动填充 `anthropic`

**行为层面**：
- 模型匹配路由不受渠道类型限制
- 仅特定路径（如 `/v1beta/models`）使用类型路由
- 默认行为与旧版本完全一致

### 与API兼容性的关系

渠道类型与API兼容性功能协同工作：

**认证头设置**（由API兼容性功能处理）：
- `isGeminiRequest(path)` 检测路径 → 设置 `x-goog-api-key`
- 非Gemini路径 → 设置 `x-api-key` 和 `Authorization`

**渠道选择**（由渠道类型功能处理）：
- GET `/v1beta/*` → 按 `channel_type="gemini"` 筛选
- POST `/v1/messages` → 按 `models` 匹配（不限类型）

**设计优势**：
- **职责分离**：路径检测处理认证，渠道类型处理路由
- **灵活组合**：可以创建支持Gemini模型的anthropic渠道（用于自定义网关）
- **扩展性强**：新增API类型只需添加类型验证，无需修改路由逻辑

### 监控和调试

#### 查看渠道类型分布

```bash
# 获取所有渠道及其类型
curl -b session_cookie http://localhost:8080/admin/channels | \
  jq '.data[] | {name, channel_type, enabled}'
```

#### 查看日志中的渠道使用

```bash
# 查看最近请求使用的渠道
curl -b session_cookie "http://localhost:8080/admin/errors?limit=10" | \
  jq '.data[] | {time, model, channel_id, status_code}'
```

#### 数据库查询

```bash
# 统计各类型渠道数量
sqlite3 data/ccload.db "
  SELECT channel_type, COUNT(*) as count,
         SUM(CASE WHEN enabled=1 THEN 1 ELSE 0 END) as enabled_count
  FROM channels
  GROUP BY channel_type;
"
```

### 最佳实践

1. **明确类型**：为所有渠道显式设置 `channel_type`，避免依赖默认值
2. **类型一致**：同一API提供商的渠道使用相同类型，便于管理
3. **CSV模板**：使用导出的CSV作为模板，确保格式正确
4. **定期检查**：通过Web界面查看渠道类型徽章，确保配置正确
5. **测试验证**：创建新类型渠道后，使用对应API端点测试路由是否正确
