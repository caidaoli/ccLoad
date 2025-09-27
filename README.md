# ccLoad - Claude Code & Codex API 代理服务

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8.svg)](https://golang.org)
[![Gin](https://img.shields.io/badge/Gin-v1.10+-blue.svg)](https://github.com/gin-gonic/gin)
[![Docker](https://img.shields.io/badge/Docker-Supported-2496ED.svg)](https://hub.docker.com)
[![GitHub Actions](https://img.shields.io/badge/CI%2FCD-GitHub%20Actions-2088FF.svg)](https://github.com/features/actions)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

一个高性能的 Claude Code & Codex API 透明代理服务，使用 Go 1.25.0 和 Gin 框架构建。支持多渠道负载均衡、故障切换和实时监控。

## 🎯 痛点解决

在使用 Claude API 服务时，用户通常会面临以下痛点：

- **多渠道管理复杂**：需要同时管理多个 API 渠道，有的渠道时效短，有的渠道每天有限量
- **手动切换不便**：每次手动切换渠道费时费力，影响工作效率
- **故障处理困难**：当某个渠道出现故障时，需要手动切换到其他可用渠道

ccLoad 通过以下特性解决这些痛点：

- **智能路由**：根据渠道优先级优先请求高优先级渠道，相同优先级则轮询调用
- **自动故障切换**：当渠道出现故障时，自动切换到其他可用渠道
- **指数级冷却机制**：故障渠道使用指数级别冷却时间，避免持续请求故障服务
- **零手动干预**：客户端无需手动切换上游渠道，系统自动处理

## ✨ 主要特性

- 🚀 **高性能架构** - 基于 Gin 框架，支持 1000+ 并发连接，响应延迟降低 50-80%
- ⚡ **性能优化** - Sonic JSON 库 + Ristretto 缓存 + 连接池优化
- 🔀 **智能路由** - 基于优先级和轮询的渠道选择算法
- 🛡️ **故障切换** - 自动失败检测和指数退避冷却机制
- 📊 **实时监控** - 内置趋势分析、日志记录和统计面板
- 🎯 **透明代理** - 仅替换 API Key，保持请求完整性
- 📦 **单文件部署** - 无外部依赖，包含嵌入式 SQLite
- 🔒 **安全认证** - 基于 Session 的管理界面访问控制
- 🏷️ **构建标签** - 支持 GOTAGS，默认启用高性能 JSON 库
- 🐳 **Docker 支持** - 多架构镜像（amd64/arm64），自动化 CI/CD
- ☁️ **云原生** - 支持容器化部署，GitHub Actions 自动构建

## 🏗️ 架构概览

```mermaid
graph TB
    subgraph "客户端"
        A[用户应用] --> B[ccLoad代理]
    end
    
    subgraph "ccLoad服务"
        B --> C[认证层]
        C --> D[路由分发]
        D --> E[渠道选择器]
        E --> F[负载均衡器]
        
        subgraph "核心组件"
            F --> G[渠道A<br/>优先级:10]
            F --> H[渠道B<br/>优先级:5]
            F --> I[渠道C<br/>优先级:5]
        end
        
        subgraph "存储层"
            J[(SQLite数据库)]
        end
        
        subgraph "监控层"
            K[日志系统]
            L[统计分析]
            M[趋势图表]
        end
    end
    
    subgraph "上游服务"
        G --> N[Claude API]
        H --> O[Claude API]
        I --> P[Claude API]
    end
    
    E <--> J
    F <--> J
    K <--> J
    L <--> J
    M <--> J
    
    style B fill:#4F46E5,stroke:#000,color:#fff
    style F fill:#059669,stroke:#000,color:#fff
    style E fill:#0EA5E9,stroke:#000,color:#fff
```

## 🚀 快速开始

### 方式一：Docker 部署（推荐）

```bash
# 拉取预构建镜像
docker pull ghcr.io/caidaoli/ccload:latest

# 运行容器
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -e CCLOAD_AUTH=your_api_token \
  -v ccload_data:/app/data \
  ghcr.io/caidaoli/ccload:latest

# 或使用 docker-compose
curl -o docker-compose.yml https://raw.githubusercontent.com/caidaoli/ccLoad/master/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/caidaoli/ccLoad/master/.env.docker.example
# 编辑 .env 文件设置密码
docker-compose up -d
```

### 方式二：源码编译

```bash
# 克隆项目
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# 构建项目（默认使用高性能 JSON 库）
go build -tags go_json -o ccload .

# 或使用 Makefile
make build

# 直接运行开发模式
go run -tags go_json .
# 或
make dev
```

### 方式三：二进制下载

```bash
# 从 GitHub Releases 下载对应平台的二进制文件
wget https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64
chmod +x ccload-linux-amd64
./ccload-linux-amd64
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

### 批量数据管理

支持CSV格式的渠道配置导入导出：

**导出配置**:
```bash
# Web界面: 访问 /web/channels.html，点击"导出CSV"按钮
# API调用:
curl -H "Cookie: session=xxx" \
  http://localhost:8080/admin/channels/export > channels.csv
```

**导入配置**:
```bash
# Web界面: 访问 /web/channels.html，点击"导入CSV"按钮
# API调用:
curl -X POST -H "Cookie: session=xxx" \
  -F "file=@channels.csv" \
  http://localhost:8080/admin/channels/import
```

**CSV格式示例**:
```csv
name,api_key,url,priority,models,enabled
Claude-API-1,sk-ant-xxx,https://api.anthropic.com,10,"[\"claude-3-sonnet-20240229\"]",true
Claude-API-2,sk-ant-yyy,https://api.anthropic.com,5,"[\"claude-3-opus-20240229\"]",true
```

**特性**:
- 支持中英文列名自动映射
- 智能数据验证和错误提示
- 增量导入和覆盖更新
- UTF-8编码，Excel兼容

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
| `GOTAGS` | "go_json" | 构建标签（go_json/std） |

### Docker 镜像

项目支持多架构 Docker 镜像：

- **支持架构**：`linux/amd64`, `linux/arm64`
- **镜像仓库**：`ghcr.io/caidaoli/ccload`
- **可用标签**：
  - `latest` - 最新稳定版本
  - `v0.1.0` - 具体版本号
  - `v0.1` - 主要.次要版本
  - `v0` - 主要版本

### 镜像标签说明

```bash
# 拉取最新版本
docker pull ghcr.io/caidaoli/ccload:latest

# 拉取指定版本
docker pull ghcr.io/caidaoli/ccload:v0.1.0

# 指定架构（Docker 通常自动选择）
docker pull --platform linux/amd64 ghcr.io/caidaoli/ccload:latest
docker pull --platform linux/arm64 ghcr.io/caidaoli/ccload:latest
```

### 数据库结构

- `channels` - 渠道配置（具有name字段UNIQUE约束）
- `logs` - 请求日志
- `cooldowns` - 冷却状态（channel_id, until, duration_ms）
- `rr` - 轮询指针（model, priority, next_index）

**向后兼容迁移**:
- 自动检测并修复重复渠道名称
- 智能添加UNIQUE约束，确保数据完整性
- 启动时自动执行，无需手动干预

## 🛡️ 安全考虑

- 生产环境必须设置强密码 `CCLOAD_PASS`
- 建议设置 `CCLOAD_AUTH` 以保护 API 端点访问
- API Key 仅在内存使用，不记录日志
- 支持 HttpOnly 和 SameSite Cookie
- 建议使用 HTTPS 反向代理
- Docker 镜像使用非 root 用户运行，增强安全性

## 🔄 CI/CD

项目使用 GitHub Actions 实现自动化 CI/CD：

- **触发条件**：推送版本标签（`v*`）或手动触发
- **构建输出**：多架构 Docker 镜像推送到 GitHub Container Registry
- **版本管理**：自动生成语义化版本标签
- **缓存优化**：利用 GitHub Actions 缓存加速构建

### 发布新版本

```bash
# 创建并推送版本标签
git tag v0.2.0
git push origin v0.2.0

# 自动触发构建，生成镜像：
# - ghcr.io/caidaoli/ccload:v0.2.0
# - ghcr.io/caidaoli/ccload:v0.2
# - ghcr.io/caidaoli/ccload:v0
# - ghcr.io/caidaoli/ccload:latest
```

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

### 开发环境

```bash
# 本地开发
go run -tags go_json .

# 本地 Docker 测试
docker build -t ccload:dev .
docker run --rm -p 8080:8080 -e CCLOAD_PASS=test123 ccload:dev
```

### 故障排除

**端口被占用**:
```bash
# 查找并终止占用 8080 端口的进程
lsof -i :8080 && kill -9 <PID>
```

**容器问题**:
```bash
# 查看容器日志
docker logs ccload -f
# 检查容器健康状态
docker inspect ccload --format='{{.State.Health.Status}}'
```

**配置验证**:
```bash
# 测试服务是否正常响应
curl -s http://localhost:8080/public/summary
# 检查环境变量配置
env | grep CCLOAD
```

## 📄 许可证

MIT License