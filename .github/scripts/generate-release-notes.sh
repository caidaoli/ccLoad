#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file=$1
  local value=$2
  grep -Fq -- "$value" "$file" || fail "$file does not contain: $value"
}

assert_not_contains() {
  local file=$1
  local value=$2
  if grep -Fq -- "$value" "$file"; then
    fail "$file unexpectedly contains: $value"
  fi
}

self_test_tmp=

cleanup_self_test() {
  if [[ -n "$self_test_tmp" && -d "$self_test_tmp" ]]; then
    rm -rf -- "$self_test_tmp"
  fi
}

self_test() {
  local script_path repo notes
  script_path=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/$(basename -- "${BASH_SOURCE[0]}")
  self_test_tmp=$(mktemp -d)
  trap cleanup_self_test EXIT
  repo=$self_test_tmp/repo
  notes=$self_test_tmp/notes.md

  git init -q -b master "$repo"
  git -C "$repo" config user.name test
  git -C "$repo" config user.email test@example.com

  printf 'initial\n' > "$repo/change.txt"
  git -C "$repo" add change.txt
  git -C "$repo" commit -q -m 'chore: initial'
  git -C "$repo" tag -a v1.0.0 -m 'Release v1.0.0'

  printf 'beta one\n' > "$repo/change.txt"
  git -C "$repo" commit -qam 'fix(core): direct beta fix'
  git -C "$repo" tag -a v1.1.0-beta.1 -m 'Release v1.1.0-beta.1'

  (
    cd "$repo"
    GITHUB_REPOSITORY=caidaoli/ccLoad bash "$script_path" v1.1.0-beta.1 "$notes"
  )
  assert_contains "$notes" 'fix(core): direct beta fix'
  assert_contains "$notes" 'compare/v1.0.0...v1.1.0-beta.1'
  assert_not_contains "$notes" 'chore: initial'

  printf 'beta two\n' > "$repo/change.txt"
  git -C "$repo" commit -qam 'feat(api): beta follow-up'
  git -C "$repo" tag -a v1.1.0-beta.2 -m 'Release v1.1.0-beta.2'

  (
    cd "$repo"
    GITHUB_REPOSITORY=caidaoli/ccLoad bash "$script_path" v1.1.0-beta.2 "$notes"
  )
  assert_contains "$notes" 'feat(api): beta follow-up'
  assert_contains "$notes" 'compare/v1.1.0-beta.1...v1.1.0-beta.2'
  assert_not_contains "$notes" 'fix(core): direct beta fix'

  printf 'stable\n' > "$repo/change.txt"
  git -C "$repo" commit -qam 'docs: stable notes'
  git -C "$repo" tag -a v1.1.0 -m 'Release v1.1.0'

  (
    cd "$repo"
    GITHUB_REPOSITORY=caidaoli/ccLoad bash "$script_path" v1.1.0 "$notes"
  )
  assert_contains "$notes" 'fix(core): direct beta fix'
  assert_contains "$notes" 'feat(api): beta follow-up'
  assert_contains "$notes" 'docs: stable notes'
  assert_contains "$notes" 'compare/v1.0.0...v1.1.0'

  git -C "$repo" tag -a v1.1.1-beta.1 -m 'Release v1.1.1-beta.1'
  if (
    cd "$repo"
    GITHUB_REPOSITORY=caidaoli/ccLoad bash "$script_path" v1.1.1-beta.1 "$notes"
  ) >/dev/null 2>&1; then
    fail 'empty release range unexpectedly succeeded'
  fi

  printf 'PASS: release notes self-test\n'
}

latest_stable_before() {
  local release_tag=$1
  local release_commit=$2
  local candidate

  while IFS= read -r candidate; do
    [[ "$candidate" == "$release_tag" ]] && continue
    if [[ "$candidate" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done < <(git tag --merged "$release_commit" --list 'v*' --sort=-version:refname)

  return 1
}

previous_beta_before() {
  local release_core=$1
  local release_number=$2
  local release_commit=$3
  local escaped_core candidate candidate_number
  local previous_tag=
  local previous_number=0

  escaped_core=${release_core//./\.}
  while IFS= read -r candidate; do
    if [[ "$candidate" =~ ^v${escaped_core}-beta\.([1-9][0-9]*)$ ]]; then
      candidate_number=${BASH_REMATCH[1]}
      if (( candidate_number < release_number && candidate_number > previous_number )); then
        previous_tag=$candidate
        previous_number=$candidate_number
      fi
    fi
  done < <(git tag --merged "$release_commit" --list "v$release_core-beta.*")

  [[ -n "$previous_tag" ]] || return 1
  printf '%s\n' "$previous_tag"
}

resolve_base_tag() {
  local release_tag=$1
  local release_commit=$2
  local release_core release_number previous_beta

  if [[ "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    latest_stable_before "$release_tag" "$release_commit"
    return
  fi
  if [[ "$release_tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-beta\.([1-9][0-9]*)$ ]]; then
    release_core=${BASH_REMATCH[1]}
    release_number=${BASH_REMATCH[2]}
    if previous_beta=$(previous_beta_before "$release_core" "$release_number" "$release_commit"); then
      printf '%s\n' "$previous_beta"
      return
    fi
    latest_stable_before "$release_tag" "$release_commit"
    return
  fi
  fail "unsupported release tag: $release_tag"
}

main() {
  local release_tag=${1:-}
  local output=${2:-}
  local repository=${GITHUB_REPOSITORY:-caidaoli/ccLoad}
  local release_commit base_tag commit_count commit subject short_commit

  [[ -n "$release_tag" && -n "$output" && $# -eq 2 ]] || \
    fail 'usage: generate-release-notes.sh <release-tag> <output-file>'
  [[ "$repository" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || \
    fail "invalid GITHUB_REPOSITORY: $repository"

  release_commit=$(git rev-parse -q --verify "$release_tag^{commit}") || \
    fail "release tag does not exist or is not a commit: $release_tag"
  base_tag=$(resolve_base_tag "$release_tag" "$release_commit") || \
    fail "no comparison base found for $release_tag"
  git merge-base --is-ancestor "$base_tag^{commit}" "$release_commit" || \
    fail "$base_tag is not an ancestor of $release_tag"

  commit_count=$(git rev-list --count --no-merges "$base_tag..$release_tag")
  (( commit_count > 0 )) || fail "no non-merge commits exist in $base_tag..$release_tag"

  mkdir -p -- "$(dirname -- "$output")"
  {
    printf "## What's Changed\n\n"
    while IFS=$'\t' read -r commit subject; do
      short_commit=${commit:0:8}
      printf -- "- %s ([\`%s\`](https://github.com/%s/commit/%s))\n" \
        "$subject" "$short_commit" "$repository" "$commit"
    done < <(git log --reverse --no-merges --format='%H%x09%s' "$base_tag..$release_tag")
    printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' \
      "$repository" "$base_tag" "$release_tag"
  } > "$output"

  printf 'Release notes: %s..%s -> %s\n' "$base_tag" "$release_tag" "$output"
}

if [[ "${1:-}" == --self-test ]]; then
  self_test
  exit 0
fi

main "$@"
