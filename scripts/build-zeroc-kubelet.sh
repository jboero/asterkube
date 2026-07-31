#!/bin/bash
# build-zeroc-kubelet.sh — build the COMBINED, CGO-free kubelet: the real
# upstream Kubernetes kubelet with our init / node-agent / pure-Go OCI runtime /
# mount applet folded in, as a single static binary with ZERO C.
#
# How: the real kubelet command lives in k8s.io/kubernetes (cmd/kubelet/app),
# which our k8s.io/kubelet staging module can't import. So we assemble a combined
# `cmd/asterkube-kubelet` package INSIDE a kubernetes checkout: our init sources
# (everything except the placeholder kubelet_entry_stub.go) plus a real entry
# that runs the upstream kubelet command. Then `CGO_ENABLED=0 go build`.
#
# Output: a single binary that is the kubelet (default), our init (as PID 1),
# our runc (argv0=runc), and mount/umount (argv0=mount/umount) — all zero C.
#
# Usage: asterkube/build-zeroc-kubelet.sh [output-path]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
KUBELET_SRC=${KUBELET_REPO:-$(cd "$(dirname "$0")/.." && pwd)}/cmd/asterkube-init
K8S=${K8S_SRC:-$(cd "$(dirname "$0")/.." && pwd)/kubernetes}
K8S_VERSION=${K8S_VERSION:-v1.35.6}
OUT=${1:-$(cd "$(dirname "$0")/.." && pwd)/build/asterkube-kubelet}

if [ ! -d "$K8S/cmd/kubelet" ]; then
  echo "==> cloning kubernetes $K8S_VERSION (shallow)"
  git clone --depth 1 --branch "$K8S_VERSION" https://github.com/kubernetes/kubernetes "$K8S"
fi

PKG="$K8S/cmd/asterkube-kubelet"
echo "==> assembling combined package at $PKG"
rm -rf "$PKG"; mkdir -p "$PKG"
# Our init sources, minus the standalone-only kubelet stub.
for f in "$KUBELET_SRC"/*.go; do
  case "$(basename "$f")" in
    *_test.go|kubelet_entry_stub.go) continue ;;
  esac
  cp "$f" "$PKG/"
done
[ -d "$KUBELET_SRC/testdata" ] && cp -r "$KUBELET_SRC/testdata" "$PKG/" || true

# The real kubelet entry: runAsKubelet runs the upstream kubelet command.
cat > "$PKG/kubelet_entry_real.go" <<'GO'
// Code assembled by build-zeroc-kubelet.sh — the real kubelet entry point.
package main

import (
	"context"
	"os"

	"k8s.io/component-base/cli"
	kubeletapp "k8s.io/kubernetes/cmd/kubelet/app"
)

// runAsKubelet runs the real, full upstream Kubernetes kubelet — compiled into
// this CGO-free binary, so the node's kubelet is ours and contains zero C.
func runAsKubelet() {
	command := kubeletapp.NewKubeletCommand(context.Background())
	os.Exit(cli.Run(command))
}
GO

echo "==> building (CGO_ENABLED=0) — this is heavy (~minutes)"
LDFLAGS="-s -w -X k8s.io/component-base/version.gitVersion=$K8S_VERSION"
( cd "$K8S" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-trimpath \
    go build -buildvcs=false -ldflags="$LDFLAGS" -o "$OUT" ./cmd/asterkube-kubelet )

echo "==> verifying zero C"
link=$(file -b "$OUT" | grep -oE 'statically linked|dynamically linked' || true)
# `grep -c` exits 1 when the count is 0 — which is exactly the zero-C case — so
# guard it against `set -o pipefail` aborting the build on success.
needed=$(readelf -d "$OUT" 2>/dev/null | grep -c NEEDED || true)
printf "   %s  (size %s, NEEDED=%s)\n" "$link" "$(ls -lh "$OUT"|awk '{print $5}')" "$needed"
if [ "$link" != "statically linked" ] || [ "$needed" != "0" ]; then
  echo "   !! NOT zero-C"; exit 1
fi
echo "   kubelet: $("$OUT" --version 2>&1 | head -1)"
echo "==> combined zero-C kubelet at $OUT"
