#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <goos> <goarch> <output>" >&2
  exit 2
fi

goos=$1
goarch=$2
output=$3
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock_file="$repo_root/third_party/cursor-sdk-bridge/bridge.lock"

read_lock() {
  sed -n "s/^$1=//p" "$lock_file"
}

version=$(read_lock version)
case "$goarch" in
  amd64) upstream_arch=x64 ;;
  arm64) upstream_arch=arm64 ;;
  *) echo "unsupported architecture: $goarch" >&2; exit 2 ;;
esac
case "$goos" in
  darwin|linux) upstream_os=$goos; entry=bin/cursor-sdk-bridge ;;
  windows) upstream_os=win32; entry=bin/cursor-sdk-bridge.exe ;;
  *) echo "unsupported operating system: $goos" >&2; exit 2 ;;
esac
if [ "$upstream_os" = win32 ] && [ "$upstream_arch" != x64 ]; then
  echo "unsupported bridge platform: $goos/$goarch" >&2
  exit 2
fi

hash_key="sha256_${upstream_os}_${upstream_arch}"
expected=$(read_lock "$hash_key")
if [ -z "$version" ] || [ -z "$expected" ]; then
  echo "missing $hash_key in $lock_file" >&2
  exit 1
fi

archive="cursor-sdk-bridge-standalone-${upstream_os}-${upstream_arch}.tar.gz"
url="https://github.com/cursor/sdk-bridge/releases/download/${version}/${archive}"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ccload-cursor-bridge.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

curl --fail --location --retry 3 --output "$tmp_dir/$archive" "$url"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')
fi
if [ "$actual" != "$expected" ]; then
  echo "checksum mismatch for $archive" >&2
  exit 1
fi

mkdir -p "$(dirname -- "$output")"
tar -xOzf "$tmp_dir/$archive" "$entry" > "$output"
chmod 0755 "$output"
