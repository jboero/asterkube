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
	"syscall"
)

// rootless_probe is the adversarial gate for user-namespace rootless support.
// It proves the capability boundary that makes "root in a container" safe:
//
//   1. An UNPRIVILEGED process (uid 1000, no caps) cannot create a network
//      namespace in the initial user namespace — EPERM. (baseline: unprivileged)
//   2. After it enters a NEW user namespace via clone(CLONE_NEWUSER), it CAN
//      create a network namespace — it is effectively root within its own
//      namespace subtree. (rootless works)
//   3. It still CANNOT act on a host-owned resource (signal init's PID 1) —
//      EPERM. (no privilege escalation)
//
// If 1 or 3 ever fail, the boundary is broken and the change must not ship.

const (
	rootlessProbeEnv = "ASTROKUBE_ROOTLESS_PROBE" // "unpriv" | "userns"

	cloneNewUser = 0x10000000
	cloneNewNet  = 0x40000000
	sysUnshare   = 272

	unprivUID = 1000
)

func unshareErrno(flags uintptr) syscall.Errno {
	_, _, e := syscall.Syscall(sysUnshare, flags, 0, 0)
	return e
}

// rootlessProbeUserns runs in the CLONE_NEWUSER child — "root" in a fresh user
// namespace. It must be able to create a netns but not touch host PID 1.
func rootlessProbeUserns() {
	netErr := unshareErrno(cloneNewNet)
	hostErr := syscall.Kill(1, 0) // permission check only; no signal sent

	netOK := netErr == 0
	hostDenied := hostErr == syscall.EPERM
	fmt.Printf("rootless-userns: create-netns err=%v (want ok); signal-host-pid1 err=%v (want EPERM)\n",
		errnoStr(netErr), hostErr)
	if netOK && hostDenied {
		os.Exit(0)
	}
	os.Exit(3)
}

// rootlessProbeUnpriv runs as the unprivileged uid-1000 process. It confirms it
// is genuinely unprivileged, then spawns the userns child.
func rootlessProbeUnpriv() {
	baseErr := unshareErrno(cloneNewNet)
	baselineDenied := baseErr == syscall.EPERM
	fmt.Printf("rootless-unpriv: uid=%d create-netns-in-init err=%v (want EPERM)\n",
		os.Getuid(), errnoStr(baseErr))

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), rootlessProbeEnv+"=userns")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: cloneNewUser}
	out, childErr := child.CombinedOutput()
	fmt.Printf("  %s", out)

	if baselineDenied && childErr == nil {
		os.Exit(0)
	}
	os.Exit(3)
}

// runRootlessProbe (parent side, called from init as root) spawns the
// unprivileged actor and reports the rootless verdict.
func runRootlessProbe() {
	fmt.Println()
	fmt.Println("astrokube-init: ===== rootless (user-namespace capability) probe =====")
	fmt.Println("astrokube-init: unprivileged -> userns gains caps in its OWN ns, stays powerless on host")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	unpriv := exec.Command(self)
	unpriv.Env = append(os.Environ(), rootlessProbeEnv+"=unpriv")
	// Run as an unprivileged uid; NoSetGroups avoids a setgroups dependency.
	unpriv.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: unprivUID, Gid: unprivUID, NoSetGroups: true},
	}
	out, uerr := unpriv.CombinedOutput()
	fmt.Printf("%s", out)

	if unpriv.ProcessState != nil && unpriv.ProcessState.ExitCode() == 0 && uerr == nil {
		fmt.Println("astrokube-init: rootless is ENFORCED — unprivileged gains caps inside its userns, denied on host ✓")
	} else {
		fmt.Println("astrokube-init: rootless probe INCOMPLETE/FAILED — see above (boundary not verified)")
	}
	fmt.Println("astrokube-init: ===== end rootless probe =====")
}
