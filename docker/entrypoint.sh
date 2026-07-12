#!/bin/sh
set -eu

: "${CCLOAD_HOME:=/app}"
: "${CCLOAD_BIN:=$CCLOAD_HOME/ccload}"
: "${CCLOAD_REPO:=caidaoli/ccLoad}"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S %z')" "$*" >&2
}

release_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      printf 'amd64'
      ;;
    aarch64|arm64)
      printf 'arm64'
      ;;
    *)
      log "unsupported architecture: $(uname -m)"
      return 1
      ;;
  esac
}

download() {
  url=$1
  output=$2

  curl -fsSL \
    --connect-timeout 15 \
    --max-time 300 \
    --retry 3 \
    --retry-delay 2 \
    -o "$output" \
    "$url"
}

install_from_source() {
  base_url=$1
  asset=$2
  source_name=$3
  install_tmp_dir=$(mktemp -d) || return 1
  binary_tmp="$install_tmp_dir/$asset"
  checksums_tmp="$install_tmp_dir/checksums.txt"

  log "downloading $asset from $source_name"
  if ! download "$base_url/$asset" "$binary_tmp"; then
    log "$source_name asset download failed"
    rm -rf "$install_tmp_dir"
    return 1
  fi
  if ! download "$base_url/checksums.txt" "$checksums_tmp"; then
    log "$source_name checksums download failed"
    rm -rf "$install_tmp_dir"
    return 1
  fi

  checksum_line=$(
    grep -E "^[0-9a-fA-F]{64}[[:space:]]+\*?$asset$" "$checksums_tmp" | head -n 1 || true
  )
  if [ -z "$checksum_line" ]; then
    log "$source_name checksum entry not found for $asset"
    rm -rf "$install_tmp_dir"
    return 1
  fi
  if ! printf '%s\n' "$checksum_line" | (cd "$install_tmp_dir" && sha256sum -c -); then
    log "$source_name checksum verification failed for $asset"
    rm -rf "$install_tmp_dir"
    return 1
  fi

  chmod 0755 "$binary_tmp"
  mkdir -p "$(dirname "$CCLOAD_BIN")"
  mv -f "$binary_tmp" "$CCLOAD_BIN"
  rm -rf "$install_tmp_dir"
}

install_latest_release() {
  arch=$(release_arch) || return 1
  asset="ccload-linux-$arch"

  if [ -n "${CCLOAD_RELEASE_BASE_URL:-}" ]; then
    custom_base=$(printf '%s' "$CCLOAD_RELEASE_BASE_URL" | sed 's:/*$::')
    case "$custom_base" in
      */releases/latest/download) ;;
      *)
        log "CCLOAD_RELEASE_BASE_URL must end with /releases/latest/download"
        return 2
        ;;
    esac
    install_from_source "$custom_base" "$asset" "custom release source"
    return
  fi

  ghproxy_base="https://ghproxy.net/https://github.com/$CCLOAD_REPO/releases/latest/download"
  github_base="https://github.com/$CCLOAD_REPO/releases/latest/download"
  install_from_source "$ghproxy_base" "$asset" "ghproxy.net" ||
    install_from_source "$github_base" "$asset" "github.com"
}

main() {
  mkdir -p "$CCLOAD_HOME/data"

  if install_latest_release; then
    :
  else
    install_status=$?
    if [ "$install_status" -eq 2 ]; then
      exit 1
    fi
    if [ -x "$CCLOAD_BIN" ]; then
      log "all release downloads failed; using existing binary"
    else
      log "failed to prepare ccLoad binary"
      exit 1
    fi
  fi

  exec "$CCLOAD_BIN" "$@"
}

main "$@"
