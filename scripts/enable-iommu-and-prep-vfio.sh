#!/usr/bin/env bash
# Enable Intel DMA remapping (IOMMU groups for VFIO) + prep the A4000 for
# passthrough to an Asterinas guest. Run with sudo. Reboot after step 1.
#
# Why: BIOS VT-d is already enabled (DMAR + interrupt remapping are active), but
# this Fedora kernel is built without CONFIG_INTEL_IOMMU_DEFAULT_ON, so DMA
# remapping (the part that creates /sys/kernel/iommu_groups) stays off until the
# cmdline says so.
set -euo pipefail
A4000_GPU="0000:02:00.0"   # RTX A4000            10de:24b0
A4000_AUD="0000:02:00.1"   # its HDA audio func   10de:228b  (same IOMMU group -> must bind together)

case "${1:-}" in
  step1)
    echo "==> adding intel_iommu=on iommu=pt to all installed kernels (grubby)"
    grubby --update-kernel=ALL --args="intel_iommu=on iommu=pt"
    # Persist for FUTURE kernel installs too (Fedora BLS reads /etc/kernel/cmdline).
    if [ -f /etc/kernel/cmdline ] && ! grep -q intel_iommu /etc/kernel/cmdline; then
      echo "==> persisting to /etc/kernel/cmdline (future kernels)"
      sed -i 's/[[:space:]]*$/ intel_iommu=on iommu=pt/' /etc/kernel/cmdline
    fi
    echo "==> done. REBOOT, then run: sudo $0 check"
    ;;
  check)
    n=$(find /sys/kernel/iommu_groups -maxdepth 1 -mindepth 1 -type d | wc -l)
    echo "IOMMU groups: $n  (0 = flag didn't take; >0 = good)"
    [ -e /sys/bus/pci/devices/$A4000_GPU/iommu_group ] && \
      echo "A4000 group: $(basename $(readlink -f /sys/bus/pci/devices/$A4000_GPU/iommu_group))" && \
      echo "group members:" && ls /sys/bus/pci/devices/$A4000_GPU/iommu_group/devices/
    echo "--- RMRR check (can block GPU passthrough) ---"
    dmesg | grep -i RMRR | grep -i "02:00" || echo "(no RMRR tied to the A4000 — good)"
    ;;
  bind-vfio)
    # Frees the A4000 from the display + host nvidia driver and binds it to
    # vfio-pci. THIS TAKES DOWN THE LOCAL DISPLAY on the A4000 — run it from an
    # SSH session (recommended), or accept the console going dark. The Asterinas
    # boot that follows logs to a file, so you don't need the display to see the
    # result.
    echo "!! This will stop the display manager and take the A4000 offline."
    echo "!! Ctrl-C now if you are NOT on SSH / not ready to lose the local display."
    sleep 5
    echo "==> stopping the display manager (so the GPU isn't in use when we unbind)"
    systemctl stop display-manager 2>/dev/null || true
    # Also drop the nvidia DRM/modeset users if present.
    sleep 2
    echo "==> binding $A4000_GPU + $A4000_AUD to vfio-pci"
    modprobe vfio-pci
    for d in $A4000_GPU $A4000_AUD; do
      echo "vfio-pci" > /sys/bus/pci/devices/$d/driver_override
      echo "$d" > /sys/bus/pci/devices/$d/driver/unbind 2>/dev/null || true
      echo "$d" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null || true
      echo "  $d -> $(basename $(readlink /sys/bus/pci/devices/$d/driver))"
    done
    echo "==> done. Now: sudo scripts/run-asterinas-gpu.sh   (result in /tmp/asterinas-gpu.log)"
    echo "    To restore your desktop later: sudo systemctl start display-manager"
    echo "    (or unbind vfio-pci + 'systemctl start display-manager' after a reboot)"
    ;;
  *)
    echo "usage: sudo $0 {step1|check|bind-vfio}"
    echo "  step1     add intel_iommu=on iommu=pt to cmdline (then reboot)"
    echo "  check     after reboot: verify IOMMU groups + A4000 isolation + RMRR"
    echo "  bind-vfio bind the A4000(+audio) to vfio-pci (after freeing it from display)"
    ;;
esac
