# Configuration

**English | [简体中文](configuration.zh-CN.md)** · [← Back to README](../../README.md)

## Environment Variables

Environment variables cover bootstrap configuration only — the values ccLoad needs before a database connection exists. Everything the gateway consumes after startup (limits, cooldown durations, timeouts, health scoring) is a system setting managed from the admin console. In particular, `SQLITE_PATH`, `SQLITE_JOURNAL_MODE`, `CCLOAD_MYSQL`, `CCLOAD_POSTGRES`, `CCLOAD_ENABLE_SQLITE_REPLICA`, and `CCLOAD_SQLITE_LOG_DAYS` decide *how the database is opened*, so they cannot be stored in that same database and will stay environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `CCLOAD_PASS` | None | Admin password (**Required**, exits if not set) |
| `CCLOAD_API_TOKENS` | None | Pre-seed API access tokens on startup. Format: `token1,token2` or `token1\|production,token2\|development`; existing tokens are not overwritten |
| `API_TOKENS` | None | Compatibility alias for `CCLOAD_API_TOKENS`; startup fails if both variables are set with different values |
| `CCLOAD_MYSQL` | None | MySQL DSN (optional, format: `user:pass@tcp(host:port)/db?charset=utf8mb4`)<br/>**Mutually exclusive with `CCLOAD_POSTGRES`** |
| `CCLOAD_POSTGRES` | None | PostgreSQL DSN (optional, URL or libpq keywords, e.g. `postgres://user:pass@host:5432/db?sslmode=disable`)<br/>**Mutually exclusive with `CCLOAD_MYSQL`** |
| `CCLOAD_ENABLE_SQLITE_REPLICA` | `0` | Hybrid storage mode switch (`1`=enable, needs MySQL or Postgres primary DSN) |
| `CCLOAD_SQLITE_LOG_DAYS` | `7` | Initial log window when hybrid SQLite has no logs (-1=all); `0` disables all startup log imports, while other values make later startups import only logs after SQLite's latest timestamp |
| `CCLOAD_ALLOW_INSECURE_TLS` | `0` | Disable upstream TLS cert validation (`1`=enable; ⚠️for troubleshooting/controlled intranet only) |
| `PORT` | `8080` | Service port |
| `GIN_MODE` | `release` | Run mode (`debug`/`release`) |
| `GIN_LOG` | `true` | Gin access log switch (`false`/`0`/`no`/`off` to disable) |
| `TRUSTED_PROXIES` | Private ranges + Loopback + `100.64.0.0/10` | Trusted proxy CIDRs (comma-separated); `none` = trust no proxies |
| `SQLITE_PATH` | `data/ccload.db` | SQLite database file path (pure SQLite and hybrid modes) |
| `CURSOR_SDK_BRIDGE_BIN` | Auto-discovered | Explicit Cursor SDK Bridge executable; invalid overrides fail startup when a Cursor channel exists |
| `SQLITE_JOURNAL_MODE` | `WAL` | SQLite Journal mode (WAL/TRUNCATE/DELETE, recommend TRUNCATE for containers) |
| `CCLOAD_HOST_OVERRIDES` | None | DNS override: pin upstream domains to fixed IPs, bypassing DNS resolution. Format: `host1=ip1,host2=ip2`, e.g. `anyrouter.top=47.246.23.200`. TLS SNI/cert/Host header unaffected |
| `CCLOAD_MODEL_CATALOG_CACHE` | None | Explicit path for the models.dev catalog cache file (default `data/model-catalog.json`, falls back to a temp path when the default dir is not writable) |
| `CCLOAD_ANTHROPIC_CLI_VERSION_SYNC` | `true` | Hourly sync of the latest Claude Code CLI version from GitHub, used as the version floor for Anthropic OAuth fingerprints; `false` keeps the built-in and cached version only |
| `CCLOAD_ANTHROPIC_CLI_VERSION_CACHE` | None | Explicit path for the Claude Code CLI version cache (default `anthropic-cli-version.json` next to the model catalog cache; keep it on a persistent volume in containers) |

