#!/bin/bash
# build-hello-image.sh — build a minimal "from scratch" OCI image whose only
# content is one static (CGO-free) Go binary that prints and exits, then export
# it as a docker-archive at /tmp/hello-docker.tar for `ctr image import` in the
# guest. No libc, no busybox — honours the astrokube no-C/libc constraint.
set -euo pipefail
REF=${REF:-docker.io/astrokube/hello:latest}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat > "$work/hello.go" <<'EOF'
package main

import (
	"fmt"
	"os"
)

func main() {
	host, _ := os.Hostname()
	fmt.Printf("hello from a containerd-managed container on Asterinas! pid=%d host=%q\n", os.Getpid(), host)
}
EOF
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$work/hello" "$work/hello.go"
echo "built static hello ($(stat -c%s "$work/hello") bytes)"

c=$(buildah from scratch)
buildah copy "$c" "$work/hello" /hello >/dev/null
buildah config --entrypoint '["/hello"]' "$c"
id=$(buildah commit --format oci "$c" "$REF")
buildah rm "$c" >/dev/null
echo "committed image $id ($REF)"

rm -f /tmp/hello-docker.tar
skopeo copy "containers-storage:$id" "docker-archive:/tmp/hello-docker.tar:$REF" >/dev/null
echo "==> /tmp/hello-docker.tar ($(stat -c%s /tmp/hello-docker.tar) bytes), ref=$REF"
