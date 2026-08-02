#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
entrypoint="$repo_root/docker/entrypoint.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

make_fake_tools() {
  case_dir=$1
  mkdir -p "$case_dir/bin"

  cat > "$case_dir/bin/uname" <<'EOF'
#!/bin/sh
printf 'x86_64\n'
EOF

  cat > "$case_dir/bin/curl" <<'EOF'
#!/bin/sh
set -eu

output=
url=
progress_bar=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    --connect-timeout|--max-time|--retry|--retry-delay)
      shift 2
      ;;
    --progress-bar)
      progress_bar=true
      shift
      ;;
    -*)
      shift
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done

[ -n "$output" ] || exit 2
[ -n "$url" ] || exit 2
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
if [ "$progress_bar" = true ]; then
  printf 'fake curl progress: 100%%\n' >&2
fi

case "$url" in
  https://v4.gh-proxy.org/*) source=ghproxy_v4 ;;
  https://gh-proxy.com/*) source=ghproxy_com ;;
  https://ghp.keleyaa.com/*) source=keleyaa ;;
  https://github.com/*) source=github ;;
  https://mirror.example/*) source=custom ;;
  *) source=unknown ;;
esac

case "$url" in
  */checksums.txt) kind=checksums ;;
  *) kind=asset ;;
esac

fail_source=false
if [ "${FAKE_FAIL_SOURCE:-}" = "$source" ]; then
  fail_source=true
elif [ "${FAKE_FAIL_SOURCE:-}" = "all-mirrors" ]; then
  case "$source" in
    ghproxy_v4|ghproxy_com|keleyaa) fail_source=true ;;
  esac
fi
if [ "$fail_source" = true ] && [ "${FAKE_FAIL_KIND:-}" = "$kind" ]; then
  exit 22
fi

if [ "$kind" = "asset" ]; then
  cp "$FAKE_RELEASE_ASSET" "$output"
  exit 0
fi

if [ "${FAKE_BAD_CHECKSUM_SOURCE:-}" = "$source" ] || [ "${FAKE_BAD_CHECKSUM_SOURCE:-}" = "all" ]; then
  hash=0000000000000000000000000000000000000000000000000000000000000000
else
  hash=$(sha256sum "$FAKE_RELEASE_ASSET" | awk '{print $1}')
fi
printf '%s  ccload-linux-amd64\n' "$hash" > "$output"
EOF

  chmod +x "$case_dir/bin/uname" "$case_dir/bin/curl"
}

make_fixture_asset() {
  path=$1
  cat > "$path" <<'EOF'
#!/bin/sh
printf 'downloaded\n' >> "$CCLOAD_EXEC_LOG"
EOF
  chmod +x "$path"
}

make_old_binary() {
  path=$1
  mkdir -p "$(dirname "$path")"
  cat > "$path" <<'EOF'
#!/bin/sh
printf 'old\n' >> "$CCLOAD_EXEC_LOG"
EOF
  chmod +x "$path"
}

run_entrypoint() {
  case_dir=$1
  custom_base=${2:-}

  export PATH="$case_dir/bin:$PATH"
  export CCLOAD_HOME="$case_dir/home"
  export CCLOAD_BIN="$case_dir/home/ccload"
  export CCLOAD_EXEC_LOG="$case_dir/exec.log"
  export FAKE_CURL_LOG="$case_dir/curl.log"
  export FAKE_RELEASE_ASSET="$case_dir/release-asset"
  if [ -n "$custom_base" ]; then
    export CCLOAD_RELEASE_BASE_URL=$custom_base
  else
    unset CCLOAD_RELEASE_BASE_URL || true
  fi

  sh "$entrypoint"
}

