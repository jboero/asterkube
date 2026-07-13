#!/usr/bin/env bash
# Bind the A5000 (01:00.0) + its audio fn to vfio-pci. Display is on the Intel
# iGPU, so this does not affect the laptop's screen. Idempotent.
set -u
GPU=0000:01:00.0
AUD=0000:01:00.1

modprobe vfio-pci 2>/dev/null || true

# nvidia_drm/nvidia may hold the GPU even though it's not driving a display.
# Try a clean per-device unbind first; if the GPU is held, unload nvidia stack.
bind_one() {
  local d=$1
  [ -e /sys/bus/pci/devices/"$d" ] || return 0
  echo vfio-pci > /sys/bus/pci/devices/"$d"/driver_override 2>/dev/null || true
  echo "$d" > /sys/bus/pci/devices/"$d"/driver/unbind 2>/dev/null || true
  echo "$d" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null || true
}

bind_one "$AUD"
bind_one "$GPU"

cur=$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)" 2>/dev/null || echo NONE)
if [ "$cur" != vfio-pci ]; then
  echo "GPU still on '$cur' — unloading nvidia stack and retrying"
  # stop anything holding nvidia (persistence daemon), then unload
  systemctl stop nvidia-persistenced 2>/dev/null || true
  modprobe -r nvidia_drm nvidia_modeset nvidia_uvm nvidia 2>/dev/null || true
  bind_one "$GPU"
  cur=$(basename "$(readlink /sys/bus/pci/devices/$GPU/driver 2>/dev/null)" 2>/dev/null || echo NONE)
fi

acur=$(basename "$(readlink /sys/bus/pci/devices/$AUD/driver 2>/dev/null)" 2>/dev/null || echo NONE)
echo "GPU  $GPU -> $cur"
echo "AUD  $AUD -> $acur"
[ "$cur" = vfio-pci ] && echo "VFIO-READY" || echo "VFIO-FAILED"
