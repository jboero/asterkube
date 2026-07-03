#!/bin/bash
# fresh-ext2.sh — regenerate test/initramfs/build/ext2.qcow2 as a fresh, empty
# ext2 filesystem. The asterkube node is ephemeral and uses the ext2 disk for
# the kubelet root-dir; carrying it across boots is a problem because the VM
# powers off uncleanly (no fs sync), leaving half-written manager checkpoints
# (e.g. the kubelet's DRA dra_manager_state) that abort kubelet startup with
# "unexpected end of JSON input". Asterinas ext2 also returns ESTALE when
# unlinking such stale files, so the guest cannot clean them itself. Starting
# from a fresh fs each boot sidesteps both issues.
#
# Usage: asterkube/fresh-ext2.sh [size-mb]   (default 256)
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/
BUILD=test/initramfs/build
SIZE_MB=${1:-256}
mkdir -p "$BUILD"

RAW=$(mktemp /tmp/asterkube-ext2.XXXXXX.img)
trap 'rm -f "$RAW"' EXIT
dd if=/dev/zero of="$RAW" bs=1M count="$SIZE_MB" status=none
mke2fs -F -q -t ext2 "$RAW"
qemu-img convert -f raw -O qcow2 "$RAW" "$BUILD/ext2.qcow2"
echo "fresh ext2 (${SIZE_MB} MB) -> $BUILD/ext2.qcow2 ($(stat -c%s "$BUILD/ext2.qcow2") bytes qcow2)"
