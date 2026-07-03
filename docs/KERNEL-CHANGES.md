# asterkube kernel changes — Part 1 progress

Changes made to the Asterinas kernel (`asterkube` branch) to move toward running
Kubernetes. Every item below was verified by booting the kernel under QEMU and
exercising it from the asterkube PID 1 init's capability probe
(`capability-probe-after.log`). `cargo fmt --check` and `cargo osdk clippy` are
clean.

## 1. cgroup v2 — writable memory controller

`kernel/src/fs/fs_impls/cgroupfs/controller/memory.rs`

Before: every `memory.*` file was registered read-only and read/write returned
`AttributeError` — so a container runtime writing `memory.max` would fail.

After: the memory sub-controller stores the configured limits and exposes the
cgroup v2 memory interface:

- Writable: `memory.max`, `memory.high`, `memory.min`, `memory.low`,
  `memory.swap.max` (accept numeric values and the `max` sentinel).
- Readable: `memory.current`, `memory.swap.current` (report `0` until usage
  accounting exists), `memory.events`, `memory.stat`.

**Verified:** a child cgroup with `+memory` delegated accepts
`memory.max = 104857600` and reads it back. Limits are stored but not yet
*enforced* (no reclaim/OOM) — that is the remaining memory-controller work.

## 2. NET namespaces — first-class and creatable

New: `kernel/src/net/net_ns.rs` (the `NetNamespace` object).
Wired through: `net/mod.rs`, `process/namespace/nsproxy.rs` (field, init
singleton, builder, clone on `CLONE_NEWNET`), `syscall/setns.rs` (join via fd or
pidfd), `fs/fs_impls/procfs/pid/task/ns.rs` (the `/proc/<pid>/ns/net` symlink),
`fs/fs_impls/pseudofs/nsfs.rs` (`NsType::Net` activated).

Before: `CLONE_NEWNET` returned `EINVAL`; no `NetNamespace` type existed; `net`
was absent from `/proc/<pid>/ns`.

After: network namespaces are a first-class, hierarchical-capable namespace
object that can be **created** (`clone`/`unshare` with `CLONE_NEWNET`), **joined**
(`setns`), and **inspected** (`/proc/<pid>/ns/net`). Creation requires
`CAP_SYS_ADMIN`, matching Linux.

**Verified:** `CLONE_NEWNET` now succeeds; `net` appears in `/proc/self/ns`.

### 2a. NET namespace traffic isolation — per-namespace loopback ✅

`net/net_ns.rs` now gives each created namespace its **own loopback interface**
(driven by its own polling thread, `net/iface/poll.rs::spawn_poll_thread`), while
the initial namespace keeps using the global registry unchanged. Socket bind and
connect resolve interfaces through the **current** namespace
(`NetNamespace::current()` in `net/socket/ip/common.rs`), so a socket binds to the
loopback of its own namespace.

