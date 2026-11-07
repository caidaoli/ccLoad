# Accio protocol reference

ccLoad's Accio implementation is independently written Go code. No agent2api
implementation, static model table, or account storage workflow is copied or
translated. The reference is used only to identify externally observable protocol
facts; its MIT license and additional restrictions are not imported as ccLoad code.

Reference: [Agent2API/agent2api](https://github.com/Agent2API/agent2api), commit
`072e3c71d253cd6dc265f3656d8cb481dd7c9f84`.
Files read in full under
`desktop-tauri/src-tauri/server/src/server/core/providers/accio/`:
`credentials.rs`, `refresh.rs`, `auth.rs`, `endpoints.rs`, `models.rs`,
`balance.rs`, `oauth.rs`, `protocol.rs`, `stream.rs`, and `chat.rs`.
The same commit's `server/core/egress.rs` supplies its default `undici` User-Agent.

The 2026-11-07 comparison follows [aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API)
at `e8bed7397aaf9d8962489074557c2f1f1a8d4f97`, whose migration document points to
[ProvidersPlugin/Agent2Providers](https://github.com/ProvidersPlugin/Agent2Providers)
at `3055f416841c1393ec04294a702603fd41e840aa`. Its Accio protocol files live in
`crates/providers/src/providers/accio/`. That plugin is AGPL-3.0; only protocol
facts are used, with no implementation copied or translated.

Protocol facts used:

- Both editions use `https://phoenix-gw.alibaba.com`; `x-package-region`
  distinguishes `CN` and `GLOBAL`.
- All outbound operations share Accio headers: JSON
  Content-Type/Accept, `Accept-Language: en`, `x-language: en`,
  `x-platform: desktop`, `x-os` (host OS; macOS uses `macos`),
  `x-app-version: 0.34.4`, and the edition's `x-package-region`.
  All requests use a standard Chrome/146 browser UA without ccLoad branding.
  Inference overrides Accept to SSE and adds
  `version: 0.34.4` and `appKey: OneChat`. No Bearer header is added.
  A user-authorized GLOBAL import on 2026-10-20 returned business code 423
  with HTTP 200 under Go's default UA; the same credential succeeded with an
  explicit UA. The original 0.32.6 shared headers passed identity verification and
  imported 43 visible chat models. That check validated import, not live OAuth or inference.
- Browser login uses `https://www.accio-ai.com/login` (CN) or
  `https://www.accio.com/login` (GLOBAL), with `return_url`, `state`,
  `code_challenge`, `code_challenge_method=S256`, and `client_id=accio-work`.
  `POST /api/oauth/token` takes `code`, `codeVerifier`, `clientId`, and
  `redirectUri` (identical to `return_url`) and returns camelCase token fields.
- `POST /api/auth/userinfo` authenticates with JSON body `accessToken`.
  This corrects the reference commit's GET assumption: a live unauthenticated
  probe on 2026-10-20 returned HTTP 405 for GET, business `401 NOT_LOGIN` for
  POST with an invalid `accessToken`, and business 400 (missing `accessToken`)
  for POST with `token` or query-only credentials. No real credentials were used.
- `GET /api/entitlement/quota` and `/api/entitlement/currentSubscription`
  authenticate with query `accessToken`; the same probe confirmed GET reaches
  authentication while POST returns HTTP 405.
- `POST /api/auth/refresh_token` takes `accessToken` and `refreshToken` and
  returns `accessToken`, optional rotated `refreshToken`, and `expiresAt`.
- Official desktop token JSON uses camelCase; ccLoad persists its own snake_case
  envelope with an explicit `region` and Unix-second expiry. Millisecond and
  RFC3339 official expiries normalize on import. No local desktop files are read.
- Optional `sgcookie` / `sgCookie` is retained on import, OAuth exchange and refresh,
  persisted as `sgcookie`, and sent only as an inference header. Missing or empty
  refresh/reimport fields preserve the existing value. Values must be at most
  4096 bytes of visible ASCII (spaces allowed); invalid values are rejected without
  echoing them. Debug headers are redacted. A concurrent cookie-only update is
  preserved while publishing a successful token rotation, rather than discarding
  the rotated tokens as though the account had been reauthorized.
- `POST /api/llm/config` takes `token` and `supportAutoModel: true`. The `data`
  provider array (or older `{providers: [...]}` envelope) contains `modelList`.
  `modelName` is both the public model ID and the inference `model` value.
  Public lists deduplicate names and do not generate `redirect_model` values.
  Entries without names are skipped. `modelCode` is ignored; saved code redirects
  are no longer resolved and must be replaced with model names.
  A live GLOBAL catalog on 2026-10-20 confirmed readable `modelName` values
  alongside opaque codes (for example `gpt-6-astra` / `1Helix-G6aS8tR2qN7m`).
  `modelDisplayName`, `protocol`, `reasoningEfforts`, `contextWindow`, and
  `multimodal` are retained without inferring a different model or output limit.
  Explicitly invisible and image/audio/video generation entries are excluded.
- Quota supplies `usagePercent` and `refreshCountdownSeconds`, not a dollar
  balance. Subscription absolute quantities and declared unit remain separate.

ccLoad decisions: refresh margin is one minute; omitted refresh expiry becomes
unknown rather than a fabricated TTL; missing/invalid quota and an empty visible
chat catalog are errors. Browser OAuth reuses ccLoad's in-memory OAuth manager
and validates identity before persisting credentials. HTTP 403 is normalized to 401 for credential refresh.
Control-plane errors expose the endpoint path, actual HTTP status and numeric
business code, while retaining normalized status separately for retry decisions;
upstream response messages and token-bearing URLs are never exposed.
Subscription is optional: failures preserve quota unless authentication fails or
the request context is canceled. No static model fallback, model aliases, SDK-generated
fingerprints, raw response exposure, or redirect following is added.

Inference uses `POST /api/adk/llm/generateContent?sg_k=md5(request_id)` with
body `token`, `message_id`, `tenant=accio-agent`, `iai_tag=phoenix-desktop`,
`properties.normalized_response="true"`, and the `appKey`/version/region headers.
ADK uses snake_case content parts and JSON-string tool declarations/arguments/results.
Catalog protocol `openai`/`responses` puts reasoning effort in `properties`;
other protocols put it at the top level. Native SSE camelCase/snake_case fields
normalize to Chat text, reasoning, function calls, usage and explicit terminal states.
An object-valued `content` may omit `parts` or contain `parts: null` on an
empty/metadata frame; neither is itself a completion signal. Explicit malformed
non-array parts remain errors. Finish/turn-complete fields and usage must still
be processed when content has no parts.
Unexpected EOF remains an error; output already sent is never automatically replayed.

Model-name routing follows the user-provided local `accio-manager` checkout,
commit `e0cd0dfa6cc88eecc7dd474ed322d4b7996d99b2`:
`accio_panel/model_catalog.py` reads `modelName` and deduplicates public names;
`accio_panel/gemini_proxy.py:build_generate_content_request` sends that name
unchanged in `model`. Only model selection is aligned here; other request fields
remain unchanged. This change is verified with mock upstream tests, not a new
successful live call from the previously blocked local egress.

Live inference findings (2026-10-20):

- An omitted `utdid` header returned HTTP 200 `text/html` with `Bxpunish: 1`.
  Changing only User-Agent did not fix inference. Supplying the imported device
  ID or a reference-style `desktop-...` client ID produced real SSE instead.
- Official `utdid`/`deviceId` is preserved as canonical `device_id` and sent on
  inference. If absent, ccLoad derives a stable `desktop-` identifier from
  SHA-256 of `accio/<region>/<verified user_id>` (first 16 bytes, hex), so existing
  credentials and browser OAuth do not change device identity on token refresh.
  Explicit imported IDs take precedence; reauthorization without one preserves
  an existing imported ID. No hardware fingerprint or browser cookie is fabricated.
- With the device header, both Auto and a catalog model returned native SSE
  `error_code: 555`, `blocked by sentinel rate limit`; this is classified as 429.
  The user subsequently obtained generated text through a proxy using the current
  request format, followed by a local `invalid SSE parts` normalization error.
  This supports an egress-dependent rejection; it does not establish account
  exhaustion or a header/body defect. Empty-content terminal frames are covered
  by regression tests; successful live completion after the fix remains unverified.
- Gateway challenge responses are classified before SSE normalization; they do
  not become `unexpected EOF` or trigger token refresh. Challenge cookies and
  redirect locations are omitted from normalized diagnostics. Management tests
  report the native outbound request, masking body token and device headers.

Accio system/developer text first replaces literal `Claude` and `Codex` with
`Accio`, then applies configured zero-width obfuscation. Replacement is case
sensitive and applies to both string and text-block content after channel body
rules, without rewriting user/assistant content, models, or tool data. An empty
sensitive-word list disables only zero-width insertion. `Claude` replacement
follows accio-manager's system-instruction handling; `Codex` is a user-requested
extension. Live replay of the reported Sonnet 4.6 prompt on 2026-10-20 repeatedly
failed with original/zero-width-only text and succeeded with `Claude` replacement,
including when followed by zero-width handling.
The updated product preparation path was then replayed with a prompt containing
both Claude Code and Codex CLI text: HTTP 200, no SSE error, and a complete
`[DONE]` terminator. The temporary live diagnostic test was removed afterward.

Live inference findings (2026-11-07):

- The reported GLOBAL `accio_gateway_blocked` failure was reproduced with the
  configured account and direct egress: `User-Agent: undici` returned HTTP 200
  HTML with `Bxpunish: 1`. Changing only the UA returned real SSE with text and
  a terminal frame; changing only the version headers or appKey remained blocked.
- The final standard Chrome/146 browser UA (no ccLoad product token), paired
  with version 0.34.4 and appKey OneChat, passed raw SSE and both streaming and
  non-streaming management inference with the catalog's `auto` model. The account
  had a device ID but no sgcookie, so the fix does not require reimporting it.
  Channel configuration was read only; requests used an isolated in-memory server.
  No credential refresh, production configuration write or deployment was performed.
- The same standard browser UA also passed userinfo, the 39-model catalog,
  quota and subscription queries. Control-plane and inference now share that UA;
  no Accio request retains `undici`.
- Cookie persistence/rotation, reauthorization races and redaction are covered
  by mock HTTP tests. Live browser OAuth and CN inference were not exercised.
