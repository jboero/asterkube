# Part 1 gap analysis — what Asterinas needs to run Kubernetes

> **Status:** this is the original snapshot. Most gaps below have since been
> addressed on the `asterkube` branch — see [`KERNEL-CHANGES.md`](KERNEL-CHANGES.md).
> **Done:** writable cgroup `memory.*`; NET namespace creation/join/visibility;
> **PID namespaces with renumbering — `CLONE_NEWPID` works and a container init
> sees `getpid()==1`** (verified). Remaining: NET-namespace traffic isolation,
> cgroup cpu/memory enforcement, and the PID refinements noted in KERNEL-CHANGES.


Combines **runtime probing** (the astrokube init's capability probe, booted on the
real kernel — see [`capability-probe.log`](capability-probe.log)) with a
**source audit** of `kernel/src`. The two agree.

## Namespaces

Kubernetes isolates pods/containers with namespaces. Flag validation lives in
[`kernel/src/process/namespace/nsproxy.rs`](../kernel/src/process/namespace/nsproxy.rs)
(`check_unsupported_ns_flags()`); `/proc/<pid>/ns/*` entries come from
[`kernel/src/fs/fs_impls/procfs/pid/task/ns.rs`](../kernel/src/fs/fs_impls/procfs/pid/task/ns.rs)
(`NsProxyEntry`).

| Namespace | Status | Evidence |
|-----------|--------|----------|
| mount (NEWNS)   | ✅ implemented | `fs/vfs/path/mount_namespace.rs` |
| uts   (NEWUTS)  | ✅ implemented | `net/uts_ns.rs` |
| ipc   (NEWIPC)  | ✅ implemented | `ipc/ipc_ns.rs` |
| cgroup(NEWCGROUP)| ✅ implemented | `fs/fs_impls/cgroupfs/cgroup_ns.rs` |
| **pid (NEWPID)** | ❌ **missing** | rejected EINVAL in `nsproxy.rs`; TODO in `process/pid_file.rs` ("we do not support PID namespaces"); no `pid` entry in `NsProxyEntry` |
| **net (NEWNET)** | ❌ **missing** | rejected EINVAL; no `NetNamespace` struct anywhere; `net/mod.rs` has only `iface`, `socket`, `uts_ns` |
| user (NEWUSER)  | ❌ rejected | `process/clone.rs` `clone_user_ns()` → explicit EINVAL |

**The two blockers for a real node are PID and NET namespaces.** Kubernetes
cannot meaningfully isolate pods without them. User namespaces are optional
(rootless/userns-isolated pods) and can come later.

### PID namespace — scope
- Add a `PidNamespace` carrying its own PID allocator and `init` (PID-1-of-ns).
- Thread it through `Process`/`nsproxy`, `clone.rs` (allow `CLONE_NEWPID`), the
  PID allocator, `/proc` (per-ns PID views), `wait`/reaping, and signal routing.
- Add the `pid`/`pid_for_children` entries to `NsProxyEntry`.
- Largest single item; touches process management broadly.

### NET namespace — scope
- Introduce a `NetNamespace` owning the interface list + socket/loopback state
  (today networking is global in `net/iface`, `net/socket`).
- Allow `CLONE_NEWNET`; add the `net` entry to `NsProxyEntry`.
- Per-ns loopback at minimum; veth-style plumbing for pod networking later.

## cgroup v2

Functionally the best-developed piece. Hierarchy creation and controller
delegation via `cgroup.subtree_control` **work** (verified live: created a child
cgroup and delegated `+memory`). Controllers are defined in
[`cgroupfs/controller/mod.rs`](../kernel/src/fs/fs_impls/cgroupfs/controller/mod.rs)
(`SubCtrlType::ALL = [CpuSet, Cpu, Memory, Pids]`).

| Controller | Status | Evidence |
|------------|--------|----------|
| **pids** | ✅ enforced | `controller/pids.rs` `try_charge()` enforces `pids.max` at fork (EAGAIN) |
| cpu | ⚠️ interface-only | `controller/cpu.rs`: `cpu.stat` reads ok; `cpu.weight`/`cpu.max` accepted but **not enforced** (TODOs: "Enforce CPU weight/bandwidth") |
| memory | ⚠️ stubbed | `controller/memory.rs`: `memory.max`/`memory.stat` read/write return `AttributeError` (TODO) — no limit enforcement |
| cpuset | ⚠️ stubbed | `controller/cpuset.rs`: only effective cpus/mems, hardcoded; TODO for real read/write |
| io, hugetlb, rdma, misc | ❌ absent | not in `SubCtrlType::ALL` |

**Implication for a Kubernetes node agent:** the cgroup *filesystem shape* is real
enough that a node agent can build its hierarchy, but `cpu`/`memory` limits won't
be enforced yet, and it may error if it writes `memory.max` (returns
`AttributeError`). Enforcement is the cgroup work item; making the stubbed
attribute files at least store values is a small unblocking step.

## Recommended order

1. **Make stubbed cgroup attribute files accept+store values** (small) — stops
   a node agent erroring on `memory.max` writes even before enforcement exists.
2. **PID namespaces** (large) — the keystone for pod isolation.
3. **NET namespaces + per-ns loopback** (large) — pod networking.
4. **cpu/memory enforcement** (medium-large) — real resource limits.
5. User namespaces (optional).

Filesystems are in good shape already: **overlay** (container images), virtiofs,
ext2, exfat, configfs, tmpfs are all supported.
