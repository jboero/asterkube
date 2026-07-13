// gpu-container-init — a pure-Go (CGO_ENABLED=0, no C) PID 1 that launches a
// minimal *container* inside Asterinas and, from within it, uses a
// passed-through NVIDIA GPU through /dev/nvidia0.
//
// It demonstrates the whole "use the GPU from within a container in Asterinas"
// path with no C anywhere in the shipped result:
//   parent (PID 1): re-exec self in NEW pid + uts + mount + ipc namespaces
//   child  (the container):
//     - set its own hostname (UTS isolation)
//     - make its mount namespace private and mount a FRESH tmpfs over /dev,
//       hiding the host devtmpfs, then mknod its OWN /dev/nvidia0 (c 195 0)
//     - open that node and read/write + mmap GPU VRAM
//
// The child runs as PID 1 of its pid namespace with a container-private /dev it
// built itself, proving the GPU is reachable from inside a real container.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const (
	devNode  = "/dev/nvidia0"
	nvMajor  = 195
	nvMinor  = 0
	childArg = "container-child"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == childArg {
		runContainer()
		return
	}
	runParent()
}

// runParent re-execs this binary inside new namespaces — the container.
func runParent() {
	fmt.Println("gpu-container: PID 1 launching container (new pid/uts/mount/ipc namespaces)")
	// Use the known init path rather than os.Executable() — the latter reads
	// /proc/self/exe, and this minimal initramfs has no /proc mounted.
	self := "/sbin/init"
	cmd := &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC,
	}
	pid, err := forkExec(self, []string{self, childArg}, cmd)
	if err != nil {
		fmt.Printf("gpu-container: failed to create container: %v\n", err)
		poweroff()
		return
	}
	var ws syscall.WaitStatus
	syscall.Wait4(pid, &ws, 0, nil)
	fmt.Printf("gpu-container: container exited (status %d)\n", ws.ExitStatus())
	poweroff()
}

func forkExec(path string, argv []string, attr *syscall.SysProcAttr) (int, error) {
	return syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Files: []uintptr{0, 1, 2},
		Sys:   attr,
	})
}

// runContainer executes inside the container's namespaces.
func runContainer() {
	host, _ := os.Hostname()
	fmt.Printf("gpu-container[child]: pid=%d (in its pid namespace) host=%q\n", os.Getpid(), host)

	// UTS isolation: our own hostname, invisible to the parent namespace.
	if err := syscall.Sethostname([]byte("gpu-container")); err != nil {
		fmt.Printf("gpu-container[child]: sethostname: %v\n", err)
	} else {
		h, _ := os.Hostname()
		fmt.Printf("gpu-container[child]: hostname now %q (UTS-isolated)\n", h)
	}

	// Mount isolation: private mount ns + a fresh tmpfs /dev we populate ourselves.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Printf("gpu-container[child]: make-rprivate: %v\n", err)
	}
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", 0, "mode=0755"); err != nil {
		fmt.Printf("gpu-container[child]: mount tmpfs /dev: %v (falling back to host /dev)\n", err)
	} else {
		fmt.Println("gpu-container[child]: mounted private tmpfs over /dev")
		dev := (nvMajor << 8) | nvMinor
		if err := syscall.Mknod(devNode, syscall.S_IFCHR|0o666, dev); err != nil {
			fmt.Printf("gpu-container[child]: mknod %s: %v\n", devNode, err)
		} else {
			fmt.Printf("gpu-container[child]: created container-private %s (c %d %d)\n",
				devNode, nvMajor, nvMinor)
		}
	}

	ok := gpuRW() && gpuMmap()
	if ok {
		fmt.Println("GPU-IN-CONTAINER: PASS — a containerized process used the GPU via /dev/nvidia0")
	} else {
		fmt.Println("GPU-IN-CONTAINER: FAIL")
	}
	os.Exit(0)
}

func gpuRW() bool {
	f, err := os.OpenFile(devNode, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("gpu-container[child]: open %s: %v\n", devNode, err)
		return false
	}
	defer f.Close()
	const off = 0x6000
	want := uint32(0x1337_c0de)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], want)
	if _, err := f.WriteAt(buf[:], off); err != nil {
		fmt.Printf("gpu-container[child]: WriteAt: %v\n", err)
		return false
	}
	var rb [4]byte
	if _, err := f.ReadAt(rb[:], off); err != nil {
		fmt.Printf("gpu-container[child]: ReadAt: %v\n", err)
		return false
	}
	got := binary.LittleEndian.Uint32(rb[:])
	fmt.Printf("gpu-container[child]: read/write @%#x wrote=%#08x read=%#08x -> %v\n", off, want, got, got == want)
	return got == want
}

func gpuMmap() bool {
	f, err := os.OpenFile(devNode, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	const mapLen = 0x10000
	data, err := syscall.Mmap(int(f.Fd()), 0, mapLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		fmt.Printf("gpu-container[child]: mmap: %v\n", err)
		return false
	}
	defer syscall.Munmap(data)
	const off = 0x7000
	want := uint32(0xb01dface)
	binary.LittleEndian.PutUint32(data[off:off+4], want)
	got := binary.LittleEndian.Uint32(data[off : off+4])
	fmt.Printf("gpu-container[child]: mmap @%#x wrote=%#08x read=%#08x -> %v\n", off, want, got, got == want)
	return got == want
}

func poweroff() {
	if os.Getpid() == 1 {
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		for {
		}
	}
}
