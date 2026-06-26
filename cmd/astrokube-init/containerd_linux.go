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
	fmt.Println("containerd: binary runs on Asterinas")

	// Step 3: start the daemon and talk to it with ctr over its AF_UNIX socket.
	ctr := filepath.Join(virtiofsMount, "ctr")
	startContainerdDaemon(containerd, ctr)
}

// startContainerdDaemon launches containerd, waits for its control socket, and
// runs `ctr version` against it to prove the gRPC/ttrpc API works over AF_UNIX.
func startContainerdDaemon(containerd, ctr string) {
	root := "/run/containerd/root"   // image/content store
	state := "/run/containerd/state" // ephemeral runtime state
	sock := "/run/containerd/containerd.sock"
	logPath := "/run/containerd/containerd.log"
	for _, d := range []string{root, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("containerd: FAILED mkdir %s: %v\n", d, err)
			return
		}
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("containerd: FAILED to create daemon log: %v\n", err)
		return
	}
	defer logFile.Close()

	// PATH must reach runc (initramfs /usr/bin) and the shim (virtio-fs share).
	env := []string{
		"PATH=/usr/bin:/bin:" + virtiofsMount,
		"LD_LIBRARY_PATH=/lib64",
		"XDG_RUNTIME_DIR=/run",
		"TMPDIR=/tmp",
	}
	daemon := exec.Command(containerd,
		"--root", root, "--state", state, "--address", sock, "--log-level", "debug")
	daemon.Env = env
	daemon.Stdout = logFile
	daemon.Stderr = logFile
	if err := daemon.Start(); err != nil {
		fmt.Printf("containerd: FAILED to start daemon: %v\n", err)
		return
	}
	defer func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	}()

	// Wait for the control socket to appear (daemon ready).
	ready := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			ready = true
			break
		}
		// If the daemon died early, stop waiting.
		if daemon.ProcessState != nil && daemon.ProcessState.Exited() {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !ready {
		fmt.Println("containerd: daemon socket never appeared; daemon log:")
		dumpTail(logPath, 40)
		return
	}
	fmt.Printf("containerd: daemon up, socket %s present\n", sock)

	// Talk to the daemon: `ctr version` exercises the client→daemon API path.
	out, err := runWithTimeoutEnv(15*time.Second, env, ctr, "--address", sock, "version")
	for _, line := range splitLines(out) {
		if line != "" {
			fmt.Printf("ctr| %s\n", line)
		}
	}
	if err != nil {
		fmt.Printf("containerd: FAILED `ctr version`: %v\n", err)
		fmt.Println("containerd: daemon log tail:")
		dumpTail(logPath, 40)
		return
	}
	fmt.Println("containerd: daemon serves the API over AF_UNIX (ctr version OK)")
}

// dumpTail prints the last n lines of a file with a "  log| " prefix.
func dumpTail(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("  (could not read %s: %v)\n", path, err)
		return
	}
	lines := splitLines(string(data))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		if line != "" {
			fmt.Printf("  log| %s\n", line)
		}
	}
}

// runWithTimeout runs a command, returning its combined output, killing it if it
// exceeds d.
func runWithTimeout(d time.Duration, name string, args ...string) (string, error) {
	return runWithTimeoutEnv(d, []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=/lib64"}, name, args...)
}

// runWithTimeoutEnv is runWithTimeout with an explicit environment.
func runWithTimeoutEnv(d time.Duration, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
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
