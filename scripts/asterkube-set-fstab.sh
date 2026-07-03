#!/bin/bash
# asterkube-set-fstab.sh — repackage the node image with YOUR /etc/fstab.
#
# Swaps a custom fstab into the EXISTING initramfs (no kubelet/containerd rebuild)
# and regenerates the bootable ISO + QCOW2. Use it to ship extra data mounts
# (disks, partitions, tmpfs, virtio-fs shares) without touching source. The
# default table is asterkube/fstab.default — copy it, edit, and pass it here.
#
# Usage:
#   asterkube/asterkube-set-fstab.sh <your-fstab>     # repackage with your fstab
#   asterkube/asterkube-set-fstab.sh --validate <f>   # just sanity-check it
#   asterkube/asterkube-set-fstab.sh --print          # print the current image's fstab
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                                # asterinas/
CPIO=test/initramfs/build/initramfs.cpio.gz
ISO=target/osdk/aster-kernel-osdk-bin.iso

# Filesystems the node can actually mount (keep in sync with fstab.default).
KNOWN_FSTYPES="ext2 ext4 exfat tmpfs ramfs overlay virtiofs proc sysfs devtmpfs cgroup2 configfs devpts"

validate_fstab() {                                    # <file>
  local f=$1 line n=0 bad=0
  [ -f "$f" ] || { echo "!! no such file: $f" >&2; return 1; }
  while IFS= read -r line; do
    n=$((n+1))
    case "$line" in ''|\#*) continue ;; esac          # skip blanks/comments
    # shellcheck disable=SC2086
    set -- $line
    if [ "$#" -lt 3 ]; then
      echo "  line $n: need at least <src> <mnt> <fstype>: $line" >&2; bad=$((bad+1)); continue
    fi
    local fstype=$3
    case " $KNOWN_FSTYPES " in
      *" $fstype "*) ;;
      *) echo "  line $n: WARNING fstype '$fstype' is not supported by Asterinas" >&2; bad=$((bad+1)) ;;
    esac
  done < "$f"
  if [ "$bad" -ne 0 ]; then echo "==> fstab has $bad issue(s) above" >&2; return 1; fi
  echo "==> fstab OK ($f)"
}

current_fstab() {
  local tmp src; tmp=$(mktemp -d); src=$(readlink -f "$CPIO")
  ( cd "$tmp" && zcat "$src" | cpio -idm --quiet 2>/dev/null )
  cat "$tmp/etc/fstab" 2>/dev/null || echo "(no /etc/fstab in the current image)"
  rm -rf "$tmp"
}

case "${1:-}" in
  ''|-h|--help) sed -n '2,16p' "$0"; exit 0 ;;
  --print)      current_fstab; exit 0 ;;
  --validate)   validate_fstab "${2:?usage: --validate <fstab>}"; exit $? ;;
esac

FSTAB=$1
validate_fstab "$FSTAB" || { echo "!! refusing to repackage an invalid fstab (override by fixing the warnings)"; exit 1; }
[ -f "$CPIO" ] || { echo "!! missing $CPIO — build the image first (asterkube/zero-c-initramfs.sh)" >&2; exit 1; }

WORK=$(mktemp -d /tmp/asterkube-setfstab.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
echo "==> extracting current initramfs"
mkdir -p "$WORK/root"
( cd "$WORK/root" && zcat "$OLDPWD/$CPIO" | cpio -idm --quiet )

echo "==> installing your /etc/fstab"
install -m 0644 "$FSTAB" "$WORK/root/etc/fstab"

echo "==> repacking initramfs"
( cd "$WORK/root" && find . -print0 | cpio --null -o -H newc --owner=0:0 --quiet | gzip -9 ) > "$CPIO"
ls -lh "$CPIO" | awk '{print "    initramfs:", $5}'

echo "==> rebuilding ISO (cargo osdk, release+stripped) in the dev container"
docker start asterkube >/dev/null 2>&1 || true
docker exec asterkube bash -lc \
  'git config --global --add safe.directory /root/asterinas; cd /root/asterinas/kernel && cargo osdk build --release --strip-elf --grub-boot-protocol=multiboot2' \
  2>&1 | tail -2
ls -lh "$ISO" | awk '{print "    ISO:", $5}'

echo "==> regenerating QCOW2"
bash asterkube/build-qcow2.sh 2>&1 | grep -iE "QCOW2 ready"

echo "==> done. Boot:  ./run-host-qemu.sh asterkube   (or asterkube-qcow2)"
