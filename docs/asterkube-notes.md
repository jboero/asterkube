# asterkube — a Kubernetes node with no C and no libc

**Goal:** boot a Kubernetes node directly on the [Asterinas](https://asterinas.github.io/)
Rust kernel, where the init process (PID 1) is a small Go program that doubles as
the node agent. No C, no libc — a Rust kernel and a Go userspace.

This directory documents the experiment and how to reproduce it. It lives on the
`asterkube` branch. **This branch is the kernel-side work**; the PID 1 init is a
separate, out-of-tree static Go binary (referred to here as `asterkube-init`) and
is not part of this repository.

## Status: the node runs pods ✅

The Go init now acts as a **node agent**: it reads static pod manifests
(Kubernetes `--pod-manifest-path` style) from `/etc/asterkube/pods/*.json` and runs
each as an isolated, resource-limited container — exercising every kernel feature
built for Part 1, end to end. Verified output (`pod-on-node-verified.log`):

```
asterkube-init: node agent starting (static-pod mode)
asterkube-init: starting pod "hello" (mem=128MiB cpu=50%, namespaces=pid,uts,ipc,net,mnt)
  pod[hello] container started: pid=1 hostname="hello-pod"      # PID + UTS namespaces
  pod[hello] isolated loopback: ok (127.0.0.1:32768)            # NET namespace (own lo)
  pod[hello] workload: ok (allocated 8MiB, computed checksum …) # runs under cgroup limits
  pod[hello] container exiting cleanly
asterkube-init: pod "hello" completed successfully
asterkube-init: starting pod "web" (mem=64MiB cpu=25%, …)
  pod[web]   container started: pid=1 hostname="web-pod"
  pod[web]   isolated loopback: ok (127.0.0.1:32768)            # same port, isolated netns!
  ...
```

Both pods get PID 1, distinct hostnames, and **each binds the same
`127.0.0.1:32768`** in its own network namespace — proving real per-pod
isolation. Each runs in a cgroup with enforced `memory.max`/`cpu.max`. The node
agent uses the sync-pipe pattern (move the container into its cgroup before it
runs, as runc does) and a per-pod watchdog so one bad pod can't wedge the node.
The node-agent logic lives in the out-of-tree Go init.

This is the milestone the whole experiment was aiming at: **a C-free node — Rust
kernel, Go init — that boots, becomes the node agent, and actually runs
containerized pods with namespace isolation and enforced resource limits.** What
it does *not* yet do is give non-hostNetwork pods routable IPs (needs veth) or
program Services (needs nftables) — see [`KERNEL-CHANGES.md`](KERNEL-CHANGES.md).

## Architecture

```
            ┌──────────────────────────────────────────────┐
            │ QEMU (q35, KVM) — all virtio devices          │
            │                                               │
            │   Asterinas kernel (Rust, framekernel)        │
            │     multiboot2, GRUB rescue ISO               │
            │        │ unpacks initramfs.cpio.gz            │
            │        ▼                                       │
            │   /sbin/init ──symlink──▶ asterkube-init       │
            │        │ (static CGO_ENABLED=0 Go binary)      │
            │        ▼                                       │
            │   asterkube-init: getpid()==1 ?               │
            │        ├─ yes ▶ act as system init             │
            │        │        mount proc,sys,cgroup2,...      │
            │        │        mount virtio-blk ext2/exfat     │
            │        │        run pods as the node agent      │
            │        └─ no  ▶ defer to normal (non-init) mode │
            └──────────────────────────────────────────────┘
```

The PID 1 init is a small static Go binary (`CGO_ENABLED=0`), built out of tree —
it is **not** part of this kernel branch. When it detects `getpid() == 1` it acts
as the system init and node agent; otherwise it is a no-op. This branch contains
only the **Asterinas kernel changes** that make such a node possible.

## Milestone 1 — boot + mount + halt (DONE ✅)

PID 1 detection, filesystem mounting (virtual + virtio block), and clean
poweroff. Captured console output is in [`milestone1-boot.log`](milestone1-boot.log):

```
 asterkube-init: running as PID 1 on the Asterinas kernel
 Rust kernel + Go init, no C, no libc.

asterkube-init: mounting filesystems
  [ ok ] /proc                (proc)
  [ ok ] /sys                 (sysfs)
  [skip] /dev                 (devtmpfs): already populated by kernel
  [ ok ] /tmp                 (tmpfs)
  [ ok ] /run                 (tmpfs)
  [ ok ] /sys/fs/cgroup       (cgroup2)
  [ ok ] /sys/kernel/config   (configfs)
  [ ok ] /ext2                (ext2)     # virtio-blk /dev/vda
  [ ok ] /exfat               (exfat)    # virtio-blk /dev/vdb
asterkube-init: milestone 1 complete (filesystems mounted). Powering off.
```

Notable: Asterinas already supports mounting **cgroup2** and **configfs**, and
exposes virtio block devices as `/dev/vda` / `/dev/vdb`. It does *not* support a
`devtmpfs` mount — `/dev` is pre-populated by the kernel, so the init skips it.

## How to reproduce

Everything runs inside the Asterinas dev container (it carries the pinned Rust
nightly, `cargo-osdk`, GRUB, QEMU and `VDSO_LIBRARY_DIR`).

### 1. Build the Go init and stage the initramfs (on the host)

Build the out-of-tree `asterkube-init` Go program as a static binary, then stage a
minimal initramfs around it (no busybox, no C/libc):

```bash
# Build your asterkube-init Go binary (static, CGO disabled) to /tmp/asterkube-init,
# e.g. CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/asterkube-init <pkg>

# Minimal initramfs: the init binary + a /sbin/init symlink to it.
ROOT=/tmp/asterkube-initramfs
mkdir -p "$ROOT"/{usr/bin,sbin,proc,sys,dev,tmp,run,ext2,exfat}
cp /tmp/asterkube-init "$ROOT/usr/bin/asterkube-init"
ln -sf ../usr/bin/asterkube-init "$ROOT/sbin/init"
( cd "$ROOT" && find . -print0 | cpio --null -o -H newc --owner=0:0 \
    | gzip -9 ) > asterinas/test/initramfs/build/initramfs.cpio.gz
```

### 2. Build + boot the kernel (inside the container)

```bash
docker run -d --name asterkube --privileged --network=host -v /dev:/dev \
    -v "$PWD/asterinas:/root/asterinas" -w /root/asterinas \
    asterinas/asterinas:0.18.0-20260603 sleep infinity

docker exec asterkube bash -lc 'OSDK_LOCAL_DEV=1 cargo install cargo-osdk --path osdk'
docker exec asterkube bash asterkube-run.sh      # creates virtio disk images, then cargo osdk run
```

The console (hvc0 → virtio-console) is logged to `qemu.log` in the container.

## Boot configuration

[`../OSDK.toml`](../OSDK.toml) `[run.boot]` is set for asterkube:

- `init=/sbin/init` (the symlink to `asterkube-init`), `console=hvc0`, empty
  `init_args` (no shell — asterkube ships no busybox).
- `initramfs = test/initramfs/build/initramfs.cpio.gz` (our minimal image, *not*
  the Nix-built test rootfs).

## Roadmap

- [x] **Milestone 1:** PID 1 detection, mount filesystems, halt.
- [x] **Capability probe:** scope cgroup v2 + namespace support — see
  [`PART1-GAP-ANALYSIS.md`](PART1-GAP-ANALYSIS.md) and [`capability-probe.log`](capability-probe.log).
- [x] **Kernel Part 1, batch 1** — see [`KERNEL-CHANGES.md`](KERNEL-CHANGES.md)
  (all verified booting + fmt/clippy clean):
  - [x] cgroup v2 memory controller stores limits (`memory.max` writable).
  - [x] NET namespaces: `CLONE_NEWNET` + `setns` + `/proc/<pid>/ns/net`.
  - [x] PID namespace object + `/proc/<pid>/ns/pid`.
  - [x] **PID-namespace renumbering** — `CLONE_NEWPID` works; container init sees
    `getpid()==1` (verified, [`pid-renumbering-verified.log`](pid-renumbering-verified.log)).
- [x] **NET-namespace loopback isolation** — each netns has its own `lo`; two
    namespaces bind the same `127.0.0.1:port` independently (verified,
    [`netns-isolation-verified.log`](netns-isolation-verified.log)).
- [x] **cgroup `memory.max` enforcement** — a process in a 32 MiB-limited cgroup
    is killed allocating 128 MiB; root/init unaffected (verified,
    [`memory-enforcement-verified.log`](memory-enforcement-verified.log)).
- [x] **cgroup `cpu.max` enforcement** — a 10%-quota cgroup does ~7% of an
    unthrottled run's CPU work; root/init unaffected (verified,
    [`cpu-enforcement-verified.log`](cpu-enforcement-verified.log)).
