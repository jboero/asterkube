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

// socket_mac_probe exercises the astromac NETWORK MAC: an IPv4 endpoint carries
// a tenant label, and a process of a different tenant may not connect to it even
// on a shared network (pods on a shared bridge can reach each other's IPs, which
// namespaces do not prevent). The MAC hook runs before the connect itself, so a
// cross-tenant connect fails with EPERM regardless of whether anything listens;
// allowed connects fall through to a fast loopback ECONNREFUSED.

const (
	socketMacProbeEnv = "ASTERKUBE_SOCKET_MAC_PROBE" // "main" | "peer"

	// "KUIP": label an IPv4 endpoint (arg2 = ip be-u32) with a tenant (arg3).
	prAsterkubeLabelIp = 0x4b554950

	// 127.0.0.1 as u32::from_be_bytes([127,0,0,1]); a closed high port.
	testIPBE   = 0x7f000001
	testPort   = 59999
)

func labelIP(ip uintptr, tenant uintptr) error {
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prAsterkubeLabelIp, ip, tenant, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// connectErrno makes one TCP connect attempt to testIP:testPort and returns the
// resulting errno (0 on the unlikely success).
func connectErrno() syscall.Errno {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		if e, ok := err.(syscall.Errno); ok {
			return e
		}
		return syscall.EIO
	}
	defer syscall.Close(fd)
	sa := &syscall.SockaddrInet4{Port: testPort}
	sa.Addr = [4]byte{
		byte((testIPBE >> 24) & 0xff), byte((testIPBE >> 16) & 0xff),
		byte((testIPBE >> 8) & 0xff), byte(testIPBE & 0xff),
	}
	err = syscall.Connect(fd, sa)
	if err == nil {
		return 0
	}
	if e, ok := err.(syscall.Errno); ok {
		return e
	}
	return syscall.EIO
}

// socketMacProbePeer (tenant B) tries to connect to the tenant-A-labeled IP and
// exits 0 only if it was denied with EPERM.
func socketMacProbePeer() {
	if err := prctlSetTenant(tenantB); err != nil {
		fmt.Printf("socket-peer: FAILED to set tenant: %v\n", err)
		os.Exit(2)
	}
	errno := connectErrno()
	fmt.Printf("socket-peer: connect(labeled IP) as tenant B err=%v (want EPERM)\n", errnoStr(errno))
	if errno == syscall.EPERM {
		os.Exit(0)
	}
	os.Exit(3)
}

// socketMacProbeMain runs the scenario and prints results.
func socketMacProbeMain() {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	if err := prctlMacMode(macModeEnforcing); err != nil {
		fmt.Printf("socket-main: FAILED to set enforcing: %v\n", err)
		os.Exit(2)
	}
	if err := labelIP(testIPBE, tenantA); err != nil {
		fmt.Printf("socket-main: FAILED to label IP: %v\n", err)
		os.Exit(2)
	}

	// Scenario 1 — ENFORCING, cross-tenant: a tenant-B peer must be denied.
	peer := exec.Command(self)
	peer.Env = append(os.Environ(), socketMacProbeEnv+"=peer")
	peerOut, peerErr := peer.CombinedOutput()
	fmt.Printf("  %s", peerOut)
	crossDenied := peerErr == nil // peer exits 0 only if it got EPERM

	// "Allowed" means the MAC hook did not deny — i.e. NOT EPERM (the actual
	// connect then fails with ECONNREFUSED on the closed loopback port).
	allowed := func(e syscall.Errno) bool { return e != syscall.EPERM }

	// Scenario 2 — ENFORCING, same-tenant.
	_ = prctlSetTenant(tenantA)
	sameErr := connectErrno()
	sameAllowed := allowed(sameErr)
	fmt.Printf("  enforcing same-tenant connect: err=%v (want not-EPERM)\n", errnoStr(sameErr))

	// Scenario 3 — ENFORCING, unconfined (tenant 0).
	_ = prctlSetTenant(0)
	unconfErr := connectErrno()
	unconfAllowed := allowed(unconfErr)
	fmt.Printf("  enforcing unconfined connect: err=%v (want not-EPERM)\n", errnoStr(unconfErr))

	// Scenario 4 — PERMISSIVE, cross-tenant: allowed, kernel logs a would-be deny.
	_ = prctlMacMode(macModePermissive)
	_ = prctlSetTenant(tenantB)
	permErr := connectErrno()
	permAllowed := allowed(permErr)
	fmt.Printf("  permissive cross-tenant connect: err=%v (want not-EPERM; kernel logs it)\n", errnoStr(permErr))

	// Cleanup: unlabel the IP and restore the permissive default.
	_ = labelIP(testIPBE, 0)
	_ = prctlMacMode(macModePermissive)
	_ = prctlSetTenant(0)

	if crossDenied && sameAllowed && unconfAllowed && permAllowed {
		fmt.Println("socket-main: RESULT PASS")
		os.Exit(0)
	}
	fmt.Println("socket-main: RESULT FAIL")
	os.Exit(3)
}

// runSocketMacProbe (parent side, called from init) drives the network-MAC demo.
func runSocketMacProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== astromac network MAC probe =====")
	fmt.Println("asterkube-init: tenant-labeled IP; cross-tenant connects are mediated")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	main := exec.Command(self)
	main.Env = append(os.Environ(), socketMacProbeEnv+"=main")
	out, _ := main.CombinedOutput()
	fmt.Printf("%s", out)

	ok := main.ProcessState != nil && main.ProcessState.ExitCode() == 0
	if ok {
		fmt.Println("asterkube-init: astromac network MAC is ENFORCED (cross-tenant deny + same/unconfined allow + permissive log) ✓")
	} else {
		fmt.Println("asterkube-init: astromac network MAC probe INCOMPLETE — see above")
	}
	fmt.Println("asterkube-init: ===== end astromac network MAC probe =====")
}
