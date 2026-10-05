# CLIProxyAPI translator provenance

- Repository: `https://github.com/caidaoli/CLIProxyAPI`
- Module source path: `github.com/router-for-me/CLIProxyAPI/v8`
- Last synchronized commit: `a6dfa6bdf07fc1ad68bb513f60b49e84e2720148` (`fork/v10.3.0`)
- Synchronized at: `2026-10-04`

This directory is maintained by one atomic synchronization operation. It currently
contains the four-protocol conversion core. Allowlisted provider-specific pure
translators enter `providers/` through that same operation; the provider section
below records what is actually present. Core and imported provider adapters always
share the same upstream commit and synchronization date. Authentication,
configuration, routing, cache services, plugins, dynamic registries, executors,
and network refreshers are intentionally excluded. ccLoad-specific generic wire
adaptation lives in `internal/protocol/builtin`.

The machine-readable core scope and the reviewed core/provider per-file delta
for the latest atomic synchronization live in
`.agents/skills/sync-cliproxy-core/references/core-snapshot.manifest`. Full sync
verification compares the previous immutable commit with the commit above and
fails on every unclassified or unstamped core change. The manifest deliberately
does not carry a second commit or date; the previous commit is anchored to the
version of this file stored in Git `HEAD` before the synchronization edits.

## Synchronization adaptations (2026-10-04)

The core and the allowlisted Antigravity adapter share the target above.
Adopted: Antigravity Claude 5.5 double-layer CAQS signature validation and
replay gating, the shared synthetic `signaturetest` envelope, the local shell
tool for OpenAI Responses, Codex URL citation annotations in Chat output,
Claude Responses request fixes, Gemini Responses usage/terminal handling, and
the target model catalog.

The Gemini-to-Responses converter now waits for usage or `[DONE]` after a
`finishReason`. Gemini has no `[DONE]` on the wire; upstream's executor feeds
one at EOF, and ccLoad's application boundary now synthesizes the same
terminator for Gemini upstreams with Responses clients when the source stream
is semantically complete (Zed's Gemini relay does the same on `stream_ended`).
Gemini-to-Claude is not fed a synthetic `[DONE]`: it drops `finishReason` on
`[DONE]` and would report `MAX_TOKENS` as `end_turn`.

The ported `pause_turn` buffered test calls the non-stream converter directly.
ccLoad's production non-stream path first runs `NormalizeAnthropicResponse`,
which rejects `server_tool_use` / `web_search_tool_result` blocks, so
non-stream Claude server-tool responses remain unsupported; streaming is
unaffected.

Upstream's `common/claude_native_response.go` (native Claude JSON replayed as
SSE) is excluded. ccLoad keeps its direct native JSON converters, which emit
one output item per Claude block and preserve structured reasoning, usage
details and `apply_patch` identity; the ported native-response test uses a
single text block accordingly. The Codex request converter keeps preserving the
client `include` list, so the two upstream include-normalization tests are
recorded as `skip-test`. Antigravity in-stream error frames, multi-line
payloads and post-terminal client cancellation are handled at ccLoad's wire
boundary rather than by the excluded executor.

## Synchronization adaptations (2026-10-02)

The four-protocol core and every allowlisted Antigravity adapter share the
explicit target above. The pure `internal/client/codex/apply-patch` helpers are
mapped to `applypatch/`; the shared input decoder, event builders and Responses
bridge are mapped to `common/`. No upstream client runtime, executor, plugin,
configuration system, or Interactions wire support is imported.

Custom `apply_patch` declarations now retain their grammar while describing the
JSON input wrapper required by function-tool upstreams. Claude no longer drops
the tool. Responses output restores custom-tool identity, preserves patch
whitespace and Unicode, emits decoded input deltas, and rejects invalid argument
envelopes and conflicting identity/snapshot evidence. Antigravity reuses the
Gemini converter and carries its matching provider contract test. Ordinary
same-name functions retain their original identity and behavior.

