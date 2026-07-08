<p align="center">
  <img src="assets/upbound-logo.png" alt="Upbound" height="64">
</p>

<h1 align="center">Asterkube</h1>

<p align="center">
  <b>Secure Kubernetes without Linux or C.</b><br>
  A bootable Kubernetes node built on the <a href="https://github.com/asterinas/asterinas">Asterinas</a>
  memory-safe Rust framekernel, with a CGO-free Go userspace — the real upstream
  <b>kubelet v1.35.6</b> running as PID&nbsp;1. No Linux kernel. No systemd. No libc.
</p>

<p align="center">
  <a href="https://medium.com/upbound-labs/secure-kubernetes-without-linux-or-c-7f8cf4c824e8"><img alt="Blog" src="https://img.shields.io/badge/read-the%20story%20on%20Medium-000000.svg"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
  <img alt="kernel: MPL-2.0" src="https://img.shields.io/badge/kernel-Asterinas%20(MPL--2.0)-brightgreen.svg">
  <img alt="kubernetes v1.35.6" src="https://img.shields.io/badge/kubernetes-v1.35.6-326ce5.svg">
</p>

---

## Why

CoreOS pioneered the minimal, immutable Linux VM for running Kubernetes securely — but
it still rode on the Linux kernel, systemd, and glibc, and those C-based layers have
shipped **thousands of memory-unsafe CVEs** in the years since (the Linux kernel alone
has ~11,000–12,000 CVEs, roughly half memory-safety bugs). These get exploited in the
wild and cost billions in ransomware and data compromise every year. Just as this was
being written, a 15-year-old use-after-free in Linux's futex code
([GhostLock, CVE-2026-43499](https://thehackernews.com/)) surfaced — giving any local
user root and enabling container escape on most distros.

A VM doesn't need a hardware kernel. **The hypervisor** already does the hard job —
device drivers, ECC, memory management, error detection — and presents simplified,
virtualized interfaces to the guest. That leaves room for a small, **memory-safe kernel
written entirely in Rust** to run cloud workloads. [Asterinas](https://github.com/asterinas/asterinas)
is exactly that: a Linux-ABI-compatible framekernel in Rust (MPL-2.0).

**asterkube** is a proof of concept that turns Asterinas into a real Kubernetes node —
and strips **C out of the entire image** (the sole exception is the GRUB2 bootloader, the
next target). It is the full story behind the Upbound Labs post:
**[Secure Kubernetes Without Linux or C](https://medium.com/upbound-labs/secure-kubernetes-without-linux-or-c-7f8cf4c824e8)**.

> **Full disclosure:** this is an experimental proof of concept, largely written and
> tested with Claude (Fable 5 and Opus 4.6–4.8). It is **not** production-ready, and the
> Asterinas maintainers have final say on any of this reaching their branches.

## The stack

| Layer | Traditional | asterkube |
|---|---|---|
| Container runtime | containerd (Go) | containerd (Go) |
| Orchestration | kubelet (Go) | **Extended kubelet (Go)** — orchestration **+ init** |
| Init | systemd (**C**) | *(folded into the kubelet)* |
| Kernel | Linux (**C**) | **Asterinas (Rust)** |

The kubelet is extended to act as PID 1 — mounting filesystems, bringing up networking
(DHCP-first), and running pods — so there is no systemd and no separate init. The
Kubernetes node service *is* the operating system.

## Architecture

```
asterkube  (this repo, Apache-2.0)
├── cmd/asterkube-init/   ← our Go node agent (the CGO-free, kubelet-fused init)
├── scripts/              ← build + packaging pipeline
├── config/               ← default /etc/fstab and /etc/os-release baked into the image
├── docs/                  ← FEATURES.md (full itemized featureset), SECURITY-POSTURE.md, …
├── asterinas/  (submodule) → jboero/asterinas @ asterkube   (kernel, MPL-2.0)
└── kubernetes/ (build-fetched @ v1.35.6, gitignored)        (kubelet source, Apache-2.0)
```

- **`asterinas`** is a git submodule pinned to our kernel fork — ~50 commits of kernel
  work (namespaces, seccomp-BPF, the astromac MAC, a Service NAT datapath, an
  nftables-compatible netlink surface) live there.
- **`kubernetes`** is *not* vendored — the build fetches the pinned tag `v1.35.6` on
  demand, because compiling the real `cmd/kubelet/app` needs the full tree.
- **`cmd/asterkube-init/`** is original Go that *imports* Kubernetes; it is not a fork of it.

## The image

The whole node — memory-safe Rust kernel + CGO-free Go userspace, **zero C** (no
glibc/musl, no `/lib64`) — is a self-contained **~40 MB** ISO or QCOW2 that **boots in
under 5 seconds**. Consolidating the userspace into a handful of static binaries and
compressing with zstd takes the 138 MiB of contents down to a 26 MiB initramfs.

```
├─ boot artifacts (zstd-compressed initramfs)
│  ├─ kernel ELF (release, stripped) ........  5.3 MiB
│  ├─ initramfs.cpio.zst  (zstd --ultra -22) . 26.4 MiB   ← 44 MiB gzip  (−40%)
│  ├─ ISO  (bootable, isohybrid) ............. 40.6 MiB   ← 58 MiB gzip  (−31%)
│  └─ QCOW2 (disk image) ..................... 38.2 MiB   ← 56 MiB gzip  (−32%)
│
└─ initramfs contents  (138 MiB uncompressed → 26.4 MiB zstd)
   ├─ sbin/init → ../usr/bin/kubelet ......... symlink
   ├─ usr/bin/
   │  ├─ kubelet ............................. 79.1 MiB   real upstream kubelet + init(PID 1)
   │  │                                        + pure-Go runc + mount applet + DHCP
   │  ├─ containerd ......................... 42.7 MiB   static, merged containerd+ctr
   │  ├─ containerd-shim-runc-v2 ............ 13.9 MiB   static
   │  └─ ctr → containerd ................... symlink
   ├─ usr/lib64/ ............................ empty  (proof: no C runtime)
   ├─ usr/share/asterkube/hello.tar ......... 1.5 MiB   demo image
   ├─ etc/os-release ........................ identifies as Asterinas
   └─ etc/fstab ............................. documented default mounts
```

Three static, CGO-free Go binaries make up the entire userspace — `kubelet` (which is
simultaneously init, the OCI runtime, the mount applet and the DHCP client), the merged
`containerd`+`ctr`, and the runc shim. The empty `usr/lib64/` is the zero-C proof.

## What's inside

The kernel gaps that stood between Asterinas and a working Kubernetes node — and the Go
node agent that drives it — are **itemized in [`docs/FEATURES.md`](docs/FEATURES.md)**.
The highlights:

**Kernel (Asterinas fork):**
- **Namespaces & cgroups** — PID/network/user namespaces (rootless, uid/gid mapping,
  privesc-safe capability boundary); cgroup v2 cpu/memory/pids enforcement.
- **Real seccomp-BPF** — a classic-BPF interpreter enforcing filters at the syscall gate
  (was a permissive stub); `PR_GET/SET_SECCOMP`.
- **astromac** — a framekernel-native Mandatory Access Control module (native
  capability-MAC, not a SELinux port): cross-tenant **signal**, **file**, and **network**
  isolation with unforgeable per-process tenant labels.
- **Networking datapath** — L3 bridge, Service DNAT (ClusterIP) with load-balanced
  backends, masquerade/SNAT, an **nftables-compatible `NETLINK_NETFILTER`** surface that
  ingests kube-proxy's rules, netlink route programming, ICMP, wildcard UDP bind (for DHCP).
- **OCI enablers** — `bpf()` for the cgroup device filter, `pivot_root`, overlayfs,
  mqueue, and the procfs/sysfs surface the kubelet ContainerManager needs.
- **zstd initramfs** — a pure-Rust (`ruzstd`, no_std) zstd decoder so the image compresses ~40% smaller.

**Go node agent (`cmd/asterkube-init/`):**
- The real upstream kubelet **fused with init** as one multi-call binary (init, runc,
  mount, DHCP dispatched by `argv[0]`).
- **DHCP-first** userspace networking (RFC 2131) with lease renewal.
- **Zero-C container runtime** — static containerd + a pure-Go, CGO-free runc replacement.
- **Boot-args cluster join** — kubeadm-style bootstrap from the kernel cmdline, apiserver
  pinned by IP + CA-hash.
- **Adversarial boot-time probes** that exercise every kernel security guarantee and fail
  the boot if it doesn't hold.

## OS identity

The node truthfully identifies as **Asterinas** (via `/etc/os-release` → `OS Image`, and a
kernel version of `5.13.0-asterinas`), and advertises a custom `kernel.asterinas.io/name=asterinas`
node label. Crucially it **keeps `kubernetes.io/os=linux`** — Asterinas runs the Linux
ABI, so "linux" there is a functional compatibility declaration (like gVisor or FreeBSD's
linuxulator), not a brand claim. Changing that well-known label would break pod scheduling
and OCI image matching across the ecosystem. It's *Linux-compatible*, not Linux.

## Try it now (no build)

Grab a [release](https://github.com/upbound/asterkube/releases) image and boot it —
you only need `qemu-system-x86_64`, UEFI firmware (`edk2-ovmf` / `ovmf`), and KVM:

```bash
gh release download asterkube-v0.1 --repo upbound/asterkube && sha256sum -c SHA256SUMS
curl -sO https://raw.githubusercontent.com/upbound/asterkube/main/scripts/run-release.sh
chmod +x run-release.sh && ./run-release.sh asterkube-node-v0.1.iso     # or the .qcow2
```

It boots in a few seconds and **stays live** until you shut it down — `Ctrl-a c` then
`system_powerdown` in the QEMU monitor. Full walkthrough in **[QUICKSTART.md](QUICKSTART.md)**.

## Build from source

Prerequisites: Docker, Go, `qemu-system-x86_64` + OVMF, `zstd`, and initramfs tooling
(`cpio`, `readelf`). The build container carries the Rust + OSDK toolchain.

```bash
git clone --recursive https://github.com/upbound/asterkube && cd asterkube

# create the Asterinas OSDK build container (name MUST be 'asterkube') + install cargo-osdk
docker run -d --name asterkube --privileged --network=host -v /dev:/dev \
    -v "$PWD/asterinas:/root/asterinas" -w /root/asterinas \
    asterinas/asterinas:0.18.0-20260603 sleep infinity
docker exec asterkube bash -lc 'OSDK_LOCAL_DEV=1 cargo install cargo-osdk --path osdk'

scripts/build-containerd-merged.sh                 # static containerd+ctr → build/static-bins
scripts/build-hello-image.sh                       # demo image (needs buildah/skopeo)
scripts/build-zeroc-kubelet.sh                     # combined CGO-free kubelet (fetches k8s v1.35.6)
INIT_BIN=build/asterkube-kubelet scripts/zero-c-initramfs.sh   # zero-C initramfs + bootable ISO
scripts/build-qcow2.sh                             # (optional) convert the ISO to a QCOW2 disk

scripts/run-release.sh asterinas/target/osdk/aster-kernel-osdk-bin.iso   # boot what you built
```

To **join a cluster**, generate boot args (this writes a short-lived bootstrap token to
*your* cluster), add them to the kernel cmdline, and rebuild:

```bash
scripts/asterkube-boot-args.sh -n my-node          # prints ASTERKUBE_* lines
# paste them into asterinas/OSDK.toml [run.boot] kcmd_args, then re-run zero-c-initramfs.sh
```

The node then registers in `kubectl get nodes` — `NotReady` until a CNI is added (see below).

## Limitations

This is a proof of concept, not a product:

- **Not production-ready** — largely AI-authored; Asterinas maintainers gate anything upstream.
- **astromac ships Permissive** (log-only) — armed but not blocking until set to Enforcing.
- **NAT is a minimal datapath** — small global conntrack, no endpoint removal; not full Service semantics.
- **No CNI** in the self-contained image — a joined node stays `NotReady` until one is added.
- **Kernel gaps** — virtio devices only (no other NIC/driver classes, no GPU), no journaled
  filesystem (ext4/btrfs) or aarch64 yet. Fine for virtio-backed VM workloads; not bare metal.
- **GRUB2** remains the one C component in the boot path.

## Licensing

asterkube is **Apache-2.0** (see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE)). It references
its two upstreams by pinned revision rather than vendoring them, so each keeps its own license:

| Component | License | How it's included |
|---|---|---|
| **asterkube** (this repo's own code) | Apache-2.0 | native source |
| **Asterinas** (kernel + our fork's changes) | MPL-2.0 | git submodule |
| **Kubernetes** (kubelet / CRI) | Apache-2.0 | build-fetched at `v1.35.6` |

MPL-2.0 is file-level copyleft on the kernel; the Apache-2.0 Go userspace runs as a
separate process across the syscall boundary — no linking, no license mixing. The bootable
images merely aggregate the MPL-2.0 kernel and the Apache-2.0 binaries as separate files;
kernel source stays available at the Asterinas fork.

---

<p align="center">
  <sub>
    By <a href="https://medium.com/@boeroboy">John Boero</a> ·
    <a href="https://medium.com/upbound-labs/secure-kubernetes-without-linux-or-c-7f8cf4c824e8">Upbound Labs</a> ·
    built with Claude.<br>
    Asterinas © the Asterinas Authors (MPL-2.0) · Kubernetes © the Kubernetes Authors (Apache-2.0).
  </sub>
</p>
