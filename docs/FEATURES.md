# asterkube — complete featureset

Everything built to run a Kubernetes node from a zero-C image: the Asterinas
Rust framekernel plus one static, CGO-free Go binary that *is* the real upstream
kubelet (v1.35.6). Split by where the work lives.

- **Kernel changes** live in the [`asterinas`](https://github.com/jboero/asterinas) fork, branch `asterkube` (~50 commits, ~10k lines across ~160 files).
- **Go node agent** lives in this repo under [`cmd/asterkube-init/`](../cmd/asterkube-init) (50 commits, ~8.8k lines).

---

## Asterinas (the kernel)

### Namespaces & process isolation
- **PID namespaces** — nested `CLONE_NEWPID`, virtual PIDs, namespace-aware `getpid`/`getppid`/`gettid`, `wait4`/`waitid` scoped to the namespace.
- **Network namespaces** — per-namespace network stacks for pod isolation.
- **User namespaces (rootless), staged:** real child userns on `CLONE_NEWUSER`; a capability boundary where userns-root holds caps only over its own subtree (privesc-safe); write-once uid/gid mapping (`/proc/[pid]/uid_map`, `gid_map`, `setgroups`) so a container is uid 0 inside while mapped to an unprivileged host uid.
- **`setns`/nsproxy** plumbing for joining existing namespaces.
- **cgroup v2 enforcement** — cpu, memory, and pids controllers actually enforced (`memory.max`, `pids.max`), plus `memory.oom.group`.

### Security enforcement
- **Real seccomp-BPF** — a classic-BPF interpreter that runs and enforces filters at the syscall gate (previously a permissive stub). Full `SECCOMP_RET_*` action set (allow / errno / trap→SIGSYS / kill-thread / kill-process) with Linux severity precedence, STRICT mode, per-thread state inherited across clone/fork and preserved across execve.
- **`PR_GET_SECCOMP` / `PR_SET_SECCOMP`** — seccomp detection + legacy install path.
- **astromac — a framekernel-native Mandatory Access Control module** (native capability-MAC, not a SELinux port). Every process carries an unforgeable tenant label; tenant 0 is unconfined; Disabled/Permissive/Enforcing modes. Three mediation domains:
  - **Signal** — deny cross-tenant `kill`/signals (checked before DAC).
  - **File** — deny cross-tenant read/write/exec via an in-kernel `(dev, ino)`→tenant table, hooked into `check_permission`.
  - **Network** — deny cross-tenant IPv4 `connect()` via an IP→tenant table.
  - Control: `PR_ASTERKUBE_SETTENANT`, `PR_ASTERKUBE_MAC_MODE`, `PR_ASTERKUBE_LABEL_FD`, `PR_ASTERKUBE_LABEL_IP`.
  - New LSM hook points: `signal_access`, `file_access`, `socket_connect`.
- **Mount-flag hardening** — `nosuid` / `noexec` / `nodev` enforced on exec/access, closing volume-based privilege escalation.

### Networking datapath
- **L3 bridge** for pod-to-pod traffic; **inter-bridge L3 forwarding** (host namespace as a router).
- **Service DNAT (ClusterIP)** — in-kernel IPv4 NAT with connection tracking; **load-balanced multi-endpoint backends** (client-hash sticky selection); **node-originated ClusterIP DNAT** (Services reachable from the node itself).
- **Masquerade (source NAT)** for pod egress toward off-cluster networks.
- **nftables/netfilter translation** — a minimal `NETLINK_NETFILTER` family that ingests kube-proxy's rules and translates its Service rules into the NAT datapath; `/proc/sys/net/netfilter` conntrack sysctls; conntrack-list dumps answered.
- **Netlink route programming** — `RTM_NEWROUTE` (CNI default route), `RTM_NEWLINK` (set link UP), `RTM_NEWADDR` attribute ordering.
- **ICMP echo** — ping sockets + interface echo responder.
- **Wildcard UDP bind** — `bind(0.0.0.0:<port>)` resolves to the default iface (enables the userspace DHCP client).
- **Interface MAC via netlink `IFLA_ADDRESS`** (correct DHCP `chaddr`).
- **Socket options** — `SO_TYPE` on UNIX sockets, `IPV6_V6ONLY`/IPv6 options accepted, `MSG_PEEK`/`MSG_TRUNC` on UNIX stream/seqpacket recv.

### Container-runtime enablers (runc / containerd)
- Minimal **`bpf()`** syscall stub (cgroup-v2 device filter); **`pivot_root`** when old root has no parent; read-only **overlayfs** mounts; **`CAP_DAC_OVERRIDE`** always grants directory search; **`mqueue`** filesystem; **shebang** fix (pass script path, not `argv[0]`).

### kubelet ContainerManager surface
- `/sys/devices/system/cpu` topology; `/proc/[pid]/mountinfo` real major:minor; `/proc/sys/kernel/osrelease` and the `/proc/sys` knobs ContainerManager reads; `/proc/meminfo` Swap/Buffers/Cached.

### Platform / boot
- **ACPI power-button monitor** for graceful node drain; **boot-args cluster join** (`ASTERKUBE_*` kernel cmdline → init env); `run-host-qemu.sh` host harness.

---

## Go node agent (`cmd/asterkube-init/`)

### Init / single-binary architecture
- **kubelet-derived PID 1 node agent** — the real upstream kubelet, fused with our init, running as process 1.
- **Multi-call single binary** — one static binary dispatches on `argv[0]`/PID to be init, kubelet, runc, mount/umount applet, and DHCP client.
- **kubelet entry split** so the init folds into the genuine upstream kubelet build.
- **Persistent, interactive live-node serve window.**

### Networking (userspace)
- **DHCP-first** — a from-scratch userspace DHCPv4 client (RFC 2131) brings eth0 up (IP + route + DNS) before anything else.
- **Lease renewal** — T1 unicast RENEWING, T2 broadcast REBINDING, re-DISCOVER on loss.
- **netlink helpers** (link-by-name, real MAC read, route/addr programming); **minimal CNI install** to reach Ready.

### Container runtime (zero-C)
- **containerd + ctr merged** into one multi-call binary (shim kept separate); **static containerd** daemon + image import/run; **pure-Go, CGO-free OCI runtime** (runc replacement); **zero-C image mode** (no glibc/musl/`/lib64`); **CRI** path (sandbox + real container); runtime **joins pod sandbox namespaces**; runtime **read from initramfs** (self-contained, no virtio-fs share).

### Cluster join & filesystem
- **Boot-args join** — kubeadm-style bootstrap kubeconfig from the `ASTERKUBE_*` cmdline; apiserver pinned by IP + CA-hash. **Dual node identity**: advertises `kernel.asterinas.io/name=asterinas` + `kernel.asterinas.io/compat=linux` via `--node-labels` while keeping the well-known `kubernetes.io/os=linux` (the ABI personality that keeps scheduling + OCI image matching working). **CA-bundle install**, in-cluster apiserver hostname resolution, `/etc/fstab` support, kubelet `--root-dir` on the ext2 block device, outbound TCP+TLS gate before join.

### Verification probes (adversarial, run at boot)
Each exercises a kernel guarantee end-to-end and fails the boot if it doesn't hold: seccomp enforcement; astromac signal/file/network MAC; tenant adapter (label real pods from spec); user-namespace + capability-boundary + id-mapping; volume-mount hardening; `NETLINK_NETFILTER`; shebang path. Plus datapath probes: Service DNAT (bridged pods, TCP, load-balanced backends), cross-subnet forwarding, pod-egress masquerade, node-originated ClusterIP DNAT, end-to-end Service DNAT via kube-proxy's own rule.

---

## Image

The whole node — memory-safe Rust kernel + one static, CGO-free Go binary — with
**zero C** (no glibc/musl, no `/lib64`), self-contained so nothing is fetched at boot.

```
astrokube node image
├─ boot artifacts (zstd-compressed initramfs)
│  ├─ kernel ELF (release, stripped) ........  5.3 MiB
│  ├─ initramfs.cpio.zst  (zstd --ultra -22) . 26.4 MiB   ← 44 MiB gzip  (−40%)
│  ├─ ISO  (bootable, isohybrid) ............. 40.6 MiB   ← 58 MiB gzip  (−31%)
│  └─ QCOW2 (disk image) ..................... 38.2 MiB   ← 56 MiB gzip  (−32%)
│
└─ initramfs contents  (138 MiB uncompressed → 26.4 MiB zstd)
   ├─ sbin/init → ../usr/bin/kubelet ......... symlink
   ├─ usr/bin/
   │  ├─ kubelet ............................. 79.1 MiB   THE binary: real upstream kubelet
   │  │                                                   + init (PID 1) + pure-Go runc
   │  │                                                   + mount applet + DHCP client
   │  ├─ containerd ......................... 42.7 MiB   static, merged containerd+ctr
   │  ├─ containerd-shim-runc-v2 ............ 13.9 MiB   static
   │  └─ ctr → containerd ................... symlink
   ├─ usr/lib64/ ............................ empty  (proof: no C runtime)
   ├─ usr/share/asterkube/hello.tar ......... 1.5 MiB   demo image
   ├─ etc/os-release ........................ 742 B    identifies as Asterinas
   ├─ etc/fstab ............................. 3.0 KiB  documented default mounts
   └─ etc/astrokube/pods/*.json ............. ~2.3 KiB pod specs (7 files)
```

| Artifact | gzip -9 | **zstd --ultra -22 --long** | saving |
|---|---|---|---|
| initramfs | 44 MiB | **26.4 MiB** | −40% |
| ISO | 58 MiB | **40.6 MiB** | −31% |
| QCOW2 | 56 MiB | **38.2 MiB** | −32% |

The kernel decompresses either format (magic-byte detected), so the compressor is a
free choice; zstd is a pure size win — boot time is unchanged (the 138 MiB cpio
unpack into the in-memory rootfs dominates, not decompression).

---

## Current limitations (honesty flags)

- **astromac ships in Permissive (log-only) mode** — armed but not blocking until the mode is set to Enforcing.
- **NAT is a minimal datapath** — small global conntrack table, no endpoint removal, masquerade keeps the source port. Fine for the demo; not yet production Service semantics.
- **No CNI in the self-contained image** — a joined node registers but stays `NotReady` until a CNI is added.
