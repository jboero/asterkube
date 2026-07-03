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
	"syscall"
)

// usermap_probe verifies user-namespace Stage 2: an unprivileged process can
// write uid_map/gid_map for a new namespace and the container then SEES itself
// as root (uid 0) — the id-mapping half of functional rootless containers. The
// capability boundary (separately proven by rootless_probe) still holds: the
// mapped "root" has no effective capabilities against host resources.

const usermapProbeEnv = "ASTERKUBE_USERMAP_PROBE" // "unpriv" | "child"

// usermapProbeChild runs inside the mapped user namespace.
func usermapProbeChild() {
	uid := os.Getuid()
	gid := os.Getgid()
	uidMap := strings.TrimSpace(readFileBestEffort("/proc/self/uid_map"))
	gidMap := strings.TrimSpace(readFileBestEffort("/proc/self/gid_map"))
	fmt.Printf("usermap-child: getuid=%d getgid=%d uid_map=%q gid_map=%q\n", uid, gid, uidMap, gidMap)
	if uid == 0 && gid == 0 {
		os.Exit(0)
	}
	os.Exit(3)
}

// usermapProbeUnpriv (uid 1000) creates a mapped user namespace mapping its own
// uid/gid to container 0, and checks the child sees itself as root.
func usermapProbeUnpriv() {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), usermapProbeEnv+"=child")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		// Map container uid/gid 0 to this unprivileged process's own uid/gid.
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: unprivUID, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: unprivUID, Size: 1}},
		// Leave setgroups denied (the kernel requires this for an unprivileged
		// gid_map); Go writes "deny" to /proc/<pid>/setgroups for us.
		GidMappingsEnableSetgroups: false,
	}
	out, cerr := child.CombinedOutput()
	fmt.Printf("  %s", out)
	if cerr == nil {
		os.Exit(0)
	}
	fmt.Printf("usermap-unpriv: child failed: %v\n", cerr)
	os.Exit(3)
}

// runUsermapProbe (parent side, called from init as root) drives the id-mapping
// demo from an unprivileged actor.
func runUsermapProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== rootless id-mapping (Stage 2) probe =====")
	fmt.Println("asterkube-init: unprivileged maps its uid/gid -> container sees itself as root 0")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	unpriv := exec.Command(self)
	unpriv.Env = append(os.Environ(), usermapProbeEnv+"=unpriv")
	unpriv.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: unprivUID, Gid: unprivUID, NoSetGroups: true},
	}
	out, uerr := unpriv.CombinedOutput()
	fmt.Printf("%s", out)

	if unpriv.ProcessState != nil && unpriv.ProcessState.ExitCode() == 0 && uerr == nil {
		fmt.Println("asterkube-init: rootless id mapping WORKS — unprivileged container root maps to host uid, sees uid 0 ✓")
	} else {
		fmt.Println("asterkube-init: rootless id-mapping probe INCOMPLETE — see above")
	}
	fmt.Println("asterkube-init: ===== end rootless id-mapping probe =====")
}
