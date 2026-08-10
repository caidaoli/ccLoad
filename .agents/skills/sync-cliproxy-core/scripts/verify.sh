#!/usr/bin/env bash
set -euo pipefail

run_tests=0
require_providers=0
upstream_repo=""

usage() {
  printf 'Usage: %s [--tests] [--require-providers] [--upstream-repo PATH]\n' "$0"
  printf 'Audits the atomic core + provider-adapter snapshot.\n'
}

while (($# > 0)); do
  case "$1" in
    --tests)
      run_tests=1
      shift
      ;;
    --require-providers)
      require_providers=1
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

if ((require_providers == 1)) && [[ -z "$upstream_repo" ]]; then
  printf 'FAIL: --require-providers requires --upstream-repo for source and test provenance checks\n' >&2
  exit 2
fi

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$repo_root" ]]; then
  printf 'FAIL: not inside a Git repository\n' >&2
  exit 1
fi
cd "$repo_root"
go_module_root="$(dirname "$(go env GOMOD)")"

failures=0
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

audit_provider_imports() {
  local provider="$1"
  local scope="$2"
  local imports="$3"
  local import_path first_segment

  while IFS= read -r import_path; do
    [[ -n "$import_path" ]] || continue
    case "$import_path" in
      github.com/tidwall/gjson|github.com/tidwall/sjson)
        continue
        ;;
      google.golang.org/protobuf/encoding/protowire)
        if [[ "$scope" == "test" ]]; then
          continue
        fi
        fail "provider production code imports a test-only package: $provider ($import_path)"
        continue
        ;;
      bytes|cmp|context|encoding|encoding/*|errors|fmt|hash|hash/*|io|maps|math|math/*|path|path/*|reflect|regexp|sort|slices|strconv|strings|sync|sync/*|time|unicode|unicode/*)
        continue
        ;;
      runtime|testing)
        if [[ "$scope" == "test" ]]; then
          continue
        fi
        fail "provider production code imports a test-only standard package: $provider ($import_path)"
        continue
        ;;
    esac

    if [[ "$import_path" == "ccLoad/internal/protocol/cliproxy/providers/$provider" || "$import_path" == "ccLoad/internal/protocol/cliproxy/providers/$provider/"* ]]; then
      continue
    fi
    if [[ "$import_path" =~ ^ccLoad/internal/protocol/cliproxy/(claude|codex|common|gemini|misc|openai|registry|signature|thinking|util)(/|$) ]]; then
      continue
    fi
    if [[ "$import_path" == ccLoad/* ]]; then
      fail "provider adapter imports a ccLoad package outside its positive allowlist: $provider ($import_path)"
      continue
    fi

    first_segment="${import_path%%/*}"
    if [[ "$first_segment" == *.* ]]; then
      fail "provider adapter imports an external package outside its positive allowlist: $provider ($import_path)"
    else
      fail "provider adapter imports a standard package outside its positive allowlist: $provider ($import_path)"
    fi
  done <<< "$imports"
}

require_file() {
  if [[ ! -f "$1" ]]; then
    fail "missing required file: $1"
  fi
}

is_clean_relative_path() {
  case "$1" in
    ""|/*|.|..|./*|../*|*/./*|*/../*|*/.|*/..|*//*)
      return 1
      ;;
    *)
      return 0
      ;;
  esac
}

snapshot="internal/protocol/cliproxy"
providers_snapshot="$snapshot/providers"
upstream_doc="$snapshot/UPSTREAM.md"
register_file="internal/protocol/builtin/register.go"
adapter_file="internal/protocol/builtin/cliproxy_adapter.go"
registry_file="internal/protocol/registry.go"
canonical_skill=".agents/skills/sync-cliproxy-core"
provider_reference="$canonical_skill/references/provider-adapters.md"
provider_manifest="$canonical_skill/references/provider-adapters.manifest"
provider_test_lister="$canonical_skill/scripts/list_go_tests.go"
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
require_file "$provider_reference"
require_file "$provider_manifest"
require_file "$canonical_skill/scripts/verify.sh"
require_file "$provider_test_lister"

providers_snapshot_is_symlink=0
if [[ -L "$providers_snapshot" ]]; then
  fail "provider snapshot root must not be a symlink: $providers_snapshot"
  providers_snapshot_is_symlink=1
fi

