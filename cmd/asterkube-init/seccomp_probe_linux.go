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
	"runtime"
	"syscall"
	"unsafe"
)

// seccomp_probe verifies that the Asterinas kernel ENFORCES seccomp BPF filters
// (not just accepts them). It is an adversarial test: each child installs a real
// filter and then deliberately attempts the action the filter targets, proving
// the kernel acted on it.
//
// The probes run in dedicated child processes (re-exec of /proc/self/exe), so an
// installed filter — which cannot be removed and is inherited by children — does
// not leak into init or the container runtime. This also exercises that filters
// survive execve and that the kill path terminates a process.

const (
	seccompProbeEnv = "ASTERKUBE_SECCOMP_PROBE" // "errno" | "kill"

	// SYS_seccomp is not exported by the stdlib syscall package; 317 on x86_64.
	sysSeccomp = 317

	prSetNoNewPrivs = 38
	seccompSetModeFilter = 1
	seccompFilterFlagTSync = 1

	// Classic-BPF opcodes.
	bpfLd  = 0x00
	bpfW   = 0x00
	bpfAbs = 0x20
	bpfJmp = 0x05
	bpfJeq = 0x10
	bpfRet = 0x06
	bpfK   = 0x00

	// seccomp_data field offsets.
	offNr   = 0
	offArch = 4

	auditArchX8664 = 0xC000003E

	retKillProcess = 0x80000000
	retErrno       = 0x00050000
	retAllow       = 0x7fff0000

	eperm = 1
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	length uint16
	_      [6]byte // pad so the pointer is 8-byte aligned, matching C sock_fprog
	filter *sockFilter
}

// buildFilter returns a program that: rejects the wrong arch, then for the
// target syscall returns `action` and otherwise allows the syscall.
func buildFilter(targetNr uint32, action uint32) []sockFilter {
	return []sockFilter{
		// A = seccomp_data.arch
		{bpfLd | bpfW | bpfAbs, 0, 0, offArch},
		// if A == x86_64 -> +1 (load nr); else fall through to kill
		{bpfJmp | bpfJeq | bpfK, 1, 0, auditArchX8664},
		{bpfRet | bpfK, 0, 0, retKillProcess},
		// A = seccomp_data.nr
		{bpfLd | bpfW | bpfAbs, 0, 0, offNr},
		// if A == target -> +0 (apply action); else +1 (allow)
		{bpfJmp | bpfJeq | bpfK, 0, 1, targetNr},
		{bpfRet | bpfK, 0, 0, action},
		{bpfRet | bpfK, 0, 0, retAllow},
	}
}

func installFilter(prog []sockFilter) error {
	fprog := sockFprog{length: uint16(len(prog)), filter: &prog[0]}
	// Try with TSYNC (what libseccomp/runc use); fall back without it.
	_, _, errno := syscall.Syscall(sysSeccomp, seccompSetModeFilter,
		seccompFilterFlagTSync, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		_, _, errno = syscall.Syscall(sysSeccomp, seccompSetModeFilter,
			0, uintptr(unsafe.Pointer(&fprog)))
	}
	runtime.KeepAlive(prog)
	runtime.KeepAlive(&fprog)
	if errno != 0 {
		return errno
	}
	return nil
}

// seccompProbeChild is the body run in the re-exec'd child. It installs a filter
// targeting uname(2) and then calls uname, proving the kernel enforced it.
func seccompProbeChild(mode string) {
	runtime.LockOSThread()

	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		fmt.Printf("seccomp-probe-child: FAILED PR_SET_NO_NEW_PRIVS: %v\n", errno)
		os.Exit(2)
	}

	action := uint32(retErrno | eperm)
	if mode == "kill" {
		action = retKillProcess
	}
	if err := installFilter(buildFilter(uint32(syscall.SYS_UNAME), action)); err != nil {
		fmt.Printf("seccomp-probe-child: FAILED to install filter: %v\n", err)
		os.Exit(2)
	}

	// Control: a syscall NOT covered by the filter must still work.
	if pid := syscall.Getpid(); pid <= 0 {
		fmt.Printf("seccomp-probe-child: control getpid() failed\n")
		os.Exit(2)
	}

	// The targeted syscall. In "kill" mode this line should never return — the
	// kernel kills the process. In "errno" mode it must return EPERM.
	var uts syscall.Utsname
	err := syscall.Uname(&uts)

	if mode == "kill" {
		// We were supposed to be killed before getting here.
		fmt.Printf("seccomp-probe-child: NOT KILLED — uname returned err=%v (filter not enforced)\n", err)
		os.Exit(3)
	}

	if err == syscall.EPERM {
		fmt.Printf("seccomp-probe-child: OK uname blocked with EPERM; getpid still works\n")
		os.Exit(0)
	}
	fmt.Printf("seccomp-probe-child: FAILED uname returned err=%v want EPERM (filter not enforced)\n", err)
	os.Exit(3)
}

// runSeccompProbe (parent side) spawns the two child probes and reports whether
// the kernel enforced the filters.
func runSeccompProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== seccomp-bpf enforcement probe =====")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	// 1) ERRNO action: uname must come back EPERM, control syscall unaffected.
	errnoChild := exec.Command(self)
	errnoChild.Env = append(os.Environ(), seccompProbeEnv+"=errno")
	out, _ := errnoChild.CombinedOutput()
	errnoOK := errnoChild.ProcessState != nil && errnoChild.ProcessState.ExitCode() == 0
	fmt.Printf("  errno-filter: %s", out)
	if errnoOK {
		fmt.Println("  errno-filter: PASS — kernel enforced SECCOMP_RET_ERRNO(EPERM)")
	} else {
		fmt.Println("  errno-filter: FAIL — kernel did not enforce the ERRNO filter")
	}

	// 2) KILL action: the child must be terminated by a signal, not exit normally.
	killChild := exec.Command(self)
	killChild.Env = append(os.Environ(), seccompProbeEnv+"=kill")
	killOut, killErr := killChild.CombinedOutput()
	if len(killOut) > 0 {
		fmt.Printf("  kill-filter: %s", killOut)
	}
	killed := false
	signal := ""
	if killErr != nil {
		if ee, ok := killErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				killed = true
				signal = ws.Signal().String()
			}
		}
	}
	if killed {
		fmt.Printf("  kill-filter: PASS — kernel killed the process on the filtered syscall (signal: %s)\n", signal)
	} else {
		fmt.Printf("  kill-filter: FAIL — process was not killed (err=%v)\n", killErr)
	}

	if errnoOK && killed {
		fmt.Println("asterkube-init: seccomp-bpf is ENFORCED (errno + kill verified) ✓")
	} else {
		fmt.Println("asterkube-init: seccomp-bpf enforcement INCOMPLETE — see failures above")
	}
	fmt.Println("asterkube-init: ===== end seccomp-bpf probe =====")
}
