#!/bin/bash
# run-release.sh — boot a released asterkube image (ISO or QCOW2) under QEMU.
# No build toolchain required: just qemu-system-x86_64, edk2-ovmf, and KVM.
#
# Usage:
#   scripts/run-release.sh asterkube-node-v0.1.iso      # bootable ISO
#   scripts/run-release.sh asterkube-node-v0.1.qcow2    # disk image
#
# The node boots in a few seconds, runs its capability demos, and then stays
# LIVE. Shut it down gracefully with an ACPI event: in the QEMU monitor
# (Ctrl-a c) type `system_powerdown`, or just kill QEMU with `Ctrl-a x`.
set -euo pipefail
IMG=${1:?usage: run-release.sh <image.iso|image.qcow2>}
[ -f "$IMG" ] || { echo "no such image: $IMG" >&2; exit 1; }

# Locate OVMF (UEFI firmware). Distro paths differ; override with OVMF_CODE / OVMF_VARS.
find_ovmf() {
  local names_code=(OVMF_CODE.fd OVMF_CODE.4m.fd OVMF_CODE_4M.fd)
  local dirs=(/usr/share/edk2/ovmf /usr/share/OVMF /usr/share/qemu /usr/share/edk2-ovmf/x64)
  for d in "${dirs[@]}"; do for c in "${names_code[@]}"; do
    [ -f "$d/$c" ] && { echo "$d/$c"; return; }
  done; done
}
OVMF_CODE=${OVMF_CODE:-$(find_ovmf)}
[ -n "$OVMF_CODE" ] || { echo "OVMF firmware not found — install edk2-ovmf (Fedora) / ovmf (Debian/Ubuntu), or set OVMF_CODE=" >&2; exit 1; }
OVMF_VARS_SRC=${OVMF_VARS_SRC:-$(dirname "$OVMF_CODE")/OVMF_VARS.fd}
[ -f "$OVMF_VARS_SRC" ] || OVMF_VARS_SRC=$(dirname "$OVMF_CODE")/OVMF_VARS_4M.fd
VARS=$(mktemp /tmp/asterkube-OVMF_VARS.XXXXXX.fd); cp "$OVMF_VARS_SRC" "$VARS"
trap 'rm -f "$VARS"' EXIT

# KVM if available (much faster); falls back to TCG otherwise.
ACCEL=(); CPU=(-cpu qemu64)
[ -w /dev/kvm ] && { ACCEL=(-accel kvm); CPU=(-cpu host); }

# Attach the image as a CD-ROM (ISO) or a virtio disk (QCOW2).
case "$IMG" in
  *.iso)   MEDIA=(-cdrom "$IMG" -boot d) ;;
  *)       MEDIA=(-drive if=none,format=qcow2,id=disk0,file="$IMG"
                  -device virtio-blk-pci,drive=disk0,disable-legacy=on,disable-modern=off,bootindex=0) ;;
esac

echo "==> booting $IMG  (Ctrl-a c = monitor, then 'system_powerdown' to shut down; Ctrl-a x = quit)"
exec qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split "${ACCEL[@]}" "${CPU[@]},+x2apic" -m 4G -smp 1 \
  --no-reboot -nographic -serial null \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$VARS" \
  -chardev stdio,id=mux,mux=on,signal=off \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux -monitor chardev:mux \
  -object rng-random,id=rng0,filename=/dev/urandom \
  -device virtio-rng-pci,rng=rng0,disable-legacy=on,disable-modern=off \
  "${MEDIA[@]}" \
  -netdev user,id=net01 \
  -device virtio-net-pci,netdev=net01,disable-legacy=on,disable-modern=off,mrg_rxbuf=off,ctrl_rx=off,ctrl_rx_extra=off,ctrl_vlan=off,ctrl_vq=off,ctrl_guest_offloads=off,ctrl_mac_addr=off,event_idx=off,queue_reset=off,guest_announce=off,indirect_desc=off
