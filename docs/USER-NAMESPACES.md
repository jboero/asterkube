# User Namespaces on Asterinas — staged implementation plan

**Status: ROOTLESS DONE — secure AND functional, verified in-VM.** Stage 0–1
(`9ed051af9`) + capability boundary (`cf06bc683`) + uid/gid mapping
(`00427f5d5`); probes `kubelet ac9bf04`/`68bc863`/`490f5d1`. Tag
`astrokube-rootless`. Only remaining userns item is `setns(NEWUSER)` (Stage 4),
which joins an existing userns — not needed for rootless containers. 2026-06-29.

> **Stage 2 (id mapping) done:** UserNamespace stores write-once uid_map/gid_map
> + setgroups flag; `/proc/[pid]/{uid_map,gid_map,setgroups}` are writable
> (Linux auth: CAP_SETUID/SETGID in parent, or unprivileged single self-map with
> setgroups denied); getuid/geteuid/getgid/getegid translate to the caller's ns
> (identity for init → no regression). DAC stays in global uids so
> check_permission needs no translation; capabilities unchanged so the boundary
> holds. Verified: unprivileged uid 1000 maps 0->1000, child sees getuid=0/
> getgid=0. **Rootless is complete: a container root looks and acts like root
> inside, with zero power on the host.**

> **The capability boundary (the security core) is done — done a safer way than
> Linux.** Rather than widening a new userns's capability set to full and then
> ns-scoping every capability check (miss one = privesc), `UserNamespace::
> check_cap` grants "root within your own non-initial userns subtree" via
> namespace ancestry **without touching the global capability set**. So the
> direct `effective_capset()` checks (DAC_OVERRIDE, setuid, prctls, seccomp)
> remain empty-capset for an unprivileged container root → they deny host access
> by construction, no per-check auditing needed. Adversarial gate passes:
> unprivileged uid 1000 is denied creating a netns in init (EPERM), CAN after
> clone(CLONE_NEWUSER) (root in its own ns), and is denied signaling host PID 1
> (EPERM). No regression.
>
> **What's left for *functional* (not just secure) rootless = Stage 2 id
> mapping:** writable uid_map/gid_map + translating ids at the presentation
> boundary (getuid/stat) so a container sees itself as uid 0 and owns its files
> as mapped uids. This is container-compat/functionality, not a security gap —
> the boundary above holds regardless of mapping. It is DAC-adjacent (id
> confusion) so worth doing carefully.


> **Correction to the assessment below:** `clone(CLONE_NEWUSER)` did not silently
> no-op — the fork path rejected it with **EINVAL** in `clone_user_ns`. After
> Stage 1 it creates a real child namespace. Verified: a CLONE_NEWUSER child
> lands in a distinct user ns (parent `user:[2]` vs child `user:[26]`), with no
> privilege change and no regression.

**Why a plan and not a patch:** user namespaces are the one remaining Phase-1
item that is *security-critical* — a wrong implementation is a privilege-
escalation hole, and a timid one is inert. It also touches the most sensitive
code (credentials/capabilities) and must preserve the hard constraint: existing
Kubernetes workloads keep working. That combination makes it the wrong thing to
land in one unattended pass; it should be built in reviewable stages, each
boot-tested, with the security invariant checked at every step.

## Current state (measured, file:line)

- `clone(CLONE_NEWUSER)` **succeeds but is a silent no-op**: the flag passes the
  combination checks (`process/clone.rs:189-226`) but the child just clones the
  parent's userns Arc (`process/clone.rs:416`); `nsproxy` explicitly strips
  `CLONE_NEWUSER` (`namespace/nsproxy.rs:59,230`). Userspace thinks it got a new
  user namespace and did not — a semantic lie.
- `setns(CLONE_NEWUSER)` is explicitly rejected (`syscall/setns.rs:67-68,122-123`).
- `UserNamespace` (`process/namespace/user_ns.rs`) is a **singleton**: no parent,
  no level, `owner_uid()` hard-returns root, `is_same_or_ancestor_of()` is
  `ptr_eq`, `check_cap()` ignores the namespace and tests the thread's single
  global effective capset.
- `/proc/[pid]/uid_map` and `/gid_map` exist but are **read-only identity stubs**
  (`fs/.../procfs/pid/task/uid_map.rs` writes `0 0 INVALID`); no `setgroups`.
