#!/bin/bash
# build-qcow2.sh — produce a QCOW2 of the self-contained astrokube node image.
#
# The OSDK ISO (target/osdk/aster-kernel-osdk-bin.iso) is an isohybrid GPT image:
# it carries a protective MBR + GPT with an EFI System Partition, so OVMF boots it
# equally as a CD-ROM or as a plain disk. Since the finished node image is
# self-contained (the container runtime is baked into the initramfs — no virtio-fs
# share), a QCOW2 is just a format conversion of that same ISO; nothing else needs
# to ride along.
#
# Boot it with:  ./run-host-qemu.sh astrokube-qcow2
#
# Usage: astrokube/build-qcow2.sh
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/

ISO=${ISO:-target/osdk/aster-kernel-osdk-bin.iso}
# target/osdk is owned by the build container (root); write the qcow2 to the
# host-writable build dir alongside the other qcow2 artifacts.
QCOW=${QCOW:-test/initramfs/build/astrokube-node.qcow2}

[ -f "$ISO" ] || { echo "!! missing $ISO — build it first (astrokube/zero-c-initramfs.sh)" >&2; exit 1; }

echo "==> converting $ISO -> $QCOW"
qemu-img convert -f raw -O qcow2 "$ISO" "$QCOW"
qemu-img info "$QCOW" | grep -iE "file format|virtual size|disk size"
echo "==> QCOW2 ready: $QCOW"
echo "    boot it: ./run-host-qemu.sh astrokube-qcow2"