> If the service sits behind a reverse proxy or load balancer, set `TRUSTED_PROXIES` explicitly so spoofed `X-Forwarded-For` values cannot affect client IP detection or login rate limiting.
> Responses WebSocket runtime usage and limits are available from `GET /admin/runtime-metrics`.

### Hybrid Storage Mode (Authoritative SQLite + Async Primary Replica)

HuggingFace Spaces and similar environments lose local data on restart, but remote MySQL/Postgres can have high query latency. Hybrid mode offers the best of both worlds:

- **Authoritative SQLite**: Configuration, credentials, keys, cooldowns, settings, and logs are synchronously read and written locally; a successful SQLite commit completes the request
- **Async Primary Replica**: An in-memory worker coalesces final state by entity and retries failures after 10 seconds, so primary latency does not block requests; excessive distinct dirty entities collapse into one full-state reconciliation instead of growing memory without bound
- **Startup Semantics**: A newly created SQLite file imports configuration from primary; `CCLOAD_SQLITE_LOG_DAYS=0` disables startup log imports, otherwise every startup requires primary to be available, imports only logs newer than `MAX(sqlite.logs.time)` when SQLite already has logs, and uses the configured window only when SQLite logs are empty; existing SQLite configuration and logs are never replaced
- **Log Semantics**: Primary log writes and cleanup are attempted once, best-effort, and do not enter the 10-second retry loop; newer batches replace older pending batches and increment the dropped metric
- **Health**: Readiness checks authoritative SQLite only; primary sync health is exposed by runtime metrics
- **Local-only Data**: Web sessions and raw DebugData remain process-local in SQLite
- **Boundary**: This is a single-instance, single-writer design. Pending in-memory work is lost on process restart; there is no outbox and no multi-instance write coordination

