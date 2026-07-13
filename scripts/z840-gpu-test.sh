#!/usr/bin/env bash
# z840-gpu-test.sh — on the headless Z840: pass a Quadro GV100 through to an
# Asterinas guest and capture what the in-kernel comps/nvidia driver enumerates.
# Headless, so no display to disrupt; needs root for the vfio bind + qemu memlock.
#
#   sudo ~/asterinas-gpu/z840-gpu-test.sh
#
# GV100 is Volta (pre-GSP): Asterinas should SEE it + decode NV_PMC_BOOT_0 as
# Volta, then (correctly) decline to drive it (no GSP). This validates the
# passthrough + enumeration path on real hardware.
set -u
GPU=0000:04:00.0; AUD=0000:04:00.1
DIR=$(cd "$(dirname "$0")" && pwd)
KERNEL=$DIR/aster-kernel-nvidia.qemu_elf
INITRD=$DIR/node-x86.cpio.gz
LOG=$DIR/asterinas-gpu.log
[ "$(id -u)" -eq 0 ] || exec sudo "$0" "$@"
[ -f "$KERNEL" ] || { echo "missing kernel $KERNEL"; exit 1; }

echo "== binding $GPU + $AUD to vfio-pci =="
modprobe vfio-pci
for d in "$GPU" "$AUD"; do
  echo vfio-pci > /sys/bus/pci/devices/"$d"/driver_override
  echo "$d" > /sys/bus/pci/devices/"$d"/driver/unbind 2>/dev/null || true
  echo "$d" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null || true
  echo "  $d -> $(basename "$(readlink /sys/bus/pci/devices/$d/driver 2>/dev/null)" 2>/dev/null || echo NONE)"
done
drv=$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)" 2>/dev/null || echo NONE)
[ "$drv" = "vfio-pci" ] || { echo "!! $GPU is on '$drv', not vfio-pci — aborting"; exit 1; }

echo "== booting Asterinas on q35 with $GPU passed through (40s) =="
: > "$LOG"
timeout --signal=KILL 40 qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split -accel kvm -cpu host -m 8G -smp 4 --no-reboot -nographic \
  -serial none -monitor chardev:mux -chardev stdio,id=mux,mux=on,signal=off,logfile="$LOG" \
  -kernel "$KERNEL" -initrd "$INITRD" -append 'console=hvc0 init=/sbin/init' \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux \
  -device isa-debug-exit,iobase=0xf4,iosize=0x04 \
  -device vfio-pci,host="$GPU" \
  -device vfio-pci,host="$AUD" \
  >/dev/null 2>&1 || true

echo "== nvidia driver output from the Asterinas guest =="
grep -aE 'nvidia:' "$LOG" | sed 's/^/    /' || echo "    (none — see $LOG)"
echo "== boot log tail =="; grep -av '^$' "$LOG" | tail -15
echo
echo "== (optional) restore nvidia: echo $GPU | sudo tee /sys/bus/pci/drivers/vfio-pci/unbind; echo $GPU | sudo tee /sys/bus/pci/drivers_probe =="
