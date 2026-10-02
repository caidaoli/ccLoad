# Architecture

**English | [简体中文](architecture.zh-CN.md)** · [← Back to README](../../README.md)

## Protocol Routing

Every channel accepts all four client protocols. Upstream protocol selection is controlled by `protocol_transform_mode` and each structured URL's `protocols` declaration. `upstream` is strict client-protocol passthrough. `auto` tries the client protocol first, then probes OpenAI → Anthropic → Codex → Gemini while skipping the protocol already attempted, and advances only after an uncommitted capability error. `local` prioritizes URLs with explicit declarations and follows each URL's declared order. For Responses requests from an official Codex client, a URL that declares Codex is promoted to the native Codex path; URLs that do not declare Codex do not gain that capability, and other clients keep the configured order. Only when every URL is undeclared does it try Anthropic → Codex → OpenAI → Gemini. Incompatible URLs are skipped without a request or cooldown. Successful automatic detection is cached per URL and request family until restart or channel configuration changes. Only stable endpoint-level non-model 404/405 responses cache an all-protocols-unsupported result for that URL and request family; it is probed again after 10 minutes. Request-dependent 400/403/500 responses and local transform failures are retried on the next request.

![ccLoad program architecture](../../images/ccload-architecture.jpg)

## Soft-Error Detection

HTTP 200 responses that are actually errors trigger the same failover path as regular upstream failures:

- JSON responses containing an `{"error": {...}}` structure
- Responses whose `type` field is `"error"`
- Explicit rate limits in SSE `error` events (`rate_limit_exceeded` / `too_many_requests`), handled as `429`
- Plain-text load warnings such as `"当前模型负载过高"` ("current model load too high")

## Core Dependencies

| Component | Version | Purpose | Performance Advantage |
|-----------|---------|---------|----------------------|
| **Go** | 1.27.0+ | Runtime | Native concurrency, modern toolchain |
| **Gin** | v1.12.0 | Web Framework | High-performance HTTP routing |
| **modernc/sqlite** | v1.59.0 | Embedded Database | Pure Go, zero CGO dependency, single file (default) |
| **MySQL** | v1.10.1 | RDBMS | Optional, for high-concurrency production |
| **PostgreSQL (pgx)** | v5.11.0 | RDBMS | Optional, supports URL and libpq DSNs |
| **Sonic** | v1.15.4 | JSON Library | High-performance JSON encoding/decoding |
| **gjson / sjson** | v1.19.0 / v1.2.5 | Protocol JSON transforms | Targeted reads and writes without generic map conversion |
| **godotenv** | v1.5.1 | Env Config | Simplified config management |

## Architecture Features

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
  - `protocol/cliproxy/`: In-tree snapshot boundary for the pure [CLIProxyAPI](https://github.com/caidaoli/CLIProxyAPI) four-protocol core and allowlisted provider request/response adapters; [`UPSTREAM.md`](../../internal/protocol/cliproxy/UPSTREAM.md) records provenance, synchronization rules, and which provider adapters are actually present
  - Upstream refresh workflow: invoke `$sync-cliproxy-core` in Codex or `/sync-cliproxy-core` in Claude Code; one atomic operation pins one commit and synchronizes both the core and every registered provider adapter
  - Requests that cannot be represented in the selected upstream protocol return `400 Bad Request`; they do not trigger channel failover or cooldown
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
- **Storage Layer**:
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

**Connection Pool Optimization**:
- SQLite: 10 connections for memory mode / 5 for file mode, 5-minute lifetime
- HTTP clients: keepalive enabled; idle capacity is 2 per startup channel (2–1024 per transport), 20 per host, with a 90-second idle timeout
- TLS: Session cache (1024 capacity), reduces handshake latency

## Database Structure

**Storage Architecture (Factory Pattern)**:
```
storage/
├── store.go     # Store interface (unified contract)
├── factory.go   # NewStore() auto-selects database
├── migrate*.go  # Startup schema/column/data migrations
├── hybrid_*.go  # Authoritative SQLite + async primary replication
├── cache.go     # Channel/API-key read caches
├── schema/      # Table definitions (DefineXxxTable) and the SQLite/MySQL/PostgreSQL schema builder
├── sql/         # Shared SQL implementation for all three databases: channel config, API keys,
│                # cooldowns, URL states, logs, debug logs, metrics, auth tokens and their stats,
│                # Web sessions, system settings, OAuth quota cost, transactions, replica writes
└── sqlite/      # SQLite-specific tests only
```

**Database Selection Logic**:
- `CCLOAD_MYSQL` set → MySQL primary (fatal if also set with `CCLOAD_POSTGRES`)
- `CCLOAD_POSTGRES` set → PostgreSQL primary
- Neither set → SQLite (default)
- Primary DSN + `CCLOAD_ENABLE_SQLITE_REPLICA=1` → Hybrid (authoritative SQLite + async primary replica)

**Core Table Structure** (SQLite / MySQL / PostgreSQL shared):
- `channels` - Channel config (channel-level cooldown inline, UNIQUE constraint on name, with multi-protocol handling config, scheduled check config, RPM/concurrency limit config)
- `api_keys` - API keys (key-level cooldown inline, multi-key strategies, `allowed_models` allowlist)
- `channel_models` - Per-channel model list with per-model redirect target and disabled flag
- `channel_model_cooldowns` - Model-level runtime cooldown keyed by channel and actual upstream model
- `channel_url_states` - Per-URL enable/disable state for multi-URL channels, keyed by channel and URL hash
- `logs` - Request logs (with base_url upstream URL tracking)
- `debug_logs` - Debug logs (upstream request/response raw data, independent cleanup policy)
- `auth_tokens` - Auth tokens (with cost limits, model/channel restrictions, concurrency limits, first byte time tracking)
- `web_sessions` - Role-aware Web sessions bound to an optional API token
- `system_settings` - System config (database-backed, applied after automatic restart)

**Architecture Features**:
- ✅ **Unified SQL Layer**: SQLite, MySQL, and PostgreSQL share `storage/sql/` implementation
- ✅ **Unified Schema Definition**: `storage/schema/` defines table structures, supports database differences
- ✅ Factory pattern unified interface (OCP, easy to extend new storage)
- ✅ Channel/Key cooldown data inline; model-scoped cooldown stored separately so one unavailable model does not disable the whole channel
- ✅ Indexes for channel selection and key lookup
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
- ✅ **Per-key model allowlists**: `api_keys.allowed_models` restricts which channel models each Key serves; empty means unrestricted, and a channel is skipped when none of its Keys serve the requested model

**Backward Compatible Migration**:
- Auto-detects and fixes duplicate channel names
- Intelligently adds UNIQUE constraints, ensures data integrity
- Runs automatically on startup, no manual intervention needed
- Log database merged into main database (single data source)
