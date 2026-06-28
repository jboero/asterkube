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
	"os/signal"
	"syscall"
	"time"
)

// liveProcs are the long-running node processes (containerd and the
// apiserver-connected kubelet) that a *persistent* node keeps running after the
// boot-time capability demos. gracefulShutdown stops them on the way down.
var liveProcs []*exec.Cmd

// keepAlive registers a started, long-running process so the node keeps it
// running instead of tearing it down when its setup function returns.
func keepAlive(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		liveProcs = append(liveProcs, cmd)
	}
}

// nodeIsLive reports whether the persistent node actually came up: at least one
// long-running process is registered and still running.
func nodeIsLive() bool {
	for _, c := range liveProcs {
		if c != nil && c.Process != nil && (c.ProcessState == nil || !c.ProcessState.Exited()) {
			return true
		}
	}
	return false
}

// serveForever keeps the node up as a live, interactive cluster member. It
// blocks until a termination signal arrives — including the one the kernel
// raises for an ACPI power-button event (QEMU `system_powerdown` / virsh
// shutdown), which it delivers to PID 1 — then shuts the node down gracefully.
func serveForever() {
	const line = "============================================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println(" astrokube node is LIVE and persistent.")
	fmt.Println(" `kubectl get nodes` shows astrokube Ready; schedule workloads onto it.")
	fmt.Println(" It stays up until shut down via ACPI (virsh shutdown /")
	fmt.Println(" QEMU `system_powerdown`), which it handles gracefully.")
	fmt.Println(line)

	// NOTE: we deliberately do NOT run a wait(-1) zombie reaper here. containerd
	// and the kubelet manage their own children, and a blanket reaper in this Go
	// process races their bookkeeping (the runc-v2 shim "start" handshake fails
	// with a spurious ENOENT). Over a demo session the few reparented zombies are
	// harmless; a namespace-scoped reaper that excludes the runtimes is future work.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPWR)
	s := <-sigs
	fmt.Printf("\nastrokube-init: received %v — shutting the node down gracefully.\n", s)
	gracefulShutdown()
}

// gracefulShutdown asks the node's processes to stop, gives them a moment to
// drain, force-kills any stragglers, flushes filesystems, and powers off.
func gracefulShutdown() {
	for _, c := range liveProcs {
		if c != nil && c.Process != nil {
			_ = c.Process.Signal(syscall.SIGTERM)
		}
	}
	time.Sleep(3 * time.Second)
	for _, c := range liveProcs {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
		}
	}
	syscall.Sync()
	fmt.Println("astrokube-init: node stopped. Powering off.")
	powerOff()
}
