#!/bin/bash
# restore-full-initramfs.sh — rebuild the FULL (glibc) initramfs used by the
# real-cluster demo, after zero-c-initramfs.sh has stripped it. It re-derives the
# glibc closure that the dynamically-linked upstream binaries (containerd, ctr,
# crictl, kubelet, shim, runc) need — straight from the Fedora host, the same way
# they were originally staged — and copies the host's runc back in.
#
# Usage: asterkube/restore-full-initramfs.sh [path-to-kubelet-repo]
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/
KUBELET=${1:-$(cd "$(dirname "$0")/.." && pwd)}
SHARE=${VIRTIOFS_SHARE:-/tmp/asterkube-vfs}
BUILD=test/initramfs/build
CPIO="$BUILD/initramfs.cpio.gz"
WORK=$(mktemp -d /tmp/asterkube-fullinit.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

echo "==> building asterkube-init (static, CGO_ENABLED=0) from $KUBELET"
( cd "$KUBELET" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o "$WORK/init-bin" ./cmd/asterkube-init )

echo "==> extracting current initramfs"
mkdir -p "$WORK/root"
( cd "$WORK/root" && gzip -dc "$OLDPWD/$CPIO" | cpio -idm --quiet )
install -m 0755 "$WORK/init-bin" "$WORK/root/usr/bin/kubelet"
install -m 0755 "$WORK/init-bin" "$WORK/root/usr/bin/asterkube-init"

echo "==> restoring the host runc"
install -D -m 0755 /usr/bin/runc "$WORK/root/usr/bin/runc"

echo "==> deriving the glibc closure from the dynamic binaries"
mkdir -p "$WORK/root/lib64"
bins="/usr/bin/runc"
for b in containerd ctr crictl containerd-shim-runc-v2 kubelet; do
  [ -f "$SHARE/$b" ] && bins="$bins $SHARE/$b"
done
# Copy every shared object the binaries pull in, plus the ELF interpreter.
{ for b in $bins; do ldd "$b" 2>/dev/null | awk '/=>/{print $3} /ld-linux/{print $1}'; done; } \
  | grep -E '^/' | sort -u | while read -r so; do
    install -m 0755 "$so" "$WORK/root/lib64/$(basename "$so")"
done
echo "    staged $(ls "$WORK/root/lib64" | wc -l) shared objects into /lib64"

echo "==> repacking $CPIO"
( cd "$WORK/root" && find . -print0 \
    | cpio --null -o -H newc --owner=0:0 --quiet | gzip -9 ) > "$CPIO"
echo "==> initramfs done:"; ls -lh "$CPIO"

if [ "${SKIP_ISO:-0}" != "1" ]; then
  echo "==> rebuilding ISO in the dev container"
  docker start asterkube >/dev/null 2>&1 || true
  docker exec asterkube bash -lc \
    'git config --global --add safe.directory /root/asterinas; cd /root/asterinas/kernel && cargo osdk build --release --strip-elf --grub-boot-protocol=multiboot2' \
    2>&1 | tail -2
fi