ccLoad's Registry/builtin and application boundaries consume tool input errors
and finalize converter state at EOF. When the upstream already carried its
semantic terminal (for example an OpenAI `finish_reason` without `[DONE]`),
ccLoad synthesizes the terminator first so finalization only rejects truly
truncated patch-enabled streams; transport errors keep their original error.
Failure chunks
are delivered before returning conversion errors, and a completed upstream
cannot erase a conversion failure. Existing upstream errors take precedence
over missing-terminator checks. HTTP, Antigravity, Cursor and Zed paths retain
their cancellation and error behavior; parsed usage is preserved even when
non-stream response translation fails. Native same-protocol passthrough and
the client model catalog's disabled `apply_patch_tool_type` remain unchanged.

The Codex-to-Chat converter also carries a local correction to the target:
input.done, output_item.done and response.completed snapshots complete any
unstreamed patch suffix. Conflicting snapshots and a missing source terminator
are reported through the same tool-error boundary instead of returning a
truncated successful function call. Native Claude JSON remains supported with
the new custom-tool validation; cache accounting, terminal SSE events, client
tool_search, and namespace restoration retain ccLoad's existing contracts.
The Chat-to-Codex request and Codex-to-Chat response converters also accept
OpenAI Chat's nested custom tools (`{"type":"custom","custom":{...}}`) as a
local correction. Declarations, grammar formats and tool_choice are flattened
to the Responses shape. Calls to those tools return as native Chat
`type:"custom"` tool_calls with raw input.
Flat custom declarations keep the function-envelope behavior.

Other adopted changes include self-terminating shared SSE frames, Claude tool
name collision handling and thinking replay separation, explicit Claude effort
preservation, empty Codex function-history arguments normalized to `{}`, and the
target model catalog. The locally added Antigravity model entry remains intact.
Gemini diagnostic logging remains excluded; its new log-only tests, the opaque
Responses output digest, and two allocation/literal-description tests are
explicitly recorded in the manifest. The runtime thinking-policy test remains
excluded with its implementation. The license and upstream attribution are unchanged.

## Synchronization adaptations (2026-09-30)

Core sources remain the four-protocol trees and shared helpers under
`internal/translator/{claude,codex,gemini,openai,common}`, plus the allowlisted
`internal/{signature,thinking,util,misc}` sources and embedded model catalog.
They are synchronized under `internal/protocol/cliproxy`; Antigravity's source
and destination directories are recorded in the provider section below.
The upstream module changed from `/v7` to `/v8`; all local imports remain under
`ccLoad/internal/protocol/cliproxy`, with no runtime module dependency.

Chat Completions-to-Codex now normalizes trimmed, case-insensitive `fast` and
`priority` service tiers to `priority`, preserves `ultrafast`, and omits other
values. Claude requests targeting Antigravity Claude models now isolate tool
results immediately after their model call turn, combine parallel result turns,
and move intervening text/reminders after those results. Gemini model targets
retain their existing merge behavior. The shared split helper and public model
predicates are imported with their tests; `util/claude_model.go` is no longer
excluded as a runtime helper because the provider converter directly uses its
pure model classification. The existing HTTP provider contract tests cover the
different Claude/Gemini ordering rules.

Antigravity Responses summary visibility still belongs to ccLoad's application
boundary. Upstream's `thinking.ExtractSummaryConfig`/`ApplySummaryConfig` changes
are not imported into the pure provider; the synchronized summary test retains
the local no-injection contract for explicit auto/null/none/legacy summary
values as well as effort-only input. Existing signature, usage/cache, SSE,
request-validation, and same-protocol passthrough contracts remain intact.

The new `thinking/configuration_update.go` helpers are used only by the excluded
runtime `thinking/apply.go` policy. No mapped converter moved field injection to
that layer. The model catalog's new `support_configuration_update` flags are
copied as source data but are not consumed by ccLoad's static loader; this sync
does not enable the upstream runtime policy. Authentication, dynamic registries,
executors, caches, logging, Interactions, and runtime-only tests remain excluded.
The upstream license and attribution are unchanged.

## Synchronization adaptations (2026-09-25)

The earlier one-entry `claude-opus-5-5` catalog backport is absorbed by the full
`models.json` sync. Claude-to-OpenAI Chat Completions now follows upstream for
`tool_result` media: the tool message keeps the text, and images/documents are
relayed in the immediately following user message. Gemini Responses input files
use camelCase `fileData` with the upstream `file_id` fallback; Codex/OpenAI cache
aliases and the `fast` to `priority` service tier follow upstream precedence.

