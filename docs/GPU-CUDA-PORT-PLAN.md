# Plan: native NVIDIA compute in an Asterinas guest (`--features nvidia`)

Status: **proposal / not started.** This is an engineering plan for running a
CUDA stack inside an Asterinas VM by porting the open-source NVIDIA GPU kernel
driver (`NVIDIA/open-gpu-kernel-modules`, "nvidia-open") into Asterinas as a
native, Rust-glued, optionally-compiled component. It is deliberately scoped to
be finite rather than open-ended.

Related: [FEATURES.md](FEATURES.md), [KERNEL-CHANGES.md](KERNEL-CHANGES.md),
`astrokube/POC-STATUS.md` in the kernel tree (the CPU-node PoC this builds on).

---

## 1. Scope (deliberately narrow)

- **Headless, compute-only CUDA** — no display / KMS / graphics / Vulkan.
- GPU **passed through (VFIO)** to the VM; the driver runs **natively in
  Asterinas** (not remoted — see §5 for the remoting escape hatch used only to
  de-risk userspace).
- **Turing or newer only.** nvidia-open requires a GSP (GPU System Processor);
  Pascal/Volta and older need the closed modules and are out.
- Gated behind a cargo feature so **default builds stay 100% C-free**.
- **Two tracks must both land:** (1) the kernel driver, (2) enough Linux
  userspace ABI to run the closed CUDA userspace.

Explicitly out of scope for v1: display (nvidia-modeset / nvidia-drm), NVLink
peer memory (nvidia-peermem), MIG, multi-GPU, graphics/Vulkan, pre-Turing.

---

## 2. The one architectural decision that makes this finite

nvidia-open is two very different bodies of code:

| Part | What it is | Decision |
|---|---|---|
| **`src/nvidia/` — the RM core** | OS-agnostic Resource Manager; huge; mostly `nvoc`-generated C; the part that actually talks to GSP. | **Keep as vendored, compiled C.** Do NOT rewrite in Rust — it's millions of LOC and re-diverges every driver release. |
| **`kernel-open/nvidia*/` — the OS glue** | Linux-specific layer: memory, DMA, IRQ, PCI, char devices, mmap, UVM↔mm hooks. ~tens of thousands of LOC. | **Rewrite in Rust.** This is the kernel-touching, safety-critical, Asterinas-specific surface — the only place Rust actually buys safety. |

→ **Hybrid: Rust OS-layer + uAPI, C RM core, behind the flag.** The default
kernel stays C-free; opting into `nvidia` accepts the vendored C RM.

Purist alternatives, for the record:
- **`c2rust` the RM** into *unsafe* Rust — nominally Rust, semantically C,
  re-transpiled every release. Possible, low value.
- **Full hand-rewrite of the RM** — infeasible; don't.

**The GSP tailwind** is why this is very-hard rather than impossible: on Turing+,
most RM logic runs as **firmware on the GPU's GSP**, and the host-side RM is
increasingly an **RPC client** over shared-memory message queues. That shrinks
the host surface you must get exactly right and makes the boundary cleaner and
more portable.

---

## 3. Track 1 — kernel driver (the long pole)

### P0 — PCI / DMA substrate  *(weeks, low risk)*
Extend Asterinas as needed: PCI config space, **large / resizable-BAR** mapping,
**MSI-X** interrupts, IOMMU + DMA-coherent allocation, MMIO accessors. Much of
the PCI/virtio plumbing exists (`comps/pci`, `comps/virtio`); the GPU adds
large-BAR + MSI-X + real DMA mapping.

### P1 — RM brings the GPU up  *(months, highest risk)*
Implement the nvidia-open portability interfaces (`nvport` + the `nv` OS
interface, the `nv-linux.h` equivalents) in Rust; compile + link the C RM as
`comps/nvidia`; load signed **GSP firmware**; complete RM init + the GSP RPC
handshake.
**Milestone: "GPU detected + GSP booted"** — the in-kernel equivalent of the
driver probing the card. This is the make-or-break vertical slice; build it
*first and thin*.

### P2 — UVM  *(months, high risk)*
Port the `nvidia-uvm` OS layer onto Asterinas's MM: device memory, GPU page
faults, page migration, the mmu-notifier-equivalent, DMA. CUDA managed / unified
memory depends on this; it is the chunk most coupled to Asterinas internals.

### P3 — uAPI  *(weeks–months, medium risk)*
Expose `/dev/nvidiactl`, `/dev/nvidia0`, `/dev/nvidia-uvm` (+ `-uvm-tools`) with
**byte-exact ioctl semantics** and mmap of GPU BARs / allocations into
userspace — this is what `libcuda` actually calls.

---

## 4. Track 2 — run the closed CUDA userspace  *(parallel, own hard problems)*

Closed `libcuda.so.1` + the CUDA runtime are **dynamically-linked glibc**
binaries. Asterinas runs static musl today; this needs a working **dynamic
linker + glibc-level ABI**, `dlopen`, TLS models, plus the P3 device-node
fidelity.

- **De-risk with the remoting escape hatch:** early on, keep almost no CUDA in
  the guest — a thin client that forwards CUDA driver-API calls over
  virtio-vsock to a host proxy holding the real GPU. This proves the *kernel*
  driver end-to-end **before** betting on full glibc completeness. Migrate the
  userspace in-guest as the ABI matures.
- Milestones: RM query (`nvidia-smi`-equivalent) → `deviceQuery` → a cuBLAS
  SGEMM → PyTorch inference.

---

