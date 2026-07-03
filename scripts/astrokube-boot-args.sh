#!/bin/bash
# astrokube-boot-args.sh — print the kernel boot args that bind a generic
# astrokube node image to YOUR cluster, kubeadm-style. Run it on a box with
# kubectl admin access to the TARGET cluster. It:
#   1. discovers the apiserver endpoint,
#   2. creates a short-lived bootstrap-token Secret (group
#      system:bootstrappers:kubeadm:default-node-token, which the cluster's
#      node-autoapprove-bootstrap RBAC auto-approves into a node cert),
#   3. computes the kubeadm CA pubkey hash (sha256 of the CA SubjectPublicKeyInfo),
#   4. prints ASTROKUBE_* boot args for both OSDK.toml `kcmd_args` and `-append`.
#
# The node reads these from the kernel cmdline (Asterinas passes key=value cmdline
# tokens to init as env), TLS-bootstraps with the token, and registers — no config
# drive, no cloud-init. See boot_args_join_linux.go.
#
# Usage: astrokube/astrokube-boot-args.sh [-n node-name] [-s apiserver-host:port]
#                                         [-d cluster-dns] [--secure]
set -euo pipefail

NODE="astrokube"
SERVER_OVERRIDE=""     # apiserver URL the node dials (default: slirp guest view)
CLUSTER_DNS=""
SECURE=""              # if set, emit CA hash + verify (needs apiserver addr in cert SANs)

while [ $# -gt 0 ]; do
  case "$1" in
    -n) NODE="$2"; shift 2 ;;
    -s) SERVER_OVERRIDE="$2"; shift 2 ;;
    -d) CLUSTER_DNS="$2"; shift 2 ;;
    --secure) SECURE=1; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done
command -v kubectl >/dev/null || { echo "!! kubectl not found" >&2; exit 1; }
command -v openssl >/dev/null || { echo "!! openssl not found" >&2; exit 1; }

# 1. apiserver endpoint. From the guest under QEMU slirp the host is 10.0.2.2.
HOST_SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
PORT=${HOST_SERVER##*:}
GUEST_SERVER=${SERVER_OVERRIDE:+https://$SERVER_OVERRIDE}
GUEST_SERVER=${GUEST_SERVER:-https://10.0.2.2:${PORT}}

# 2. bootstrap token: id=[a-z0-9]{6}, secret=[a-z0-9]{16}.
TID=$(openssl rand -hex 3)
TSECRET=$(openssl rand -hex 8)
EXPIRES=$(date -u -d '+24 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+24H +%Y-%m-%dT%H:%M:%SZ)
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${TID}"
  token-secret: "${TSECRET}"
  expiration: "${EXPIRES}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: system:bootstrappers:kubeadm:default-node-token
EOF
TOKEN="${TID}.${TSECRET}"
echo "==> created bootstrap token ${TID}.****  (expires ${EXPIRES})" >&2

# 3. kubeadm CA pubkey hash (sha256 of the CA's DER SubjectPublicKeyInfo).
CA_HASH=""
CA_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
if [ -n "$CA_DATA" ]; then
  HASH=$(echo "$CA_DATA" | base64 -d \
    | openssl x509 -noout -pubkey \
    | openssl pkey -pubin -outform DER 2>/dev/null \
    | openssl dgst -sha256 | awk '{print $NF}')
  [ -n "$HASH" ] && CA_HASH="sha256:${HASH}"
fi

# 4. Emit the boot args.
ARGS=( "ASTROKUBE_APISERVER=${GUEST_SERVER}" "ASTROKUBE_TOKEN=${TOKEN}" "ASTROKUBE_NODE_NAME=${NODE}" )
[ -n "$CLUSTER_DNS" ] && ARGS+=( "ASTROKUBE_CLUSTER_DNS=${CLUSTER_DNS}" )
if [ -n "$SECURE" ] && [ -n "$CA_HASH" ]; then
  ARGS+=( "ASTROKUBE_CA_HASH=${CA_HASH}" )
else
  # Default (slirp/dev): the apiserver's slirp address isn't in its cert SANs, so
  # skip server-name verification. The node still authenticates with the token.
  ARGS+=( "ASTROKUBE_INSECURE=1" )
  [ -n "$CA_HASH" ] && echo "==> (CA hash ${CA_HASH} available; pass --secure once the apiserver addr is in its cert SANs)" >&2
fi

echo
echo "# --- paste into OSDK.toml [run.boot] kcmd_args (then rebuild the ISO) ---"
for a in "${ARGS[@]}"; do echo "    \"$a\","; done
echo
echo "# --- or pass at launch via -append (direct kernel boot, no rebake) ---"
echo "${ARGS[*]}"
echo
echo "# target cluster: ${HOST_SERVER}  (node dials ${GUEST_SERVER})"
