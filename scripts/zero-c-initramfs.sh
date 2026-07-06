#!/bin/bash
# zero-c-initramfs.sh — build a ZERO-C node image: a bootable initramfs that
# contains no C runtime at all (no glibc/musl shared objects, no dynamically
# linked or C-static binaries). Only the static, CGO-free Go init remains.
#
# It takes the normal staged initramfs and:
#   * swaps in a fresh CGO_ENABLED=0 asterkube-init,
#   * deletes /lib64 + /lib (the glibc closure),
#   * deletes every dynamically-linked binary (e.g. the upstream runc),
#   * verifies NOTHING ELF-dynamic survives,
# then repacks the cpio and rebuilds the bootable ISO.
#
# The init detects the absent libc at boot (pureGoMode) and runs only its
# pure-Go path: mounts, capability probes, and the node agent that launches
# real namespaced + cgroup-limited containers via clone() directly — no runc,
# no containerd, no C.
#
# Usage: asterkube/zero-c-initramfs.sh [path-to-kubelet-repo]
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/
KUBELET=${1:-$(cd "$(dirname "$0")/.." && pwd)}
BUILD=test/initramfs/build
CPIO="$BUILD/initramfs.cpio.gz"
WORK=$(mktemp -d /tmp/asterkube-zeroc.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

# The finished image is SELF-CONTAINED: the static container runtime and the
# side-loaded image are baked into the initramfs (they used to be delivered over
# a virtio-fs share during development). Source them from the static-bins dir
# (containerd + shim, built by build-containerd-merged.sh) and the hello image.
STATIC_BINS=${STATIC_BINS:-$(cd "$(dirname "$0")/.." && pwd)/build/static-bins}
HELLO_TAR=${HELLO_TAR:-/tmp/asterkube-vfs-zeroc/hello.tar}

# INIT_BIN lets us drop in a pre-built binary — e.g. the COMBINED zero-C kubelet
# (real upstream kubelet + our init), built by build-zeroc-kubelet.sh. Otherwise
# build the lightweight standalone init/node-agent from this module.
if [ -n "${INIT_BIN:-}" ]; then
  echo "==> using pre-built init binary: $INIT_BIN"
  cp "$INIT_BIN" "$WORK/init-bin"; chmod 0755 "$WORK/init-bin"
else
  echo "==> building asterkube-init (static, CGO_ENABLED=0) from $KUBELET"
  ( cd "$KUBELET" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -ldflags="-s -w" -o "$WORK/init-bin" ./cmd/asterkube-init )
fi

echo "==> extracting staged initramfs $CPIO"
mkdir -p "$WORK/root"
# Format-agnostic decompress: the staged initramfs may be gzip (the OSDK default)
# or zstd (what we repack to below). Detect by magic bytes.
decompress_cpio() {  # $1 = compressed cpio
  case "$(head -c2 "$1" | od -An -tx1 | tr -d ' ')" in
    1f8b) gzip -dc "$1" ;;              # gzip
    28b5) zstd -dc --long=27 "$1" ;;    # zstd
    *)    cat "$1" ;;                   # raw cpio
  esac
}
( cd "$WORK/root" && decompress_cpio "$OLDPWD/$CPIO" | cpio -idm --quiet )

# /sbin/init -> ../usr/bin/kubelet, so the init binary lives there. We ship ONE
# binary (no duplicate): `kubelet` is our static, CGO-free init/node-agent, and
# it is also the OCI runtime, mount/umount applet, etc. via argv[0] multi-call.
echo "==> swapping in fresh static init (single binary, no duplicate)"
install -m 0755 "$WORK/init-bin" "$WORK/root/usr/bin/kubelet"
rm -f "$WORK/root/usr/bin/asterkube-init"

# Bake the static container runtime + image into the initramfs so the image is
# self-contained (no virtio-fs share). containerd and ctr are one binary (hard
# link); the shim is its own. All are static/CGO-free, so they survive the
# dynamic-binary purge below and the zero-C verification.
echo "==> baking the static container runtime into the initramfs (self-contained)"
for b in containerd containerd-shim-runc-v2; do
  [ -f "$STATIC_BINS/$b" ] || { echo "    !! missing $STATIC_BINS/$b (run build-containerd-merged.sh)"; exit 1; }
  install -m 0755 "$STATIC_BINS/$b" "$WORK/root/usr/bin/$b"
