#!/bin/bash
# asterkube_demo.sh — download and boot the asterkube v0.1 release image under QEMU.
# Copy this single file to any Linux box with qemu + OVMF + KVM and run it.
#
#   ./asterkube_demo.sh                      # download + boot the standalone demo (ISO)
#   ./asterkube_demo.sh --qcow2              # boot the QCOW2 disk instead
#   ./asterkube_demo.sh /path/img.iso        # boot an image you already have
#
#   ./asterkube_demo.sh --kubeconfig         # JOIN the cluster in your current kubectl
#                                            # context (uses ~/.kube/config or $KUBECONFIG)
#   ./asterkube_demo.sh --kubeconfig ~/.kube/other.yaml --node-name web-1
#   ./asterkube_demo.sh --kubeconfig --apiserver 192.168.1.50:6443   # override endpoint
#
# In --kubeconfig mode the script reads your control-plane endpoint, mints a
# short-lived kubeadm bootstrap token in the cluster (a write to kube-system),
# patches those coordinates into the ISO's kernel cmdline (no rebuild), and boots
# a node that registers with `kubectl get nodes`.
#
# The node boots in seconds and STAYS LIVE. Shut it down with the QEMU monitor:
# Ctrl-a then c, type `system_powerdown`. Ctrl-a then x quits QEMU.
set -euo pipefail
trap 'rc=$?; [ $rc -ne 0 ] && echo "asterkube_demo.sh: aborted at line $LINENO (exit $rc)" >&2' ERR

TAG=asterkube-v0.1
BASE="https://github.com/jboero/asterkube/releases/download/${TAG}"

# SIGPIPE-safe [a-z0-9] generator (tr </dev/urandom | head trips pipefail via SIGPIPE).
rnd(){ local s; s=$(LC_ALL=C tr -dc 'a-z0-9' < <(head -c $(( $1 * 16 )) /dev/urandom)); printf '%s' "${s:0:$1}"; }

IMG=""; JOIN=0; KCFG=""; NODE=""; APISERVER_OVERRIDE=""; SECURE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --qcow2) IMG="asterkube-node-v0.1.qcow2"; shift ;;
    --iso)   IMG="asterkube-node-v0.1.iso"; shift ;;
    --kubeconfig) JOIN=1; shift
                  case "${1:-}" in ""|--*) : ;; *) KCFG="$1"; shift ;; esac ;;
    --node-name)  NODE="$2"; shift 2 ;;
    --apiserver)  APISERVER_OVERRIDE="$2"; shift 2 ;;
    --secure)     SECURE=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) IMG="$1"; shift ;;
  esac
done
[ -n "$IMG" ] || IMG="asterkube-node-v0.1.iso"
[ "$JOIN" = 1 ] && case "$IMG" in *.iso) : ;; *) echo "--kubeconfig requires the ISO (cmdline patch); drop --qcow2" >&2; exit 1 ;; esac

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1 ($2)" >&2; exit 1; }; }
need qemu-system-x86_64 "qemu-system-x86 / qemu-kvm"

# --- download (if missing) + verify -----------------------------------------
if [ ! -f "$IMG" ]; then
  echo "==> downloading $IMG"
  curl -fL# -C - -o "$IMG" "${BASE}/${IMG}"
  if command -v sha256sum >/dev/null 2>&1; then
    curl -fsSL -o SHA256SUMS "${BASE}/SHA256SUMS" 2>/dev/null || true
    [ -f SHA256SUMS ] && grep "  ${IMG##*/}\$" SHA256SUMS | sha256sum -c - || true
  fi
fi
[ -f "$IMG" ] || { echo "image not found: $IMG" >&2; exit 1; }

