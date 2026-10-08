# Usage Guide

**English | [简体中文](usage.zh-CN.md)** · [← Back to README](../../README.md)

## API Proxy

**Claude API Proxy (Requires Auth)**:

First, configure API access token in Web admin interface `http://localhost:8080/web/tokens.html`, then use that token to access API:

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
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

**Image Generation (Images API)**:

`POST /v1/images/generations` is OpenAI Images-compatible, with its own body-size limit controlled by `max_image_body_bytes`. When the channel model is an xAI conversational model at `grok-4.6` or later, ccLoad bridges the request into an xAI Responses `image_generation` tool call: non-streaming requests aggregate into standard Images JSON, and streaming requests emit `partial_image` / `completed` SSE events.

**Codex CLI (recommended configuration)**:

Define a custom provider for ccLoad and select it with `model_provider`:

```bash
# Log in with an API key; use your ccLoad API token as the key
printenv CCLOAD_API_TOKEN | codex login --with-api-key
```

```toml
# ~/.codex/config.toml
model_provider = "ccload"   # top-level key: keep it before any [...] section

[model_providers.ccload]
name = "OpenAI"
base_url = "http://localhost:8080/v1"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true
```

`requires_openai_auth = true` makes the provider use the API key stored by `codex login --with-api-key`. Custom providers do not use WebSocket unless `supports_websockets = true` is set. They also omit the `version` header, so the Codex backend does not apply per-model `minimal_client_version` gating to these requests.

**Codex Responses WebSocket**:

The downstream and upstream WebSockets are independent. Authenticated clients can always upgrade `GET /v1/responses` or the Codex direct-route aliases `GET /v1/codex/responses` and `GET /backend-api/codex/responses`; a channel's `websockets` field only controls whether ccLoad tries a native Codex upstream WebSocket. Channels without that field still participate through the HTTP/SSE bridge and remain eligible for failover.

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

All channel-upstream HTTP transports, including Antigravity's credential-isolated HTTP/1.1 pools, enable connection reuse with up to 20 idle connections per host and a 90-second idle timeout. At startup, each transport's total idle-connection capacity is sized to twice the persisted channel count, with a minimum of 2 and a maximum of 1024. Channel proxies and Antigravity credentials own isolated transports, so 1024 is a per-transport limit rather than a process-wide socket cap.

Reconnects must use the same API token and stable execution headers. `Session-Id` identifies the top-level Codex session; when `Thread-Id` is present, ccLoad combines both headers so the parent and every subagent thread own independent transcripts, Response IDs, and turn locks. Clients without `Thread-Id` retain the `Session-Id`-only contract. `prompt_cache_key`, body `session_id`, and other cache-routing hints do not identify an execution session and never serialize or share local conversation state. An execution session is in-memory and process-local: new installations retain at most 256 sessions with a process-wide transcript payload budget of 256 MiB. Existing database records are not migrated. The idle TTL remains 15 minutes by default (10 minutes is suitable for small-memory hosts). After all downstream attachments have been gone for five minutes, the one-minute cleanup loop closes the physical upstream connection, so actual reclamation takes about 5–6 minutes while the transcript remains until the session TTL. A stable session and its committed transcript are never evicted by session-capacity or memory-budget pressure before that TTL expires. When the session ceiling is full, only a new session identity is rejected; an existing stable session may continue. Once the committed payload is over budget, every new turn, including turns on existing sessions, is rejected before upstream work starts. Both limits use a WebSocket `429/rate_limit_error/rate_limit` event; retry after TTL reclamation, or change the setting and restart. A restart loses in-memory sessions, so the client must then resend the complete conversation input without `previous_response_id`.

The transcript budget is an admission threshold, not a strict allocation cap: turns already admitted are allowed to complete and commit. The finite worst-case overshoot is `responses_ws_max_sessions × max_body_bytes` in addition to the configured budget. Process restarts do not restore sessions or cumulative session metrics. Multi-instance deployments need sticky routing so reconnects reach the same instance. Otherwise, the client must send the complete conversation input without `previous_response_id`. Adjust session count, TTL, and transcript budget with `responses_ws_max_sessions`, `responses_ws_session_ttl_minutes`, and `responses_ws_max_transcript_bytes` in system settings. `GET /admin/runtime-metrics` reports the current effective payload as `transcript_bytes`; it excludes the Go runtime, WebSocket buffers, and temporary request-processing objects. The same response exposes WebSocket rejection counters, log queue/drop/persistence-failure counters, and—when hybrid storage is enabled—primary-sync backlog, failures, dropped tasks, and the last successful sync time.

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

## Model Thinking Suffix

