#!/bin/bash
# make-node-kubeconfig.sh — mint a kubelet kubeconfig that lets an Asterinas
# node join the local Kind cluster's apiserver.
#
# Strategy: use the cluster's own CertificateSigningRequest API (NOT the cluster
# CA private key). Generate the node key locally, submit a CSR to the
# kube-apiserver-client-kubelet signer with identity CN=system:node:<node>,
# O=system:nodes, approve it with admin kubectl, and pull back the signed cert.
# The CA private key never leaves the cluster. That identity is what the Node
# authorizer + the built-in system:node ClusterRole grant, so the node is
# authorized with no extra RBAC.
#
# The apiserver is reached from the guest over QEMU slirp at 10.0.2.2:<hostport>.
# The server cert SANs don't include 10.0.2.2, so the kubeconfig uses
# insecure-skip-tls-verify — the node still authenticates to the apiserver with
# its CA-signed client cert; only server-name verification is skipped. This is a
# first-join scaffold, not the final posture.
#
# Usage: asterkube/make-node-kubeconfig.sh [node-name] [share-dir]
set -euo pipefail
cd "$(dirname "$0")/../asterinas"                       # asterinas/

NODE=${1:-asterkube}
SHARE=${2:-/tmp/asterkube-vfs}
CSR_NAME="asterkube-node-${NODE}"
WORK=$(mktemp -d /tmp/asterkube-kubeconfig.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

# Host port the apiserver is published on (maps to the control-plane's 6443).
SERVER_HOSTPORT=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
PORT=${SERVER_HOSTPORT##*:}
# From the guest, the host is the slirp gateway 10.0.2.2.
GUEST_SERVER="https://10.0.2.2:${PORT}"
echo "==> apiserver: host=${SERVER_HOSTPORT}  guest-view=${GUEST_SERVER}"

echo "==> generating node key + CSR (CN=system:node:${NODE}, O=system:nodes)"
openssl genrsa -out "$WORK/node.key" 2048 >/dev/null 2>&1
openssl req -new -key "$WORK/node.key" -out "$WORK/node.csr" \
  -subj "/O=system:nodes/CN=system:node:${NODE}" >/dev/null 2>&1

echo "==> submitting CertificateSigningRequest ${CSR_NAME} to the cluster signer"
kubectl delete csr "$CSR_NAME" >/dev/null 2>&1 || true
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: ${CSR_NAME}
spec:
  request: $(base64 -w0 "$WORK/node.csr")
  signerName: kubernetes.io/kube-apiserver-client-kubelet
  usages: [digital signature, key encipherment, client auth]
EOF

echo "==> approving and retrieving the signed certificate"
kubectl certificate approve "$CSR_NAME" >/dev/null
# The signer populates .status.certificate asynchronously; wait briefly.
for _ in $(seq 1 20); do
  CERT=$(kubectl get csr "$CSR_NAME" -o jsonpath='{.status.certificate}' 2>/dev/null || true)
  [ -n "$CERT" ] && break
  sleep 0.5
done
if [ -z "${CERT:-}" ]; then
  echo "ERROR: the cluster signer did not issue a certificate for ${CSR_NAME}" >&2
  exit 1
fi
echo "$CERT" | base64 -d > "$WORK/node.crt"
kubectl delete csr "$CSR_NAME" >/dev/null 2>&1 || true

b64() { base64 -w0 "$1"; }
KUBECONFIG_OUT="$SHARE/kubeconfig"
mkdir -p "$SHARE"
cat > "$KUBECONFIG_OUT" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: kind
  cluster:
    server: ${GUEST_SERVER}
    insecure-skip-tls-verify: true
users:
- name: node
  user:
    client-certificate-data: $(b64 "$WORK/node.crt")
    client-key-data: $(b64 "$WORK/node.key")
contexts:
- name: node@kind
  context:
    cluster: kind
    user: node
current-context: node@kind
EOF
chmod 0644 "$KUBECONFIG_OUT"
echo "==> wrote ${KUBECONFIG_OUT} (node=${NODE}, server=${GUEST_SERVER})"
ls -l "$KUBECONFIG_OUT"

# In-cluster clients (e.g. the kube-proxy DaemonSet) connect to the apiserver by
# its cluster hostname, which the guest's slirp DNS can't resolve. Stage a hosts
# mapping the init appends to the node's /etc/hosts; the control-plane container
# is reachable from the guest over slirp by its Docker-network IP.
CP_NAME=$(docker ps --filter name=control-plane --format '{{.Names}}' | head -1)
if [ -n "$CP_NAME" ]; then
  CP_IP=$(docker inspect "$CP_NAME" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
  echo "${CP_IP} ${CP_NAME}" > "$SHARE/hosts-extra"
  echo "==> wrote ${SHARE}/hosts-extra ($(cat "$SHARE/hosts-extra"))"
fi
