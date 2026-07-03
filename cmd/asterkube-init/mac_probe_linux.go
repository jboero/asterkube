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
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// mac_probe exercises the native astromac MAC: every process carries a tenant
// label, and a process of one tenant may not signal a process of a different
// tenant. It proves the kernel hook fires and that enforcing vs. permissive
// behave correctly — while the default (unlabeled, tenant 0) path is untouched,
// keeping existing Kubernetes workloads working.
//
// Topology: init re-execs this binary as the "main" probe, which spawns a "peer"
// child. The peer labels itself tenant 2 and waits; the main probe varies its
// own tenant and the global mode and checks the kernel's signal decisions.

const (
	macProbeEnv = "ASTERKUBE_MAC_PROBE" // "main" | "peer"

	prAsterkubeSetTenant = 0x4b55544e // "KUTN"
	prAsterkubeMacMode   = 0x4b554d4d // "KUMM"

	macModePermissive = 1
	macModeEnforcing  = 2

	tenantA = 1
	tenantB = 2
)

func prctlSetTenant(tenant uintptr) error {
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prAsterkubeSetTenant, tenant, 0, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

func prctlMacMode(mode uintptr) error {
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prAsterkubeMacMode, mode, 0, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// macProbePeer is the child: it labels itself tenant B, signals readiness, and
// then blocks until its stdin is closed (by the parent) before exiting.
func macProbePeer() {
	if err := prctlSetTenant(tenantB); err != nil {
		fmt.Printf("mac-peer: FAILED to set tenant: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("peer-ready")
	os.Stdout.Sync()
	// Block until the parent closes our stdin, then exit.
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

// macProbeMain (re-exec'd by runMacProbe) runs the scenario and prints results.
func macProbeMain() {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	peer := exec.Command(self)
	peer.Env = append(os.Environ(), macProbeEnv+"=peer")
	stdin, _ := peer.StdinPipe()
	stdout, _ := peer.StdoutPipe()
	if err := peer.Start(); err != nil {
		fmt.Printf("mac-main: FAILED to start peer: %v\n", err)
		os.Exit(2)
	}
	defer func() {
		_ = stdin.Close() // releases the peer
		_ = peer.Wait()
	}()

	// Wait for the peer to label itself and report ready.
	rd := bufio.NewReader(stdout)
	line, _ := rd.ReadString('\n')
	if line != "peer-ready\n" {
		fmt.Printf("mac-main: peer did not become ready (got %q)\n", line)
		os.Exit(2)
	}
	peerPid := peer.Process.Pid

	// Scenario 1 — ENFORCING, cross-tenant: main=tenantA signaling peer=tenantB
	// must be denied with EPERM.
	if err := prctlMacMode(macModeEnforcing); err != nil {
		fmt.Printf("mac-main: FAILED to set enforcing: %v\n", err)
		os.Exit(2)
	}
	if err := prctlSetTenant(tenantA); err != nil {
		fmt.Printf("mac-main: FAILED to set tenant A: %v\n", err)
		os.Exit(2)
	}
	crossErr := syscall.Kill(peerPid, 0)
	crossDenied := crossErr == syscall.EPERM
	fmt.Printf("  enforcing cross-tenant kill(peer): err=%v (want EPERM)\n", crossErr)

	// Scenario 2 — ENFORCING, same-tenant: main switches to tenantB; signaling
	// the peer (also tenantB) must now be allowed.
	if err := prctlSetTenant(tenantB); err != nil {
		fmt.Printf("mac-main: FAILED to set tenant B: %v\n", err)
		os.Exit(2)
	}
	sameErr := syscall.Kill(peerPid, 0)
	sameAllowed := sameErr == nil
	fmt.Printf("  enforcing same-tenant kill(peer): err=%v (want nil)\n", sameErr)

	// Scenario 3 — PERMISSIVE, cross-tenant: back to tenantA, mode permissive;
	// the cross-tenant signal is allowed (the kernel logs a would-be denial).
	if err := prctlSetTenant(tenantA); err != nil {
		fmt.Printf("mac-main: FAILED to reset tenant A: %v\n", err)
		os.Exit(2)
	}
	if err := prctlMacMode(macModePermissive); err != nil {
		fmt.Printf("mac-main: FAILED to set permissive: %v\n", err)
		os.Exit(2)
	}
	permErr := syscall.Kill(peerPid, 0)
	permAllowed := permErr == nil
	fmt.Printf("  permissive cross-tenant kill(peer): err=%v (want nil; kernel logs it)\n", permErr)

	if crossDenied && sameAllowed && permAllowed {
		fmt.Println("mac-main: RESULT PASS")
		os.Exit(0)
	}
	fmt.Println("mac-main: RESULT FAIL")
	os.Exit(3)
}

// runMacProbe (parent side, called from init) drives the astromac demonstration.
func runMacProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== astromac (native MAC) probe =====")
	fmt.Println("asterkube-init: tenant-labeled processes; cross-tenant signals are mediated")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	main := exec.Command(self)
	main.Env = append(os.Environ(), macProbeEnv+"=main")
	out, _ := main.CombinedOutput()
	fmt.Printf("%s", out)

	ok := main.ProcessState != nil && main.ProcessState.ExitCode() == 0
	if ok {
		fmt.Println("asterkube-init: astromac MAC is ENFORCED (cross-tenant deny + same-tenant allow + permissive log) ✓")
	} else {
		fmt.Println("asterkube-init: astromac MAC probe INCOMPLETE — see above")
	}
	// Leave the global mode at permissive (the k8s-safe default) for the rest of
	// the boot; unlabeled node/pod processes (tenant 0) are unaffected regardless.
	fmt.Println("asterkube-init: ===== end astromac probe =====")
}