# --- --kubeconfig: derive join args, mint a token, patch the ISO cmdline ----
BOOT_IMG="$IMG"
if [ "$JOIN" = 1 ]; then
  need kubectl "install kubectl"; need xorriso "install xorriso"
  export KUBECONFIG="${KCFG:-${KUBECONFIG:-$HOME/.kube/config}}"
  CTX=$(kubectl config current-context)
  echo "==> joining cluster from context: $CTX  ($KUBECONFIG)"

  if [ -n "$APISERVER_OVERRIDE" ]; then
    APISERVER="$APISERVER_OVERRIDE"
  else
    # Prefer the apiserver's REAL endpoint (what ClusterIP 10.96.0.1:443 maps to),
    # from the `kubernetes` service endpoints. The kubeconfig `server` is often a
    # loopback port-forward (e.g. kind's docker-proxy) that mangles the kubelet's
    # HTTP/2 CSR over the demo NIC; the real endpoint (a pod/host-network IP) is
    # reachable guest->slirp->host and avoids that.
    EIP=$(kubectl get endpoints kubernetes -n default -o 'jsonpath={.subsets[0].addresses[0].ip}' 2>/dev/null)
    EPORT=$(kubectl get endpoints kubernetes -n default -o 'jsonpath={.subsets[0].ports[0].port}' 2>/dev/null)
    if [ -n "$EIP" ] && [ -n "$EPORT" ]; then
      APISERVER="$EIP:$EPORT"
      echo "    using real apiserver endpoint $APISERVER (bypasses any loopback port-forward)"
    else
      SRV=$(kubectl config view --minify -o 'jsonpath={.clusters[0].cluster.server}')
      HP=${SRV#*://}; HOST=${HP%%:*}; PORT=${HP##*:}; [ "$PORT" = "$HP" ] && PORT=6443
      case "$HOST" in 127.0.0.1|localhost|::1|0.0.0.0)
        echo "    control-plane is loopback ($HOST) — remapping to slirp host 10.0.2.2"; HOST=10.0.2.2 ;; esac
      APISERVER="$HOST:$PORT"
    fi
  fi
  NODE="${NODE:-asterkube-$(rnd 5)}"

  # Mint a short-lived kubeadm bootstrap token (auto-approved into a node cert).
  TID=$(rnd 6)
  TSECRET=$(rnd 16)
  EXP=$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+2H +%Y-%m-%dT%H:%M:%SZ)
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: { name: bootstrap-token-${TID}, namespace: kube-system }
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${TID}"
  token-secret: "${TSECRET}"
  expiration: "${EXP}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: system:bootstrappers:kubeadm:default-node-token
EOF
  echo "    minted bootstrap token ${TID}.****  (expires ${EXP})"

  ARGS="ASTERKUBE_APISERVER=${APISERVER} ASTERKUBE_TOKEN=${TID}.${TSECRET} ASTERKUBE_NODE_NAME=${NODE} ASTERKUBE_INSECURE=1"
  if [ "$SECURE" = 1 ] && command -v openssl >/dev/null 2>&1; then
    CAHASH=$(kubectl config view --minify --raw -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}' \
      | base64 -d 2>/dev/null | openssl x509 -pubkey -noout 2>/dev/null \
      | openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null | awk '{print $2}') || CAHASH=""
    [ -n "$CAHASH" ] && ARGS="ASTERKUBE_APISERVER=${APISERVER} ASTERKUBE_TOKEN=${TID}.${TSECRET} ASTERKUBE_NODE_NAME=${NODE} ASTERKUBE_CA_HASH=sha256:${CAHASH}"
  fi

  echo "==> patching ISO cmdline: node=${NODE} apiserver=${APISERVER}"
  BOOT_IMG="asterkube-node-${NODE}.iso"
  cp -f "$IMG" "$BOOT_IMG"
  GC=$(mktemp); GC2=$(mktemp)
  xorriso -osirrox on -indev "$BOOT_IMG" -extract /boot/grub/grub.cfg "$GC" 2>/dev/null
  grep -q ' init=/sbin/init' "$GC" || { echo "ERROR: $IMG doesn't look like the asterkube release ISO (no init= in grub.cfg)." >&2; exit 1; }
  sed "s# init=/sbin/init# ${ARGS} init=/sbin/init#" "$GC" > "$GC2"
  xorriso -dev "$BOOT_IMG" -boot_image any keep -update "$GC2" /boot/grub/grub.cfg -commit >/dev/null 2>&1
  # VERIFY the join args actually landed in the ISO — never boot a non-joining image silently.
  xorriso -osirrox on -indev "$BOOT_IMG" -extract /boot/grub/grub.cfg "$GC2" 2>/dev/null
  if ! grep -q 'ASTERKUBE_APISERVER=' "$GC2"; then
    echo "ERROR: ISO patch failed — join args are NOT in $BOOT_IMG (xorriso problem?)." >&2
    echo "       Refusing to boot a standalone image that would falsely claim it joined." >&2
    rm -f "$GC" "$GC2"; exit 1
  fi
  rm -f "$GC" "$GC2"
  echo "    patched OK — at boot you should see 'boot-args cluster join' + 'registering node ${NODE}'."
  echo "    then on the host:  kubectl get nodes | grep ${NODE}"
