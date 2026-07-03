#!/bin/bash
# stage-virtiofs-zeroc.sh — stage a 100% C-FREE virtio-fs share for the zero-C
# image. It contains ONLY statically-linked, CGO-free Go binaries (the container
# runtime) plus a side-loaded image — no upstream kubelet, no glibc-linked
# anything. Every ELF placed here is verified to be static with zero NEEDED libs.
#
# The container runtime is: static containerd + ctr + containerd-shim-runc-v2.
# containerd and ctr are ONE multi-call binary (same Go module) installed once and
# hard-linked; the shim is its own binary (it can't co-reside with the daemon's
# plugins). Both built by asterkube/build-containerd-merged.sh.
# The OCI runtime (runc) is OUR binary in the initramfs (no copy needed here).
#
# Usage: asterkube/stage-virtiofs-zeroc.sh
set -euo pipefail
SRC=${STATIC_BINS:-$(cd "$(dirname "$0")/.." && pwd)/build/static-bins}
DST=${VIRTIOFS_SHARE_ZEROC:-/tmp/asterkube-vfs-zeroc}
OLD=${VIRTIOFS_SHARE:-/tmp/asterkube-vfs}

rm -rf "$DST"; mkdir -p "$DST"
echo "==> staging static (CGO-free) container runtime into $DST"
# containerd and ctr are one multi-call binary: install it once and hard-link
# `ctr` so the share carries a single copy. The shim is a separate binary.
install -m 0755 "$SRC/containerd" "$DST/containerd"
ln -f "$DST/containerd" "$DST/ctr"
install -m 0755 "$SRC/containerd-shim-runc-v2" "$DST/containerd-shim-runc-v2"
# A side-loaded OCI image to run (the workload may be anything; this one is a
# static Go 'hello').
[ -f "$OLD/hello.tar" ] && cp -f "$OLD/hello.tar" "$DST/hello.tar"
printf 'marker: asterkube ZERO-C share (no glibc, no upstream kubelet)\n' > "$DST/MARKER"

echo "==> verifying every binary on the share is static / zero-C"
bad=0
for f in "$DST"/*; do
  head -c4 "$f" 2>/dev/null | grep -q $'\x7fELF' || continue
  if readelf -d "$f" 2>/dev/null | grep -q NEEDED || readelf -l "$f" 2>/dev/null | grep -q INTERP; then
    echo "   !! DYNAMIC: $(basename "$f")"; bad=$((bad+1))
  else
    printf "   %-26s %6s  static, zero C\n" "$(basename "$f")" "$(ls -lh "$f"|awk '{print $5}')"
  fi
done
if [ "$bad" -ne 0 ]; then echo "==> FAILED: $bad dynamic binaries on the share"; exit 1; fi
echo "==> OK — share is 100% C-free. Boot with: VIRTIOFS_SHARE=$DST ./run-host-qemu.sh asterkube"
