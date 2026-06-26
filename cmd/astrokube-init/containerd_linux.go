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
	"syscall"
	"time"
)

// containerd and its tools are delivered to the guest over virtio-fs rather than
// baked into the initramfs: the initramfs is only a boot scaffold, while in a
// real node these binaries (and images) live outside it. The host shares a
// directory holding containerd/ctr/the shim; the guest mounts it read-write at
// virtiofsMount and runs the daemon from there. Their glibc closure (libc,
// libresolv, ld-linux) is already present in the initramfs /lib64, so no
// libraries need to ride along on the share.
const (
	virtiofsTag   = "astrokubevfs"
	virtiofsMount = "/mnt/vfs"
	virtiofsMark  = "MARKER"
)

// containerdPhase2 mounts the virtio-fs share and brings containerd up from it.
// It advances one verifiable step at a time: (1) prove virtio-fs mounts and
// reads, (2) prove the containerd binary runs on Asterinas. Daemon start and
// `ctr run` of a side-loaded image come next.
func containerdPhase2() {
	fmt.Println("-- phase 2: containerd daemon --")

	// Step 1: mount the virtio-fs share. Absence of the device is not a failure;
	// a plain boot (no virtiofsd on the host) simply skips this phase.
	if err := os.MkdirAll(virtiofsMount, 0o755); err != nil {
		fmt.Printf("containerd: FAILED mkdir %s: %v\n", virtiofsMount, err)
		return
	}
	if err := syscall.Mount(virtiofsTag, virtiofsMount, "virtiofs", 0, ""); err != nil {
		fmt.Printf("containerd: SKIPPED (no virtio-fs share %q: %v)\n", virtiofsTag, err)
		return
	}
	defer syscall.Unmount(virtiofsMount, 0)
	fmt.Printf("containerd: mounted virtio-fs tag=%q at %s\n", virtiofsTag, virtiofsMount)

	// Prove the mount is readable: list it and read the marker the host wrote.
	entries, err := os.ReadDir(virtiofsMount)
	if err != nil {
		fmt.Printf("containerd: FAILED to read virtio-fs dir: %v\n", err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	fmt.Printf("containerd: virtio-fs contents: %v\n", names)
	if mark, err := os.ReadFile(filepath.Join(virtiofsMount, virtiofsMark)); err != nil {
		fmt.Printf("containerd: FAILED to read marker: %v\n", err)
		return
	} else {
		fmt.Printf("containerd: virtio-fs OK, marker=%q\n", trimSpace(string(mark)))
	}

	// Step 2: prove the containerd binary actually executes on Asterinas. Its
	// interpreter (/lib64/ld-linux-x86-64.so.2) and libraries are in the
	// initramfs, so it can run straight from the share.
	containerd := filepath.Join(virtiofsMount, "containerd")
	if _, err := os.Stat(containerd); err != nil {
		fmt.Printf("containerd: SKIPPED daemon (no containerd on the share: %v)\n", err)
		return
	}
	out, err := runWithTimeout(10*time.Second, containerd, "--version")
	for _, line := range splitLines(out) {
		if line != "" {
			fmt.Printf("containerd| %s\n", line)
		}
	}
	if err != nil {
		fmt.Printf("containerd: FAILED to run `containerd --version`: %v\n", err)
		return
	}
	fmt.Println("containerd: binary runs on Asterinas (daemon bring-up next)")
}

// runWithTimeout runs a command, returning its combined output, killing it if it
// exceeds d.
func runWithTimeout(d time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=/lib64"}
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		_ = cmd.Process.Kill()
		return string(out), fmt.Errorf("timed out after %s", d)
	}
	return string(out), runErr
}

// trimSpace trims leading/trailing ASCII whitespace without pulling in strings
// for one call site.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