if [[ -d "$snapshot" ]]; then
  for entry in "$snapshot"/*; do
    base="$(basename "$entry")"
    case "$base" in
      LICENSE|UPSTREAM.md|claude|codex|common|gemini|misc|openai|providers|registry|signature|thinking|util)
        ;;
      *)
        fail "unexpected top-level snapshot entry: $entry"
        ;;
    esac
  done
fi

manifest_providers="$(awk -F '|' '$1 == "provider" { print $2 }' "$provider_manifest" | sort -u)"
if [[ -z "$manifest_providers" ]]; then
  fail "provider manifest contains no providers"
fi

duplicate_manifest_keys="$(awk -F '|' '
  $1 == "provider" { key = $1 FS $2 }
  $1 == "file" { key = $1 FS $2 FS $5 }
  $1 == "exclude" { key = $1 FS $2 FS $3 }
  $1 == "wiring" { key = $1 FS $2 FS $3 FS $4 }
  $1 == "contract" { key = $1 FS $2 FS $3 FS $4 }
  key != "" { count[key]++; key = "" }
  END { for (key in count) if (count[key] > 1) print key }
' "$provider_manifest")"
if [[ -n "$duplicate_manifest_keys" ]]; then
  printf '%s\n' "$duplicate_manifest_keys" >&2
  fail "provider manifest contains duplicate rows"
fi

while IFS='|' read -r kind provider field1 field2 extra; do
  [[ -z "$kind" || "$kind" == \#* ]] && continue
  case "$kind" in
    provider)
      [[ "$provider" =~ ^[a-z0-9][a-z0-9_-]*$ && -n "$field1" && -n "$field2" && -z "$extra" ]] || fail "invalid provider manifest row: $kind|$provider|$field1|$field2|$extra"
      ;;
    file)
      [[ -n "$provider" && ("$field1" == "source" || "$field1" == "test") && -n "$field2" && -n "$extra" ]] || fail "invalid file manifest row for provider: $provider"
      ;;
    exclude)
      [[ -n "$provider" && -n "$field1" && -z "$field2" && -z "$extra" ]] || fail "invalid exclude manifest row for provider: $provider"
      ;;
    wiring)
      [[ -n "$provider" && -n "$field1" && -n "$field2" && -z "$extra" ]] || fail "invalid $kind manifest row for provider: $provider"
      ;;
    contract)
      [[ -n "$provider" && -n "$field1" && "$field2" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && -z "$extra" ]] || fail "invalid contract manifest row for provider: $provider"
      ;;
    *)
      fail "unknown provider manifest row kind: $kind"
      ;;
  esac
  if [[ "$kind" != "provider" ]] && ! printf '%s\n' "$manifest_providers" | grep -Fxq -- "$provider"; then
    fail "provider manifest row references an undeclared provider: $provider"
  fi
done < "$provider_manifest"

provider_count=0
while IFS= read -r provider; do
  [[ -n "$provider" ]] || continue
  provider_root="$(awk -F '|' -v provider="$provider" '$1 == "provider" && $2 == provider { print $4 }' "$provider_manifest")"
  provider_upstream_root="$(awk -F '|' -v provider="$provider" '$1 == "provider" && $2 == provider { print $3 }' "$provider_manifest")"
  provider_rows="$(awk -F '|' -v provider="$provider" '$1 == "provider" && $2 == provider { count++ } END { print count + 0 }' "$provider_manifest")"
  if [[ "$provider_rows" != "1" || -z "$provider_root" || -z "$provider_upstream_root" ]]; then
    fail "provider manifest must define exactly one root for: $provider"
    continue
  fi
  if [[ "$provider_upstream_root" != "internal/translator/$provider" ]] || ! is_clean_relative_path "$provider_upstream_root"; then
    fail "provider upstream root must match its canonical translator path: $provider_upstream_root"
    continue
  fi
  if [[ "$provider_root" != "$providers_snapshot/$provider" ]]; then
    fail "provider root must match its canonical in-repository path: $provider_root"
    continue
  fi

  while IFS='|' read -r _ _ exclude_pattern _ _; do
    if ! is_clean_relative_path "$exclude_pattern"; then
      fail "provider exclusion is not a clean relative path: $exclude_pattern"
      continue
    fi
    case "$exclude_pattern" in
      "$provider_upstream_root"/*)
        ;;
      *)
        fail "provider exclusion is outside its declared upstream root: $exclude_pattern"
        ;;
    esac
  done < <(awk -F '|' -v provider="$provider" '$1 == "exclude" && $2 == provider { print }' "$provider_manifest")

  if ((providers_snapshot_is_symlink == 1)); then
    continue
  fi
  if [[ -L "$provider_root" ]]; then
    fail "provider root must not be a symlink: $provider_root"
    continue
  fi
  if [[ ! -d "$provider_root" ]]; then
    if ((require_providers == 1)); then
      fail "required provider adapter is not synchronized: $provider"
    fi
    continue
  fi

  provider_symlinks="$(find "$provider_root" -type l -print)"
  if [[ -n "$provider_symlinks" ]]; then
    printf '%s\n' "$provider_symlinks" >&2
    fail "provider adapter tree must not contain symlinks: $provider"
    continue
  fi

  provider_count=$((provider_count + 1))
  provider_source_count=0
  provider_test_count=0
  while IFS='|' read -r _ _ role upstream_file local_file; do
    if ! is_clean_relative_path "$upstream_file"; then
      fail "provider source is not a clean relative path: $upstream_file"
    fi
    if ! is_clean_relative_path "$local_file"; then
      fail "provider file is not a clean relative path: $local_file"
    fi
    case "$upstream_file" in
      "$provider_upstream_root"/*)
        ;;
      *)
        fail "provider source is outside its declared upstream root: $upstream_file"
        ;;
    esac
    case "$local_file" in
      "$provider_root"/*)
        ;;
      *)
        fail "provider file is outside its declared root: $local_file"
        ;;
    esac
    require_file "$local_file"
    if [[ "$role" == "source" ]]; then
      if [[ "$upstream_file" == *_test.go || "$local_file" == *_test.go ]]; then
        fail "provider production mapping points at a test file: $upstream_file -> $local_file"
      fi
      provider_source_count=$((provider_source_count + 1))
    else
      if [[ "$upstream_file" != *_test.go || "$local_file" != *_test.go ]]; then
        fail "provider test mapping must use _test.go on both sides: $upstream_file -> $local_file"
      fi
      provider_test_count=$((provider_test_count + 1))
    fi
  done < <(awk -F '|' -v provider="$provider" '$1 == "file" && $2 == provider { print }' "$provider_manifest")
  ((provider_source_count > 0)) || fail "provider manifest has no production files: $provider"
  ((provider_test_count > 0)) || fail "provider manifest has no tests: $provider"

  wiring_count="$(awk -F '|' -v provider="$provider" '$1 == "wiring" && $2 == provider { count++ } END { print count + 0 }' "$provider_manifest")"
  contract_count="$(awk -F '|' -v provider="$provider" '$1 == "contract" && $2 == provider { count++ } END { print count + 0 }' "$provider_manifest")"
  ((wiring_count > 0)) || fail "provider manifest has no production wiring contract: $provider"
  ((contract_count > 0)) || fail "provider manifest has no public behavior tests: $provider"

  while IFS= read -r local_file; do
    if ! awk -F '|' -v local_file="$local_file" '$1 == "file" && $5 == local_file { found = 1 } END { exit !found }' "$provider_manifest"; then
      fail "provider Go file is absent from manifest: $local_file"
    fi
  done < <(find "$provider_root" -type f -name '*.go' | sort)

  provider_package_pattern="./$provider_root/..."
  if production_imports="$(go list -tags sonic -f '{{range .Imports}}{{println .}}{{end}}' "$provider_package_pattern")"; then
    audit_provider_imports "$provider" "production" "$production_imports"
  else
    fail "cannot load provider production packages: $provider"
  fi
  if test_imports="$(go list -tags sonic -f '{{range .TestImports}}{{println .}}{{end}}{{range .XTestImports}}{{println .}}{{end}}' "$provider_package_pattern")"; then
    audit_provider_imports "$provider" "test" "$test_imports"
  else
    fail "cannot load provider test packages: $provider"
  fi
  # These dollar variables belong to Go templates, not the shell.
  # shellcheck disable=SC2016
  provider_build_files_template='{{- $dir := .Dir -}}{{range .GoFiles}}{{printf "%s/%s\n" $dir .}}{{end}}'
  # shellcheck disable=SC2016
  provider_test_build_files_template='{{- $dir := .Dir -}}{{range .TestGoFiles}}{{printf "%s/%s\n" $dir .}}{{end}}{{range .XTestGoFiles}}{{printf "%s/%s\n" $dir .}}{{end}}'
  if provider_build_files="$(go list -tags sonic -f "$provider_build_files_template" "$provider_package_pattern")" &&
     provider_test_build_files="$(go list -tags sonic -f "$provider_test_build_files_template" "$provider_package_pattern")"; then
    while IFS='|' read -r _ _ role _ local_file; do
      expected_build_file="$go_module_root/$local_file"
      if [[ "$role" == "source" ]]; then
        participating_files="$provider_build_files"
      else
        participating_files="$provider_test_build_files"
      fi
      if ! printf '%s\n' "$participating_files" | grep -Fxq -- "$expected_build_file"; then
        fail "mapped provider file is ignored by the active Go build: $local_file"
      fi
    done < <(awk -F '|' -v provider="$provider" '$1 == "file" && $2 == provider { print }' "$provider_manifest")
  else
    fail "cannot enumerate active provider build files: $provider"
  fi

  while IFS='|' read -r _ _ import_path search_root; do
    if [[ "$import_path" != "ccLoad/$provider_root" && "$import_path" != "ccLoad/$provider_root/"* ]]; then
      fail "provider wiring import is outside its declared root: $provider ($import_path)"
    fi
    case "$search_root" in
      internal/*)
        ;;
      *)
        fail "provider wiring search root must stay under internal/: $search_root"
        ;;
    esac
    if ! is_clean_relative_path "$search_root"; then
      fail "provider wiring search root is not a clean relative path: $search_root"
      continue
    fi
    if [[ ! -d "$search_root" ]]; then
      fail "provider wiring search root does not exist: $search_root"
      continue
    fi
    if ! production_dependencies="$(go list -tags sonic -deps -f '{{.ImportPath}}' "./$search_root/...")"; then
      fail "cannot load production wiring graph: $search_root"
      continue
    fi
    if ! printf '%s\n' "$production_dependencies" | grep -Fxq -- "$import_path"; then
      fail "provider adapter is not connected to production path: $provider ($import_path)"
    fi
  done < <(awk -F '|' -v provider="$provider" '$1 == "wiring" && $2 == provider { print }' "$provider_manifest")

  while IFS='|' read -r _ _ test_file test_symbol; do
    if ! is_clean_relative_path "$test_file"; then
      fail "provider contract test path is not a clean relative path: $test_file"
      continue
    fi
    require_file "$test_file"
    [[ -f "$test_file" ]] || continue
    test_package="./$(dirname "$test_file")"
    if ! listed_tests="$(go test -tags sonic "$test_package" -list "^${test_symbol}$")"; then
      fail "cannot discover provider contract tests: $test_package"
      continue
    fi
    if ! printf '%s\n' "$listed_tests" | grep -Fxq -- "$test_symbol"; then
      fail "missing provider contract test: $test_symbol in $test_file"
    fi
  done < <(awk -F '|' -v provider="$provider" '$1 == "contract" && $2 == provider { print }' "$provider_manifest")

  if ! grep -Fq -- "- Local destination: \`$provider_root\`" "$upstream_doc"; then
    fail "UPSTREAM.md does not record provider destination: $provider_root"
  fi
done <<< "$manifest_providers"

if [[ -d "$providers_snapshot" && ! -L "$providers_snapshot" ]]; then
  for provider_dir in "$providers_snapshot"/*; do
    [[ -e "$provider_dir" ]] || continue
    provider="$(basename "$provider_dir")"
    if ! printf '%s\n' "$manifest_providers" | grep -Fxq -- "$provider"; then
      fail "provider adapter is absent from manifest: $provider_dir"
    fi
  done

fi

manifest_provider_count="$(printf '%s\n' "$manifest_providers" | grep -c . || true)"
pending_provider_count="$(grep -Ec '^- Snapshot status: pending initial import' "$upstream_doc" || true)"
synchronized_provider_count="$(grep -Ec '^- Snapshot status: synchronized at shared commit$' "$upstream_doc" || true)"
if ((provider_count == 0)); then
  [[ "$pending_provider_count" == "$manifest_provider_count" ]] || fail "UPSTREAM.md must record one pending status per provider while no provider snapshot exists"
  [[ "$synchronized_provider_count" == "0" ]] || fail "UPSTREAM.md claims a provider is synchronized while no provider snapshot exists"
else
  [[ "$pending_provider_count" == "0" ]] || fail "UPSTREAM.md still claims a provider import is pending"
  [[ "$synchronized_provider_count" == "$provider_count" ]] || fail "UPSTREAM.md must record one synchronized status per imported provider"
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
  else
    while IFS= read -r provider; do
      [[ -n "$provider" ]] || continue
      provider_root="$(awk -F '|' -v provider="$provider" '$1 == "provider" && $2 == provider { print $4 }' "$provider_manifest")"
      [[ -d "$provider_root" ]] || continue
      while IFS='|' read -r _ _ _ upstream_file _; do
        if ! git -C "$upstream_repo" cat-file -e "${synchronized_commit}:${upstream_file}" 2>/dev/null; then
          fail "recorded commit lacks mapped provider source: $provider ($upstream_file)"
        fi
      done < <(awk -F '|' -v provider="$provider" '$1 == "file" && $2 == provider { print }' "$provider_manifest")

      while IFS='|' read -r _ _ _ upstream_test_file local_test_file; do
        if ! upstream_test_symbols="$(git -C "$upstream_repo" show "${synchronized_commit}:${upstream_test_file}" | go run "$provider_test_lister" -stdin-name "$upstream_test_file")"; then
          fail "cannot parse mapped upstream provider test: $provider ($upstream_test_file)"
          continue
        fi
        if ! local_test_symbols="$(go run "$provider_test_lister" -file "$local_test_file")"; then
          fail "cannot parse mapped local provider test: $provider ($local_test_file)"
          continue
        fi
        while IFS= read -r test_symbol; do
          [[ -n "$test_symbol" ]] || continue
          if ! printf '%s\n' "$local_test_symbols" | grep -Fxq -- "$test_symbol"; then
            fail "mapped provider test lost an upstream test symbol: $provider ($upstream_test_file: $test_symbol)"
          fi
        done <<< "$upstream_test_symbols"
      done < <(awk -F '|' -v provider="$provider" '$1 == "file" && $2 == provider && $3 == "test" { print }' "$provider_manifest")

      while IFS= read -r upstream_file; do
        if awk -F '|' -v provider="$provider" -v upstream_file="$upstream_file" '$1 == "file" && $2 == provider && $4 == upstream_file { found = 1 } END { exit !found }' "$provider_manifest"; then
          continue
        fi
        excluded=0
        while IFS='|' read -r _ _ exclude_pattern _ _; do
          # The manifest field is deliberately a glob, not a literal path.
          # shellcheck disable=SC2053
          if [[ "$upstream_file" == $exclude_pattern ]]; then
            excluded=1
            break
          fi
        done < <(awk -F '|' -v provider="$provider" '$1 == "exclude" && $2 == provider { print }' "$provider_manifest")
        if ((excluded == 0)); then
          fail "recorded commit contains an unmapped provider Go file: $provider ($upstream_file)"
        fi
      done < <(git -C "$upstream_repo" ls-tree -r --name-only "$synchronized_commit" -- "$provider_upstream_root" | grep -E '\.go$' || true)
    done <<< "$manifest_providers"
  fi
fi

if ! git diff --check; then
  fail "git diff --check reported whitespace errors"
fi

if ((failures > 0)); then
  printf 'Snapshot audit failed with %d error(s).\n' "$failures" >&2
  exit 1
fi

printf 'Snapshot audit passed: commit=%s tests=%s providers=%s\n' "$synchronized_commit" "$test_count" "$provider_count"

if ((run_tests == 1)); then
  go test -tags sonic ./internal/protocol/cliproxy/...
  go test -tags sonic ./internal/protocol
  while IFS= read -r provider; do
    [[ -n "$provider" ]] || continue
    provider_root="$(awk -F '|' -v provider="$provider" '$1 == "provider" && $2 == provider { print $4 }' "$provider_manifest")"
    [[ -d "$provider_root" && ! -L "$provider_root" ]] || continue
    while IFS='|' read -r _ _ test_file test_symbol; do
      go test -tags sonic "./$(dirname "$test_file")" -run "^${test_symbol}$" -count=1
    done < <(awk -F '|' -v provider="$provider" '$1 == "contract" && $2 == provider { print }' "$provider_manifest")
  done <<< "$manifest_providers"
fi
