# astrokube Security Posture & Architecture

**Status:** roadmap largely DELIVERED + verified in-VM (2026-06-29). Milestone
tag `astrokube-hardening`.
**Threat model:** multi-tenant *untrusted* workloads on a shared Asterinas node
**Approach:** hybrid — retrofit cheap high-value Linux mechanisms now; design a
framekernel-native isolation model for the deep tenancy boundary over time.

## Progress (through 2026-06-29)

- ✅ **rootless containers** — capability boundary (privesc-safe: "root in your
  own userns subtree" via ns ancestry, NO global capset widening, so direct
  cap checks deny host access by construction) + uid/gid mapping (container sees
  uid 0, mapped to an unprivileged host uid). `asterinas cf06bc683`+`00427f5d5`.
  Tag `astrokube-rootless`. See `astrokube/USER-NAMESPACES.md`.
- ✅ **volume-mount hardening** — kernel now ENFORCES `nosuid`/`noexec`/`nodev`
  (it stored but ignored them): a setuid binary on a nosuid volume can't escalate
  and code on a noexec volume can't run. Closes the "rootless container + writable
  volume → host root" vector. `asterinas 57eb7804f`, `kubelet 9be85a7`.
- ✅ **pod-sandbox networking** — the pure-Go OCI runtime now `setns`-joins the
  sandbox's net/ipc/uts/cgroup namespaces (was create-fresh only), so a pod's
  containers share localhost. `kubelet 48d0cbc`. Default pods work end-to-end.
- ⏭ **Remaining (large, deferred — see USER-NAMESPACES.md / memory):**
  `setns(CLONE_NEWPID)` for shareProcessNamespace pods (needs pid_ns_for_children,
  non-default k8s feature); idmapped mounts (deeper per-tenant volume isolation;
  nosuid/nodev already give the practical protection); capability drop-set audit;
  cgroup cpu.max/accounting. **Dropped:** microVM-per-tenant (overkill on a
  memory-safe framekernel), resolve daemon (use resolv.conf/hosts/nsswitch/DHCP),
  SELinux ABI port (native astromac MAC instead). **Already covered:** tenant
  volume isolation (astromac file MAC denies cross-tenant `(dev,ino)` access).

## Earlier progress (2026-06-28)