Responses-to-Chat adopts upstream's request-scoped `responsesToolIndex` for
declaration, namespace, and custom-tool lookup. ccLoad's native client
`tool_search` bridge is layered on top of that index instead of keeping a second
scan. Upstream's `thinking.ApplyTranslatedSummaryToClaude` call sites in the
Claude-target request converters stay excluded: summary visibility depends on the
excluded dynamic registry and remains an application runtime policy. The new
Antigravity Responses `RequestEnvelope` entry point and its dynamic web-search
capability probes are SDK/runtime code and stay excluded; its pure media tests are
synchronized. Upstream gemini sanitizer log-suppression tests require the
excluded logrus runtime. Helpers left without callers by upstream's
`responsesToolIndex` refactor are dropped locally so lint stays clean.

## Synchronization adaptations (2026-09-17)

The earlier scoped Antigravity search/reminder backport is now included in the
shared atomic baseline. Responses tool history repair, namespace-aware tool name
capping/collision handling and tool choice conversion, OpenAI reasoning aliases
and interrupted tool-call finish reasons are synchronized with their tests.
Single-system-message input remains the user's sole prompt; only mid-conversation
system/developer messages receive the reminder envelope.

Native search capability reads use the immutable model catalog, without dynamic
registration or probes. Gemini Responses request tests use a catalog model with
explicit search capability; response tests pass the effective translated Gemini
request so search buffering reflects the actual outgoing tools. The upstream
runtime-only dynamic capability veto test is explicitly excluded in the manifest;
provider isolation is checked through generic versus dedicated search requests. Grounding and citation behavior is tested through public converter
outputs rather than private merge helpers. The new search test file follows its
upstream source; previously backported duplicate tests were consolidated into it.
Antigravity keeps app-selected dedicated search routing and request-driven
thinking visibility. Claude usage/cache accounting and stream termination fixes
remain local contracts. Codex Responses Lite HTTP/WS header recognition stays
excluded because it is only used by upstream runtime executors and handlers.

Signature recovery lives in `internal/app/antigravity_replay.go`, outside this
pure snapshot. It restores Anthropic thinking signatures and Responses reasoning
or detached text carriers within caller/session/account/model/origin/protocol
boundaries, commits only complete successful responses, and invalidates the scope
on signature rejection. Explicit carriers remain authoritative. No upstream
runtime cache or executor is imported, and signature-400 history-stripping retries
are removed. The Hub UA fallback is `2.9.1` in the app auth service.

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
- Pure provider test files synchronized: 8; ccLoad HTTP wire contracts cover request, non-stream response, and stream response for Claude, Codex, Gemini, and OpenAI clients

## Synchronized tests

The core snapshot includes 79 `_test.go` files from the same commit as the
production sources:

- `applypatch`: 1
- `claude/gemini`: 2
- `claude/openai/chat-completions`: 3
- `claude/openai/responses`: 9
- `codex/claude`: 4
- `codex/gemini`: 2
- `codex/openai/chat-completions`: 2
- `codex/openai/responses`: 2
- `common`: 14
- `gemini/claude`: 3
- `gemini/openai/chat-completions`: 4
- `gemini/openai/responses`: 6
- `openai/claude`: 3
- `openai/gemini`: 2
- `openai/openai/responses`: 7
- `signature`: 8
- `util`: 7