**Verified on hardware** (`netns-isolation-verified.log`): a process in a new
network namespace binds `127.0.0.1:54321` **at the same time** the initial
namespace holds that exact port — both succeed, proving independent loopback
stacks and port spaces. (The new namespace's first ephemeral port is `32768`, a
fresh allocator, while the initial namespace's climbs independently.) Initial-
namespace networking is byte-for-byte unchanged (its loopback round-trip still
passes).

**Remaining:** physical NICs are not yet movable between namespaces (a created
netns has only `lo`, like a fresh Linux netns before `veth` is added);
per-namespace routing tables, broadcast handling, and netlink (`ip addr`/`ip
link`) enumeration of created namespaces are follow-ups.

## 3. PID namespaces — creatable, with renumbering ✅

New: `kernel/src/process/namespace/pid_ns.rs` (hierarchical `PidNamespace` with a
per-namespace PID allocator). Wired through: `process/process/mod.rs` (`Process`
gains `pid_ns` + `vpid`), `process/clone.rs` (`CLONE_NEWPID` handling +
caller-namespace return value), the `getpid`/`getppid`/`gettid` and
`wait4`/`waitid` syscalls, procfs (`/proc/<pid>/ns/pid`), and `nsfs.rs`.

Before: no `PidNamespace`; `CLONE_NEWPID` returned `EINVAL`; PID was a single
global `u32` everywhere.

After: `CLONE_NEWPID` **creates a real PID namespace and renumbers**. The model
keeps the process's number in the *initial* namespace as the canonical global PID
(so the global PID table, process groups, sessions and signals are unchanged),
and overlays a namespace-local number (`Process.vpid`) for nested namespaces:

- The first process in a new namespace becomes its `init` — **`getpid()` returns 1**.
- `getppid()` returns the parent's number in the caller's namespace (0 for an
  `init` whose parent lives in an ancestor namespace).
- `gettid()` returns the namespace-local TID for the main thread.
- `fork`/`clone` return the child's PID in the **caller's** namespace.
- `wait4`/`waitid` report the child's PID in the **waiter's** namespace.
- `/proc/<pid>/ns/pid` reflects the process's namespace.

**Verified on hardware** (`pid-renumbering-verified.log`): a child cloned with
`CLONE_NEWPID` reports `getpid()==1`, while same-namespace children keep their
global PIDs; the host `init` forks and reaps all children cleanly.

**Remaining refinements** (documented, not blocking the container-init semantic):
`kill(pid)`/`waitpid(pid)` resolving a *namespace-local* PID *argument* from
inside a non-initial namespace (needs a per-namespace process registry);
namespace-local TIDs for non-main threads; per-namespace `/proc` process
enumeration; translation across more than one nesting level; and tearing down a
namespace (SIGKILL its members) when its `init` exits. The host-managed container
path (node agent → runc on the host) does not depend on these.

## 4. cgroup v2 — `memory.max` enforcement ✅

`vm/vmar/vm_mapping.rs` now enforces `memory.max` at the page-fault commit point.
Before committing a new **anonymous** page, the kernel checks the faulting
process's cgroup: if the process's projected resident anonymous memory would
exceed its cgroup `memory.max`, the fault fails with `ENOMEM` (delivering a fatal
fault to the offending process). Helpers: `Controller::memory_max()` and
`cgroupfs::process_memory_max()` (`fs/fs_impls/cgroupfs/controller/`); the
projected RSS comes from the existing `RssDelta` (`vm/vmar/vmar_impls/`).

**Safety boundary:** a process is checked *only* when it is in a non-root cgroup
with a finite `memory.max`. The root cgroup and any unlimited cgroup return
`None`, so the init process and all ordinary processes are never restricted —
boot is unaffected by construction.

**Verified on hardware** (`memory-enforcement-verified.log`): a child that joins
a cgroup with `memory.max = 32 MiB` and tries to allocate 128 MiB is **killed**,
while the init process (root cgroup) boots and runs normally, and loopback /
net-ns isolation are unaffected.

**Scope:** anonymous memory only (file-backed pages are reclaimable and not yet
counted); enforcement fails the allocation rather than running a full OOM-killer
with reclaim. Per-cgroup aggregate accounting (`memory.current` across all member
processes) is a follow-up — the current limit is applied per faulting process,
which is exact for the common single-process-per-cgroup (pod) case.

## 5. cgroup v2 — `cpu.max` bandwidth enforcement ✅

CPU enforcement is implemented as **`cpu.max` bandwidth throttling** (not
`cpu.weight`). This is the right choice for two reasons: it is what Kubernetes
pod CPU *limits* map to, and it is *safe* — a quota only caps the throttled
cgroup, so it cannot starve other tasks (a flat `cpu.weight` fold would let a
high-weight pod outweigh and starve the kernel's own init and poll threads;
correct `cpu.weight` needs hierarchical runqueues).

Mechanism (`fs/fs_impls/cgroupfs/controller/cpu.rs`, `thread/task.rs`,
`process/process/timer_manager.rs`):
- Each tick charges the running process's cgroup one tick of bandwidth against a
  per-period budget (`quota_usec` per `period_usec`), refilled lazily each period.
- When a cgroup goes over budget, the task-run loop's "kernel event" predicate
  fires, so the task returns from user space promptly (instead of finishing a
  full scheduler time slice) and **blocks at a safe point until the period
  refills** (`throttle_cpu_if_needed`, using a timed `Waiter` sleep). Blocking
  yields the CPU, so other tasks run normally.

**Safety boundary:** the root cgroup and any cgroup with `cpu.max = "max"` have no
quota and are never throttled — init and ordinary processes are unaffected.

**Verified on hardware** (`cpu-enforcement-verified.log`): the same CPU-bound
loop, run for equal wall-clock time, does **~7% of the work** when its cgroup is
throttled to `cpu.max="10000 100000"` (10% of a CPU) versus an unthrottled run —
while init boots and runs normally.

**Scope:** throttling is checked at the leaf cgroup (the common pod case);
multi-level `cpu.max` and SMP-fair distribution across CPUs are refinements.
`cpu.weight` (proportional shares) still needs hierarchical scheduling.

## 6. veth pairs — cross-namespace pod networking ✅

A pod in its own network namespace can now exchange IP traffic with the host (and,
in principle, other pods) over a **veth pair** — the wire CNI uses to give a pod a
routable IP. Pieces:

- **veth device** (`kernel/libs/aster-bigtcp/src/device/veth.rs`): a smoltcp
  `Device` pair sharing a `VethChannel`. A frame transmitted on one end is pushed
  to the peer end's receive queue, and the peer's interface is woken via a
  notifier callback — so a packet sent in one namespace is polled and delivered in
  the other.
- **pair creation** (`kernel/src/net/iface/veth.rs`, `new_veth_pair`): builds both
  interfaces, wires the cross-wake notifiers to each peer's poll scheduler, and
  spawns a poll thread per end.
- **placement + addressing + routing** (`kernel/src/net/net_ns.rs`):
  `create_veth_pair` puts each end into a chosen namespace; `set_iface_addr_v4`
  assigns an interface's address after creation; `add_iface`/`find_iface_by_index`/
  `all_ifaces` let a namespace gain and enumerate interfaces at runtime; and
  `ephemeral_iface` does **subnet routing** so a packet to an address on the veth's
  subnet egresses the local veth end.
- **trigger**: real **RTNETLINK** (`RTM_NEWLINK`/`RTM_NEWADDR`) — see §7. (This
  replaced an earlier non-Linux `prctl` shim, now removed.)

The original cross-namespace connectivity proof used the `prctl` shim; the current
flow (RTNETLINK) is in `rtnetlink-veth-verified.log` and described in §7.

## 7. RTNETLINK — veth creation + addressing the way CNI does it ✅

The netlink route protocol was previously **dump-only** (`RTM_GETLINK`/`RTM_GETADDR`).
It now also handles the two **write** operations a CNI plugin needs to wire a pod,
so a pod's veth is created with real netlink messages instead of the `prctl` shim:

- **`RTM_NEWLINK` for veth** (`netlink/route/kernel/link.rs`, `do_new_link`): parses
  the nested attribute tree a CNI plugin / `ip link add` sends —
  `IFLA_LINKINFO` → `IFLA_INFO_KIND="veth"` → `IFLA_INFO_DATA` → `VETH_INFO_PEER`
  (itself an `ifinfomsg` + the peer's `IFLA_IFNAME` and `IFLA_NET_NS_PID`). It
  creates the pair, placing the primary end in the caller's netns and the peer end
  in the namespace named by `IFLA_NET_NS_PID` — i.e.
  `ip link add veth-X type veth peer name eth0 netns <pid>`. The nested-TLV parser
  is in `netlink/route/message/attr/link.rs`.
- **`RTM_NEWADDR`** (`netlink/route/kernel/addr.rs`, `do_new_addr`): parses
  `IFA_LOCAL`/`IFA_ADDRESS` + the `ifaddrmsg` prefix length and assigns the address
  to the interface (by index) **in the caller's network namespace** — `ip addr add`.
  The address attributes are now parsed (`attr/addr.rs`) rather than skipped.
- **netns-awareness**: every link/addr op resolves against the **calling thread's**
  network namespace (`NetNamespace::current()`), and the `RTM_GETLINK`/`RTM_GETADDR`
  dumps now enumerate *that* namespace's interfaces (`all_ifaces`) instead of the
  global boot set — so a pod sees its own `eth0`, not the host's NICs.
- **acknowledgements**: write requests with `NLM_F_ACK` get a proper `NLMSG_ERROR`
  ack (code 0 on success), matching Linux, so a standard netlink client proceeds.

Because netlink's namespace model is "the socket acts in its own netns", address
assignment is split exactly as real CNI splits it: the node agent (host netns)
creates the pair + addresses the host end, then the pod (its own netns) addresses
its `eth0`. The Go side drives this with a tiny dependency-free raw-netlink client
(in the out-of-tree asterkube init) speaking the same wire format as
iproute2 / `vishvananda/netlink`.

**Verified on hardware** (`rtnetlink-veth-verified.log`): the node agent creates
`veth-netpod` (`10.244.0.1/24`) with peer `eth0` placed in the pod's netns by PID
via `RTM_NEWLINK`, addresses both ends via `RTM_NEWADDR`, and the pod **receives a
datagram from the host over the veth** — `CROSS-NS OK … (pod 10.244.0.2,
RTNETLINK)`. Reliable 4/4 boots; no `prctl` shim involved.

**Safety / hardening.** These handlers parse **untrusted user-space input**, so:
- the write ops (`RTM_NEWLINK`/`RTM_NEWADDR`) require **`CAP_NET_ADMIN`** in the
  caller's effective capability set (`kernel/util.rs::require_net_admin`), matching
  Linux and the existing privileged-socket-option check;
- the nested-TLV parser (`message/attr/link.rs`) is bounds-checked: every
  sub-attribute length is validated against the remaining budget *and* against the
  attribute-header size **before** any subtraction, so a malformed
  `len < 4` cannot underflow (which would panic an overflow-checked build); the
  walk is depth-bounded (only `LINKINFO→INFO_DATA→VETH_INFO_PEER` recurse) and each
  iteration consumes ≥1 header, so it always terminates; truncated input is handled
  gracefully rather than via `unwrap`. There is **no `unsafe`** in any of the
  netlink/veth/namespace changes.

**Scope / remaining:** placing the peer by `IFLA_NET_NS_FD` (vs `_PID`),
`RTM_SETLINK` (admin up/down, post-hoc netns move), `RTM_NEWROUTE`, per-namespace
*kernel* netlink sockets (currently one global dispatcher, run in caller context),
and Services/NAT (nftables, for `kube-proxy`) are the follow-on networking layers.
Two privilege-model refinements remain for strict multi-tenant isolation: the
`IFLA_NET_NS_PID` lookup interprets the pid in the **root** PID namespace (correct
for a host-run node agent, but not pid-namespace-relative), and the capability
check is on the **caller's** effective set rather than `CAP_NET_ADMIN` in the owning
user namespace of each *target* netns (Linux's `netns_capable`). A created
namespace's veth/loopback poll thread is still leaked at teardown (benign — it
sleeps).

### Concurrency fix found along the way

Bringing up repeated pods exposed an intermittent boot hang that turned out to be
a real bug in the §5 `cpu.max` work: the bandwidth budget is charged from the
**timer interrupt**, but its `SpinLock`s defaulted to `PreemptDisabled` (not
IRQ-disabling). A process holding the lock could be interrupted by the timer on
the same CPU and deadlock. Fixed by declaring those locks
`SpinLock<_, LocalIrqDisabled>` (`controller/cpu.rs`). After the fix, boots are
reliable. The node agent and capability probe also bound every child process with
a watchdog timeout, so no single pod can wedge the node regardless.

## 8. ICMPv4 — echo responder + raw/datagram ICMP sockets ✅

`kernel/libs/aster-bigtcp/src/socket/icmp.rs` (new), `iface/poll.rs` (responder),
`socket_table.rs`, `ext.rs`.

Before: the kernel could not answer or originate ICMP; `ping` and any in-cluster
reachability probe failed, which made every networking bug hard to triage.

After: a built-in echo responder answers ICMP echo requests addressed to an
interface, and ICMP sockets (raw + datagram, smoltcp `socket-icmp`) let userspace
send/receive echo. Checksums are recomputed (these ifaces validate). Replies to a
*local* destination are re-processed through a bounded loop rather than emitted to
the device (see bug below).

**Verified:** echo over loopback, eth0, and a cross-namespace veth all reply.

## 9. Bridging + `RTM_NEWROUTE` — an L3 hub and pod default routes ✅

`kernel/libs/aster-bigtcp/src/device/bridge.rs` + `iface/bridge.rs` (new), netlink
`RTM_NEWROUTE`/`RTM_GETROUTE` handlers, `IFLA_MASTER` enslavement.

Before: pods could only reach a directly-wired veth peer; there was no bridge to
join several pods on one subnet, and no way to install a default route.

After: a `BridgeHub` connects many veth ports to one gateway-owning local
attachment. veth links are `Medium::Ip`, so the hub forwards at **L3** — it learns
IPv4 *source addresses* per port and routes by destination, floods
broadcast/multicast, and hands anything unroutable to the local stack. Pods
install a default route via `RTM_NEWROUTE` (read back via `RTM_GETROUTE`), exactly
as a CNI plugin does. Built by three parallel agents against a frozen device/
netlink/Go contract; integration compiled and passed first try.

**Verified:** two pods on `br0` ping and exchange UDP through the hub; each
installs and reads back its default route.

## 10. Service (ClusterIP) NAT — kube-proxy's data plane ✅

`kernel/libs/aster-bigtcp/src/nat.rs` (new), hooked into the bridge forward path;
control via a temporary `prctl` (`PR_ASTERKUBE_DNAT`, `CAP_NET_ADMIN`-gated) until
the `nftables`-compatible netlink surface exists.

A global NAT table rewrites a VIP `vip:vport/proto` to a backend on the way in
(DNAT) and restores the VIP as the source on the reply (reverse, via a
connection-tracking table). IPv4 + UDP/TCP; IP and L4 checksums recomputed from
scratch. Three increments:

- **DNAT + conntrack (UDP).** A datagram to the VIP reaches a backend pod; the
  reply arrives at the client *from the VIP*, proving the reverse rewrite.
- **TCP.** The same engine (it already keyed on `vip+vport+proto`); a completed
  TCP handshake through DNAT is itself the proof of reverse-NAT, since the client
  only accepts a SYN-ACK sourced from exactly the VIP it dialed.
- **Load balancing.** A VIP carries a *set* of backends; a new flow is pinned to
  one by hashing the client address+port (FNV-1a) and the choice recorded in
  conntrack for per-connection stickiness — like kube-proxy spreading a Service
  over its endpoints.

**Verified:** UDP and TCP ClusterIP both work; two endpoints split 8 client flows
4/4 with every reply arriving from the VIP.

## 11. Inter-bridge L3 forwarding — the host namespace as a router ✅

`kernel/libs/aster-bigtcp/src/device/bridge.rs` (hub `id` + a global router hook),
`kernel/src/net/iface/bridge.rs` (router that forwards by destination subnet).

Before: a bridge could only deliver to one of its own pods or its local gateway
stack — pods on *different* bridges/subnets could not reach each other.

After: when a hub cannot deliver a unicast frame to one of its own ports, it
offers the frame to a global L3 router before falling back to its local stack.
The kernel installs a router that forwards the frame to the sibling bridge whose
subnet owns the destination (and never back to the source bridge). This makes the
host namespace forward between pod subnets — the foundation for multi-subnet pod
networking, masquerade, and overlays. No router / no sibling owns the dst ⇒
behaviour is exactly as before (no regression to single-bridge paths).

**Verified:** a pod on `br1` (10.244.2.0/24) pings a pod on `br0` (10.244.1.0/24)
and back, across subnets, while the single-bridge Service paths keep passing.

## 12. Masquerade (source NAT) — pod egress through the host ✅

`kernel/libs/aster-bigtcp/src/nat.rs` (masquerade table + reverse), `net/iface/
bridge.rs` (uplink set + SNAT in the router), `prctl.rs` (`PR_ASTERKUBE_MASQ`).

Before: a pod could reach other pods (even on other subnets, via §11) but not a
network the cluster does not own — the router dropped such frames at the local
stack, and a pod's private source address is unroutable from outside.

After: a bridge can be marked a masquerade *uplink*. When the §11 router forwards
a frame onto an uplink, the frame's source is rewritten to the uplink's own
address (SNAT); the masquerade table records the flow, and the reply's
destination is rewritten back to the pod by `NatTable::apply` on its first bridge
hop home. The port is left unchanged and doubles as the reply demux key (PAT —
per-flow port reallocation on collision — is future work). This is exactly how a
node masquerades a pod to its `eth0`; here an uplink bridge is a verifiable
stand-in for `eth0` (raw NIC injection for real internet egress, plus the
slirp-bound test environment, are the remaining piece).

**Verified:** a pod on `br2` (10.244.3.0/24) reaches a server on the uplink `bre`
(192.168.99.0/24); the server sees the source as 192.168.99.1 (masqueraded) and
the reply returns to the pod.

### Bugs found along the way (networking)

- **Off-subnet pod egress went out loopback (`net_ns.rs::ephemeral_iface`).**
  Surfaced by the first Service test: a client's VIP packet egressed
  `127.0.0.1`. In a freshly created netns the source-interface selection fell
  through to the default interface, which is loopback (`ifaces[0]`). Fixed: an
  off-subnet IPv4 destination now routes via an interface that has a default
  gateway. **This repaired *all* off-subnet pod egress (Service VIPs *and* the
  outside world), not just NAT** — the kind of bug only an end-to-end test finds.
- **ICMP replies to local destinations were stranded.** A self-ping's reply was
  emitted to the device and lost; fixed with a bounded local re-processing loop
  mirroring the TCP path.
- **Warning hygiene.** Cleared an unused `mut` (`nat.rs`) and two
  `smoltcp::wire::` over-qualifications (`iface/common.rs`); `aster-bigtcp` is
  warning-clean.
- **Latent peer-UDP race (test-side, exposed by the §11 multi-pod topology).**
  The same-bridge pod↔pod UDP probe had the sender fire its datagrams before the
  listener — several seconds behind under heavier load — had bound its socket, so
  every datagram was lost and the listener stalled its whole timeout, cascading
  into a watchdog kill of the Service test. Fixed in the init by binding the
  listener socket *before* the ICMP exchange (so the kernel buffers early
  datagrams) and widening the sender window. Not a kernel bug, but a real
  ordering hazard the load surfaced.

## Net effect on `/proc/<pid>/ns`

`cgroup ipc mnt user uts`  →  **`cgroup ipc mnt net pid user uts`**

## Files (2 new, 16 modified; ~380 insertions)

```
 cgroupfs/controller/memory.rs   writable memory controller
 net/net_ns.rs                   (new) NetNamespace
 process/namespace/pid_ns.rs     (new) hierarchical PidNamespace + allocator
 process/process/mod.rs          Process gains pid_ns + vpid + pid_nr_in()
 process/process/init_proc.rs    init process -> initial PID namespace
 process/clone.rs                CLONE_NEWPID create + caller-ns return value
 process/wait.rs                 WaitStatus::pid_in_ns()
 syscall/{getpid,getppid,gettid} namespace-local PID/PPID/TID
 syscall/{wait4,waitid}          report child PID in waiter's namespace
 syscall/setns.rs                join a network namespace
 process/namespace/nsproxy.rs    NetNamespace wiring; accept CLONE_NEWPID
 procfs/pid/task/ns.rs           /proc/<pid>/ns/{net,pid}
 pseudofs/nsfs.rs                NsType::Net, ::Pid activated
 OSDK.toml + module wiring
```

## Remaining Part 1 work (prioritized)

1. ~~PID namespace renumbering~~ — **done** (see §3); remaining refinements listed there.
2. ~~NET namespace per-ns loopback isolation~~ — **done** (see §2a); veth between
   namespaces and per-ns routing/netlink remain.
3. ~~cgroup memory enforcement~~ — **done** (§4). ~~cgroup cpu enforcement~~ —
   **done via `cpu.max`** (§5). `cpu.weight` (proportional shares) still needs
   hierarchical scheduling.
4. ~~veth between namespaces~~ — **done** (§6): veth device + pair creation +
   subnet routing; cross-namespace connectivity verified.
5. ~~RTNETLINK veth creation~~ — **done** (§7): `RTM_NEWLINK` (veth, peer placed by
   `IFLA_NET_NS_PID`) + `RTM_NEWADDR`, netns-aware, driven by a real Go netlink
   client (the `prctl` shim is removed).
6. ~~ICMPv4~~ — **done** (§8). ~~`RTM_NEWROUTE` + bridging~~ — **done** (§9).
   ~~Services/NAT datapath~~ — **done** (§10: ClusterIP DNAT/conntrack, UDP+TCP,
   load balancing). ~~Inter-bridge forwarding~~ — **done** (§11: the host
   namespace routes between pod subnets). ~~Masquerade/SNAT~~ — **done** (§12:
   uplink source-NAT + reverse). Remaining networking, in order:
   - **Real `eth0` egress**: emit a masqueraded frame out the physical NIC and
     intercept the reply (raw injection/interception on the virtio device);
     §12's SNAT engine already does the translation. slirp makes real internet
     egress in the test VM unreliable regardless.
   - **`nftables`-compatible `NETLINK_NETFILTER`** surface, to replace the
     temporary `PR_ASTERKUBE_DNAT` `prctl` so real `kube-proxy`/`nft` program the
     §10 engine unmodified.
   - **VXLAN overlay** (multi-node pod networking); `IFLA_NET_NS_FD`,
     `RTM_SETLINK`, per-ns kernel netlink sockets.
7. User namespaces.