done
# ctr is the same multi-call binary as containerd; a relative SYMLINK (not a hard
# link — Asterinas' cpio extractor doesn't reconstruct hard links, leaving an
# empty file) keeps argv[0] dispatch working (exec'ing /usr/bin/ctr keeps
# argv[0]="ctr").
ln -sf containerd "$WORK/root/usr/bin/ctr"
mkdir -p "$WORK/root/usr/share/asterkube"
if [ -f "$HELLO_TAR" ]; then
  install -m 0644 "$HELLO_TAR" "$WORK/root/usr/share/asterkube/hello.tar"
  echo "    image: usr/share/asterkube/hello.tar"
else
  echo "    !! missing hello image $HELLO_TAR (run build-hello-image.sh)"; exit 1
fi

# Ship the documented default /etc/fstab (operator-editable table of extra
# mounts; the init reads it after the essential pseudo-filesystems).
HERE=$(cd "$(dirname "$0")" && pwd)
install -m 0644 "$HERE/../config/fstab.default" "$WORK/root/etc/fstab"
echo "    fstab: etc/fstab (default template)"

# Identify the node OS as Asterinas: kubelet/cadvisor reads /etc/os-release and
# reports its PRETTY_NAME as the node's "OS Image". ID_LIKE=linux keeps
# Linux-compatible tooling working. (The "Operating System" field stays "linux"
# — that is GOOS, load-bearing for pod scheduling.)
install -m 0644 "$HERE/../config/os-release" "$WORK/root/etc/os-release"
echo "    os-release: etc/os-release (OS Image → Asterinas)"

echo "==> removing the C runtime (glibc closure) and any dynamic binaries"
rm -rf "$WORK/root/lib64" "$WORK/root/lib"
# Walk every regular file; drop anything that is a dynamically-linked ELF or a
# C-static binary that still carries an interpreter/NEEDED entry.
removed=0
while IFS= read -r -d '' f; do
  head -c4 "$f" 2>/dev/null | grep -q $'\x7fELF' || continue
  if readelf -d "$f" 2>/dev/null | grep -q NEEDED || \
     readelf -l "$f" 2>/dev/null | grep -q INTERP; then
    echo "    drop dynamic: ${f#$WORK/root}"
    rm -f "$f"; removed=$((removed+1))
  fi
done < <(find "$WORK/root" -type f -print0)
echo "    removed $removed dynamic binaries"

echo "==> verifying NOTHING C/dynamic remains"
bad=0
while IFS= read -r -d '' f; do
  head -c4 "$f" 2>/dev/null | grep -q $'\x7fELF' || continue
  if readelf -d "$f" 2>/dev/null | grep -q NEEDED || \
     readelf -l "$f" 2>/dev/null | grep -q INTERP; then
    echo "    !! STILL DYNAMIC: ${f#$WORK/root}"; bad=$((bad+1))
  fi
done < <(find "$WORK/root" -type f -print0)
if [ -d "$WORK/root/lib64" ] || [ -d "$WORK/root/lib" ]; then
  echo "    !! lib/ or lib64/ still present"; bad=$((bad+1))
fi
if [ "$bad" -ne 0 ]; then echo "==> FAILED: $bad C/dynamic artifacts remain"; exit 1; fi
echo "    OK — every ELF in the image is static and CGO-free; no lib64/."

echo "==> ELF inventory of the zero-C image:"
while IFS= read -r -d '' f; do
  head -c4 "$f" 2>/dev/null | grep -q $'\x7fELF' || continue
  printf "    %-28s %s\n" "${f#$WORK/root/}" "$(file -b "$f" | grep -oE 'statically linked')"
done < <(find "$WORK/root" -type f -print0)

echo "==> repacking $CPIO (zstd --ultra -22, the kernel unpacks gzip or zstd by magic)"
# Max zstd: level 22 + a 128MB long-distance window. --no-check omits the content
# checksum (the kernel's ruzstd is built without the hash feature). ~40% smaller
# than gzip -9 on this image; boot time is ~neutral (cpio unpack dominates).
( cd "$WORK/root" && find . -print0 \
    | cpio --null -o -H newc --owner=0:0 --quiet \
    | zstd -q -T0 --ultra -22 --no-check --long=27 ) > "$CPIO"
echo "==> initramfs done:"; ls -lh "$CPIO"

if [ "${SKIP_ISO:-0}" != "1" ]; then
  echo "==> rebuilding ISO in the dev container"
  docker start asterkube >/dev/null 2>&1 || true
  docker exec asterkube bash -lc \
    'git config --global --add safe.directory /root/asterinas; cd /root/asterinas/kernel && cargo osdk build --release --strip-elf --grub-boot-protocol=multiboot2' \
    2>&1 | tail -2
  echo "==> ISO:"; ls -lh target/osdk/aster-kernel-osdk-bin.iso
fi
