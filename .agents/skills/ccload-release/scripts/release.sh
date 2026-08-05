#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: release.sh [beta|preview|stable] [--dry-run|--publish]
       release.sh --self-test

The default channel is beta. Stable releases require the explicit stable argument.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

next_core_version() {
  local base=$1
  local bump=$2
  local major minor patch
  IFS=. read -r major minor patch <<EOF
$base
EOF
  case "$bump" in
    major)
      printf '%d.0.0\n' "$((major + 1))"
      ;;
    minor)
      printf '%d.%d.0\n' "$major" "$((minor + 1))"
      ;;
    patch)
      printf '%d.%d.%d\n' "$major" "$minor" "$((patch + 1))"
      ;;
    *)
      fail "invalid bump: $bump"
      ;;
  esac
}

assert_equal() {
  local want=$1
  local got=$2
  local label=$3
  if [[ "$got" != "$want" ]]; then
    fail "$label: got $got, want $want"
  fi
}

self_test() {
  assert_equal 2.0.0 "$(next_core_version 1.2.3 major)" major
  assert_equal 1.3.0 "$(next_core_version 1.2.3 minor)" minor
  assert_equal 1.2.4 "$(next_core_version 1.2.3 patch)" patch
  printf 'PASS: release script self-test\n'
}

channel=beta
mode=dry-run
channel_argument_seen=false

for argument in "$@"; do
  case "$argument" in
    beta|preview)
      if [[ "$channel_argument_seen" == true ]]; then
        fail "release channel may only be specified once"
      fi
      channel=beta
      channel_argument_seen=true
      ;;
    stable)
      if [[ "$channel_argument_seen" == true ]]; then
        fail "release channel may only be specified once"
      fi
      channel=stable
      channel_argument_seen=true
      ;;
    --dry-run)
      mode=dry-run
      ;;
    --publish)
      mode=publish
      ;;
    --self-test)
      self_test
      exit 0
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "unsupported argument: $argument"
      ;;
  esac
done

require_command git

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || fail "not inside a Git repository"
cd "$repo_root"

origin_url=$(git remote get-url origin 2>/dev/null) || fail "origin remote is missing"
if [[ ! "$origin_url" =~ (^|[:/])caidaoli/ccLoad(\.git)?$ ]]; then
  fail "origin is not caidaoli/ccLoad: $origin_url"
fi

if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  fail "working tree is not clean"
fi

branch=$(git branch --show-current)
[[ "$branch" == master ]] || fail "current branch must be master, got ${branch:-detached HEAD}"

git fetch --prune origin master
git fetch --tags origin

head_sha=$(git rev-parse HEAD)
origin_sha=$(git rev-parse refs/remotes/origin/master)
[[ "$head_sha" == "$origin_sha" ]] || fail "local HEAD does not match origin/master"

latest_stable_tag=
while IFS= read -r candidate; do
  if [[ "$candidate" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    latest_stable_tag=$candidate
    break
  fi
done < <(git tag --list 'v*' --sort=-version:refname)

if [[ -n "$latest_stable_tag" ]]; then
  base_version=${latest_stable_tag#v}
  commit_range="$latest_stable_tag..HEAD"
else
  base_version=0.0.0
  commit_range=HEAD
fi

commit_count=$(git rev-list --count "$commit_range")
[[ "$commit_count" -gt 0 ]] || fail "no commits exist after ${latest_stable_tag:-repository start}"

subjects=$(git log "$commit_range" --format='%s')
messages=$(git log "$commit_range" --format='%s%n%b')
if printf '%s\n' "$messages" | grep -Eq '(^|[[:space:]])BREAKING([ -]CHANGE)?:' || \
   printf '%s\n' "$subjects" | grep -Eq '^[[:alnum:]_-]+(\([^)]*\))?!:'; then
  bump=major
elif printf '%s\n' "$subjects" | grep -Eq '^feat(\([^)]*\))?!?:'; then
  bump=minor
else
  bump=patch
fi

next_core=$(next_core_version "$base_version" "$bump")

if [[ "$channel" == stable ]]; then
  release_tag="v$next_core"
  git rev-parse -q --verify "refs/tags/$release_tag" >/dev/null && fail "tag already exists: $release_tag"
else
  escaped_core=${next_core//./\.}
  beta_number=0
  latest_beta_tag=
  while IFS= read -r candidate; do
    if [[ "$candidate" =~ ^v${escaped_core}-beta\.([1-9][0-9]*)$ ]]; then
      candidate_number=${BASH_REMATCH[1]}
      if (( candidate_number > beta_number )); then
        beta_number=$candidate_number
        latest_beta_tag=$candidate
      fi
    fi
  done < <(git tag --list "v$next_core-beta.*")
  if [[ -n "$latest_beta_tag" ]] && [[ "$(git rev-list -n 1 "$latest_beta_tag")" == "$head_sha" ]]; then
    fail "HEAD is already published as $latest_beta_tag"
  fi
  release_tag="v$next_core-beta.$((beta_number + 1))"
fi

cat <<EOF
Release plan
  channel:         $channel
  previous stable: ${latest_stable_tag:-none}
  commits:         $commit_count
  bump:            $bump
  target tag:      $release_tag
  mode:            $mode
EOF

if [[ "$mode" == dry-run ]]; then
  exit 0
fi

for command_name in go make node golangci-lint gh; do
  require_command "$command_name"
done
if [[ "$channel" == stable ]]; then
  require_command docker
fi

gh auth status >/dev/null

go test -tags sonic ./internal/...
make verify-web
make build
golangci-lint config verify
golangci-lint run ./...
git diff --check

if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  fail "verification changed the working tree; refusing to tag"
fi

git tag -a "$release_tag" -m "Release $release_tag"
git push origin "refs/tags/$release_tag"

run_id=
attempt=0
while [[ -z "$run_id" ]] && (( attempt < 60 )); do
  run_id=$(gh run list \
    --workflow release.yml \
    --event push \
    --limit 50 \
    --json databaseId,headBranch,headSha \
    --jq ".[] | select(.headBranch == \"$release_tag\" and .headSha == \"$head_sha\") | .databaseId" \
    | head -n 1)
  if [[ -z "$run_id" ]]; then
    sleep 2
  fi
  attempt=$((attempt + 1))
done
[[ -n "$run_id" ]] || fail "GitHub Actions run was not found for $release_tag"

printf 'GitHub Actions run: https://github.com/caidaoli/ccLoad/actions/runs/%s\n' "$run_id"
gh run watch "$run_id" --exit-status

is_prerelease=$(gh release view "$release_tag" --json isPrerelease --jq '.isPrerelease')
release_url=$(gh release view "$release_tag" --json url --jq '.url')
if [[ "$channel" == beta ]]; then
  [[ "$is_prerelease" == true ]] || fail "$release_tag was not published as a prerelease"
  printf 'Release: %s\n' "$release_url"
  printf 'Container: skipped for Beta\n'
else
  [[ "$is_prerelease" == false ]] || fail "$release_tag was unexpectedly published as a prerelease"
  image=ghcr.io/caidaoli/ccload
  docker buildx imagetools inspect "$image:$release_tag" >/dev/null
  docker buildx imagetools inspect "$image:latest" >/dev/null
  printf 'Release: %s\n' "$release_url"
  printf 'Container: %s:%s and %s:latest\n' "$image" "$release_tag" "$image"
fi
