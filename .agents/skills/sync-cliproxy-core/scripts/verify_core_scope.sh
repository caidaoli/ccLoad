#!/usr/bin/env bash
set -euo pipefail

upstream_repo=""
target_commit=""
base_commit=""
core_manifest=""
provider_manifest=""
run_self_test=0
run_refresh=0

usage() {
  printf 'Usage: %s --upstream-repo PATH --target-commit SHA [--base-commit SHA] [--core-manifest PATH] [--provider-manifest PATH]\n' "$0"
  printf '       %s --refresh-manifest --upstream-repo PATH --base-commit SHA --target-commit SHA [--core-manifest PATH] [--provider-manifest PATH]\n' "$0"
  printf '       %s --self-test\n' "$0"
  printf 'Manifests default to the skill references; run from the repository root.\n'
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
    --refresh-manifest)
      run_refresh=1
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
test_lister_bin=""
work_dir=""
references_dir="$(dirname "$(dirname "$script_path")")/references"

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

file_role() {
  if [[ "$1" == *_test.go ]]; then
    printf 'test\n'
  else
    printf 'source\n'
  fi
}

manifest_scope_paths() {
  { awk -F '|' '$1 == "root" { print $2 }' "$core_manifest"; awk -F '|' '$1 == "file" || $1 == "delete" { print $3 }' "$core_manifest"; } | sort -u
}

delta_paths() {
  local scope_path
  local -a scope_paths=()

  while IFS= read -r scope_path; do
    scope_paths+=("$scope_path")
  done < <(manifest_scope_paths)
  ((${#scope_paths[@]} > 0)) || die "core manifest defines no upstream scope"
  git -C "$upstream_repo" diff --name-only --diff-filter=ACDMRTUXB "$base_commit" "$target_commit" -- "${scope_paths[@]}"
}

ensure_work_dir() {
  [[ -z "$work_dir" ]] || return 0
  work_dir="$(mktemp -d "${TMPDIR:-/tmp}/verify-core-scope.XXXXXX")"
  trap 'rm -rf -- "$work_dir"' EXIT
}

build_test_lister() {
  [[ -f "$test_lister" ]] || die "missing Go test symbol lister: $test_lister"
  ensure_work_dir
  go build -o "$work_dir/list_go_tests" "$test_lister" || die "cannot build Go test symbol lister: $test_lister"
  test_lister_bin="$work_dir/list_go_tests"
}

# Every upstream file is classified, so lookups must not fork: load_manifests reads both
# manifests once into small arrays (scanned linearly) and newline-delimited sets. Only
# test membership on the sets; bash 3.2 prefix removal on long strings is quadratic.
load_manifests() {
  local kind f2 f3 f4 f5

  root_ups=()
  root_locals=()
  special_ups=()
  special_locals=()
  core_excludes=()
  provider_ups=()
  provider_locals=()
  provider_excludes=()
  deleted_files=$'\n'
  reviewed_files=$'\n'
  skipped_tests=$'\n'
  # audit_target_tree adds the local file of every target upstream core source.
  accounted_local_files=$'\n'
  while IFS='|' read -r kind f2 f3 f4 _ || [[ -n "$kind" ]]; do
    case "$kind" in
      root)
        root_ups+=("$f2")
        root_locals+=("$f3")
        ;;
      file)
        special_ups+=("$f3")
        special_locals+=("$f4")
        accounted_local_files+="$f4"$'\n'
        ;;
      local)
        accounted_local_files+="$f3"$'\n'
        ;;
      delete)
        deleted_files+="$f3"$'\n'
        ;;
      exclude)
        core_excludes+=("$f2")
        ;;
      review)
        reviewed_files+="$f3"$'\n'
        ;;
      skip-test)
        skipped_tests+="$f2|$f3"$'\n'
        ;;
    esac
  done < "$core_manifest"
  while IFS='|' read -r kind _ f3 f4 f5 _ || [[ -n "$kind" ]]; do
    case "$kind" in
      file)
        provider_ups+=("$f4")
        provider_locals+=("$f5")
        ;;
      exclude)
        provider_excludes+=("$f3")
        ;;
      skip-test)
        # Provider skips persist in the provider manifest because verify.sh audits every provider test symbol.
        skipped_tests+="$f3|$f4"$'\n'
        ;;
    esac
  done < "$provider_manifest"
}

