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
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// zeroCContainerdTest brings up a STATIC (CGO-free) containerd from the share
// and runs a container through it — with the container runtime being our own
// pure-Go runc replacement. If it succeeds, the entire path
//
//	containerd (static) -> containerd-shim-runc-v2 (static) -> runc (pure Go) -> container
//
// runs with zero C, on a node image that has no libc at all.
func zeroCContainerdTest() {
	fmt.Println()
	fmt.Println("astrokube-init: ===== zero-C containerd path =====")
	fmt.Println("astrokube-init: static containerd + shim driving the pure-Go OCI runtime")

	if err := os.MkdirAll(virtiofsMount, 0o755); err != nil {
		fmt.Printf("astrokube-init: SKIPPED (mkdir %s: %v)\n", virtiofsMount, err)
		return
	}
	if err := syscall.Mount(virtiofsTag, virtiofsMount, "virtiofs", 0, ""); err != nil {
		// Possibly already mounted; only bail if the binary is missing below.
		fmt.Printf("astrokube-init: note: virtio-fs mount: %v\n", err)
	}

	containerd := filepath.Join(virtiofsMount, "containerd")
	ctr := filepath.Join(virtiofsMount, "ctr")
	if _, err := os.Stat(containerd); err != nil {
		fmt.Printf("astrokube-init: SKIPPED (no static containerd on share: %v)\n", err)
		return
	}
	// Confirm the delivered containerd is genuinely C-free (static, no interp).
	if dynamic, _ := isDynamicELF(containerd); dynamic {
		fmt.Println("astrokube-init: SKIPPED (share containerd is dynamically linked, not the zero-C build)")
		return
	}
	fmt.Println("astrokube-init: containerd on share is statically linked (zero C) ✓")

	// Provide our pure-Go OCI runtime as `runc` on PATH, so the shim drives it.
	_ = os.Remove("/usr/bin/runc")
	if err := os.Symlink("/usr/bin/astrokube-init", "/usr/bin/runc"); err != nil {
		fmt.Printf("astrokube-init: WARN could not link /usr/bin/runc: %v\n", err)
	} else {
		fmt.Println("astrokube-init: /usr/bin/runc -> pure-Go OCI runtime ✓")
	}

	root := "/run/containerd/root"
	state := "/run/containerd/state"
	sock := "/run/containerd/containerd.sock"
	logPath := "/run/containerd/containerd.log"
	_ = os.MkdirAll(root, 0o755)
	_ = os.MkdirAll(state, 0o755)
	// PATH reaches the shim + ctr (share) and runc (/usr/bin). No LD_LIBRARY_PATH:
	// nothing here links libc.
	env := append(os.Environ(),
		"PATH=/usr/bin:/bin:"+virtiofsMount,
		"XDG_RUNTIME_DIR=/run",
	)

	logf, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("astrokube-init: FAILED to open containerd log: %v\n", err)
		return
	}
	daemon := exec.Command(containerd,
		"--root", root, "--state", state, "--address", sock, "--log-level", "info")
	daemon.Env = env
	daemon.Stdout = logf
	daemon.Stderr = logf
	if err := daemon.Start(); err != nil {
		fmt.Printf("astrokube-init: FAILED to start static containerd: %v\n", err)
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
		fmt.Println("astrokube-init: FAILED static containerd did not open its socket")
		dumpTail(logPath, 40)
		return
	}

	if out, err := runWithTimeoutEnv(40*time.Second, env, ctr, "--address", sock, "version"); err != nil {
		fmt.Printf("astrokube-init: FAILED `ctr version`: %v\n", err)
		dumpTail(logPath, 40)
		return
	} else {
		for _, l := range splitLines(out) {
			if strings.Contains(l, "Version") || strings.Contains(l, "Server") || strings.Contains(l, "UUID") {
				fmt.Printf("ctr| %s\n", strings.TrimSpace(l))
			}
		}
		fmt.Println("astrokube-init: static containerd daemon serves its API over AF_UNIX (zero C) ✓")
	}

	// Import + run a container; the shim spawns our pure-Go runc to do it.
	ctrImportAndRun(ctr, sock, env, logPath)
	fmt.Println("astrokube-init: ===== end zero-C containerd path =====")
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
