#!/bin/bash
# asterkube-join.sh — generate a "join bundle" config drive that binds a generic
# asterkube node image to YOUR Kubernetes cluster, the way cloud-init/Ignition
# bind a generic OS image to a host. The node image stays cluster-agnostic; all
# the cluster-specific bits (apiserver endpoint, CA/trust bundle, node identity,
# DNS) live on a small disk you attach at boot.
#
# Run this on a workstation that has `kubectl` admin access to the TARGET cluster
# (the one you want the node to join). It:
#   1. discovers the apiserver endpoint + cluster CA from your current kubectl
#      context (so different clusters / DNS / trust bundles just work),
#   2. mints a node client cert via the cluster's CSR API — the CA private key
#      never leaves the cluster (identity CN=system:node:<node>, O=system:nodes,
#      which the built-in Node authorizer already grants),
#   3. writes a kubeconfig that trusts the REAL cluster CA (no insecure-skip),
#   4. optionally pins the apiserver hostname->IP in /etc/hosts and sets DNS,
#   5. packs it all into an ext2 image (rootless, via `mke2fs -d`) that the node
#      mounts at first boot.
#
# Attach the result to the VM as an extra disk, e.g.:
#   -drive if=none,format=raw,id=join,file=asterkube-join.img \
#   -device virtio-blk-pci,drive=join,serial=asterkubecfg
# The node looks for a disk whose serial/label is "asterkubecfg", mounts it, and
# joins. (run-host-qemu.sh: JOIN_BUNDLE=asterkube-join.img ./run-host-qemu.sh asterkube)
#
# Usage: asterkube/asterkube-join.sh [-n node-name] [-s apiserver-host[:port]]
#                                    [-a apiserver-ip] [-d cluster-dns]
#                                    [-o out.img] [--dir DIR]
set -euo pipefail

NODE="asterkube-$(hostname -s 2>/dev/null || echo node)"
SERVER_OVERRIDE=""      # force the apiserver URL the node dials (else from kubectl)
APISERVER_IP=""         # pin apiserver hostname -> this IP in the node's /etc/hosts
CLUSTER_DNS=""          # cluster DNS service IP (e.g. 10.96.0.10) for pods
NODE_IP=""              # node address as the apiserver should see it (else auto)
OUT="asterkube-join.img"
DIRONLY=""              # if set, emit a plain directory instead of a disk image
TAINTS="asterkube.io/experimental=true:NoSchedule"

while [ $# -gt 0 ]; do
  case "$1" in
    -n) NODE="$2"; shift 2 ;;
    -s) SERVER_OVERRIDE="$2"; shift 2 ;;
    -a) APISERVER_IP="$2"; shift 2 ;;
    -d) CLUSTER_DNS="$2"; shift 2 ;;
    -i|--node-ip) NODE_IP="$2"; shift 2 ;;
    -o) OUT="$2"; shift 2 ;;
    --taints) TAINTS="$2"; shift 2 ;;
    --dir) DIRONLY="$2"; shift 2 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

command -v kubectl >/dev/null || { echo "!! kubectl not found (need admin access to the target cluster)" >&2; exit 1; }
command -v openssl >/dev/null || { echo "!! openssl not found" >&2; exit 1; }

WORK=$(mktemp -d /tmp/asterkube-join.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
BUNDLE="$WORK/bundle"; mkdir -p "$BUNDLE"

# 1. Discover the apiserver endpoint + cluster CA from the current kubectl context.
SERVER=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
[ -n "$SERVER" ] || { echo "!! could not read apiserver URL from kubectl context" >&2; exit 1; }
CA_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
if [ -z "$CA_DATA" ]; then
  CA_FILE=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority}')
  [ -n "$CA_FILE" ] && CA_DATA=$(base64 -w0 "$CA_FILE")
