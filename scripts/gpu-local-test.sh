#!/usr/bin/env bash
# gpu-local-test.sh — enumerate a SPARE (non-display) NVIDIA GPU inside Asterinas
# on this machine, headless, with zero disruption to the display GPU.
#
# Designed for a card the host driver doesn't claim (e.g. the Kepler K4200 —
# nvidia-595 dropped Kepler, nouveau blacklisted), so binding it to vfio-pci
# affects nothing. Run with sudo:  sudo scripts/gpu-local-test.sh
set -u
DISPLAY_GPU=${DISPLAY_GPU:-0000:02:00.0}          # the A4000 — never touched
ISO=/home/john/code/asterkube/artifacts/aster-kernel-nvidia.iso
LOG=/home/john/code/asterkube/asterinas-gpu.log
[ "$(id -u)" -eq 0 ] || exec sudo "$0" "$@"
[ -f "$ISO" ] || { echo "missing ISO $ISO"; exit 1; }

# Find an NVIDIA VGA/3D controller that is NOT the display GPU.
GPU=""
for d in $(lspci -Dn | awk '$2 ~ /^0300|^0302/ && /10de:/ {print $1}'); do
  [ "$d" = "$DISPLAY_GPU" ] && continue
  GPU="$d"; break
done
[ -n "$GPU" ] || { echo "!! no spare (non-display) NVIDIA GPU found — is the card installed?"; exit 1; }
# Its audio function shares the slot (…:00.1) and IOMMU group; it must go to vfio too.
AUD="${GPU%.*}.1"
echo "== spare GPU: $GPU ($(lspci -nns "${GPU#0000:}" | cut -d']' -f2-)) =="
echo "== audio fn: $AUD =="

modprobe vfio-pci
for d in "$GPU" "$AUD"; do
  [ -e /sys/bus/pci/devices/"$d" ] || continue
  echo vfio-pci > /sys/bus/pci/devices/"$d"/driver_override 2>/dev/null || true
  echo "$d" > /sys/bus/pci/devices/"$d"/driver/unbind 2>/dev/null || true
  echo "$d" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null || true
  echo "  $d -> $(basename "$(readlink /sys/bus/pci/devices/$d/driver 2>/dev/null)" 2>/dev/null || echo NONE)"
done
[ "$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)")" = vfio-pci ] || {
  echo "!! $GPU did not bind to vfio-pci"; exit 1; }

echo "== booting instrumented Asterinas + $GPU passthrough (55s) =="
V=$(mktemp /tmp/ovmfvars.XXXX.fd); cp /usr/share/edk2/ovmf/OVMF_VARS.fd "$V"
: > "$LOG"
timeout --signal=KILL 55 qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split -accel kvm -cpu host -m 8G -smp 4 --no-reboot -nographic -serial none \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/ovmf/OVMF_CODE.fd \
  -drive if=pflash,format=raw,file="$V" \
  -chardev stdio,id=mux,mux=on,signal=off,logfile="$LOG" \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux -monitor chardev:mux \
  -device isa-debug-exit,iobase=0xf4,iosize=0x04 \
  -cdrom "$ISO" -boot d \
  -device vfio-pci,host="$GPU" \
  $([ -e /sys/bus/pci/devices/"$AUD" ] && echo -device vfio-pci,host="$AUD") \
  >/dev/null 2>&1 || true
rm -f "$V"

echo
echo "== ★ KERNEL nvidia REPORT ★ =="
sed -r 's/\x1b\[[0-9;]*m//g' "$LOG" | grep -aiE 'nvidia:|GPU enumerated|no NVIDIA GPU matched|probed .* PCI devices' | head
echo
echo "(the spare GPU is left on vfio-pci; the host never used it. A reboot detaches it.)"