Tests for excluded packages are not copied. Performance-only benchmarks are
also excluded: the translator-wide benchmark requires the excluded dynamic
Registry and Interactions paths, while the Claude-to-Codex
benchmark measures allocation details rather than a wire contract. Upstream
`noop_optimization_test.go` files and allocation-reuse assertions are likewise
excluded because they test private implementation and memory reuse instead of
the public conversion contract. This also excludes
`TestParseGJSONBytesNoCopyReferencesInput`, `TestSortByDepthUsesSegmentsAndIsStable`,
and `TestByteEntropyRatio_SingleByteReturnsZero`: pointer identity, internal path
sorting, and entropy for a one-byte buffer rejected by the public validator's
length check do not define converter behavior. JSON parsing, nested schema
cleanup, and malformed/low-entropy signature rejection remain covered.
The `thinking` package keeps only the pure
conversion sources (`convert.go`, `suffix.go`, `text.go`, `types.go`); upstream's
runtime thinking application (`apply.go`, `configuration_update.go`, `strip.go`, `summary.go`,
`validate.go`, `errors.go`, `provider/`) and its tests stay excluded, as does
the upstream SDK translator Registry and its summary test. The OpenAI-to-OpenAI
Chat Completions no-op converter and its post-`[DONE]` tests are excluded because
ccLoad's Registry defines same-protocol traffic as byte-for-byte passthrough and
never registers same-protocol converters.
The `common/antigravity_tools.go` and `common/devin_tools.go` helpers and their
tests are also excluded: their tool-name mapping is used exclusively by the
unsupported Interactions translators, not the registered generateContent or
Chat Completions adapters. Gemini
Responses `trailing_signature.go` and its tests are excluded because they bind
the converter to the runtime reasoning replay cache; ccLoad keeps the explicit
pure-wire signature carrier and removes it before emitting Gemini wire data.

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
- Codex-to-OpenAI Chat Completions maps cache-write usage to both
  `prompt_tokens_details.cached_creation_tokens` and `cache_write_tokens` in
  streaming and non-streaming responses. It also preserves a valid upstream
  `service_tier` across the streaming lifecycle, and does not expose Codex
  encrypted reasoning carriers; readable reasoning summaries remain available
  as `reasoning_content`; the local top-level `cache_creation_input_tokens` alias
  remains accepted.
- Codex-to-Claude maps both top-level `cache_creation_input_tokens` and
  `input_tokens_details.cache_write_tokens` (including its
  `cache_creation_tokens` alias) to Anthropic
  `cache_creation_input_tokens`, and subtracts cache reads and writes from the
  reported uncached input count.
- OpenAI-to-Claude also subtracts both cache reads and writes from uncached input.
  It accepts ccLoad's `cached_creation_tokens` extension and top-level
  `cache_creation_input_tokens` alongside upstream's cache-write aliases, in
  streaming and native non-stream JSON. Upstream's tests are adapted to avoid
  counting cache writes twice.
- Claude-to-OpenAI relays `tool_result` images and documents in the following
  user message, matching upstream; OpenAI-compatible providers reject media parts
  inside `role: tool` content.
- Claude-to-OpenAI streaming emits the trailing empty-choices usage chunk only
  when the original request sets `stream_options.include_usage=true`; otherwise
  `message_stop` maps directly to the terminal `[DONE]` frame.
- Codex-to-OpenAI accepts both `reasoning_summary_text` and `reasoning_text`
  stream events. Non-stream output preserves ccLoad's full-content-first rule:
  reasoning `content` wins when present, with `summary` as the fallback, rather
  than concatenating two representations of the same reasoning.
- Codex-to-Gemini requests keep the caller's `stream` flag and do not force
  `reasoning.summary`.
- OpenAI Chat Completions-to-Responses keeps ccLoad's custom-tool namespace and
  usage extensions around the synchronized terminal state machine. Plain-text
  streams may still complete on `[DONE]` without `finish_reason`; reasoning-only
  streams without an explicit finish and partial/invalid tool streams do not
  report false completion. All buffered tool states participate in that guard,
  even before an ID or name arrives. An explicit reasoning stop still completes,
  while `length` and `content_filter` terminate as `response.incomplete` even
  when no message or tool item was emitted.
- OpenAI Chat Completions-to-Codex maps `web_search_options` to a Responses
  `web_search` tool while preserving its search context and user location.
- Claude-to-Codex keeps top-level system text in `instructions`, supports the
  broader ccLoad URL/file/redacted-thinking input shapes, and omits an empty
  `input` array for instructions-only requests.
- Claude-target Responses requests map a plain string `input` to a single user
  message. The former local branch was adopted upstream and removed locally.
- Claude/OpenAI Responses now preserves server-side web search as a replayable
  `web_search_call`, including encrypted result carriers and citation indices.
  Streaming and non-streaming output keep text/search/text order and contiguous
  output indices. The pure core intentionally omits upstream debug logging.
- Claude Fable, Opus 5, and Sonnet 4.6 targets drop a trailing assistant prefill
  and synthesize a user fallback when that was the only turn; compatibility mode retains the prefill.
  Claude-to-OpenAI Chat Completions assigns tool calls their own zero-based,
  contiguous indices instead of leaking Anthropic content-block indices.
