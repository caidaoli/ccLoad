# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

ccLoad 是一个 Claude Code API 代理服务，使用 Go 构建。主要功能：

- **透明代理**：将 `/v1/messages` 请求转发到上游 Claude Code API，仅替换 API Key
- **智能路由**：基于模型支持、优先级和轮询策略选择渠道
- **故障切换**：失败时自动切换渠道并实施指数退避冷却（起始1秒，错误翻倍，封顶30分钟）
- **身份验证**：管理页面需要密码登录，支持session管理和自动过期
- **统计监控**：首页公开显示请求统计，管理界面提供详细的趋势和日志分析
- **前端管理**：提供现代化 Web 界面管理渠道、查看趋势、日志和调用统计

## 核心架构

### 主要组件
- `main.go`: 程序入口，初始化SQLite存储和HTTP服务器，支持.env文件读取
- `server.go`: HTTP服务器实现，路由注册、JSON工具函数、身份验证系统和性能优化组件
- `proxy.go`: 核心代理逻辑，处理`/v1/messages`请求转发和流式传输
- `selector.go`: 候选渠道选择算法（优先级+轮询），使用缓存优化
- `admin.go`: 管理API实现（渠道CRUD、请求日志、趋势数据、公开统计API）
- `middleware.go`: HTTP中间件（请求日志等）  
- `sqlite_store.go`: SQLite存储实现，管理渠道、冷却状态、日志和轮询指针
- `models.go`: 数据模型和Store接口定义
- `web/`: 前端静态文件（index.html、channels.html、trend.html、logs.html、stats.html、login.html）

### 关键数据结构
- `Config`（渠道）: 渠道配置（API Key、URL、优先级、支持的模型列表）
- `LogEntry`: 请求日志（时间、模型、渠道ID、状态码、消息）
- `Store` 接口: 数据持久化抽象层
- `MetricPoint`: 时间序列数据点（用于趋势分析）
- `StatsEntry`: 统计数据聚合（按渠道和模型分组）

### 核心算法
**候选选择策略** (`selectCandidates` in selector.go:18-80):
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

**性能优化特性**:
- **连接池优化**: SQLite连接数从1增加到25，HTTP客户端最大100连接
- **多级缓存**: 渠道配置缓存60秒，轮询指针内存化，冷却状态缓存
- **异步处理**: 日志批量写入，3个工作协程处理，1000条缓冲队列
- **流式传输**: 64KB缓冲区，减少系统调用次数

## 开发命令

### 构建和运行
```bash
# 开发环境运行（使用默认配置：端口8080，数据库data/ccload.db）
go run .

# 使用环境变量配置
CCLOAD_PASS=your_password SQLITE_PATH=./data/ccload.db PORT=8080 go run .

# 使用.env文件配置（推荐）
echo "CCLOAD_PASS=your_password" > .env
echo "SQLITE_PATH=./data/ccload.db" >> .env  
echo "PORT=8080" >> .env
go run .

# 构建生产版本
go build -o ccload .

# 构建到临时目录（避免污染工作空间）
go build -o /tmp/ccload .
```

### 依赖管理
```bash
go mod tidy      # 整理依赖
go mod verify    # 验证依赖
go mod download  # 下载依赖
```

### 代码格式化和测试
```bash
go fmt ./...     # 格式化代码
go vet ./...     # 静态检查
go test ./...    # 运行测试（当前项目暂无测试文件）
```

## 环境配置

### 环境变量
- `CCLOAD_PASS`: 管理后台密码（默认: "admin"，生产环境必须设置）
- `SQLITE_PATH`: SQLite数据库路径（默认: "data/ccload.db"）
- `PORT`: HTTP服务端口（默认: "8080"）

项目支持 `.env` 文件配置（优先于系统环境变量）

## API端点

### 公开端点（无需认证）
```
POST /v1/messages          # Claude API 透明代理
GET  /public/summary       # 基础统计数据
GET  /web/index.html       # 首页
GET  /web/login.html       # 登录页面
```

### 管理端点（需要登录）
```
GET/POST    /admin/channels       # 渠道列表和创建
GET/PUT/DEL /admin/channels/{id}  # 渠道详情、更新、删除
GET         /admin/errors         # 请求日志列表（支持分页）
GET         /admin/stats          # 调用统计数据
GET         /admin/metrics        # 趋势数据（支持hours和bucket_min参数）
GET         /web/channels.html    # 渠道管理页面
GET         /web/logs.html        # 请求日志页面
GET         /web/stats.html       # 调用统计页面
GET         /web/trend.html       # 趋势图表页面
```

## SQLite数据库架构

### 数据表结构
- **channels**: 渠道配置（id, name, api_key, url, priority, models, enabled, created_at, updated_at）
- **logs**: 请求日志（id, time, model, channel_id, status_code, message）
- **cooldowns**: 冷却状态（channel_id, until, duration_ms）
- **round_robin**: 轮询指针（model, priority, next_index）

### 重要注意事项

**透明转发原则**:
- 仅替换 `x-api-key` 头，其他请求头和请求体保持原样
- 客户端需自行设置 `anthropic-version` 等必需头
- 2xx 响应支持流式转发

**身份验证系统** (server.go:14-380):
- Session基于随机ID和内存存储，支持并发安全
- Cookie使用HttpOnly和SameSite保护
- 24小时会话有效期，每小时自动清理过期session
- 后台协程定期清理，避免内存泄漏

**性能优化架构**:
- **缓存层**: 渠道配置60秒缓存，减少90%数据库查询
- **异步日志**: 带缓冲的channel，3个worker协程批量处理
- **连接池**: SQLite(25连接) + HTTP(100连接)提升并发能力
- **内存优化**: sync.Map存储轮询指针和冷却状态，支持高并发
- **透明代理**: 保持零干预，不主动断开连接

**安全注意**:
- 生产环境必须设置强密码 `CCLOAD_PASS`
- API Key不记录到日志中，仅在内存中使用
- 生产环境需限制 `data/` 目录访问权限

## 前端架构

前后端分离设计，纯HTML/CSS/JavaScript实现，无框架依赖：

### 页面文件
- `web/index.html`: 首页，显示24小时请求统计
- `web/login.html`: 登录页面
- `web/channels.html`: 渠道管理（CRUD操作）
- `web/logs.html`: 请求日志（支持分页）
- `web/stats.html`: 调用统计（按渠道/模型分组）
- `web/trend.html`: 趋势图表（SVG绘制24小时曲线）
- `web/styles.css`: 共享样式文件
- `web/ui.js`: 共享JavaScript工具函数

## 项目特点

- **单二进制部署**：纯Go实现，使用嵌入式SQLite，无外部依赖
- **透明代理**：仅替换API Key，保持请求完整性，不干预连接生命周期
- **智能路由**：优先级分组 + 组内轮询 + 故障切换
- **指数退避**：失败渠道冷却时间1s→2s→4s...最大30分钟
- **前后端分离**：静态HTML + JSON API，无框架依赖
- **Session认证**：基于内存的安全会话管理
- **高性能架构**：多级缓存 + 异步处理 + 连接池优化
- **并发友好**：支持高并发请求，响应延迟降低50-80%
