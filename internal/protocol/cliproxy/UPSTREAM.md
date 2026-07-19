# CLIProxyAPI translator provenance

- Repository: `https://github.com/caidaoli/CLIProxyAPI`
- Module source path: `github.com/router-for-me/CLIProxyAPI/v7`
- Last synchronized commit: `4edfb63e62537362a44ac4ae0bc695d4e344d446` (`fork/v8.34.0`)
- Synchronized at: `2026-07-19`

This directory contains the four-protocol conversion core only. Authentication,
configuration, routing, caches, plugins, dynamic registries, network refreshers,
Antigravity, and Interactions are intentionally excluded. ccLoad-specific wire
adaptation lives in `internal/protocol/builtin`, not in this directory.

## Synchronized tests

The snapshot includes 29 upstream `_test.go` files from the same commit as the
production sources:

- `claude/gemini`: 2
- `claude/openai/chat-completions`: 2
- `claude/openai/responses`: 2
- `codex/claude`: 2
- `codex/gemini`: 2
- `codex/openai/chat-completions`: 2
- `codex/openai/responses`: 2
- `common`: 2
- `gemini/claude`: 2
- `gemini/openai/chat-completions`: 3
- `gemini/openai/responses`: 2
- `openai/claude`: 2
- `openai/gemini`: 2
- `openai/openai/responses`: 2

Tests for excluded packages are not copied. Performance-only benchmarks are
also excluded: the translator-wide benchmark requires the excluded dynamic
Registry, Antigravity, and Interactions paths, while the Claude-to-Codex
benchmark measures allocation details rather than a wire contract.

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

## Updating from CLIProxyAPI

Run this procedure through the repository skill: use `$sync-cliproxy-core` in
Codex or `/sync-cliproxy-core` in Claude Code. Both entry points resolve to the
canonical skill under `.agents/skills/sync-cliproxy-core`.

1. Fetch the ccLoad CLIProxyAPI fork and choose one immutable commit or tag.
2. Diff both production sources and the corresponding 29 test files against
   the commit above. Source and tests must always come from the same commit.
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
