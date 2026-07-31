#!/bin/bash
# fetch-cni-plugins.sh — download the official static, CGO-free CNI plugins
# (ptp, portmap, host-local, loopback) into build/cni for the image build.
# These are what setupCNI installs so a joined node reaches NetworkReady.
set -euo pipefail
VER=${CNI_PLUGINS_VERSION:-v1.5.1}
ARCH=${ARCH:-amd64}
OUT="$(cd "$(dirname "$0")/.." && pwd)/build/cni"
mkdir -p "$OUT"
TGZ="cni-plugins-linux-${ARCH}-${VER}.tgz"
URL="https://github.com/containernetworking/plugins/releases/download/${VER}/${TGZ}"
echo "==> downloading $TGZ"
curl -fL# -o "/tmp/$TGZ" "$URL"
tar -xzf "/tmp/$TGZ" -C "$OUT" ./ptp ./portmap ./host-local ./loopback
echo "==> CNI plugins in $OUT:"
for p in ptp portmap host-local loopback; do
  printf '  %-10s %s (NEEDED=%s)\n' "$p" "$(stat -c%s "$OUT/$p") bytes" \
    "$(readelf -d "$OUT/$p" 2>/dev/null | grep -c NEEDED)"
done
echo "==> now: scripts/zero-c-initramfs.sh will bundle these"
