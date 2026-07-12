# API Token Web Dashboard Design

## Problem

The web console has one identity: an administrator session. API tokens can authenticate proxy requests, and request logs already persist `auth_token_id`, but web handlers do not derive a mandatory data scope from the authenticated subject. Hiding navigation entries would therefore be cosmetic and would leave direct API access vulnerable to cross-token queries.

The root fix is a first-class web-session identity carrying both a role and, for API-token users, an immutable token scope.

## Goals

- Reuse `/web/login.html` with administrator-password and API-token login modes.
- Let an API-token user access overview, statistics, request trend, sanitized logs, and model testing.
- Force every API-token query to the session's own `auth_token_id` on the server.
- Make the API-token experience read-only.
- Hide channels, token management, and settings from API-token users, while returning `403` for direct requests.
- Test models through the normal proxy path with the logged-in API token, without exposing channels or upstream credentials.
- Never persist the plaintext API token in browser storage.

## Non-goals

- No per-token permission editor.
- No channel-aware model testing for API-token users.
- No API-token access to active requests, debug logs, channel metadata, upstream URLs, upstream API keys, settings, or token administration.
- No duplicate `/token/*` copy of the admin API.

## Authentication Model

`POST /login` accepts a required `mode`:

- `admin`: validate `password` against the administrator password.
- `api_token`: hash and validate `token` against the active API-token store.

Both modes return a random 24-hour web-session bearer token. The plaintext API token is used only during the login request and is not returned.

Persist web sessions as:

```text
token_hash       SHA-256 of random web-session token
role             admin | api_token
auth_token_id    NULL for admin, required for api_token
expires_at       session expiry
created_at       creation time
```

The authentication middleware loads a `WebIdentity` into Gin context:

```go
type WebIdentity struct {
    Role        WebRole
    AuthTokenID int64
}
```

For `api_token` sessions, every request revalidates that the bound API token still exists, is enabled, and is not expired. Disabled, deleted, or expired tokens invalidate the web session immediately and return `401`.

## Authorization Model

Use two middleware capabilities:

- `RequireWebAuth`: accepts both roles and attaches `WebIdentity`.
- `RequireAdminAuth`: accepts only `admin`; an authenticated API-token session receives `403`.

Route groups:

- `/admin`: administrator-only mutable and sensitive APIs.
- `/dashboard`: read-only APIs shared by both roles, with server-enforced token scope.

Shared dashboard endpoints:

```text
GET  /dashboard/session
GET  /dashboard/summary
GET  /dashboard/stats
GET  /dashboard/stats/filter-options
GET  /dashboard/metrics
GET  /dashboard/logs
GET  /dashboard/logs/bootstrap
GET  /dashboard/models
POST /dashboard/model-test
```

Administrator pages may continue using existing `/admin` endpoints where appropriate. Shared frontend data loaders use `/dashboard` endpoints so one rendering path supports both roles.

## Mandatory Data Scoping

Add one helper that applies identity scope after parsing query parameters:

```go
func ApplyWebIdentityScope(c *gin.Context, filter *model.LogFilter) error
```

- Administrator: leave the requested filter unchanged.
- API-token user: overwrite `filter.AuthTokenID` with the session's token ID, ignoring any user-supplied `auth_token_id`.
- Missing or invalid identity: fail closed.

This helper is used by summary, stats, metrics, logs, models, filter options, and bootstrap queries. The summary handler must stop using an unfiltered public query for authenticated dashboards.

## Logs

API-token users receive a dedicated response projection. Allowed fields are:

- request/log ID
- time
- requested and actual model
- status code and sanitized error summary
- duration and first-byte time
- streaming flag
- input/output/cache token counts
- standard and effective cost
- service tier and thinking effort when present

Remove channel ID/name, upstream URL, upstream API-key material or hashes, client IP, debug-log availability/content, and all channel/test/delete actions. Administrators keep the current full log response.

`/dashboard/logs/bootstrap` returns only scoped models plus the current token descriptor required by filters. It must not read settings, enumerate all tokens, or enumerate channels for API-token users.

## Model Testing

API-token users do not use the existing channel test handlers. `/dashboard/model-test` accepts a small proxy-style request containing protocol, model, messages/content, streaming, and supported generation options.

The handler:

1. Obtains the bound API token from the authenticated session.
2. Rejects models outside `AllowedModels` when that list is non-empty.
3. Sends the request through the existing proxy selection, protocol conversion, accounting, limit, and logging path with that token identity.
4. Never accepts a channel ID, upstream URL, or upstream API key.

The token-mode model-test UI contains only model selection, protocol, prompt/messages, options, run, and response. Channel selection, model fetch/add/delete, channel enable, and priority controls are absent. Administrator mode retains the existing channel-oriented test UI.

## Frontend Behavior

The login page has two tabs:

- Administrator password
- API Token

Browser storage contains only the random web-session token, expiry, and non-sensitive role metadata returned by `/dashboard/session`. It never stores the submitted API token.

Navigation by role:

- `admin`: overview, channels, tokens, stats, trend, logs, model test, settings.
- `api_token`: overview, stats, trend, logs, model test.

Page initialization calls `/dashboard/session` and redirects unauthenticated users to login. Hidden navigation is usability only; server middleware remains authoritative.

The overview page switches from the unauthenticated `/public/summary` endpoint to `/dashboard/summary`. `/public/summary` may remain for the existing public homepage contract, but it is never used for a token-scoped dashboard.

## Error Handling

- Invalid login payload: `400`.
- Invalid password or API token: `401` with a generic credential error.
- Expired/disabled/deleted API token backing a session: `401`, delete the session.
- API-token access to administrator APIs: `403`.
- Attempted foreign `auth_token_id`: ignored and overwritten by the session scope.
- Unsupported or disallowed model test: `400` or `403` without revealing channel availability.

## Verification

Meaningful behavior tests cover:

- both login modes and generic credential failures;
- persisted role/token binding and restart reload;
- immediate invalidation when the backing API token becomes invalid;
- administrator access versus API-token `403` on mutable APIs;
- forced log/stat/metric/model scoping despite a forged query token ID;
- log response sanitization;
- model tests using only allowed models and recording the bound token ID;
- frontend role-based navigation and token-login payload/session storage behavior.

Run existing backend tests, web tests, lint/type checks, and builds according to `CLAUDE.md`.
