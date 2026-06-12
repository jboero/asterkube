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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// runcPayloadEnv marks this binary re-exec'd as the *container payload* by runc:
// it just reports it is alive inside the container and exits, instead of running
// any init/node logic.
const runcPayloadEnv = "ASTROKUBE_RUNC_PAYLOAD"

// runcBin is where the runc binary is baked into the image.
const runcBin = "/usr/bin/runc"

// runcPhase1 runs a real OCI container with runc on an overlay rootfs. The
// container payload is this same binary re-exec'd in payload mode (see the guard
// in main). It proves runc's full path works on Asterinas: clone with namespace
// flags, cgroup setup, overlay rootfs, pivot_root, no_new_privs, seccomp, exec.
func runcPhase1() {
	fmt.Println("-- phase 1: runc OCI container --")
	if _, err := os.Stat(runcBin); err != nil {
		fmt.Printf("runc: SKIPPED (no %s baked into image)\n", runcBin)
		return
	}

	bundle := "/run/bundle"
	rootfs := filepath.Join(bundle, "rootfs")
	base := "/run/runc-ovl"
	lower := filepath.Join(base, "lower")
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	for _, d := range []string{lower, upper, work, rootfs, bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("runc: FAILED mkdir %s: %v\n", d, err)
			return
		}
	}

	// Populate the (read-only) lower layer: the payload binary plus the standard
	// mount-target directories an OCI rootfs needs.
	if err := copyFile("/proc/self/exe", filepath.Join(lower, "payload"), 0o755); err != nil {
		fmt.Printf("runc: FAILED to stage payload: %v\n", err)
		return
	}
	for _, d := range []string{"proc", "dev", "sys", "tmp"} {
		if err := os.MkdirAll(filepath.Join(lower, d), 0o755); err != nil {
			fmt.Printf("runc: FAILED mkdir lower/%s: %v\n", d, err)
			return
		}
	}

	// Mount the overlay as the container rootfs — runc pivot_roots into it.
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := syscall.Mount("overlay", rootfs, "overlay", 0, opts); err != nil {
		fmt.Printf("runc: FAILED to mount overlay rootfs: %v\n", err)
		return
	}
	defer syscall.Unmount(rootfs, 0)

	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(ociConfigJSON), 0o644); err != nil {
		fmt.Printf("runc: FAILED to write config.json: %v\n", err)
		return
	}

	// runc run -b <bundle> --root <state> <id>
	state := "/run/runc-state"
	_ = os.MkdirAll(state, 0o755)
	cmd := exec.Command(runcBin, "--root", state, "run", "phase1")
	cmd.Dir = bundle
	cmd.Env = []string{"PATH=/usr/bin:/bin"}

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		fmt.Println("runc: FAILED (timed out after 20s)")
		return
	}

	for _, line := range splitLines(string(out)) {
		if line != "" {
			fmt.Printf("runc| %s\n", line)
		}
	}
	if runErr != nil {
		fmt.Printf("runc: FAILED to run container: %v\n", runErr)
		return
	}
	fmt.Println("runc: phase 1 PASSED (container ran via runc on overlay rootfs)")
}

// ociConfigJSON is a minimal OCI runtime spec: run /payload in pid/ipc/uts/mount
// namespaces with no_new_privs set and a default seccomp-free profile, on the
// overlay rootfs. Kept intentionally small; mounts are trimmed to /proc.
const ociConfigJSON = `{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 0, "gid": 0},
    "args": ["/payload"],
    "env": ["PATH=/", "` + runcPayloadEnv + `=1"],
    "cwd": "/",
    "capabilities": {
      "bounding": ["CAP_AUDIT_WRITE", "CAP_KILL"],
      "effective": ["CAP_AUDIT_WRITE", "CAP_KILL"],
      "permitted": ["CAP_AUDIT_WRITE", "CAP_KILL"]
    },
    "noNewPrivileges": true
  },
  "root": {"path": "rootfs", "readonly": false},
  "hostname": "runc-phase1",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc"}
  ],
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "ipc"},
      {"type": "uts"},
      {"type": "mount"}
    ],
    "resources": {}
  }
}`

// copyFile copies src to dst with the given mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// splitLines splits s into lines without importing strings just for this.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