fi

# --- OVMF firmware + KVM -----------------------------------------------------
find_ovmf() { for d in /usr/share/edk2/ovmf /usr/share/OVMF /usr/share/edk2-ovmf/x64 /usr/share/qemu; do
  for c in OVMF_CODE.fd OVMF_CODE.4m.fd OVMF_CODE_4M.fd; do [ -f "$d/$c" ] && { echo "$d/$c"; return; }; done; done; }
OVMF_CODE=${OVMF_CODE:-$(find_ovmf)}
[ -n "$OVMF_CODE" ] || { echo "OVMF not found — install edk2-ovmf (Fedora) / ovmf (Debian/Ubuntu), or set OVMF_CODE=" >&2; exit 1; }
OVMF_VARS_SRC=${OVMF_VARS_SRC:-$(dirname "$OVMF_CODE")/OVMF_VARS.fd}
[ -f "$OVMF_VARS_SRC" ] || OVMF_VARS_SRC=$(dirname "$OVMF_CODE")/OVMF_VARS_4M.fd
VARS=$(mktemp /tmp/asterkube-OVMF_VARS.XXXXXX.fd); cp "$OVMF_VARS_SRC" "$VARS"; trap 'rm -f "$VARS"' EXIT

ACCEL=(); CPU=(-cpu qemu64)
if [ -w /dev/kvm ]; then ACCEL=(-accel kvm); CPU=(-cpu host); else echo "==> WARNING: no KVM — slow TCG emulation"; fi

case "$BOOT_IMG" in
  *.iso) MEDIA=(-cdrom "$BOOT_IMG" -boot d) ;;
  *)     MEDIA=(-drive if=none,format=qcow2,id=disk0,file="$BOOT_IMG"
               -device virtio-blk-pci,drive=disk0,disable-legacy=on,disable-modern=off,bootindex=0) ;;
esac

echo "==> booting $BOOT_IMG   (Ctrl-a c = monitor -> 'system_powerdown' | Ctrl-a x = quit)"
exec qemu-system-x86_64 \
  -machine q35,kernel-irqchip=split "${ACCEL[@]}" "${CPU[@]},+x2apic" -m 4G -smp 1 \
  --no-reboot -nographic -serial null \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$VARS" \
  -chardev stdio,id=mux,mux=on,signal=off \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off \
  -device virtconsole,chardev=mux -monitor chardev:mux \
  -object rng-random,id=rng0,filename=/dev/urandom \
  -device virtio-rng-pci,rng=rng0,disable-legacy=on,disable-modern=off \
  "${MEDIA[@]}" \
  -netdev user,id=net01 \
  -device virtio-net-pci,netdev=net01,disable-legacy=on,disable-modern=off,mrg_rxbuf=off,ctrl_rx=off,ctrl_rx_extra=off,ctrl_vlan=off,ctrl_vq=off,ctrl_guest_offloads=off,ctrl_mac_addr=off,event_idx=off,queue_reset=off,guest_announce=off,indirect_desc=off
