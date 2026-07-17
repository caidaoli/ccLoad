#!/usr/bin/env bash
set -euo pipefail

run_tests=0
upstream_repo=""

usage() {
  printf 'Usage: %s [--tests] [--upstream-repo PATH]\n' "$0"
}

while (($# > 0)); do
  case "$1" in
    --tests)
      run_tests=1
      shift
      ;;
    --upstream-repo)
      if (($# < 2)); then
        usage >&2
        exit 2
      fi
      upstream_repo="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$repo_root" ]]; then
  printf 'FAIL: not inside a Git repository\n' >&2
  exit 1
fi
cd "$repo_root"

failures=0
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

require_file() {
  if [[ ! -f "$1" ]]; then
    fail "missing required file: $1"
  fi
}

snapshot="internal/protocol/cliproxy"
upstream_doc="$snapshot/UPSTREAM.md"
register_file="internal/protocol/builtin/register.go"
adapter_file="internal/protocol/builtin/cliproxy_adapter.go"
registry_file="internal/protocol/registry.go"
canonical_skill=".agents/skills/sync-cliproxy-core"
claude_skill=".claude/skills/sync-cliproxy-core"

require_file "go.mod"
require_file "CLAUDE.md"
require_file "$upstream_doc"
require_file "$snapshot/LICENSE"
require_file "$register_file"
require_file "$adapter_file"
require_file "$registry_file"
require_file "$canonical_skill/SKILL.md"
require_file "$canonical_skill/agents/openai.yaml"
require_file "$canonical_skill/scripts/verify.sh"

if [[ -d "$snapshot" ]]; then
  for entry in "$snapshot"/*; do
    base="$(basename "$entry")"
    case "$base" in
      LICENSE|UPSTREAM.md|claude|codex|common|gemini|misc|openai|registry|signature|thinking|util)
        ;;
      *)
        fail "unexpected top-level snapshot entry: $entry"
        ;;
    esac
  done
fi

if [[ -f "$upstream_doc" ]]; then
  if ! grep -Fq -- "- Repository: \`https://github.com/caidaoli/CLIProxyAPI\`" "$upstream_doc"; then
    fail "UPSTREAM.md repository is missing or unexpected"
  fi
  if ! grep -Fq -- "- Module source path: \`github.com/router-for-me/CLIProxyAPI/v7\`" "$upstream_doc"; then
    fail "UPSTREAM.md module source path is missing or unexpected"
  fi

  commit_count="$(grep -Ec "^- Last synchronized commit: \`[0-9a-f]{40}\`" "$upstream_doc" || true)"
  commit_line="$(grep -E "^- Last synchronized commit: \`[0-9a-f]{40}\`" "$upstream_doc" || true)"
  if [[ "$commit_count" != "1" ]]; then
    fail "UPSTREAM.md must record one full 40-character commit SHA"
    synchronized_commit=""
  else
    synchronized_commit="$(printf '%s\n' "$commit_line" | sed -E "s/.*\`([0-9a-f]{40})\`.*/\\1/")"
  fi

  date_count="$(grep -Ec "^- Synchronized at: \`[0-9]{4}-[0-9]{2}-[0-9]{2}\`$" "$upstream_doc" || true)"
  if [[ "$date_count" != "1" ]]; then
    fail "UPSTREAM.md must record one synchronization date as YYYY-MM-DD"
  fi
else
  synchronized_commit=""
fi

runtime_imports="$(grep -R -n --include='*.go' -E 'github.com/(router-for-me|caidaoli)/CLIProxyAPI' "$snapshot" "$adapter_file" 2>/dev/null || true)"
if [[ -n "$runtime_imports" ]]; then
  printf '%s\n' "$runtime_imports" >&2
  fail "snapshot imports CLIProxyAPI as a runtime module"
fi

if grep -Eq 'github.com/(router-for-me|caidaoli)/CLIProxyAPI' go.mod; then
  fail "go.mod must not depend on CLIProxyAPI"
fi

if [[ -f "$register_file" ]]; then
  request_count="$(grep -c 'reg\.RegisterRequest' "$register_file" || true)"
  stream_count="$(grep -c 'reg\.RegisterStreamResponse' "$register_file" || true)"
  non_stream_count="$(grep -c 'reg\.RegisterNonStreamResponse' "$register_file" || true)"
  [[ "$request_count" == "12" ]] || fail "expected 12 request registrations, found $request_count"
  [[ "$stream_count" == "12" ]] || fail "expected 12 stream response registrations, found $stream_count"
  [[ "$non_stream_count" == "12" ]] || fail "expected 12 non-stream response registrations, found $non_stream_count"
fi

test_count="$(find "$snapshot" -type f -name '*_test.go' 2>/dev/null | wc -l | tr -d '[:space:]')"
if [[ -z "$test_count" || "$test_count" == "0" ]]; then
  fail "snapshot contains no synchronized tests"
fi

if [[ ! -L "$claude_skill" ]]; then
  fail "$claude_skill must be a symlink to the canonical skill"
else
  canonical_path="$(cd "$canonical_skill" && pwd -P)"
  link_target="$(readlink "$claude_skill")"
  if [[ "$link_target" = /* ]]; then
    resolved_path="$(cd "$link_target" 2>/dev/null && pwd -P || true)"
  else
    resolved_path="$(cd "$(dirname "$claude_skill")/$link_target" 2>/dev/null && pwd -P || true)"
  fi
  if [[ -z "$resolved_path" || "$resolved_path" != "$canonical_path" ]]; then
    fail "$claude_skill does not resolve to $canonical_skill"
  fi
fi

if [[ -n "$upstream_repo" ]]; then
  if ! git -C "$upstream_repo" rev-parse --git-dir >/dev/null 2>&1; then
    fail "--upstream-repo is not a Git checkout: $upstream_repo"
  elif [[ -z "$synchronized_commit" ]]; then
    fail "cannot verify upstream checkout without a recorded commit"
  elif ! git -C "$upstream_repo" cat-file -e "${synchronized_commit}^{commit}" 2>/dev/null; then
    fail "recorded commit $synchronized_commit is absent from $upstream_repo"
  fi
fi

if ! git diff --check; then
  fail "git diff --check reported whitespace errors"
fi

if ((failures > 0)); then
  printf 'Snapshot audit failed with %d error(s).\n' "$failures" >&2
  exit 1
fi

printf 'Snapshot audit passed: commit=%s tests=%s\n' "$synchronized_commit" "$test_count"

if ((run_tests == 1)); then
  go test -tags sonic ./internal/protocol/cliproxy/...
  go test -tags sonic ./internal/protocol
fi
