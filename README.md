# ccLoad - Claude Code & Codex & Gemini & OpenAI 兼容 API 代理服务

**[English](README_EN.md) | 简体中文**

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8.svg)](https://golang.org)
[![Gin](https://img.shields.io/badge/Gin-v1.10+-blue.svg)](https://github.com/gin-gonic/gin)
[![Docker](https://img.shields.io/badge/Docker-Supported-2496ED.svg)](https://hub.docker.com)
[![Hugging Face](https://img.shields.io/badge/%F0%9F%A4%97%20Hugging%20Face-Spaces-yellow)](https://huggingface.co/spaces)
[![GitHub Actions](https://img.shields.io/badge/CI%2FCD-GitHub%20Actions-2088FF.svg)](https://github.com/features/actions)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> 🚀 高性能AI API透明代理 | 多渠道智能调度 | 故障秒切 | 实时监控 | 开箱即用

兄弟们，用Claude API是不是有这些烦恼：渠道太多管不过来、限流了手动切换、挂了只能干等？ccLoad帮你全搞定！一个Go语言写的高性能代理服务，支持Claude Code、Codex、Gemini、OpenAI四大平台。**智能路由+自动故障切换+实时监控**，让你的API调用稳如老狗🐶

## 🎯 痛点解决

用 Claude API 的兄弟们，这些场景是不是似曾相识👇

- 😫 **渠道管理累死人**：手里一堆API渠道，有的快过期，有的有限额，切来切去头都大
- 🔄 **手动切换烦透了**：这个渠道挂了换那个，那个限流了再换，一天光切渠道了
- 🤯 **故障来了手忙脚乱**：渠道突然 502/504，只能干等着，影响工作进度
- 👀 **请求发出去就像石沉大海**：发完请求傻等，不知道卡在哪一步，焦虑感拉满
- 🎭 **上游骗你说成功了**：返回 200 状态码，结果响应内容是报错，坑得你一脸懵

ccLoad 一站式解决👇

- 🎯 **智能路由**：高优先级渠道优先用，同级按Key数量加权随机，省心
- 🔀 **自动故障切换**：渠道挂了秒切，你甚至感知不到
- ⏰ **指数级冷却**：故障渠道自动休息，2分钟→4分钟→8分钟，不会反复踩坑
- 🙌 **零手动干预**：躺平就行，系统全自动处理
- 📊 **实时请求监控**：正在跑的请求一目了然，告别盲等
- 🔍 **软错误检测**：HTTP 200 伪装成功？逃不过检测！自动识别以下"假成功"：
  - `{"error": {...}}` 结构的 JSON 错误
  - `type` 字段是 `"error"` 的响应
  - `"当前模型负载过高"` 之类的纯文本告警

## ✨ 主要特性

这波配置真的很顶👇

| 能力 | 亮点 | 效果 |
|------|------|------|
| 🚀 **性能怪兽** | Gin框架 + Sonic JSON | 1000+并发，Redis同步<1ms |
| 🧮 **本地算Token** | 不调API就能估算消耗 | 响应<5ms，准确度93%+ |
| 🎯 **错误分类器** | Key级/渠道级/客户端错误 | 200伪装错误也能揪出来 |
| 🔀 **智能调度** | 优先级+加权随机+健康度排序 | 烂渠道自动靠边站 |
| 🛡️ **故障秒切** | 指数退避冷却机制 | 2min→4min→8min→30min |
| 📊 **数据大屏** | 趋势图+日志+Token统计 | 一眼看清用量情况 |
| 🎯 **多API兼容** | Claude/Gemini/OpenAI | 一套配置走天下 |
| 📦 **开箱即用** | 单文件+嵌入式SQLite | 零依赖，下载就能跑 |
| 🐳 **云原生** | 多架构镜像+CI/CD | amd64/arm64都支持 |
| 🤗 **白嫖福利** | Hugging Face免费托管 | 个人用完全够了 |
| 💰 **成本限额** | 渠道每日成本上限 | 达到限额自动跳过 |
| 🔐 **令牌限额** | API令牌费用上限+模型限制 | 精细化访问控制 |
| ⏱️ **首字节监控** | 流式请求TTFB记录 | 便于诊断上游延迟 |

## 🏗️ 架构概览

想知道ccLoad怎么跑起来的？其实很简单👇

从你的应用发请求到API返回结果，中间经过这几层：
- **认证层** - 验证你的访问权限，拒绝白嫖党
- **路由分发** - 判断是Claude还是Gemini，分流处理
- **智能调度** - 从一堆渠道里选个最靠谱的给你用
- **故障切换** - 选中的渠道挂了？秒切备用，你根本感知不到

核心亮点：**存储层用工厂模式**，SQLite和MySQL共享代码，消除了467行重复代码（DRY原则拉满）。数据层架构清晰，想换数据库？改个环境变量就完事👇

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
            J[(存储工厂)]
            J3[Schema定义层]
            J4[统一SQL层]
            J1[(SQLite)]
            J2[(MySQL)]
            J --> J3
            J3 --> J4
            J4 --> J1
            J4 --> J2
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

3分钟部署，选一个适合你的方式👇

| 部署方式 | 难度 | 成本 | 适合谁 | HTTPS | 持久化 |
|---------|------|------|--------|-------|--------|
| 🐳 **Docker** | ⭐⭐ | 需VPS | 生产环境、追求稳定 | 需配置 | ✅ |
| 🤗 **Hugging Face** | ⭐ | **白嫖** | 个人玩家、先体验一下 | ✅自动 | ✅ |
| 🔧 **源码编译** | ⭐⭐⭐ | 需服务器 | 爱折腾、想魔改 | 需配置 | ✅ |
| 📦 **二进制** | ⭐⭐ | 需服务器 | 懒人福音、轻量部署 | 需配置 | ✅ |

### 方式一：Docker 部署（推荐）💪

兄弟们，生产环境就选这个！镜像已经打好了，直接拉下来用，稳定又省心。

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
  -v ccload_data:/app/data \
  ghcr.io/caidaoli/ccload:latest
```

**从源码构建**：

想自己编译镜像？也行，适合对官方镜像不放心的同学👇
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
  -v ccload_data:/app/data \
  ccload:local
```

### 方式二：源码编译

爱折腾的兄弟看过来！想魔改代码就选这个，Go环境准备好就能跑👇

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

懒人福音！不想装Docker，也不想装Go？直接下个可执行文件就完事👇

```bash
# 从 GitHub Releases 下载对应平台的二进制文件
wget https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64
chmod +x ccload-linux-amd64
./ccload-linux-amd64
```

### 方式四：Hugging Face Spaces 部署

白嫖党狂喜时刻！Hugging Face提供免费Docker托管，HTTPS自动配，个人用绝对够👇

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
   | `REDIS_URL` | `rediss://user:pass@host:port` | ⚪ 可选 | Redis 连接地址，用于渠道数据备份和恢复 |

   **注意**: API 访问令牌现在通过 Web 管理界面 `/web/tokens.html` 进行配置，不再通过环境变量设置。

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
- ⚠️ **公网访问**: Space 默认公开，必须通过 Web 管理界面配置 API 访问令牌才能访问 /v1/* API（否则 401）

#### 数据持久化

**重要**: Hugging Face Spaces 的存储策略

由于 Hugging Face Spaces 的限制（`/tmp` 目录重启后清空），**强烈推荐使用外部 MySQL 数据库**实现完整的数据持久化：

**方案一：MySQL 外部数据库（推荐）**
- ✅ **完整持久化**: 渠道配置、日志记录、统计数据全部保留
- ✅ **重启不丢数据**: 数据存储在外部数据库，不受 Space 重启影响
- ✅ **免费方案**: 多个服务商提供免费 MySQL 数据库
- 配置方法: 在 Secrets 中添加 `CCLOAD_MYSQL` 环境变量

**推荐的免费 MySQL 服务**:
- [TiDB Cloud Serverless](https://tidbcloud.com/) - 免费 5GB 存储，MySQL 兼容，无连接数限制，推荐首选
- [Aiven for MySQL](https://aiven.io/) - 免费 1GB 存储，支持多区域部署

**MySQL 配置示例（以 TiDB Cloud 为例）**:
1. 注册 [TiDB Cloud](https://tidbcloud.com/) 账户
2. 创建 Serverless Cluster（免费）
3. 获取连接信息，格式为：`user:password@tcp(host:4000)/database?tls=true`
4. 在 Hugging Face Space 的 Secrets 中添加 `CCLOAD_MYSQL` 变量
5. 重启 Space，所有数据将自动持久化到 MySQL

**Dockerfile 示例（使用 MySQL）**:
```dockerfile
FROM ghcr.io/caidaoli/ccload:latest
ENV TZ=Asia/Shanghai
ENV PORT=7860
# 不需要 SQLITE_PATH，使用 CCLOAD_MYSQL 环境变量
EXPOSE 7860
```

**方案二：Redis 备份（仅渠道配置）**
- ✅ **自动恢复**: Space 重启后自动从 Redis 恢复渠道配置
- ✅ **实时同步**: 渠道增删改自动同步到 Redis
- ⚠️ **仅配置数据**: 日志和统计数据不会备份，重启后丢失
- 配置方法: 在 Secrets 中添加 `REDIS_URL` 环境变量

**推荐的免费 Redis 服务**:
- [Upstash Redis](https://upstash.com/) - 免费 10,000 命令/天，支持 TLS
- [Redis Cloud](https://redis.com/try-free/) - 免费 30MB 存储

**方案三：仅本地存储（不推荐）**
- ⚠️ **数据丢失**: Space 重启后 `/tmp` 目录会清空，渠道配置会丢失
- ⚠️ **手动恢复**: 需要重新通过 Web 界面或 CSV 导入配置渠道
- 使用场景: 仅用于临时测试

**Redis 备份工作流程**:
1. **首次启动**: 如果 `/tmp/ccload.db` 不存在且配置了 `REDIS_URL`，自动从 Redis 恢复渠道
2. **运行期间**: 渠道增删改自动同步到 Redis
3. **Space 重启**: `/tmp` 清空，应用启动时从 Redis 恢复渠道配置
4. **日志数据**: 存储在 `/tmp`，重启后清空（可通过 Web 界面导出历史日志）

#### 更新部署

由于使用预构建镜像，更新非常简单：

**自动更新**:
- 当官方发布新版本镜像（`ghcr.io/caidaoli/ccload:latest`）时
- 在 Space 设置中点击 "Factory rebuild" 即可自动拉取最新镜像
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

部署完了就该配置了！选SQLite还是MySQL？看你场景👇

**SQLite 模式（默认）**：
个人用或小团队，这个最省心，零配置，单文件搞定👇
```bash
# 设置环境变量
export CCLOAD_PASS=your_admin_password
export PORT=8080
export SQLITE_PATH=./data/ccload.db

# 或使用 .env 文件
echo "CCLOAD_PASS=your_admin_password" > .env
echo "PORT=8080" >> .env
echo "SQLITE_PATH=./data/ccload.db" >> .env

# 启动服务
./ccload
```

**MySQL 模式**：
生产环境or高并发？上MySQL稳定性更好，多实例也不怕👇
```bash
# 1. 创建 MySQL 数据库
mysql -u root -p -e "CREATE DATABASE ccload CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 2. 设置环境变量
export CCLOAD_PASS=your_admin_password
export CCLOAD_MYSQL="user:password@tcp(localhost:3306)/ccload?charset=utf8mb4"
export PORT=8080

# 或使用 .env 文件
echo "CCLOAD_PASS=your_admin_password" > .env
echo "CCLOAD_MYSQL=user:password@tcp(localhost:3306)/ccload?charset=utf8mb4" >> .env
echo "PORT=8080" >> .env

# 3. 启动服务（自动创建表结构）
./ccload
```

**Docker + MySQL**:
```bash
# 方式 1: docker-compose（推荐）
cat > docker-compose.mysql.yml << 'EOF'
version: '3.8'
services:
  mysql:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: rootpass
      MYSQL_DATABASE: ccload
      MYSQL_USER: ccload
      MYSQL_PASSWORD: ccloadpass
    volumes:
      - mysql_data:/var/lib/mysql
    ports:
      - "3306:3306"
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "localhost"]
      interval: 10s
      timeout: 5s
      retries: 5

  ccload:
    image: ghcr.io/caidaoli/ccload:latest
    environment:
      CCLOAD_PASS: your_admin_password
      CCLOAD_MYSQL: "ccload:ccloadpass@tcp(mysql:3306)/ccload?charset=utf8mb4"
      PORT: 8080
    ports:
      - "8080:8080"
    depends_on:
      mysql:
        condition: service_healthy

volumes:
  mysql_data:
EOF

docker-compose -f docker-compose.mysql.yml up -d

# 方式 2: 直接运行（需要已有 MySQL 服务）
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_admin_password \
  -e CCLOAD_MYSQL="user:pass@tcp(mysql_host:3306)/ccload?charset=utf8mb4" \
  ghcr.io/caidaoli/ccload:latest
```

服务启动后访问：
- 管理界面：`http://localhost:8080/web/`
- API 代理：`POST http://localhost:8080/v1/messages`
- **API 令牌管理**：`http://localhost:8080/web/tokens.html` - 通过 Web 界面配置 API 访问令牌

## 📖 使用说明

配好了就该用起来了！看看怎么调用API👇

### API 代理

**Claude API 代理（需授权）**：

先在Web界面配个令牌，然后就能用了。把ccLoad当Claude官方API用就行👇

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

**OpenAI 兼容 API 代理（Chat Completions）**：

用OpenAI SDK的兄弟有福了！直接换个base_url就能用，原来的代码一行不用改👇

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {
        "role": "user",
        "content": "Hello!"
      }
    ]
  }'
```

### 本地 Token 计数

发请求前想知道要花多少Token？用这个接口秒算，不花一分钱👇

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
- ✅ 需授权令牌访问（在 Web 管理界面 `/web/tokens.html` 配置令牌）

### 渠道管理

Web界面和API都能管理渠道，看你喜欢哪种👇

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

渠道多了手动加太累？支持CSV导入导出，Excel编辑完直接导入👇

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

管理后台有多香？一看便知👇

![ccLoad管理界面](images/ccload-dashboard.jpeg)
![ccLoad日志界面](images/ccload-logs.jpg)
*实时监控大屏：Claude Code、Codex、OpenAI、Gemini四大平台数据一目了然*

**核心功能**：
- 📈 **24小时趋势图** - 请求量一目了然，高峰低谷清清楚楚
- 🔴 **实时错误日志** - 哪个渠道炸了，秒级发现
- 📊 **渠道调用统计** - 谁在干活谁在摸鱼，数据说话
- ⚡ **性能指标** - 延迟、成功率，性能瓶颈无处藏
- 💰 **Token用量统计** - 钱花哪了心里有数：
  - 自定义时间范围，想看哪段看哪段
  - 按API令牌分类，多租户也能分账
  - 支持Gemini/OpenAI缓存Token展示

**界面亮点**：
- 🎨 渐变紫色主题，看着舒服
- 📱 响应式设计，手机电脑都好用
- ⚡ 数据实时刷新，不用手动F5
- 📊 多维度统计卡片，关键数据一屏看完

## 🔧 技术栈

想知道ccLoad用了啥技术？看这里👇

### 核心依赖

| 组件 | 版本 | 用途 | 性能优势 |
|------|------|------|----------|
| **Go** | 1.25.0+ | 运行时环境 | 原生并发支持，内置 min 函数 |
| **Gin** | v1.10.1 | Web框架 | 高性能HTTP路由 |
| **SQLite3** | v1.38.2 | 嵌入式数据库 | 零配置，单文件存储（默认） |
| **MySQL** | v1.8.1 | 关系型数据库 | 可选，适合高并发生产环境 |
| **Sonic** | v1.14.1 | JSON库 | 比标准库快2-3倍 |
| **go-redis** | v9.7.0 | Redis客户端 | 可选渠道数据同步 |
| **godotenv** | v1.5.1 | 环境配置 | 简化配置管理 |

### 架构特点

代码写得怎么样？来看看这些亮点👇

**模块化架构**（SOLID原则实践）:
- **proxy模块拆分**（SRP原则）：
  - `proxy_handler.go`：HTTP入口、并发控制、路由选择
  - `proxy_forward.go`：核心转发逻辑、请求构建、响应处理
  - `proxy_error.go`：错误处理、冷却决策、重试逻辑
  - `proxy_util.go`：常量、类型定义、工具函数
  - `proxy_stream.go`：流式响应、首字节检测
  - `proxy_gemini.go`：Gemini API特殊处理
  - `proxy_sse_parser.go`：SSE解析器（防御性处理，支持 Gemini/OpenAI 缓存 Token 解析）
- **admin模块拆分**（SRP原则）：
  - `admin_channels.go`：渠道CRUD操作
  - `admin_stats.go`：统计分析API
  - `admin_cooldown.go`：冷却管理API
  - `admin_csv.go`：CSV导入导出
  - `admin_types.go`：管理API类型定义
  - `admin_auth_tokens.go`：API访问令牌CRUD（支持Token统计、费用限额、模型限制）
  - `admin_settings.go`：系统设置管理
  - `admin_models.go`：模型列表管理
  - `admin_testing.go`：渠道测试功能
- **冷却管理器**（DRY原则）：
  - `cooldown/manager.go`：统一冷却决策引擎
  - 消除重复代码，冷却逻辑统一管理
  - 区分网络错误和HTTP错误的分类策略
  - 内置单Key渠道自动升级逻辑
- **存储层重构**（2025-12优化，消除467行重复代码）：
  - `storage/schema/`：统一Schema定义（支持SQLite/MySQL差异）
  - `storage/sql/`：通用SQL实现层（SQLite/MySQL共享）
  - `storage/factory.go`：工厂模式自动选择数据库
  - 复合索引优化，统计查询性能提升

**多级缓存系统**（性能拉满）:
- 渠道配置缓存（60秒TTL）- 减少数据库查询
- 轮询指针缓存（内存）- 毫秒级选择
- 冷却状态内联（直接存表）- 无需JOIN，速度飞起
- 错误分类缓存（1000容量）- 重复错误秒判

**异步处理架构**（响应贼快）:
- Redis同步（单worker协程，非阻塞触发，响应<1ms）
- 日志系统（1000条缓冲 + 单worker，保证FIFO顺序）
- Token/日志清理（后台协程，定期维护）

**统一响应系统**（代码复用典范）:
- `StandardResponse[T]` 泛型结构体（DRY原则）- 一个结构搞定所有响应
- `ResponseHelper` 辅助类及9个快捷方法 - 少写重复代码
- 自动提取应用级错误码，统一JSON格式 - 前端调用更方便

**连接池优化**（榨干性能）:
- SQLite: 内存模式10个连接/文件模式5个连接，5分钟生命周期
- HTTP客户端: 100最大连接，30秒超时，keepalive优化
- TLS: 会话缓存（1024容量），减少握手耗时

## 🔧 配置说明

想精细调优？这些配置项了解一下👇

### 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `CCLOAD_PASS` | 无 | 管理界面密码（**必填**，未设置将退出） |
| `CCLOAD_MYSQL` | 无 | MySQL DSN（可选，格式: `user:pass@tcp(host:port)/db?charset=utf8mb4`）<br/>**设置后使用 MySQL，否则使用 SQLite** |
| `CCLOAD_ALLOW_INSECURE_TLS` | `0` | 禁用上游 TLS 证书校验（`1`=启用；⚠️仅用于临时排障/受控内网环境） |
| `PORT` | `8080` | 服务端口 |
| `GIN_MODE` | `release` | 运行模式（`debug`/`release`） |
| `GIN_LOG` | `true` | Gin 访问日志开关（`false`/`0`/`no`/`off` 关闭） |
| `SQLITE_PATH` | `data/ccload.db` | SQLite 数据库文件路径（仅 SQLite 模式） |
| `SQLITE_JOURNAL_MODE` | `WAL` | SQLite Journal 模式（WAL/TRUNCATE/DELETE 等，容器环境建议 TRUNCATE） |
| `CCLOAD_MAX_CONCURRENCY` | `1000` | 最大并发请求数（限制同时处理的代理请求数量） |
| `CCLOAD_MAX_BODY_BYTES` | `2097152` | 请求体最大字节数（2MB，防止大包打爆内存） |
| `REDIS_URL` | 无 | Redis 连接 URL（可选，用于渠道数据异步备份） |
| `CCLOAD_COOLDOWN_AUTH_SEC` | `300` | 认证错误(401/402/403)初始冷却时间（秒） |
| `CCLOAD_COOLDOWN_SERVER_SEC` | `120` | 服务器错误(5xx)初始冷却时间（秒） |
| `CCLOAD_COOLDOWN_TIMEOUT_SEC` | `60` | 超时错误(597/598)初始冷却时间（秒） |
| `CCLOAD_COOLDOWN_RATE_LIMIT_SEC` | `60` | 限流错误(429)初始冷却时间（秒） |
| `CCLOAD_COOLDOWN_MAX_SEC` | `1800` | 指数退避冷却上限（秒，30分钟） |
| `CCLOAD_COOLDOWN_MIN_SEC` | `10` | 指数退避冷却下限（秒） |

### Web 管理配置（支持热重载）

这些配置在Web界面就能改，不用重启服务，改完立即生效👇

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `log_retention_days` | `7` | 日志保留天数（-1永久保留，1-365天） |
| `max_key_retries` | `3` | 单个渠道内最大Key重试次数 |
| `upstream_first_byte_timeout` | `0` | 上游首字节超时（秒，0=禁用） |
| `enable_health_score` | `false` | 启用基于健康度的渠道动态排序 |
| `success_rate_penalty_weight` | `100` | 成功率惩罚权重（见下方说明） |
| `health_score_window_minutes` | `30` | 成功率统计时间窗口（分钟） |
| `health_score_update_interval` | `30` | 成功率缓存更新间隔（秒） |
| `health_min_confident_sample` | `20` | 置信样本量阈值（样本量达到此值时惩罚全额生效） |

#### 健康度排序说明

想让烂渠道自动靠边站？启用健康度排序就行了👇

启用 `enable_health_score` 后，系统会根据渠道的历史成功率动态调整优先级，成功率低的渠道优先级自动降低：

```
置信度 = min(1.0, 样本量 / health_min_confident_sample)
有效优先级 = 基础优先级 - (失败率 × success_rate_penalty_weight × 置信度)
```

**置信度因子**：解决新渠道或低流量渠道因样本量小导致的过度惩罚问题。样本量越小，置信度越低，惩罚打折越多。

**示例**（`success_rate_penalty_weight = 100`，`health_min_confident_sample = 20`）：

| 渠道 | 基础优先级 | 成功率 | 样本量 | 置信度 | 惩罚值 | 有效优先级 |
|------|-----------|--------|--------|--------|--------|-----------|
| A | 100 | 95% | 100 | 1.0 | 5 | **95** |
| B | 90 | 70% | 80 | 1.0 | 30 | **60** |
| C | 80 | 60% | 4 | 0.2 | 8 | **72** |
| D | 70 | 100% | 50 | 1.0 | 0 | **70** |

基础优先级排序：A > B > C > D
**有效优先级排序：A (95) > C (72) > D (70) > B (60)**

**动态排序效果**：
- 渠道 B 原本排第二，但 70% 成功率导致惩罚 30，降至最后
- 渠道 D 原本排最后，但 100% 成功率使其超越 B 和 C
- 渠道 C 成功率仅 60%，但样本量 4（置信度 0.2）使惩罚从 40 降为 8，避免新渠道被过早淘汰

**权重调优建议**：
- 默认值 100 适合渠道优先级间隔为 10 的场景
- 权重 100 时：10% 失败率 = 降一档优先级（满置信度时）
- 若优先级间隔为 5，可调整为 50
- `health_min_confident_sample` 建议根据日均请求量调整，默认 20 适合中等流量场景

#### API 访问令牌配置

**划重点**：API令牌现在在Web界面管理，不用改环境变量了👇

- 访问 `http://localhost:8080/web/tokens.html` 进行令牌管理
- 支持添加、删除、查看令牌
- 所有令牌存储在数据库中，支持持久化
- 未配置任何令牌时，所有 `/v1/*` 与 `/v1beta/*` API 返回 `401 Unauthorized`

**令牌高级功能**（2026-01新增）：
- **费用限额**：为每个令牌设置费用上限（美元），超限后拒绝请求返回 429
- **模型限制**：限制令牌可访问的模型列表，增强访问控制
- **首字节时间**：记录流式请求的 TTFB（毫秒），便于诊断上游延迟

#### 行为摘要

兄弟们注意这几条安全策略👇

- 未设置 `CCLOAD_PASS`：程序启动失败并退出（安全第一）
- 未配置 API 访问令牌：所有 `/v1/*` 与 `/v1beta/*` API 返回 `401 Unauthorized`，去Web界面 `/web/tokens.html` 配置令牌
- 公开端点：`GET /health`（健康检查）和 `GET /public/summary`（统计摘要）无需认证，其他都要授权

### Docker 镜像

多架构镜像都准备好了，amd64/arm64随便选👇

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

想了解数据怎么存的？看这里👇

**存储架构（工厂模式）**:
```
storage/
├── store.go         # Store 接口（统一契约）
├── factory.go       # NewStore() 自动选择数据库
├── schema/          # 统一 Schema 定义层（2025-12 新增）
│   ├── tables.go    # 表结构定义（DefineXxxTable 函数）
│   └── builder.go   # Schema 构建器（支持 SQLite/MySQL 差异）
├── sql/             # 通用 SQL 实现层（2025-12 重构，消除 467 行重复代码）
│   ├── store_impl.go      # SQLStore 核心实现
│   ├── config.go          # 渠道配置 CRUD
│   ├── apikey.go          # API 密钥 CRUD
│   ├── cooldown.go        # 冷却管理
│   ├── log.go             # 日志存储
│   ├── metrics.go         # 指标统计
│   ├── metrics_filter.go  # 过滤条件交集支持
│   ├── auth_tokens.go     # API 访问令牌
│   ├── admin_sessions.go  # 管理会话
│   ├── system_settings.go # 系统设置
│   ├── redis_sync.go      # Redis 同步
│   └── helpers.go         # 辅助函数
├── sqlite/          # SQLite 特定（仅测试文件）
└── redis/           # Redis 同步
    └── sync.go      # Redis 备份恢复
```

**数据库选择逻辑**:
- 设置 `CCLOAD_MYSQL` 环境变量 → 使用 MySQL
- 未设置 → 使用 SQLite（默认）

**核心表结构**（SQLite 和 MySQL 共用）:
- `channels` - 渠道配置（冷却数据内联，UNIQUE 约束 name）
- `api_keys` - API 密钥（Key 级冷却内联，支持多 Key 策略）
- `logs` - 请求日志（已合并到主数据库）
- `key_rr` - 轮询指针（channel_id → idx）
- `auth_tokens` - 认证令牌（支持费用限额、模型限制、首字节时间记录）
- `admin_sessions` - 管理会话
- `system_settings` - 系统配置（支持热重载）

**架构特性** (✅ 2025-12月优化):
- ✅ **统一SQL层**（重构）：SQLite/MySQL共享`storage/sql/`实现，消除467行重复代码
- ✅ **统一Schema定义**（新增）：`storage/schema/`定义表结构，支持数据库差异
- ✅ 工厂模式统一接口（OCP 原则，易扩展新存储）
- ✅ 冷却数据内联（废弃独立 cooldowns 表，减少 JOIN 开销）
- ✅ 性能索引优化（渠道选择延迟↓30-50%，Key 查找延迟↓40-60%）
- ✅ 复合索引优化（统计查询性能提升）
- ✅ 外键约束（级联删除，保证数据一致性）
- ✅ 多 Key 支持（sequential/round_robin 策略）
- ✅ 自动迁移（启动时自动创建/更新表结构）
- ✅ Token统计增强（支持时间范围选择、按令牌ID分类、缓存优化）

**向后兼容迁移**:
- 自动检测并修复重复渠道名称
- 智能添加 UNIQUE 约束，确保数据完整性
- 启动时自动执行，无需手动干预
- 日志数据库已合并到主数据库（单一数据源）

## 🛡️ 安全考虑

兄弟们，安全这块不能马虎！注意这几点👇

- 生产环境**务必**设置强密码 `CCLOAD_PASS`，别用123456
- 在Web界面 `/web/tokens.html` 配好API令牌，保护你的接口
- API Key只在内存用，日志里不记录，放心
- Token存在浏览器localStorage，24小时过期，安全又方便
- 建议套一层HTTPS反向代理（nginx/Caddy），别裸奔
- Docker镜像用非root用户跑，黑客拿到容器也搞不了大事

### Token 认证系统

基于Token的认证，简单又高效👇

**认证方式**：
- **管理界面**：登录后获取24小时有效期的Token，存储在 `localStorage`
- **API端点**：支持 `Authorization: Bearer <token>` 头认证

**核心特性**：
- ✅ **无状态认证**：Token不依赖服务端Session，水平扩展随便搞
- ✅ **统一认证体系**：API和Web用同一套Token，简单
- ✅ **简洁架构**：纯Token认证，代码又少又稳（KISS原则）
- ✅ **跨域支持**：Token存localStorage，跨域访问完全OK

**使用示例**：

看个例子就懂了👇
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


## 🔄 CI/CD

GitHub Actions全自动化，推个tag就能发版👇

- **触发条件**：推送版本标签（`v*`）或手动触发
- **构建输出**：多架构 Docker 镜像推送到 GitHub Container Registry
- **版本管理**：自动生成语义化版本标签
- **缓存优化**：利用 GitHub Actions 缓存加速构建



## 🤝 贡献

欢迎贡献代码！发现Bug或有新想法？来提Issue或PR吧👇

- 提Issue：https://github.com/caidaoli/ccLoad/issues
- 提PR：Fork项目→改代码→提交PR
- 代码规范：遵循项目现有风格，保持KISS原则

### 故障排除

遇到问题了？常见坑在这里👇

**端口被占用**：

8080端口已经被占了？换个端口或干掉占用的进程👇
```bash
# 查找并终止占用 8080 端口的进程
lsof -i :8080 && kill -9 <PID>
```

**容器问题**：

Docker容器起不来？看看日志找找原因👇
```bash
# 查看容器日志
docker logs ccload -f
# 检查容器健康状态
docker inspect ccload --format='{{.State.Health.Status}}'
```

**配置验证**：

想确认服务启动成功？试试这几个命令👇
```bash
# 测试服务健康状态（轻量级健康检查，<5ms）
curl -s http://localhost:8080/health
# 或查看统计摘要（返回业务数据，50-200ms）
curl -s http://localhost:8080/public/summary
# 检查环境变量配置
env | grep CCLOAD
```

## 📄 许可证

MIT License