Every protocol entry point accepts a thinking suffix appended to the model name, such as `claude-sonnet-4-6(high)`, `gpt-5.2(xhigh)`, or `gemini-3.1-pro(8192)`. ccLoad strips the suffix for routing, then writes the level into the request body's thinking parameters for the protocol actually forwarded upstream (Anthropic `thinking`, OpenAI/Codex `reasoning.effort`, Gemini `thinkingBudget`):

- **Levels**: `minimal` / `low` / `medium` / `high` / `xhigh` / `max`; a level beyond the upstream model's capability clamps to the nearest supported tier
- **Disable**: `(none)` or `(0)` turns thinking off
- **Auto**: `(auto)` defers to the upstream default thinking behavior
- **Numeric budget**: a non-negative integer such as `(16384)` is forwarded as a token budget (Anthropic `budget_tokens`, Gemini `thinkingBudget`)

The suffix is not a model identity: routing, auth, cooldown, logging, and the upstream model name always use the base name, so channel model lists do not need suffixed entries. The HTTP proxy, Responses WebSocket, and admin channel testing all honor the suffix; channel custom request rules run later and can override the fields it writes. A parenthesized model name whose suffix is not a known level or non-negative integer (an upstream really named `foo(bar)`) passes through unchanged.

## Multimodal Fallback

System setting `model_multimodal_fallback` maps non-vision models to fallback models as a JSON object `{"text-model":"fallback-model"}` (max 64 mappings / 8 KB; keys are normalized to the lower-cased base name, values may carry a thinking suffix). When a request contains non-text content — images, files — ccLoad rewrites the incoming model to the fallback **before** thinking-suffix handling and token, channel, and Key filtering, so routing, cooldowns, and logs all follow the fallback model. HTTP entries inspect the client-protocol body; Responses WebSocket turns inspect the complete conversation transcript, so an image that entered the history keeps every later turn on the fallback deterministically. Open **Multimodal Fallback Models** on the settings page to edit the mapping. Unlike every other system setting, saving only this mapping takes effect immediately; a commit that touches any other setting still restarts the process about two seconds later.

## Local Token Counting

Quickly estimate request token consumption (no upstream API call needed):

```bash
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-token" \
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
- ✅ Local estimate: no upstream call, no API quota consumption
- ✅ Supports system prompts, tool definitions, large-scale tool scenarios
- ✅ Requires auth token (configure at `/web/tokens.html`)

## Channel Management

Manage channels via Web interface `/web/channels.html` or API:

```bash
# Add a channel with per-URL protocol capabilities
curl -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer your_admin_token" \
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

**OpenAI-compatible upstream example**:

```bash
# Add a channel using the OpenAI wire protocol
curl -X POST http://localhost:8080/admin/channels \
  -H "Authorization: Bearer your_admin_token" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "OpenAI-Compatible",
    "api_key": "sk-xxx",
    "urls": [
      {"url": "https://api.openai.com", "protocols": ["openai"]}
    ],
    "protocol_transform_mode": "auto",
    "priority": 10,
    "rpm_limit": 0,
    "max_concurrency": 0,
    "models": [{"model": "gpt-4o"}],
    "enabled": true
  }'
```

> This works with any OpenAI-compatible provider by changing `urls[].url` to its API base URL. Omit `/v1` and endpoint paths because ccLoad appends them for the selected protocol. The `protocols: ["openai"]` declaration routes the channel as an OpenAI upstream.

