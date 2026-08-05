#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: release.sh [beta|preview|stable] [--dry-run|--publish] [--commit-message <subject>]
       release.sh --self-test

The default channel is beta. Stable releases require the explicit stable argument.
When the worktree is dirty, --commit-message must be a single-line Conventional Commit subject.
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

self_test_root=

is_conventional_commit_message() {
  local message=$1
  local pattern='^[[:alnum:]_-]+(\([^()[:space:]]+\))?(!)?:[[:space:]].+$'
  [[ -n "$message" ]] && [[ "$message" != *$'\n'* ]] && [[ "$message" =~ $pattern ]]
}

branch_relation() {
  local head_sha=$1
  local origin_sha=$2
  if [[ "$head_sha" == "$origin_sha" ]]; then
    printf 'equal\n'
  elif git merge-base --is-ancestor "$origin_sha" "$head_sha"; then
    printf 'local-ahead\n'
  elif git merge-base --is-ancestor "$head_sha" "$origin_sha"; then
    printf 'remote-ahead\n'
  else
    printf 'diverged\n'
  fi
}

derive_release_plan() {
  local pending_message=$1
  local pending_subject=${pending_message%%$'\n'*}
  local candidate candidate_commit candidate_major candidate_minor candidate_patch candidate_number
  local release_base_tag stable_major stable_minor stable_patch target_patch target_beta_number
  local large_beta_change=false

  latest_stable_tag=
  while IFS= read -r candidate; do
    if [[ "$candidate" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      latest_stable_tag=$candidate
      break
    fi
  done < <(git tag --list 'v*' --sort=-version:refname)

  if [[ -n "$latest_stable_tag" ]]; then
    base_version=${latest_stable_tag#v}
  else
    base_version=0.0.0
  fi
  IFS=. read -r stable_major stable_minor stable_patch <<EOF
$base_version
EOF

  latest_beta_tag=
  latest_beta_patch=-1
  latest_beta_number=0
  if [[ "$channel" == beta ]] && [[ -n "$latest_stable_tag" ]]; then
    while IFS= read -r candidate; do
      if [[ ! "$candidate" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)-beta\.([1-9][0-9]*)$ ]]; then
        continue
      fi
      candidate_major=${BASH_REMATCH[1]}
      candidate_minor=${BASH_REMATCH[2]}
      candidate_patch=${BASH_REMATCH[3]}
      candidate_number=${BASH_REMATCH[4]}
      candidate_commit=$(git rev-list -n 1 "$candidate")
      git merge-base --is-ancestor "$latest_stable_tag" "$candidate_commit" || continue
      if (( candidate_major != stable_major || candidate_minor != stable_minor )); then
        fail "invalid Beta tag after $latest_stable_tag: $candidate changes the stable major/minor lane"
      fi
      git merge-base --is-ancestor "$candidate_commit" HEAD || continue
      if (( candidate_patch <= stable_patch )); then
        continue
      fi
      if (( candidate_patch > latest_beta_patch )) || \
         (( candidate_patch == latest_beta_patch && candidate_number > latest_beta_number )); then
        latest_beta_tag=$candidate
        latest_beta_patch=$candidate_patch
        latest_beta_number=$candidate_number
      fi
    done < <(git tag --list 'v*-beta.*')
  fi

  if [[ "$channel" == stable ]]; then
    release_base_tag=$latest_stable_tag
  else
    release_base_tag=${latest_beta_tag:-$latest_stable_tag}
  fi
  if [[ -n "$release_base_tag" ]]; then
    commit_range="$release_base_tag..HEAD"
  else
    commit_range=HEAD
  fi

  commit_count=$(git rev-list --count "$commit_range")
  subjects=$(git log "$commit_range" --format='%s')
  messages=$(git log "$commit_range" --format='%s%n%b')
  if [[ -n "$pending_message" ]]; then
    commit_count=$((commit_count + 1))
    subjects=$(printf '%s\n%s' "$pending_subject" "$subjects")
    messages=$(printf '%s\n%s' "$pending_message" "$messages")
  fi
  [[ "$commit_count" -gt 0 ]] || fail "no commits exist after ${release_base_tag:-repository start}"

  if printf '%s\n' "$messages" | grep -Eq '(^|[[:space:]])BREAKING([ -]CHANGE)?:' || \
     printf '%s\n' "$subjects" | grep -Eq '^[[:alnum:]_-]+(\([^)]*\))?!:'; then
    if [[ "$channel" == stable ]]; then
      bump='major'
    else
      large_beta_change=true
    fi
  elif printf '%s\n' "$subjects" | grep -Eq '^feat(\([^)]*\))?!?:'; then
    if [[ "$channel" == stable ]]; then
      bump='minor'
    else
      large_beta_change=true
    fi
  else
    bump='patch'
  fi

  if [[ "$channel" == stable ]]; then
    next_core=$(next_core_version "$base_version" "$bump")
    release_tag="v$next_core"
    if git rev-parse -q --verify "refs/tags/$release_tag" >/dev/null; then
      fail "tag already exists: $release_tag"
    fi
    return 0
  fi

  if [[ -z "$latest_beta_tag" ]]; then
    target_patch=$((stable_patch + 1))
    target_beta_number=1
    bump='beta-patch'
  elif [[ "$large_beta_change" == true ]]; then
    target_patch=$((latest_beta_patch + 1))
    target_beta_number=1
    bump='beta-patch'
  else
    target_patch=$latest_beta_patch
    target_beta_number=$((latest_beta_number + 1))
    bump='beta-sequence'
  fi
  release_tag="v${stable_major}.${stable_minor}.${target_patch}-beta.${target_beta_number}"
  if git rev-parse -q --verify "refs/tags/$release_tag" >/dev/null; then
    fail "tag already exists: $release_tag"
  fi
}

self_test() {
  require_command git
  assert_equal 2.0.0 "$(next_core_version 1.2.3 major)" major
  assert_equal 1.3.0 "$(next_core_version 1.2.3 minor)" minor
  assert_equal 1.2.4 "$(next_core_version 1.2.3 patch)" patch
  is_conventional_commit_message 'feat(release): prepare revision' || fail "valid Conventional Commit subject was rejected"
  if is_conventional_commit_message 'prepare release'; then
    fail "invalid Conventional Commit subject was accepted"
  fi

  local script_path remote_dir work_dir other_dir stub_dir stub_path before_head before_remote
  local dry_run_output publish_output published_head published_tag_target remote_ahead_error
  local small_beta_output large_beta_output stable_output invalid_beta_error
  local stable_commit stable_tree invalid_beta_commit
  script_path=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
  self_test_root=$(mktemp -d)
  trap 'rm -rf -- "$self_test_root"' EXIT
  remote_dir="$self_test_root/caidaoli/ccLoad.git"
  work_dir="$self_test_root/work"
  stub_dir="$self_test_root/bin"
  mkdir -p "$(dirname "$remote_dir")" "$stub_dir"

  git init --bare "$remote_dir" >/dev/null
  git clone "$remote_dir" "$work_dir" >/dev/null 2>&1
  git -C "$work_dir" config user.name 'Release Self Test'
  git -C "$work_dir" config user.email 'release-self-test@example.invalid'
  printf 'base\n' >"$work_dir/release-test.txt"
  git -C "$work_dir" add release-test.txt
  git -C "$work_dir" commit -m 'chore: initial release' >/dev/null
  git -C "$work_dir" tag -a v1.0.0 -m 'Release v1.0.0'
  git -C "$work_dir" push origin master refs/tags/v1.0.0 >/dev/null 2>&1
  printf 'local commit\n' >>"$work_dir/release-test.txt"
  git -C "$work_dir" add release-test.txt
  git -C "$work_dir" commit -m 'fix: local ahead' >/dev/null
  printf 'pending change\n' >>"$work_dir/release-test.txt"

  stub_path="$stub_dir/tool-stub"
  cat >"$stub_path" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$(basename "$0")" != gh ]]; then
  exit 0
fi
case "${1:-} ${2:-}" in
  'auth status'|'run watch')
    exit 0
    ;;
  'run list')
    printf '4242\n'
    ;;
  'release view')
    case " $* " in
      *' --json isPrerelease '*) printf 'true\n' ;;
      *' --json url '*) printf 'https://example.invalid/releases/test\n' ;;
      *) exit 1 ;;
    esac
    ;;
  *)
    exit 1
    ;;
