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

// userns_probe verifies asterkube user-namespace Stage 1: clone(CLONE_NEWUSER)
// now creates a real, tracked child user namespace (previously it failed with
// EINVAL). It does NOT yet test rootless/id-mapping (Stage 2/3) — and by design
// Stage 1 grants no new privilege, so this only confirms the namespace was
// created and is distinct from the parent's.
//
// The child is launched with CLONE_NEWUSER and no uid/gid mappings (so it keeps
// its root credentials and does not depend on the not-yet-implemented writable
// uid_map), then compares /proc/self/ns/user against the parent's.

const (
	usernsProbeEnv  = "ASTERKUBE_USERNS_PROBE" // "child"
	usernsParentEnv = "ASTERKUBE_USERNS_PARENT"
)

func userNsLink() string {
	link, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return "ERR:" + err.Error()
	}
	return link
}

// usernsProbeChild runs in the CLONE_NEWUSER child: it reports its user-ns link
// and exits 0 only if it differs from the parent's (i.e. a new ns was created).
func usernsProbeChild() {
	child := userNsLink()
	parent := os.Getenv(usernsParentEnv)
	uidMap := strings.TrimSpace(readFileBestEffort("/proc/self/uid_map"))
	fmt.Printf("userns-child: parent ns=%s child ns=%s uid_map=%q\n", parent, child, uidMap)
	if child != parent && !strings.HasPrefix(child, "ERR:") {
		os.Exit(0)
	}
	os.Exit(3)
}

func readFileBestEffort(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return string(b)
}

// runUsernsProbe (parent side, called from init) drives the Stage-1 demo.
func runUsernsProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== user-namespace (Stage 1) probe =====")
	fmt.Println("asterkube-init: clone(CLONE_NEWUSER) should create a real child user namespace")

	parentLink := userNsLink()

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), usernsProbeEnv+"=child", usernsParentEnv+"="+parentLink)
	// Request a new user namespace, with no id mappings (Stage 1 keeps creds).
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	out, err := child.CombinedOutput()
	fmt.Printf("  %s", out)

	switch {
	case err == nil:
		fmt.Println("asterkube-init: user namespaces Stage 1 OK — clone(CLONE_NEWUSER) creates a distinct, tracked namespace ✓")
		fmt.Println("asterkube-init: (id mapping + ns-aware capabilities are Stage 2/3 — see USER-NAMESPACES.md)")
	default:
		fmt.Printf("asterkube-init: user-namespace Stage 1 INCOMPLETE — clone(CLONE_NEWUSER) failed: %v\n", err)
	}
	fmt.Println("asterkube-init: ===== end user-namespace probe =====")
}