```bash
# Enable hybrid mode (MySQL primary)
export CCLOAD_MYSQL="user:pass@tcp(host:3306)/db?charset=utf8mb4"
export CCLOAD_ENABLE_SQLITE_REPLICA=1
export CCLOAD_SQLITE_LOG_DAYS=7  # Seed the last 7 days when SQLite is empty, then import only the tail

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

## Web Admin Configuration (Database-backed, Auto Restart)

These settings live in the database and are managed from `/web/settings.html`. Saving a change writes it to the database and restarts the process about two seconds later; the restart is what applies the new value, so in-flight requests finish first. `model_multimodal_fallback` is the only hot-reloaded setting — saving just that mapping atomically swaps an immutable in-memory snapshot without a restart:

| Setting | Default | Description |
|---------|---------|-------------|
| `log_retention_days` | `7` | Log retention days (-1 for permanent, 1-365 days) |
| `max_key_retries` | `3` | Max key retries within single channel |
| `max_concurrency` | `1000` | Max concurrent proxy requests |
| `http_read_timeout_seconds` | `0` | Downstream request read timeout in seconds; `0` uses the built-in 120-second default. It covers the complete request header/body read, returns 408 on timeout, and is independent of body-size limits. |
| `max_body_bytes` | `33554432` | Max request body bytes, 32MB by default |
| `max_image_body_bytes` | `20971520` | Max Images API request body bytes, 20MB by default |
| `cooldown_auth_seconds` | `300` | Auth error (401/402/403) initial cooldown in seconds |
| `cooldown_server_seconds` | `120` | Server error (5xx) initial cooldown in seconds |
| `cooldown_timeout_seconds` | `60` | Timeout error (597/598) initial cooldown in seconds |
| `cooldown_rate_limit_seconds` | `60` | Rate limit error (429) initial cooldown in seconds |
| `codex_map_429_to_503` | `false` | Map the final upstream 429 returned to official Codex clients to 503 so they retry it as a 5xx; other Responses clients and ccLoad's own limits are unaffected |
| `cooldown_min_seconds` | `10` | Exponential backoff cooldown floor in seconds |
| `cooldown_max_seconds` | `1800` | Exponential backoff cooldown ceiling in seconds (an inverted floor/ceiling pair falls back to both defaults) |
| `cooldown_fallback_enabled` | `true` | When every channel is cooling down, fall back to the channel that recovers soonest instead of failing (keys follow the same earliest-recovery rule); set to `false` to reject the request outright |
| `global_cooldown_detection_rules` | `{}` | Global cooldown detection rules, inherited by channels that define no `cooldown_detection_rules` of their own |
| `TypeSafe_enabled` | `false` | Enable TypeSafe (Jev) error analysis fallback; requires an API key and restart |
| `TypeSafe_api_key` | empty | TypeSafe API key; never returned by the settings API. Reset clears the key and disables TypeSafe |
| `upstream_connection_reuse_limit_seconds` | `0` | Maximum upstream connection reuse time in seconds (`0` = unlimited); applies to HTTP/1.1, HTTP/2, and WebSocket, drains active requests, then reconnects on demand |
| `antigravity_sensitive_words` | `["API","proxy","Claude","Anthropic"]` | JSON string array of words replaced with zero-width characters in Antigravity `systemInstruction` and CodeBuddy system/developer message text |
| `upstream_first_byte_timeout` | `0` | Upstream first valid stream content timeout (seconds, 0=disabled, stream only) |
| `stream_timeout` | `0` | Stream request total timeout (seconds, 0=disabled) |
| `stream_idle_timeout` | `0` | Stream request upstream idle timeout: no upstream bytes for this long aborts the attempt (seconds, 0=disabled) |
| `non_stream_timeout` | `600` | Non-stream request timeout (seconds, 0=disabled) |
| `anthropic_first_byte_timeout` | `0` | Anthropic first valid stream content timeout (seconds, 0=use global `upstream_first_byte_timeout`) |
| `anthropic_non_stream_timeout` | `0` | Anthropic non-stream request timeout (seconds, 0=use global `non_stream_timeout`) |
| `anthropic_stream_idle_timeout` | `180` | Anthropic stream upstream idle timeout (seconds, 0=use global `stream_idle_timeout`) |
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
| `channel_test_content` | `sonnet 4.0的发布日期是什么` | Default prompt for manual channel tests and scheduled checks; separate multiple contents with `\|` to have one picked per test. Cannot be empty |
| `model_catalog_sync_interval_hours` | `6` | Syncs the models.dev catalog every 6 hours; `0` disables network sync. At startup, the last-good cache is used, with the embedded catalog as fallback; channel `cost_multiplier` still applies. |
| `auto_update_interval_hours` | `12` | Non-container release check interval (hours, 0=disabled, minimum enabled value is 1); unavailable in containers |
| `auto_update_channel` | `stable` | Non-container release channel: `stable` accepts stable releases only; `preview` accepts stable and prerelease versions and selects the highest SemVer; unavailable in containers |
| `model_multimodal_fallback` | `{}` | JSON map `{"non-vision model":"fallback model"}` (max 64 mappings / 8 KB); the only setting applied immediately without a process restart |
| `model_fuzzy_match` | `false` | When an exact model name misses, fall back to substring matching plus version sorting |
| `model_custom_pricing` | `{}` | JSON object overriding model prices (USD per million tokens); takes precedence over the models.dev catalog and the embedded pricing |
| `responses_ws_max_connections` | `128` | Max concurrent downstream Responses WebSocket connections across the process; `0` uses the built-in default |
| `responses_ws_max_connections_per_token` | `64` | Max concurrent downstream Responses WebSocket connections per auth token; `0` uses the built-in default |
| `responses_ws_max_sessions` | `256` | Max retained Responses WebSocket execution sessions across the process; `0` uses the built-in default |
| `responses_ws_session_ttl_minutes` | `15` | Idle execution-session retention in minutes; `0` uses the built-in default |
| `responses_ws_max_transcript_bytes` | `268435456` | Process-wide retained transcript payload budget (256 MiB); `0` uses the built-in default |
| `debug_log_enabled` | `false` | Capture upstream request/response debug logs |
| `debug_log_retention_minutes` | `2` | Debug log retention in minutes |
| `api_token_login_enabled` | `false` | Allow an API access token to sign in to the Web admin interface; does not affect API calls |
| `api_token_show_channels` | `false` | Show channel names and actual model names to token-authenticated Web users; disabling hides both while retaining call statistics; channel configuration stays closed; restart required after saving |
| `log_channel_click_action` | `edit` | What clicking a channel name on the logs page does (`edit` opens the channel editor, `filter` filters by that channel) |
| `channel_stats_range` | `today` | Cost statistics range on the channel management page (`today`, `yesterday`, `day_before_yesterday`, `this_week`, `last_week`, `this_month`, `last_month`) |
| `auto_refresh_interval_seconds` | `0` | Web page auto-refresh interval in seconds (`0` = disabled, `>= 30` recommended); a refresh is skipped while a dialog is open |
| `CODEX_BASE_URL` | Empty | Global upstream address for Codex OAuth channels (complete Responses URL; official default `https://chatgpt.com/backend-api/codex/responses`) |
| `ANTHROPIC_BASE_URL` | Empty | Global API root for Anthropic OAuth channels (official default `https://api.anthropic.com`) |
| `XAI_BASE_URL` | Empty | Global API root for xAI OAuth channels (usually ends with `/v1`; official default `https://cli-chat-proxy.grok.com/v1`) |
| `ANTIGRAVITY_URL` | Empty | Global upstream address for Antigravity OAuth channels (official default `https://daily-cloudcode-pa.googleapis.com`, backup `https://cloudcode-pa.googleapis.com`) |

