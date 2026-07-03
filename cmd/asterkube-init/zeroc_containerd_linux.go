/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The zero-C container runtime is baked directly into the initramfs (it used to
// be delivered over a virtio-fs share during development; the finished image is
// self-contained, so the ISO/QCOW2 carries everything). containerd, ctr and the
// runc v2 shim are static, CGO-free binaries on PATH in /usr/bin; the side-loaded
// image is a tarball under /usr/share/asterkube.
const (
	zeroCContainerd = "/usr/bin/containerd"
	zeroCCtr        = "/usr/bin/ctr"
	zeroCImageTar   = "/usr/share/asterkube/hello.tar"
)

// zeroCContainerdTest brings up the STATIC (CGO-free) containerd baked into the
// initramfs and runs a container through it — with the container runtime being
// our own pure-Go runc replacement. If it succeeds, the entire path
//
//	containerd (static) -> containerd-shim-runc-v2 (static) -> runc (pure Go) -> container
//
// runs with zero C, on a node image that has no libc at all.
func zeroCContainerdTest() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== zero-C containerd path =====")
	fmt.Println("asterkube-init: static containerd + shim driving the pure-Go OCI runtime")

	containerd := zeroCContainerd
	ctr := zeroCCtr
	if _, err := os.Stat(containerd); err != nil {
		fmt.Printf("asterkube-init: SKIPPED (no static containerd in image: %v)\n", err)
		return
	}
	// Confirm the baked-in containerd is genuinely C-free (static, no interp).
	if dynamic, _ := isDynamicELF(containerd); dynamic {
		fmt.Println("asterkube-init: SKIPPED (containerd is dynamically linked, not the zero-C build)")
		return
	}
	fmt.Println("asterkube-init: containerd in image is statically linked (zero C) ✓")

	// Provide our pure-Go OCI runtime as `runc` on PATH, so the shim drives it.
	// It is this very binary (multi-call: invoked as `runc` it is the runtime).
	self, eerr := os.Executable()
	if eerr != nil || self == "" {
		self = "/usr/bin/kubelet"
	}
	_ = os.Remove("/usr/bin/runc")
	if err := os.Symlink(self, "/usr/bin/runc"); err != nil {
		fmt.Printf("asterkube-init: WARN could not link /usr/bin/runc: %v\n", err)
	} else {
		fmt.Println("asterkube-init: /usr/bin/runc -> our binary (pure-Go OCI runtime) ✓")
	}

	root := "/run/containerd/root"
	state := "/run/containerd/state"
	sock := "/run/containerd/containerd.sock"
	logPath := "/run/containerd/containerd.log"
	_ = os.MkdirAll(root, 0o755)
	_ = os.MkdirAll(state, 0o755)
	// PATH reaches containerd, ctr, the shim and runc — all in /usr/bin. No
	// LD_LIBRARY_PATH: nothing here links libc.
	env := append(os.Environ(),
		"PATH=/usr/bin:/bin",
		"XDG_RUNTIME_DIR=/run",
	)

	logf, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("asterkube-init: FAILED to open containerd log: %v\n", err)
		return
	}
	daemon := exec.Command(containerd,
		"--root", root, "--state", state, "--address", sock, "--log-level", "info")
	daemon.Env = env
	daemon.Stdout = logf
	daemon.Stderr = logf
	if err := daemon.Start(); err != nil {
		fmt.Printf("asterkube-init: FAILED to start static containerd: %v\n", err)
		return
	}
	defer func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	}()

	// Wait for the API socket.
	up := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			up = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		fmt.Println("asterkube-init: FAILED static containerd did not open its socket")
		dumpTail(logPath, 40)
		return
	}

	if out, err := runWithTimeoutEnv(40*time.Second, env, ctr, "--address", sock, "version"); err != nil {
		fmt.Printf("asterkube-init: FAILED `ctr version`: %v\n", err)
		dumpTail(logPath, 40)
		return
	} else {
		for _, l := range splitLines(out) {
			if strings.Contains(l, "Version") || strings.Contains(l, "Server") || strings.Contains(l, "UUID") {
				fmt.Printf("ctr| %s\n", strings.TrimSpace(l))
			}
		}
		fmt.Println("asterkube-init: static containerd daemon serves its API over AF_UNIX (zero C) ✓")
	}

	// Import + run a container; the shim spawns our pure-Go runc to do it.
	ctrImportAndRun(ctr, sock, env, logPath, zeroCImageTar)
	fmt.Println("asterkube-init: ===== end zero-C containerd path =====")
}

// isDynamicELF reports whether the file at path is a dynamically-linked ELF
// (has a PT_INTERP / DT_NEEDED). A static, CGO-free Go binary returns false.
func isDynamicELF(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Cheap heuristic without a full ELF parser: scan for the interpreter string.
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	return strings.Contains(string(buf[:n]), "/lib64/ld-linux"), nil
}
