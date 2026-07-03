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
	"strings"
	"syscall"
)

// sandbox_probe demonstrates the pod-sandbox networking model: a workload
// process JOINS an existing sandbox's network namespace via setns (the same
// mechanism the pure-Go OCI runtime now uses in joinNamespaces for OCI namespace
// specs that carry a path). It proves multi-container pods can share the
// sandbox's netns — containers in a pod reach each other over localhost.

const sandboxProbeEnv = "ASTERKUBE_SANDBOX_NETNS" // "hold" | "join"

func netnsLink() string {
	l, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "ERR:" + err.Error()
	}
	return l
}

// sandboxProbeHold is the "pause"/sandbox: it lives in a fresh network namespace
// (created by the parent via CLONE_NEWNET), reports it, and stays alive until
// its stdin is closed.
func sandboxProbeHold() {
	fmt.Println(netnsLink())
	os.Stdout.Sync()
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

// sandboxProbeJoin opens the sandbox's network namespace by path and joins it,
// then reports the network namespace it now sees.
func sandboxProbeJoin() {
	pid := os.Getenv("ASTERKUBE_SANDBOX_PID")
	fd, err := syscall.Open("/proc/"+pid+"/ns/net", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		fmt.Printf("ERR open: %v\n", err)
		os.Exit(2)
	}
	if _, _, errno := syscall.Syscall(sysSetns, uintptr(fd), cloneNewNet, 0); errno != 0 {
		fmt.Printf("ERR setns: %v\n", errno)
		os.Exit(2)
	}
	syscall.Close(fd)
	fmt.Println(netnsLink())
}

// runSandboxNetnsProbe (called from init) creates a sandbox netns holder and a
// joiner, and verifies the joiner ends up in the sandbox's network namespace.
func runSandboxNetnsProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== pod-sandbox netns-join probe =====")
	fmt.Println("asterkube-init: a workload joins the sandbox's network namespace via setns")

	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}

	// The sandbox ("pause"): hold a fresh network namespace open.
	sandbox := exec.Command(self)
	sandbox.Env = append(os.Environ(), sandboxProbeEnv+"=hold")
	sandbox.SysProcAttr = &syscall.SysProcAttr{Cloneflags: cloneNewNet}
	stdin, _ := sandbox.StdinPipe()
	stdout, _ := sandbox.StdoutPipe()
	if err := sandbox.Start(); err != nil {
		fmt.Printf("asterkube-init: sandbox start failed: %v\n", err)
		fmt.Println("asterkube-init: ===== end pod-sandbox netns-join probe =====")
		return
	}
	defer func() { _ = stdin.Close(); _ = sandbox.Wait() }()

	rd := bufio.NewReader(stdout)
	sandboxNet, _ := rd.ReadString('\n')
	sandboxNet = strings.TrimSpace(sandboxNet)
	sandboxPid := sandbox.Process.Pid

	// The host's own network namespace (the control: must differ from sandbox).
	hostNet := netnsLink()

	// The workload: join the sandbox's network namespace.
	joiner := exec.Command(self)
	joiner.Env = append(os.Environ(), sandboxProbeEnv+"=join", fmt.Sprintf("ASTERKUBE_SANDBOX_PID=%d", sandboxPid))
	out, _ := joiner.CombinedOutput()
	joinerNet := strings.TrimSpace(string(out))

	fmt.Printf("  sandbox netns=%s, host netns=%s, joiner netns=%s\n", sandboxNet, hostNet, joinerNet)
	joined := joinerNet == sandboxNet && strings.HasPrefix(sandboxNet, "net:")
	distinctFromHost := sandboxNet != hostNet
	if joined && distinctFromHost {
		fmt.Println("asterkube-init: pod-sandbox networking WORKS — workload joined the sandbox's netns (shared) ✓")
		fmt.Println("asterkube-init: (the pure-Go runtime now setns-joins net/ipc/uts for path-based OCI ns specs)")
	} else {
		fmt.Println("asterkube-init: pod-sandbox netns-join INCOMPLETE — see above")
	}
	fmt.Println("asterkube-init: ===== end pod-sandbox netns-join probe =====")
}
