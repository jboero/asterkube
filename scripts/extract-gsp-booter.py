#!/usr/bin/env python3
# extract-gsp-booter.py — pull the signed, version-matched GSP boot ucodes out of
# NVIDIA's open-gpu-kernel-modules source, for the native Asterinas GSP boot
# (Phase A of docs/CUDA-CONTAINER-PLAN.md).
#
# WHY: the SEC2 "booter_load" HS ucode and the GSP RISC-V bootloader are
# NVIDIA-signed and cannot be authored. They are NOT shipped as standalone files
# in /lib/firmware for driver 610.43.x (that dir has only gsp_ga10x.bin +
# ucodes_ga10x.bin, and the NVUCODES container has no in-source table of
# contents). They ARE, however, compiled into the open driver as generated
# bindata (raw-DEFLATE-compressed C byte arrays). This script inflates them.
#
# Verified against open-gpu-kernel-modules tag 610.43.02 (GA102 / RTX A5000):
#   booter_load.img  60416 B   SEC2 HS booter image  (IMAGE_PROD)
#   booter_load.sig    768 B   RSA3K production signature (SIG_PROD)
#   booter_load.hdr     36 B   HS header (HEADER_PROD)
#   gsprmboot.img    24576 B   GSP RISC-V bootloader (UCODE_IMAGE_PROD)
#   gsprmboot.desc      84 B   RM_RISCV_UCODE_DESC (UCODE_DESC_PROD)
#
# Usage: extract-gsp-booter.py <ogkm-checkout> <out-dir>
#   e.g. extract-gsp-booter.py ~/code/ogkm-610 build/gsp-ucode
import re, sys, zlib, os

def get_array(path, var):
    src = open(path).read()
    m = re.search(re.escape(var) + r"\s*\[\]\s*=\s*\{", src)
    if not m:
        return None
    s = m.end(); e = src.index("};", s)
    return bytes(int(x, 16) for x in re.findall(r"0x[0-9a-fA-F]{2}", src[s:e]))

def inflate(b):
    if b is None:
        return None
    try:
        return zlib.decompress(b, -15)   # raw DEFLATE — matches driver's utilGzGetData
    except Exception:
        return b                         # stored uncompressed

def grab(path, base, label, expect=None):
    out = inflate(get_array(path, f"{base}_BINDATA_LABEL_{label}_data"))
    if out is None:
        raise SystemExit(f"!! {base} {label} not found in {path}")
    if expect is not None and len(out) != expect:
        print(f"   WARN {label}: got {len(out)} B, expected {expect}")
    return out

def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: extract-gsp-booter.py <ogkm-checkout> <out-dir>")
    ogkm, out = sys.argv[1], sys.argv[2]
    os.makedirs(out, exist_ok=True)
    gen = f"{ogkm}/src/nvidia/generated"

    bf = f"{gen}/g_bindata_kgspGetBinArchiveBooterLoadUcode_GA102.c"
    bb = "kgspBinArchiveBooterLoadUcode_GA102"
    blobs = {
        "booter_load.img": grab(bf, bb, "IMAGE_PROD", 60416),
        "booter_load.sig": grab(bf, bb, "SIG_PROD", 768),
        "booter_load.hdr": grab(bf, bb, "HEADER_PROD", 36),
    }
    lf = f"{gen}/g_bindata_kgspGetBinArchiveGspRmBoot_GA102.c"
    lb = "kgspBinArchiveGspRmBoot_GA102"
    blobs["gsprmboot.img"]  = grab(lf, lb, "UCODE_IMAGE_PROD", 24576)
    blobs["gsprmboot.desc"] = grab(lf, lb, "UCODE_DESC_PROD", 84)

    for name, data in blobs.items():
        open(f"{out}/{name}", "wb").write(data)
        print(f"  {name:18s} {len(data):6d} B")
    print(f"==> wrote {len(blobs)} GSP boot ucode blobs to {out}")

if __name__ == "__main__":
    main()
