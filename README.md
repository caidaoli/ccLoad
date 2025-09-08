# ccLoad - Claude Code API 代理服务

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

一个高性能的 Claude Code API 透明代理服务，使用 Go 构建。支持多渠道负载均衡、故障切换和实时监控。

## ✨ 主要特性

- 🚀 **高性能架构** - 支持 1000+ 并发连接，响应延迟降低 50-80%
- 🔀 **智能路由** - 基于优先级和轮询的渠道选择算法
- 🛡️ **故障切换** - 自动失败检测和指数退避冷却机制
- 📊 **实时监控** - 内置趋势分析、日志记录和统计面板
- 🎯 **透明代理** - 仅替换 API Key，保持请求完整性
- 📦 **单文件部署** - 无外部依赖，包含嵌入式 SQLite
- 🔒 **安全认证** - 基于 Session 的管理界面访问控制

## 🚀 快速开始

### 安装

```bash
# 克隆项目
git clone <repository-url>
cd ccLoad

# 构建项目
go build -o ccload .

# 或直接运行
go run .
```

### 基本配置

```bash
# 设置环境变量
export CCLOAD_PASS=your_admin_password
export CCLOAD_AUTH=token1,token2,token3  # 可选，API 访问令牌
export PORT=8080
export SQLITE_PATH=./data/ccload.db

# 或使用 .env 文件
echo "CCLOAD_PASS=your_admin_password" > .env
echo "CCLOAD_AUTH=your_api_token" >> .env
echo "PORT=8080" >> .env
echo "SQLITE_PATH=./data/ccload.db" >> .env

# 启动服务
./ccload
```

服务启动后访问：
- 管理界面：`http://localhost:8080/web/`
- API 代理：`POST http://localhost:8080/v1/messages`

## 📖 使用说明

### API 代理

发送请求到 Claude API：

```bash
# 无需认证（未设置 CCLOAD_AUTH）
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-claude-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-3-sonnet-20240229",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "Hello, Claude!"
      }
    ]
  }'

# 需要认证（设置了 CCLOAD_AUTH）
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -H "x-api-key: your-claude-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-3-sonnet-20240229",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "Hello, Claude!"
      }
    ]
  }'
```

### 渠道管理

通过 Web 界面 `/web/channels.html` 或 API 管理渠道：

```bash
# 添加渠道
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude-API",
    "api_key": "sk-ant-api03-xxx",
    "url": "https://api.anthropic.com",
    "priority": 10,
    "models": ["claude-3-sonnet-20240229", "claude-3-opus-20240229"],
    "enabled": true
  }'
```

## 🏗️ 架构设计

### 核心组件

```
ccLoad
├── main.go           # 程序入口
├── server.go         # HTTP 服务器 & 缓存管理
├── proxy.go          # 代理转发逻辑
├── selector.go       # 渠道选择算法
├── admin.go          # 管理 API
├── sqlite_store.go   # SQLite 存储层
├── models.go         # 数据模型
└── web/              # 前端静态文件
```

### 性能优化

- **多级缓存**：渠道配置缓存 60 秒，轮询指针内存化
- **异步日志**：3 个工作协程批量处理，1000 条缓冲队列
- **连接池**：SQLite 25 连接 + HTTP 100 连接池
- **流式传输**：64KB 缓冲区优化
- **内存优化**：sync.Map 存储热数据

### 路由算法

1. 按优先级分组渠道
2. 同优先级内轮询分发
3. 失败自动切换到下一渠道
4. 指数退避冷却（1s → 2s → 4s ... 最大 30m）

## 📊 监控指标

访问管理界面查看：
- 24 小时请求趋势图
- 实时错误日志
- 渠道调用统计
- 性能指标监控

## 🔧 配置说明

### 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_PASS` | "admin" | 管理界面密码 |
| `CCLOAD_AUTH` | 无 | API 访问令牌（多个用逗号分隔） |
| `PORT` | "8080" | 服务端口 |
| `SQLITE_PATH` | "data/ccload.db" | 数据库文件路径 |

### 数据库结构

- `channels` - 渠道配置
- `logs` - 请求日志
- `cooldowns` - 冷却状态（channel_id, until, duration_ms）
- `rr` - 轮询指针（model, priority, next_index）

## 🛡️ 安全考虑

- 生产环境必须设置强密码 `CCLOAD_PASS`
- 建议设置 `CCLOAD_AUTH` 以保护 API 端点访问
- API Key 仅在内存使用，不记录日志
- 支持 HttpOnly 和 SameSite Cookie
- 建议使用 HTTPS 反向代理


## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

## 📄 许可证

MIT License