> **Protocol behavior**: Each `urls` entry may list `protocols` (`anthropic`, `codex`, `openai`, `gemini`). A non-empty list is authoritative. See [Protocol Routing](architecture.md#protocol-routing) for how the `upstream`, `auto`, and `local` modes choose the upstream protocol.

> **Multi-URL Note**: `urls` is an ordered array of `{url, exact, protocols}` objects. `exact: true` means the URL is already the complete upstream request URL. The system uses latency-weighted selection and independent URL cooldown; local mode first partitions explicitly declared URLs ahead of automatic ones while preserving order inside each group.

> **Model Entry Note**: each `models` element is `{model, redirect_model, disabled, pricing}`. A channel may configure the same `model` on multiple rows with distinct `redirect_model` targets. Enabled, uncooled rows rotate in order, each with its own disabled state and price; retries across keys, URLs, and protocols keep the selected row. After an exact redirect, the target's first enabled row may supply one further redirect (`A→B→C` sends A to C); the lookup stops there and does not advance B's rotation cursor. The model disappears from the channel only when every row is disabled. Admin tests may specify `redirect_model` to test one row; scheduled checks use enabled rows only.

> **Per-key model allowlist**: each `api_keys` entry may carry an `allowed_models` string array restricting which models that Key serves. Omitting it, leaving it empty, or passing `"*"` means unrestricted, preserving the previous behavior. Every listed model must already exist in the channel's `models` (unless the channel declares a wildcard model), otherwise the save is rejected; on save the names are normalized to the channel's canonical casing, deduplicated, and capped at 2000 encoded bytes. Matching runs on the **channel-side logical model**: fuzzy matching happens first, the allowlist is checked next, and `redirect_model` rewriting happens afterwards, so list channel model names rather than upstream ones. Keys are filtered by model before the key-retry loop; when no Key in a channel serves the requested model, that channel is skipped without cooldown and without recording a failure. In the web UI use **Model Scope** on the Key row, and **Detect This Key** to probe the upstream models and match them against the channel models. This fits relays where different Keys carry different model entitlements.

> **Independent-key relay fallback**: In the channel editor, open **Advanced → Other** and enable **Try another key on failure** only when the channel's Keys reach independent upstream providers behind the same relay. For retryable model- or channel-level upstream failures (such as 5xx, connection errors, and first-byte timeouts), ccLoad then cools the current Key and tries another Key in that channel before moving to another channel. The option is off by default, preserving the normal model/channel cooldown behavior.

> **RPM Limit Note**: `rpm_limit` is a per-channel request cap over a rolling 60-second window; `0` means unlimited. Proxy forwarding, manual tests, single-URL tests, and scheduled checks all count toward the cap. Multi-URL failover counts each actual upstream HTTP request. The counter is in-memory: restart clears it, and multiple instances count independently.

> **Concurrency Limit Note**: `max_concurrency` is a per-channel cap on simultaneous in-flight upstream requests; `0` means unlimited. A slot is acquired before the upstream request starts and released when the response body is closed, so streaming requests hold the slot until the stream ends. Over-limit channels are skipped without cooldown. The counter is in-memory and per instance.

### Z.ai Coding Plan (ZCode)

In the channel manager, choose **Z.ai Coding Plan** and either complete the browser authorization flow or import an existing Coding Plan API key. API-key import remains available when the provider's browser OAuth flow is unavailable.

ccLoad loads the Coding Plan model catalog from the account when creating or refreshing the channel, falls back to models.dev, then uses its built-in list only as a last resort. The channel card can also refresh and show the Coding Plan quota windows.

### Cursor

In the channel manager, choose **Cursor** and import a Cursor user API key. ccLoad exchanges it for a control-plane session; browser login and session `accessToken` import are intentionally unavailable because they cannot authenticate inference. Identity and quota refresh use `api2.cursor.sh`; model discovery calls the SDK Bridge's `ListModels`, stores the IDs returned by Cursor, and adds the SDK-supported `-fast` form without inventing reasoning variants. At startup, a Cursor channel makes ccLoad locate and probe an existing `cursor-sdk-bridge` in the background; if none works, it downloads the pinned official archive, verifies its embedded SHA-256, and atomically installs it under `cursor-sdk/bin/<version>` beside `SQLITE_PATH`, or in the OS user cache when `SQLITE_PATH` is unset (system temp is the final fallback). This does not block HTTP startup, and no Cursor CLI installation is required. Manual and offline downloads are available from the [official Cursor SDK Bridge release page](https://github.com/cursor/sdk-bridge/releases).

The SDK Agent allows only Cursor's `mcp` capability group: SDK custom tools are exposed through Cursor's synthetic `custom-user-tools` MCP server, while shell, file, and other built-in tools remain disabled on the gateway host. Client functions are registered through `LocalAgentOptions.custom_tools`. ccLoad serves the authenticated loopback `SdkCustomToolCallbackService`, returns each native callback as an Anthropic `tool_use` or OpenAI `tool_calls` item, and keeps that Agent suspended until the client sends the matching result on its next turn. Sessions are isolated by `agent_id` and callbacks by `call_id`, including concurrent requests sharing one Cursor channel.

The channel card can refresh included / API / Auto spend windows from `DashboardService/GetCurrentPeriodUsage`.

### Zed

In the channel manager, choose **Zed** and complete the native sign-in. This is not an OAuth code/PKCE flow: each sign-in generates a temporary RSA-2048 key on a random loopback port, passes its PKCS#1 DER public key (base64url) to `zed.dev/native_app_signin`, and decrypts the returned `access_token` with RSA-OAEP/SHA-256 into a long-lived native credential; the temporary private key is never persisted. `system_id` is an optional Zed installation identity used primarily to bind trial entitlement to a real installation (form value → `CCLOAD_ZED_SYSTEM_ID` → local Zed `db/0-global/db.sqlite`); sign-in can be attempted without it, requests omit `x-zed-system-id`, and the upstream decides whether the account has trial access. Never generate a random value or copy a fixed value from another machine; re-authorizing an account keeps the stored value.

Data requests first exchange the native credential for a short-lived JWT via `/client/llm_tokens` (refreshed 60 seconds early, single-flight, CAS-persisted), then call `/completions` with `Authorization: Bearer`. Channels use a fixed exact `/completions` URL, the Codex protocol with local conversion, and WebSockets disabled. ccLoad exposes the Zed `/models` entries it can convert across the OpenAI/Anthropic/Google providers; requests are wrapped in a Zed `thread_id/prompt_id/intent/provider/model/provider_request` envelope. A `plan` 403 cools only the current model and switches channels, while other 401/403 errors refresh the credential.

### Management Account (API-Key Channels)

API-key channels can optionally bind an upstream management account for balance queries and daily check-ins. In the channel editor, open **Advanced → Management Account**, choose a profile, and enter the upstream credentials:

| Profile | Balance | Check-in | Notes |
|---------|---------|----------|-------|
| **New API** | ✅ | ✅ | Optional `user_id` for multi-tenant sites |
| **Sub2API** | ✅ | ❌ | Balance only |
| **Sub2API Pro** | ✅ | ✅ | Subscription-based balance with daily/weekly/monthly windows |

**Daily auto check-in**: Enable it in the management account panel and set a time (HH:MM, server local time). ccLoad scans every minute; a missed window is caught up on the next scan. Check-in results are recorded as `checkin` audit log entries visible on the logs page. Profiles without check-in support (Sub2API) are silently skipped.

**Manual operations**: The channel card provides **Refresh Balance** and **Check-in** buttons. The same operations are available via Admin API:
```bash
# Refresh balance
curl -X POST http://localhost:8080/admin/channels/:id/management-account/balance \
  -H "Authorization: Bearer your_admin_token"

# Manual check-in
curl -X POST http://localhost:8080/admin/channels/:id/management-account/checkin \
  -H "Authorization: Bearer your_admin_token"
```

> **CSV round-trip**: Export includes `management_daily_checkin_enabled` and `management_daily_checkin_time` columns; import can update check-in settings without touching credentials. The `oauth_credential` column carries both OAuth credentials and management envelopes for cross-instance migration.

## Custom Request Rules (Advanced)

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

## Batch Data Management

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
name,api_key,urls,priority,model_entries_json,enabled
Claude-API-1,sk-ant-xxx,"[{""url"":""https://api.anthropic.com"",""protocols"":[""anthropic""]}]",10,"[{""model"":""auto"",""redirect_model"":""claude-sonnet-4-6""},{""model"":""auto"",""redirect_model"":""claude-opus-4-6""}]",true
```

**Features**:
- Auto column name mapping (Chinese/English)
- Smart data validation with error messages
- Incremental import and overwrite update
- UTF-8 encoding, Excel compatible
- `oauth_credential` includes both OAuth credentials and API-key channel management envelopes for cross-instance migration; treat exported CSV files as sensitive and delete them after use

Upgrade note: new CSV exports use only `model_entries_json` for ordered model rows, disabled states, and prices. Legacy `models`/`model_redirects`/`model_pricing` columns remain importable on their own; mixing them with the new column is rejected. The existing two-step `A→B→C` redirect remains available alongside multiple A targets; a third redirect is not followed.

## Monitoring and Admin Console

The admin console shows requests, logs, token usage, and channel state in real time:

![ccLoad Dashboard](../../images/ccload-dashboard.jpeg)
![ccLoad Logs](../../images/ccload-logs.jpg)
*Real-time Monitoring Dashboard: Claude Code, Codex, OpenAI, and Gemini platform metrics at a glance*

**Core Features**:
- 📈 **24-hour Trend Charts** - Request volumes clearly visualized with peaks and valleys
- 🔴 **Real-time Error Logs** - Instantly detect which channel has issues; abort an in-flight request directly from the logs page — the abort is classified as an upstream disconnect, so the request fails over instead of looking like a client cancellation
- 📊 **Channel Call Statistics** - See which channels are performing well with data-backed insights
- 💬 **Model Testing Workbench** - Test by channel, by model, or in a chat-style model testing workflow:
  - Upload or paste images in chat mode to verify multimodal requests directly
  - Toggle reasoning level, built-in search, and streaming to inspect transformed upstream behavior
  - Export conversations as Markdown or HTML for review, incident notes, or regression records
  - Generate images in a dedicated tab via the Images API or Chat Completions, with the prompt preserved across page reloads
- ⚡ **Performance Metrics** - Latency, success rates, and bottleneck detection
- 💰 **Token Usage Stats** - Know exactly where your budget goes:
  - Custom time range selector for flexible analysis
  - Per API token ID classification for multi-tenant billing
  - Supports Gemini/OpenAI cache token visualization
- 🎛️ **Log Column Customization** - Click the gear icon to show/hide columns, settings auto-saved to browser
