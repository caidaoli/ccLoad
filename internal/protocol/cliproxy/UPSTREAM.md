# CLIProxyAPI translator provenance

- Repository: `https://github.com/caidaoli/CLIProxyAPI`
- Module source path: `github.com/router-for-me/CLIProxyAPI/v7`
- Last synchronized commit: `d0326e0038a02254f82d18ddbc48addbb2e6c9af` (`fork/v8.57.0`)
- Synchronized at: `2026-08-01`

This directory contains the four-protocol conversion core only. Authentication,
configuration, routing, caches, plugins, dynamic registries, network refreshers,
Antigravity, and Interactions are intentionally excluded. ccLoad-specific wire
adaptation lives in `internal/protocol/builtin`, not in this directory.

## Synchronized tests

The snapshot includes 43 upstream `_test.go` files from the same commit as the
production sources:

- `claude/gemini`: 2
- `claude/openai/chat-completions`: 2
- `claude/openai/responses`: 2
- `codex/claude`: 3
- `codex/gemini`: 2
- `codex/openai/chat-completions`: 2
- `codex/openai/responses`: 2
- `common`: 3
- `gemini/claude`: 2
- `gemini/openai/chat-completions`: 4
- `gemini/openai/responses`: 3
- `openai/claude`: 2
- `openai/gemini`: 2
- `openai/openai/responses`: 2
- `signature`: 6
- `util`: 4

Tests for excluded packages are not copied. Performance-only benchmarks are
also excluded: the translator-wide benchmark requires the excluded dynamic
Registry, Antigravity, and Interactions paths, while the Claude-to-Codex
benchmark measures allocation details rather than a wire contract. Upstream
`noop_optimization_test.go` files and allocation-reuse assertions are likewise
excluded because they test private implementation and memory reuse instead of
the public conversion contract. The `thinking` package keeps only the pure
conversion sources (`convert.go`, `suffix.go`, `text.go`, `types.go`); upstream's
runtime thinking application (`apply.go`, `strip.go`, `summary.go`,
`validate.go`, `errors.go`, `provider/`) and its tests stay excluded, as does
the upstream SDK translator Registry and its summary test.

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
  Gemini camelCase fields, Codex top-level `instructions`, terminal `[DONE]`,
  top-level cache-creation usage, and unsigned Anthropic thinking preserved as
  OpenAI reasoning.
- Codex-to-OpenAI keeps the top-level `usage.cache_creation_input_tokens`
  field alongside upstream's `prompt_tokens_details.cached_creation_tokens`.
- Codex-to-Gemini requests keep the caller's `stream` flag and do not force
  `reasoning.summary`.
- Gemini Responses `[DONE]` finalization is upstream behavior as of
  `fork/v8.57.0`; the previously documented local divergence was adopted
  upstream.
- Gemini signature sanitization keeps upstream signature ownership and parallel
  function-call semantics without importing its runtime debug logger.

## Updating from CLIProxyAPI

Run this procedure through the repository skill: use `$sync-cliproxy-core` in
Codex or `/sync-cliproxy-core` in Claude Code. Both entry points resolve to the
canonical skill under `.agents/skills/sync-cliproxy-core`.

1. Fetch the ccLoad CLIProxyAPI fork and choose one immutable commit or tag.
2. Diff both production sources and the synchronized test files listed above
   against the commit above. Source and tests must always come from the same commit.
3. Copy the changed pure conversion files and matching tests only; do not add a
   Go module import, `replace`, authentication, configuration, routing, caches,
   plugins, SDK registries, or network update code.
4. Keep Antigravity, Interactions, and tests for uncopied packages excluded.
5. Resolve the diff against the documented local wire contract instead of
   overwriting it, then update the commit and date above.
6. Run `go test -tags sonic ./internal/protocol/cliproxy/...`,
   `go test -tags sonic ./internal/protocol`, and the repository verification
   commands from `CLAUDE.md`.

The upstream core tests prove the snapshot was synchronized without losing its
conversion behavior. The Registry boundary tests remain ccLoad's compatibility
authority. A future upstream sync is incomplete if either layer fails or any of
the 12 request, non-stream response, or stream response directions regress.