fi
[ -n "$CA_DATA" ] || { echo "!! could not obtain the cluster CA bundle" >&2; exit 1; }
echo "$CA_DATA" | base64 -d > "$BUNDLE/ca.crt"
NODE_SERVER=${SERVER_OVERRIDE:+https://$SERVER_OVERRIDE}
NODE_SERVER=${NODE_SERVER:-$SERVER}
echo "==> target cluster: $SERVER   (node will dial: $NODE_SERVER)"

# 2. Mint a node client cert via the cluster's CSR signer (no CA key involved).
CSR_NAME="asterkube-join-${NODE}"
openssl genrsa -out "$BUNDLE/node.key" 2048 >/dev/null 2>&1
openssl req -new -key "$BUNDLE/node.key" -out "$WORK/node.csr" \
  -subj "/O=system:nodes/CN=system:node:${NODE}" >/dev/null 2>&1
echo "==> submitting CSR ${CSR_NAME} (CN=system:node:${NODE}, O=system:nodes)"
kubectl delete csr "$CSR_NAME" >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: ${CSR_NAME}
spec:
  request: $(base64 -w0 "$WORK/node.csr")
  signerName: kubernetes.io/kube-apiserver-client-kubelet
  usages: [digital signature, key encipherment, client auth]
EOF
kubectl certificate approve "$CSR_NAME" >/dev/null
CERT=""
for _ in $(seq 1 20); do
  CERT=$(kubectl get csr "$CSR_NAME" -o jsonpath='{.status.certificate}' 2>/dev/null || true)
  [ -n "$CERT" ] && break; sleep 0.5
done
kubectl delete csr "$CSR_NAME" >/dev/null 2>&1 || true
[ -n "$CERT" ] || { echo "!! cluster signer did not issue a cert (auto-approve off? approve ${CSR_NAME} manually)" >&2; exit 1; }
echo "$CERT" | base64 -d > "$BUNDLE/node.crt"

# 3. Write a kubeconfig that trusts the REAL cluster CA (full server verification).
{
  echo "apiVersion: v1"; echo "kind: Config"
  echo "clusters: [{name: cluster, cluster: {server: ${NODE_SERVER}, certificate-authority-data: ${CA_DATA}}}]"
  echo "users: [{name: node, user: {client-certificate-data: $(base64 -w0 "$BUNDLE/node.crt"), client-key-data: $(base64 -w0 "$BUNDLE/node.key")}}]"
  echo "contexts: [{name: node@cluster, context: {cluster: cluster, user: node}}]"
  echo "current-context: node@cluster"
} > "$BUNDLE/kubeconfig"
rm -f "$BUNDLE/node.crt" "$BUNDLE/node.key"   # now embedded in the kubeconfig

# 4. Node settings + DNS. Different clusters have different DNS/hostnames, so the
#    node reads these instead of guessing.
APISERVER_HOST=$(echo "$NODE_SERVER" | sed -E 's#^https?://##; s#[:/].*$##')
{
  echo "NODE_NAME=${NODE}"
  [ -n "$NODE_IP" ]      && echo "NODE_IP=${NODE_IP}"
  [ -n "$CLUSTER_DNS" ]  && echo "CLUSTER_DNS=${CLUSTER_DNS}"
  [ -n "$TAINTS" ]       && echo "TAINTS=${TAINTS}"
  echo "APISERVER_HOST=${APISERVER_HOST}"
} > "$BUNDLE/node.env"
# Optional: pin the apiserver hostname so the node resolves it without cluster DNS.
if [ -n "$APISERVER_IP" ] && ! echo "$APISERVER_HOST" | grep -qE '^[0-9.]+$'; then
  echo "${APISERVER_IP} ${APISERVER_HOST}" > "$BUNDLE/hosts"
  echo "==> pinning ${APISERVER_HOST} -> ${APISERVER_IP} in the node's /etc/hosts"
fi
# Optional: ship a resolv.conf if the cluster DNS should be the node's resolver.
[ -n "$CLUSTER_DNS" ] && printf 'nameserver %s\n' "$CLUSTER_DNS" > "$BUNDLE/resolv.conf"
: > "$BUNDLE/.asterkube-join"   # marker so the node recognizes this disk

echo "==> bundle contents:"; ls -1 "$BUNDLE" | sed 's/^/     /'

# 5. Emit either a directory (for virtio-fs) or an ext2 disk image (attachable).
if [ -n "$DIRONLY" ]; then
  mkdir -p "$DIRONLY"; cp -a "$BUNDLE/." "$DIRONLY/"
  echo "==> wrote join bundle dir: $DIRONLY  (share it via virtio-fs, tag=asterkubecfg)"
else
  command -v mke2fs >/dev/null || { echo "!! mke2fs (e2fsprogs) not found; use --dir instead" >&2; exit 1; }
  # 16 MiB ext2, populated rootlessly from the bundle dir; label asterkubecfg.
  # Asterinas' ext2 driver is deliberately minimal: it mounts ONLY 4096-byte-block
  # filesystems (super_block.rs requires log_block_size==2) and rejects the
  # resize_inode ro-compat feature. mke2fs defaults to 1024-byte blocks +
  # resize_inode for a small fs, which the driver refuses with EINVAL — so pin the
  # block size and drop resize_inode to produce a disk the node can actually mount.
  rm -f "$OUT"; truncate -s 16M "$OUT"
  mke2fs -q -t ext2 -b 4096 -O ^resize_inode -L asterkubecfg -d "$BUNDLE" "$OUT" >/dev/null
  echo "==> wrote join bundle image: $OUT ($(ls -lh "$OUT" | awk '{print $5}'), ext2, label=asterkubecfg)"
  echo "    attach it to the VM:"
  echo "      JOIN_BUNDLE=$OUT ./run-host-qemu.sh asterkube"
  echo "    or manually:"
  echo "      -drive if=none,format=raw,id=join,file=$OUT -device virtio-blk-pci,drive=join,serial=asterkubecfg"
fi