- Claude-to-Codex maps `output_config.format` JSON schema settings to Responses
  `text.format`. Gemini-to-Claude reports cached prompt tokens separately as
  `cache_read_input_tokens` and subtracts them from uncached input tokens.
- Claude-to-Gemini preserves an absent adaptive effort and performs the
  excluded runtime `ApplyThinking` level normalization inline: exact target
  levels are retained, unsupported valid levels are clamped to the nearest
  declared level (lower wins ties), and Antigravity level-suffixed Gemini model
  names resolve capabilities through their base model.
- Claude Responses native non-stream JSON keeps ccLoad request-field echoing,
  cache-creation and reasoning usage details, and the same marked
  redacted-thinking carrier used by the synchronized SSE path.
  Both native JSON and SSE now report `max_tokens` as `incomplete`, mark the
  final partial output item, and carry `max_output_tokens` in incomplete details.
  Stream item completion waits until the next content block or message stop,
  when the final status is known; summary deltas can finish at content-block stop.
- Responses-to-Claude merges adjacent reasoning without moving it across tool
  calls and removes unsupported trailing thinking blocks. Default output limits
  are 32000 tokens (64000 for Fable), capped using the embedded catalog's
  `max_completion_tokens`; the local static loader now exposes that pure field.
- Claude-to-Codex and Claude-to-OpenAI retain tool-call/result adjacency across
  system reminders, including reversed parallel results. Claude-to-Codex downgrades
  incompatible strict JSON schemas instead of forcing optional fields to required.
- Gemini and Antigravity preserve mid-conversation reminders, merge eligible
  adjacent user turns, and retain model/signature boundaries. Tool-result images
  in Responses-to-Gemini are nested inside `functionResponse.parts`. Schema
  cleaning supplies missing array `items` for tool schemas without changing the
  Antigravity response-schema contract.
- Gemini Responses `[DONE]` finalization is upstream behavior as of
  `fork/v8.57.0`; the previously documented local divergence was adopted
  upstream.
- Gemini signature sanitization keeps upstream signature ownership and parallel
  function-call semantics without importing its runtime debug logger.
- Antigravity adapters keep only request-local conversion state. Runtime signature
  caches, dynamic model registries, and logging side effects remain outside the
  provider packages; OpenAI summary aliases are normalized locally as wire data.
  Claude-target finalization preserves compatible Claude thinking signatures and
  assigns Antigravity's validator-bypass signature to the first function call in
  each model turn; parallel sibling function calls remain unsigned. All supported
  ingress paths converge on this rule, preventing sequential tool history from
  being rejected before execution.
  OpenAI Responses reasoning effort does not inject `includeThoughts` in the
  pure converter; summary visibility remains an application runtime policy.
  The shared Antigravity wire finalizer also performs the excluded runtime
  `ApplyThinking` effort-alias normalization (`minimal` to `low`, `xhigh`/`max`
  to `high`) for every client protocol before the request is sent.
  Signature-carrier validation now considers neighboring semantic blocks and
  carrier direction. Provider-native signature output was adopted upstream;
  client signature recovery from a runtime cache remains excluded. The app's
  existing protocol-mismatch guard still rejects OpenAI-only `developer` roles
  on the Anthropic Messages endpoint; synchronization does not broaden ingress.
  The complete provider-test audit also restored two omitted sanitizer cases.
  Signature cleanup now reuses the already decoded `UseNumber` object before
  serialization, matching upstream's duplicate-key handling while preserving
  large numeric arguments and ccLoad's first-call bypass signature. The former
  raw-path deletion could leave a signature hidden in a duplicate parent key.
  At `fork/v8.75.0`, upstream's Claude response adapter also tries to cache
  trailing signature-only carriers with an empty thinking-text key. Its cache
  rejects empty text, making those calls no-ops; ccLoad has no runtime signature
  cache and keeps the existing pure wire carrier path instead.
- Antigravity-to-Claude reports cached prompt tokens the same way Gemini-to-Claude
  does: `usage.input_tokens` excludes `cachedContentTokenCount` and the cached
  amount is reported separately as `cache_read_input_tokens`. Upstream subtracts
  in the streaming path only, so its non-stream JSON double-counts cached input at
  the full input price. Both `totalTokenCount`-based output fallbacks also add the
  cached amount back before subtracting the prompt, since `totalTokenCount`
  includes cached tokens while the adjusted prompt count does not; upstream omits
  this and bills cache-hit input as output tokens.
