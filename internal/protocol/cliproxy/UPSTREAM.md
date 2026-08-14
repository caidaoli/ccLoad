# CLIProxyAPI translator provenance

- Repository: `https://github.com/caidaoli/CLIProxyAPI`
- Module source path: `github.com/router-for-me/CLIProxyAPI/v7`
- Last synchronized commit: `b6924c1db93fc8926fc43dfe9d95e1e0da76d406` (`fork/v8.65.0`)
- Synchronized at: `2026-08-12`

This directory is maintained by one atomic synchronization operation. It currently
contains the four-protocol conversion core. Allowlisted provider-specific pure
translators enter `providers/` through that same operation; the provider section
below records what is actually present. Core and imported provider adapters always
share the same upstream commit and synchronization date. Authentication,
configuration, routing, cache services, plugins, dynamic registries, executors,
and network refreshers are intentionally excluded. ccLoad-specific generic wire
adaptation lives in `internal/protocol/builtin`.

## Provider adapter snapshot

The canonical synchronization operation audits the semantic boundary in
`.agents/skills/sync-cliproxy-core/references/provider-adapters.md` and the sole
machine-readable file/exclusion/wiring/test allowlist in the adjacent
`provider-adapters.manifest`. Provider adapters are never synchronized in a
second pass or assigned an independent version.

Antigravity is the first eligible provider adapter:

- Upstream sources: `internal/translator/antigravity/{claude,gemini,openai/chat-completions,openai/responses}`
- Local destination: `internal/protocol/cliproxy/providers/antigravity`
- Snapshot status: synchronized at shared commit
- Excluded: dynamic `init.go` registration, noop/allocation tests, runtime cache/logging services, executors, auth, Interactions, and the two Claude request/response suites coupled to those runtime services
- Pure provider tests synchronized: 8; ccLoad HTTP wire contracts cover request, non-stream response, and stream response for Claude, Codex, Gemini, and OpenAI clients

## Synchronized tests

The core snapshot includes 53 `_test.go` files from the same commit as the
production sources:

- `claude/gemini`: 2
- `claude/openai/chat-completions`: 3
- `claude/openai/responses`: 3
- `codex/claude`: 4
- `codex/gemini`: 2
- `codex/openai/chat-completions`: 2
- `codex/openai/responses`: 2
- `common`: 6
- `gemini/claude`: 3
- `gemini/openai/chat-completions`: 4
- `gemini/openai/responses`: 3
- `openai/claude`: 3
- `openai/gemini`: 2
- `openai/openai/responses`: 2
- `signature`: 8
- `util`: 4

Tests for excluded packages are not copied. Performance-only benchmarks are
also excluded: the translator-wide benchmark requires the excluded dynamic
Registry and Interactions paths, while the Claude-to-Codex
benchmark measures allocation details rather than a wire contract. Upstream
`noop_optimization_test.go` files and allocation-reuse assertions are likewise
excluded because they test private implementation and memory reuse instead of
the public conversion contract. The `thinking` package keeps only the pure
conversion sources (`convert.go`, `suffix.go`, `text.go`, `types.go`); upstream's
runtime thinking application (`apply.go`, `strip.go`, `summary.go`,
`validate.go`, `errors.go`, `provider/`) and its tests stay excluded, as does
the upstream SDK translator Registry and its summary test. The OpenAI-to-OpenAI
Chat Completions no-op converter and its post-`[DONE]` tests are excluded because
ccLoad's Registry defines same-protocol traffic as byte-for-byte passthrough and
never registers same-protocol converters.

## Local contract fixes

The snapshot is intentionally maintained in ccLoad instead of imported as a
runtime module. ccLoad carries protocol fixes required by its Registry contract,
including canonical Anthropic JSON/SSE non-stream responses, terminal SSE
events, cross-chunk tool arguments, reasoning/signature propagation, usage
details, and mixed Chat Completions/Responses ingress handling.

The synchronized tests keep their upstream behavior coverage, with only these
documented adaptations:

- module imports point at `ccLoad/internal/protocol/cliproxy`;
- the excluded upstream SDK Registry helper calls the exported core stream
  converter directly;
- assertions follow ccLoad's public wire contract for native non-stream JSON,
  Gemini camelCase fields, Codex top-level `instructions`, system-only prompts
  preserved as the sole user content, terminal `[DONE]`, protocol-specific
  cache-creation usage, and unsigned Anthropic thinking preserved as OpenAI
  reasoning.
- Codex-to-OpenAI Chat Completions maps cache-write usage to
  `prompt_tokens_details.cached_creation_tokens` in both streaming and
  non-streaming responses, and does not expose Codex encrypted reasoning
  carriers; readable reasoning summaries remain available as `reasoning_content`.
- Codex-to-Gemini requests keep the caller's `stream` flag and do not force
  `reasoning.summary`.
- OpenAI Chat Completions-to-Responses streaming synthesizes a terminal choice
  when `[DONE]` arrives without `finish_reason`, so item-level done events are
  emitted before `response.completed`; upstream `fork/v8.61.0` emits only the
  completed response in that case.
- OpenAI Chat Completions-to-Codex maps `web_search_options` to a Responses
  `web_search` tool while preserving its search context and user location.
- Claude-to-Codex keeps top-level system text in `instructions`, supports the
  broader ccLoad URL/file/redacted-thinking input shapes, and omits an empty
  `input` array for instructions-only requests.