## 5. Build order — de-risk, don't build bottom-up

1. **Thinnest vertical slice first:** P0 essentials → **P1 GSP boot** → one RM
   control RPC over MSI-X → one DMA buffer. If GSP won't init under Asterinas's
   memory/DMA model, stop and rethink — everything downstream depends on it.
2. Then P2 (UVM) → P3 (uAPI) → Track 2, each hitting its milestone.

---

## 6. Build-flag mechanics (keeps the C-free promise intact)

- Cargo feature `nvidia` on the kernel crate → a new `comps/nvidia` component
  (mirrors `comps/nvme` / `comps/virtio`).
- `build.rs` + `cc` / `bindgen` vendors and compiles the C RM **only** when the
  feature is on; `bindgen` generates the FFI the Rust OS-layer implements
  against.
- **CI matrix:** default (C-free, must stay green) **and** `--features nvidia`.
- `scripts/vendor-nvidia-open.sh` pins / re-vendors nvidia-open + the matching
  GSP firmware per release.

---

## 7. Risks / realities (honest)

- **GSP firmware:** signed NVIDIA blob, **version-locked** to the RM;
  redistribution has its own license; must match the vendored RM exactly.
- **Upstream churn:** RM + firmware move together every release — budget
  re-vendor + re-glue each bump.
- **DMA / IOMMU coherency + large BAR** under Asterinas's MM is subtle and a
  classic failure point.
- **Closed userspace ABI:** the glibc / dynamic-linking lift is itself a project
  — hence the remoting shim first.
- **Licensing:** nvidia-open is dual **MIT / GPLv2** — take the **MIT** path for
  the Rust derivative to avoid GPL-tainting the MPL-2.0 kernel; keep firmware
  handling clean and separate.
- **Effort:** compute-only, single-GPU, headless is still **multiple
  engineer-quarters**; P1 (GSP boot) alone can eat a quarter. This is a
  research-grade undertaking, not a feature.

---

## 8. Fastest credible demo

Kernel Track to **"GSP booted + RM answers a control RPC,"** with Track 2 via the
**remoting shim** for the userspace — i.e. a real CUDA call hitting a
passed-through GPU from an Asterinas guest, with the *kernel* driver native and
Rust-glued. That proves the architecture before committing to the UVM and
glibc-userspace marathons.

---

## 9. Immediate next step (P0 reconnaissance)

Before any porting, audit what the Asterinas `integration` tree can do **today**
against what GSP boot needs, and turn it into a concrete P0 task list:

- PCI: config-space access, BAR enumeration, **resizable BAR** support?
- Interrupts: **MSI-X** allocation + per-vector routing?
- DMA: coherent allocation, IOMMU integration, address-width limits?
- MMIO: large mappings, write-combining / cache attributes?
- MM: what's the closest primitive to `mmu_notifier` / HMM for UVM (P2)?

The gaps found there become the P0 backlog and size the rest of the plan.

---

## 10. Progress log

### 2026-07-11 — P0 done + scaffold landed

**Target hardware (this machine):** NVIDIA **RTX A4000** `10de:24b0` (GA104,
Ampere — GSP-capable, `nvidia-open`-supported). BAR0 16 MiB regs, BAR1 256 MiB
VRAM aperture, BAR3 32 MiB, MSI-X. Host runs **nvidia-open 595** (`Dual MIT/GPL`)
with **`gsp_ga10x.bin`** present — exactly the RM + firmware the port targets.

**Hard blocker for testing on this machine (needs operator action):**
- **IOMMU is disabled** — no `intel_iommu=on` on the kernel cmdline, `0` IOMMU
  groups. **VFIO passthrough is impossible without it**, so the GPU cannot be
  handed to an Asterinas guest at all. Enabling needs a **grub cmdline change +
  BIOS VT-d + reboot**.
- The **A4000 is the live display GPU** (`card1`, `nvidia-drm.modeset=1`); it
  must be freed (move display to the other GPU, or go headless) before it can be
  bound to `vfio-pci`.
- The spare **Quadro K4200** is Kepler (pre-Turing, no GSP) — not a valid target.

Until IOMMU is on and the A4000 is freed, *nothing on the native path can be
runtime-tested* — the first testable milestone ("GPU visible inside Asterinas")
itself requires passthrough.

**Asterinas substrate audit (`integration` tree):** PCI (config space, BARs,
capabilities), **MSI-X** (`comps/pci/.../msix.rs`), DMA (`DmaStream`/
`DmaCoherent`), and an x86 **VT-d IOMMU** driver already exist — a solid P0 base.

**Landed (compiles, verified both ways):** a C-free **`comps/nvidia`** component
+ **`nvidia_gpu`** cargo feature on the kernel:
- Off by default → `aster-nvidia` is **not compiled** (default kernel stays
  C-free, verified).
- On (`--features nvidia_gpu`) → registers a `PciDriver` that enumerates NVIDIA
  display-class devices, **classifies GSP capability** (Turing+), reads
  `NV_PMC_BOOT_0` from BAR0, acquires MSI-X, and stubs the **P1** GSP-boot entry.
  Compiles clean and is linked + enumerated at runtime.

**Next (P1, blocked on hardware access):** vendor `nvidia-open` 595, implement
the `nvport`/`nv` OS interface in Rust, link the C RM behind `nvidia_gpu`, load
`gsp_ga10x.bin`, and complete the RM/GSP handshake → the "GPU detected + GSP
booted" milestone. Cannot be exercised here until IOMMU + passthrough are
available.
