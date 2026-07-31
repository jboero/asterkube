# Phase A implementation spec — boot the GSP to `GSP_INIT_DONE`

Turnkey reference for implementing the GA10x (GA102 / RTX A5000) GSP boot in the
Asterinas `aster-nvidia` driver, cross-referenced to open-gpu-kernel-modules tag
**610.43.02** (matches the target's `/lib/firmware/nvidia/610.43.02/`). This is
the front half of [CUDA-CONTAINER-PLAN.md](CUDA-CONTAINER-PLAN.md) Phase A.
Everything below is source-confirmed unless marked `[INFERRED]`.

**Status (hardware-validated on a real RTX A5000; code on asterinas branch
`cuda-p1.5c`):**
- ✅ FB size read (`LOCAL_MEMORY_RANGE` → 16 GB, decode verified) + full WPR2
  layout computed on hardware.
- ✅ SEC2 booter run implemented: parse HS header, fuse-select + patch the RSA3K
  signature, DMA the signed booter IMEM/DMEM into SEC2, program BROM PKC.
- ✅ **The signed 610.43.02 booter executes on the real A5000** — SEC2 reset +
  `kflcnSwitchToFalcon` releases the `0xbadf` priv lockdown (CPUCTL `0x10`), the
  booter authenticates its own signature and halts.
- ⏳ It returns ACR code `MAILBOX0=0x91`. **Blocker identified (resource, not
  logic).** Six candidate classes were eliminated on real hardware, each a
  committed iteration on `cuda-p1.5c`: booter execution, WPR2 location (mmu-lock),
  `.fwsignature_ga10x`, full 256-byte meta byte-correctness, GSP reset-into-RISC-V
  ordering, and radix3 PTE format (bare `RmPhysAddr`, matches `kgspCreateRadix3`).
  `0x91` survives all. The remaining prerequisite is **scrubbing/unlocking the FB
  region** the booter DMAs WPR2 into: our WPR is ~212 MB (84 MB firmware + heap,
  min 88 MB), larger than the pre-scrubbed top-of-FB region, so it needs the
  **scrubber ucode** first — which is **not present in open-gpu-kernel-modules**
  (no `g_bindata_*Scrubber*`), unlike the booter. The alternative prerequisite,
  **FWSEC-FRTS**, is parsed from the **VBIOS ROM** (`kernel_gsp_fwsec.c`, BIT
  tokens), a separate multi-hour sub-project.
  - **Seventh elimination (HW, this session):** set `frtsSize=0` in the meta —
    i.e. told the booter *there is no FRTS region at all*. Still `0x91`
    (`MAILBOX1=0x2` unchanged). If `0x91` meant "FRTS missing/invalid," removing
    the FRTS requirement would have changed the code. It did not — so **`0x91` is
    not FRTS-gated**, which *deprioritizes the FWSEC-from-VBIOS sub-project* and
    points at the more fundamental **unscrubbed-FB / WPR-scrub** gate, whose
    scrubber ucode is not in the open driver.
  So `0x91 → 0` requires obtaining a
  prerequisite ucode not available from the open driver. Once `MAILBOX0==0` + `WPR2_ADDR_HI!=0` +
  `verified==0xa0a0…`, proceed to §4 (GSP kick + msgq + `GSP_INIT_DONE`).

---

## 0. Firmware inputs (all extracted, verified byte-exact)

`scripts/extract-gsp-booter.py <ogkm-610> <out>` produces, from the open bindata
(raw-DEFLATE `wbits=-15`; NVUCODES/`ucodes_ga10x.bin` is a dead end — no in-source TOC):

| file | size | role |
|---|---|---|
| `booter_load.img` | 60416 | SEC2 HS booter (image = IMEM code + DMEM data) |
| `booter_load.sig` | 768 | RSA3K PROD signature (loaded into SEC2 DMEM; addr → BROM PARAADDR) |
| `booter_load.hdr` | 36 | HS header (IMEM/DMEM/sig sub-region offsets — parse to split the image) |
| `gsprmboot.img` | 24576 | GSP RISC-V bootloader (DMA'd to sysmem; addr → WprMeta) |
| `gsprmboot.desc` | 84 | `RM_RISCV_UCODE_DESC` (below) |

Plus `gsp_ga10x.bin` (84 MB, already staged via `GSP_FW`): its `.fwimage` is
radix3-paged (P1.5b, done) and its phys addr → `WprMeta.sysmemAddrOfRadix3Elf`.

**Delivery:** bake all five into the initramfs (extend `zero-c-initramfs.sh`
`GSP_FW` staging) at fixed paths, e.g. `/booter_load.{img,sig,hdr}`,
`/gsprmboot.{img,desc}`; driver reads them in `load_gsp_firmware` after rootfs.

**`RM_RISCV_UCODE_DESC`** (`rmRiscvUcode.h`, 21×u32 for this build), decoded from
`gsprmboot.desc`: `version=5`, `bootloaderOffset=0x5000`, `bootloaderSize=0x880`,
`manifestOffset=0x0`, `manifestSize=0x800`, `monitorDataOffset=0x800`,
`monitorDataSize=0x1000`, `monitorCodeOffset=0x1800`, `monitorCodeSize=0x2900`,
`fbReservedSize=0x6000`. WprMeta uses monitor{Code,Data}Offset + manifestOffset.

---

## 1. `GspFwWprMeta` (256 B) — populate then hand its phys addr to the booter

`boot.rs` already has the byte-exact struct + `magic=0xdc3aae21371a60b3`,
`revision=1`. Fill per `kgspPopulateWprMeta_TU102` (`kernel_gsp_tu102.c:755`),
FB layout computed **top-down**:

```
fbSize                = kmemsysGetUsableFbSize()            # read from GPU
vgaWorkspaceOffset    = fbSize - DRF_SIZE(NV_PRAMIN)        # =1MB, unless display fuse/VGA base
vbiosReservedOffset   = min(mmuLockLo, vgaWorkspaceOffset)  # mmuLock via memmgrReadMmuLock; else vgaWorkspaceOffset
sizeOfRadix3Elf       = fwimage size
gspFwWprEnd           = ALIGN_DOWN(vbiosReservedOffset - wprEndMargin, 128K)   # WPR_ALIGNMENT=128K
frtsSize              = 1MB ; frtsOffset = gspFwWprEnd - frtsSize
sizeOfBootloader      = 24576 ; bootBinOffset = ALIGN_DOWN(frtsOffset - sizeOfBootloader, 4K)
gspFwOffset           = ALIGN_DOWN(bootBinOffset - sizeOfRadix3Elf, 64K)
nonWprHeapSize        = ALIGN_UP(kgspGetNonWprHeapSize(), 1MB)
wprHeapSize           = kgspGetFwHeapSize(pre, post)       # see §1a
gspFwHeapOffset       = ALIGN_DOWN(gspFwOffset - wprHeapSize, 1MB)
gspFwHeapSize         = ALIGN_DOWN(gspFwOffset - gspFwHeapOffset, 1MB)
gspFwWprStart         = gspFwHeapOffset - ALIGN_UP(256, 1MB)   # =gspFwHeapOffset - 1MB
nonWprHeapOffset      = gspFwWprStart - nonWprHeapSize
gspFwRsvdStart        = nonWprHeapOffset
sysmemAddrOfRadix3Elf = phys(radix3 root)                  # P1.5b output
sysmemAddrOfBootloader= phys(gsprmboot.img in sysmem)
bootloaderCodeOffset  = desc.monitorCodeOffset (0x1800)
bootloaderDataOffset  = desc.monitorDataOffset (0x800)
bootloaderManifestOffset = desc.manifestOffset (0x0)
bootCount=0 ; verified=0 ; (flags |= CLOCK_BOOST if enabled)
```

### 1a. Sizing constants (`gsp_fw_heap.h`, `kernel_gsp.c`)
- `WPR_ALIGNMENT = RM_PAGE_SIZE_128K = 0x20000`; `frtsSize = 1MB`.
- Heap params: `OS_SIZE_LIBOS3_BAREMETAL=22MB`, `BASE_RM_SIZE_TU10X=8MB`,
  `SIZE_PER_GB=96KB`, `CLIENT_ALLOC_SIZE=48KB*2048`. `kgspGetFwHeapSize` →
  `_kgspCalculateFwHeapSize` (registry `heapSizeMBOverride` else computed; clamp
  min/max HAL). `kgspGetNonWprHeapSize` — transcribe from source.
- `kgspGetWprEndMargin`: 0 via registry default on first populate; the recursive
  branch is only for re-populate. Start with margin **0**. `[INFERRED-OK]`
- `DRF_SIZE(NV_PRAMIN) = 1MB`. FB size read: replicate `kmemsysGetUsableFbSize`
  (BAR0 FB config) or, for bring-up, read the A5000's 24 GB and subtract known
  reserved. **Getting these approximately right is fine to start — the booter
  returns a specific `MAILBOX0` error if the layout is invalid; iterate on it.**

---

## 2. Reset GSP into RISC-V + program BROM (before booter)

`kflcnResetIntoRiscv_GA102`. GSP falcon base `0x110000`, RISC-V base
`NV_FALCON2_GSP_BASE = 0x111000`. Current `gsp::reset()` does the falcon reset;
**add** the BCR program:
- pre-reset wait `HWCFG2.RESET_READY`; reset; wait `HWCFG2.MEM_SCRUBBING==0` (bit12).
- `RISCV_BCR_CTRL (0x111668) = 0x111` (`CORE_SELECT_RISCV|VALID|BRFETCH`) — confirmed.
- Then program GSP boot-args mailbox (§4): `MAILBOX0(0x110040)=lo32`, `MAILBOX1(0x110044)=hi32` of the LibOS-init-args phys addr.

## 3. Run the SEC2 booter (`kgspExecuteBooterLoad_TU102` / `kgspExecuteHsFalcon_GA102`)

SEC2 falcon base `0x840000`; riscv/BROM base `0x841000`; FBIF `0x840600`.
Falcon regs (add base): MAILBOX0 `0x040`, MAILBOX1 `0x044`, DMACTL `0x10c`,
DMATRFBASE `0x110`, DMATRFMOFFS `0x114`, DMATRFCMD `0x118`, DMATRFFBOFFS `0x11c`,
DMATRFBASE1 `0x128`, CPUCTL `0x100`, CPUCTL_ALIAS `0x130`, BOOTVEC `0x104`,
HWCFG2 `0x0f4`. BROM regs (add `0x841000`): MOD_SEL `0x180`, CURR_UCODE_ID `0x198`,
ENGIDMASK `0x19c`, PARAADDR(0) `0x210`.

Sequence:
1. reset SEC2; disable ctx (`FBIF_CTL.ALLOW_PHYS_NO_CTX=ALLOW`; `DMACTL=0`).
2. `FBIF_TRANSCFG(0)` (=`0x840600`) = `TARGET_COHERENT_SYSMEM | MEM_TYPE_PHYSICAL`.
3. DMA booter **IMEM**: cmd `0x614` (`SIZE_256B<<8 | IMEM<<4 | SEC<<2`). Per 256-B
   block: poll `DMATRFCMD.FULL(bit0)==0`; `DMATRFBASE=lo32(src>>8)`,
   `DMATRFBASE1=hi32(src>>8)&0x1ff`, `DMATRFMOFFS=dest`, `DMATRFFBOFFS=memoff`,
   `DMATRFCMD=cmd`; advance +256. After: poll `DMATRFCMD.IDLE(bit1)==1`.
   src = phys(booter img)+codeOffset; sizes/offsets from the 36-B HS header.
4. DMA booter **DMEM**: cmd `0x600` (or `0x10600` with `SET_DMTAG` bit16).
5. BROM PKC: `PARAADDR(0)=hsSigDmemAddr` (where sig was DMA'd in DMEM),
   `ENGIDMASK=engineIdMask`, `CURR_UCODE_ID=ucodeId`, `MOD_SEL.ALGO=RSA3K(1)`.
6. `BOOTVEC = imemVa`; `MAILBOX0/1 = lo/hi(phys(WprMeta))`.
7. start: `CPUCTL.STARTCPU(bit1)=1` (or `CPUCTL_ALIAS` if `ALIAS_EN`).
8. poll `CPUCTL.HALTED(bit4)==1`; **success iff `MAILBOX0==0`**, `WPR2_ADDR_HI
   (0x1FA828)!=0`, and `WprMeta.verified==0xa0a0a0a0a0a0a0a0`. Nonzero MAILBOX0 =
   booter error code — the debug signal to iterate the WprMeta layout.

The 36-B `booter_load.hdr` gives IMEM/DMEM/sig sub-region offsets to split the
60416-B image; parse per `kernel_gsp_booter.c` (`ucodeBootFromHs`). **[decode the
36 bytes against that struct]**.

## 4. Kick GSP + reach `GSP_INIT_DONE` (back half — Agent B, source-cited)

After the booter unlocks WPR2 it releases the GSP RISC-V core (inside the signed
blob; driver does not write GSP `CPUCTL`). Driver then:
1. `FALCON_OS(0x110080)=desc.appVersion`; poll `RISCV_CPUCTL(0x111388).ACTIVE_STAT(bit7)==1`.
2. Shared msgq block in **cached** sysmem: `pageTable(1pg) + cmdQueue(256KB) + statusQueue(256KB)`;
   pageTable[0..] = phys of each 4KB page; `sharedMemPA = pageTable[0]`.
3. CPU cmd-queue `msgqTxHeader{version=0,size,msgSize=4096,msgCount,writePtr=0,
   flags=SWAP_RX(1),rxHdrOff=32,entryOff=4096}`; zero status queue (GSP inits it).
4. `GSP_ARGUMENTS_CACHED.messageQueueInitArguments`: `sharedMemPhysAddr=sharedMemPA`,
   `cmdQueueOffset=pageTableSize`, `statQueueOffset=+256KB`, `queueElementSizeMin=4096`,
   `Max=65536`, `queueHeaderAlign=4`, `queueElementAlign=12`.
5. LibOS init-args array: entry `id8=hash("RMARGS")`, `pa=phys(GSP_ARGUMENTS_CACHED)`,
   `size=0x1000`; write its phys addr to GSP `MAILBOX0/1 (0x110040/44)` (§2).
6. Send early RPCs (SET_SYSTEM_INFO, SET_REGISTRY): element =
   `mctpHeader | nvdmHeader(NVDM_TYPE_RM_RPC=0x25,vendor 0x10de) | checkSum |
   seqNum | rpc_message_header_v(signature 0x43505256,function,length,rpc_result,
   sequence) | data`; element XOR-32 checksum must total 0; advance cmd writePtr;
   **doorbell: write 0 to `NV_PGSP_QUEUE_HEAD (0x110C00)`**.
7. Link status queue (spin until GSP publishes its status `msgqTxHeader`).
8. Poll for **`GSP_INIT_DONE = 0x1001`**: spin on GSP's `writePtr` in the status
   `msgqTxHeader` (sysmem, full fence each iter); on `writePtr!=readPtr` copy the
   slot, verify checksum==0, MCTP version==1, vendor==0x10de, `seqNum==rxSeqNum`;
   parse rpc header; match `function==0x1001 && sequence==0`; require
   `rpc_result==0`. **→ PHASE A COMPLETE.**

---

## Key source files (in the 610.43.02 tree)
- `arch/turing/kernel_gsp_tu102.c` — `kgspPopulateWprMeta_TU102`, `kgspExecuteBooterLoad_TU102`, `kgspProgramLibosBootArgsAddr_TU102`, `kgspSetCmdQueueHead_TU102`.
- `arch/turing/kernel_gsp_falcon_ga102.c` / `kernel_falcon_ga102.c` — `kgspExecuteHsFalcon_GA102`, `s_dmaTransfer_GA102`, `kflcnResetIntoRiscv_GA102`, `kflcnRiscvProgramBcr_GA102`, `kflcnIsRiscvActive_GA102`.
- `kernel_gsp.c` — heap/margin sizing, `kgspPopulateGspRmInitArgs`, `kgspSetupLibosInitArgs`, `_kgspRpcRecvPoll`, `kgspWaitForRmInitDone`.
- `message_queue_cpu.c` / `message_queue_priv.h` / `msgq_priv.h` — queue layout, MCTP/NVDM, checksum.
- `dev_gsp.h` (ampere/ga102), `dev_falcon_v4.h`, `dev_falcon_second_pri.h`, `dev_riscv_pri.h`, `dev_fb.h` (tu102) — register offsets.
