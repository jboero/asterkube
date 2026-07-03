#!/bin/bash
# build-pause-image.sh — build a minimal "pause" (sandbox) image tagged exactly
# as containerd's default CRI sandbox image, so the CRI plugin uses it without a
# registry pull. The entrypoint is a static Go binary that blocks on a signal
# (no CPU), which is all a pod-sandbox/pause container needs to do.
set -euo pipefail
REF=${REF:-registry.k8s.io/pause:3.10.1}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat > "$work/pause.go" <<'EOF'
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	<-c
}
EOF
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$work/pause" "$work/pause.go"
echo "built static pause ($(stat -c%s "$work/pause") bytes)"

c=$(buildah from scratch)
buildah copy "$c" "$work/pause" /pause >/dev/null
buildah config --entrypoint '["/pause"]' "$c"
id=$(buildah commit --format oci "$c" "$REF")
buildah rm "$c" >/dev/null
echo "committed pause image $id ($REF)"

rm -f /tmp/pause-docker.tar
skopeo copy "containers-storage:$id" "docker-archive:/tmp/pause-docker.tar:$REF" >/dev/null
echo "==> /tmp/pause-docker.tar ($(stat -c%s /tmp/pause-docker.tar) bytes), ref=$REF"
