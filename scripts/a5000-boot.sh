#!/usr/bin/env bash
# Boot Asterinas (container ISO) with the A5000 (01:00.0, on vfio) passed through.
set -u
GPU=0000:01:00.0
AUD=0000:01:00.1
ISO=/home/jboero/aster-kernel-nvidia-container.iso
LOG=/home/jboero/asterinas-a5000.log
OVMF_CODE=/usr/share/edk2/ovmf/OVMF_CODE.fd
OVMF_VARS=/usr/share/edk2/ovmf/OVMF_VARS.fd
[ -f "$ISO" ] || { echo "missing ISO $ISO"; exit 1; }
drv=$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)" 2>/dev/null || echo NONE)
[ "$drv" = vfio-pci ] || { echo "!! $GPU not on vfio-pci ($drv)"; exit 1; }

V=$(mktemp /tmp/ovmfvars.XXXX.fd); cp "$OVMF_VARS" "$V"
: > "$LOG"
echo "== booting Asterinas + A5000 ($GPU) passthrough (55s) =="
timeout --signal=KILL 55 qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split -accel kvm -cpu host -m 8G -smp 4 --no-reboot -nographic -serial none \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$V" \
  -chardev stdio,id=mux,mux=on,signal=off,logfile="$LOG" \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux -monitor chardev:mux \
  -device isa-debug-exit,iobase=0xf4,iosize=0x04 \
  -cdrom "$ISO" -boot d \
  -device vfio-pci,host="$GPU" -device vfio-pci,host="$AUD" \
  >/dev/null 2>&1 || true
rm -f "$V"
echo; echo "== KERNEL report + USERSPACE/CONTAINER output (A5000 / Ampere) =="
sed -r 's/\x1b\[[0-9;]*m//g' "$LOG" | grep -aiE 'nvidia:|gpu-container|GPU-IN-CONTAINER|GPU enumerated' | head -30
echo "== (end) =="
