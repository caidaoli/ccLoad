#!/usr/bin/env bash
set -euo pipefail

upstream_repo=""
target_commit=""
base_commit=""
core_manifest=""
provider_manifest=""
run_self_test=0

usage() {
  printf 'Usage: %s --upstream-repo PATH --target-commit SHA --core-manifest PATH --provider-manifest PATH [--base-commit SHA]\n' "$0"
  printf '       %s --self-test\n' "$0"
}

while (($# > 0)); do
  case "$1" in
    --upstream-repo)
      (($# >= 2)) || { usage >&2; exit 2; }
      upstream_repo="$2"
      shift 2
      ;;
    --target-commit)
      (($# >= 2)) || { usage >&2; exit 2; }
      target_commit="$2"
      shift 2
      ;;
    --base-commit)
      (($# >= 2)) || { usage >&2; exit 2; }
      base_commit="$2"
      shift 2
      ;;
    --core-manifest)
      (($# >= 2)) || { usage >&2; exit 2; }
      core_manifest="$2"
      shift 2
      ;;
    --provider-manifest)
      (($# >= 2)) || { usage >&2; exit 2; }
      provider_manifest="$2"
      shift 2
      ;;
    --self-test)
      run_self_test=1
      shift
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

script_path="$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")"
test_lister="$(dirname "$script_path")/list_go_tests.go"

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

is_hash() {
  [[ "$1" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]]
}

die() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

manifest_row_count() {
  local kind="$1"
  awk -F '|' -v kind="$kind" '$1 == kind { count++ } END { print count + 0 }' "$core_manifest"
}

provider_classification() {
  local upstream_file="$1"
  local exclude_pattern

  if awk -F '|' -v upstream_file="$upstream_file" '$1 == "file" && $4 == upstream_file { found = 1 } END { exit !found }' "$provider_manifest"; then
    printf 'provider\n'
    return 0
  fi
  while IFS='|' read -r _ _ exclude_pattern _ _; do
    # Manifest exclusions are deliberate globs.
    # shellcheck disable=SC2053
    if [[ "$upstream_file" == $exclude_pattern ]]; then
      printf 'provider-excluded\n'
      return 0
    fi
  done < <(awk -F '|' '$1 == "exclude" { print }' "$provider_manifest")
  return 1
}

special_local_file() {
  local upstream_file="$1"
  awk -F '|' -v upstream_file="$upstream_file" '$1 == "file" && $3 == upstream_file { print $4; exit }' "$core_manifest"
}

provider_local_file() {
  local upstream_file="$1"
  awk -F '|' -v upstream_file="$upstream_file" '$1 == "file" && $4 == upstream_file { print $5; exit }' "$provider_manifest"
}

direct_local_file() {
  local upstream_file="$1"
  local upstream_root local_root relative local_file

  while IFS='|' read -r _ upstream_root local_root; do
    case "$upstream_file" in
      "$upstream_root"/*)
        relative="${upstream_file#"$upstream_root"/}"
        local_file="$local_root/$relative"
        if [[ -f "$local_file" ]]; then
          printf '%s\n' "$local_file"
          return 0
        fi
        ;;
    esac
  done < <(awk -F '|' '$1 == "root" { print }' "$core_manifest")
  return 1
}

core_exclusion() {
  local upstream_file="$1"
  local exclude_pattern

  while IFS='|' read -r _ exclude_pattern _; do
    # Manifest exclusions are deliberate globs.
    # shellcheck disable=SC2053
    if [[ "$upstream_file" == $exclude_pattern ]]; then
      return 0
    fi
  done < <(awk -F '|' '$1 == "exclude" { print }' "$core_manifest")
  return 1
}

classify_upstream_file() {
  local upstream_file="$1"
  local classification local_file

  if awk -F '|' -v upstream_file="$upstream_file" '$1 == "delete" && $3 == upstream_file { found = 1 } END { exit !found }' "$core_manifest"; then
    printf 'core-deleted\n'
    return 0
  fi
  if classification="$(provider_classification "$upstream_file")"; then
    printf '%s\n' "$classification"
    return 0
  fi
  local_file="$(special_local_file "$upstream_file")"
  if [[ -n "$local_file" ]]; then
    printf 'core\n'
    return 0
  fi
  if local_file="$(direct_local_file "$upstream_file")"; then
    printf 'core\n'
    return 0
  fi
  if core_exclusion "$upstream_file"; then
    printf 'core-excluded\n'
    return 0
  fi
  return 1
}

local_file_for_upstream() {
  local upstream_file="$1"
  local local_file

  local_file="$(provider_local_file "$upstream_file")"
  if [[ -n "$local_file" ]]; then
    printf '%s\n' "$local_file"
    return 0
  fi
  local_file="$(special_local_file "$upstream_file")"
  if [[ -n "$local_file" ]]; then
    printf '%s\n' "$local_file"
    return 0
  fi
  direct_local_file "$upstream_file"
}

validate_role() {
  local role="$1"
  local upstream_file="$2"
  local local_file="$3"

  case "$role" in
    source)
      [[ "$upstream_file" != *_test.go && "$local_file" != *_test.go ]] || die "production core mapping points at a test file: $upstream_file -> $local_file"
      ;;
    test)
      [[ "$upstream_file" == *_test.go && "$local_file" == *_test.go ]] || die "core test mapping must use _test.go on both sides: $upstream_file -> $local_file"
      ;;
    *)
      die "invalid core file role: $role"
      ;;
  esac
}

validate_manifest() {
  local invalid_rows duplicate_keys snapshot_count
  local upstream_root local_root role upstream_file local_file upstream_blob local_blob reason
  local actual_blob snapshot_symlinks

  [[ -f "$core_manifest" ]] || die "missing core manifest: $core_manifest"
  [[ -f "$provider_manifest" ]] || die "missing provider manifest: $provider_manifest"

  invalid_rows="$(awk -F '|' '
    /^($|#)/ { next }
    $1 == "snapshot" && NF == 2 { next }
    $1 == "root" && NF == 3 { next }
    $1 == "file" && NF == 6 { next }
    $1 == "delete" && NF == 5 { next }
    $1 == "exclude" && NF == 3 { next }
    $1 == "local" && NF == 5 { next }
    $1 == "review" && NF == 5 { next }
    $1 == "skip-test" && NF == 4 { next }
    { print NR ":" $0 }
  ' "$core_manifest")"
  [[ -z "$invalid_rows" ]] || die "invalid core manifest rows: $invalid_rows"

  duplicate_keys="$(awk -F '|' '
    $1 == "snapshot" { key = $1 }
    $1 == "root" { key = $1 FS $2 }
    $1 == "file" { key = $1 FS $3; local_key = "local" FS $4 }
    $1 == "delete" { key = $1 FS $3; local_key = "local" FS $4 }
    $1 == "exclude" { key = $1 FS $2 }
    $1 == "local" { key = $1 FS $3 }
    $1 == "review" { key = $1 FS $3 }
    $1 == "skip-test" { key = $1 FS $2 FS $3 }
    key != "" { count[key]++; key = "" }
    local_key != "" { count[local_key]++; local_key = "" }
    END { for (key in count) if (count[key] > 1) print key }
  ' "$core_manifest")"
  [[ -z "$duplicate_keys" ]] || die "core manifest contains duplicate mappings: $duplicate_keys"

  snapshot_count="$(manifest_row_count snapshot)"
  [[ "$snapshot_count" == "1" ]] || die "core manifest must define exactly one snapshot root"
  snapshot_root="$(awk -F '|' '$1 == "snapshot" { print $2 }' "$core_manifest")"
  is_clean_relative_path "$snapshot_root" || die "core snapshot root is not a clean relative path: $snapshot_root"
  [[ -d "$snapshot_root" ]] || die "core snapshot root does not exist: $snapshot_root"
  [[ ! -L "$snapshot_root" ]] || die "core snapshot root must not be a symlink: $snapshot_root"
  snapshot_symlinks="$(find "$snapshot_root" -type l -print)"
  [[ -z "$snapshot_symlinks" ]] || die "core snapshot tree must not contain symlinks: $snapshot_symlinks"

  (($(manifest_row_count root) > 0)) || die "core manifest contains no direct source roots"
  while IFS='|' read -r _ upstream_root local_root; do
    is_clean_relative_path "$upstream_root" || die "core upstream root is not a clean relative path: $upstream_root"
    is_clean_relative_path "$local_root" || die "core local root is not a clean relative path: $local_root"
    case "$local_root" in
      "$snapshot_root"|"$snapshot_root"/*)
        ;;
      *)
        die "core local root is outside the snapshot: $local_root"
        ;;
    esac
  done < <(awk -F '|' '$1 == "root" { print }' "$core_manifest")

  while IFS='|' read -r _ role upstream_file local_file upstream_blob local_blob; do
    is_clean_relative_path "$upstream_file" || die "core source is not a clean relative path: $upstream_file"
    is_clean_relative_path "$local_file" || die "core destination is not a clean relative path: $local_file"
    case "$local_file" in
      "$snapshot_root"/*)
        ;;
      *)
        die "mapped core destination is outside the snapshot: $local_file"
        ;;
    esac
    validate_role "$role" "$upstream_file" "$local_file"
    is_hash "$upstream_blob" || die "invalid upstream blob hash for core file: $upstream_file"
    is_hash "$local_blob" || die "invalid local blob hash for core file: $local_file"
    [[ -f "$local_file" ]] || die "missing mapped core file: $local_file"
    [[ ! -L "$local_file" ]] || die "mapped core file must not be a symlink: $local_file"
    actual_blob="$(git -C "$upstream_repo" rev-parse "$target_commit:$upstream_file" 2>/dev/null || true)"
    [[ -n "$actual_blob" ]] || die "target commit lacks mapped core source: $upstream_file"
    [[ "$actual_blob" == "$upstream_blob" ]] || die "upstream blob changed without a refreshed core manifest entry: $upstream_file"
    actual_blob="$(git hash-object "$local_file")"
    [[ "$actual_blob" == "$local_blob" ]] || die "local core file changed without a refreshed core manifest entry: $local_file"
  done < <(awk -F '|' '$1 == "file" { print }' "$core_manifest")

  while IFS='|' read -r _ role upstream_file local_file upstream_blob; do
    is_clean_relative_path "$upstream_file" || die "deleted core source is not a clean relative path: $upstream_file"
    is_clean_relative_path "$local_file" || die "deleted core destination is not a clean relative path: $local_file"
    case "$local_file" in
      "$snapshot_root"/*)
        ;;
      *)
        die "deleted core destination is outside the snapshot: $local_file"
        ;;
    esac
    validate_role "$role" "$upstream_file" "$local_file"
    is_hash "$upstream_blob" || die "invalid base blob hash for deleted core file: $upstream_file"
    if git -C "$upstream_repo" cat-file -e "$target_commit:$upstream_file" 2>/dev/null; then
      die "core deletion still exists at the target commit: $upstream_file"
    fi
    [[ ! -e "$local_file" && ! -L "$local_file" ]] || die "deleted upstream core file still exists locally: $local_file"
    if [[ -n "$base_commit" ]]; then
      actual_blob="$(git -C "$upstream_repo" rev-parse "$base_commit:$upstream_file" 2>/dev/null || true)"
      [[ "$actual_blob" == "$upstream_blob" ]] || die "deleted core source does not match the base commit: $upstream_file"
    fi
  done < <(awk -F '|' '$1 == "delete" { print }' "$core_manifest")

  while IFS='|' read -r _ role local_file reason local_blob; do
    is_clean_relative_path "$local_file" || die "local-only core path is not clean: $local_file"
    case "$local_file" in
      "$snapshot_root"/*)
        ;;
      *)
        die "local-only core file is outside the snapshot: $local_file"
        ;;
    esac
    [[ -n "$reason" ]] || die "local-only core file lacks a reason: $local_file"
    is_hash "$local_blob" || die "invalid local blob hash for local-only core file: $local_file"
    [[ -f "$local_file" ]] || die "missing local-only core file: $local_file"
    [[ ! -L "$local_file" ]] || die "local-only core file must not be a symlink: $local_file"
    validate_role "$role" "$local_file" "$local_file"
    actual_blob="$(git hash-object "$local_file")"
    [[ "$actual_blob" == "$local_blob" ]] || die "local-only core file changed without a refreshed manifest entry: $local_file"
  done < <(awk -F '|' '$1 == "local" { print }' "$core_manifest")

  while IFS='|' read -r _ upstream_file reason; do
    is_clean_relative_path "$upstream_file" || die "core exclusion is not a clean relative path: $upstream_file"
    [[ -n "$reason" ]] || die "core exclusion lacks a reason: $upstream_file"
  done < <(awk -F '|' '$1 == "exclude" { print }' "$core_manifest")

  while IFS='|' read -r _ upstream_file test_symbol reason; do
    is_clean_relative_path "$upstream_file" || die "skipped core test path is not clean: $upstream_file"
    [[ "$upstream_file" == *_test.go ]] || die "skipped core test symbol is not attached to a test file: $upstream_file"
    [[ "$test_symbol" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "invalid skipped core test symbol: $test_symbol"
    [[ -n "$reason" ]] || die "skipped core test symbol lacks a reason: $upstream_file ($test_symbol)"
  done < <(awk -F '|' '$1 == "skip-test" { print }' "$core_manifest")
}

validate_review_rows() {
  local role upstream_file upstream_blob local_blob local_file actual_blob classification

  while IFS='|' read -r _ role upstream_file upstream_blob local_blob; do
    is_clean_relative_path "$upstream_file" || die "reviewed core path is not clean: $upstream_file"
    is_hash "$upstream_blob" || die "invalid reviewed upstream blob hash: $upstream_file"
    is_hash "$local_blob" || die "invalid reviewed local blob hash: $upstream_file"
    classification="$(classify_upstream_file "$upstream_file" || true)"
    [[ "$classification" == "core" || "$classification" == "provider" ]] || die "review row does not reference a mapped atomic-sync source: $upstream_file"
    local_file="$(local_file_for_upstream "$upstream_file")"
    validate_role "$role" "$upstream_file" "$local_file"
    actual_blob="$(git -C "$upstream_repo" rev-parse "$target_commit:$upstream_file" 2>/dev/null || true)"
    [[ "$actual_blob" == "$upstream_blob" ]] || die "reviewed upstream blob does not match target commit: $upstream_file"
    actual_blob="$(git hash-object "$local_file")"
    [[ "$actual_blob" == "$local_blob" ]] || die "reviewed local blob does not match synchronized file: $local_file"
  done < <(awk -F '|' '$1 == "review" { print }' "$core_manifest")
}

audit_target_tree() {
  local upstream_file classification scope_path
  local -a scope_paths

  scope_paths=()
  while IFS= read -r scope_path; do
    scope_paths+=("$scope_path")
  done < <({ awk -F '|' '$1 == "root" { print $2 }' "$core_manifest"; awk -F '|' '$1 == "file" || $1 == "delete" { print $3 }' "$core_manifest"; } | sort -u)
  ((${#scope_paths[@]} > 0)) || die "core manifest defines no upstream scope"

  while IFS= read -r upstream_file; do
    case "$upstream_file" in
      *.go|*.json)
        ;;
      *)
        continue
        ;;
    esac
    classification="$(classify_upstream_file "$upstream_file" || true)"
    [[ -n "$classification" ]] || die "target commit contains an unclassified core source: $upstream_file"
  done < <(git -C "$upstream_repo" ls-tree -r --name-only "$target_commit" -- "${scope_paths[@]}")
}

audit_local_tree() {
  local local_file relative upstream_root local_root upstream_file classification found

  while IFS= read -r local_file; do
    case "$local_file" in
      "$snapshot_root/providers/"*)
        continue
        ;;
    esac
    if awk -F '|' -v local_file="$local_file" '($1 == "file" && $4 == local_file) || ($1 == "local" && $3 == local_file) { found = 1 } END { exit !found }' "$core_manifest"; then
      continue
    fi
    found=0
    while IFS='|' read -r _ upstream_root local_root; do
      case "$local_file" in
        "$local_root"/*)
          relative="${local_file#"$local_root"/}"
          upstream_file="$upstream_root/$relative"
          if git -C "$upstream_repo" cat-file -e "$target_commit:$upstream_file" 2>/dev/null; then
            classification="$(classify_upstream_file "$upstream_file" || true)"
            if [[ "$classification" == "core" ]]; then
              found=1
              break
            fi
          fi
          ;;
      esac
    done < <(awk -F '|' '$1 == "root" { print }' "$core_manifest")
    ((found == 1)) || die "local core file is absent from the source manifest: $local_file"
  done < <(find "$snapshot_root" -type f \( -name '*.go' -o -name '*.json' \) | sort)
}

audit_new_test_symbols() {
  local upstream_file="$1"
  local local_file="$2"
  local base_symbols target_symbols local_symbols test_symbol

  [[ -f "$test_lister" ]] || die "missing Go test symbol lister: $test_lister"
  if ! target_symbols="$(git -C "$upstream_repo" show "$target_commit:$upstream_file" | go run "$test_lister" -stdin-name "$upstream_file")"; then
    die "cannot parse target core test: $upstream_file"
  fi
  if git -C "$upstream_repo" cat-file -e "$base_commit:$upstream_file" 2>/dev/null; then
    if ! base_symbols="$(git -C "$upstream_repo" show "$base_commit:$upstream_file" | go run "$test_lister" -stdin-name "$upstream_file")"; then
      die "cannot parse base core test: $upstream_file"
    fi
  else
    base_symbols=""
  fi
  if ! local_symbols="$(go run "$test_lister" -file "$local_file")"; then
    die "cannot parse synchronized core test: $local_file"
  fi

  while IFS= read -r test_symbol; do
    [[ -n "$test_symbol" ]] || continue
    if printf '%s\n' "$base_symbols" | grep -Fxq -- "$test_symbol"; then
      continue
    fi
    if printf '%s\n' "$local_symbols" | grep -Fxq -- "$test_symbol"; then
      continue
    fi
    if awk -F '|' -v upstream_file="$upstream_file" -v test_symbol="$test_symbol" '$1 == "skip-test" && $2 == upstream_file && $3 == test_symbol { found = 1 } END { exit !found }' "$core_manifest"; then
      continue
    fi
    die "new upstream core test is neither synchronized nor explicitly skipped: $upstream_file ($test_symbol)"
  done <<< "$target_symbols"
}

audit_delta() {
  local upstream_file classification scope_path role local_file
  local changed_core=0 changed_provider=0 excluded=0
  local -a scope_paths
  local changed_paths

  [[ -n "$base_commit" ]] || return 0
  git -C "$upstream_repo" cat-file -e "${base_commit}^{commit}" 2>/dev/null || die "base commit is absent from upstream checkout: $base_commit"
  [[ "$base_commit" != "$target_commit" ]] || die "base commit must differ from target commit for a synchronization audit"

  scope_paths=()
  while IFS= read -r scope_path; do
    scope_paths+=("$scope_path")
  done < <({ awk -F '|' '$1 == "root" { print $2 }' "$core_manifest"; awk -F '|' '$1 == "file" || $1 == "delete" { print $3 }' "$core_manifest"; } | sort -u)
  changed_paths="$(git -C "$upstream_repo" diff --name-only --diff-filter=ACDMRTUXB "$base_commit" "$target_commit" -- "${scope_paths[@]}")"

  while IFS= read -r upstream_file; do
    [[ -n "$upstream_file" ]] || continue
    case "$upstream_file" in
      *.go|*.json)
        ;;
      *)
        continue
        ;;
    esac
    classification="$(classify_upstream_file "$upstream_file" || true)"
    case "$classification" in
      core)
        if ! awk -F '|' -v upstream_file="$upstream_file" '$1 == "review" && $3 == upstream_file { found = 1 } END { exit !found }' "$core_manifest"; then
          die "changed core source lacks a review entry: $upstream_file"
        fi
        role="$(awk -F '|' -v upstream_file="$upstream_file" '$1 == "review" && $3 == upstream_file { print $2; exit }' "$core_manifest")"
        if [[ "$role" == "test" ]]; then
          local_file="$(local_file_for_upstream "$upstream_file")"
          audit_new_test_symbols "$upstream_file" "$local_file"
        fi
        changed_core=$((changed_core + 1))
        ;;
      core-deleted)
        changed_core=$((changed_core + 1))
        ;;
      provider)
        if ! awk -F '|' -v upstream_file="$upstream_file" '$1 == "review" && $3 == upstream_file { found = 1 } END { exit !found }' "$core_manifest"; then
          die "changed provider source lacks a review entry: $upstream_file"
        fi
        role="$(awk -F '|' -v upstream_file="$upstream_file" '$1 == "review" && $3 == upstream_file { print $2; exit }' "$core_manifest")"
        if [[ "$role" == "test" ]]; then
          local_file="$(local_file_for_upstream "$upstream_file")"
          audit_new_test_symbols "$upstream_file" "$local_file"
        fi
        changed_provider=$((changed_provider + 1))
        ;;
      core-excluded|provider-excluded)
        excluded=$((excluded + 1))
        ;;
      *)
        die "upstream delta contains an unclassified core source: $upstream_file"
        ;;
    esac
  done <<< "$changed_paths"

  while IFS='|' read -r _ _ upstream_file _ _; do
    if ! printf '%s\n' "$changed_paths" | grep -Fxq -- "$upstream_file"; then
      die "stale core review entry is outside the requested synchronization delta: $upstream_file"
    fi
  done < <(awk -F '|' '$1 == "review" { print }' "$core_manifest")

  while IFS='|' read -r _ _ upstream_file _ _; do
    if ! printf '%s\n' "$changed_paths" | grep -Fxq -- "$upstream_file"; then
      die "stale core deletion entry is outside the requested synchronization delta: $upstream_file"
    fi
  done < <(awk -F '|' '$1 == "delete" { print }' "$core_manifest")

  while IFS='|' read -r _ upstream_file test_symbol _; do
    if ! printf '%s\n' "$changed_paths" | grep -Fxq -- "$upstream_file"; then
      die "stale skipped core test is outside the requested synchronization delta: $upstream_file ($test_symbol)"
    fi
  done < <(awk -F '|' '$1 == "skip-test" { print }' "$core_manifest")

  printf 'Core delta audit passed: core=%d providers=%d excluded=%d\n' "$changed_core" "$changed_provider" "$excluded"
}

run_audit() {
  [[ -n "$upstream_repo" && -n "$target_commit" && -n "$core_manifest" && -n "$provider_manifest" ]] || { usage >&2; exit 2; }
  [[ "$target_commit" =~ ^[0-9a-f]{40}$ ]] || die "--target-commit must be a full 40-character commit SHA"
  [[ -z "$base_commit" || "$base_commit" =~ ^[0-9a-f]{40}$ ]] || die "--base-commit must be a full 40-character commit SHA"
  git -C "$upstream_repo" rev-parse --git-dir >/dev/null 2>&1 || die "--upstream-repo is not a Git checkout: $upstream_repo"
  git -C "$upstream_repo" cat-file -e "${target_commit}^{commit}" 2>/dev/null || die "target commit is absent from upstream checkout: $target_commit"

  validate_manifest
  validate_review_rows
  audit_target_tree
  audit_local_tree
  audit_delta
  printf 'Core scope audit passed: target=%s reviews=%s\n' "$target_commit" "$(manifest_row_count review)"
}

self_test() {
  local self_test_root upstream_dir local_dir provider_file missing_manifest bad_manifest reviewed_manifest good_manifest delete_manifest
  local base target delete_target base_source_blob base_test_blob target_source_blob target_test_blob deleted_blob
  local local_source_blob local_test_blob output

  self_test_root="$(mktemp -d)"
  trap 'rm -rf -- "$self_test_root"' EXIT
  upstream_dir="$self_test_root/upstream"
  local_dir="$self_test_root/local"
  provider_file="$local_dir/provider.manifest"
  missing_manifest="$local_dir/core-missing.manifest"
  bad_manifest="$local_dir/core-bad.manifest"
  reviewed_manifest="$local_dir/core-reviewed.manifest"
  good_manifest="$local_dir/core-good.manifest"
  delete_manifest="$local_dir/core-delete.manifest"

  git init -q "$upstream_dir"
  git -C "$upstream_dir" config user.name 'core-scope-self-test'
  git -C "$upstream_dir" config user.email 'core-scope-self-test@example.invalid'
  mkdir -p "$upstream_dir/internal/translator/common" "$upstream_dir/internal/util"
  printf 'package common\n\nconst Stable = 1\n' > "$upstream_dir/internal/translator/common/request.go"
  printf 'package util\n\nconst SchemaVersion = 1\n' > "$upstream_dir/internal/util/gemini_schema.go"
  printf 'package util\n\nfunc TestSchemaVersion() {}\n' > "$upstream_dir/internal/util/gemini_schema_test.go"
  git -C "$upstream_dir" add internal
  git -C "$upstream_dir" commit -q -m base
  base="$(git -C "$upstream_dir" rev-parse HEAD)"

  mkdir -p "$local_dir/snapshot/common" "$local_dir/snapshot/util"
  git -C "$upstream_dir" show "$base:internal/translator/common/request.go" > "$local_dir/snapshot/common/request.go"
  git -C "$upstream_dir" show "$base:internal/util/gemini_schema.go" > "$local_dir/snapshot/util/gemini_schema.go"
  git -C "$upstream_dir" show "$base:internal/util/gemini_schema_test.go" > "$local_dir/snapshot/util/gemini_schema_test.go"

  printf 'package util\n\nconst SchemaVersion = 2\n' > "$upstream_dir/internal/util/gemini_schema.go"
  printf 'package util\n\nfunc TestSchemaVersion() {}\nfunc TestConditionalSchema() {}\n' > "$upstream_dir/internal/util/gemini_schema_test.go"
  git -C "$upstream_dir" add internal/util
  git -C "$upstream_dir" commit -q -m target
  target="$(git -C "$upstream_dir" rev-parse HEAD)"

  base_source_blob="$(git -C "$upstream_dir" rev-parse "$base:internal/util/gemini_schema.go")"
  base_test_blob="$(git -C "$upstream_dir" rev-parse "$base:internal/util/gemini_schema_test.go")"
  local_source_blob="$(git hash-object "$local_dir/snapshot/util/gemini_schema.go")"
  local_test_blob="$(git hash-object "$local_dir/snapshot/util/gemini_schema_test.go")"
  printf '# empty provider manifest for core-scope self-test\n' > "$provider_file"
  {
    printf 'snapshot|snapshot\n'
    printf 'root|internal/translator|snapshot\n'
    printf 'root|internal/util|snapshot/util\n'
  } > "$missing_manifest"
  if output="$(cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$missing_manifest" --provider-manifest "$provider_file" 2>&1)"; then
    die "core-scope self-test accepted an omitted gemini_schema sync"
  fi
  [[ "$output" == *"changed core source lacks a review entry: internal/util/gemini_schema.go"* ]] || die "omitted core-sync check failed for the wrong reason: $output"

  {
    printf 'snapshot|snapshot\n'
    printf 'root|internal/translator|snapshot\n'
    printf 'root|internal/util|snapshot/util\n'
    printf 'review|source|internal/util/gemini_schema.go|%s|%s\n' "$base_source_blob" "$local_source_blob"
    printf 'review|test|internal/util/gemini_schema_test.go|%s|%s\n' "$base_test_blob" "$local_test_blob"
  } > "$bad_manifest"

  if output="$(cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$bad_manifest" --provider-manifest "$provider_file" 2>&1)"; then
    die "core-scope self-test accepted stale gemini_schema provenance"
  fi
  [[ "$output" == *"reviewed upstream blob does not match target commit: internal/util/gemini_schema.go"* ]] || die "core-scope self-test failed for the wrong reason: $output"

  target_source_blob="$(git -C "$upstream_dir" rev-parse "$target:internal/util/gemini_schema.go")"
  target_test_blob="$(git -C "$upstream_dir" rev-parse "$target:internal/util/gemini_schema_test.go")"
  {
    printf 'snapshot|snapshot\n'
    printf 'root|internal/translator|snapshot\n'
    printf 'root|internal/util|snapshot/util\n'
    printf 'review|source|internal/util/gemini_schema.go|%s|%s\n' "$target_source_blob" "$local_source_blob"
    printf 'review|test|internal/util/gemini_schema_test.go|%s|%s\n' "$target_test_blob" "$local_test_blob"
  } > "$reviewed_manifest"
  if output="$(cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$reviewed_manifest" --provider-manifest "$provider_file" 2>&1)"; then
    die "core-scope self-test accepted a missing new upstream test"
  fi
  [[ "$output" == *"new upstream core test is neither synchronized nor explicitly skipped: internal/util/gemini_schema_test.go (TestConditionalSchema)"* ]] || die "core-scope test-symbol check failed for the wrong reason: $output"

  git -C "$upstream_dir" show "$target:internal/util/gemini_schema.go" > "$local_dir/snapshot/util/gemini_schema.go"
  git -C "$upstream_dir" show "$target:internal/util/gemini_schema_test.go" > "$local_dir/snapshot/util/gemini_schema_test.go"
  local_source_blob="$(git hash-object "$local_dir/snapshot/util/gemini_schema.go")"
  local_test_blob="$(git hash-object "$local_dir/snapshot/util/gemini_schema_test.go")"
  {
    printf 'snapshot|snapshot\n'
    printf 'root|internal/translator|snapshot\n'
    printf 'root|internal/util|snapshot/util\n'
    printf 'review|source|internal/util/gemini_schema.go|%s|%s\n' "$target_source_blob" "$local_source_blob"
    printf 'review|test|internal/util/gemini_schema_test.go|%s|%s\n' "$target_test_blob" "$local_test_blob"
  } > "$good_manifest"

  (cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$good_manifest" --provider-manifest "$provider_file") >/dev/null
  (cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --core-manifest "$good_manifest" --provider-manifest "$provider_file") >/dev/null

  deleted_blob="$(git -C "$upstream_dir" rev-parse "$target:internal/translator/common/request.go")"
  git -C "$upstream_dir" rm -q internal/translator/common/request.go
  git -C "$upstream_dir" commit -q -m delete
  delete_target="$(git -C "$upstream_dir" rev-parse HEAD)"
  rm -- "$local_dir/snapshot/common/request.go"
  {
    printf 'snapshot|snapshot\n'
    printf 'root|internal/translator|snapshot\n'
    printf 'root|internal/util|snapshot/util\n'
    printf 'delete|source|internal/translator/common/request.go|snapshot/common/request.go|%s\n' "$deleted_blob"
  } > "$delete_manifest"
  (cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$delete_target" --base-commit "$target" --core-manifest "$delete_manifest" --provider-manifest "$provider_file") >/dev/null

  rm -rf -- "$self_test_root"
  trap - EXIT
  printf 'PASS: core scope verifier self-test\n'
}

if ((run_self_test == 1)); then
  [[ -z "$upstream_repo$target_commit$base_commit$core_manifest$provider_manifest" ]] || { usage >&2; exit 2; }
  self_test
  exit 0
fi

run_audit
