#!/bin/bash
# rebuild-initramfs.sh — rebuild test/initramfs/build/initramfs.cpio.gz by
# re-building the astrokube-init Go binary and swapping it into the EXISTING
# staged initramfs (which already carries runc + the glibc closure + pod specs).
# This avoids re-deriving the whole stage from scratch for an init-only change.
#
# Usage: astrokube/rebuild-initramfs.sh [path-to-kubelet-repo]
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/
KUBELET=${1:-$(cd "$(dirname "$0")/.." && pwd)}
BUILD=test/initramfs/build
CPIO="$BUILD/initramfs.cpio.gz"
WORK=$(mktemp -d /tmp/astrokube-initramfs.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

echo "==> building astrokube-init (static, CGO disabled) from $KUBELET"
( cd "$KUBELET" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o "$WORK/init-bin" ./cmd/astrokube-init )

echo "==> extracting existing initramfs $CPIO"
mkdir -p "$WORK/root"
( cd "$WORK/root" && gzip -dc "$OLDPWD/$CPIO" | cpio -idm --quiet )

# /sbin/init -> ../usr/bin/kubelet, so overwrite that binary.
echo "==> swapping in fresh init at usr/bin/kubelet"
install -m 0755 "$WORK/init-bin" "$WORK/root/usr/bin/kubelet"
# Keep the legacy copy in sync too, in case anything refers to it.
install -m 0755 "$WORK/init-bin" "$WORK/root/usr/bin/astrokube-init"

echo "==> repacking $CPIO"
( cd "$WORK/root" && find . -print0 \
    | cpio --null -o -H newc --owner=0:0 --quiet | gzip -9 ) > "$CPIO"

echo "==> initramfs done:"; ls -lh "$CPIO"

# The initramfs is embedded into the bootable ISO at `cargo osdk build` time, so
# the ISO must be rebuilt for an initramfs change to take effect at boot.
if [ "${SKIP_ISO:-0}" != "1" ]; then
  echo "==> rebuilding ISO in the dev container (embeds the new initramfs)"
  docker start astrokube >/dev/null 2>&1 || true
  docker exec astrokube bash -lc \
    'git config --global --add safe.directory /root/asterinas; cd /root/asterinas/kernel && cargo osdk build --release --strip-elf --grub-boot-protocol=multiboot2' \
    2>&1 | tail -3
  echo "==> ISO:"; ls -lh target/osdk/aster-kernel-osdk-bin.iso
fi
