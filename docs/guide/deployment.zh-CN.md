# 部署

**[English](deployment.md) | 简体中文** · [← 返回 README](../../README.zh-CN.md)

选择适合当前环境的部署方式：

| 部署方式 | 难度 | 成本 | 适合谁 | HTTPS | 持久化 |
|---------|------|------|--------|-------|--------|
| 🐳 **Docker** | ⭐⭐ | 需VPS | 生产环境、追求稳定 | 需配置 | ✅ |
| 🤗 **Hugging Face** | ⭐ | **免费** | 个人试用、快速体验 | ✅自动 | ✅ |
| 🔧 **源码编译** | ⭐⭐⭐ | 需服务器 | 开发、定制构建 | 需配置 | ✅ |
| 📦 **二进制** | ⭐⭐ | 需服务器 | 轻量部署 | 需配置 | ✅ |

## 方式一：Docker 部署（推荐）

生产环境建议优先使用 Docker。官方镜像已发布到 GitHub Container Registry，可直接拉取运行。

**使用预构建镜像（推荐）**：
```bash
# 方式 1: 使用 Docker Compose（最简单）
curl -o docker-compose.yml https://raw.githubusercontent.com/caidaoli/ccLoad/master/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/caidaoli/ccLoad/master/.env.docker.example
# 编辑 .env 文件设置 CCLOAD_PASS（必填，未设置服务会拒绝启动）
docker compose up -d

# 方式 2: 直接运行镜像
docker pull ghcr.io/caidaoli/ccload:latest
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -v ccload_data:/app/data \
  ghcr.io/caidaoli/ccload:latest
```

**从源码构建**：

需要审计镜像内容或定制构建时，可从源码构建：
```bash
# 克隆项目
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# 使用 Docker Compose 构建并运行
cp .env.docker.example .env  # 编辑 .env 设置 CCLOAD_PASS
docker compose -f docker-compose.build.yml up -d

# 或手动构建
docker build -t ccload:local .
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -v ccload_data:/app/data \
  ccload:local
```

## 方式二：源码编译

需要本地开发或修改代码时，使用源码编译：

```bash
# 克隆项目
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# 构建项目（默认使用高性能 JSON 库）
go build -tags sonic -o ccload .

# 或使用 Makefile
make build

# 直接运行开发模式
go run -tags sonic .
# 或
make dev
```

## 方式三：二进制下载

不需要 Docker 或 Go 环境时，可直接下载对应平台的二进制文件：

```bash
# 从 GitHub Releases 下载对应平台的二进制文件
wget https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64
chmod +x ccload-linux-amd64
./ccload-linux-amd64
```

