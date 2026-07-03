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
	"path/filepath"
	"syscall"
)

// runContainerRuntimeTests exercises the container-runtime substrate the way
// containerd/runc need it: an overlay snapshot (Phase 0), a real runc container
// on an OCI bundle (Phase 1), and containerd driving runc (Phase 2). Each phase
// is best-effort and bounded; a missing binary or a kernel gap prints a FAILED
// line rather than hanging.
func runContainerRuntimeTests() {
	fmt.Println()
	fmt.Println("== container runtime tests ==")
	overlayPhase0()
	runcPhase1()
	containerdPhase2()
}

// overlayPhase0 validates the overlayfs snapshotter substrate: it builds a
// lower/upper/work/merged layout, mounts an overlay, and checks the three
// behaviours containerd relies on — a lower file shows through, a write lands in
// the upper layer, and a delete is recorded as a whiteout (the lower stays
// intact). This is the exact mount the overlay snapshotter issues.
func overlayPhase0() {
	fmt.Println("-- phase 0: overlayfs snapshot --")
	base := "/run/ovl"
	lower := filepath.Join(base, "lower")
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	merged := filepath.Join(base, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("overlay: FAILED to mkdir %s: %v\n", d, err)
			return
		}
	}

	// A file that exists only in the (read-only) lower layer.
	if err := os.WriteFile(filepath.Join(lower, "from-lower.txt"), []byte("lower-data"), 0o644); err != nil {
		fmt.Printf("overlay: FAILED to seed lower: %v\n", err)
		return
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		fmt.Printf("overlay: FAILED to mount (%s): %v\n", opts, err)
		return
	}
	defer syscall.Unmount(merged, 0)
	fmt.Printf("overlay: mounted %s\n", opts)

	// 1. The lower file shows through the merged view.
	if data, err := os.ReadFile(filepath.Join(merged, "from-lower.txt")); err != nil {
		fmt.Printf("overlay: FAILED lower not visible: %v\n", err)
		return
	} else if string(data) != "lower-data" {
		fmt.Printf("overlay: FAILED lower content %q\n", string(data))
		return
	}
	fmt.Println("overlay: OK lower layer visible through merged view")

	// 2. A write through the merged view lands in the upper layer (copy-up for
	//    new files goes straight to upper).
	if err := os.WriteFile(filepath.Join(merged, "from-upper.txt"), []byte("upper-data"), 0o644); err != nil {
		fmt.Printf("overlay: FAILED to write through merged: %v\n", err)
		return
	}
	if _, err := os.Stat(filepath.Join(upper, "from-upper.txt")); err != nil {
		fmt.Printf("overlay: FAILED upper write not in upperdir: %v\n", err)
		return
	}
	fmt.Println("overlay: OK write through merged landed in upper layer")

	// 3. Deleting a lower-only file through the merged view is recorded as a
	//    whiteout; the lower copy is untouched.
	if err := os.Remove(filepath.Join(merged, "from-lower.txt")); err != nil {
		fmt.Printf("overlay: FAILED to delete lower file via merged: %v\n", err)
		return
	}
	if _, err := os.Stat(filepath.Join(merged, "from-lower.txt")); !os.IsNotExist(err) {
		fmt.Printf("overlay: FAILED deleted file still visible (err=%v)\n", err)
		return
	}
	if _, err := os.Stat(filepath.Join(lower, "from-lower.txt")); err != nil {
		fmt.Printf("overlay: FAILED lower copy disturbed by whiteout: %v\n", err)
		return
	}
	fmt.Println("overlay: OK delete recorded as whiteout, lower intact")
	fmt.Println("overlay: phase 0 PASSED (snapshotter substrate works)")
}
