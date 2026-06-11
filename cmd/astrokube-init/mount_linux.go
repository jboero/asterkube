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
	"syscall"
)

// mountSpec describes one filesystem the init process tries to mount.
type mountSpec struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	// device, when true, marks source as a block device node that must exist
	// before we attempt the mount; missing devices are skipped, not failed.
	device bool
	// skipIfPopulated, when true, skips the mount if the target directory is
	// already non-empty. Asterinas exposes device nodes under /dev without a
	// devtmpfs mount, so mounting one would be both unnecessary and unsupported.
	skipIfPopulated bool
}

// mountResult records the outcome of attempting one mountSpec.
type mountResult struct {
	spec    mountSpec
	skipped bool
	note    string
	err     error
}

// defaultMounts is the set of virtual and virtio-backed filesystems a
// Kubernetes node needs early. Pseudo-filesystems come first; the virtio block
// devices are best-effort because a minimal boot may not attach them.
//
// The list intentionally mirrors the filesystems Asterinas' own reference init
// shell script sets up, so behavior matches a known-good baseline.
func defaultMounts() []mountSpec {
	return []mountSpec{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs", skipIfPopulated: true},
		{source: "tmpfs", target: "/tmp", fstype: "tmpfs"},
		{source: "tmpfs", target: "/run", fstype: "tmpfs"},
		{source: "cgroup2", target: "/sys/fs/cgroup", fstype: "cgroup2"},
		{source: "configfs", target: "/sys/kernel/config", fstype: "configfs"},
		// virtio block devices (assume virtio wherever possible).
		{source: "/dev/vda", target: "/ext2", fstype: "ext2", device: true},
		{source: "/dev/vdb", target: "/exfat", fstype: "exfat", device: true},
	}
}

// mountAll attempts every spec in order and returns the results. It never
// aborts on a single failure: an init that gives up on the first missing
// filesystem is less useful than one that brings up everything it can.
func mountAll(specs []mountSpec) []mountResult {
	results := make([]mountResult, 0, len(specs))
	for _, spec := range specs {
		results = append(results, mountOne(spec))
	}
	return results
}

func mountOne(spec mountSpec) mountResult {
	if spec.device && !exists(spec.source) {
		return mountResult{spec: spec, skipped: true, note: "device " + spec.source + " not present"}
	}
	if spec.skipIfPopulated && isPopulated(spec.target) {
		return mountResult{spec: spec, skipped: true, note: "already populated by kernel"}
	}
	// Best-effort: ensure the mount point exists. Failures here (for example a
	// read-only parent) are surfaced by the mount call itself.
	_ = os.MkdirAll(spec.target, 0o755)

	if err := syscall.Mount(spec.source, spec.target, spec.fstype, spec.flags, spec.data); err != nil {
		return mountResult{spec: spec, err: err}
	}
	return mountResult{spec: spec}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isPopulated reports whether the directory at path already contains entries.
func isPopulated(path string) bool {
	dir, err := os.Open(path)
	if err != nil {
		return false
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	return err == nil && len(names) > 0
}

// report prints a concise table of what was mounted, skipped, or failed.
func report(results []mountResult) {
	fmt.Println("astrokube-init: mounting filesystems")
	for _, r := range results {
		switch {
		case r.skipped:
			fmt.Printf("  [skip] %-20s (%s): %s\n", r.spec.target, r.spec.fstype, r.note)
		case r.err != nil:
			fmt.Printf("  [fail] %-20s (%s): %v\n", r.spec.target, r.spec.fstype, r.err)
		default:
			fmt.Printf("  [ ok ] %-20s (%s)\n", r.spec.target, r.spec.fstype)
		}
	}
}

// powerOff halts the machine the same way a normal init would: by asking the
// kernel to power down. On Asterinas under QEMU this triggers a clean shutdown.
func powerOff() {
	// Best-effort sync so any filesystem writes are flushed first.
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		fmt.Printf("astrokube-init: power off failed: %v\n", err)
		// As PID 1 we must not return; spin so the kernel does not panic on
		// init exit.
		for {
			syscall.Pause()
		}
	}
}
