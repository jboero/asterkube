#!/bin/bash
# stage-virtiofs.sh — populate the host directory that is shared into the guest
# over virtio-fs. containerd, ctr and the runc shim are copied straight from the
# Fedora host; their glibc closure (libc/libresolv/ld-linux) already lives in the
# guest initramfs /lib64, so no libraries are staged here. A MARKER file lets the
# guest prove the mount is readable.
#
# Usage: asterkube/stage-virtiofs.sh [share-dir]   (default /tmp/asterkube-vfs)
set -euo pipefail
SHARE=${1:-/tmp/asterkube-vfs}
mkdir -p "$SHARE"

echo "marker: asterkube virtio-fs share staged from $(uname -n)" > "$SHARE/MARKER"

for bin in /usr/bin/containerd /usr/bin/ctr /usr/bin/containerd-shim-runc-v2 /usr/bin/crictl /usr/bin/kubelet; do
  if [ -x "$bin" ]; then
    install -m 0755 "$bin" "$SHARE/$(basename "$bin")"
    echo "staged $(basename "$bin") ($(stat -c%s "$bin") bytes)"
  else
    echo "WARNING: $bin not found on host" >&2
  fi
done

# A side-loaded OCI image (docker-archive, carries its ref name) for `ctr image
# import` + `ctr run`. Built by asterkube/build-hello-image.sh; copied if present.
if [ -f /tmp/hello-docker.tar ]; then
  install -m 0644 /tmp/hello-docker.tar "$SHARE/hello.tar"
  echo "staged hello.tar ($(stat -c%s /tmp/hello-docker.tar) bytes)"
else
  echo "NOTE: /tmp/hello-docker.tar not present; run asterkube/build-hello-image.sh first" >&2
fi

# Pause/sandbox image for the CRI phase (built by asterkube/build-pause-image.sh).
if [ -f /tmp/pause-docker.tar ]; then
  install -m 0644 /tmp/pause-docker.tar "$SHARE/pause.tar"
  echo "staged pause.tar ($(stat -c%s /tmp/pause-docker.tar) bytes)"
else
  echo "NOTE: /tmp/pause-docker.tar not present; run asterkube/build-pause-image.sh first" >&2
fi

# Static CNI plugins (ptp/portmap/host-local/loopback) so the node can reach
# Ready. Sourced from a kind worker's /opt/cni/bin (statically linked, no libc):
#   mkdir -p /tmp/asterkube-cni && for p in ptp portmap host-local loopback; do \
#     docker cp upbound-cluster-worker:/opt/cni/bin/$p /tmp/asterkube-cni/$p; done
if [ -d /tmp/asterkube-cni ]; then
  mkdir -p "$SHARE/cni"
  install -m 0755 /tmp/asterkube-cni/* "$SHARE/cni/"
  echo "staged CNI plugins: $(ls "$SHARE/cni" | tr '\n' ' ')"
else
  echo "NOTE: /tmp/asterkube-cni not present; node will register but stay NotReady (no CNI)" >&2
fi

# Host CA trust bundle so the node's containerd can verify TLS to image
# registries (registry.k8s.io / docker.io) for the cluster's system DaemonSets.
for ca in /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem \
          /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
  if [ -s "$ca" ]; then
    install -m 0644 "$(readlink -f "$ca")" "$SHARE/ca-bundle.crt"
    echo "staged ca-bundle.crt from $ca ($(grep -c 'BEGIN CERTIFICATE' "$SHARE/ca-bundle.crt") certs)"
    break
  fi
done

# Node kubeconfig for the apiserver-join phase is produced separately by
# asterkube/make-node-kubeconfig.sh (writes $SHARE/kubeconfig).

echo "==> share contents:"; ls -lh "$SHARE"