esac
EOF
  chmod +x "$stub_path"
  for command_name in go make node golangci-lint gh; do
    ln -s tool-stub "$stub_dir/$command_name"
  done

  before_head=$(git -C "$work_dir" rev-parse HEAD)
  before_remote=$(git --git-dir="$remote_dir" rev-parse refs/heads/master)
  dry_run_output=$(cd "$work_dir" && PATH="$stub_dir:$PATH" "$script_path" beta --dry-run \
    --commit-message 'feat(release): prepare revision')
  assert_equal "$before_head" "$(git -C "$work_dir" rev-parse HEAD)" 'dry-run local HEAD'
  assert_equal "$before_remote" "$(git --git-dir="$remote_dir" rev-parse refs/heads/master)" 'dry-run remote master'
  [[ -n "$(git -C "$work_dir" status --porcelain)" ]] || fail "dry-run unexpectedly cleaned the worktree"
  [[ "$dry_run_output" == *'target tag:      v1.0.1-beta.1'* ]] || fail "dry-run derived the wrong first Beta Tag"

  publish_output=$(cd "$work_dir" && PATH="$stub_dir:$PATH" "$script_path" beta --publish \
    --commit-message 'feat(release): prepare revision')
  published_head=$(git -C "$work_dir" rev-parse HEAD)
  published_tag_target=$(git --git-dir="$remote_dir" rev-parse 'refs/tags/v1.0.1-beta.1^{}')
  assert_equal 'feat(release): prepare revision' "$(git -C "$work_dir" log -1 --format=%s)" 'automatic commit subject'
  assert_equal "$published_head" "$(git --git-dir="$remote_dir" rev-parse refs/heads/master)" 'pushed master'
  assert_equal "$published_head" "$published_tag_target" 'published Tag target'
  assert_equal '' "$(git -C "$work_dir" status --porcelain)" 'published worktree'
  [[ "$publish_output" == *'Container: skipped for Beta'* ]] || fail "publish did not report the Beta container policy"

  printf 'next change\n' >>"$work_dir/release-test.txt"
  small_beta_output=$(cd "$work_dir" && "$script_path" beta --dry-run \
    --commit-message 'fix: small Beta change')
  [[ "$small_beta_output" == *'target tag:      v1.0.1-beta.2'* ]] || \
    fail "small change did not increment only the Beta sequence"
  large_beta_output=$(cd "$work_dir" && "$script_path" beta --dry-run \
    --commit-message 'feat: large Beta change')
  [[ "$large_beta_output" == *'target tag:      v1.0.2-beta.1'* ]] || \
    fail "large change did not increment only the Beta patch"
  stable_output=$(cd "$work_dir" && "$script_path" stable --dry-run \
    --commit-message 'feat: stable minor change')
  [[ "$stable_output" == *'target tag:      v1.1.0'* ]] || \
    fail "stable release did not apply the minor increment"

  stable_commit=$(git -C "$work_dir" rev-list -n 1 v1.0.0)
  stable_tree=$(git -C "$work_dir" rev-parse 'v1.0.0^{tree}')
  invalid_beta_commit=$(printf 'invalid cross-minor Beta\n' | \
    git -C "$work_dir" commit-tree "$stable_tree" -p "$stable_commit")
  git -C "$work_dir" tag -a v1.1.0-beta.1 "$invalid_beta_commit" -m 'Invalid cross-minor Beta'
  if git -C "$work_dir" merge-base --is-ancestor "$invalid_beta_commit" HEAD; then
    fail "invalid Beta fixture unexpectedly belongs to the current HEAD history"
  fi
  if invalid_beta_error=$(cd "$work_dir" && "$script_path" beta --dry-run \
    --commit-message 'fix: invalid lane check' 2>&1); then
    fail "dry-run accepted a Beta Tag that changed the stable minor lane"
  fi
  [[ "$invalid_beta_error" == *'changes the stable major/minor lane'* ]] || \
    fail "dry-run reported the wrong invalid Beta lane error"
  git -C "$work_dir" tag -d v1.1.0-beta.1 >/dev/null

  other_dir="$self_test_root/other"
  git clone "$remote_dir" "$other_dir" >/dev/null 2>&1
  git -C "$other_dir" config user.name 'Release Self Test'
  git -C "$other_dir" config user.email 'release-self-test@example.invalid'
  printf 'remote ahead\n' >>"$other_dir/release-test.txt"
  git -C "$other_dir" add release-test.txt
  git -C "$other_dir" commit -m 'fix: remote ahead' >/dev/null
  git -C "$other_dir" push origin master >/dev/null 2>&1
  if remote_ahead_error=$(cd "$work_dir" && "$script_path" beta --dry-run 2>&1); then
    fail "dry-run accepted a remote-ahead master"
  fi
  [[ "$remote_ahead_error" == *'origin/master is ahead of local master'* ]] || \
    fail "dry-run reported the wrong remote-ahead error"
  assert_equal "$published_head" "$(git -C "$work_dir" rev-parse HEAD)" 'remote-ahead local HEAD'

  rm -rf -- "$self_test_root"
  self_test_root=
  trap - EXIT
  printf 'PASS: release script self-test\n'
}

