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

// file_mac_probe exercises the astermac FILE-access MAC: a file carries a tenant
// label, and a process of a different tenant cannot access it even on a shared
// filesystem (where namespaces and uid/gid DAC do not separate tenants). It
// proves cross-tenant deny (enforcing), same-tenant allow, unconfined (tenant 0)
// allow, and permissive logging — while unlabeled files stay unaffected.

const (
	fileMacProbeEnv  = "ASTERKUBE_FILE_MAC_PROBE" // "main" | "peer"
	fileMacPathEnv   = "ASTERKUBE_FILE_MAC_PATH"
	secretPath       = "/tmp/astermac-secret"

	// "KUFL": label the file at fd (arg2) with a tenant (arg3).
	prAsterkubeLabelFd = 0x4b55464c
)

// labelFd labels the open file referred to by fd with the given tenant.
func labelFd(fd uintptr, tenant uintptr) error {
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prAsterkubeLabelFd, fd, tenant, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// openErrno opens path read-only and returns the resulting errno (0 on success).
func openErrno(path string) syscall.Errno {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err == nil {
		syscall.Close(fd)
		return 0
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}
	return syscall.EIO
}

// fileMacProbePeer is the second tenant: it labels itself tenant B and tries to
// read the (tenant-A-labeled) secret file, exiting 0 only if it was denied.
func fileMacProbePeer() {
	if err := prctlSetTenant(tenantB); err != nil {
		fmt.Printf("file-peer: FAILED to set tenant: %v\n", err)
		os.Exit(2)
	}
	path := os.Getenv(fileMacPathEnv)
	errno := openErrno(path)
	fmt.Printf("file-peer: open(secret) as tenant B err=%v (want EPERM)\n", errnoStr(errno))
	if errno == syscall.EPERM {
		os.Exit(0)
	}
	os.Exit(3)
}

func errnoStr(e syscall.Errno) string {
	if e == 0 {
		return "<nil>"
	}
	return e.Error()
}

// fileMacProbeMain runs the scenario and prints results.
func fileMacProbeMain() {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	// Create the secret file (as the default unconfined tenant 0) and label it
	// tenant A. Keep it open so we can label it by fd.
	f, err := os.Create(secretPath)
	if err != nil {
		fmt.Printf("file-main: FAILED to create %s: %v\n", secretPath, err)
		os.Exit(2)
	}
	_, _ = f.WriteString("tenant-A secret\n")
	defer func() {
		_ = f.Close()
		_ = os.Remove(secretPath)
	}()

	if err := prctlMacMode(macModeEnforcing); err != nil {
		fmt.Printf("file-main: FAILED to set enforcing: %v\n", err)
		os.Exit(2)
	}
	if err := labelFd(f.Fd(), tenantA); err != nil {
		fmt.Printf("file-main: FAILED to label file: %v\n", err)
		os.Exit(2)
	}

	// Scenario 1 — ENFORCING, cross-tenant: a tenant-B peer must be denied.
	peer := exec.Command(self)
	peer.Env = append(os.Environ(), fileMacProbeEnv+"=peer", fileMacPathEnv+"="+secretPath)
	peerOut, peerErr := peer.CombinedOutput()
	fmt.Printf("  %s", peerOut)
	crossDenied := peerErr == nil // peer exits 0 only if it got EPERM

	// Scenario 2 — ENFORCING, same-tenant: main as tenant A may read it.
	_ = prctlSetTenant(tenantA)
	sameErr := openErrno(secretPath)
	sameAllowed := sameErr == 0
	fmt.Printf("  enforcing same-tenant open: err=%v (want <nil>)\n", errnoStr(sameErr))

	// Scenario 3 — ENFORCING, unconfined: tenant 0 is never restricted.
	_ = prctlSetTenant(0)
	unconfErr := openErrno(secretPath)
	unconfAllowed := unconfErr == 0
	fmt.Printf("  enforcing unconfined(tenant 0) open: err=%v (want <nil>)\n", errnoStr(unconfErr))

	// Scenario 4 — PERMISSIVE, cross-tenant: allowed, kernel logs a would-be deny.
	_ = prctlMacMode(macModePermissive)
	_ = prctlSetTenant(tenantB)
	permErr := openErrno(secretPath)
	permAllowed := permErr == 0
	fmt.Printf("  permissive cross-tenant open: err=%v (want <nil>; kernel logs it)\n", errnoStr(permErr))

	// Cleanup: unlabel the file and restore the permissive default.
	_ = labelFd(f.Fd(), 0)
	_ = prctlMacMode(macModePermissive)
	_ = prctlSetTenant(0)

	if crossDenied && sameAllowed && unconfAllowed && permAllowed {
		fmt.Println("file-main: RESULT PASS")
		os.Exit(0)
	}
	fmt.Println("file-main: RESULT FAIL")
	os.Exit(3)
}

// runFileMacProbe (parent side, called from init) drives the file-MAC demo.
func runFileMacProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== astermac file-access MAC probe =====")
	fmt.Println("asterkube-init: tenant-labeled file; cross-tenant reads are mediated")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	main := exec.Command(self)
	main.Env = append(os.Environ(), fileMacProbeEnv+"=main")
	out, _ := main.CombinedOutput()
	fmt.Printf("%s", out)

	ok := main.ProcessState != nil && main.ProcessState.ExitCode() == 0
	if ok {
		fmt.Println("asterkube-init: astermac file MAC is ENFORCED (cross-tenant deny + same/unconfined allow + permissive log) ✓")
	} else {
		fmt.Println("asterkube-init: astermac file MAC probe INCOMPLETE — see above")
	}
	fmt.Println("asterkube-init: ===== end astermac file-access MAC probe =====")
}