in_set() {
  [[ "$1" == *$'\n'"$2"$'\n'* ]]
}

# Sets classification to core, core-deleted, core-excluded, provider or provider-excluded
# (empty when unclassified) and mapped_local to the synchronized file of core/provider sources.
classify_upstream_file() {
  local upstream_file="$1"
  local i relative

  classification=""
  mapped_local=""
  if in_set "$deleted_files" "$upstream_file"; then
    classification="core-deleted"
    return 0
  fi
  for ((i = 0; i < ${#provider_ups[@]}; i++)); do
    if [[ "${provider_ups[i]}" == "$upstream_file" ]]; then
      classification="provider"
      mapped_local="${provider_locals[i]}"
      return 0
    fi
  done
  for ((i = 0; i < ${#provider_excludes[@]}; i++)); do
    # Manifest exclusions are deliberate globs.
    # shellcheck disable=SC2053
    if [[ "$upstream_file" == ${provider_excludes[i]} ]]; then
      classification="provider-excluded"
      return 0
    fi
  done
  for ((i = 0; i < ${#special_ups[@]}; i++)); do
    if [[ "${special_ups[i]}" == "$upstream_file" ]]; then
      classification="core"
      mapped_local="${special_locals[i]}"
      return 0
    fi
  done
  for ((i = 0; i < ${#root_ups[@]}; i++)); do
    case "$upstream_file" in
      "${root_ups[i]}"/*)
        relative="${upstream_file#"${root_ups[i]}"/}"
        if [[ -f "${root_locals[i]}/$relative" ]]; then
          classification="core"
          mapped_local="${root_locals[i]}/$relative"
          return 0
        fi
        ;;
    esac
  done
  for ((i = 0; i < ${#core_excludes[@]}; i++)); do
    # shellcheck disable=SC2053
    if [[ "$upstream_file" == ${core_excludes[i]} ]]; then
      classification="core-excluded"
      return 0
    fi
  done
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

# Review rows grow with the delta, so blobs are resolved by one git process per tree:
# cat-file --batch-check and hash-object --stdin-paths both answer in input order.
validate_review_rows() {
  local role upstream_file upstream_blob local_blob actual_blob actual_blobs classification index
  local -a upstream_files=() local_files=() upstream_blobs=() local_blobs=()

  while IFS='|' read -r _ role upstream_file upstream_blob local_blob; do
    is_clean_relative_path "$upstream_file" || die "reviewed core path is not clean: $upstream_file"
    is_hash "$upstream_blob" || die "invalid reviewed upstream blob hash: $upstream_file"
    is_hash "$local_blob" || die "invalid reviewed local blob hash: $upstream_file"
    classify_upstream_file "$upstream_file"
    [[ "$classification" == "core" || "$classification" == "provider" ]] || die "review row does not reference a mapped atomic-sync source: $upstream_file"
    validate_role "$role" "$upstream_file" "$mapped_local"
    [[ -f "$mapped_local" ]] || die "reviewed synchronized file is missing: $mapped_local"
    upstream_files+=("$upstream_file")
    local_files+=("$mapped_local")
    upstream_blobs+=("$upstream_blob")
    local_blobs+=("$local_blob")
  done < <(awk -F '|' '$1 == "review" { print }' "$core_manifest")
  ((${#upstream_files[@]} > 0)) || return 0

  actual_blobs="$(printf '%s\n' "${upstream_files[@]/#/$target_commit:}" | git -C "$upstream_repo" cat-file --batch-check='%(objectname)')" ||
    die "cannot resolve reviewed upstream blobs at the target commit"
  index=0
  while IFS= read -r actual_blob; do
    [[ "$actual_blob" == "${upstream_blobs[index]}" ]] || die "reviewed upstream blob does not match target commit: ${upstream_files[index]}"
    index=$((index + 1))
  done <<< "$actual_blobs"

  actual_blobs="$(printf '%s\n' "${local_files[@]}" | git hash-object --stdin-paths)" || die "cannot hash reviewed synchronized files"
  index=0
  while IFS= read -r actual_blob; do
    [[ "$actual_blob" == "${local_blobs[index]}" ]] || die "reviewed local blob does not match synchronized file: ${local_files[index]}"
    index=$((index + 1))
  done <<< "$actual_blobs"
}

audit_target_tree() {
  local upstream_file classification scope_path
  local -a scope_paths

  scope_paths=()
  while IFS= read -r scope_path; do
    scope_paths+=("$scope_path")
  done < <(manifest_scope_paths)
  ((${#scope_paths[@]} > 0)) || die "core manifest defines no upstream scope"

  while IFS= read -r upstream_file; do
    case "$upstream_file" in
      *.go|*.json)
        ;;
      *)
        continue
        ;;
    esac
    classify_upstream_file "$upstream_file"
    [[ -n "$classification" ]] || die "target commit contains an unclassified core source: $upstream_file"
    if [[ "$classification" == "core" ]]; then
      accounted_local_files+="$mapped_local"$'\n'
    fi
  done < <(git -C "$upstream_repo" ls-tree -r --name-only "$target_commit" -- "${scope_paths[@]}")
}

# Runs after audit_target_tree: a local core file must be a manifest row or the mapped
# copy of a target upstream core source.
audit_local_tree() {
  local local_file

  while IFS= read -r local_file; do
    case "$local_file" in
      "$snapshot_root/providers/"*)
        continue
        ;;
    esac
    in_set "$accounted_local_files" "$local_file" || die "local core file is absent from the source manifest: $local_file"
  done < <(find "$snapshot_root" -type f \( -name '*.go' -o -name '*.json' \) | sort)
}

audit_new_test_symbols() {
  local upstream_file="$1"
  local local_file="$2"
  local base_symbols target_symbols local_symbols known_symbols test_symbol

  [[ -n "$test_lister_bin" ]] || build_test_lister
  if ! target_symbols="$(git -C "$upstream_repo" show "$target_commit:$upstream_file" | "$test_lister_bin" -stdin-name "$upstream_file")"; then
    die "cannot parse target core test: $upstream_file"
  fi
  if git -C "$upstream_repo" cat-file -e "$base_commit:$upstream_file" 2>/dev/null; then
    if ! base_symbols="$(git -C "$upstream_repo" show "$base_commit:$upstream_file" | "$test_lister_bin" -stdin-name "$upstream_file")"; then
      die "cannot parse base core test: $upstream_file"
    fi
  else
    base_symbols=""
  fi
  if ! local_symbols="$("$test_lister_bin" -file "$local_file")"; then
    die "cannot parse synchronized core test: $local_file"
  fi

  known_symbols=$'\n'"$base_symbols"$'\n'"$local_symbols"$'\n'
  while IFS= read -r test_symbol; do
    [[ -n "$test_symbol" ]] || continue
    in_set "$known_symbols" "$test_symbol" && continue
    in_set "$skipped_tests" "$upstream_file|$test_symbol" && continue
    die "new upstream core test is neither synchronized nor explicitly skipped: $upstream_file ($test_symbol)"
  done <<< "$target_symbols"
}

audit_delta() {
  local upstream_file classification changed_paths changed_files
  local changed_core=0 changed_provider=0 excluded=0

  [[ -n "$base_commit" ]] || return 0
  [[ "$base_commit" != "$target_commit" ]] || die "base commit must differ from target commit for a synchronization audit"

  changed_paths="$(delta_paths)"
  changed_files=$'\n'"$changed_paths"$'\n'

  while IFS= read -r upstream_file; do
    [[ -n "$upstream_file" ]] || continue
    case "$upstream_file" in
      *.go|*.json)
        ;;
      *)
        continue
        ;;
    esac
    classify_upstream_file "$upstream_file"
    case "$classification" in
      core|provider)
        in_set "$reviewed_files" "$upstream_file" || die "changed $classification source lacks a review entry: $upstream_file"
        # validate_review_rows already tied every review role to the _test.go suffix.
        if [[ "$upstream_file" == *_test.go ]]; then
          audit_new_test_symbols "$upstream_file" "$mapped_local"
        fi
        if [[ "$classification" == "core" ]]; then
          changed_core=$((changed_core + 1))
        else
          changed_provider=$((changed_provider + 1))
        fi
        ;;
      core-deleted)
        changed_core=$((changed_core + 1))
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
    in_set "$changed_files" "$upstream_file" || die "stale core review entry is outside the requested synchronization delta: $upstream_file"
  done < <(awk -F '|' '$1 == "review" { print }' "$core_manifest")

  while IFS='|' read -r _ _ upstream_file _ _; do
    in_set "$changed_files" "$upstream_file" || die "stale core deletion entry is outside the requested synchronization delta: $upstream_file"
  done < <(awk -F '|' '$1 == "delete" { print }' "$core_manifest")

  while IFS='|' read -r _ upstream_file test_symbol _; do
    in_set "$changed_files" "$upstream_file" || die "stale skipped core test is outside the requested synchronization delta: $upstream_file ($test_symbol)"
  done < <(awk -F '|' '$1 == "skip-test" { print }' "$core_manifest")

  printf 'Core delta audit passed: core=%d providers=%d excluded=%d\n' "$changed_core" "$changed_provider" "$excluded"
}

check_inputs() {
  [[ -n "$upstream_repo" && -n "$target_commit" ]] || { usage >&2; exit 2; }
  [[ "$target_commit" =~ ^[0-9a-f]{40}$ ]] || die "--target-commit must be a full 40-character commit SHA"
  [[ -z "$base_commit" || "$base_commit" =~ ^[0-9a-f]{40}$ ]] || die "--base-commit must be a full 40-character commit SHA"
  git -C "$upstream_repo" rev-parse --git-dir >/dev/null 2>&1 || die "--upstream-repo is not a Git checkout: $upstream_repo"
  git -C "$upstream_repo" cat-file -e "${target_commit}^{commit}" 2>/dev/null || die "target commit is absent from upstream checkout: $target_commit"
  [[ -z "$base_commit" ]] || git -C "$upstream_repo" cat-file -e "${base_commit}^{commit}" 2>/dev/null || die "base commit is absent from upstream checkout: $base_commit"
  [[ -f "$core_manifest" ]] || die "missing core manifest: $core_manifest"
  [[ -f "$provider_manifest" ]] || die "missing provider manifest: $provider_manifest"
}

refresh_todo() {
  printf 'TODO: %s\n' "$1" >&2
  refresh_todos=$((refresh_todos + 1))
}

# Recomputes every blob-bearing core manifest row from the current trees: the review
# block for base..target, file/local blobs, and per-delta delete/skip-test rows that
# fall outside the delta. Files that still need a human decision block the write.
refresh_manifest() {
  local upstream_file classification local_file blob changed_paths review_count dropped_count
  refresh_todos=0

  check_inputs
  load_manifests
  [[ -n "$base_commit" ]] || { usage >&2; exit 2; }
  [[ "$base_commit" != "$target_commit" ]] || die "base commit must differ from target commit for a manifest refresh"
  ensure_work_dir
  changed_paths="$(delta_paths)"
  printf '%s\n' "$changed_paths" > "$work_dir/delta"
  : > "$work_dir/reviews"
  : > "$work_dir/blobs"

  while IFS= read -r upstream_file; do
    case "$upstream_file" in
      *.go|*.json)
        ;;
      *)
        continue
        ;;
    esac
    classify_upstream_file "$upstream_file"
    if ! blob="$(git -C "$upstream_repo" rev-parse -q --verify "$target_commit:$upstream_file")"; then
      case "$classification" in
        core-deleted|core-excluded|provider-excluded)
          ;;
        *)
          refresh_todo "upstream removed $upstream_file: remove its local copy and mapping; core files record delete|$(file_role "$upstream_file")|$upstream_file|<local path>|$(git -C "$upstream_repo" rev-parse "$base_commit:$upstream_file")"
          ;;
      esac
      continue
    fi
    case "$classification" in
      core|provider)
        local_file="$mapped_local"
        if [[ ! -f "$local_file" ]]; then
          refresh_todo "mapped local file is missing: $local_file ($upstream_file)"
          continue
        fi
        printf 'review|%s|%s|%s|%s\n' "$(file_role "$upstream_file")" "$upstream_file" "$blob" "$(git hash-object "$local_file")" >> "$work_dir/reviews"
        ;;
      core-excluded|provider-excluded)
        ;;
      core-deleted)
        refresh_todo "delete row names a file that still exists at the target commit: $upstream_file"
        ;;
      *)
        refresh_todo "unclassified upstream file: $upstream_file (port it under a mapped root, or add a file/exclude row)"
        ;;
    esac
  done <<< "$changed_paths"

  while IFS='|' read -r _ _ upstream_file local_file _ _; do
    blob="$(git -C "$upstream_repo" rev-parse -q --verify "$target_commit:$upstream_file")" || continue
    if [[ ! -f "$local_file" ]]; then
      refresh_todo "missing mapped core file: $local_file"
      continue
    fi
    printf 'file|%s|%s|%s\n' "$upstream_file" "$blob" "$(git hash-object "$local_file")" >> "$work_dir/blobs"
  done < <(awk -F '|' '$1 == "file" { print }' "$core_manifest")
  while IFS='|' read -r _ _ local_file _ _; do
    if [[ ! -f "$local_file" ]]; then
      refresh_todo "missing local-only core file: $local_file"
      continue
    fi
    printf 'local|%s|%s\n' "$local_file" "$(git hash-object "$local_file")" >> "$work_dir/blobs"
  done < <(awk -F '|' '$1 == "local" { print }' "$core_manifest")

  ((refresh_todos == 0)) || die "$refresh_todos delta item(s) need a manifest decision; $core_manifest left unchanged"

  awk -F '|' -v OFS='|' -v work_dir="$work_dir" -v header="# Reviewed atomic delta: $base_commit -> $target_commit." '
    BEGIN {
      while ((getline line < (work_dir "/blobs")) > 0) {
        split(line, f, "|")
        if (f[1] == "file") { upstream_blob[f[2]] = f[3]; file_blob[f[2]] = f[4] } else { local_blob[f[2]] = f[3] }
      }
      while ((getline line < (work_dir "/delta")) > 0) changed[line] = 1
    }
    function emit_reviews(  line) {
      print header
      while ((getline line < (work_dir "/reviews")) > 0) print line
      emitted = 1
    }
    $1 == "review" { next }
    ($1 == "delete" && !($3 in changed)) || ($1 == "skip-test" && !($2 in changed)) { dropped++; next }
    /^# Reviewed atomic delta:/ { if (!emitted) emit_reviews(); next }
    $1 == "file" && ($3 in upstream_blob) { $5 = upstream_blob[$3]; $6 = file_blob[$3] }
    $1 == "local" && ($3 in local_blob) { $5 = local_blob[$3] }
    { print }
    END {
      if (!emitted) { print ""; emit_reviews() }
      print dropped + 0 > (work_dir "/dropped")
    }
  ' "$core_manifest" > "$work_dir/manifest"

  review_count="$(grep -c . "$work_dir/reviews" || true)"
  dropped_count="$(cat "$work_dir/dropped")"
  if cmp -s "$work_dir/manifest" "$core_manifest"; then
    printf 'Core manifest already current: reviews=%s\n' "$review_count"
    return 0
  fi
  cat "$work_dir/manifest" > "$core_manifest"
  printf 'Refreshed core manifest: reviews=%s dropped-stale=%s; review git diff, then run verify.sh\n' "$review_count" "$dropped_count"
}

run_audit() {
  check_inputs
  load_manifests
  validate_manifest
  validate_review_rows
  audit_target_tree
  audit_local_tree
  audit_delta
  printf 'Core scope audit passed: target=%s reviews=%s\n' "$target_commit" "$(manifest_row_count review)"
}

self_test() {
  local self_test_root upstream_dir local_dir provider_file missing_manifest bad_manifest reviewed_manifest good_manifest delete_manifest refreshed_manifest
  local base target delete_target base_source_blob base_test_blob target_source_blob target_test_blob deleted_blob
  local local_source_blob local_test_blob output

  self_test_root="$(mktemp -d "${TMPDIR:-/tmp}/verify-core-scope.XXXXXX")"
  trap 'rm -rf -- "$self_test_root"' EXIT
  upstream_dir="$self_test_root/upstream"
  local_dir="$self_test_root/local"
  provider_file="$local_dir/provider.manifest"
  missing_manifest="$local_dir/core-missing.manifest"
  bad_manifest="$local_dir/core-bad.manifest"
  reviewed_manifest="$local_dir/core-reviewed.manifest"
  good_manifest="$local_dir/core-good.manifest"
  delete_manifest="$local_dir/core-delete.manifest"
  refreshed_manifest="$local_dir/core-refresh.manifest"

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

  cp -- "$missing_manifest" "$refreshed_manifest"
  (cd "$local_dir" && bash "$script_path" --refresh-manifest --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$refreshed_manifest" --provider-manifest "$provider_file") >/dev/null
  [[ "$(grep '^review|' "$refreshed_manifest")" == "$(grep '^review|' "$good_manifest")" ]] || die "manifest refresh generated unexpected review rows: $(cat "$refreshed_manifest")"
  (cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$target" --base-commit "$base" --core-manifest "$refreshed_manifest" --provider-manifest "$provider_file") >/dev/null

  deleted_blob="$(git -C "$upstream_dir" rev-parse "$target:internal/translator/common/request.go")"
  git -C "$upstream_dir" rm -q internal/translator/common/request.go
  git -C "$upstream_dir" commit -q -m delete
  delete_target="$(git -C "$upstream_dir" rev-parse HEAD)"
  rm -- "$local_dir/snapshot/common/request.go"
  cp -- "$good_manifest" "$delete_manifest"
  printf 'skip-test|internal/util/gemini_schema_test.go|TestConditionalSchema|previous-delta-only\n' >> "$delete_manifest"
  if output="$(cd "$local_dir" && bash "$script_path" --refresh-manifest --upstream-repo "$upstream_dir" --target-commit "$delete_target" --base-commit "$target" --core-manifest "$delete_manifest" --provider-manifest "$provider_file" 2>&1)"; then
    die "manifest refresh accepted an unrecorded upstream deletion"
  fi
  [[ "$output" == *"TODO: upstream removed internal/translator/common/request.go"* ]] || die "manifest refresh rejected the deletion for the wrong reason: $output"
  grep -q '^review|' "$delete_manifest" || die "manifest refresh wrote despite unresolved TODOs"
  printf 'delete|source|internal/translator/common/request.go|snapshot/common/request.go|%s\n' "$deleted_blob" >> "$delete_manifest"
  (cd "$local_dir" && bash "$script_path" --refresh-manifest --upstream-repo "$upstream_dir" --target-commit "$delete_target" --base-commit "$target" --core-manifest "$delete_manifest" --provider-manifest "$provider_file") >/dev/null
  ! grep -Eq '^(review|skip-test)\|' "$delete_manifest" || die "manifest refresh kept rows outside the requested delta: $(cat "$delete_manifest")"
  (cd "$local_dir" && bash "$script_path" --upstream-repo "$upstream_dir" --target-commit "$delete_target" --base-commit "$target" --core-manifest "$delete_manifest" --provider-manifest "$provider_file") >/dev/null

  rm -rf -- "$self_test_root"
  trap - EXIT
  printf 'PASS: core scope verifier self-test\n'
}

if ((run_self_test == 1)); then
  [[ -z "$upstream_repo$target_commit$base_commit$core_manifest$provider_manifest" && "$run_refresh" == 0 ]] || { usage >&2; exit 2; }
  self_test
  exit 0
fi

core_manifest="${core_manifest:-$references_dir/core-snapshot.manifest}"
provider_manifest="${provider_manifest:-$references_dir/provider-adapters.manifest}"
if ((run_refresh == 1)); then
  refresh_manifest
else
  run_audit
fi
