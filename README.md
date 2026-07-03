<p align="center">
  <img src="assets/upbound-logo.svg" alt="Upbound" height="52">
</p>

<h1 align="center">asterkube</h1>

<p align="center">
  <b>A Kubernetes node from a zero-C image.</b><br>
  The <a href="https://github.com/asterinas/asterinas">Asterinas</a> Rust framekernel + one static, CGO-free Go binary that <i>is</i> the real upstream kubelet.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
  <img alt="kernel: MPL-2.0" src="https://img.shields.io/badge/kernel-Asterinas%20(MPL--2.0)-brightgreen.svg">
  <img alt="kubernetes v1.35.6" src="https://img.shields.io/badge/kubernetes-v1.35.6-326ce5.svg">
</p>

---

## What this is

**asterkube** boots a real Kubernetes node with **no C anywhere in the image** — no
glibc, no musl, no `/lib64`. The kernel is [Asterinas](https://github.com/asterinas/asterinas),
a memory-safe Rust framekernel. Userspace is a **single static, CGO-free Go binary**
that is simultaneously PID 1, the OCI runtime, the mount applet, the DHCP client —
and the genuine upstream **kubelet v1.35.6**, compiled in. It DHCPs, runs
containers via a static containerd (or a pure-Go runtime), and can join a cluster
from kernel boot args.

It exists to answer a question: *how much of a production Kubernetes node can you
stand up on a memory-safe microkernel, and what does the kernel need to grow to
get there?* The answer — seccomp-BPF, a native MAC, namespaces, cgroup
enforcement, a Service NAT datapath, an nftables-compatible netlink surface — is
[itemized in `docs/FEATURES.md`](docs/FEATURES.md).

## Architecture

```
        asterkube  (this repo, Apache-2.0)
        ├── cmd/asterkube-init/   ← our Go node agent (the CGO-free kubelet)
        ├── scripts/              ← build + packaging pipeline
        ├── asterinas/  (submodule) → jboero/asterinas @ asterkube   (kernel, MPL-2.0)
        └── kubernetes/ (build-fetched @ v1.35.6, gitignored)        (kubelet source, Apache-2.0)
```

- **`asterinas`** is a git submodule pinned to our kernel fork — the ~50 commits of
  kernel work (namespaces, seccomp, astromac MAC, the NAT datapath, …) live there.
- **`kubernetes`** is *not* vendored. The build fetches the pinned tag `v1.35.6`
  on demand, because compiling the real `cmd/kubelet/app` needs the full tree.
- **`cmd/asterkube-init/`** is our original Go — additive code that imports
  Kubernetes, not a fork of it.

## Repository layout

| Path | What |
|---|---|
| `cmd/asterkube-init/` | The Go node agent — init, kubelet fusion, pure-Go OCI runtime, DHCP, MAC/seccomp probes |
| `scripts/` | Build + packaging pipeline (see below) |
| `config/fstab.default` | Documented default `/etc/fstab` baked into the image |
| `docs/` | `FEATURES.md`, `SECURITY-POSTURE.md`, `USER-NAMESPACES.md`, `KERNEL-CHANGES.md`, verification logs |
| `asterinas/` | Kernel submodule (`jboero/asterinas@asterkube`) |
| `build/` | Generated artifacts (gitignored) |

## Build

Prerequisites: Docker (the Asterinas OSDK build container, named `asterkube`), Go,
`qemu-system-x86_64`, and the usual initramfs tooling (`cpio`, `gzip`, `readelf`).

```bash
git clone --recursive https://github.com/upbound/asterkube
cd asterkube
# shallow submodule if you don't want the full kernel history:
#   git clone https://github.com/upbound/asterkube && cd asterkube
#   git submodule update --init --depth 1

# 1. static containerd+ctr+shim (one merged multi-call binary) → build/static-bins
scripts/build-containerd-merged.sh

# 2. the combined CGO-free kubelet (real upstream kubelet + our init); fetches
#    kubernetes v1.35.6 into ./kubernetes on first run
scripts/build-zeroc-kubelet.sh

# 3. bake the zero-C initramfs and rebuild the bootable ISO
INIT_BIN=build/asterkube-kubelet scripts/zero-c-initramfs.sh

# 4. (optional) convert the ISO to a QCOW2 disk
scripts/build-qcow2.sh
```

Artifacts land in `asterinas/target/osdk/` (ISO) and `asterinas/test/initramfs/build/` (QCOW2).

## Run

```bash
scripts/../asterinas/run-host-qemu.sh asterkube        # boot the ISO under QEMU (slirp NIC)
```

To **join a cluster**, generate boot args (this writes a short-lived
bootstrap-token to *your* cluster), add them to the kernel cmdline, and rebuild:

```bash
scripts/asterkube-boot-args.sh -n my-node   # prints ASTERKUBE_* lines
# paste them into asterinas/OSDK.toml [run.boot] kcmd_args, then re-run zero-c-initramfs.sh
```

The node registers in `kubectl get nodes` — `NotReady` until a CNI is added (see limitations).

## Licensing

asterkube is **Apache-2.0** (see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE)). It is a
superproject that references two upstreams by pinned revision rather than copying
their source, so each keeps its own license:

| Component | License | How it's included |
|---|---|---|
| **asterkube** (this repo's own code) | Apache-2.0 | native source |
| **Asterinas** (kernel + our fork's changes) | MPL-2.0 | git submodule |
| **Kubernetes** (kubelet/CRI) | Apache-2.0 | build-fetched at `v1.35.6` |

MPL-2.0 is file-level copyleft on the kernel; our Apache-2.0 Go runs as a separate
userspace process across the syscall boundary — no linking, no license mixing. The
bootable images aggregate the MPL-2.0 kernel and the Apache-2.0 binary as separate
files; kernel source stays available at the Asterinas fork.

## Limitations

- **astromac ships Permissive (log-only)** — armed, not blocking, until set to Enforcing.
- **NAT is a minimal datapath** — small global conntrack, no endpoint removal, masquerade keeps the source port. Not yet production Service semantics.
- **No CNI in the self-contained image** — a joined node stays `NotReady` until one is added.

---

<p align="center"><sub>Built at <b>Upbound</b>. Asterinas © the Asterinas Authors (MPL-2.0). Kubernetes © the Kubernetes Authors (Apache-2.0).</sub></p>
