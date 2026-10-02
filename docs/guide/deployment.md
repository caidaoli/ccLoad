# Deployment

**English | [简体中文](deployment.zh-CN.md)** · [← Back to README](../../README.md)

Choose the deployment method that suits you best:

| Method | Difficulty | Cost | Use Case | HTTPS | Persistence |
|--------|------------|------|----------|-------|-------------|
| 🐳 **Docker** | ⭐⭐ | VPS required | Production, high performance | Config required | ✅ |
| 🤗 **Hugging Face** | ⭐ | **Free** | Personal use, quick trial | ✅ Auto | ✅ |
| 🔧 **Source Build** | ⭐⭐⭐ | Server required | Development, customization | Config required | ✅ |
| 📦 **Binary** | ⭐⭐ | Server required | Lightweight, simple setup | Config required | ✅ |

## Method 1: Docker Deployment (Recommended)

**Using pre-built images (Recommended)**:
```bash
# Option 1: Using Docker Compose (Simplest)
curl -o docker-compose.yml https://raw.githubusercontent.com/caidaoli/ccLoad/master/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/caidaoli/ccLoad/master/.env.docker.example
# Edit .env file to set CCLOAD_PASS (required, service exits without it)
docker compose up -d

# Option 2: Run image directly
docker pull ghcr.io/caidaoli/ccload:latest
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -v ccload_data:/app/data \
  ghcr.io/caidaoli/ccload:latest
```

**Building from source**:
```bash
# Clone project
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# Build and run with Docker Compose
cp .env.docker.example .env  # edit .env to set CCLOAD_PASS
docker compose -f docker-compose.build.yml up -d

# Or build manually
docker build -t ccload:local .
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -v ccload_data:/app/data \
  ccload:local
```

## Method 2: Source Build

```bash
# Clone project
git clone https://github.com/caidaoli/ccLoad.git
cd ccLoad

# Build project (uses high-performance JSON library by default)
go build -tags sonic -o ccload .

# Or use Makefile
make build

# Run in development mode
go run -tags sonic .
# Or
make dev
```

## Method 3: Binary Download

```bash
# Download binary for your platform from GitHub Releases
wget https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64
chmod +x ccload-linux-amd64
./ccload-linux-amd64
```