Per-protocol timeouts apply to the runtime upstream protocol: if a transformed request is forwarded to OpenAI, ccLoad reads `openai_*_timeout`; when that value is `0`, it falls back to the global timeout.

Scheduled channel checks are configured per channel rather than globally. The channel editor owns `scheduled_check_enabled`, `scheduled_check_interval_minutes` (1–1440, default `300`), and `scheduled_check_start_time` (`HH:MM`, default `00:00`), so each channel can run on its own cadence. The legacy global `channel_check_interval_hours` setting was removed: the first startup after upgrading converts its value into every channel's interval and deletes the row.

The four global OAuth upstream addresses default to empty, meaning each provider's official address is used. When set, the matching OAuth channels use only that global address for data requests, model discovery, channel tests, and quota queries, ignoring the channel URL; OAuth authorization and token exchange/refresh still use the provider's official endpoints, and API-key channels are unaffected.

### Dynamic Channel Sorting

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

### API Access Token Configuration

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
- **Cost Limits**: Set independent total, daily, and monthly USD limits with `cost_limit_usd`, `cost_daily_limit_usd`, and `cost_monthly_limit_usd`; `0` means unlimited. A request is rejected with 429 when any enabled limit is reached. Cost-limited tokens also require `max_concurrency > 0`.
- **Model Restrictions**: Restrict which models a token can access for fine-grained access control
- **Channel Restrictions**: Combine `allowed_channel_ids` with `channel_restriction_mode` — `allow` treats the list as an allowlist, `deny` as a denylist; an empty list is unrestricted in either mode
- **Concurrency Limit**: `max_concurrency` caps a token's simultaneous in-flight requests (`0` = unlimited)
- **First Byte Time**: Records streaming request TTFB (milliseconds) for upstream latency diagnosis

### Behavior Summary

- `CCLOAD_PASS` not set: Program fails to start and exits (secure default)
- No API access tokens configured: All `/v1/*` and `/v1beta/*` APIs return `401 Unauthorized`. Configure tokens via Web interface `/web/tokens.html`
- Public endpoints: `GET /health` (health check) and `GET /public/summary` (stats summary) require no auth, all others require auth token

## Token Authentication System

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