- Claude-to-Gemini preserves an absent adaptive effort and performs the
  excluded runtime `ApplyThinking` level normalization inline: exact target
  levels are retained, unsupported valid levels are clamped to the nearest
  declared level (lower wins ties), and Antigravity level-suffixed Gemini model
  names resolve capabilities through their base model.
- Claude Responses native non-stream JSON keeps ccLoad request-field echoing,
  cache-creation and reasoning usage details, and the same marked
  redacted-thinking carrier used by the synchronized SSE path.
- Gemini Responses `[DONE]` finalization is upstream behavior as of
  `fork/v8.57.0`; the previously documented local divergence was adopted
  upstream.
- Gemini signature sanitization keeps upstream signature ownership and parallel
  function-call semantics without importing its runtime debug logger.
- Antigravity adapters keep only request-local conversion state. Runtime signature
  caches, dynamic model registries, and logging side effects remain outside the
  provider packages; OpenAI summary aliases are normalized locally as wire data.
  The shared Antigravity wire finalizer also performs the excluded runtime
  `ApplyThinking` effort-alias normalization (`minimal` to `low`, `xhigh`/`max`
  to `high`) for every client protocol before the request is sent.
- Antigravity stream payloads are framed at the app boundary because the upstream
  executor normally supplies SSE delimiters; ccLoad writes provider chunks directly.
  The app boundary preserves the client's streaming mode when choosing
  `generateContent` versus `streamGenerateContent`; both modes share the same
  ordered provider base-URL fallback policy.
- Executor/runtime parity remains implemented at the app boundary rather than in
  this snapshot. Antigravity uses refresh-token-scoped HTTP/1.1 pools with native
  keepalive limits and bounded LRU eviction. Its runtime User-Agent is resolved
  once at startup from the official Hub updater manifest, falls back to Hub 2.8.1,
  and is shared by data, project/model, and quota requests (the OAuth token
  endpoint retains its native client UA); its request finalizer performs one
  object-tree rewrite per attempt. Claude OAuth preserves confirmed native and
  measured Haiku-helper request shapes, owns cache placement only for cloaked
  callers, rejects legacy mid-conversation system messages locally for Anthropic's
  first-party origin, and removes only automatically injected context management
  that outlives eligible thinking.
  The excluded plugin registry and Antigravity reasoning replay cache still have
  no ccLoad runtime equivalent, so plugin-hook and replay-index changes are not
  copied as translator code.
- ccLoad has no Kimi OAuth authenticator or executor. The unused top-level Kimi
  OAuth model catalog is therefore omitted from the embedded registry. Generic
  API-key channels remain model-agnostic and retain Kimi pricing and wire-format
  compatibility.
- `fork/v8.65.0` also rewrites Gemini function-call pairing validation to use
  short-circuiting `gjson.ForEach`; ccLoad carries that pure control-flow update
  while preserving its local `ccLoad/internal/protocol/cliproxy/util` import and
  established error strings.
- The target snapshot's Responses-to-Chat conversion still keeps ccLoad wire
  extensions for `reasoning.content`, cache-creation usage, `input_file`, and
  Responses `web_search` to `web_search_options`; these are intentional local
  contract differences and must survive future upstream syncs.
- Claude-target request converters never synthesize process-global
  `metadata.user_id`. Provider session identity belongs to ccLoad's request
  boundary, where Anthropic credentials and `Session-Id` + `Thread-Id` are
  available; keeping it out of the pure converters prevents cross-request
  identity collisions and the upstream package-global initialization race.
- The embedded capability catalog exposes Antigravity models to Gemini wire
  conversion. `gemini-3.7-flash-high` follows the canonical entry added by
  `router-for-me/models` commit `cbe1e6c59429bc92dd8d6654873670fc0c274cad`;
  the CLIProxyAPI snapshot commit above remains unchanged.

## Updating from CLIProxyAPI

Run this procedure through the repository skill: use `$sync-cliproxy-core` in
Codex or `/sync-cliproxy-core` in Claude Code. Both entry points resolve to the
canonical skill under `.agents/skills/sync-cliproxy-core`.

1. Fetch the ccLoad CLIProxyAPI fork and choose one immutable commit or tag.
2. Generate one combined diff for the four-protocol core and every provider in
   the canonical allowlist. All production sources and tests must come from that commit.
3. Copy changed pure conversion files and matching tests only; do not add a Go
   module import, `replace`, authentication, configuration, routing, cache services,
   plugins, SDK registries, executors, or network update code.
4. Port provider-specific pure request/response semantics into `providers/<provider>`
   and connect request, non-stream response, and stream response paths. Keep
   Interactions excluded until ccLoad supports that public wire protocol.
5. Resolve the combined diff against Registry and provider wire contracts. If any
   domain cannot be integrated or tested, leave the whole synchronization incomplete.
6. After core, all providers, production wiring, and tests pass, update the single
   shared commit and date above once.
7. Run `go test -tags sonic ./internal/protocol/cliproxy/...`,
   `go test -tags sonic ./internal/protocol`, and the repository verification
   commands from `CLAUDE.md`.

The upstream core/provider tests prove the snapshot was synchronized without
losing conversion behavior. Registry and provider boundary tests remain ccLoad's
compatibility authority. A future upstream sync is incomplete if either layer
fails, any of the 12 core request/non-stream/stream directions regresses, or an
allowlisted provider is omitted.