When a Cursor channel exists, ccLoad automatically downloads its pinned SDK Bridge, verifies the embedded SHA-256, and installs it atomically under its managed state directory. For offline installations, download the matching archive from the [official Cursor SDK Bridge releases](https://github.com/cursor/sdk-bridge/releases), place its `cursor-sdk-bridge` executable beside ccLoad, or set `CURSOR_SDK_BRIDGE_BIN`.

## Method 4: Hugging Face Spaces Deployment

Hugging Face Spaces provides free container hosting with Docker support, ideal for personal and small team use.

### Deployment Steps

1. **Login to Hugging Face**

   Visit [huggingface.co](https://huggingface.co) and log into your account

2. **Create New Space**

   - Click "New" → "Space" in the top right
   - **Space name**: `ccload` (or custom name)
   - **License**: `MIT`
   - **Select the SDK**: `Docker`
   - **Visibility**: `Public` or `Private` (private requires paid subscription)
   - Click "Create Space"

3. **Create Dockerfile**

   Create a `Dockerfile` in the Space repository:

   ```dockerfile
   FROM ghcr.io/caidaoli/ccload:latest
   ENV TZ=Asia/Shanghai
   ENV PORT=7860
   ENV SQLITE_PATH=/tmp/ccload.db
   EXPOSE 7860
   ```

   Create via:

   **Method A - Web Interface** (Recommended):
   - Click "Files" tab on Space page
   - Click "Add file" → "Create a new file"
   - Enter `Dockerfile` as filename
   - Paste the content above
   - Click "Commit new file to main"

   **Method B - Git Command Line**:
   ```bash
   # Clone your Space repository
   git clone https://huggingface.co/spaces/YOUR_USERNAME/ccload
   cd ccload

   # Add the Dockerfile shown above, then commit and push
   git add Dockerfile
   git commit -m "Add Dockerfile for ccLoad deployment"
   git push
   ```

4. **Configure Environment Variables (Secrets)**

   In Space settings (Settings → Variables and secrets → New secret):

   | Variable | Value | Required | Description |
   |----------|-------|----------|-------------|
   | `CCLOAD_PASS` | None | ✅ **Required** | Admin interface password |
   | `CCLOAD_API_TOKENS` | `token1\|production,token2\|development` | Optional | Pre-seed API access tokens on startup |

   **Note**: API access tokens can be pre-seeded with `CCLOAD_API_TOKENS` or managed in the Web admin interface `/web/tokens.html`.

5. **Wait for Build and Startup**

   After pushing Dockerfile, Hugging Face will automatically:
   - Pull pre-built image (~30 seconds)
   - Start application container (~10 seconds)
   - Total time ~1-2 minutes

6. **Access Application**

   After build completes, access via:
   - **App URL**: `https://YOUR_USERNAME-ccload.hf.space`
   - **Admin Interface**: `https://YOUR_USERNAME-ccload.hf.space/web/`
   - **API Endpoint**: `https://YOUR_USERNAME-ccload.hf.space/v1/messages`

   **First Access Note**:
   - If Space is sleeping, first access takes 20-30 seconds to wake
   - Subsequent accesses respond immediately

### Hugging Face Deployment Characteristics

**Advantages**:
- ✅ **Completely Free**: Public Spaces are permanently free with CPU and storage
- ✅ **Fast Deployment**: Pre-built image, 1-2 minutes
- ✅ **Auto HTTPS**: No SSL certificate configuration needed
- ✅ **Auto Restart**: Automatic restart after crashes
- ✅ **Version Control**: Git-based, easy rollback and collaboration
- ✅ **Simple Maintenance**: Only 5-line Dockerfile, no source code management

**Limitations**:
- ⚠️ **Resource Limits**: Free tier provides 2 CPU + 16GB RAM
- ⚠️ **Sleep Policy**: 48 hours without access triggers sleep, first access takes ~20-30s to wake
- ⚠️ **Fixed Port**: Must use port 7860
- ⚠️ **Public Access**: Spaces are public by default, must configure API tokens via Web admin to access /v1/* APIs (otherwise 401)

### Data Persistence

**Important**: Hugging Face Spaces Storage Policy

Due to Hugging Face Spaces limitations (`/tmp` directory clears on restart), **we strongly recommend using an external MySQL or PostgreSQL database** for complete data persistence:

**Option 1: Hybrid Storage Mode (Recommended, Best Performance)**
- ✅ **Local authoritative I/O**: Config, credentials, keys, cooldowns, and logs commit to SQLite first, keeping remote latency off the scheduling path
- ✅ **Eventually consistent primary**: Writes are coalesced by entity and retried every 10 seconds after failure
- ⚠️ **Single-instance semantics**: Multiple hybrid writers and external primary writes are unsupported; process exit may lose in-memory pending syncs
- ✅ **Stats caching**: Smart TTL cache reduces repetitive aggregate queries
- Configuration: Add one primary DSN (`CCLOAD_MYSQL` or `CCLOAD_POSTGRES`) plus `CCLOAD_ENABLE_SQLITE_REPLICA=1` in Secrets

**Dockerfile**: reuse the Dockerfile from step 3 without the `SQLITE_PATH` line; the DSN goes in Secrets.

**Option 2: Pure External Database Mode**
- ✅ **Complete Persistence**: Channel configs, logs, and stats all preserved
- ✅ **Restart-Safe**: Data stored externally, unaffected by Space restarts
- ⚠️ **Database Latency**: Stats page latency depends on the remote database and region
- Configuration: Add exactly one of `CCLOAD_MYSQL` or `CCLOAD_POSTGRES` in Secrets

**Recommended Free MySQL Services**:
- [TiDB Cloud Serverless](https://tidbcloud.com/) - Free 5GB storage, MySQL compatible, no connection limits, recommended first choice
- [Aiven for MySQL](https://aiven.io/) - Free 1GB storage, multi-region support

**MySQL Configuration Example (TiDB Cloud)**:
1. Register for [TiDB Cloud](https://tidbcloud.com/) account
2. Create Serverless Cluster (free)
3. Get connection info, format: `user:password@tcp(host:4000)/database?tls=true`
4. Add `CCLOAD_MYSQL` variable in Hugging Face Space Secrets
5. **(Optional) Enable Hybrid Mode**: Add `CCLOAD_ENABLE_SQLITE_REPLICA=1` for best performance
6. Restart Space, all data will auto-persist to MySQL

**PostgreSQL Configuration Example**:
```bash
CCLOAD_POSTGRES=postgres://user:password@host:5432/ccload?sslmode=require
```

URL and libpq keyword DSNs are supported. Do not set `CCLOAD_MYSQL` and `CCLOAD_POSTGRES` at the same time.

**Dockerfile**: reuse the Dockerfile from step 3 without the `SQLITE_PATH` line; the DSN goes in Secrets.

**Option 3: Local Storage Only (Not Recommended)**
- ⚠️ **Data Loss**: `/tmp` clears on Space restart, channel config lost
- ⚠️ **Manual Recovery**: Must re-import via Web interface or CSV
- Use case: Temporary testing only

### Update Deployment

With pre-built images, updates are simple:

**Image Refresh**:
- When new version image (`ghcr.io/caidaoli/ccload:latest`) is released
- Click "Factory rebuild" in Space settings to pull latest image
- Or wait for Hugging Face auto-restart (typically after 48 hours)

**Manual Trigger Update**:
```bash
# Add empty commit to trigger rebuild
git commit --allow-empty -m "Trigger rebuild to pull latest image"
git push
```

**Version Pinning** (Optional):
To lock a specific version, change the `FROM` line of the Dockerfile to an exact version Tag:
```dockerfile
FROM ghcr.io/caidaoli/ccload:v4.10.2
```

## Basic Configuration

Choose SQLite, MySQL, or PostgreSQL based on the deployment shape. MySQL and PostgreSQL are mutually exclusive.

**SQLite Mode (Default)**:
```bash
# Set environment variables
export CCLOAD_PASS=your_admin_password
export PORT=8080
export SQLITE_PATH=./data/ccload.db

# Or use .env file
echo "CCLOAD_PASS=your_admin_password" > .env
echo "PORT=8080" >> .env
echo "SQLITE_PATH=./data/ccload.db" >> .env

# Start service
./ccload
```

**MySQL Mode**:
```bash
# 1. Create MySQL database
mysql -u root -p -e "CREATE DATABASE ccload CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 2. Set environment variables
export CCLOAD_PASS=your_admin_password
export CCLOAD_MYSQL="user:password@tcp(localhost:3306)/ccload?charset=utf8mb4"
export PORT=8080

# Or use .env file
echo "CCLOAD_PASS=your_admin_password" > .env
echo "CCLOAD_MYSQL=user:password@tcp(localhost:3306)/ccload?charset=utf8mb4" >> .env
echo "PORT=8080" >> .env

# 3. Start service (auto-creates tables)
./ccload
```

**PostgreSQL Mode**:
```bash
# 1. Create the database and user in PostgreSQL

# 2. Set environment variables
export CCLOAD_PASS=your_admin_password
export CCLOAD_POSTGRES="postgres://user:password@localhost:5432/ccload?sslmode=disable"
export PORT=8080

# 3. Start service (auto-creates and migrates tables)
./ccload
```

**Docker + MySQL**:
```bash
# Option 1: Docker Compose (Recommended)
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

# Option 2: Direct run (requires existing MySQL service)
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_admin_password \
  -e CCLOAD_MYSQL="user:pass@tcp(mysql_host:3306)/ccload?charset=utf8mb4" \
  ghcr.io/caidaoli/ccload:latest
```

**Docker + PostgreSQL** (requires an existing PostgreSQL service):
```bash
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_admin_password \
  -e CCLOAD_POSTGRES="postgres://user:pass@postgres_host:5432/ccload?sslmode=require" \
  ghcr.io/caidaoli/ccload:latest
```

After service starts, access:
- Admin Interface: `http://localhost:8080/web/`
- API Proxy: `POST http://localhost:8080/v1/messages`
- **API Token Management**: `http://localhost:8080/web/tokens.html` - Configure API access tokens via Web interface

## Docker Images

Project supports multi-arch Docker images:

- **Supported Architectures**: `linux/amd64`, `linux/arm64`
- **Image Registry**: `ghcr.io/caidaoli/ccload`
- **Available Tags**:
  - `latest` - Latest stable version
  - `beta` - Latest Beta version
  - `v4.10.2` - Exact stable version, matching the GitHub Release tag
  - `vX.Y.Z-beta.N` - Exact Beta version, matching the GitHub prerelease tag

The official GHCR runtime image is Debian/glibc-based and immutable because the upstream Cursor SDK Bridge standalone binary is dynamically linked against glibc. Image builds fetch and verify the pinned bridge directly from its official Cursor release; GitHub Releases contain only ccLoad binaries. Exact version Tags are immutable; `latest` and `beta` are rolling aliases. Containers do not update in process; pull the desired Tag and recreate the container.

## Image Tag Guide

```bash
# Pull latest version
docker pull ghcr.io/caidaoli/ccload:latest

# Pull specific version
docker pull ghcr.io/caidaoli/ccload:v4.10.2

# Pull latest Beta; replace beta with a published vX.Y.Z-beta.N Tag to pin it
docker pull ghcr.io/caidaoli/ccload:beta

# With Compose, set image to :latest or :beta, then apply the change
docker compose pull
docker compose up -d

# Specify architecture (Docker usually auto-selects)
docker pull --platform linux/amd64 ghcr.io/caidaoli/ccload:latest
docker pull --platform linux/arm64 ghcr.io/caidaoli/ccload:latest
```

## Auto Updates

For non-container deployments, one update manager owns release checks, version notifications, and optional in-process updates. It checks once at startup and every 12 hours by default. `auto_update_channel=stable` accepts stable releases only; `preview` considers stable and prerelease versions and selects the highest valid SemVer without downgrading the running or pending version. Change both settings from the Web admin settings page. Set `auto_update_interval_hours=0` to disable all scheduled release checks — the **Check for updates** button next to these settings (`POST /admin/update/check`) still runs the complete discovery/verification/replace flow on demand even when the interval is `0`.

Stable metadata is resolved through the configured release sources. Preview discovery reads GitHub's Releases Atom feed, which includes stable and prerelease entries without using the rate-limited REST API. After resolving an exact Tag, ccLoad downloads the application and checksum file from the configured sources—by default `gh.monlor.com`, `fastgit.cc`, `ghfast.top`, then GitHub—and replaces the executable only after SHA256 verification. Cursor SDK Bridge installation is independent and always uses the pinned official Cursor release.

Official containers do not run the release check or in-process update loop. Every stable and Beta image contains the exact binary produced by the matching GitHub Release. Stable releases publish an exact version Tag plus `latest`; Beta releases publish an exact prerelease Tag plus the rolling `beta` alias. Switch the image Tag in Compose, then pull and recreate the container.

To use a private release mirror, set `CCLOAD_RELEASE_BASE_URL` to a complete latest-download base such as `https://mirror.example/caidaoli/ccLoad/releases/latest/download`. An explicit value disables built-in fallback sources for stable metadata and all asset downloads. Preview metadata still comes from GitHub's Releases Atom feed. This setting does not configure `HTTP_PROXY` or `HTTPS_PROXY` for upstream API traffic.

## 🔄 CI/CD

Project uses GitHub Actions for automated CI/CD:

- **Trigger Conditions**: Push version tags (`v*`) or manual trigger
- **Build Output**: Multi-arch Docker images pushed to GitHub Container Registry
- **Version Management**: Auto-generates semantic version tags
- **Cache Optimization**: Uses GitHub Actions cache to accelerate builds

## Troubleshooting

**Port In Use**:
```bash
# Find and kill process using port 8080
lsof -i :8080 && kill <PID>
```

**Container Issues**:
```bash
# View container logs
docker logs ccload -f
# Check container health status
docker inspect ccload --format='{{.State.Health.Status}}'
```

**Config Validation**:
```bash
# Test service health (lightweight health check)
curl -s http://localhost:8080/health
# Or view stats summary (returns business data)
curl -s http://localhost:8080/public/summary
# Check environment variable config
env | grep CCLOAD
```
