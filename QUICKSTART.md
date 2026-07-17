# Quick start

Two ways to run asterkube on a fresh machine: boot a **pre-built release image**
(fast, no toolchain — recommended for a demo), or **build from source**.

---

## A. Boot a released image (recommended)

**Needs:** `qemu-system-x86_64`, UEFI firmware (`edk2-ovmf` on Fedora, `ovmf` on
Debian/Ubuntu), and KVM (`/dev/kvm`, optional but much faster).

```bash
# 1. download the release artifacts
gh release download asterkube-v0.1 --repo jboero/asterkube
#    (or grab the .iso / .qcow2 from https://github.com/jboero/asterkube/releases)
sha256sum -c SHA256SUMS

# 2. boot it — the helper auto-detects OVMF and uses KVM when available
curl -sO https://raw.githubusercontent.com/jboero/asterkube/main/scripts/run-release.sh
chmod +x run-release.sh
./run-release.sh asterkube-node-v0.1.iso        # or the .qcow2
```

The node boots in a few seconds, DHCPs, runs its seccomp / MAC / container
capability demos, and then **stays LIVE — it will not power off on its own**.

- **Console:** you're looking at it (serial/virtio console on your terminal).
- **Shut down gracefully:** press `Ctrl-a` then `c` for the QEMU monitor, type
  `system_powerdown` (the node drains via ACPI and powers off). `Ctrl-a` then `x`
  quits QEMU immediately.

> If you don't have the `run-release.sh` helper, the equivalent one-liner is a
> plain `qemu-system-x86_64 … -cdrom asterkube-node-v0.1.iso …` through OVMF — the
> script just fills in the OVMF paths and the virtio-net flags the Asterinas
> driver expects.

### Join a Kubernetes cluster (optional)

The released image runs standalone as a **demo**. To make it register with *your*
cluster, mint a join bundle (a small config disk with a host-issued node cert) and
attach it to the VM — no rebuild needed:

```bash
scripts/asterkube-join.sh -n asterkube -s <apiserver-ip:port>   # writes asterkube-join.img
# boot the released ISO/qcow2 with the bundle attached as a virtio-blk disk
# (serial=asterkubecfg); the node mounts it, installs the bundled CNI, and joins.
kubectl get nodes    # asterkube ... Ready
```

Verified: the node registers and reaches `Ready`. See the README "join a cluster"
section for the full QEMU line.

---

## B. Build from source

**Needs on the host:** Docker, Go, `qemu-system-x86_64` + OVMF, and initramfs
tooling (`cpio`, `zstd`, `readelf`/binutils). The container image below carries the
Rust + OSDK toolchain, so you don't install those on the host.

```bash
# 1. clone with the kernel submodule
git clone --recursive https://github.com/jboero/asterkube && cd asterkube

# 2. create the Asterinas OSDK build container (named 'asterkube') and install cargo-osdk
docker run -d --name asterkube --privileged --network=host -v /dev:/dev \
    -v "$PWD/asterinas:/root/asterinas" -w /root/asterinas \
    asterinas/asterinas:0.18.0-20260603 sleep infinity
docker exec asterkube bash -lc 'OSDK_LOCAL_DEV=1 cargo install cargo-osdk --path osdk'

# 3. build the userspace + kernel image
scripts/build-containerd-merged.sh                 # static containerd+ctr → build/static-bins
scripts/build-hello-image.sh                       # the demo image (needs buildah/skopeo)
scripts/build-zeroc-kubelet.sh                     # combined CGO-free kubelet (fetches k8s v1.35.6)
INIT_BIN=build/asterkube-kubelet scripts/zero-c-initramfs.sh   # zero-C initramfs + bootable ISO
scripts/build-qcow2.sh                             # (optional) convert the ISO to a QCOW2

# 4. boot what you built
scripts/run-release.sh asterinas/target/osdk/aster-kernel-osdk-bin.iso
```

The container name **must** be `asterkube` (the build scripts `docker exec asterkube …`).
The kernel toolchain is pinned to `nightly-2026-04-03` inside the image; the first
`cargo osdk build` compiles the kernel and takes a few minutes.

### C. Boot with no GRUB2 (fully C-free) via rubu

The released ISO boots via GRUB2 (C). To remove that last C component, boot through
[rubu](https://github.com/jboero/rubu), a pure-Rust UEFI bootloader — the whole chain
becomes C-free (rubu → Asterinas → Go kubelet). Build an EFI-handover bzImage and boot it:

```bash
git clone https://github.com/jboero/rubu ../rubu     # sibling checkout
docker exec asterkube bash -lc \
  'cd /root/asterinas/kernel && cargo osdk build --grub-boot-protocol linux --boot-method qemu-direct'
RUBU=../rubu \
KERNEL=asterinas/target/osdk/aster-kernel-osdk-bin \
INITRD=asterinas/test/initramfs/build/initramfs.cpio.gz \
JOIN_BUNDLE=asterkube-join.img \
scripts/boot-via-rubu.sh
```

Verified: booted via rubu, the node still joins a cluster and reaches `Ready`. See the
README's **Fully C-free boot** section.

---

## Troubleshooting

- **`OVMF firmware not found`** — install `edk2-ovmf` (Fedora) or `ovmf`
  (Debian/Ubuntu), or point the helper at it: `OVMF_CODE=/path/OVMF_CODE.fd ./run-release.sh …`.
- **Slow boot / high CPU** — you're on TCG (no KVM). Ensure `/dev/kvm` exists and is
  writable (add your user to the `kvm` group, or run with the right permissions).
- **Nothing on screen** — the console is serial; the helper runs `-nographic`, so
  output appears in the terminal you launched it from.
- **It powered off after the demo** — you're running an old image without persist;
  use the `v0.1` release (or rebuild), which sets `ASTERKUBE_PERSIST=1`.