channel=beta
mode=dry-run
channel_argument_seen=false
commit_message=

while (( $# > 0 )); do
  case "$1" in
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
    --commit-message)
      (( $# >= 2 )) || fail "--commit-message requires a value"
      commit_message=$2
      shift
      ;;
    --commit-message=*)
      commit_message=${1#*=}
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
      fail "unsupported argument: $1"
      ;;
  esac
  shift
done

require_command git

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || fail "not inside a Git repository"
cd "$repo_root"

origin_url=$(git remote get-url origin 2>/dev/null) || fail "origin remote is missing"
if [[ ! "$origin_url" =~ (^|[:/])caidaoli/ccLoad(\.git)?$ ]]; then
  fail "origin is not caidaoli/ccLoad: $origin_url"
fi

branch=$(git branch --show-current)
[[ "$branch" == master ]] || fail "current branch must be master, got ${branch:-detached HEAD}"

git fetch --prune origin master
git fetch --tags origin

head_sha=$(git rev-parse HEAD)
origin_sha=$(git rev-parse refs/remotes/origin/master)
relation=$(branch_relation "$head_sha" "$origin_sha")
case "$relation" in
  equal|local-ahead) ;;
  remote-ahead) fail "origin/master is ahead of local master; reconcile it before release" ;;
  diverged) fail "local master has diverged from origin/master; reconcile it before release" ;;
  *) fail "unknown branch relation: $relation" ;;
