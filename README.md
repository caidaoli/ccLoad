# ccLoad - Claude Code & Codex API 代理服务

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8.svg)](https://golang.org)
[![Gin](https://img.shields.io/badge/Gin-v1.10+-blue.svg)](https://github.com/gin-gonic/gin)
[![Docker](https://img.shields.io/badge/Docker-Supported-2496ED.svg)](https://hub.docker.com)
[![Hugging Face](https://img.shields.io/badge/%F0%9F%A4%97%20Hugging%20Face-Spaces-yellow)](https://huggingface.co/spaces)
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

- 🚀 **高性能架构** - 基于 Gin 框架，支持 1000+ 并发连接，异步Redis同步响应<1ms
- 🧮 **本地Token计数** - 符合官方API规范的本地Token估算，响应<5ms，准确度93%+，支持大规模工具场景
- 🎯 **智能错误分类** - 区分Key级错误、渠道级错误和客户端错误，精准故障切换
- 🔀 **智能路由** - 基于优先级和轮询的渠道选择算法，支持多Key负载均衡
- 🛡️ **故障切换** - 自动失败检测和指数退避冷却机制（1s → 2s → 4s → ... → 30min）
- 📊 **实时监控** - 内置趋势分析、日志记录和统计面板
- 🎯 **透明代理** - 支持Claude和Gemini API，智能识别认证方式
- 📦 **单文件部署** - 无外部依赖，包含嵌入式 SQLite
- 🔒 **安全认证** - 基于 Token 的管理界面和API访问控制
- 🏷️ **构建标签** - 支持 GOTAGS，默认启用高性能 JSON 库
- 🐳 **Docker 支持** - 多架构镜像（amd64/arm64），自动化 CI/CD
- ☁️ **云原生** - 支持容器化部署，GitHub Actions 自动构建
- 🤗 **Hugging Face** - 支持一键部署到 Hugging Face Spaces，免费托管

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

选择最适合你的部署方式：

| 部署方式 | 难度 | 成本 | 适用场景 | HTTPS | 持久化 |
|---------|------|------|----------|-------|--------|
| 🐳 **Docker** | ⭐⭐ | 需VPS | 生产环境、高性能需求 | 需配置 | ✅ |
| 🤗 **Hugging Face** | ⭐ | **免费** | 个人使用、快速体验 | ✅自动 | ✅ |
| 🔧 **源码编译** | ⭐⭐⭐ | 需服务器 | 开发调试、定制化 | 需配置 | ✅ |
| 📦 **二进制** | ⭐⭐ | 需服务器 | 轻量部署、简单环境 | 需配置 | ✅ |

### 方式一：Docker 部署（推荐）

**使用预构建镜像（推荐）**：
```bash
# 方式 1: 使用 docker-compose（最简单）
curl -o docker-compose.yml https://raw.githubusercontent.com/caidaoli/ccLoad/master/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/caidaoli/ccLoad/master/.env.example
# 编辑 .env 文件设置密码
docker-compose up -d

# 方式 2: 直接运行镜像
docker pull ghcr.io/caidaoli/ccload:latest
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -e CCLOAD_AUTH=your_api_token \
  -v ccload_data:/app/data \
  ghcr.io/caidaoli/ccload:latest
```

**从源码构建**：
```bash
# 克隆项目
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# 使用 docker-compose 构建并运行
docker-compose -f docker-compose.build.yml up -d

# 或手动构建
docker build -t ccload:local .
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -e CCLOAD_AUTH=your_api_token \
  -v ccload_data:/app/data \
  ccload:local
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

### 方式四：Hugging Face Spaces 部署

Hugging Face Spaces 提供免费的容器托管服务，支持 Docker 应用，适合个人和小团队使用。

#### 部署步骤

1. **登录 Hugging Face**

   访问 [huggingface.co](https://huggingface.co) 并登录你的账户

2. **创建新 Space**

   - 点击右上角 "New" → "Space"
   - **Space name**: `ccload`（或自定义名称）
   - **License**: `MIT`
   - **Select the SDK**: `Docker`
   - **Visibility**: `Public` 或 `Private`（私有需付费订阅）
   - 点击 "Create Space"

3. **创建 Dockerfile**

   在 Space 仓库中创建 `Dockerfile` 文件，内容如下：

   ```dockerfile
   FROM ghcr.io/caidaoli/ccload:latest
   ENV TZ=Asia/Shanghai
   ENV PORT=7860
   ENV SQLITE_PATH=/tmp/ccload.db
   EXPOSE 7860
   ```

   可以通过以下方式创建：

   **方式 A - Web 界面**（推荐）:
   - 在 Space 页面点击 "Files" 标签
   - 点击 "Add file" → "Create a new file"
   - 文件名输入 `Dockerfile`
   - 粘贴上述内容
   - 点击 "Commit new file to main"

   **方式 B - Git 命令行**:
   ```bash
   # 克隆你的 Space 仓库
   git clone https://huggingface.co/spaces/YOUR_USERNAME/ccload
   cd ccload

   # 创建 Dockerfile
   cat > Dockerfile << 'EOF'
   FROM ghcr.io/caidaoli/ccload:latest
   ENV TZ=Asia/Shanghai
   ENV PORT=7860
   ENV SQLITE_PATH=/tmp/ccload.db
   EXPOSE 7860
   EOF

   # 提交并推送
   git add Dockerfile
   git commit -m "Add Dockerfile for ccLoad deployment"
   git push
   ```

4. **配置环境变量（Secrets）**

   在 Space 设置页面（Settings → Variables and secrets → New secret）添加：

   | 变量名 | 值 | 必填 | 说明 |
   |--------|-----|------|------|
   | `CCLOAD_PASS` | `your_admin_password` | ✅ **必填** | 管理界面密码 |
  | `CCLOAD_AUTH` | `token1,token2` | ✅ 必填 | API 访问令牌（多个用逗号分隔；访问 /v1/* API 必须设置，否则返回 401） |
   | `REDIS_URL` | `rediss://user:pass@host:port` | ⚪ 可选 | Redis 连接地址，用于渠道数据备份和恢复 |

   **Redis URL 格式说明**:
   ```
   rediss://用户名:密码@服务器地址:端口

   示例:
   rediss://default:mypassword@redis.example.com:6379
   rediss://user123:pass456@127.0.0.1:6380
   ```

   **注意**:
   - `PORT` 和 `SQLITE_PATH` 已在 Dockerfile 中设置，无需配置
   - Hugging Face Spaces 重启后 `/tmp` 目录会清空
   - 配置 `REDIS_URL` 后，渠道数据会自动从 Redis 恢复

5. **等待构建和启动**

   推送 Dockerfile 后，Hugging Face 会自动：
   - 拉取预构建镜像（约 30 秒）
   - 启动应用容器（约 10 秒）
   - 总耗时约 1-2 分钟（比从源码构建快 3-5 倍）

6. **访问应用**

   构建完成后，通过以下地址访问：
   - **应用地址**: `https://YOUR_USERNAME-ccload.hf.space`
   - **管理界面**: `https://YOUR_USERNAME-ccload.hf.space/web/`
   - **API 端点**: `https://YOUR_USERNAME-ccload.hf.space/v1/messages`

   **首次访问提示**:
   - 如果 Space 处于休眠状态，首次访问需等待 20-30 秒唤醒
   - 后续访问会立即响应

#### Hugging Face 部署特点

**优势**:
- ✅ **完全免费**: 公开 Space 永久免费，包含 CPU 和存储
- ✅ **极速部署**: 使用预构建镜像，1-2 分钟即可完成（比源码构建快 3-5 倍）
- ✅ **自动 HTTPS**: 无需配置 SSL 证书，自动提供安全连接
- ✅ **Redis 备份**: 配置 Redis 后渠道数据自动备份，重启自动恢复
- ✅ **自动重启**: 应用崩溃后自动重启
- ✅ **版本控制**: 基于 Git，方便回滚和协作
- ✅ **简单维护**: 仅需 5 行 Dockerfile，无需管理源码

**限制**:
- ⚠️ **资源限制**: 免费版提供 2 CPU + 16GB RAM
- ⚠️ **休眠策略**: 48 小时无访问会进入休眠，首次访问需等待唤醒（约 20-30 秒）
- ⚠️ **固定端口**: 必须使用 7860 端口
- ⚠️ **公网访问**: Space 默认公开，必须设置 `CCLOAD_AUTH` 才能访问 /v1/* API（否则 401）

#### 数据持久化

**重要**: Hugging Face Spaces 的存储策略

由于 Hugging Face Spaces 的限制，推荐使用 **Redis 备份方案**：

**方案一：Redis 备份（推荐）**
- ✅ **自动恢复**: Space 重启后自动从 Redis 恢复渠道配置
- ✅ **实时同步**: 渠道增删改自动同步到 Redis
- ✅ **数据安全**: Redis 数据不受 Space 重启影响
- 配置方法: 在 Secrets 中添加 `REDIS_URL` 环境变量

**方案二：仅本地存储（不推荐）**
- ⚠️ **数据丢失**: Space 重启后 `/tmp` 目录会清空，渠道配置会丢失
- ⚠️ **手动恢复**: 需要重新通过 Web 界面或 CSV 导入配置渠道
- 使用场景: 仅用于临时测试

**Redis 备份工作流程**:
1. **首次启动**: 如果 `/tmp/ccload.db` 不存在且配置了 `REDIS_URL`，自动从 Redis 恢复渠道
2. **运行期间**: 渠道增删改自动同步到 Redis
3. **Space 重启**: `/tmp` 清空，应用启动时从 Redis 恢复渠道配置
4. **日志数据**: 存储在 `/tmp`，重启后清空（可通过 Web 界面导出历史日志）

**推荐的免费 Redis 服务**:
- [Upstash Redis](https://upstash.com/) - 免费 10,000 命令/天，支持 TLS
- [Redis Cloud](https://redis.com/try-free/) - 免费 30MB 存储
- [Railway Redis](https://railway.app/) - 免费 512MB

**配置示例（以 Upstash 为例）**:
1. 注册 [Upstash](https://upstash.com/) 账户
2. 创建 Redis 数据库（选择 TLS 启用）
3. 复制连接地址（格式：`rediss://default:xxx@xxx.upstash.io:6379`）
4. 在 Hugging Face Space 的 Secrets 中添加 `REDIS_URL` 变量，粘贴连接地址
5. 重启 Space，渠道数据会自动同步到 Redis

#### 更新部署

由于使用预构建镜像，更新非常简单：

**自动更新**:
- 当官方发布新版本镜像（`ghcr.io/caidaoli/ccload:latest`）时
- 在 Space 设置中点击 "Factory reboot" 即可自动拉取最新镜像
- 或等待 Hugging Face 自动重启（通常 48 小时后）

**手动触发更新**:
```bash
# 在 Space 仓库中添加一个空提交来触发重建
git commit --allow-empty -m "Trigger rebuild to pull latest image"
git push
```

**版本锁定**（可选）:
如果需要锁定特定版本，修改 Dockerfile：
```dockerfile
FROM ghcr.io/caidaoli/ccload:v0.2.0  # 指定版本号
ENV TZ=Asia/Shanghai
ENV PORT=7860
ENV SQLITE_PATH=/tmp/ccload.db
EXPOSE 7860
```

### 基本配置

```bash
# 设置环境变量
export CCLOAD_PASS=your_admin_password
export CCLOAD_AUTH=token1,token2,token3  # API 访问令牌（访问 /v1/* 必须设置）
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

**Claude API 代理（需授权）**：

```bash
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

### 本地 Token 计数

快速估算请求的 Token 消耗（无需调用上游 API）：

```bash
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ],
    "system": "You are a helpful assistant."
  }'

# 响应示例
# {
#   "input_tokens": 28
# }
```

**特点**：
- ✅ 符合 Anthropic 官方 API 规范
- ✅ 本地计算，响应 <5ms，不消耗 API 配额
- ✅ 准确度 93%+（与官方 API 对比）
- ✅ 支持系统提示词、工具定义、大规模工具场景
- ✅ 需授权令牌访问（未提供 CCLOAD_AUTH 时返回 401）

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
curl -H "Authorization: Bearer your_token" \
  http://localhost:8080/admin/channels/export > channels.csv
```

**导入配置**:
```bash
# Web界面: 访问 /web/channels.html，点击"导入CSV"按钮
# API调用:
curl -X POST -H "Authorization: Bearer your_token" \
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

## 🔧 技术栈

### 核心依赖

| 组件 | 版本 | 用途 | 性能优势 |
|------|------|------|----------|
| **Go** | 1.25.0+ | 运行时环境 | 原生并发支持，内置 min 函数 |
| **Gin** | v1.10.1 | Web框架 | 高性能HTTP路由 |
| **SQLite3** | v1.38.2 | 嵌入式数据库 | 零配置，单文件存储 |
| **Sonic** | v1.14.1 | JSON库 | 比标准库快2-3倍 |
| **go-redis** | v9.7.0 | Redis客户端 | 可选渠道数据同步 |
| **godotenv** | v1.5.1 | 环境配置 | 简化配置管理 |

### 架构特点

**模块化架构** (✅ P2重构完成，2025-10-29):
- **proxy模块拆分**（SRP原则）：
  - `proxy_handler.go` (236行)：HTTP入口、并发控制、路由选择
  - `proxy_forward.go` (299行)：核心转发逻辑、请求构建、响应处理
  - `proxy_error.go` (170行)：错误处理、冷却决策、重试逻辑
  - `proxy_util.go` (484行)：常量、类型定义、工具函数
  - `proxy_stream.go` (77行)：流式响应、首字节检测
  - `proxy_gemini.go` (42行)：Gemini API特殊处理
- **admin模块拆分**（SRP原则）：
  - `admin_channels.go`：渠道CRUD操作
  - `admin_stats.go`：统计分析API
  - `admin_cooldown.go`：冷却管理API
  - `admin_csv.go`：CSV导入导出
  - `admin_types.go`：管理API类型定义
- **冷却管理器**（DRY原则）：
  - `cooldown/manager.go` (122行)：统一冷却决策引擎
  - 消除重复代码83%，冷却逻辑统一管理
  - 区分网络错误和HTTP错误的分类策略
  - 内置单Key渠道自动升级逻辑

**多级缓存系统**:
- 渠道配置缓存（60秒TTL）
- 轮询指针缓存（内存）
- 冷却状态内联（channels/api_keys表直接存储）
- 错误分类缓存（1000容量）

**异步处理架构**:
- Redis同步（单worker协程，非阻塞触发，响应<1ms）
- 日志系统（1000条缓冲 + 3个worker，批量写入）
- Token/日志清理（后台协程，定期维护）

**统一响应系统** (✅ P1重构完成):
- `StandardResponse[T]` 泛型结构体（DRY原则）
- `ResponseHelper` 辅助类及9个快捷方法
- 自动提取应用级错误码，统一JSON格式

**连接池优化**:
- SQLite: 内存模式10个连接/文件模式5个连接，1分钟生命周期
- HTTP客户端: 100最大连接，30秒超时，keepalive优化
- TLS: 会话缓存（1024容量），减少握手耗时

## 🔧 配置说明

### 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_PASS` | 无 | 管理界面密码（必填，未设置将退出） |
| `CCLOAD_AUTH` | 无 | API 访问令牌（多个用逗号分隔；访问 /v1/* API 必须设置，否则返回 401） |
| `CCLOAD_MAX_KEY_RETRIES` | "3" | 单个渠道内最大Key重试次数 |
| `CCLOAD_USE_MEMORY_DB` | "false" | 启用内存数据库模式（需配合Redis使用） |
| `CCLOAD_SKIP_TLS_VERIFY` | "false" | 跳过TLS证书验证（仅开发环境） |
| `PORT` | "8080" | 服务端口 |
| `SQLITE_PATH` | "data/ccload.db" | 数据库文件路径 |
| `SQLITE_JOURNAL_MODE` | "WAL" | SQLite Journal模式（WAL/TRUNCATE/DELETE等） |

#### 行为摘要

- 未设置 `CCLOAD_PASS`：程序启动失败并退出（安全默认）。
- 未设置 `CCLOAD_AUTH`：所有 `/v1/*` 与 `/v1beta/*` API 返回 `401 Unauthorized`。
- 公开端点：仅 `GET /public/summary` 无需认证，其他端点均需授权令牌。
| `REDIS_URL` | 无 | Redis连接URL（可选，用于渠道数据异步备份） |
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

**核心表**:
- `channels` - 渠道配置（冷却数据内联，UNIQUE约束name）
- `api_keys` - API密钥（Key级冷却内联，支持多Key策略）
- `logs` - 请求日志
- `key_rr` - 轮询指针（channel_id → idx）

**架构特性** (✅ 2025-10月优化):
- ✅ 冷却数据内联（废弃独立cooldowns表，减少JOIN开销）
- ✅ 性能索引优化（渠道选择延迟↓30-50%，Key查找延迟↓40-60%）
- ✅ 外键约束（级联删除，保证数据一致性）
- ✅ 多Key支持（sequential/round_robin策略）

**向后兼容迁移**:
- 自动检测并修复重复渠道名称
- 智能添加UNIQUE约束，确保数据完整性
- 启动时自动执行，无需手动干预

## 🛡️ 安全考虑

- 生产环境必须设置强密码 `CCLOAD_PASS`
- 建议设置 `CCLOAD_AUTH` 以保护 API 端点访问
- API Key 仅在内存使用，不记录日志
- Token 存储在客户端 localStorage，24小时有效期
- 建议使用 HTTPS 反向代理
- Docker 镜像使用非 root 用户运行，增强安全性

### Token 认证系统

ccLoad 使用基于 Token 的认证机制，提供简洁高效的安全访问控制。

**认证方式**：
- **管理界面**：登录后获取24小时有效期的Token，存储在 `localStorage`
- **API端点**：支持 `Authorization: Bearer <token>` 头认证

**核心特性**：
- ✅ **无状态认证**：Token不依赖服务端Session，天然支持水平扩展
- ✅ **统一认证体系**：API和管理界面使用相同的Token机制
- ✅ **简洁架构**：纯Token认证，代码简单可靠（KISS原则）
- ✅ **跨域支持**：Token存储在localStorage，完全支持跨域访问

**使用示例**：
```bash
# 1. 登录获取Token
curl -X POST http://localhost:8080/login \
  -H "Content-Type: application/json" \
  -d '{"password":"your_admin_password"}' | jq

# 响应示例：
# {
#   "status": "success",
#   "token": "abc123...",  # 64字符十六进制Token
#   "expiresIn": 86400     # 24小时（秒）
# }

# 2. 使用Token访问管理API
curl http://localhost:8080/admin/channels \
  -H "Authorization: Bearer <your_token>"

# 3. 登出（可选，Token会在24小时后自动过期）
curl -X POST http://localhost:8080/logout \
  -H "Authorization: Bearer <your_token>"
```

**迁移详情**：参见 [TOKEN_AUTH_MIGRATION.md](TOKEN_AUTH_MIGRATION.md)

## 🔄 CI/CD

项目使用 GitHub Actions 实现自动化 CI/CD：

- **触发条件**：推送版本标签（`v*`）或手动触发
- **构建输出**：多架构 Docker 镜像推送到 GitHub Container Registry
- **版本管理**：自动生成语义化版本标签
- **缓存优化**：利用 GitHub Actions 缓存加速构建



## 🤝 贡献

欢迎提交 Issue 和 Pull Request！


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
