#!/bin/sh
# Bundle cursor-agent into the runtime image. Cursor's CLI is a glibc Node
# build (linux-*-gnu native addons), so this must run on Debian/Ubuntu, not Alpine.
# Default version must stay in sync with internal/cursorauth.ClientVersion.
set -eu

version="${CURSOR_AGENT_VERSION:?CURSOR_AGENT_VERSION is required}"
arch="${TARGETARCH:-$(uname -m)}"

case "$arch" in
amd64 | x86_64) cursor_arch=x64 ;;
arm64 | aarch64) cursor_arch=arm64 ;;
*)
	echo "unsupported TARGETARCH=$arch (need amd64 or arm64)" >&2
	exit 1
	;;
esac

url="https://downloads.cursor.com/lab/${version}/linux/${cursor_arch}/agent-cli-package.tar.gz"
dest="/opt/cursor-agent/share/versions/${version}"

mkdir -p "$dest" /opt/cursor-agent/bin
wget -qO /tmp/cursor-agent.tar.gz "$url"
tar -xzf /tmp/cursor-agent.tar.gz -C "$dest" --strip-components=1
rm -f /tmp/cursor-agent.tar.gz

if [ ! -x "$dest/cursor-agent" ] || [ ! -x "$dest/node" ]; then
	echo "cursor-agent package is missing executables in $dest" >&2
	ls -la "$dest" >&2
	exit 1
fi

ln -sf "$dest/cursor-agent" /opt/cursor-agent/bin/cursor-agent
ln -sf "$dest/cursor-agent" /opt/cursor-agent/bin/agent
chown -R 1001:1001 /opt/cursor-agent