test_default_tries_three_mirrors_then_github() {
  case_dir="$test_root/default-fallback"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  export FAKE_FAIL_SOURCE=all-mirrors
  export FAKE_FAIL_KIND=asset
  unset FAKE_BAD_CHECKSUM_SOURCE || true

  run_entrypoint "$case_dir"

  asset_path="https://github.com/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64"
  [ "$(sed -n '1p' "$case_dir/curl.log")" = "https://v4.gh-proxy.org/$asset_path" ] ||
    fail "first default request did not use v4.gh-proxy.org"
  [ "$(sed -n '2p' "$case_dir/curl.log")" = "https://gh-proxy.com/$asset_path" ] ||
    fail "second default request did not use gh-proxy.com"
  [ "$(sed -n '3p' "$case_dir/curl.log")" = "https://ghp.keleyaa.com/$asset_path" ] ||
    fail "third default request did not use ghp.keleyaa.com"
  [ "$(sed -n '4p' "$case_dir/curl.log")" = "$asset_path" ] ||
    fail "GitHub was not the final fallback"
  [ "$(cat "$case_dir/exec.log")" = "downloaded" ] || fail "fallback binary was not executed"
}

test_custom_source_does_not_fallback() {
  case_dir="$test_root/custom-only"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  make_old_binary "$case_dir/home/ccload"
  export FAKE_FAIL_SOURCE=custom
  export FAKE_FAIL_KIND=asset
  unset FAKE_BAD_CHECKSUM_SOURCE || true

  run_entrypoint "$case_dir" "https://mirror.example/caidaoli/ccLoad/releases/latest/download"

  if grep -q \
    -e '^https://v4.gh-proxy.org/' \
    -e '^https://gh-proxy.com/' \
    -e '^https://ghp.keleyaa.com/' \
    -e '^https://github.com/' \
    "$case_dir/curl.log"; then
    fail "custom source unexpectedly fell back to a built-in source"
  fi
  [ "$(cat "$case_dir/exec.log")" = "old" ] || fail "existing binary was not preserved after custom source failure"
}

test_bad_checksums_never_replace_existing_binary() {
  case_dir="$test_root/bad-checksums"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  make_old_binary "$case_dir/home/ccload"
  unset FAKE_FAIL_SOURCE FAKE_FAIL_KIND || true
  export FAKE_BAD_CHECKSUM_SOURCE=all

  run_entrypoint "$case_dir"

  [ "$(cat "$case_dir/exec.log")" = "old" ] || fail "bad checksum replaced the existing binary"
  grep -q '^https://v4.gh-proxy.org/' "$case_dir/curl.log" || fail "v4.gh-proxy.org was not attempted"
  grep -q '^https://gh-proxy.com/' "$case_dir/curl.log" || fail "gh-proxy.com was not attempted"
  grep -q '^https://ghp.keleyaa.com/' "$case_dir/curl.log" || fail "ghp.keleyaa.com was not attempted"
  grep -q '^https://github.com/' "$case_dir/curl.log" || fail "GitHub was not attempted after mirror checksum failures"
}

test_invalid_custom_source_fails_fast() {
  case_dir="$test_root/invalid-custom"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  make_old_binary "$case_dir/home/ccload"
  unset FAKE_FAIL_SOURCE FAKE_FAIL_KIND FAKE_BAD_CHECKSUM_SOURCE || true

  if run_entrypoint "$case_dir" "https://mirror.example/caidaoli/ccLoad/releases/download"; then
    fail "invalid custom source unexpectedly started ccLoad"
  fi
  [ ! -e "$case_dir/exec.log" ] || fail "invalid custom source executed the existing binary"
}

test_download_reports_full_url_and_progress() {
  case_dir="$test_root/download-output"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  unset FAKE_FAIL_SOURCE FAKE_FAIL_KIND FAKE_BAD_CHECKSUM_SOURCE || true

  run_entrypoint "$case_dir" "https://mirror.example/caidaoli/ccLoad/releases/latest/download" \
    2> "$case_dir/stderr.log"

  asset_url="https://mirror.example/caidaoli/ccLoad/releases/latest/download/ccload-linux-amd64"
  grep -Fq "download URL: $asset_url" "$case_dir/stderr.log" ||
    fail "full asset download URL was not reported"
  grep -Fq 'fake curl progress: 100%' "$case_dir/stderr.log" ||
    fail "curl progress was not visible"
}

test_default_tries_three_mirrors_then_github
test_custom_source_does_not_fallback
test_bad_checksums_never_replace_existing_binary
test_invalid_custom_source_fails_fast
test_download_reports_full_url_and_progress
printf 'PASS: docker entrypoint release source behavior\n'