- Capabilities are a single per-thread set (`credentials/credentials_.rs:71`),
  not scoped to a namespace. There are **only 18 `check_cap(` call sites**, and
  most privileged ops already funnel through `UserNamespace::check_cap` — a
  genuine seam that makes ns-aware enforcement tractable.

## The security invariant (must hold after every stage)

1. A process in a **new** user namespace has a full capability set **within that
   namespace and its descendants**, and **no** capability over resources owned by
   ancestor namespaces (except through a valid id mapping).
2. "Root in the container" (uid 0 in a new userns) must map to an **unprivileged**
   uid in the parent — never to real host root unless explicitly mapped.
3. Unmodified workloads (no new userns, no maps) must behave exactly as today
   (tenant-0-style: untouched). This is the k8s-compat guarantee.

A quick way to keep yourself honest: an adversarial probe (like the seccomp/MAC
ones) that creates a userns as a mapped non-root user and then attempts a
host-privileged operation — it MUST fail with EPERM with the mapped uid.

## Staged plan

**Stage 0 — UserNamespace becomes a tree (no behavior change).**
Add `parent: Option<Arc<UserNamespace>>`, `level: u32`, `owner_uid: Uid`, and
empty uid/gid map slots to `UserNamespace`. Keep `get_init_singleton` as the
root (level 0). Make `is_same_or_ancestor_of` walk the parent chain. No clone/
setns changes yet → nothing observable changes. Boot-test: green baseline holds.

**Stage 1 — `clone(CLONE_NEWUSER)` creates a real child userns.**
In `process/clone.rs`, when `CLONE_NEWUSER` is set, build a child `UserNamespace`
with `parent = current`, `level+1`, `owner_uid = caller's euid`, and set it as
the child's user_ns (stop stripping it in nsproxy). Per Linux: the child starts
with an **empty** uid/gid map and the creating process gets a full capability set
**in the new ns**. To stay safe before Stage 3, scope this by recording the
"caps valid in this userns" — do NOT widen the global capset. Boot-test: a child
can `clone(CLONE_NEWUSER)`, `/proc/self/ns/user` differs, `NS_GET_PARENT` works.

**Stage 2 — writable uid_map/gid_map + setgroups.**
Make `uid_map.rs`/`gid_map.rs` writable with the Linux rules (write-once; ranges;
the writer needs the right caps in the parent ns; `setgroups` must be denied
before a gid_map is written by an unprivileged mapper). Store ranges on the
userns; `read_at` reflects them. Add ID translation helpers
(`ns_to_init(uid)`, `init_to_ns(uid)`). Boot-test: write `0 100000 65536`, read
it back, verify translation.

**Stage 3 — capabilities & id checks become ns-aware (the crux).**
Route the 18 `check_cap` sites through `UserNamespace::check_cap` so a capability
is honored only if the thread has it **in the userns that owns the target
resource** (or an ancestor, via mapping). A process has full caps in a userns it
created. Resources (inodes, processes, sockets) must expose their owning userns
(many already reachable via their namespaces). Translate uids at the syscall
boundary: `getuid`/`stat`/ownership checks report/compare ns-local ids. This is
the stage that delivers rootless containers AND where a bug = privesc, so it
needs the adversarial probe above as a gate. Boot-test: mapped-non-root userns
cannot touch host-root resources; mapped root inside can manage its own.

**Stage 4 — `setns(CLONE_NEWUSER)` + polish.**
Allow joining an existing userns (`syscall/setns.rs`), with the Linux rule that
you may not join your own/an ancestor and you need the right caps. Update
`owner_uid()`, `NS_GET_PARENT` (already half-wired in `user_ns.rs`). Boot-test:
join a peer userns by fd.

## Interaction with astrokube's other work

- **Rootless + astromac**: once Stage 3 lands, a tenant's pods can run with uid 0
  mapped to an unprivileged host uid, *and* be astromac-labeled — defense in
  depth (a userns escape still hits the MAC).
- **seccomp**: unaffected; orthogonal.
- **CRI setns-into-PID-ns** (the other deferred gap) is independent of userns but
  similarly central; can proceed in parallel.

## Recommendation

Do Stage 0–1 first (safe, observable, low blast radius), boot-test, commit.
Then Stage 2. Treat Stage 3 as its own reviewed change with the adversarial
probe as the acceptance test — that is the one I would not merge without a human
in the loop, because it rewrites privilege checks.
