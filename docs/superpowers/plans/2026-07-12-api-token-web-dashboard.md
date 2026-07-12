# API Token Web Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only, token-scoped web dashboard entered from the existing login page without exposing API tokens or channel configuration.

**Architecture:** Replace administrator-only session semantics with a persisted `WebSession` carrying a role and optional API-token ID. Shared `/dashboard` read endpoints derive an immutable token filter from that identity, while `/admin` remains administrator-only. Token-mode model tests reuse the normal proxy handler through scoped `/dashboard/v1` routes.

**Tech Stack:** Go 1.25, Gin, SQLite/MySQL storage abstraction, vanilla JavaScript, HTML/CSS, Node test runner.

## Global Constraints

- All Go commands use `-tags sonic`.
- API-token users are read-only and can access only their own data.
- Server authorization is authoritative; frontend hiding is not a security boundary.
- Plaintext API tokens are accepted only by `POST /login` and are never persisted in browser storage or web-session storage.
- Token-mode responses must not expose channel IDs/names, upstream URLs, upstream API keys/hashes, client IPs, or debug logs.
- Token-mode model tests use the same model/channel/cost/concurrency/accounting rules as direct API calls.

---

### Task 1: Persist Role-Aware Web Sessions

**Files:**
- Create: `internal/model/web_session.go`
- Replace: `internal/storage/sql/admin_sessions.go` with `internal/storage/sql/web_sessions.go`
- Modify: `internal/storage/store.go`
- Modify: `internal/storage/hybrid_store.go`
- Modify: `internal/storage/schema/tables.go`
- Modify: `internal/storage/schema/integration_test.go`
- Modify: `internal/storage/migrate_sqlite_test.go`
- Modify: `internal/storage/migrate_mysql_test.go`
- Replace tests: `internal/storage/sql/admin_sessions_test.go` with `internal/storage/sql/web_sessions_test.go`

**Interfaces:**
- Produces: `model.WebRole`, `model.WebSession`, `CreateWebSession`, `GetWebSession`, `DeleteWebSession`, `CleanExpiredWebSessions`, `LoadWebSessions`.

- [ ] Add failing storage tests proving role/token binding survives create/get/load and expired sessions are excluded.
- [ ] Define:

```go
type WebRole string

const (
    WebRoleAdmin    WebRole = "admin"
    WebRoleAPIToken WebRole = "api_token"
)

type WebSession struct {
    TokenHash  string
    Role       WebRole
    AuthTokenID int64
    ExpiresAt  time.Time
}
```

- [ ] Replace the old `admin_sessions` schema with `web_sessions(token_hash, role, auth_token_id, expires_at, created_at)` and an expiry index. Sessions are ephemeral, so old administrator sessions are intentionally not migrated.
- [ ] Implement SQL and hybrid-store methods using token hashes only.
- [ ] Run: `go test -tags sonic ./internal/storage/sql ./internal/storage/schema ./internal/storage/...`
- [ ] Commit: `refactor(auth): persist role-aware web sessions`

### Task 2: Unify Web Authentication and Authorization

**Files:**
- Modify: `internal/app/auth_service.go`
- Create: `internal/app/web_identity.go`
- Modify: `internal/app/auth_service_handlers_test.go`
- Modify: `internal/app/auth_middleware_test.go`

**Interfaces:**
- Produces: `WebIdentityFromContext`, `RequireWebAuth`, `RequireAdminAuth`, `RequireWebAPITokenProxyAuth`, `HandleWebSession`.

- [ ] Add failing tests for admin login, API-token login, generic credential errors, persisted identity reload, token invalidation, admin `403`, and API-token proxy context attachment.
- [ ] Change `POST /login` payload to:

```go
type webLoginRequest struct {
    Mode     model.WebRole `json:"mode" binding:"required"`
    Password string        `json:"password"`
    Token    string        `json:"token"`
}
```

- [ ] Validate API tokens through the existing active-token maps and create a random `api_token` web session bound to its ID.
- [ ] Store `map[tokenHash]model.WebSession` in memory and reload it at startup.
- [ ] Implement middleware that attaches `WebIdentity`; revalidate the backing token for every API-token request and delete invalid sessions.
- [ ] Factor direct API authentication so `RequireWebAPITokenProxyAuth` applies the same model/channel/cost/concurrency/last-used behavior before setting `token_hash` and `token_id`.
- [ ] Return `{token, expiresIn, role}` from login and `{role, auth_token_id, description, allowed_models}` from `GET /dashboard/session` without returning token hashes.
- [ ] Run: `go test -tags sonic ./internal/app -run 'Test(Auth|WebSession|Login)'`
- [ ] Commit: `feat(auth): add api token web sessions`

### Task 3: Add Server-Enforced Dashboard Scope

**Files:**
- Create: `internal/app/dashboard_scope.go`
- Modify: `internal/app/handlers.go`
- Modify: `internal/app/admin_stats.go`
- Modify: `internal/app/admin_logs_bootstrap.go`
- Modify: `internal/app/server.go`
- Extend: `internal/app/handlers_test.go`
- Extend: `internal/app/admin_list_shapes_test.go`
- Create: `internal/app/dashboard_scope_test.go`

**Interfaces:**
- Produces: `ApplyWebIdentityScope(*gin.Context, *model.LogFilter) error`, token-safe response projection helpers.