- ✅ **seccomp-bpf enforcement** (Tier A #1) — kernel now runs real cBPF filters
  (was a no-op stub). `kernel/src/seccomp.rs`. Verified: ERRNO filter blocks a
  syscall, KILL filter terminates the process; no-filter = no change.
  *asterinas `56e7ae76d`, kubelet `70eaab2`.*
- ✅ **astromac native MAC** (Tier C #7) — per-process tenant labels mediating
  cross-tenant operations, permissive by default. A new LSM module on the
  Yama-style framework, NOT an SELinux port. **Three domains:**
  - *signals* (`kill`): `asterinas 624f225e1`, `kubelet 2edbfed`.
  - *file access* (inode `check_permission`, `(dev,ino)` object labels via
    `PR_ASTROKUBE_LABEL_FD`): `asterinas 2aec30b34`, `kubelet 1dd9e80`.
  - *network* (`connect`, IPv4 object labels via `PR_ASTROKUBE_LABEL_IP`):
    `asterinas 69174eb64`, `kubelet 0fc2115`.
  Verified for all three: enforcing cross-tenant → EPERM, same-tenant/unconfined
  allowed, permissive logs; unlabeled (tenant 0 = all of k8s) untouched.
- ✅ **Tenant adapter** — the node agent maps a pod's `tenant` spec field into
  kernel labels (`PR_ASTROKUBE_SETTENANT` on the pod + `LABEL_IP` on its IP +
  enforcing), demonstrated with two real pods where tn1 is denied connecting to
  tn2. `kubelet 5990501`. This is where a launcher would map a
  namespace/seLinuxOptions/annotation onto astromac.
  Tags `astrokube-sec-phase2` (signal+file), later commits on `asterkube`.
- ⏭ **Next:** user namespaces + uid_map (Tier A #2) — assessed; see
  `astrokube/USER-NAMESPACES.md` for the staged plan. Security-critical (a bug =
  privesc), so staged with a human in the loop for the privilege-check rewrite
  (Stage 3). Also: capability drop-set audit (Tier A #3); CRI setns-into-PID-ns.

Baseline before this work: git tag `astrokube-zero-c-baseline`.

---

---

## 1. Threat model

We assume **hostile pods**: a tenant's container actively tries to (a) break out
to the node, (b) read/affect another tenant's pods, (c) exhaust shared
resources, (d) tamper with the control plane or node identity. The node is a
Kubernetes worker; tenants get namespaced pods, not node access.

Trust boundaries, strongest → weakest *today*:

| Boundary | Mechanism on Asterinas now | Strength |
|---|---|---|
| Pod ↔ pod memory/PID/net | namespaces (pid/net/uts/ipc/mnt) | **real** |
| Pod ↔ node files | DAC (uid/gid/mode), caps | partial |
| Pod resource abuse | cgroup `memory.max`, `pids.max` | partial (cpu weak, no accounting) |
| Pod syscall surface | seccomp | **none (stub)** |
| Pod uid 0 ↔ node uid 0 | user namespaces | **none** |
| Pod ↔ pod policy (MAC) | LSM/SELinux/AppArmor | **none** (Yama only) |
| Tenant ↔ tenant (hard) | — | **none** |

### The honest headline
**A shared kernel — even a memory-safe Rust one — is not sufficient for hostile
multi-tenancy today.** This is true of Linux too: that's why hostile
multi-tenancy in practice uses a VM boundary (Kata, Firecracker, gVisor). The
Asterinas framekernel *removes an entire bug class* (memory-safety/UAF/overflow
in the kernel TCB → fewer privilege-escalation primitives), but it does **not**
remove logic, isolation, or missing-enforcement bugs. So the credible
architecture is **defense-in-depth on the shared kernel, with a microVM
per-tenant escape hatch for the truly-untrusted tier.**

---

## 2. What the framekernel buys us (and what it doesn't)

**Buys:** the kernel TCB is `#![forbid(unsafe)]` outside OSTD; the classic
container-escape via a kernel memory-corruption bug is largely off the table.
A Rust **capability-by-construction** model (unforgeable handles, no ambient
authority) can be *more* trustworthy than SELinux retrofitted onto C, because
the type system enforces it rather than runtime hooks bolted across the syscall
layer.

**Doesn't buy:** syscall filtering, uid isolation, MAC policy, resource
accounting, side-channel resistance, or a tenancy boundary. Those are *features*
that still have to be built — memory safety is necessary, not sufficient.

---

## 3. Layered architecture (defense in depth)

```
        ┌─────────────────────────────────────────────────────────┐
  L5    │ Observability & audit: syscall-deny events, OOM-kills,    │
        │ policy violations → node log / audit stream               │
        ├─────────────────────────────────────────────────────────┤
  L4    │ Network policy: per-pod nft default-deny, tenant VLAN/    │
        │ subnet isolation, egress control (kube-proxy nftables)    │
        ├─────────────────────────────────────────────────────────┤
  L3    │ TENANCY BOUNDARY (hostile tier): microVM-per-tenant       │
        │ (Asterinas-in-Asterinas / KVM) — the hard wall            │
        ├─────────────────────────────────────────────────────────┤
  L2    │ MAC / isolation: framekernel-native capability model;     │
        │ per-tenant labels; LSM hook surface (Linux-compat shim)   │
        ├─────────────────────────────────────────────────────────┤
  L1    │ Kernel hardening: seccomp-bpf ENFORCED, user namespaces,  │
        │ setns PID-ns, capability tightening, cgroup cpu/accounting│
        ├─────────────────────────────────────────────────────────┤
  L0    │ Boot/identity integrity: measured boot, signed images,    │
        │ read-only rootfs, immutable zero-C node image             │
        └─────────────────────────────────────────────────────────┘
```

L0–L1 are mostly **retrofit** (Linux-compat, high value, tractable now).
L2–L3 are the **framekernel-native** investments and the real multi-tenant wall.
L4–L5 are policy/visibility we can largely build on what exists.

---

## 4. Gap → work, prioritized for *multi-tenant untrusted*

### Tier A — table stakes (retrofit, do first)
1. **seccomp-bpf enforcement.** Today `seccomp.rs` accepts and ignores filters.
   A hostile pod has the *entire* syscall surface. Implement a real cBPF
   interpreter over `seccomp_data` at the syscall entry trampoline; enforce
   `SECCOMP_RET_KILL/ERRNO/ALLOW`. This is the single biggest attack-surface
   reduction. *Asterinas: `kernel/src/syscall/seccomp.rs`, syscall dispatch in
   `kernel/src/syscall/mod.rs`.* Pure-Go OCI runtime already passes profiles
   through; nothing in user space needs to change.
2. **User namespaces + uid_map.** Without `CLONE_NEWUSER`, "root in pod" == root
   on node for anything not separately gated. Implement uid/gid mapping so pod
   uid 0 maps to an unprivileged node uid. *Asterinas:
   `kernel/src/process/namespace/user_ns.rs` (only init ns today),
   `setns.rs:67` rejects it.* Largest isolation win for untrusted root.
3. **Capability default-deny / drop set.** Audit which privileged syscalls are
   capability-gated; ensure the OCI default-drop bounding set is honored and
   ambient caps actually enforced. *`process/credentials/capabilities.rs`.*
4. **cgroup cpu.max enforcement + memory accounting.** A tenant can currently
   starve CPU (cpu.max stored, weakly enforced) and `memory.current` reads 0.
   Needed for fair-share + for the OOM killer / metrics to be trustworthy.
   *`fs/fs_impls/cgroupfs/controller/{cpu,memory}.rs`.*

### Tier B — CRI completeness + isolation (mixed)
5. **setns into PID namespace.** No `CLONE_NEWPID` branch in `setns.rs` ⇒ a
   workload can't join a pod sandbox's PID ns ⇒ the real CRI pod model
   (pause-container sandbox + workloads) can't be done zero-C. This is the
   already-tracked CRI stretch. *`setns.rs:87-109`.*
6. **LSM hook surface (Linux-compat shim).** Generalize the existing minimal LSM
   infra (currently Yama-only) into real permission hooks at file/exec/socket/
   ns operations, so a MAC module can deny cross-tenant access. This is the
   bridge that lets L2 talk to k8s-shaped tooling.

### Tier C — framekernel-native tenancy (design-led, the real wall)
7. **Capability-based MAC, native.** Instead of porting SELinux (Linux-specific,
   label+policy+AVC machinery), give every pod a Rust **tenant capability
   token** at creation; kernel objects (inodes, sockets, ns handles) carry a
   tenant tag; access = token∋tag, enforced in-type, not by string policy. This
   is your "different concept because it's not Linux" — cleaner than AVC and
   provable.
8. **microVM-per-tenant (L3).** For the genuinely-hostile tier, run each tenant
   in its own Asterinas microVM over KVM (Kata-style). The shared-kernel layers
   become defense-in-depth *inside* each microVM; the VM is the hard boundary.

### Tier D — integrity & visibility
9. **Measured/verified boot + signed images**, read-only immutable node rootfs
   (the zero-C image is already a tiny, auditable TCB — lean into it).
10. **Audit stream**: emit seccomp denials, cap denials, OOM-kills, MAC
    violations to a node audit log the control plane can scrape.

---

## 5. Why SELinux specifically is the wrong port

SELinux is type-enforcement via security contexts (`user:role:type:level`),
policy binary, AVC cache, and `security.selinux` xattr labels — deeply tied to
the Linux LSM design and a large policy language. On a Rust framekernel:
- the **xattr plumbing exists** (`security.*` namespace) but nothing consumes it;
- porting AVC + policy compiler is enormous and gives a *worse* security
  argument than a native capability model that the type system enforces.

**Recommendation:** do **not** port SELinux/AppArmor. Provide (a) a thin
LSM-compatible hook surface so existing tooling/labels don't error, and (b) the
native capability MAC (Tier C #7) as the real mechanism.

---

## 6. Phased roadmap (hybrid)

- **Phase 1 (now, retrofit):** seccomp-bpf enforcement (#1) → user namespaces
  (#2) → capability drop-set audit (#3). Each independently boot-tested on the
  zero-C image; each measurably shrinks the untrusted-pod attack surface.
- **Phase 2 (CRI + hooks):** cgroup cpu/accounting (#4), setns PID-ns (#5),
  LSM hook surface (#6) → unlocks the real CRI pod model and a place to enforce.
- **Phase 3 (native tenancy):** capability MAC (#7), then microVM-per-tenant
  (#8) for the hostile tier.
- **Cross-cutting:** integrity/audit (#9, #10) land alongside, cheaply.

Each phase keeps the zero-C invariant and the prior milestones green.

---

## 7. Measuring it (don't claim, prove)

For every item: a boot-tested in-guest probe (like the existing capability/
namespace probes in `cmd/astrokube-init`) that *attempts the attack and shows it
blocked*. E.g. seccomp: a pod that calls a denied syscall and is killed; userns:
a pod uid 0 that tries a node-privileged op and gets EPERM with the mapped uid.
A failing probe is the acceptance test, not a passing one.
