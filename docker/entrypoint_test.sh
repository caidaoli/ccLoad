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
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    --connect-timeout|--max-time|--retry|--retry-delay)
      shift 2
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

case "$url" in
  https://ghproxy.net/*) source=ghproxy ;;
  https://github.com/*) source=github ;;
  https://mirror.example/*) source=custom ;;
  *) source=unknown ;;
esac

case "$url" in
  */checksums.txt) kind=checksums ;;
  *) kind=asset ;;
esac

if [ "${FAKE_FAIL_SOURCE:-}" = "$source" ] && [ "${FAKE_FAIL_KIND:-}" = "$kind" ]; then
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

test_default_falls_back_to_github() {
  case_dir="$test_root/default-fallback"
  make_fake_tools "$case_dir"
  make_fixture_asset "$case_dir/release-asset"
  export FAKE_FAIL_SOURCE=ghproxy
  export FAKE_FAIL_KIND=asset
  unset FAKE_BAD_CHECKSUM_SOURCE || true

  run_entrypoint "$case_dir"

  first_url=$(sed -n '1p' "$case_dir/curl.log")
  case "$first_url" in
    https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/latest/download/*) ;;
    *) fail "first default request did not use ghproxy: $first_url" ;;
  esac
  grep -q '^https://github.com/caidaoli/ccLoad/releases/latest/download/' "$case_dir/curl.log" ||
    fail "GitHub fallback was not requested"
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

  if grep -q -e '^https://ghproxy.net/' -e '^https://github.com/' "$case_dir/curl.log"; then
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
  grep -q '^https://ghproxy.net/' "$case_dir/curl.log" || fail "ghproxy was not attempted"
  grep -q '^https://github.com/' "$case_dir/curl.log" || fail "GitHub was not attempted after ghproxy checksum failure"
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

test_default_falls_back_to_github
test_custom_source_does_not_fallback
test_bad_checksums_never_replace_existing_binary
test_invalid_custom_source_fails_fast
printf 'PASS: docker entrypoint release source behavior\n'