- [ ] Add failing tests that forge another `auth_token_id` and prove logs, stats, metrics, models, filters, bootstrap, and summary use the session token ID.
- [ ] Make `BuildLogFilter` apply web identity after parsing query filters. API-token identity overwrites `AuthTokenID`; admin identity preserves the requested filter.
- [ ] Register shared read routes under `/dashboard` with `RequireWebAuth`; keep mutable/sensitive routes under `/admin` with `RequireAdminAuth`.
- [ ] Register `/dashboard/v1/*path` and `/dashboard/v1beta/*path` with `RequireWebAuth`, `RequireWebAPITokenProxyAuth`, and `captureClientRequestMetadata`, then call `HandleProxyRequest`.
- [ ] Make dashboard summary use the scoped filter instead of global data.
- [ ] For API-token responses, aggregate stats by model without channel identifiers, remove `MetricPoint.Channels`, return no channel filter options, and project logs to safe fields only.
- [ ] Make token bootstrap omit settings, channels, other auth tokens, debug metadata, and channel actions.
- [ ] Run: `go test -tags sonic ./internal/app -run 'Test(Dashboard|HandleStats|HandleMetrics|HandleErrors|HandleGetModels)'`
- [ ] Commit: `feat(dashboard): enforce api token data scope`

### Task 4: Reuse Login and Navigation for Both Roles

**Files:**
- Modify: `web/login.html`
- Modify: `web/assets/js/login.js`
- Modify: `web/assets/js/ui.js`
- Modify: `web/assets/css/styles.css`
- Modify: `web/assets/locales/zh-CN.js`
- Modify: `web/assets/locales/en.js`
- Extend: `web/assets/js/ui.test.js`
- Create: `web/assets/js/login.test.js` only because no existing login behavior test exists.

**Interfaces:**
- Produces browser helpers: `getWebRole()`, `isAPITokenRole()`, `getDashboardAPI(path)`.

- [ ] Add browser-unit tests for login payload selection, safe redirect, storage containing only the web-session token/expiry/role, and role-filtered navigation.
- [ ] Add administrator/API-token tabs to the existing form and submit `{mode,password}` or `{mode,token}`.
- [ ] Store only returned web-session data; clear role on logout/401.
- [ ] Filter navigation so API-token users see only overview, stats, trend, logs, and model test.
- [ ] Verify session state through `/dashboard/session` during authenticated page initialization.
- [ ] Run: `make verify-web`
- [ ] Commit: `feat(web): add api token login mode`

### Task 5: Move Read Pages to Scoped Dashboard APIs

**Files:**
- Modify: `web/index.html`
- Modify: `web/assets/js/index.js`
- Modify: `web/assets/js/stats.js`
- Modify: `web/assets/js/trend.js`
- Modify: `web/assets/js/logs.js`
- Modify: `web/assets/js/page-filters.js`
- Extend existing tests: `web/assets/js/filter-state.test.js`, relevant page tests.

**Interfaces:**
- Consumes: `/dashboard/summary`, `/dashboard/stats`, `/dashboard/metrics`, `/dashboard/logs`, `/dashboard/logs/bootstrap`, `/dashboard/models`.

- [ ] Replace authenticated page reads with `/dashboard` endpoints.
- [ ] Remove the public-summary prefetch from the authenticated overview and fetch scoped summary with web auth.
- [ ] In API-token mode, hide channel, channel-type, and auth-token filter controls; keep date/model/status filters.
- [ ] In API-token logs, suppress debug/channel/key actions and render only the safe response fields.
- [ ] Ensure forged URL filter parameters cannot restore hidden controls or broaden server results.
- [ ] Run: `make verify-web`
- [ ] Commit: `feat(web): scope dashboard pages to current token`

### Task 6: Add Token-Mode Model Testing

**Files:**
- Modify: `web/model-test.html`
- Modify: `web/assets/js/model-test.js`
- Extend: `web/assets/js/model-test.js` existing tests `web/assets/js/model-test*.test.js`
- Extend: `internal/app/proxy_integration_test.go`

**Interfaces:**
- Consumes: `/dashboard/session`, `/dashboard/v1/*`, `/dashboard/v1beta/*`.

- [ ] Add tests proving token mode offers only allowed models, sends normal protocol requests through dashboard proxy routes, and exposes no channel mutation controls.
- [ ] Keep the existing administrator channel-test UI unchanged.
- [ ] Add a token-mode UI containing protocol, allowed model, content/messages, supported options, stream toggle, run, and response.
- [ ] Map protocols to `/dashboard/v1/messages`, `/dashboard/v1/chat/completions`, `/dashboard/v1/responses`, or `/dashboard/v1beta/models/{model}:generateContent`.
- [ ] Verify proxy integration records the bound token ID and rejects disallowed models/channels and exhausted limits exactly like direct API auth.
- [ ] Run: `go test -tags sonic ./internal/app -run 'Test.*Dashboard.*Proxy|Test.*Proxy.*Token'` and `make verify-web`.
- [ ] Commit: `feat(web): test models with current api token`

### Task 7: Full Verification

**Files:**
- Modify only files required by verification failures.

- [ ] Run: `gofmt -w` on changed Go files.
- [ ] Run: `go test -tags sonic ./internal/...`.
- [ ] Run: `make race-fast`.
- [ ] Run: `make verify-web`.
- [ ] Run: `make build`.
- [ ] Run: `golangci-lint run ./...`.
- [ ] Run: `git diff --check` and inspect `git status --short`.
- [ ] Commit any verification-only fixes with a focused message.