- Antigravity stream payloads are framed at the app boundary because the upstream
  executor normally supplies SSE delimiters; ccLoad writes provider chunks directly.
  The same boundary supplies the Gemini converter's legacy `ctx["alt"]` mode value;
  without it the synchronized converter intentionally emits no stream chunks.
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
- ccLoad has no Kimi OAuth authenticator or executor. The upstream
  `models.json` may still include a top-level kimi catalog; ccLoad's
  `modelCatalog` does not deserialize that key, so Lookup never sees those
  models. Generic API-key channels remain model-agnostic and retain Kimi
  pricing and wire-format compatibility.
- `fork/v8.65.0` also rewrites Gemini function-call pairing validation to use
  short-circuiting `gjson.ForEach`; ccLoad carries that pure control-flow update
  while preserving its local `ccLoad/internal/protocol/cliproxy/util` import and
  established error strings.
- The target snapshot's Responses-to-Chat conversion still keeps ccLoad wire
  extensions for `reasoning.content`, cache-creation usage, `input_file`, and
  Responses `web_search` to `web_search_options`; these are intentional local
  contract differences and must survive future upstream syncs.
- Claude-target request converters derive a deterministic `metadata.user_id`
  from caller-supplied identity or stable request signals (prompt cache key,
  session/conversation ID, or the first user prompt). Explicit caller values
  remain unchanged; no process-global mutable identity or credential state is
  introduced.
- Responses tool namespace discovery and sanitization live in the pure
  `util/responses_tools.go` helper. Top-level declarations win over
  `additional_tools`, namespace children retain qualified names, and the
  helper carries no runtime registry or network dependency.
- The embedded capability catalog exposes Antigravity models to Gemini wire
  conversion. `gemini-3.7-flash-high` follows the canonical entry added by
  `router-for-me/models` commit `cbe1e6c59429bc92dd8d6654873670fc0c274cad`;
  that catalog provenance is independent of the CLIProxyAPI snapshot commit above.
- `util/gemini_schema.go` and its test carry four lint-forced equivalent
  rewrites required by ccLoad's zero-warning `golangci-lint` gate:
  `escapeGJSONPathKey` uses `strings.ContainsAny` instead of upstream's
  `strings.IndexAny(...) == -1` (staticcheck S1003), `mergeDescriptionRaw` uses a
  tagged `switch` (QF1002), and the test's two `json.Unmarshal` calls check their
  error (errcheck). Behavior is identical to upstream; each site is annotated
  in place.
  The synchronized cleaner also handles `contains` hints, preserves parent object
  properties while flattening `anyOf`/`oneOf`, prefers typed union branches over
  untyped/null shells, and removes orphan `required` arrays.
- `util/claude_attribution.go` and its test are now part of the snapshot. The
  previous `private-helper-test` exclusion no longer holds: the file is an
  exported pure string transform on Claude system prompts, and its test asserts
  that public contract. Its sole upstream caller stays in the excluded runtime
  executor layer, so the function has no ccLoad call site yet, exactly like
  `CleanJSONSchemaForAntigravityTool`.
- `util/header_helpers.go` and its test are excluded as runtime HTTP helpers.
  The target revision makes the file depend on `github.com/gin-gonic/gin`, which
  confirms it belongs to upstream's HTTP serving layer rather than the pure
  conversion core.
- The synchronized signature-carrier switch and two terminal-event test switches
  use equivalent tagged switches to satisfy ccLoad's staticcheck QF1003 gate.

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
7. Run the core scope verifier self-test, then run the atomic verifier with the
   previous synchronized commit as `--base-commit`.
8. Run `go test -tags sonic ./internal/protocol/cliproxy/...`,
   `go test -tags sonic ./internal/protocol`, and the repository verification
   commands from `CLAUDE.md`.

The upstream core/provider tests prove the snapshot was synchronized without
losing conversion behavior. Registry and provider boundary tests remain ccLoad's
compatibility authority. A future upstream sync is incomplete if either layer
fails, any of the 12 core request/non-stream/stream directions regresses, or an
allowlisted provider is omitted.