esac

worktree_status=$(git status --porcelain --untracked-files=all)
pending_message=
if [[ -n "$worktree_status" ]]; then
  [[ -n "$commit_message" ]] || fail "working tree has changes; provide --commit-message with a Conventional Commit subject"
  is_conventional_commit_message "$commit_message" || \
    fail "invalid --commit-message; expected a single-line Conventional Commit subject"
  [[ -z "$(git diff --name-only --diff-filter=U)" ]] || fail "working tree has unmerged paths"
  git var GIT_AUTHOR_IDENT >/dev/null 2>&1 || fail "Git author identity is not configured"
  pending_message=$commit_message
fi

derive_release_plan "$pending_message"
planned_release_tag=$release_tag
planned_bump=$bump
planned_commit_count=$commit_count

if [[ -n "$pending_message" ]]; then
  worktree_action="commit as $pending_message"
else
  worktree_action=clean
fi
if [[ "$relation" == local-ahead ]] || [[ -n "$pending_message" ]]; then
  branch_action='push verified master'
else
  branch_action='already synchronized'
fi

cat <<EOF
Release plan
  channel:         $channel
  previous stable: ${latest_stable_tag:-none}
  previous beta:   ${latest_beta_tag:-none}
  commits:         $commit_count
  bump:            $bump
  target tag:      $release_tag
  mode:            $mode
  worktree:        $worktree_action
  branch:          $branch_action
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

if [[ -n "$pending_message" ]]; then
  git add -A
  git diff --cached --check
  if git diff --cached --quiet; then
    fail "working tree changed but there is nothing to commit"
  fi
  git commit -m "$pending_message"
fi

derive_release_plan ""
[[ "$release_tag" == "$planned_release_tag" ]] || \
  fail "release Tag changed after automatic commit: got $release_tag, planned $planned_release_tag"
[[ "$bump" == "$planned_bump" ]] || \
  fail "semantic version bump changed after automatic commit: got $bump, planned $planned_bump"
[[ "$commit_count" == "$planned_commit_count" ]] || \
  fail "release commit count changed after automatic commit: got $commit_count, planned $planned_commit_count"
head_sha=$(git rev-parse HEAD)

go test -tags sonic ./internal/...
make verify-web
make build
golangci-lint config verify
golangci-lint run ./...
git diff --check

if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  fail "verification changed the working tree; refusing to push or tag"
fi

git fetch --prune origin master
origin_sha=$(git rev-parse refs/remotes/origin/master)
relation=$(branch_relation "$head_sha" "$origin_sha")
case "$relation" in
  equal) ;;
  local-ahead) git push origin 'HEAD:refs/heads/master' ;;
  remote-ahead) fail "origin/master advanced during verification; refusing to push or tag" ;;
  diverged) fail "origin/master diverged during verification; refusing to push or tag" ;;
  *) fail "unknown branch relation after verification: $relation" ;;
esac

git fetch --prune origin master
origin_sha=$(git rev-parse refs/remotes/origin/master)
[[ "$head_sha" == "$origin_sha" ]] || fail "local HEAD does not match origin/master after branch push"

git fetch --tags origin
derive_release_plan ""
[[ "$release_tag" == "$planned_release_tag" ]] || \
  fail "release Tag changed while verification was running: got $release_tag, planned $planned_release_tag"

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
