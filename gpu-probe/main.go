// gpu-probe — a pure-Go (CGO_ENABLED=0, no C) userspace program that runs
// inside Asterinas and *uses* a passed-through NVIDIA GPU through the kernel's
// /dev/nvidia0 character device: it opens the device, round-trips a test
// pattern through GPU VRAM via write()/read(), and then again via mmap().
//
// This is the userspace half of "use the GPU from within a container in
// Asterinas": the kernel exposes the GPU's BAR1 VRAM window as /dev/nvidia0,
// and this ordinary process reads and writes GPU memory with no special
// privileges and no C. Run as PID 1 in a minimal initramfs; on Asterinas it
// prints its results to the console and then powers off.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const devPath = "/dev/nvidia0"

func main() {
	fmt.Println("gpu-probe: userspace GPU test starting")

	ok := true
	if !rwTest() {
		ok = false
	}
	if !mmapTest() {
		ok = false
	}

	if ok {
		fmt.Println("GPU-USERSPACE: PASS — a userspace process read & wrote GPU VRAM via /dev/nvidia0")
	} else {
		fmt.Println("GPU-USERSPACE: FAIL")
	}

	// If we are PID 1, power the machine off so the QEMU run terminates cleanly.
	if os.Getpid() == 1 {
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		for {
		}
	}
}

// rwTest writes a pattern into GPU VRAM through the device file and reads it
// back with pread/pwrite (offset-addressed I/O into the BAR1 aperture).
func rwTest() bool {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("gpu-probe: open %s failed: %v\n", devPath, err)
		return false
	}
	defer f.Close()

	const off = 0x4000 // distinct from the offsets the kernel itself tests
	want := uint32(0xdead_beef)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], want)

	if _, err := f.WriteAt(buf[:], off); err != nil {
		fmt.Printf("gpu-probe: WriteAt failed: %v\n", err)
		return false
	}
	var rb [4]byte
	if _, err := f.ReadAt(rb[:], off); err != nil {
		fmt.Printf("gpu-probe: ReadAt failed: %v\n", err)
		return false
	}
	got := binary.LittleEndian.Uint32(rb[:])
	fmt.Printf("gpu-probe: read/write @%#x wrote=%#08x read=%#08x -> %v\n",
		off, want, got, got == want)
	return got == want
}

// mmapTest maps the GPU VRAM window into this process's address space and
// reads/writes it as plain memory — the same path CUDA-style userspace uses.
func mmapTest() bool {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("gpu-probe: open (mmap) failed: %v\n", err)
		return false
	}
	defer f.Close()

	const mapLen = 0x10000
	data, err := syscall.Mmap(int(f.Fd()), 0, mapLen,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		fmt.Printf("gpu-probe: mmap failed: %v\n", err)
		return false
	}
	defer syscall.Munmap(data)

	const off = 0x5000
	want := uint32(0xcafe_f00d)
	binary.LittleEndian.PutUint32(data[off:off+4], want)
	got := binary.LittleEndian.Uint32(data[off : off+4])
	fmt.Printf("gpu-probe: mmap @%#x wrote=%#08x read=%#08x -> %v\n",
		off, want, got, got == want)
	return got == want
}