- [x] **veth between namespaces** — a pod gets a routable IP and exchanges UDP
    with the host over a veth pair; cross-namespace connectivity verified 6/6
    ([`veth-on-node-verified.log`](veth-on-node-verified.log)). Also fixed an
    IRQ-context spinlock deadlock in the `cpu.max` path found along the way.
- [x] **RTNETLINK veth creation** — the netlink route protocol now handles the
    write ops a CNI plugin uses: `RTM_NEWLINK` (create a veth pair, place the peer
    in a pod's netns by `IFLA_NET_NS_PID`) and `RTM_NEWADDR` (assign addresses),
    all network-namespace-aware. A pod's veth is now wired with **real netlink**
    (a tiny Go netlink client) instead of the earlier `prctl` shim, which is gone.
    Cross-namespace connectivity verified 4/4 ([`rtnetlink-veth-verified.log`](rtnetlink-veth-verified.log)).
- [ ] cgroup `cpu.weight` proportional shares (needs hierarchical scheduling).
- [ ] Networking next layers: `IFLA_NET_NS_FD`/`RTM_SETLINK`/`RTM_NEWROUTE`,
    per-ns kernel netlink sockets, Services/NAT (nftables for `kube-proxy`).
- [ ] cgroup cpu/memory enforcement (wire stored limits to scheduler / MM).
- [x] **Node agent runs pods** — the Go PID 1 init reads static pod manifests and
  runs them as isolated, resource-limited containers (verified,
  [`pod-on-node-verified.log`](pod-on-node-verified.log)).
- [ ] Stay alive as PID 1: reap zombies (SIGCHLD/`wait`), handle signals.
- [ ] Per-pod networking (veth) + Services (nftables) for routable pod IPs.
- [ ] Hand off to a full node agent / container runtime (CRI).

## Part 1 gap analysis (summary)

Verified live + against source. **Already works:** cgroup v2 hierarchy with
controller delegation (`cpu`/`memory`/`pids`/`cpuset` advertised; `pids` actually
*enforced*); mount/uts/ipc/cgroup namespaces; overlay/virtiofs/ext2/exfat
filesystems. **Missing (blocks Kubernetes):** PID namespaces and NET namespaces
(both rejected with EINVAL — not implemented); user namespaces; and cpu/memory
cgroup *enforcement* (interface files exist but don't limit). Full detail and
file pointers in [`PART1-GAP-ANALYSIS.md`](PART1-GAP-ANALYSIS.md).
