#!/bin/bash
# build-containerd-merged.sh — build the merged, static, CGO-free containerd
# multi-call binary used by the zero-C share.
#
# The astrokube node ships three containerd-family programs that are all the SAME
# Go module (github.com/containerd/containerd/v2): the containerd daemon, the ctr
# CLI, and the runc v2 shim. Linked separately they are three near-identical
# ~20-65 MB binaries that each bake in their own copy of the Go runtime and the
# entire containerd library.
#
# This builds TWO binaries instead of three:
#   1. containerd               — daemon + ctr folded into one busybox-style
#                                 binary dispatching on argv[0] (containerd-merged/
#                                 main.go); `ctr` is a hard link to it at staging.
#   2. containerd-shim-runc-v2  — the shim, still its OWN binary. The shim inits
#                                 every plugin in the global registry with no
#                                 config, so it cannot share a binary with the
#                                 daemon's builtin plugins (it would panic on,
#                                 e.g., imageverifier/bindir). Upstream ships it
#                                 separately for the same reason.
#
# It builds from the EXACT v2.2.3 module in the Go module cache (so the result
# matches the previously-verified standalone binaries) by copying that read-only
# module to a writable scratch dir and adding our cmd/ package to it — the
# module's own go.mod/go.sum drive dependency resolution.
#
# Usage: astrokube/build-containerd-merged.sh
set -euo pipefail

VERSION=${CONTAINERD_VERSION:-2.2.3}
MODSRC=${CONTAINERD_MODULE:-$HOME/go/pkg/mod/github.com/containerd/containerd/v2@v$VERSION}
HERE=$(cd "$(dirname "$0")" && pwd)
MAIN="$HERE/containerd-merged/main.go"
OUT=${STATIC_BINS:-$(cd "$(dirname "$0")/.." && pwd)/build/static-bins}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[ -d "$MODSRC" ] || { echo "!! containerd v$VERSION not in module cache: $MODSRC" >&2; exit 1; }
[ -f "$MAIN" ]   || { echo "!! merged main.go missing: $MAIN" >&2; exit 1; }

echo "==> staging writable copy of containerd v$VERSION module"
cp -a "$MODSRC/." "$WORK/"
chmod -R u+w "$WORK"
mkdir -p "$WORK/cmd/astrokube-containerd"
cp "$MAIN" "$WORK/cmd/astrokube-containerd/main.go"

build_static() { # <out-name> <pkg-path>
  local name=$1 pkg=$2
  echo "==> building $name (static, CGO-free, tags: no_btrfs,no_devmapper)"
  ( cd "$WORK" && GOSUMDB=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -mod=mod -tags no_btrfs,no_devmapper \
        -ldflags="-s -w -X github.com/containerd/containerd/v2/version.Version=$VERSION" \
        -o "$WORK/$name" "$pkg" )
  if readelf -d "$WORK/$name" 2>/dev/null | grep -q NEEDED \
     || readelf -l "$WORK/$name" 2>/dev/null | grep -q INTERP; then
    echo "!! $name is NOT static (has NEEDED/INTERP)" >&2; exit 1
  fi
}

# 1. The merged daemon+ctr binary (our package added to the module copy).
build_static containerd ./cmd/astrokube-containerd
# 2. The shim, still its own binary (upstream's package, unchanged).
build_static containerd-shim-runc-v2 ./cmd/containerd-shim-runc-v2

mkdir -p "$OUT"
install -m 0755 "$WORK/containerd"              "$OUT/containerd"
install -m 0755 "$WORK/containerd-shim-runc-v2" "$OUT/containerd-shim-runc-v2"
# ctr is now subsumed by the merged binary (hard-linked at staging time); drop
# any stale standalone copy so staging can't ship the old separate binary.
rm -f "$OUT/ctr"

szc=$(ls -lh "$OUT/containerd" | awk '{print $5}')
szs=$(ls -lh "$OUT/containerd-shim-runc-v2" | awk '{print $5}')
echo "==> OK — containerd(+ctr) $szc + shim $szs, both static / CGO-free"
echo "    (ctr is hard-linked to containerd at staging time)"
