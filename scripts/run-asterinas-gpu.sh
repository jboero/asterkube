#!/usr/bin/env bash
# run-asterinas-gpu.sh — boot the Asterinas kernel (built with --features
# nvidia_gpu) on q35 with the RTX A4000 passed through via VFIO, and watch the
# in-kernel `comps/nvidia` driver enumerate it.
#
# Prerequisites (run these first, they need sudo):
#   1. IOMMU on:   sudo scripts/enable-iommu-and-prep-vfio.sh step1  (+ reboot)
#   2. Free the A4000 from the host nvidia driver / display, then:
#                  sudo scripts/enable-iommu-and-prep-vfio.sh bind-vfio
#   3. Build:      (in the asterinas tree)
#                  OSDK_LOCAL_DEV=1 cargo osdk build --target-arch x86_64 \
#                      --scheme microvm --features nvidia_gpu
#
# VFIO device pinning + the vfio group node need privilege, so run this with sudo.
#
# Success looks like these lines in the log:
#   nvidia: found NVIDIA GPU 10de:24b0 ...
#   nvidia:   NV_PMC_BOOT_0 = 0x1720xxxx -> Ampere impl 0x4 rev ...
#   nvidia:   MSI-X: N vectors ...
# i.e. the A4000 is visible from inside Asterinas.
set -euo pipefail

ASTER=${ASTER:-/home/john/code/asterinas}
KERNEL=${KERNEL:-/home/john/code/asterkube/artifacts/aster-kernel-nvidia.qemu_elf}
INITRD=${INITRD:-/home/john/code/asterkube/artifacts/node-x86.cpio.gz}
GPU=${GPU:-0000:02:00.0}
GPU_AUDIO=${GPU_AUDIO:-0000:02:00.1}
LOG=${LOG:-/home/john/code/asterkube/asterinas-gpu.log}   # persistent (survives reboot; /tmp does not)
SECS=${SECS:-40}

[ -f "$KERNEL" ] || { echo "no kernel at $KERNEL (build with --features nvidia_gpu)"; exit 1; }
[ -f "$INITRD" ] || { echo "no initramfs at $INITRD"; exit 1; }

# Confirm the GPU is on vfio-pci (not nvidia) before trying to hand it over.
drv=$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)" 2>/dev/null || echo NONE)
if [ "$drv" != "vfio-pci" ]; then
  echo "!! $GPU is bound to '$drv', not vfio-pci."
  echo "   Free it from the display and run: sudo scripts/enable-iommu-and-prep-vfio.sh bind-vfio"
  exit 1
fi

ACCEL="-accel kvm"; [ -w /dev/kvm ] || ACCEL="-accel tcg"
: > "$LOG"
echo "==> booting Asterinas on q35 with $GPU passed through (log: $LOG)"

# q35 (real PCIe, so the guest enumerates the GPU and its BARs) + PVH -kernel
# boot of the microvm-scheme kernel. The A4000 and its audio function go together
# (same IOMMU group).
timeout --signal=KILL "$SECS" qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split $ACCEL -cpu host -m 4G -smp 1 --no-reboot -nographic \
  -serial none -monitor chardev:mux -chardev stdio,id=mux,mux=on,signal=off,logfile="$LOG" \
  -kernel "$KERNEL" -initrd "$INITRD" \
  -append 'console=hvc0 init=/sbin/init' \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux \
  -device isa-debug-exit,iobase=0xf4,iosize=0x04 \
  -device vfio-pci,host="$GPU" \
  -device vfio-pci,host="$GPU_AUDIO" \
  >/dev/null 2>&1 || true

echo "==> nvidia driver output from the Asterinas guest:"
grep -aE 'nvidia:' "$LOG" | sed 's/^/    /' || echo "    (no nvidia: lines — see $LOG)"