存在 Cursor 渠道时，ccLoad 会自动下载锁定版本的 SDK Bridge，校验内置 SHA-256，并原子安装到托管状态目录。离线环境可从 [Cursor SDK Bridge 官方发布页](https://github.com/cursor/sdk-bridge/releases)下载匹配版本，把其中的 `cursor-sdk-bridge` 放到 ccLoad 同目录，或设置 `CURSOR_SDK_BRIDGE_BIN`。

## 方式四：Hugging Face Spaces 部署

Hugging Face Spaces 提供免费的 Docker 托管和自动 HTTPS，适合个人试用与轻量场景。

### 部署步骤

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

   # 把上面的 Dockerfile 放进仓库，然后提交并推送
   git add Dockerfile
   git commit -m "Add Dockerfile for ccLoad deployment"
   git push
   ```

4. **配置环境变量（Secrets）**

   在 Space 设置页面（Settings → Variables and secrets → New secret）添加：

   | 变量名 | 值 | 必填 | 说明 |
   |--------|-----|------|------|
   | `CCLOAD_PASS` | 无 | ✅ **必填** | 管理界面密码 |
   | `CCLOAD_API_TOKENS` | `token1\|生产,token2\|开发` | 可选 | 启动时预置 API 访问令牌 |

   **注意**:
   - API 访问令牌可通过 `CCLOAD_API_TOKENS` 预置，也可在 Web 管理界面 `/web/tokens.html` 配置
   - `PORT` 和 `SQLITE_PATH` 已在 Dockerfile 中设置，无需配置
   - Hugging Face Spaces 重启后 `/tmp` 目录会清空

5. **等待构建和启动**

   推送 Dockerfile 后，Hugging Face 会自动：
   - 拉取预构建镜像（约 30 秒）
   - 启动应用容器（约 10 秒）
   - 总耗时约 1-2 分钟

6. **访问应用**

   构建完成后，通过以下地址访问：
   - **应用地址**: `https://YOUR_USERNAME-ccload.hf.space`
   - **管理界面**: `https://YOUR_USERNAME-ccload.hf.space/web/`
   - **API 端点**: `https://YOUR_USERNAME-ccload.hf.space/v1/messages`

   **首次访问提示**:
   - 如果 Space 处于休眠状态，首次访问需等待 20-30 秒唤醒
   - 后续访问会立即响应

### Hugging Face 部署特点

**优势**:
- ✅ **完全免费**: 公开 Space 永久免费，包含 CPU 和存储
- ✅ **极速部署**: 使用预构建镜像，1-2 分钟即可完成
- ✅ **自动 HTTPS**: 无需配置 SSL 证书，自动提供安全连接
- ✅ **自动重启**: 应用崩溃后自动重启
- ✅ **版本控制**: 基于 Git，方便回滚和协作
- ✅ **简单维护**: 仅需 5 行 Dockerfile，无需管理源码

**限制**:
- ⚠️ **资源限制**: 免费版提供 2 CPU + 16GB RAM
- ⚠️ **休眠策略**: 48 小时无访问会进入休眠，首次访问需等待唤醒（约 20-30 秒）
- ⚠️ **固定端口**: 必须使用 7860 端口
- ⚠️ **公网访问**: Space 默认公开，必须通过 Web 管理界面配置 API 访问令牌才能访问 /v1/* API（否则 401）

### 数据持久化

**重要**: Hugging Face Spaces 的存储策略

由于 Hugging Face Spaces 的限制（`/tmp` 目录重启后清空），**强烈推荐使用外部 MySQL 或 PostgreSQL 数据库**实现完整的数据持久化：

**方案一：混合存储模式（推荐，性能最优）**
- ✅ **本地权威读写**: 配置、凭据、Key、冷却与日志都先提交 SQLite，远程数据库延迟不进入调度热路径
- ✅ **后台最终一致**: 主库写入按实体合并，失败后每 10 秒重试
- ⚠️ **单实例语义**: 不支持多个混合实例或外部程序同时写主库；进程退出可能丢失尚未同步的内存任务
- ✅ **统计缓存**: 智能 TTL 缓存，减少重复聚合查询
- 配置方法: 在 Secrets 中添加一个主库 DSN（`CCLOAD_MYSQL` 或 `CCLOAD_POSTGRES`），再加 `CCLOAD_ENABLE_SQLITE_REPLICA=1`

**Dockerfile**：沿用步骤 3 的 Dockerfile，去掉 `SQLITE_PATH` 一行即可，DSN 在 Secrets 中配置。

**方案二：纯外部数据库模式**
- ✅ **完整持久化**: 渠道配置、日志记录、统计数据全部保留
- ✅ **重启不丢数据**: 数据存储在外部数据库，不受 Space 重启影响
- ⚠️ **数据库延迟**: 统计页面响应时间取决于远程数据库和部署地域
- 配置方法: 在 Secrets 中二选一配置 `CCLOAD_MYSQL` 或 `CCLOAD_POSTGRES`

**推荐的免费 MySQL 服务**:
- [TiDB Cloud Serverless](https://tidbcloud.com/) - 免费 5GB 存储，MySQL 兼容，无连接数限制，推荐首选
- [Aiven for MySQL](https://aiven.io/) - 免费 1GB 存储，支持多区域部署

**MySQL 配置示例（以 TiDB Cloud 为例）**:
1. 注册 [TiDB Cloud](https://tidbcloud.com/) 账户
2. 创建 Serverless Cluster（免费）
3. 获取连接信息，格式为：`user:password@tcp(host:4000)/database?tls=true`
4. 在 Hugging Face Space 的 Secrets 中添加 `CCLOAD_MYSQL` 变量
5. **（可选）启用混合模式**: 添加 `CCLOAD_ENABLE_SQLITE_REPLICA=1` 获得最佳性能
6. 重启 Space，所有数据将自动持久化到 MySQL

**PostgreSQL 配置示例**:
```bash
CCLOAD_POSTGRES=postgres://user:password@host:5432/ccload?sslmode=require
```

支持 URL 和 libpq 关键字 DSN。`CCLOAD_MYSQL` 与 `CCLOAD_POSTGRES` 不能同时配置。

**Dockerfile**：沿用步骤 3 的 Dockerfile，去掉 `SQLITE_PATH` 一行即可，DSN 在 Secrets 中配置。

**方案三：仅本地存储（不推荐）**
- ⚠️ **数据丢失**: Space 重启后 `/tmp` 目录会清空，渠道配置会丢失
- ⚠️ **手动恢复**: 需要重新通过 Web 界面或 CSV 导入配置渠道
- 使用场景: 仅用于临时测试

### 更新部署

由于使用预构建镜像，更新非常简单：

**镜像更新**:
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
如果需要锁定特定版本，把 Dockerfile 的 `FROM` 行改为精确版本 Tag：
```dockerfile
FROM ghcr.io/caidaoli/ccload:v4.10.2
```

## 基本配置

部署完成后，按场景选择 SQLite、MySQL 或 PostgreSQL。MySQL 与 PostgreSQL 互斥。

**SQLite 模式（默认）**：
个人或小团队可优先使用 SQLite，零外部依赖，单文件持久化：
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
生产环境、高并发或多实例部署建议使用 MySQL：
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

**PostgreSQL 模式**：
```bash
# 1. 在 PostgreSQL 中创建数据库和用户

# 2. 设置环境变量
export CCLOAD_PASS=your_admin_password
export CCLOAD_POSTGRES="postgres://user:password@localhost:5432/ccload?sslmode=disable"
export PORT=8080

# 3. 启动服务（自动创建和迁移表结构）
./ccload
```

**Docker + MySQL**:
```bash
# 方式 1: Docker Compose（推荐）
cat > docker-compose.mysql.yml << 'EOF'
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

docker compose -f docker-compose.mysql.yml up -d

# 方式 2: 直接运行（需要已有 MySQL 服务）
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_admin_password \
  -e CCLOAD_MYSQL="user:pass@tcp(mysql_host:3306)/ccload?charset=utf8mb4" \
  ghcr.io/caidaoli/ccload:latest
```

**Docker + PostgreSQL**（需要已有 PostgreSQL 服务）:
```bash
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_admin_password \
  -e CCLOAD_POSTGRES="postgres://user:pass@postgres_host:5432/ccload?sslmode=require" \
  ghcr.io/caidaoli/ccload:latest
```

服务启动后访问：
- 管理界面：`http://localhost:8080/web/`
- API 代理：`POST http://localhost:8080/v1/messages`
- **API 令牌管理**：`http://localhost:8080/web/tokens.html` - 通过 Web 界面配置 API 访问令牌

## Docker 镜像

官方镜像支持多架构：

- **支持架构**：`linux/amd64`, `linux/arm64`
- **镜像仓库**：`ghcr.io/caidaoli/ccload`
- **可用标签**：
  - `latest` - 最新稳定版本
  - `beta` - 最新 Beta 版本
  - `v4.10.2` - 精确稳定版本，和 GitHub Release Tag 保持一致
  - `vX.Y.Z-beta.N` - 精确 Beta 版本，和 GitHub Prerelease Tag 保持一致

官方 GHCR 镜像基于 Debian/glibc，并保持不可变；上游 Cursor SDK Bridge standalone 是 glibc 动态链接程序，不能放进 Alpine/musl 运行。镜像构建会直接从 Cursor 官方发布页拉取并校验锁定版本的 Bridge；GitHub Release 只发布 ccLoad 二进制。容器不做进程内更新；拉取精确版本 Tag 或滚动别名后重建容器即可。

## 镜像标签说明

```bash
# 拉取最新版本
docker pull ghcr.io/caidaoli/ccload:latest

# 拉取指定版本
docker pull ghcr.io/caidaoli/ccload:v4.10.2

# 拉取最新 Beta；要锁定版本时将 beta 替换为已发布的 vX.Y.Z-beta.N Tag
docker pull ghcr.io/caidaoli/ccload:beta

# 使用 Compose 时，将 image 改为 :latest 或 :beta 后应用变更
docker compose pull
docker compose up -d

# 指定架构（Docker 通常自动选择）
docker pull --platform linux/amd64 ghcr.io/caidaoli/ccload:latest
docker pull --platform linux/arm64 ghcr.io/caidaoli/ccload:latest
```

## 自动更新

非容器部署由单一更新管理器负责版本检查、前端版本提示和可选的进程内自动更新。默认启动时检查一次，此后每 12 小时检查一次。`auto_update_channel=stable` 只接收稳定版；`preview` 同时考虑稳定版和测试版，按 SemVer 选择最高有效版本，不会把当前版本或待重启版本降级。两个设置都可以在 Web 管理后台修改；将 `auto_update_interval_hours` 设为 `0` 只关闭定时版本检查——设置旁的 **检测更新** 按钮（`POST /admin/update/check`）在间隔为 `0` 时仍可手动执行完整的检查/校验/替换流程。

稳定版元数据通过配置的发布源解析。测试版发现读取 GitHub Releases Atom feed，其中包含稳定版和测试版，无需使用受速率限制的 REST API。解析出精确 Tag 后，ccLoad 会从配置的下载源获取应用和校验文件；默认顺序是 `gh.monlor.com`、`fastgit.cc`、`ghfast.top` 和 GitHub，SHA256 校验通过后才替换应用可执行文件。Cursor SDK Bridge 独立管理，始终使用官方锁定版本。

官方容器不运行版本检查或进程内更新循环。每个稳定版和 Beta 镜像都直接包含对应 GitHub Release 生成的同版本二进制。稳定版发布精确版本 Tag 和 `latest`，Beta 发布精确测试版 Tag 和滚动 `beta` 别名。修改 Compose 中的镜像标签后，重新拉取并启动容器即可切换版本。

如需使用私有发布镜像，可将 `CCLOAD_RELEASE_BASE_URL` 设置为完整的 latest-download 地址，例如 `https://mirror.example/caidaoli/ccLoad/releases/latest/download`。显式设置后，稳定版元数据和全部发布文件下载都不会追加内置回退源；测试版元数据仍从 GitHub Releases Atom feed 获取。该变量不会设置 `HTTP_PROXY` 或 `HTTPS_PROXY`，因此不会让业务渠道请求经过下载代理。

## 🔄 CI/CD

GitHub Actions 负责自动构建和发布：

- **触发条件**：推送版本标签（`v*`）或手动触发
- **构建输出**：多架构 Docker 镜像推送到 GitHub Container Registry
- **版本管理**：自动生成语义化版本标签
- **缓存优化**：利用 GitHub Actions 缓存加速构建

## 故障排除

常见问题排查：

**端口被占用**：

如果 8080 端口已被占用，修改端口或终止占用进程：
```bash
# 查找并终止占用 8080 端口的进程
lsof -i :8080 && kill <PID>
```

**容器问题**：

Docker 容器启动失败时，先查看日志和健康状态：
```bash
# 查看容器日志
docker logs ccload -f
# 检查容器健康状态
docker inspect ccload --format='{{.State.Health.Status}}'
```

**配置验证**：

用以下命令确认服务状态：
```bash
# 测试服务健康状态（轻量级健康检查）
curl -s http://localhost:8080/health
# 或查看统计摘要（返回业务数据）
curl -s http://localhost:8080/public/summary
# 检查环境变量配置
env | grep CCLOAD
```
