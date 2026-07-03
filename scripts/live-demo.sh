#!/usr/bin/env bash
#
# live-demo.sh — one command to bring the astrokube node up as a LIVE,
# interactive Kubernetes node and tear it down gracefully.
#
#   astrokube/live-demo.sh            boot, wait until the node is LIVE, then
#                                     hand control back to you. Ctrl-C powers the
#                                     node off the way a real node shuts down:
#                                     an ACPI power-button event the kernel turns
#                                     into a graceful in-guest drain.
#
#   astrokube/live-demo.sh --restage  re-stage the virtio-fs share + kubeconfig
#                                     first (needed after a host reboot — the
#                                     share lives on tmpfs).
#
# Prereqilites already covered by the project tooling: the dev container
# `astrokube` is running and target/osdk/aster-kernel-osdk-bin.iso is built.
# This is local host tooling (the astrokube/ dir is gitignored), not in the repo.

set -u
cd "$(dirname "$0")/../asterinas"   # asterinas/

ISO=target/osdk/aster-kernel-osdk-bin.iso
QMP_SOCK=astrokube-qmp.sock
BOOT_LOG=astrokube-live.log
LIVE_MARK="astrokube node is LIVE"
RESTAGE=0
[ "${1:-}" = "--restage" ] && RESTAGE=1

note() { printf '\033[1;36m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!!  %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31mxx  %s\033[0m\n' "$*" >&2; exit 1; }

# --- preflight ---------------------------------------------------------------
command -v qemu-system-x86_64 >/dev/null || die "qemu-system-x86_64 not found"
[ -f "$ISO" ] || die "missing $ISO — build it: docker exec astrokube bash -lc 'cd /root/asterinas/kernel && cargo osdk build --release --strip-elf --grub-boot-protocol=multiboot2'"

if [ "$RESTAGE" = 1 ]; then
  note "re-staging virtio-fs share + node kubeconfig"
  bash astrokube/stage-virtiofs.sh || die "stage-virtiofs failed"
  bash astrokube/make-node-kubeconfig.sh astrokube || warn "make-node-kubeconfig failed (node may not register)"
fi

note "killing any stale VM and refreshing the ext2 disk"
pkill -9 qemu-system-x86 2>/dev/null
pkill -9 virtiofsd 2>/dev/null
rm -f "$QMP_SOCK"
sleep 1
bash astrokube/fresh-ext2.sh >/dev/null || die "fresh-ext2 failed"

# --- graceful shutdown via the ACPI power button -----------------------------
powerdown() {
  [ -S "$QMP_SOCK" ] || return 0
  python3 - "$QMP_SOCK" <<'PY' 2>/dev/null || true
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])
s.recv(4096)
s.sendall(b'{"execute":"qmp_capabilities"}\n'); time.sleep(0.2); s.recv(4096)
s.sendall(b'{"execute":"system_powerdown"}\n'); time.sleep(0.2)
PY
}

shutting_down=0
on_int() {
  [ "$shutting_down" = 1 ] && return
  shutting_down=1
  echo
  note "ACPI power button -> graceful node shutdown (the kernel signals PID 1)"
  powerdown
  for _ in $(seq 1 15); do
    grep -q "node stopped. Powering off" "$BOOT_LOG" 2>/dev/null && break
    kill -0 "$QEMU_PID" 2>/dev/null || break
    sleep 1
  done
  if grep -q "node stopped. Powering off" "$BOOT_LOG" 2>/dev/null; then
    note "node drained and powered off cleanly."
  else
    warn "node did not confirm a clean drain; forcing off."
    kill -9 "$QEMU_PID" 2>/dev/null
  fi
  exit 0
}

# --- boot --------------------------------------------------------------------
: > "$BOOT_LOG"
note "booting the astrokube node (log: $BOOT_LOG)"
./run-host-qemu.sh astrokube > "$BOOT_LOG" 2>&1 &
sleep 6
QEMU_PID=$(pgrep -n qemu-system-x86)
[ -n "$QEMU_PID" ] || die "qemu failed to start — see $BOOT_LOG"
trap on_int INT TERM

note "waiting for the node to come up (typically 3-4 min on a single vCPU)..."
LIVE=0
for _ in $(seq 1 80); do
  if grep -q "$LIVE_MARK" "$BOOT_LOG" 2>/dev/null; then LIVE=1; break; fi
  kill -0 "$QEMU_PID" 2>/dev/null || die "qemu exited during boot — tail of $BOOT_LOG:
$(tail -5 "$BOOT_LOG")"
  sleep 6
done
[ "$LIVE" = 1 ] || { warn "node did not report LIVE in time; tail of $BOOT_LOG:"; tail -8 "$BOOT_LOG"; on_int; }

# --- live ---------------------------------------------------------------------
bar=$(printf '\033[1;32m%s\033[0m' "========================================================================")
printf '\n%s\n' "$bar"
cat <<EOF
 The astrokube node is LIVE and persistent.

 From another terminal on this host:
   kubectl get nodes -o wide                  # 'astrokube' shows Ready
   kubectl get pods -A -o wide | grep astrokube

 Schedule a workload onto it (tolerates the experimental taint):
   cat <<'POD' | kubectl apply -f -
   apiVersion: v1
   kind: Pod
   metadata: { name: astrokube-live, namespace: default }
   spec:
     nodeSelector: { kubernetes.io/hostname: astrokube }
     tolerations: [{ operator: Exists }]
     hostNetwork: true
     restartPolicy: Never
     containers:
     - { name: pause, image: registry.k8s.io/pause:3.10.1, imagePullPolicy: Never }
   POD
   kubectl get pod astrokube-live -o wide -w   # -> Running on astrokube

 Live boot log: tail -f $BOOT_LOG

 Press Ctrl-C here to shut the node down gracefully
 (ACPI power button -> kernel -> SIGINT to PID 1 -> drain -> poweroff).
EOF
printf '%s\n\n' "$bar"

# Keep the node up until the operator asks to stop, or the VM exits on its own.
while kill -0 "$QEMU_PID" 2>/dev/null; do sleep 2; done
note "the VM exited."
