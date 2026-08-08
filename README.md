![ccLoad admin dashboard](images/ccload.jpg)

# ccLoad

**AI API gateway for Claude Code, Codex, Gemini, and OpenAI.**

**English | [简体中文](README.zh-CN.md)**

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](https://golang.org)
[![Gin](https://img.shields.io/badge/Gin-v1.12+-blue.svg)](https://github.com/gin-gonic/gin)
[![Docker](https://img.shields.io/badge/Docker-Supported-2496ED.svg)](https://hub.docker.com)
[![Hugging Face](https://img.shields.io/badge/%F0%9F%A4%97%20Hugging%20Face-Spaces-yellow)](https://huggingface.co/spaces)
[![GitHub Actions](https://img.shields.io/badge/CI%2FCD-GitHub%20Actions-2088FF.svg)](https://github.com/features/actions)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> Smart routing | Automatic failover | Model-aware cooldown | Multi-URL scheduling | Protocol transforms | Live monitoring | Cost control

ccLoad removes the operational mess of running multiple AI API upstreams. It keeps Claude Code, Codex, Gemini, and OpenAI-compatible clients on one stable gateway, then handles upstream selection, failover, cooldown, protocol conversion, request visibility, and cost limits in the service instead of in every client script.

## 🤖 Built with Codex and GPT-5.6

During OpenAI Build Week, Codex powered by GPT-5.6 was the primary engineering agent used to:

- Trace request routing, failover, cooldown, protocol conversion, and dashboard flows across the Go backend and embedded web UI.
- Implement and review model-scoped cooldown handling for upstream `5xx`, key-level `429`, model-unavailable `404`, and explicit model-retirement `410` failures without unnecessarily cooling an entire channel.
- Refine the model-status and call-statistics UI, update the English and Chinese documentation, and verify the result with focused Go tests, builds, and browser walkthroughs.
- Prepare the reproducible demo and Devpost submission while keeping architecture, security, and final-review decisions under human control.

GPT-5.6 is also integrated into the product itself: ccLoad exposes GPT-5.6 through OpenAI-compatible and Codex Responses endpoints, includes Sol, Terra, and Luna model presets, calculates their standard, priority, flex, cached-token, and long-context costs, and applies routing and model-scoped cooldown decisions to them like any other configured upstream model.

The repository's `AGENTS.md` and `CLAUDE.md` provide persistent engineering constraints so Codex works against the same KISS-first review and testing rules in every session.

## 🎯 What ccLoad Solves

Common failure modes when you run several AI API channels:

- **Manual channel switching**: Different keys, validity windows, quotas, and upstream URLs quickly become hard to manage.
- **Rate limits and upstream failures**: `429`, `502`, `504`, expired keys, and overloaded providers should not stop the client workflow.
- **Opaque request status**: Without live request visibility, long streaming requests become guesswork.
- **HTTP 200 with error content**: Some upstreams return a successful HTTP status while the response body is an actual error.
- **Cost drift**: Shared gateways need per-channel and per-token limits, not spreadsheet accounting after the bill arrives.

ccLoad handles those cases with:

- **Smart routing**: High-priority channels are selected first; channels at the same priority use smooth weighted round-robin.
- **Automatic failover**: Failed keys, models, channels, and URLs are skipped according to the classified error scope.
- **Model-aware cooldown**: Structured `model_cooldown` responses, upstream HTTP 5xx failures, key-level 429 rate limits, model-unavailable 404 errors, and explicit model-retirement 410 errors all cool only the actual upstream model first; other models on the same channel remain available. The channel is promoted to cooldown only after every configured model or every enabled key is cooling.
- **Multi-URL scheduling**: A single channel can use multiple upstream URLs, weighted by observed latency and health.
- **Per-URL protocol routing**: Each URL can declare the upstream wire protocols it accepts. Explicit declarations route directly; an empty declaration tries the client protocol first and caches the working fallback.
- **Responses WebSocket bridging**: Authenticated Codex clients can keep a downstream WebSocket while each candidate uses native Codex WebSocket or the existing HTTP/SSE transport.
- **Live monitoring**: Active requests, logs, token usage, TTFB, cost, and upstream details are visible in the web dashboard.
- **Soft-error detection**: HTTP 200 responses that are actually errors trigger the same failover path as regular upstream failures. Common cases include:
  - JSON responses containing `{"error": {...}}` structure
  - Responses with `type` field set to `"error"`
  - Explicit rate limits in SSE `error` events (`rate_limit_exceeded` / `too_many_requests`) are handled as `429`
  - Plain text messages like `"当前模型负载过高"` / `"Current model load too high"` (load warnings)

## ✨ Key Features

- 🚀 **High-Performance Architecture** - Gin framework, 1000+ concurrent connections, high-performance caching
- 🧮 **Local Token Counting** - API-compliant local token estimation, <5ms response, 93%+ accuracy, supports large-scale tool scenarios
- 🎯 **Smart Error Classification** - Distinguishes Key/Model/Channel/Client errors, soft error detection (200 masquerading as error), SSE rate-limit errors as 429, 1308 quota handling
- 🔀 **Smart Routing** - Priority + smooth weighted round-robin channel selection, **pre-filters cooled channels**, multi-key load balancing, **health-based dynamic sorting** (confidence factor prevents small sample over-penalization)
- 🛡️ **Failover** - Key, model, and channel failures share one exponential-backoff policy; explicit upstream reset deadlines take priority, and model-scoped failures switch channels without cooling the whole channel
- 🔒 **Race-Safe** - Key selector race condition protection, startup config validation, automatic resource cleanup
- 📊 **Real-time Monitoring** - Built-in trend analysis, logging, and stats dashboard, **Token usage stats** with time range selection and per-token classification
- 🎯 **Transparent Proxy** - Supports Claude Code, Codex, Gemini, and OpenAI compatible APIs with smart auth detection
- 🔌 **Responses WebSocket** - Downstream Codex WebSocket sessions bridge to native Codex WebSocket or HTTP/SSE candidates with transcript-aware failover
- 📦 **Single Binary Deployment** - No external dependencies, embedded SQLite included
- 🔒 **Secure Authentication** - Token-based admin interface and API access control
- 🏷️ **Build Tags** - GOTAGS support, high-performance JSON library enabled by default
- 🐳 **Docker Support** - Multi-arch images (amd64/arm64), automated CI/CD
- ☁️ **Cloud Native** - Container deployment support, GitHub Actions auto-build
- 🤗 **Hugging Face** - One-click deployment to Hugging Face Spaces, free hosting
- 💰 **Cost Limits** - Per-channel daily cost limits, per-token cost limits
- 🚦 **Channel RPM Limits** - Per-channel rolling 60-second request caps, 0=unlimited
- 🚧 **Channel Concurrency Limits** - Per-channel in-flight request caps, 0=unlimited
- 🔐 **Token Restrictions** - Per-token cost limits, model restrictions, channel allowlist/denylist, and concurrency caps for fine-grained access control
- ⏱️ **TTFB Monitoring** - Streaming request first byte time tracking for upstream latency diagnosis
- 🌐 **Multi-URL Load Balancing** - Multiple URLs per channel with latency-weighted random selection
- 💵 **service_tier Pricing** - OpenAI priority/flex/default tier multipliers for accurate cost accounting
- 🖼️ **Image Tool Billing** - Responses image_generation/gpt-image-2 cost accounting
- 📉 **Tiered Pricing** - GPT-5.4/Qwen-Plus/Gemini long-context step pricing, auto-applies lower rate at token thresholds
- 🔄 **Per-URL Protocol Routing** - Explicit Anthropic/OpenAI/Codex/Gemini capability per URL, with native-first automatic detection when left empty
- 💬 **Conversational Model Testing** - Channel/model/chat testing modes with image upload, reasoning level, built-in search, and chat export
- 🔍 **Debug Logs** - Upstream request/response raw data capture with sensitive header masking, essential for troubleshooting
- 🕐 **Scheduled Checks** - Background periodic channel availability probing, auto-detect failed channels
- 🔄 **Release Channels** - Stable updates by default, with an opt-in preview channel; check interval is configurable from the admin settings page
- 🧩 **Custom Request Rules** - Per-channel HTTP header & JSON body rewriting (remove/override/append), with auth header protection, CRLF guard, and capacity caps
- 🎛️ **Log Column Customization** - Show/hide table columns per preference, settings persist in browser localStorage

## 🏗️ Architecture Overview

Every channel accepts all four client protocols. Upstream protocol selection is controlled by `protocol_transform_mode` and each structured URL's `protocols` declaration. `upstream` is strict client-protocol passthrough. `auto` tries the client protocol first, then probes OpenAI → Anthropic → Codex → Gemini while skipping the protocol already attempted, and advances only after an uncommitted capability error. `local` prioritizes URLs with explicit declarations and follows each URL's declared order; only when every URL is undeclared does it try Anthropic → Codex → OpenAI → Gemini. Incompatible URLs are skipped without a request or cooldown. Successful automatic detection is cached per URL and request family until restart or channel configuration changes; an all-unsupported result is probed again after 10 minutes.

```mermaid
graph TB
    subgraph "Client"
        A[Claude Code / Codex / Gemini / OpenAI Client]
    end

    subgraph "ccLoad Service"
        B[HTTP Proxy]
        C[Authentication + Route Dispatch]
        D[Channel Selector<br/>Priority + Smooth Weighted RR]
        E[Protocol Registry<br/>Native Bypass / Local Transform]
        F[URL Selector<br/>Explore + 1/EWMA Weighting]
        G[(Storage Factory<br/>SQLite / MySQL / PostgreSQL)]
        H[Logs + Metrics + Cost Control]

        A --> B --> C --> D --> E --> F
        D <--> G
        H <--> G
        B --> H
    end

    subgraph "Upstream Services"
        U1[Anthropic]
        U2[OpenAI-compatible]
        U3[Gemini]
        U4[Codex Responses]
    end

    F --> U1
    F --> U2
    F --> U3
    F --> U4
    U1 -. JSON / SSE .-> E
    U2 -. JSON / SSE .-> E
    U3 -. JSON / SSE .-> E
    U4 -. JSON / SSE .-> E
    E -. Client Protocol .-> B

    style B fill:#4F46E5,stroke:#000,color:#fff
    style D fill:#059669,stroke:#000,color:#fff
    style E fill:#0EA5E9,stroke:#000,color:#fff
```

## 🚀 Quick Start

Choose the deployment method that suits you best:

| Method | Difficulty | Cost | Use Case | HTTPS | Persistence |
|--------|------------|------|----------|-------|-------------|
| 🐳 **Docker** | ⭐⭐ | VPS required | Production, high performance | Config required | ✅ |
| 🤗 **Hugging Face** | ⭐ | **Free** | Personal use, quick trial | ✅ Auto | ✅ |
| 🔧 **Source Build** | ⭐⭐⭐ | Server required | Development, customization | Config required | ✅ |
| 📦 **Binary** | ⭐⭐ | Server required | Lightweight, simple setup | Config required | ✅ |

### Method 1: Docker Deployment (Recommended)

**Using pre-built images (Recommended)**:
```bash
# Option 1: Using docker-compose (Simplest)
curl -o docker-compose.yml https://raw.githubusercontent.com/caidaoli/ccLoad/master/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/caidaoli/ccLoad/master/.env.docker.example
# Edit .env file to set CCLOAD_PASS (required, service exits without it)
docker-compose up -d

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

# Build and run with docker-compose
cp .env.docker.example .env  # edit .env to set CCLOAD_PASS
docker-compose -f docker-compose.build.yml up -d

# Or build manually
docker build -t ccload:local .
docker run -d --name ccload \
  -p 8080:8080 \
  -e CCLOAD_PASS=your_secure_password \
  -v ccload_data:/app/data \
  ccload:local
```

### Method 2: Source Build

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

### Method 3: Binary Download

```bash
# Download binary for your platform from GitHub Releases
wget https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64
chmod +x ccload-linux-amd64
./ccload-linux-amd64
```

### Method 4: Hugging Face Spaces Deployment

Hugging Face Spaces provides free container hosting with Docker support, ideal for personal and small team use.

#### Deployment Steps

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

   # Create Dockerfile
   cat > Dockerfile << 'EOF'
   FROM ghcr.io/caidaoli/ccload:latest
   ENV TZ=Asia/Shanghai
   ENV PORT=7860
   ENV SQLITE_PATH=/tmp/ccload.db
   EXPOSE 7860
   EOF

   # Commit and push
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
   - Total time ~1-2 minutes (3-5x faster than source build)

6. **Access Application**

   After build completes, access via:
   - **App URL**: `https://YOUR_USERNAME-ccload.hf.space`
   - **Admin Interface**: `https://YOUR_USERNAME-ccload.hf.space/web/`
   - **API Endpoint**: `https://YOUR_USERNAME-ccload.hf.space/v1/messages`

   **First Access Note**:
   - If Space is sleeping, first access takes 20-30 seconds to wake
   - Subsequent accesses respond immediately

#### Hugging Face Deployment Characteristics

**Advantages**:
- ✅ **Completely Free**: Public Spaces are permanently free with CPU and storage
- ✅ **Fast Deployment**: Pre-built image, 1-2 minutes (3-5x faster than source build)
- ✅ **Auto HTTPS**: No SSL certificate configuration needed
- ✅ **Auto Restart**: Automatic restart after crashes
- ✅ **Version Control**: Git-based, easy rollback and collaboration
- ✅ **Simple Maintenance**: Only 5-line Dockerfile, no source code management

**Limitations**:
- ⚠️ **Resource Limits**: Free tier provides 2 CPU + 16GB RAM
- ⚠️ **Sleep Policy**: 48 hours without access triggers sleep, first access takes ~20-30s to wake
- ⚠️ **Fixed Port**: Must use port 7860
- ⚠️ **Public Access**: Spaces are public by default, must configure API tokens via Web admin to access /v1/* APIs (otherwise 401)

#### Data Persistence

**Important**: Hugging Face Spaces Storage Policy

Due to Hugging Face Spaces limitations (`/tmp` directory clears on restart), **we strongly recommend using an external MySQL or PostgreSQL database** for complete data persistence:

**Option 1: Hybrid Storage Mode (Recommended, Best Performance)**
- ✅ **Ultra-fast queries**: Reads go through local SQLite, avoiding remote database latency
- ✅ **Restart-safe**: Durable data is stored in MySQL/PostgreSQL and restored on startup
- ✅ **Stats caching**: Smart TTL cache reduces repetitive aggregate queries
- Configuration: Add one primary DSN (`CCLOAD_MYSQL` or `CCLOAD_POSTGRES`) plus `CCLOAD_ENABLE_SQLITE_REPLICA=1` in Secrets

**Dockerfile Example (Hybrid Mode)**:
```dockerfile
FROM ghcr.io/caidaoli/ccload:latest
ENV TZ=Asia/Shanghai
ENV PORT=7860
# Configure in Secrets: CCLOAD_MYSQL or CCLOAD_POSTGRES, plus CCLOAD_ENABLE_SQLITE_REPLICA=1
EXPOSE 7860
```

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

**Dockerfile Example (Pure External Database)**:
```dockerfile
FROM ghcr.io/caidaoli/ccload:latest
ENV TZ=Asia/Shanghai
ENV PORT=7860
# Configure CCLOAD_MYSQL or CCLOAD_POSTGRES in Secrets; SQLITE_PATH is not required
EXPOSE 7860
```

**Option 3: Local Storage Only (Not Recommended)**
- ⚠️ **Data Loss**: `/tmp` clears on Space restart, channel config lost
- ⚠️ **Manual Recovery**: Must re-import via Web interface or CSV
- Use case: Temporary testing only

#### Update Deployment

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
To lock specific version, modify Dockerfile:
```dockerfile
FROM ghcr.io/caidaoli/ccload:v2.44.1  # Specify version
ENV TZ=Asia/Shanghai
ENV PORT=7860
ENV SQLITE_PATH=/tmp/ccload.db
EXPOSE 7860
```

### Basic Configuration

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
# Option 1: docker-compose (Recommended)
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

## 📖 Usage Guide

### API Proxy

**Claude API Proxy (Requires Auth)**:

First, configure API access token in Web admin interface `http://localhost:8080/web/tokens.html`, then use that token to access API:

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -H "x-api-key: your-claude-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-sonnet-4-6",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "Hello, Claude!"
      }
    ]
  }'
```

**OpenAI Compatible API Proxy (Chat Completions)**:

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

**Codex Responses WebSocket**:

The downstream and upstream WebSockets are independent. Authenticated clients can always upgrade `GET /v1/responses` or the Codex direct-route alias `GET /backend-api/codex/responses`; a channel's `websockets` field only controls whether ccLoad tries a native Codex upstream WebSocket. Channels without that field still participate through the HTTP/SSE bridge and remain eligible for failover.

In `/web/channels.html`, select a channel with a Codex-capable URL, enable **Native WebSocket**, and run **Probe**. For the Admin API, the relevant fields are shown below. Keep the URL as an `http://` or `https://` URL; ccLoad converts the scheme to `ws://` or `wss://` for native upstream WebSocket requests:

```json
{
  "urls": [{"url": "https://upstream.example.com", "protocols": ["codex"]}],
  "websockets": true
}
```

For example, connect to the downstream endpoint with `websocat`:

```bash
websocat \
  -H='Authorization: Bearer your-api-token' \
  -H='Session-Id: stable-conversation-id' \
  ws://localhost:8080/v1/responses
```

Send text frames after connecting. The first turn must use `response.create` and include `model`; later turns may use `response.append` with the previous response's `response.id`:

```json
{"type":"response.create","model":"your-model","input":[{"type":"message","role":"user","content":"Hello"}]}
```

```json
{"type":"response.append","previous_response_id":"resp_xxx","input":[{"type":"message","role":"user","content":"Continue"}]}
```

Read Responses events until `response.completed`, `response.done`, `response.incomplete`, `response.failed`, or `error`. Only text frames are accepted; binary frames receive an `unsupported_frame` error.

Failover applies only to upstream errors classified as retryable key-, model-, or channel-level failures. Client input errors, unrepresentable protocol conversions, and oversized messages do not fail over. Switching across keys, URLs, channels, or transports occurs only before any visible non-heartbeat event has been committed downstream. One same-upstream native WebSocket reconnect uses the separate semantic boundary described below.

| Current upstream | Next action | ccLoad behavior | Client behavior |
|---|---|---|---|
| HTTP/SSE fails before a visible event is committed | Try another HTTP/SSE candidate | Switches internally and replays the complete transcript | None |
| Native WS disconnect or `previous_response_not_found` before a semantic event | Reconnect the same upstream | Reconnects once internally and replays the complete transcript | None |
| Native WS fails before a visible event is committed | Switch to HTTP/SSE or another native WS candidate | Switches internally and replays the complete transcript | None |
| Native WS handshake rejection or EOF | Fall back to HTTP/SSE on the same channel, key, and URL | Falls back internally and replays the complete transcript | None |
| HTTP/SSE | The next candidate is native WS | Sends `502/server_error/upstream_unavailable`, then closes downstream with code `1011` | Reconnect with the same session hint and send the complete conversation input without `previous_response_id` |

For the same-upstream native WebSocket reconnect, `response.created`, `response.queued`, and `response.in_progress` are non-semantic, so ccLoad may still reconnect once after those events; every other event crosses that reconnect boundary. Those three lifecycle events are still visible events committed downstream, so they do not imply that cross-candidate failover remains available. Once text, reasoning, a tool call, or another actual output has been forwarded, ccLoad does not switch or replay, avoiding duplicate output, tool calls, and charges. Oversized messages close with code `1009` and do not fail over.

`upstream_connection_reuse_limit_seconds` limits how long upstream HTTP/1.1, HTTP/2, and WebSocket connections remain reusable, including connections in channel proxy pools. The default `0` leaves reuse unlimited. When a connection reaches a positive limit, it stops accepting new requests; an idle connection closes immediately, while an active request or turn finishes before closure. The next request opens a new physical connection. A native WebSocket reconnect replays the complete session transcript because an upstream Response ID is scoped to the physical WebSocket connection; this planned rotation is not reported as a request failure and does not cool down the channel.

Reconnects must use the same API token and stable execution headers. `Session-Id` identifies the top-level Codex session; when `Thread-Id` is present, ccLoad combines both headers so the parent and every subagent thread own independent transcripts, Response IDs, and turn locks. Clients without `Thread-Id` retain the `Session-Id`-only contract. `prompt_cache_key`, body `session_id`, and other cache-routing hints do not identify an execution session and never serialize or share local conversation state. An execution session is in-memory and process-local: new installations retain at most 256 sessions with a process-wide transcript payload budget of 256 MiB. Existing database records are not migrated. The idle TTL remains 15 minutes by default (10 minutes is suitable for small-memory hosts). After all downstream attachments have been gone for five minutes, the one-minute cleanup loop closes the physical upstream connection, so actual reclamation takes about 5–6 minutes while the transcript remains until the session TTL. A stable session and its committed transcript are never evicted by session-capacity or memory-budget pressure before that TTL expires. When the session ceiling is full, only a new session identity is rejected; an existing stable session may continue. Once the committed payload is over budget, every new turn, including turns on existing sessions, is rejected before upstream work starts. Both limits use a WebSocket `429/rate_limit_error/rate_limit` event; retry after TTL reclamation, or change the setting and restart. A restart loses in-memory sessions, so the client must then resend the complete conversation input without `previous_response_id`.

The transcript budget is an admission threshold, not a strict allocation cap: turns already admitted are allowed to complete and commit. The finite worst-case overshoot is `responses_ws_max_sessions × max_body_bytes` in addition to the configured budget. Process restarts do not restore sessions or cumulative session metrics. Multi-instance deployments need sticky routing so reconnects reach the same instance. Otherwise, the client must send the complete conversation input without `previous_response_id`. Adjust session count, TTL, and transcript budget with `responses_ws_max_sessions`, `responses_ws_session_ttl_minutes`, and `responses_ws_max_transcript_bytes` in system settings. `GET /admin/runtime-metrics` reports the current effective payload as `transcript_bytes`; it excludes the Go runtime, WebSocket buffers, and temporary request-processing objects. The same response also exposes cumulative `ttl_expired`, `capacity_rejected`, `budget_rejected`, and `previous_response_misses` counters for the current process.

**Codex Alpha Search (Native Passthrough Only)**:

`POST /v1/alpha/search` accepts the native Codex search payload. The `model` field is optional. This request family has no local conversion path: ccLoad tries the native endpoint, caches endpoint-missing responses per URL, and moves to the next URL or channel.

```bash
curl -X POST http://localhost:8080/v1/alpha/search \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
  -d '{
    "query": "golang channels"
  }'
```

For a regular channel base URL, ccLoad appends `/v1/alpha/search`. For an exact URL, set `exact: true` and make `url` point to the complete endpoint, for example `{"url":"https://upstream.example.com/v1/alpha/search","exact":true,"protocols":["codex"]}`. Responses-only fields `prompt_cache_key` and `prompt_cache_retention` are removed before forwarding.

### Local Token Counting

Quickly estimate request token consumption (no upstream API call needed):

```bash
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ],
    "system": "You are a helpful assistant."
  }'

# Response example
# {
#   "input_tokens": 28
# }
```

**Features**:
- ✅ Compliant with Anthropic official API spec
- ✅ Local computation, <5ms response, no API quota consumption
- ✅ 93%+ accuracy (compared to official API)
- ✅ Supports system prompts, tool definitions, large-scale tool scenarios
- ✅ Requires auth token (configure at `/web/tokens.html`)

### Channel Management

Manage channels via Web interface `/web/channels.html` or API:

```bash
# Add a channel with per-URL protocol capabilities
curl -X POST http://localhost:8080/admin/channels \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Claude-API",
    "api_key": "sk-ant-api03-xxx",
    "urls": [
      {"url": "https://api.anthropic.com", "protocols": ["anthropic"]},
      {"url": "https://api2.anthropic.com"}
    ],
    "protocol_transform_mode": "auto",
    "priority": 10,
    "rpm_limit": 0,
    "max_concurrency": 0,
    "models": [{"model": "claude-sonnet-4-6"}, {"model": "claude-opus-4-6"}],
    "enabled": true
  }'
```

> **Protocol behavior**: Each `urls` entry may list `protocols` (`anthropic`, `codex`, `openai`, `gemini`). A non-empty list is authoritative. `upstream` only passes through the client protocol; `auto` starts with the client protocol, then detects OpenAI → Anthropic → Codex → Gemini without retrying the client protocol; `local` prefers declared URLs and their configured protocol order. If every URL is undeclared in `local` mode, ccLoad tries Anthropic → Codex → OpenAI → Gemini.

> **Multi-URL Note**: `urls` is an ordered array of `{url, exact, protocols}` objects. `exact: true` means the URL is already the complete upstream request URL. The system uses latency-weighted selection and independent URL cooldown; local mode first partitions explicitly declared URLs ahead of automatic ones while preserving order inside each group.

> **Model Entry Note**: each `models` element is `{model, redirect_model, disabled}`. `redirect_model` rewrites the model name sent upstream while clients keep requesting the original name. `disabled: true` removes that model from the channel entirely — it stops being advertised, matched (exact or fuzzy), and cooled down, without deleting the entry. When you refresh the model list in `replace` mode, existing disabled flags are carried over to the newly fetched entries by original name, normalized alias, and redirect target, so a refresh does not silently re-enable models you turned off.

> **Independent-key relay fallback**: In the channel editor, open **Advanced → Other** and enable **Try another key on failure** only when the channel's Keys reach independent upstream providers behind the same relay. For retryable model- or channel-level upstream failures (such as 5xx, connection errors, and first-byte timeouts), ccLoad then cools the current Key and tries another Key in that channel before moving to another channel. The option is off by default, preserving the normal model/channel cooldown behavior.

> **RPM Limit Note**: `rpm_limit` is a per-channel request cap over a rolling 60-second window; `0` means unlimited. Proxy forwarding, manual tests, single-URL tests, and scheduled checks all count toward the cap. Multi-URL failover counts each actual upstream HTTP request. The counter is in-memory: restart clears it, and multiple instances count independently.

> **Concurrency Limit Note**: `max_concurrency` is a per-channel cap on simultaneous in-flight upstream requests; `0` means unlimited. A slot is acquired before the upstream request starts and released when the response body is closed, so streaming requests hold the slot until the stream ends. Over-limit channels are skipped without cooldown. The counter is in-memory and per instance.

### Custom Request Rules (Advanced)

The "Advanced" button in the channel editor opens a secondary modal that lets you rewrite the **HTTP headers** and **JSON request body** forwarded upstream at channel granularity. Typical use cases include `User-Agent` override, forcing API version headers, or tweaking fields like `thinking` / `max_tokens`. Rules apply in configured order and take effect for all subsequent requests on that channel as soon as they are saved.

**Action matrix**:

| Target | `remove` | `override` | `append` |
|---|---|---|---|
| HTTP Header | Delete the named header (supports token-level removal on multi-value headers such as `Anthropic-Beta`) | `Header.Set` replaces all values | `Header.Add` appends a value (multi-value semantics) |
| JSON Body | Delete a field/array element by dotted path | Set the value at a path, creating intermediate nodes as needed | Not supported (ambiguous in JSON) |

**JSON path syntax**:
- Dotted path + numeric array index: `thinking.budget_tokens`, `messages.0.role`, `generation_config.temperature`
- Values accept any JSON literal: number `0.7`, boolean `true`, string `"claude-opus-4-6"`, object `{"type":"adaptive"}`, array `["a","b"]`

**Safety constraints** (hard-enforced server-side even if the frontend is bypassed):
- **Auth header blacklist**: any rule targeting `Authorization`, `x-api-key`, or `x-goog-api-key` (case-insensitive) is silently ignored and logged via `slog.Warn`
- **CRLF injection guard**: header names/values must not contain `\r\n`
- **Non-JSON body passthrough**: requests without `application/json` content type, empty bodies, or bodies that fail to deserialize are forwarded untouched without blocking
- **Capacity caps**: ≤ 32 header rules and ≤ 32 body rules per channel, each value ≤ 8 KB; violations return HTTP 400

**Typical example**:
```jsonc
{
  "custom_request_rules": {
    "headers": [
      { "action": "override", "name": "User-Agent", "value": "claude-cli/1.0 (custom)" },
      { "action": "remove",   "name": "Anthropic-Beta", "value": "context-1m-2025-08-07" },
      { "action": "append",   "name": "Accept", "value": "application/json" }
    ],
    "body": [
      { "action": "override", "path": "thinking", "value": {"type":"adaptive"} },
      { "action": "override", "path": "max_tokens", "value": 4096 },
      { "action": "remove",   "path": "stop_sequences" }
    ]
  }
}
```

> **Interaction with built-in logic**: Custom rules run **after** the anyrouter `anthropic-beta` injection and anyrouter adaptive-thinking fallback, so they can override or remove those fields. Generated Anthropic requests use `thinking.type=adaptive` plus `output_config.effort` for thinking depth; anyrouter `/v1/messages` additionally fills missing thinking and normalizes legacy `thinking.type=enabled`. Authentication headers remain unmodifiable at all times.

### Batch Data Management

Supports CSV format for channel config import/export:

**Export Config**:
```bash
# Web interface: Visit /web/channels.html, click "Export CSV" button
# API call:
curl -H "Authorization: Bearer your_token" \
  http://localhost:8080/admin/channels/export > channels.csv
```

**Import Config**:
```bash
# Web interface: Visit /web/channels.html, click "Import CSV" button
# API call:
curl -X POST -H "Authorization: Bearer your_token" \
  -F "file=@channels.csv" \
  http://localhost:8080/admin/channels/import
```

**CSV Format Example**:
```csv
name,api_key,urls,priority,models,enabled
Claude-API-1,sk-ant-xxx,"[{""url"":""https://api.anthropic.com"",""protocols"":[""anthropic""]}]",10,claude-sonnet-4-6,true
Claude-API-2,sk-ant-yyy,"[{""url"":""https://api.anthropic.com""}]",5,claude-opus-4-6,true
```

**Features**:
- Auto column name mapping (Chinese/English)
- Smart data validation with error messages
- Incremental import and overwrite update
- UTF-8 encoding, Excel compatible

## 📊 Monitoring Metrics

Check out the awesome admin dashboard 👇

![ccLoad Dashboard](images/ccload-dashboard.jpeg)
![ccLoad Logs](images/ccload-logs.jpg)
*Real-time Monitoring Dashboard: Claude Code, Codex, OpenAI, and Gemini platform metrics at a glance*

**Core Features**:
- 📈 **24-hour Trend Charts** - Request volumes clearly visualized with peaks and valleys
- 🔴 **Real-time Error Logs** - Instantly detect which channel has issues
- 📊 **Channel Call Statistics** - See which channels are performing well with data-backed insights
- 💬 **Model Testing Workbench** - Test by channel, by model, or in a chat-style model testing workflow:
  - Upload or paste images in chat mode to verify multimodal requests directly
  - Toggle reasoning level, built-in search, and streaming to inspect transformed upstream behavior
  - Export conversations as Markdown or HTML for review, incident notes, or regression records
- ⚡ **Performance Metrics** - Latency, success rates, and bottleneck detection
- 💰 **Token Usage Stats** - Know exactly where your budget goes:
  - Custom time range selector for flexible analysis
  - Per API token ID classification for multi-tenant billing
  - Supports Gemini/OpenAI cache token visualization

- 🎛️ **Log Column Customization** - Click the gear icon to show/hide columns, settings auto-saved to browser

**UI Highlights**:
- 🎨 Modern gradient purple theme for comfortable viewing
- 📱 Responsive design works great on mobile and desktop
- ⚡ Real-time data refresh without manual page reload
- 📊 Multi-dimensional stat cards show key metrics on one screen
  - Cached query optimization
  - Gemini/OpenAI Cache Token (Cache Read) display

## 🔧 Tech Stack

### Core Dependencies

| Component | Version | Purpose | Performance Advantage |
|-----------|---------|---------|----------------------|
| **Go** | 1.26.0+ | Runtime | Native concurrency, modern toolchain |
| **Gin** | v1.12.0 | Web Framework | High-performance HTTP routing |
| **modernc/sqlite** | v1.54.0 | Embedded Database | Pure Go, zero CGO dependency, single file (default) |
| **MySQL** | v1.10.0 | RDBMS | Optional, for high-concurrency production |
| **PostgreSQL (pgx)** | v5.10.0 | RDBMS | Optional, supports URL and libpq DSNs |
| **Sonic** | v1.15.2 | JSON Library | 2-3x faster than stdlib |
| **gjson / sjson** | v1.19.0 / v1.2.5 | Protocol JSON transforms | Targeted reads and writes without generic map conversion |
| **godotenv** | v1.5.1 | Env Config | Simplified config management |

### Architecture Features

**Modular Architecture**:
- **Proxy Module Split** (SRP):
  - `proxy_handler.go`: HTTP entry, concurrency control, route selection
  - `proxy_forward.go`: Core forwarding logic, request building, response handling
  - `proxy_error.go`: Error handling, cooldown decisions, retry logic
  - `proxy_util.go`: Constants, type definitions, utility functions
  - `proxy_stream.go`: Streaming responses, first byte detection
  - `proxy_gemini.go`: Gemini API special handling
  - `proxy_sse_parser.go`: SSE parser (defensive handling, Gemini/OpenAI cache token parsing)
  - `proxy_debug.go`: Upstream request/response debug capture (with sensitive header masking)
- **Admin Module Split** (SRP):
  - `admin_channels.go`: Channel CRUD
  - `admin_stats.go`: Stats analysis API
  - `admin_cooldown.go`: Cooldown management API
  - `admin_csv.go`: CSV import/export
  - `admin_types.go`: Admin API type definitions
  - `admin_auth_tokens.go`: API access token CRUD (with token stats, cost limits, model/channel restrictions, concurrency limits)
  - `admin_settings.go`: System settings management
  - `admin_models.go`: Model list management
  - `admin_testing.go`: Channel testing with an explicit client request protocol
  - `admin_debug_log.go`: Debug log API (sensitive header masking + base64 binary encoding)
  - `channel_check_scheduler.go`: Scheduled channel check scheduler
  - `detection_log.go`: Detection result to LogEntry builder
- **Protocol Transform System**:
  - `protocol/types.go`: Four protocol definitions (Anthropic/OpenAI/Gemini/Codex)
  - `protocol/registry.go`: Contract boundary for request, streaming response, and non-stream response transforms; same-protocol traffic bypasses conversion
  - `protocol/builtin/register.go`: Registers all 12 directed cross-protocol pairs
  - `protocol/builtin/cliproxy_adapter.go`: ccLoad-owned request validation, JSON/SSE normalization, and stream framing
  - `protocol/cliproxy/`: In-tree snapshot of the pure [CLIProxyAPI](https://github.com/caidaoli/CLIProxyAPI) conversion core; provenance and synchronization rules live in [`UPSTREAM.md`](internal/protocol/cliproxy/UPSTREAM.md)
  - Upstream refresh workflow: invoke `$sync-cliproxy-core` in Codex or `/sync-cliproxy-core` in Claude Code; both resolve to the same repository Skill under `.agents/skills/`
  - Requests that cannot be represented in the selected upstream protocol return `400 Bad Request`; they do not trigger channel failover or cooldown
  - Every channel accepts Anthropic, Codex, OpenAI, and Gemini clients; upstream protocol capability belongs to each structured URL
  - Explicit protocol declarations route directly and skip incompatible URLs without request or cooldown; automatic mode starts with the client protocol, then falls back through OpenAI → Anthropic → Codex → Gemini without retrying it, while local mode falls back through Anthropic → Codex → OpenAI → Gemini only when all URLs are undeclared
  - Automatic detection translates only after an uncommitted HTTP 400, a non-model 404/405, a structured `convert_request_failed` + `not implemented` 500, or a Cloudflare 403 block page returned before the API origin; exact URLs without declarations translate directly across protocols
- **Cooldown Manager** (DRY):
  - `cooldown/manager.go`: Unified cooldown decision engine
  - Eliminates duplicate code, unified cooldown logic
  - Distinguishes network vs HTTP error classification
  - Uses separate Key/Model/Channel actions; `ActionRetryModel` does not retry another Key or URL in the same channel
  - Persists structured `model_cooldown` responses, upstream HTTP 5xx failures, key-level 429 rate limits, model-unavailable 404 errors, and explicit model-retirement 410 errors by `(channel_id, actual upstream model)`; other models on that channel stay eligible
  - Automatically promotes to channel cooldown only when all configured models or all enabled keys are cooling
- **Multi-URL Selector** (URLSelector):
  - `url_selector.go`: Smart URL selection within a single channel
  - Explore-first: Unvisited URLs get priority to collect latency data
  - Weighted random: Weight = 1/EWMA latency, lower latency = higher selection probability
  - Independent cooldown: Failed URLs cool down independently without affecting other URLs
  - BaseURL tracking: Active requests, logs, and UI carry upstream URL throughout
- **Storage Layer Refactor** (eliminated 467 lines of duplicate code):
  - `storage/schema/`: Unified schema definition (supports SQLite/MySQL/PostgreSQL differences)
  - `storage/sql/`: Common SQL implementation layer shared by SQLite, MySQL, and PostgreSQL
  - `storage/factory.go`: Factory pattern auto-selects database
  - Composite index optimization, stats query performance improved
- **OpenAI service_tier Pricing**:
  - `util.OpenAIServiceTierMultiplier()`: Returns multiplier for priority/flex/default tiers
  - `LogEntry.ServiceTier`: Persisted to database, log cost column shows tier annotation
  - Supports GPT-5.4, GPT-5.4-pro, and other latest model pricing
- **Responses image_generation Tool Billing**:
  - Parses Responses API `tool_usage.image_gen` and the `image_generation` tool model
  - Bills `gpt-image-2` by text input, image input, and image output tokens
  - Streaming/non-streaming proxy paths and channel tests share the same usage parser to keep cost accounting consistent
- **Tiered Pricing**:
  - GPT-5.4: Input price auto-steps down after token threshold
  - Qwen-Plus: Lower price tier kicks in after threshold
  - Gemini long-context: Price doubles above threshold
  - Cache discounts: Claude/Opus independent multipliers, OpenAI cache hit 50% discount

**Multi-level Cache System**:
- Channel config cache (60s TTL)
- Round-robin pointer cache (in-memory)
- Channel/Key cooldown state inline (`channels` / `api_keys`); model cooldown state in `channel_model_cooldowns`
- Error classification cache (1000 capacity)

**Async Processing Architecture**:
- Log system (1000 buffer + single worker, guarantees FIFO order)
- Token/log cleanup (background goroutine, periodic maintenance)

**Unified Response System**:
- `StandardResponse[T]` generic struct (DRY)
- `ResponseHelper` utility class with 9 shortcut methods
- Auto-extracts app-level error codes, unified JSON format

**Connection Pool Optimization**:
- SQLite: 10 connections for memory mode / 5 for file mode, 5-minute lifetime
- HTTP client: 100 max connections, 30s timeout, keepalive optimization
- TLS: Session cache (1024 capacity), reduces handshake latency

## 🔧 Configuration

### Environment Variables

Environment variables cover bootstrap configuration only — the values ccLoad needs before a database connection exists. Everything the gateway consumes after startup (limits, cooldown durations, timeouts, health scoring) is a system setting managed from the admin console. In particular, `SQLITE_PATH`, `SQLITE_JOURNAL_MODE`, `CCLOAD_MYSQL`, `CCLOAD_POSTGRES`, `CCLOAD_ENABLE_SQLITE_REPLICA`, and `CCLOAD_SQLITE_LOG_DAYS` decide *how the database is opened*, so they cannot be stored in that same database and will stay environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `CCLOAD_PASS` | None | Admin password (**Required**, exits if not set) |
| `CCLOAD_API_TOKENS` | None | Pre-seed API access tokens on startup. Format: `token1,token2` or `token1\|production,token2\|development`; existing tokens are not overwritten |
| `API_TOKENS` | None | Compatibility alias for `CCLOAD_API_TOKENS`; startup fails if both variables are set with different values |
| `CCLOAD_MYSQL` | None | MySQL DSN (optional, format: `user:pass@tcp(host:port)/db?charset=utf8mb4`)<br/>**Mutually exclusive with `CCLOAD_POSTGRES`** |
| `CCLOAD_POSTGRES` | None | PostgreSQL DSN (optional, URL or libpq keywords, e.g. `postgres://user:pass@host:5432/db?sslmode=disable`)<br/>**Mutually exclusive with `CCLOAD_MYSQL`** |
| `CCLOAD_ENABLE_SQLITE_REPLICA` | `0` | Hybrid storage mode switch (`1`=enable, needs MySQL or Postgres primary DSN) |
| `CCLOAD_SQLITE_LOG_DAYS` | `7` | Days of logs to restore from primary DB on startup in hybrid mode (-1=all, 0=no logs) |
| `CCLOAD_ALLOW_INSECURE_TLS` | `0` | Disable upstream TLS cert validation (`1`=enable; ⚠️for troubleshooting/controlled intranet only) |
| `PORT` | `8080` | Service port |
| `GIN_MODE` | `release` | Run mode (`debug`/`release`) |
| `GIN_LOG` | `true` | Gin access log switch (`false`/`0`/`no`/`off` to disable) |
| `TRUSTED_PROXIES` | Private ranges + Loopback + `100.64.0.0/10` | Trusted proxy CIDRs (comma-separated); `none` = trust no proxies |
| `SQLITE_PATH` | `data/ccload.db` | SQLite database file path (SQLite mode only) |
| `SQLITE_JOURNAL_MODE` | `WAL` | SQLite Journal mode (WAL/TRUNCATE/DELETE, recommend TRUNCATE for containers) |
| `CCLOAD_HOST_OVERRIDES` | None | DNS override: pin upstream domains to fixed IPs, bypassing DNS resolution. Format: `host1=ip1,host2=ip2`, e.g. `anyrouter.top=47.246.23.200`. TLS SNI/cert/Host header unaffected |

> If the service sits behind a reverse proxy or load balancer, set `TRUSTED_PROXIES` explicitly so spoofed `X-Forwarded-For` values cannot affect client IP detection or login rate limiting.
> Responses WebSocket runtime usage and limits are available from `GET /admin/runtime-metrics`.

#### Hybrid Storage Mode (Primary DB + SQLite Cache)

HuggingFace Spaces and similar environments lose local data on restart, but remote MySQL/Postgres can have high query latency. Hybrid mode offers the best of both worlds:

- **Primary Storage (MySQL or PostgreSQL)**: Write operations go to the primary first, ensuring data persistence
- **SQLite Local Cache**: Read operations go through local SQLite, latency <1ms
- **Startup Recovery**: Restore data from primary to SQLite, supports restoring logs by days
- **Log Special Handling**: Write to SQLite first (fast), then async sync to primary (backup)

```bash
# Enable hybrid mode (MySQL primary)
export CCLOAD_MYSQL="user:pass@tcp(host:3306)/db?charset=utf8mb4"
export CCLOAD_ENABLE_SQLITE_REPLICA=1
export CCLOAD_SQLITE_LOG_DAYS=7  # Restore last 7 days of logs (optional)

# Or PostgreSQL primary
export CCLOAD_POSTGRES="postgres://user:pass@host:5432/db?sslmode=disable"
export CCLOAD_ENABLE_SQLITE_REPLICA=1
```

**Storage Modes**:
| Mode | Configuration | Use Case |
|------|---------------|----------|
| Pure SQLite | Don't set primary DSN | Local dev, single instance |
| Pure MySQL | Set `CCLOAD_MYSQL` | Standard production |
| Pure PostgreSQL | Set `CCLOAD_POSTGRES` | Standard production (PG) |
| Hybrid Mode | Primary DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1` | HuggingFace Spaces / high-latency primary |

### Web Admin Configuration (Database-backed, Auto Restart)

These settings live in the database and are managed from `/web/settings.html`. Saving a change writes it to the database and restarts the process about two seconds later; the restart is what applies the new value, so in-flight requests finish first and there is no hot reload:

| Setting | Default | Description |
|---------|---------|-------------|
| `log_retention_days` | `7` | Log retention days (-1 for permanent, 1-365 days) |
| `max_key_retries` | `3` | Max key retries within single channel |
| `max_concurrency` | `1000` | Max concurrent proxy requests |
| `max_body_bytes` | `10485760` | Max request body bytes, 10MB by default |
| `max_image_body_bytes` | `20971520` | Max Images API request body bytes, 20MB by default |
| `cooldown_auth_seconds` | `300` | Auth error (401/402/403) initial cooldown in seconds |
| `cooldown_server_seconds` | `120` | Server error (5xx) initial cooldown in seconds |
| `cooldown_timeout_seconds` | `60` | Timeout error (597/598) initial cooldown in seconds |
| `cooldown_rate_limit_seconds` | `60` | Rate limit error (429) initial cooldown in seconds |
| `cooldown_min_seconds` | `10` | Exponential backoff cooldown floor in seconds |
| `cooldown_max_seconds` | `1800` | Exponential backoff cooldown ceiling in seconds (an inverted floor/ceiling pair falls back to both defaults) |
| `cooldown_fallback_enabled` | `true` | When every channel is cooling down, fall back to the channel that recovers soonest instead of failing (keys follow the same earliest-recovery rule); set to `false` to reject the request outright |
| `global_cooldown_detection_rules` | `{}` | Global cooldown detection rules, inherited by channels that define no `cooldown_detection_rules` of their own |
| `upstream_connection_reuse_limit_seconds` | `0` | Maximum upstream connection reuse time in seconds (`0` = unlimited); applies to HTTP/1.1, HTTP/2, and WebSocket, drains active requests, then reconnects on demand |
| `upstream_first_byte_timeout` | `0` | Upstream first valid stream content timeout (seconds, 0=disabled, stream only) |
| `stream_timeout` | `0` | Stream request total timeout (seconds, 0=disabled) |
| `non_stream_timeout` | `120` | Non-stream request timeout (seconds, 0=disabled) |
| `anthropic_first_byte_timeout` | `0` | Anthropic first valid stream content timeout (seconds, 0=use global `upstream_first_byte_timeout`) |
| `anthropic_non_stream_timeout` | `0` | Anthropic non-stream request timeout (seconds, 0=use global `non_stream_timeout`) |
| `codex_first_byte_timeout` | `0` | Codex first valid stream content timeout (seconds, 0=use global `upstream_first_byte_timeout`) |
| `codex_non_stream_timeout` | `0` | Codex non-stream request timeout (seconds, 0=use global `non_stream_timeout`) |
| `openai_first_byte_timeout` | `0` | OpenAI first valid stream content timeout (seconds, 0=use global `upstream_first_byte_timeout`) |
| `openai_non_stream_timeout` | `0` | OpenAI non-stream request timeout (seconds, 0=use global `non_stream_timeout`) |
| `gemini_first_byte_timeout` | `0` | Gemini first valid stream content timeout (seconds, 0=use global `upstream_first_byte_timeout`) |
| `gemini_non_stream_timeout` | `0` | Gemini non-stream request timeout (seconds, 0=use global `non_stream_timeout`) |
| `enable_health_score` | `false` | Enable health-based dynamic channel sorting |
| `success_rate_penalty_weight` | `100` | Success rate penalty weight (see below) |
| `health_score_window_minutes` | `30` | Success rate stats time window (minutes) |
| `health_score_update_interval` | `30` | Success rate cache update interval (seconds) |
| `health_min_confident_sample` | `20` | Confidence sample threshold (full penalty at this sample size) |
| `enable_ttfb_score` | `false` | Enable relative first-byte latency penalty; requires `enable_health_score` |
| `ttfb_penalty_weight` | `20` | TTFB penalty when average first-byte latency is 2× the candidate median at full confidence |
| `ttfb_max_slow_ratio` | `2` | Upper bound for relative TTFB slowness (`avg_ttfb / median_ttfb - 1`) |
| `ttfb_min_confident_sample` | `10` | TTFB confidence sample threshold |
| `channel_check_interval_hours` | `5` | Scheduled channel check interval (hours, supports decimals, 0=disabled) |
| `model_catalog_sync_interval_hours` | `6` | Syncs the models.dev catalog every 6 hours; `0` disables network sync. At startup, the last-good cache is used, with the embedded catalog as fallback; channel `cost_multiplier` still applies. |
| `auto_update_interval_hours` | `12` | Non-container release check interval (hours, 0=disabled, minimum enabled value is 1); unavailable in containers |
| `auto_update_channel` | `stable` | Non-container release channel: `stable` accepts stable releases only; `preview` accepts stable and prerelease versions and selects the highest SemVer; unavailable in containers |
| `model_fuzzy_match` | `false` | When an exact model name misses, fall back to substring matching plus version sorting |
| `responses_ws_max_connections` | `128` | Max concurrent downstream Responses WebSocket connections across the process; `0` uses the built-in default |
| `responses_ws_max_connections_per_token` | `64` | Max concurrent downstream Responses WebSocket connections per auth token; `0` uses the built-in default |
| `responses_ws_max_sessions` | `256` | Max retained Responses WebSocket execution sessions across the process; `0` uses the built-in default |
| `responses_ws_session_ttl_minutes` | `15` | Idle execution-session retention in minutes; `0` uses the built-in default |
| `responses_ws_max_transcript_bytes` | `268435456` | Process-wide retained transcript payload budget (256 MiB); `0` uses the built-in default |
| `debug_log_enabled` | `false` | Capture upstream request/response debug logs |
| `debug_log_retention_minutes` | `2` | Debug log retention in minutes |

Per-protocol timeouts apply to the runtime upstream protocol: if a transformed request is forwarded to OpenAI, ccLoad reads `openai_*_timeout`; when that value is `0`, it falls back to the global timeout.

#### Auto Updates

For non-container deployments, one update manager owns release checks, version notifications, and optional in-process updates. It checks once at startup and every 12 hours by default. `auto_update_channel=stable` accepts stable releases only; `preview` considers stable and prerelease versions and selects the highest valid SemVer without downgrading the running or pending version. Change both settings from the Web admin settings page. Set `auto_update_interval_hours=0` to disable all release checks.

Stable metadata is resolved through the configured release sources. Preview discovery reads GitHub's Releases Atom feed, which includes stable and prerelease entries without using the rate-limited REST API. After resolving an exact Tag, ccLoad downloads that Tag's binary and checksum from the configured download sources—by default `gh.monlor.com`, `fastgit.cc`, `ghfast.top`, then GitHub; SHA256 must match before replacement.

Official containers do not run the release check or in-process update loop. Every stable and Beta image contains the exact binary produced by the matching GitHub Release. Stable releases publish an exact version Tag plus `latest`; Beta releases publish an exact prerelease Tag plus the rolling `beta` alias. Switch the image Tag in Compose, then pull and recreate the container.

To use a private release mirror, set `CCLOAD_RELEASE_BASE_URL` to a complete latest-download base such as `https://mirror.example/caidaoli/ccLoad/releases/latest/download`. An explicit value disables built-in fallback sources for stable metadata and all asset downloads. Preview metadata still comes from GitHub's Releases Atom feed. This setting does not configure `HTTP_PROXY` or `HTTPS_PROXY` for upstream API traffic.

#### Dynamic Channel Sorting

When `enable_health_score` is enabled, ccLoad calculates an effective priority from recent channel health. The success-rate penalty is always active; the relative first-byte latency penalty is added only when `enable_ttfb_score=true`:

```
failure_confidence = min(1.0, sample_count / health_min_confident_sample)
failure_penalty = failure_rate × success_rate_penalty_weight × failure_confidence

relative_slowness = clamp(avg_ttfb / candidate_median_ttfb - 1, 0, ttfb_max_slow_ratio)
ttfb_confidence = min(1.0, ttfb_sample_count / ttfb_min_confident_sample)
ttfb_penalty = relative_slowness × ttfb_penalty_weight × ttfb_confidence

effective_priority = base_priority - failure_penalty - ttfb_penalty
```

**Confidence factors** prevent new or low-traffic channels from receiving a full penalty from a few samples. TTFB scoring compares successful first-byte samples against the median of the current candidate channels. Channels at or faster than the median receive no TTFB penalty, and scoring is skipped when fewer than two candidates have valid TTFB data.

**Success-rate-only example** (`enable_ttfb_score=false`, `success_rate_penalty_weight = 100`, `health_min_confident_sample = 20`):

| Channel | Base Priority | Success Rate | Samples | Confidence | Penalty | Effective Priority |
|---------|---------------|--------------|---------|------------|---------|-------------------|
| A | 100 | 95% | 100 | 1.0 | 5 | **95** |
| B | 90 | 70% | 80 | 1.0 | 30 | **60** |
| C | 80 | 60% | 4 | 0.2 | 8 | **72** |
| D | 70 | 100% | 50 | 1.0 | 0 | **70** |

Base priority order: A > B > C > D
**Effective priority order: A (95) > C (72) > D (70) > B (60)**

#### API Access Token Configuration

**Important**: API access tokens are normally managed in the Web admin interface; Docker and CI deployments can pre-seed them with an environment variable.

- Visit `http://localhost:8080/web/tokens.html` for token management
- Set `CCLOAD_API_TOKENS=token1|production,token2|development` to create missing tokens on startup
- Provisioning is idempotent: existing tokens keep their description, limits, model/channel restrictions, and statistics
- Only missing tokens are created; existing tokens are never modified
- Supports add, delete, view tokens
- All tokens stored in database with persistence
- Without any tokens configured, all `/v1/*` and `/v1beta/*` APIs return `401 Unauthorized`

⚠️ **Security notes**:
- In production, prefer Docker Secrets, Kubernetes Secrets, or platform encrypted Secrets over plain environment variables
- In CI/CD, do not print full environment variables to logs
- After provisioning, remove `CCLOAD_API_TOKENS` from deployment config if automatic recovery is no longer needed
- Restrict access to container inspect output, orchestration dashboards, and deployment configuration

**Advanced Token Features**:
- **Cost Limits**: Set cost limits per token (USD), requests rejected with 429 when exceeded
- **Model Restrictions**: Restrict which models a token can access for fine-grained access control
- **Channel Restrictions**: Combine `allowed_channel_ids` with `channel_restriction_mode` — `allow` treats the list as an allowlist, `deny` as a denylist; an empty list is unrestricted in either mode
- **Concurrency Limit**: `max_concurrency` caps a token's simultaneous in-flight requests (`0` = unlimited)
- **First Byte Time**: Records streaming request TTFB (milliseconds) for upstream latency diagnosis

#### Behavior Summary

- `CCLOAD_PASS` not set: Program fails to start and exits (secure default)
- No API access tokens configured: All `/v1/*` and `/v1beta/*` APIs return `401 Unauthorized`. Configure tokens via Web interface `/web/tokens.html`
- Public endpoints: `GET /health` (health check) and `GET /public/summary` (stats summary) require no auth, all others require auth token

### Docker Images

Project supports multi-arch Docker images:

- **Supported Architectures**: `linux/amd64`, `linux/arm64`
- **Image Registry**: `ghcr.io/caidaoli/ccload`
- **Available Tags**:
  - `latest` - Latest stable version
  - `beta` - Latest Beta version
  - `v2.44.1` - Exact stable version, matching the GitHub Release tag
  - `vX.Y.Z-beta.N` - Exact Beta version, matching the GitHub prerelease tag

The official GHCR runtime image is Alpine-based and immutable: every release packages the already-tested `ccload-linux-amd64` and `ccload-linux-arm64` GitHub Release binaries into the matching multi-arch image. Exact version Tags are immutable release references; `latest` and `beta` are rolling aliases for the newest stable and Beta images. Containers do not check for releases or replace their binary in process; pull an exact Tag or the desired rolling alias and recreate the container.

### Image Tag Guide

```bash
# Pull latest version
docker pull ghcr.io/caidaoli/ccload:latest

# Pull specific version
docker pull ghcr.io/caidaoli/ccload:v2.44.1

# Pull latest Beta; replace beta with a published vX.Y.Z-beta.N Tag to pin it
docker pull ghcr.io/caidaoli/ccload:beta

# With Compose, set image to :latest or :beta, then apply the change
docker compose pull
docker compose up -d

# Specify architecture (Docker usually auto-selects)
docker pull --platform linux/amd64 ghcr.io/caidaoli/ccload:latest
docker pull --platform linux/arm64 ghcr.io/caidaoli/ccload:latest
```

### Database Structure

**Storage Architecture (Factory Pattern)**:
```
storage/
├── store.go         # Store interface (unified contract)
├── factory.go       # NewStore() auto-selects database
├── schema/          # Unified schema definition layer
│   ├── tables.go    # Table definitions (DefineXxxTable functions)
│   └── builder.go   # Schema builder (supports SQLite/MySQL/PostgreSQL differences)
├── sql/             # Common SQL implementation layer (eliminated 467 lines)
│   ├── store_impl.go      # SQLStore core implementation
│   ├── config.go          # Channel config CRUD
│   ├── apikey.go          # API key CRUD
│   ├── cooldown.go        # Cooldown management
│   ├── log.go             # Log storage
│   ├── metrics.go             # Metrics stats
│   ├── metrics_filter.go      # Filter intersection support
│   ├── metrics_aggregate_rows.go  # Aggregate row processing
│   ├── metrics_finalize.go    # Finalization processing
│   ├── auth_tokens.go         # API access tokens
│   ├── auth_token_stats.go    # Token statistics
│   ├── web_sessions.go    # Role-aware Web sessions
│   ├── system_settings.go # System settings
│   └── helpers.go         # Helper functions
└── sqlite/          # SQLite specific (test files only)
```

**Database Selection Logic**:
- `CCLOAD_MYSQL` set → MySQL primary (fatal if also set with `CCLOAD_POSTGRES`)
- `CCLOAD_POSTGRES` set → PostgreSQL primary
- Neither set → SQLite (default)
- Primary DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1` → Hybrid (primary write + SQLite read cache)

**Core Table Structure** (SQLite / MySQL / PostgreSQL shared):
- `channels` - Channel config (channel-level cooldown inline, UNIQUE constraint on name, with multi-protocol handling config, scheduled check config, RPM/concurrency limit config)
- `api_keys` - API keys (key-level cooldown inline, multi-key strategies)
- `channel_model_cooldowns` - Model-level runtime cooldown keyed by channel and actual upstream model
- `logs` - Request logs (with base_url upstream URL tracking)
- `debug_logs` - Debug logs (upstream request/response raw data, independent cleanup policy)
- `key_rr` - Round-robin pointers (channel_id → idx)
- `auth_tokens` - Auth tokens (with cost limits, model/channel restrictions, concurrency limits, first byte time tracking)
- `web_sessions` - Role-aware Web sessions bound to an optional API token
- `system_settings` - System config (database-backed, applied after automatic restart)

**Architecture Features**:
- ✅ **Unified SQL Layer** (refactor): SQLite, MySQL, and PostgreSQL share `storage/sql/` implementation
- ✅ **Unified Schema Definition** (new): `storage/schema/` defines table structures, supports database differences
- ✅ Factory pattern unified interface (OCP, easy to extend new storage)
- ✅ Channel/Key cooldown data inline; model-scoped cooldown stored separately so one unavailable model does not disable the whole channel
- ✅ Performance index optimization (channel selection latency ↓30-50%, key lookup latency ↓40-60%)
- ✅ Composite index optimization (stats query performance improved)
- ✅ Foreign key constraints (cascade delete, ensures data consistency)
- ✅ Multi-key support (sequential/round_robin strategies)
- ✅ Auto migration (auto creates/updates table structure on startup)
- ✅ Token stats enhancement (time range selection, per-token ID classification, cache optimization)
- ✅ **service_tier cost tracking**: Logs persist service_tier field, cost column shows tier label
- ✅ **Responses image tool cost tracking**: `image_generation` tool costs are included in logs, stats, and cost limit accounting
- ✅ **Tiered pricing engine**: GPT-5.4/Qwen-Plus/Gemini long-context step billing
- ✅ **Log UX improvements**: Cost column formats to 3 decimal places (empty for zero), IP column shows full address on hover
- ✅ **Automatic protocol fallback**: client-native routing first, then OpenAI → Anthropic → Codex → Gemini fallback with the native protocol skipped and family-aware capability caching
- ✅ **Debug logs**: Upstream request/response raw data capture, sensitive header masking, independent cleanup policy
- ✅ **Scheduled channel checks**: Background periodic channel availability probing, configurable check model per channel
- ✅ **Channel RPM limits**: Per-channel rolling 60-second request caps, `0` means unlimited, over-limit channels are skipped
- ✅ **Channel concurrency limits**: Per-channel in-flight request caps, `0` means unlimited, over-limit channels are skipped

**Backward Compatible Migration**:
- Auto-detects and fixes duplicate channel names
- Intelligently adds UNIQUE constraints, ensures data integrity
- Runs automatically on startup, no manual intervention needed
- Log database merged into main database (single data source)

## 🛡️ Security Considerations

- Production must set strong password `CCLOAD_PASS`
- Configure API access tokens via Web admin `/web/tokens.html` to protect API endpoint access
- API keys used only in memory, not logged
- Only random Web-session tokens are stored in client localStorage, with a 24-hour expiry
- Recommend using HTTPS reverse proxy
- Docker images run as non-root user for enhanced security

### Token Authentication System

ccLoad uses token-based authentication for simple and efficient secure access control.

**Auth Methods**:
- **Web Interface**: Login with an admin password or API token and receive a 24-hour Web-session token
- **API Endpoints**: Support `Authorization: Bearer <token>` header auth

**Core Features**:
- ✅ **Scoped Web Sessions**: API-token sessions are read-only and server-bound to their own usage data
- ✅ **Immediate Revocation**: Disabling, deleting, or expiring an API token invalidates its Web session
- ✅ **Credential Isolation**: Plaintext API tokens are never stored in browser storage
- ✅ **Server-side Authorization**: Channel management, token management, settings, and debug data remain admin-only

**Usage Example**:
```bash
# 1. Login to get token
curl -X POST http://localhost:8080/login \
  -H "Content-Type: application/json" \
  -d '{"mode":"admin","password":"your_admin_password"}' | jq

# Response example:
# {
#   "status": "success",
#   "token": "abc123...",  # 64-char hex token
#   "expiresIn": 86400     # 24 hours (seconds)
# }

# 2. Use token to access admin API
curl http://localhost:8080/admin/channels \
  -H "Authorization: Bearer <your_token>"

# 3. Logout (optional, token auto-expires after 24 hours)
curl -X POST http://localhost:8080/logout \
  -H "Authorization: Bearer <your_token>"
```

## 🔄 CI/CD

Project uses GitHub Actions for automated CI/CD:

- **Trigger Conditions**: Push version tags (`v*`) or manual trigger
- **Build Output**: Multi-arch Docker images pushed to GitHub Container Registry
- **Version Management**: Auto-generates semantic version tags
- **Cache Optimization**: Uses GitHub Actions cache to accelerate builds

## 🤝 Contributing

Issues and Pull Requests welcome!

### Development Checks

Use `sonic` for Go commands. Before sending a change, run the checks that match the touched area:

```bash
bash .agents/skills/sync-cliproxy-core/scripts/verify.sh --tests  # snapshot audit + focused protocol tests
go test -tags sonic ./internal/...
make race-fast      # high-value race subset
make race           # full race suite
make verify-web     # frontend node:test checks
golangci-lint run ./...
```

When protocol translation changes, run the snapshot audit before the full internal test suite. `make race-fast` keeps the common race-sensitive packages fast enough for local iteration; use `make race` before larger or concurrency-sensitive changes. Override `RACE_P` or `RACE_PARALLEL` only when the machine needs a different parallelism cap.

### Troubleshooting

**Port In Use**:
```bash
# Find and kill process using port 8080
lsof -i :8080 && kill -9 <PID>
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
# Test service health (lightweight health check, <5ms)
curl -s http://localhost:8080/health
# Or view stats summary (returns business data, 50-200ms)
curl -s http://localhost:8080/public/summary
# Check environment variable config
env | grep CCLOAD
```

## 📄 License

MIT License. The synchronized translator snapshot under `internal/protocol/cliproxy` retains its upstream [MIT notice](internal/protocol/cliproxy/LICENSE) and [provenance record](internal/protocol/cliproxy/UPSTREAM.md).
