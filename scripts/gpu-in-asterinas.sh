#!/usr/bin/env bash
# gpu-in-asterinas.sh — ONE command (run with sudo, ideally over SSH) that:
#   1. frees the RTX A4000 from the display + host nvidia driver -> vfio-pci
#   2. boots the Asterinas kernel (built --features nvidia_gpu) on q35 with the
#      A4000 passed through, and
#   3. prints what the in-kernel comps/nvidia driver saw.
#
# It takes down the local display (the A4000 drives it), so run it from SSH.
# The result is also saved to /home/john/code/asterkube/asterinas-gpu.log — Claude reads that file.
#
#   sudo /home/john/code/asterkube/scripts/gpu-in-asterinas.sh
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
[ "$(id -u)" -eq 0 ] || { echo "run with sudo (needs vfio bind + memlock for the guest)"; exit 1; }

"$HERE/enable-iommu-and-prep-vfio.sh" bind-vfio
echo
"$HERE/run-asterinas-gpu.sh"
echo
echo "==> full boot log: /home/john/code/asterkube/asterinas-gpu.log"
echo "==> restore your desktop when done: sudo systemctl start display-manager"
